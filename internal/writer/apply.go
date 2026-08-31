package writer

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/hoklims/temper/internal/store"
)

// A mutation is planned in full before anything moves. Accepting a suggestion can touch several
// files at once — the AGENTS.md section, skill files demoted out of it, a CLAUDE.md import — and
// "wrote two of three files, then died" is the state this package exists to make impossible.
//
// The shape of the contract:
//   - BuildMutation computes every byte that will be on disk, reading but never writing.
//   - capture confines each destination, records a checkable authority over every root, and
//     records each destination's preimage, mode and checksums.
//   - the manifest goes into the store's journal BEFORE the first write, together with a claim on
//     every file it touches (writer.Accept).
//   - applyOps proves, for every destination, that the file still holds either the state the
//     manifest captured or the state it would produce — and only then writes.
//   - a failure unwinds to the preimages; a crash is finished later by Reconcile.
//
// The checksums are not diagnostics: they are the preconditions. A file in a THIRD state was
// changed by someone else, so neither applying nor rolling back is a no-op for them, and both are
// refused rather than guessed.
//
// Where the transaction ends: the LAST whole-manifest validation, after the last write, is the
// logical commit of the filesystem half. Everything before it can still be refused and unwound;
// an edit landing after it is a change made to a file this mutation had already finished writing,
// and the journal transaction that follows is not a lock on the user's own disk. This package does
// not claim to exclude it, and does not pretend the two commits are one.
//
// maxJournaledBytes bounds what a preimage may cost. A context file larger than this is not a
// context file, and journaling it would trade a bounded database for an unbounded one.
const maxJournaledBytes = 1 << 20

// ManifestVersion is the shape this build writes and the only one it will act on. A manifest is
// the sole authority saying which bytes may be written and through which directory, so an older,
// unknown or hand-edited shape is refused instead of interpreted.
const ManifestVersion = 1

// ErrConflict is that refusal. It is a sentinel because a caller — the review API in particular —
// must be able to answer "the world moved, reload and decide again" instead of "internal error".
var ErrConflict = errors.New("writer: a destination changed outside this mutation")

// errRootAbsent reports that an authorized root is not materialized: it did not exist when the
// mutation was captured and has not been created and bound by this operation yet. It is not a
// failure — capture must be able to plan into a directory that will only exist once the intent is
// journaled — and it is also a refusal to adopt: a directory that merely occupies the name is not
// the root this mutation was granted.
var errRootAbsent = errors.New("writer: the authorized root does not exist yet")

// modeIsPrecondition reports whether permission bits are part of a destination's state on this
// platform. Windows reflects only a read-only attribute, so comparing full permission bits there
// would manufacture conflicts instead of detecting them; the manifest still carries the mode, and
// the portable half of the invariant is asserted on the manifest rather than on the filesystem.
var modeIsPrecondition = runtime.GOOS != "windows"

// FileOp is one file this mutation replaces or removes, together with everything needed to put it
// back. It is serialized into the journal, so its JSON shape is a persisted format.
type FileOp struct {
	// Root is the authorized directory and the only mutation authority. Every operation is
	// performed through an os.Root opened on it and named relative to it, so a directory swapped
	// between the last check and the write cannot redirect anything out of the tree. What proves
	// the root is still the right directory is the RootAuthority recorded for it in the manifest,
	// not this name.
	Root string `json:"root"`
	// Path is the absolute destination as planned. It is data, not authority: it is re-derived
	// into a root-relative name (and re-checked) every time the manifest is loaded.
	Path string `json:"path"`
	// Content is the desired file content; ignored when Remove is set.
	Content string `json:"content,omitempty"`
	Remove  bool   `json:"remove,omitempty"`

	Existed     bool   `json:"existed"`
	Preimage    string `json:"preimage,omitempty"`
	PreimageSum string `json:"preimageSum,omitempty"`
	PostSum     string `json:"postSum,omitempty"`

	// Mode is the preimage's permission bits and ModeKnown says whether they were captured. The
	// two are separate because a file whose mode is 000 is not a file that does not exist, and a
	// single zero value used to mean both — which made a concurrent chmod to 000 invisible and
	// restored an owner-only file as world-readable.
	Mode      uint32 `json:"mode"`
	ModeKnown bool   `json:"modeKnown,omitempty"`
	// PostMode is the permission bits this operation leaves behind, so the postimage is a state
	// and not only a checksum. Unset for a removal, which leaves no file to carry a mode.
	PostMode      uint32 `json:"postMode"`
	PostModeKnown bool   `json:"postModeKnown,omitempty"`

	// rel is Path expressed under Root. Never serialized: recomputing it from the persisted pair
	// is what stops a tampered absolute path from becoming a mutation authority at replay.
	rel string
}

// RootAuthority is the durable, checkable right to act through one directory.
//
// A pathname cannot carry that right. Capture closes its handles and a later apply, rollback or
// replay reopens the root by name; renaming the root away and putting a DIFFERENT real directory
// in its place leaves every path comparison agreeing with itself. So the manifest records objects:
// the deepest ancestor that existed at capture time, the identity of that object, and the confined
// suffix leading from it to the root.
//
// A root that already existed is its own ancestor with the suffix ".", and its identity is known
// from the start. A root that did not exist yet is a promise — nothing under it can have been
// written — and it becomes an authority only when this operation creates the suffix THROUGH the
// open ancestor and records the identity of what it created, before the first target write.
type RootAuthority struct {
	Root     string `json:"root"`
	Anchor   string `json:"anchor"`
	AnchorID FileID `json:"anchorId"`
	Suffix   string `json:"suffix"`
	RootID   FileID `json:"rootId,omitempty"`
}

// Mutation is the complete filesystem effect of one decision.
type Mutation struct {
	// Version pins the manifest format. A journal entry that does not carry the current one is
	// refused rather than replayed: the fields an older build wrote may mean something else, and
	// "probably compatible" is not a property a deletion path may rely on.
	Version int      `json:"version"`
	Ops     []FileOp `json:"ops"`
	// Roots holds one authority per directory the operations name. Exactly one per root: a missing
	// entry is a mutation with no proven authority, and a superfluous one is an authority nothing
	// in this manifest asked for.
	Roots map[string]RootAuthority `json:"roots,omitempty"`
	// Notices are operator-facing lines reported only once the files are actually on disk — a
	// budget demotion announced at planning time would be a lie if the mutation rolled back.
	Notices     []string `json:"notices,omitempty"`
	WrittenPath string   `json:"writtenPath"`
}

// unboundRoot names the first authorized root that was absent at capture and never bound to a
// directory this operation created, or "" when every root has a proven identity. An operation
// found in "applying" with one of these provably wrote nothing under it.
func (m *Mutation) unboundRoot() string {
	roots := make([]string, 0, len(m.Roots))
	for root := range m.Roots {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	for _, root := range roots {
		if !m.Roots[root].RootID.known() {
			return root
		}
	}
	return ""
}

// applyHook runs after each file operation and exists so a test can interrupt a mutation.
// Panicking from it emulates a process death: nothing unwinds and the journal stays in
// "applying", exactly as a killed process leaves it. Returning an error emulates a failing write
// and takes the normal rollback path. Always nil outside tests.
//
// It is read under a lock because the tests that matter here are the concurrent ones: an
// unsynchronised global read by an in-flight acceptance while another goroutine clears the hook is
// a data race, and the race detector is one of this package's gates.
var (
	applyHookMu sync.RWMutex
	applyHook   func(index int, op FileOp) error
)

// installApplyHook sets (or clears, with nil) the interruption hook. Tests must go through
// setApplyHook, which pairs it with a cleanup that survives a panic.
func installApplyHook(fn func(index int, op FileOp) error) {
	applyHookMu.Lock()
	defer applyHookMu.Unlock()
	applyHook = fn
}

func currentApplyHook() func(index int, op FileOp) error {
	applyHookMu.RLock()
	defer applyHookMu.RUnlock()
	return applyHook
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// fileState classifies what a destination currently holds relative to one journaled operation.
type fileState int

const (
	// statePreimage: exactly what the manifest captured. Applying is safe.
	statePreimage fileState = iota
	// statePostimage: exactly what this operation produces. Applying again would be a no-op, so
	// a replay after a crash is idempotent and a rollback still knows what to restore.
	statePostimage
	// stateConflict: a third state. Something else owns these bytes now.
	stateConflict
)

// binder persists a root that has just been created and identified, while the operation is still
// applying. It is what makes "this directory is mine" survive the process: without it, a restart
// would find an operation that may have written under a root it cannot prove it created.
type binder func(manifest string) error

// fsAuthority opens each authorized root once and performs every read, write and removal relative
// to that handle. os.Root refuses any name that leaves the root — including through a symlink or
// junction planted after the last explicit check — which is what closes the window between
// checking a parent directory and creating a file inside it.
//
// It holds the manifest itself, not a copy of its identities, because binding a newly created root
// writes back into it.
type fsAuthority struct {
	m    *Mutation
	open map[string]*os.Root
	bind binder
	// capturing marks the single pass that RECORDS authorities instead of proving them. It is an
	// explicit mode rather than "nothing is recorded yet", because a manifest that carries no
	// authority at all — an entry written by an older build, or a row edited in the database —
	// would otherwise be indistinguishable from capture and granted the right it never recorded.
	capturing bool
}

func newFSAuthority(m *Mutation, bind binder) *fsAuthority {
	return &fsAuthority{m: m, open: map[string]*os.Root{}, bind: bind}
}

// newCaptureAuthority is the only authority allowed to act without a recorded root identity: it is
// the pass that establishes it.
func newCaptureAuthority(m *Mutation) *fsAuthority {
	return &fsAuthority{m: m, open: map[string]*os.Root{}, capturing: true}
}

// root hands back the open directory this manifest authorizes, having proved — every single time,
// cached handle or not — that it is still the directory the manifest captured.
//
// Two questions are asked and neither substitutes for the other. Does the recorded NAME still lead
// to the recorded object? Any directory can answer to a name, so this catches the substitution.
// Is the HANDLE about to be used that same object? A handle proved once is a proof about the world
// as it was then, and a mutation reads, writes and revalidates long after that.
//
// create is true only while the operation is applying, and it authorizes exactly one thing: making
// a root that did not exist at capture time, through the ancestor whose identity was recorded.
func (a *fsAuthority) root(dir string, create bool) (*os.Root, error) {
	if a.capturing {
		return a.captureRoot(dir)
	}
	auth, recorded := a.m.Roots[dir]
	if !recorded {
		return nil, fmt.Errorf("writer: this manifest records no authority over the root %s, so it cannot be applied or rolled back safely", clip(dir))
	}

	anchor, err := os.OpenRoot(auth.Anchor)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errRootAbsent
		}
		return nil, fmt.Errorf("writer: cannot take authority over %s: %w", clip(auth.Anchor), err)
	}
	defer anchor.Close()
	if err := proveIdentity(anchor, auth.AnchorID, auth.Anchor); err != nil {
		return nil, err
	}

	fresh, err := a.openUnderAnchor(anchor, dir, auth, create)
	if err != nil {
		return nil, err
	}
	// re-read the authority: openUnderAnchor may just have bound a root it created
	want := a.m.Roots[dir].RootID
	if err := proveIdentity(fresh, want, dir); err != nil {
		fresh.Close()
		return nil, err
	}
	held, cached := a.open[dir]
	if !cached {
		a.open[dir] = fresh
		return fresh, nil
	}
	fresh.Close()
	if err := proveIdentity(held, want, dir); err != nil {
		return nil, err
	}
	return held, nil
}

// openUnderAnchor resolves the root from the ancestor handle that has just been proved, so the
// suffix cannot be redirected by anything planted along the way.
func (a *fsAuthority) openUnderAnchor(anchor *os.Root, dir string, auth RootAuthority, create bool) (*os.Root, error) {
	if auth.RootID.known() {
		r, err := anchor.OpenRoot(auth.Suffix)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, errRootAbsent
			}
			return nil, fmt.Errorf("writer: cannot take authority over %s: %w", clip(dir), err)
		}
		return r, nil
	}
	// The root did not exist at capture time and this operation has not created it. Until the
	// identity of what IT created is durable, a directory sitting at that name is someone else's
	// and is not adopted — that is the difference between finishing this mutation and finishing it
	// inside a stranger's directory.
	if !create {
		return nil, errRootAbsent
	}
	if err := anchor.MkdirAll(auth.Suffix, 0o755); err != nil {
		return nil, err
	}
	r, err := anchor.OpenRoot(auth.Suffix)
	if err != nil {
		return nil, fmt.Errorf("writer: cannot take authority over %s: %w", clip(dir), err)
	}
	id, err := identityOfRoot(r)
	if err != nil {
		r.Close()
		return nil, err
	}
	auth.RootID = id
	if err := a.bindRoot(dir, auth); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// bindRoot makes the link between this operation and the directory it just created durable, while
// the operation is still applying and before a single target byte moves. A binding that fails to
// persist fails the write it was about to authorize: an operation that wrote under a root it
// cannot prove afterwards is exactly the state a restart cannot resolve.
func (a *fsAuthority) bindRoot(dir string, auth RootAuthority) error {
	a.m.Roots[dir] = auth
	if a.bind == nil {
		return nil
	}
	manifest, err := json.Marshal(a.m)
	if err != nil {
		return err
	}
	return a.bind(string(manifest))
}

// captureRoot is the recording pass. It never creates anything: a root that does not exist stays
// absent until the intent that needs it has been journaled.
func (a *fsAuthority) captureRoot(dir string) (*os.Root, error) {
	auth, recorded := a.m.Roots[dir]
	if !recorded {
		var err error
		if auth, err = captureAuthority(dir); err != nil {
			return nil, err
		}
		a.m.Roots[dir] = auth
	}
	if !auth.RootID.known() {
		return nil, errRootAbsent
	}
	if r, ok := a.open[dir]; ok {
		return r, nil
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("writer: cannot take authority over %s: %w", clip(dir), err)
	}
	a.open[dir] = r
	return r, nil
}

// captureAuthority records what makes a root checkable later: the deepest ancestor that exists,
// the identity of THAT object, and the confined suffix leading from it to the root. Stopping at
// the first ancestor that exists is what makes the record exact — everything above it exists too,
// and none of it is this mutation's to create.
func captureAuthority(dir string) (RootAuthority, error) {
	dir = filepath.Clean(dir)
	anchor := dir
	var missing []string
	for {
		if _, err := os.Lstat(anchor); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return RootAuthority{}, err
		}
		parent := filepath.Dir(anchor)
		if parent == anchor {
			return RootAuthority{}, fmt.Errorf("writer: %s has no existing ancestor to anchor an authority on", clip(dir))
		}
		missing = append(missing, filepath.Base(anchor))
		anchor = parent
	}
	id, err := identityOfName(anchor)
	if err != nil {
		return RootAuthority{}, fmt.Errorf("writer: cannot record the identity of %s: %w", clip(anchor), err)
	}
	auth := RootAuthority{Root: dir, Anchor: anchor, AnchorID: id, Suffix: "."}
	if len(missing) == 0 {
		auth.RootID = id
		return auth, nil
	}
	suffix := missing[len(missing)-1]
	for i := len(missing) - 2; i >= 0; i-- {
		suffix = filepath.Join(suffix, missing[i])
	}
	auth.Suffix = suffix
	return auth, nil
}

// proveIdentity refuses a directory that is not the object the manifest recorded. A manifest with
// no identity at all cannot borrow one from the filesystem: with nothing to contradict, every
// substitution would agree with itself.
func proveIdentity(r *os.Root, want FileID, name string) error {
	if !want.known() {
		return fmt.Errorf("writer: this manifest records no identity for %s, so acting through it would be trusting a name", clip(name))
	}
	got, err := identityOfRoot(r)
	if err != nil {
		return fmt.Errorf("writer: cannot identify the directory opened for %s: %w", clip(name), err)
	}
	if got != want {
		return fmt.Errorf("writer: %s is not the directory this mutation captured, so nothing is read from or written through it", clip(name))
	}
	return nil
}

func (a *fsAuthority) close() {
	for _, r := range a.open {
		_ = r.Close()
	}
	a.open = map[string]*os.Root{}
}

// readState reads a destination and its permissions through ONE open file, so the bytes and the
// mode a decision is made on cannot come from two different files. The read is bounded: anything
// past the journal limit cannot match a captured checksum anyway.
func (a *fsAuthority) readState(r *os.Root, rel string) (data []byte, perm fs.FileMode, exists bool, err error) {
	f, err := r.Open(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, false, nil
		}
		return nil, 0, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, false, err
	}
	if info.IsDir() {
		// a destination that became a directory is not a third state of a file, it is not a file
		return nil, 0, false, fmt.Errorf("writer: %s is a directory, not the file this operation captured", clip(rel))
	}
	data, err = io.ReadAll(io.LimitReader(f, maxJournaledBytes+1))
	if err != nil {
		return nil, 0, false, err
	}
	return data, info.Mode().Perm(), true, nil
}

// modeMatches compares a captured permission set against what is on disk. An uncaptured mode
// cannot be a precondition, and on a platform that does not carry permission bits neither can a
// captured one.
func modeMatches(want uint32, known bool, got fs.FileMode) bool {
	if !known || !modeIsPrecondition {
		return true
	}
	return fs.FileMode(want).Perm() == got.Perm()
}

// classify says what a destination currently holds relative to one journaled operation. It never
// creates anything: classification is a question, not a step of the mutation.
func (a *fsAuthority) classify(op FileOp) (fileState, error) {
	var (
		raw    []byte
		perm   fs.FileMode
		exists bool
	)
	r, err := a.root(op.Root, false)
	switch {
	case errors.Is(err, errRootAbsent):
		// no root, therefore no file under it
	case err != nil:
		return stateConflict, err
	default:
		if raw, perm, exists, err = a.readState(r, op.rel); err != nil {
			return stateConflict, err
		}
	}
	// postimage first: when a mutation writes the state that is already there, "already done" is
	// the honest answer and rewriting it is work nobody asked for
	if op.Remove {
		if !exists {
			return statePostimage, nil
		}
	} else if exists && sum(raw) == op.PostSum && modeMatches(op.PostMode, op.PostModeKnown, perm) {
		return statePostimage, nil
	}
	if op.Existed {
		if exists && sum(raw) == op.PreimageSum && modeMatches(op.Mode, op.ModeKnown, perm) {
			return statePreimage, nil
		}
	} else if !exists {
		return statePreimage, nil
	}
	return stateConflict, nil
}

// check is one destination's full precondition: it still resolves inside its authorized root, and
// it still holds a state this manifest is allowed to move.
func (a *fsAuthority) check(op FileOp) (fileState, error) {
	if err := confine(op.Root, op.Path); err != nil {
		return stateConflict, fmt.Errorf("writer: destination is no longer confined at mutation time: %w", err)
	}
	st, err := a.classify(op)
	if err != nil {
		return stateConflict, err
	}
	if st == stateConflict {
		return st, fmt.Errorf("%w: %s holds neither the state this operation captured nor the state it would write, so it was changed by something else and is left untouched", ErrConflict, clip(op.Path))
	}
	return st, nil
}

func (a *fsAuthority) writeFile(op FileOp, data []byte, mode fs.FileMode) error {
	r, err := a.root(op.Root, true)
	if errors.Is(err, errRootAbsent) {
		return fmt.Errorf("writer: the ancestor %s was there when this mutation was planned and is not there now, so %s is not written",
			clip(a.m.Roots[op.Root].Anchor), clip(op.Path))
	}
	if err != nil {
		return err
	}
	dir := filepath.Dir(op.rel)
	if dir != "." {
		if err := r.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return atomicWrite(r, dir, op.rel, data, mode)
}

func (a *fsAuthority) removeFile(op FileOp) error {
	r, err := a.root(op.Root, false)
	if errors.Is(err, errRootAbsent) {
		return nil // nothing under a root that does not exist
	}
	if err != nil {
		return err
	}
	if err := r.Remove(op.rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// prepare turns a manifest — freshly built, or read back from the journal — into something this
// package is willing to act on: every name re-derived under its authorized root, then every
// invariant of the format checked. Both halves run at every entry point, because a manifest is
// data from a database and the only thing that stops a hand-edited row from becoming a mutation
// authority is refusing it here.
func prepare(m *Mutation) error {
	if err := normalize(m); err != nil {
		return err
	}
	return validateManifest(m)
}

// normalize re-derives every root-relative name from the persisted pair and refuses anything that
// does not sit under its authorized root. It touches no files, so it is safe to run on a manifest
// loaded from the journal before deciding what to do with it.
func normalize(m *Mutation) error {
	if m.Roots != nil {
		cleaned := make(map[string]RootAuthority, len(m.Roots))
		for root, auth := range m.Roots {
			auth.Root = filepath.Clean(auth.Root)
			auth.Anchor = filepath.Clean(auth.Anchor)
			auth.Suffix = filepath.Clean(auth.Suffix)
			cleaned[filepath.Clean(root)] = auth
		}
		m.Roots = cleaned
	}
	seen := map[string]bool{}
	for i := range m.Ops {
		op := &m.Ops[i]
		op.Root = filepath.Clean(op.Root)
		op.Path = filepath.Clean(op.Path)
		rel, err := relativeTo(op.Root, op.Path)
		if err != nil {
			return err
		}
		if rel == "." {
			return fmt.Errorf("writer: %s is its own authorized root, not a file in it", clip(op.Path))
		}
		op.rel = rel
		key := op.Root + "\x00" + rel
		if seen[key] {
			return fmt.Errorf("writer: mutation touches %s twice", clip(op.Path))
		}
		seen[key] = true
		if !op.Remove && len(op.Content) > maxJournaledBytes {
			return fmt.Errorf("writer: refusing to write %d bytes to %s, above the %d-byte journal limit",
				len(op.Content), clip(op.Path), maxJournaledBytes)
		}
	}
	return nil
}

// validateManifest is the closed half of the format. Everything a replay would rely on is checked
// before the replay can have an effect: the version, that there is an intent at all, that every
// root named by an operation carries an authority and no other does, and that each operation's
// preimage, postimage and modes describe one possible state of one file rather than a combination
// nothing could have produced.
func validateManifest(m *Mutation) error {
	if m.Version != ManifestVersion {
		return fmt.Errorf("writer: this manifest is version %d and this build only acts on version %d, so it is refused rather than interpreted", m.Version, ManifestVersion)
	}
	if len(m.Ops) == 0 {
		return fmt.Errorf("writer: this manifest carries no operation, so there is no intent to apply or undo")
	}
	needed := map[string]bool{}
	for _, op := range m.Ops {
		if err := validateOp(op); err != nil {
			return err
		}
		needed[op.Root] = true
	}
	for root := range needed {
		auth, ok := m.Roots[root]
		if !ok {
			return fmt.Errorf("writer: this manifest writes through %s but records no authority over it", clip(root))
		}
		if err := validateAuthority(root, auth); err != nil {
			return err
		}
	}
	for root := range m.Roots {
		if !needed[root] {
			return fmt.Errorf("writer: this manifest records an authority over %s that none of its operations uses", clip(root))
		}
	}
	return nil
}

func validateAuthority(key string, auth RootAuthority) error {
	if auth.Root != key {
		return fmt.Errorf("writer: the authority filed under %s describes %s instead", clip(key), clip(auth.Root))
	}
	if !filepath.IsAbs(auth.Anchor) {
		return fmt.Errorf("writer: the authority over %s anchors on %s, which is not an absolute path", clip(key), clip(auth.Anchor))
	}
	if !auth.AnchorID.known() {
		return fmt.Errorf("writer: the authority over %s records no identity for its ancestor %s", clip(key), clip(auth.Anchor))
	}
	if auth.Suffix == "" || filepath.IsAbs(auth.Suffix) ||
		auth.Suffix == ".." || strings.HasPrefix(auth.Suffix, ".."+string(filepath.Separator)) {
		return fmt.Errorf("writer: the authority over %s reaches its root through %s, which is not confined to its ancestor", clip(key), clip(auth.Suffix))
	}
	if filepath.Clean(filepath.Join(auth.Anchor, auth.Suffix)) != key {
		return fmt.Errorf("writer: the authority over %s leads to %s instead", clip(key), clip(filepath.Join(auth.Anchor, auth.Suffix)))
	}
	// a root that IS its ancestor is one object: it cannot have been absent, and it cannot have a
	// different identity from the ancestor whose identity was recorded
	if auth.Suffix == "." && auth.RootID != auth.AnchorID {
		return fmt.Errorf("writer: the authority over %s says the root is its own ancestor but records a different identity for it", clip(key))
	}
	return nil
}

func validateOp(op FileOp) error {
	if op.Remove {
		if op.Content != "" || op.PostSum != "" || op.PostModeKnown || op.PostMode != 0 {
			return fmt.Errorf("writer: the operation on %s removes the file and also describes content to leave behind", clip(op.Path))
		}
	} else {
		if op.PostSum != sum([]byte(op.Content)) {
			return fmt.Errorf("writer: the operation on %s carries a postimage checksum that does not match the bytes it would write", clip(op.Path))
		}
		if !op.PostModeKnown {
			return fmt.Errorf("writer: the operation on %s records no permissions for the file it would leave behind", clip(op.Path))
		}
	}
	if op.Existed {
		if op.PreimageSum != sum([]byte(op.Preimage)) {
			return fmt.Errorf("writer: the operation on %s carries a preimage checksum that does not match the preimage it would restore", clip(op.Path))
		}
		if !op.ModeKnown {
			return fmt.Errorf("writer: the operation on %s captured a file but not its permissions, so a rollback could only guess them", clip(op.Path))
		}
		return nil
	}
	if op.Preimage != "" || op.PreimageSum != "" || op.ModeKnown || op.Mode != 0 {
		return fmt.Errorf("writer: the operation on %s says the file did not exist and also describes what it contained", clip(op.Path))
	}
	return nil
}

// decodeManifest reads a manifest back from the journal. The format is CLOSED: an unknown field, a
// missing or unknown version, trailing JSON, or a combination no capture could have produced is
// refused outright and nothing is touched. Interpreting an older or hand-edited shape is how a
// replay becomes a mutation nobody planned — and this decoder guards a path that deletes files.
func decodeManifest(raw string) (Mutation, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Mutation
	if err := dec.Decode(&m); err != nil {
		return Mutation{}, fmt.Errorf("writer: this manifest does not have the shape this build writes: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Mutation{}, fmt.Errorf("writer: this manifest is followed by more JSON, so which of the two is the intent is ambiguous and neither is applied")
	}
	if err := prepare(&m); err != nil {
		return Mutation{}, err
	}
	return m, nil
}

func relativeTo(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", fmt.Errorf("writer: %s is not relative to %s", clip(path), clip(root))
	}
	rel = filepath.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("writer: %s escapes %s", clip(path), clip(root))
	}
	return rel, nil
}

// capture confines every destination, records a checkable authority over every root, and records
// each destination's preimage, permissions and checksums. It runs before the journal entry is
// written, so the manifest that reaches the database is already complete enough to undo — and to
// prove, later, that the world has not moved underneath it.
//
// It creates nothing. A root that does not exist yet stays absent until the intent that needs it
// has been journaled.
func capture(m *Mutation) error {
	m.Version = ManifestVersion
	if m.Roots == nil {
		m.Roots = map[string]RootAuthority{}
	}
	if err := normalize(m); err != nil {
		return err
	}
	a := newCaptureAuthority(m) // authorities are recorded here, not proved
	defer a.close()

	for i := range m.Ops {
		op := &m.Ops[i]
		if err := confine(op.Root, op.Path); err != nil {
			return err
		}
		if op.Remove {
			op.Content, op.PostSum = "", ""
			op.PostMode, op.PostModeKnown = 0, false
		} else {
			op.PostSum = sum([]byte(op.Content))
		}
		var (
			raw    []byte
			perm   fs.FileMode
			exists bool
		)
		r, err := a.root(op.Root, false)
		switch {
		case errors.Is(err, errRootAbsent):
		case err != nil:
			return err
		default:
			if raw, perm, exists, err = a.readState(r, op.rel); err != nil {
				return err
			}
		}
		if exists {
			if len(raw) > maxJournaledBytes {
				return fmt.Errorf("writer: refusing to replace %s: %d bytes cannot be journaled for rollback (limit %d)",
					clip(op.Path), len(raw), maxJournaledBytes)
			}
			op.Existed = true
			op.Preimage = string(raw)
			op.PreimageSum = sum(raw)
			// the mode is a precondition, so failing to read it is a failure to capture, not a
			// detail to shrug off: an unrecorded mode is one this operation would silently widen
			op.Mode, op.ModeKnown = uint32(perm), true
			if !op.Remove {
				// replacing a file keeps the permissions it already had
				op.PostMode, op.PostModeKnown = op.Mode, true
			}
			continue
		}
		op.Existed = false
		op.Preimage, op.PreimageSum = "", ""
		op.Mode, op.ModeKnown = 0, false
		if !op.Remove {
			op.PostMode, op.PostModeKnown = 0o644, true
		}
	}
	return validateManifest(m)
}

// writeMode is the mode an operation's RESULT must carry.
func writeMode(op FileOp) fs.FileMode {
	if op.PostModeKnown {
		return fs.FileMode(op.PostMode).Perm()
	}
	return 0o644
}

// restoreMode is the mode an operation's PREIMAGE had. Restoring the bytes without it would put a
// file back readable by everyone because that is what the umask happened to allow.
func restoreMode(op FileOp) fs.FileMode {
	if op.ModeKnown {
		return fs.FileMode(op.Mode).Perm()
	}
	return 0o644
}

// validateOps proves every precondition of a manifest without writing anything: each destination
// still resolves inside its authorized root, that root is still the directory the manifest
// captured, and each file still holds either the captured preimage or this mutation's own result.
//
// It is separate from applying so that a refusal can be told apart from a failure. An operation
// refused here provably never touched the filesystem, so it can be closed and its resource claims
// released immediately instead of being left for a human to reconcile.
func validateOps(m *Mutation) error {
	if err := prepare(m); err != nil {
		return err
	}
	a := newFSAuthority(m, nil)
	defer a.close()
	return a.validate(m)
}

func (a *fsAuthority) validate(m *Mutation) error {
	for _, op := range m.Ops {
		if _, err := a.check(op); err != nil {
			return err
		}
	}
	return nil
}

// applyOps performs the mutation without a journal to bind a newly created root into. It is the
// path the store-less callers take; every durable one goes through applyMutation with a binder.
func applyOps(m *Mutation) error {
	_, err := applyMutation(m, nil)
	return err
}

// applyMutation performs the mutation and reports whether it changed anything on disk, which is
// what tells a caller whether there is a rollback to drive at all.
//
// Three proofs, not one. The manifest is proved whole before the first byte, so a conflict on the
// third file cannot leave the first two replaced. Then EACH destination is proved again
// immediately before it is replaced, because the global proof is already stale for every file
// after the first write — an editor that changes file N+1 while file N is being written must be
// refused, not overwritten. Then the manifest is proved whole once more.
//
// That last proof is where the filesystem half of the transaction commits. What it establishes is
// that every destination holds exactly what this mutation wrote; what happens to those files
// afterwards is somebody else changing a finished file, and the journal transaction that follows
// does not hold the disk still.
func applyMutation(m *Mutation, bind binder) (bool, error) {
	if err := prepare(m); err != nil {
		return false, err
	}
	a := newFSAuthority(m, bind)
	defer a.close()

	if err := a.validate(m); err != nil {
		return false, err
	}

	mutated := false
	for i := range m.Ops {
		op := m.Ops[i]
		st, err := a.check(op)
		if err != nil {
			return mutated, err
		}
		switch {
		case st == statePostimage:
			// already carries this operation's result: replaying a journal must not rewrite it
		case op.Remove:
			if err := a.removeFile(op); err != nil {
				return mutated, err
			}
			mutated = true
		default:
			if err := a.writeFile(op, []byte(op.Content), writeMode(op)); err != nil {
				return mutated, err
			}
			mutated = true
		}
		if hook := currentApplyHook(); hook != nil {
			if err := hook(i, op); err != nil {
				return mutated, err
			}
		}
	}

	for _, op := range m.Ops {
		st, err := a.check(op)
		if err != nil {
			return mutated, err
		}
		if st != statePostimage {
			return mutated, fmt.Errorf("%w: %s no longer holds what this operation wrote, so it was changed before the decision could be recorded and the decision is not recorded",
				ErrConflict, clip(op.Path))
		}
	}

	for _, n := range m.Notices {
		fmt.Fprint(os.Stderr, n)
	}
	return mutated, nil
}

// unwind restores every recorded preimage, most recent change first. A file that already matches
// its preimage is left alone, so unwinding a mutation that never started is a no-op and unwinding
// twice is safe. A file in a third state — edited by someone while the mutation was in flight, or
// after it finished — is NOT restored: overwriting it would destroy work this operation never had
// a claim on. It is named in the error instead (invariant 7: restore exactly, or stop and say so).
//
// Directories are never removed here. One this mutation created and left empty is untidy; one it
// merely wrote into belongs to the user, and no record kept in a manifest can tell a reconciler
// which is which after a crash reliably enough to justify a deletion.
func unwind(m *Mutation) error {
	if err := prepare(m); err != nil {
		return err
	}
	a := newFSAuthority(m, nil)
	defer a.close()

	var failures []string
	for i := len(m.Ops) - 1; i >= 0; i-- {
		op := m.Ops[i]
		if err := confine(op.Root, op.Path); err != nil {
			failures = append(failures, fmt.Sprintf("%s (%v)", clip(op.Path), err))
			continue
		}
		st, err := a.classify(op)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s (%v)", clip(op.Path), err))
			continue
		}
		switch {
		case st == statePreimage:
			// already the state this operation started from
		case st == stateConflict:
			failures = append(failures, fmt.Sprintf("%s (changed by something else since the mutation started)", clip(op.Path)))
		case op.Existed:
			if err := a.writeFile(op, []byte(op.Preimage), restoreMode(op)); err != nil {
				failures = append(failures, fmt.Sprintf("%s (%v)", clip(op.Path), err))
			}
		default:
			if err := a.removeFile(op); err != nil {
				failures = append(failures, fmt.Sprintf("%s (%v)", clip(op.Path), err))
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("writer: rollback incomplete, these files were left as they are: %s", strings.Join(failures, "; "))
	}
	return nil
}

// atomicWrite replaces a file by renaming a fully-written, fsynced temporary from the same
// directory. A reader sees either the old file or the new one — never a partial one — and the
// rename replaces the final path component itself, so a symlink planted at the destination is
// overwritten rather than followed. Every step is named relative to the root handle, so no
// component of the path can be swapped between the check and the write.
//
// The deferred Remove names the exact file this call created and nothing else. Temper does not
// collect temporaries by prefix: a name is not a proof of ownership, and a process that dies
// mid-write leaves an orphan that is untidy — whereas a sweep would delete a user's own file, or a
// live temporary belonging to another mutation in flight, on nothing but a matching name.
func atomicWrite(r *os.Root, dir, rel string, data []byte, mode fs.FileMode) error {
	tmp := filepath.Join(dir, tempName())
	f, err := r.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() { _ = r.Remove(tmp) }() // no-op once the rename has succeeded
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// O_CREATE's permissions are filtered by the umask; the recorded mode is the contract
	if err := r.Chmod(tmp, mode); err != nil {
		return err
	}
	if err := r.Rename(tmp, rel); err != nil {
		return err
	}
	syncDir(r, dir)
	return nil
}

const (
	tempPrefix = ".temper-"
	tempSuffix = ".tmp"
)

func tempName() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return tempPrefix + hex.EncodeToString(b[:]) + tempSuffix
}

// syncDir asks the filesystem to persist the rename itself. It is BEST-EFFORT and deliberately
// ignores its result: Windows does not allow opening a directory for sync, and several filesystems
// answer without a guarantee. What this package proves is therefore the application-level
// property — a destination holds the old bytes or the new ones, never a mix, and an interrupted
// manifest is replayed or restored at the next start — and not a hardware power-loss guarantee.
func syncDir(r *os.Root, dir string) {
	d, err := r.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// resources names every file this mutation would touch, in canonical form, so the journal can
// reserve them. Identity is the canonical path, not the suggestion: two DIFFERENT suggestions
// appending to the same AGENTS.md are one resource, and each planned its content from a snapshot
// the other is about to invalidate.
func (m *Mutation) resources() ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, op := range m.Ops {
		key, err := resourceKey(op.Path)
		if err != nil {
			return nil, err
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}

func resourceKey(path string) (string, error) {
	canonical, err := canonicalPath(path)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		// NTFS is case-insensitive by default: AGENTS.md and agents.md are the same file, and two
		// claims on "different" spellings would reserve nothing
		canonical = strings.ToLower(canonical)
	}
	return canonical, nil
}

// journal is the part of the store the acceptance saga drives, and nothing more. It exists so the
// commit boundary can be failed deterministically in a test — the one ordering point where "the
// decision landed" and "the decision was reported as landed" can come apart — without a second
// database or a lock held from outside.
type journal interface {
	MarkApplying(id string) error
	RebindRoot(id, manifest string) error
	MarkRollingBack(id, note string) error
	AbandonOperation(id, note string) error
	FinishRollback(id, note string) error
	CommitOperation(id string) error
	GetOperation(id string) (store.Operation, error)
}

// Accept is the durable acceptance path: journal the intent and claim every file it touches,
// mutate the filesystem, then close the decision and the journal entry in one transaction. Every
// interruption lands on a state that Reconcile can finish or restore, so a suggestion is never
// half-accepted.
func Accept(st *store.Store, g store.Suggestion) (string, error) {
	mut, err := BuildMutation(g)
	if err != nil {
		return "", err
	}
	if err := capture(&mut); err != nil {
		return "", err
	}
	op, err := journalEntry(g.ID, "accept", &mut)
	if err != nil {
		return "", err
	}
	op.TargetStatus, op.TargetBody, op.TargetPath = "accepted", g.Body, mut.WrittenPath
	resources, err := mut.resources()
	if err != nil {
		return "", err
	}
	// pending-only AND every-resource-free are enforced inside this transaction, so nothing is
	// written for a suggestion another request already decided, nor for a file another
	// acceptance is in the middle of rewriting
	if err := st.BeginOperation(op, "pending", resources); err != nil {
		return "", err
	}
	if err := runJournaled(st, op.ID, &mut); err != nil {
		return "", err
	}
	return mut.WrittenPath, nil
}

// Undo reverses an accepted suggestion through the same saga, so removing an artifact and putting
// the suggestion back in the inbox cannot come apart either.
func Undo(st *store.Store, g store.Suggestion) error {
	mut, err := compensation(st, g)
	if err != nil {
		return err
	}
	op, err := journalEntry(g.ID, "undo", &mut)
	if err != nil {
		return err
	}
	op.TargetStatus, op.TargetPath = "pending", ""
	resources, err := mut.resources()
	if err != nil {
		return err
	}
	if err := st.BeginOperation(op, "accepted", resources); err != nil {
		return err
	}
	return runJournaled(st, op.ID, &mut)
}

func journalEntry(suggestionID, kind string, mut *Mutation) (store.Operation, error) {
	manifest, err := json.Marshal(mut)
	if err != nil {
		return store.Operation{}, err
	}
	return store.Operation{
		ID: store.NewOperationID(), SuggestionID: suggestionID, Kind: kind, Manifest: string(manifest),
	}, nil
}

// ErrLegacyAcceptance is the refusal to undo an acceptance this build cannot compensate exactly —
// one that predates the journal, or one whose manifest is not the format this build acts on. It is
// a sentinel so a caller can tell "this cannot be undone, here is where to recover it" from "the
// undo failed".
var ErrLegacyAcceptance = errors.New("writer: this acceptance cannot be compensated exactly")

// compensation is what an undo actually applies: the exact inverse of the acceptance that is on
// disk, read back from its journal entry. Recomputing a removal from today's suggestion row would
// only ever delete the artifact — it would not restore a file that existed before, put back the
// CLAUDE.md that had no import line, or return a skill demoted by the budget to the section it was
// evicted from. Those are effects of the acceptance, so they belong to its compensation.
//
// Suggestions accepted before the journal existed have no manifest. For a plain artifact the
// removal is recomputed from the suggestion. For a gardener action it is not: a gardener
// acceptance REPLACED existing block content, nothing recorded what that content was, and
// recomputing a removal would delete a block instead of restoring the one it overwrote. That is
// refused by name rather than approximated — and so is a manifest whose format this build cannot
// read, because "roughly the inverse" is not a property a deletion may rest on.
func compensation(st *store.Store, g store.Suggestion) (Mutation, error) {
	accepted, ok, err := st.LastCommittedAccept(g.ID)
	if err != nil {
		return Mutation{}, err
	}
	if !ok {
		if g.Tool == "gardener" {
			return Mutation{}, fmt.Errorf("%w: gardener action %s replaced existing block content that was never recorded, so undoing it cannot restore what it overwrote; recover it from the git history of %s",
				ErrLegacyAcceptance, g.ID, clip(g.WrittenPath))
		}
		mut, err := BuildRemoval(g)
		if err != nil {
			return Mutation{}, err
		}
		if len(mut.Ops) == 0 {
			return Mutation{}, fmt.Errorf("%w: acceptance %s recorded no artifact and no manifest, so there is nothing to compensate exactly", ErrLegacyAcceptance, g.ID)
		}
		if err := capture(&mut); err != nil {
			return Mutation{}, err
		}
		return mut, nil
	}
	applied, err := decodeManifest(accepted.Manifest)
	if err != nil {
		return Mutation{}, fmt.Errorf("%w: acceptance %s was journaled in a shape this build does not act on (%v), so undoing it could only approximate what it did; recover it from the git history of %s",
			ErrLegacyAcceptance, accepted.ID, err, clip(g.WrittenPath))
	}
	inverse := invert(applied)
	// no capture here on purpose: the state to expect comes from what the acceptance recorded,
	// never from what happens to be on disk now — that is what makes the undo conditional
	if err := prepare(&inverse); err != nil {
		return Mutation{}, err
	}
	return inverse, nil
}

// HasJournaledAcceptance reports whether an accepted suggestion carries the exact manifest that
// was applied, which is what makes an undo a restoration rather than a guess.
func HasJournaledAcceptance(st *store.Store, suggestionID string) (bool, error) {
	_, ok, err := st.LastCommittedAccept(suggestionID)
	return ok, err
}

// invert turns an applied manifest into the operation that puts every file back, most recent
// change first. The acceptance's postimage becomes the state the compensation requires to find,
// and its preimage becomes what the compensation writes — so the same classify() that makes an
// apply idempotent makes an undo conditional: already undone is a no-op, and a file someone edited
// since is a conflict rather than a silent overwrite. The permissions travel the same way round,
// and so does the authority over every root, including the identity bound to one this acceptance
// created.
func invert(m Mutation) Mutation {
	inverse := Mutation{Version: ManifestVersion, Roots: m.Roots, WrittenPath: m.WrittenPath}
	for i := len(m.Ops) - 1; i >= 0; i-- {
		op := m.Ops[i]
		back := FileOp{Root: op.Root, Path: op.Path}
		if op.Remove {
			back.Existed = false
		} else {
			back.Existed = true
			back.Preimage = op.Content
			back.PreimageSum = op.PostSum
			back.Mode, back.ModeKnown = op.PostMode, op.PostModeKnown
		}
		if op.Existed {
			back.Content = op.Preimage
			back.PostSum = op.PreimageSum
			back.PostMode, back.PostModeKnown = op.Mode, op.ModeKnown
		} else {
			back.Remove = true
		}
		inverse.Ops = append(inverse.Ops, back)
	}
	return inverse
}

// runJournaled drives an already-begun operation through applying → committed.
//
// Validation comes first and on its own, before the operation is ever marked applying. A manifest
// that is already stale — captured before another operation committed, then granted the claim the
// moment that one released it — is refused here, having written nothing, and is abandoned straight
// away. Left in applying it would keep its files reserved forever and force a human reconciliation
// for a decision that never touched the disk.
//
// When a mutation that DID write fails, the intention to abandon it is made durable BEFORE the
// restoration starts. Otherwise an operation whose rollback call failed would sit in applying and
// be forward-committed by the next Reconcile — completing, from a crash, a decision that had
// already been given up on. The claims are released only once the restoration is complete.
func runJournaled(j journal, opID string, mut *Mutation) error {
	if err := validateOps(mut); err != nil {
		if rErr := j.AbandonOperation(opID, "refused before any write: "+err.Error()); rErr != nil {
			return fmt.Errorf("%w; and operation %s could not be closed (%v), so it keeps its files reserved until it is reconciled", err, opID, rErr)
		}
		return err
	}
	// committed before the first byte moves: an operation still in "prepared" provably never
	// touched the filesystem, which is what lets Reconcile release it without restoring anything
	if err := j.MarkApplying(opID); err != nil {
		if rErr := j.AbandonOperation(opID, "could not enter applying: "+err.Error()); rErr != nil {
			return fmt.Errorf("%w; and operation %s could not be closed (%v)", err, opID, rErr)
		}
		return err
	}
	mutated, err := applyMutation(mut, func(manifest string) error { return j.RebindRoot(opID, manifest) })
	if err == nil {
		return commitJournaled(j, opID, mut)
	}
	if !mutated {
		// nothing moved, so there is nothing to restore and the claim is free immediately
		if rErr := j.AbandonOperation(opID, err.Error()); rErr != nil {
			return fmt.Errorf("%w; and operation %s could not be closed (%v), so it keeps its files reserved until it is reconciled", err, opID, rErr)
		}
		return err
	}
	return abandon(j, opID, mut, err)
}

// commitJournaled closes the saga, and treats a FAILED commit as a question rather than an answer.
//
// A returned error covers two different worlds. The transaction may never have landed, and then
// the files must go back. Or it may have landed and only the report of it was lost — and undoing
// then would delete the artifact of an acceptance the store already considers made, which is worse
// than the failure it was reacting to. So the operation is re-read, and only "still applying"
// authorizes a rollback. Anything else — the row cannot be read, or it is in a state this path did
// not put it in — is left alone with its claims held, for a reconciliation that can look at the
// whole picture.
func commitJournaled(j journal, opID string, mut *Mutation) error {
	err := j.CommitOperation(opID)
	if err == nil {
		return nil
	}
	op, readErr := j.GetOperation(opID)
	if readErr != nil {
		return fmt.Errorf("%w; and operation %s could not be re-read afterwards (%v), so whether the decision landed is unknown: it stays open and keeps its files reserved until it is reconciled",
			err, opID, readErr)
	}
	switch op.State {
	case store.OpCommitted:
		return nil // the transaction landed; only the answer was lost
	case store.OpApplying:
		return abandon(j, opID, mut, err)
	default:
		return fmt.Errorf("%w; operation %s is %s afterwards and this path will not guess what that means: it keeps its files reserved until it is reconciled",
			err, opID, op.State)
	}
}

// abandon records the decision to give up on a mutation that has already written, then restores.
// The order is the whole point: the durable rolling_back state is what stops a later Reconcile
// from forward-committing an operation whose restoration is what was interrupted.
func abandon(j journal, opID string, mut *Mutation, cause error) error {
	if mErr := j.MarkRollingBack(opID, cause.Error()); mErr != nil {
		return fmt.Errorf("%w; and the intent to roll operation %s back could not be recorded (%v), so it keeps its files reserved until it is reconciled", cause, opID, mErr)
	}
	if uErr := unwind(mut); uErr != nil {
		return fmt.Errorf("%w; %v — operation %s stays open and keeps its files reserved until it is reconciled", cause, uErr, opID)
	}
	if rErr := j.FinishRollback(opID, cause.Error()); rErr != nil {
		return fmt.Errorf("%w; the files were restored but operation %s could not be closed (%v), so it keeps its files reserved until it is reconciled", cause, opID, rErr)
	}
	return cause
}

// Reconcile finishes what a crash interrupted. It is called at startup, before any command reads
// the store as if it were authoritative.
//
//   - prepared: the process died before the first write, so the filesystem was never touched. The
//     operation is released and the suggestion stays exactly as the human left it. Its manifest is
//     never parsed: releasing it touches no file, so a shape this build cannot read must not turn
//     a harmless entry into a permanent blockage.
//   - applying: the human's decision is durable and the target content is in the manifest, so the
//     mutation is replayed to completion — byte-identical to the interrupted run — and committed.
//     If replaying is impossible, the preimages are restored and the decision is released.
//   - rolling_back: the operation had ALREADY been given up on when the process died. It is
//     restored and released, never completed: forward-committing here would resurrect a decision
//     that had been explicitly abandoned.
//
// Both outcomes release the operation's resource claims; an operation that can be neither
// completed nor restored keeps them, because its files are genuinely contested.
//
// It returns one line per operation handled, for the caller to report.
func Reconcile(st *store.Store) ([]string, error) {
	ops, err := st.IncompleteOperations()
	if err != nil {
		return nil, err
	}
	var report []string
	for _, op := range ops {
		if op.State == store.OpPrepared {
			if err := st.AbandonOperation(op.ID, "reconciled: interrupted before any write"); err != nil {
				return report, err
			}
			report = append(report, fmt.Sprintf("%s (%s %s): released, nothing had been written", op.ID, op.Kind, op.SuggestionID))
			continue
		}
		mut, dErr := decodeManifest(op.Manifest)
		if dErr != nil {
			return report, fmt.Errorf("operation %s is %s and its manifest is not a shape this build acts on (%v); no file was touched and the ones it names stay reserved — resolve them by hand, `temper status` lists the operation",
				op.ID, op.State, dErr)
		}
		switch op.State {
		case store.OpRollingBack:
			if uErr := unwind(&mut); uErr != nil {
				return report, fmt.Errorf("operation %s was rolling back when it was interrupted and cannot be restored (%v); the files it names are still reserved — resolve them by hand, `temper status` lists the operation", op.ID, uErr)
			}
			if rErr := st.FinishRollback(op.ID, "reconciled: restoration resumed after an interrupted rollback"); rErr != nil {
				return report, rErr
			}
			report = append(report, fmt.Sprintf("%s (%s %s): restored, its rollback had been interrupted", op.ID, op.Kind, op.SuggestionID))
		case store.OpApplying:
			// an initially-absent root is created and bound before the first target write, so an
			// operation that never bound one provably wrote nothing under it. The directory that
			// happens to sit at that name now was made by someone else, and finishing into it is
			// the one thing this path must not do.
			if unbound := mut.unboundRoot(); unbound != "" {
				line, rErr := restore(st, op, &mut, fmt.Errorf("reconciled: the authorized root %s was never created and bound by this operation, so nothing under it is this operation's to finish", clip(unbound)))
				if rErr != nil {
					return report, rErr
				}
				report = append(report, line)
				continue
			}
			if applyErr := applyOps(&mut); applyErr != nil {
				line, rErr := restore(st, op, &mut, fmt.Errorf("reconciled: %w", applyErr))
				if rErr != nil {
					return report, rErr
				}
				report = append(report, line)
				continue
			}
			if err := st.CommitOperation(op.ID); err != nil {
				return report, err
			}
			report = append(report, fmt.Sprintf("%s (%s %s): completed", op.ID, op.Kind, op.SuggestionID))
		}
	}
	return report, nil
}

// restore takes an applying operation that cannot go forward back to its preimages, recording the
// intent to abandon it first so a second interruption resumes as a rollback rather than a replay.
func restore(st *store.Store, op store.Operation, mut *Mutation, cause error) (string, error) {
	if mErr := st.MarkRollingBack(op.ID, cause.Error()); mErr != nil {
		return "", fmt.Errorf("operation %s can neither be completed (%v) nor marked for rollback (%v); the files it names are still reserved — resolve them by hand", op.ID, cause, mErr)
	}
	if uErr := unwind(mut); uErr != nil {
		return "", fmt.Errorf("operation %s can neither be completed (%v) nor restored (%v); the files it names are still reserved — resolve them by hand, `temper status` lists the operation", op.ID, cause, uErr)
	}
	if rErr := st.FinishRollback(op.ID, cause.Error()); rErr != nil {
		return "", rErr
	}
	return fmt.Sprintf("%s (%s %s): restored, %v", op.ID, op.Kind, op.SuggestionID, cause), nil
}
