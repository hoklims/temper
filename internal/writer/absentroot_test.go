package writer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoklims/temper/internal/store"
)

// An authorized root that does not exist yet — ~/.temper/skills on a fresh machine — cannot be
// identified at capture time, because there is no object to identify. What is captured instead is
// the deepest ancestor that DOES exist, plus the confined suffix leading to the root. The mutation
// may then create that suffix, but only through the ancestor it proved, and it must record which
// object it created before writing a single byte into it.
//
// These tests are the four ways that promise can be broken: the ancestor is substituted, the root
// appears from somewhere else, and a crash lands on either side of the moment the identity becomes
// durable.

// absentRootMutation plans a single file into a root that does not exist yet, and returns the
// captured mutation together with the ancestor it anchored on.
func absentRootMutation(t *testing.T) (mut Mutation, anchor, root, target string) {
	t.Helper()
	base := t.TempDir()
	anchor = filepath.Join(base, "home")
	if err := os.Mkdir(anchor, 0o755); err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(anchor, "temper", "skills")
	target = filepath.Join(root, "rule.md")
	mut = Mutation{Ops: []FileOp{{Root: root, Path: target, Content: "planned before the root existed\n"}}, WrittenPath: target}
	if err := capture(&mut); err != nil {
		t.Fatal(err)
	}
	auth := mut.Roots[filepath.Clean(root)]
	if auth.Anchor != filepath.Clean(anchor) {
		t.Fatalf("capture anchored on %s instead of the deepest existing ancestor %s", auth.Anchor, anchor)
	}
	if !auth.AnchorID.known() {
		t.Fatalf("capture recorded no identity for the ancestor it anchored on: %+v", auth)
	}
	if auth.RootID.known() {
		t.Fatalf("capture claimed an identity for a root that does not exist: %+v", auth)
	}
	if auth.Suffix == "." || filepath.Join(auth.Anchor, auth.Suffix) != filepath.Clean(root) {
		t.Fatalf("the captured suffix does not lead from the ancestor to the root: %+v", auth)
	}
	return mut, anchor, root, target
}

// The ancestor is the only thing an absent root can be proved against, so substituting it is the
// substitution that matters. A different real directory at the ancestor's name would otherwise
// receive the whole mutation — created directories and all — with every pathname agreeing.
func TestAbsentRootRefusesAReplacedAncestor(t *testing.T) {
	mut, anchor, _, _ := absentRootMutation(t)
	base := filepath.Dir(anchor)
	replacement := filepath.Join(base, "replacement")
	if err := os.Mkdir(replacement, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(anchor, filepath.Join(base, "home-original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, anchor); err != nil {
		t.Fatal(err)
	}

	if err := applyOps(&mut); err == nil {
		t.Fatal("a mutation planned against one ancestor created its root inside a different one")
	}
	entries, err := os.ReadDir(anchor)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the mutation wrote into the directory that replaced its ancestor: %v", entries)
	}
	if mut.Roots[filepath.Clean(anchor+string(filepath.Separator)+"temper"+string(filepath.Separator)+"skills")].RootID.known() {
		t.Fatal("a refused mutation bound a root identity anyway")
	}
}

// The root appears between planning and applying, made by someone else. That is allowed — it is
// inside the proved ancestor, so it cannot be a redirection — but it is not silently trusted: the
// mutation records the identity of the object it actually wrote into, and that record is what a
// later rollback or replay is checked against.
func TestAbsentRootBindsTheDirectoryItActuallyWroteInto(t *testing.T) {
	mut, _, root, target := absentRootMutation(t)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	theirs := filepath.Join(root, "theirs.txt")
	if err := os.WriteFile(theirs, []byte("not temper' file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := applyOps(&mut); err != nil {
		t.Fatalf("a root created inside the proved ancestor was refused: %v", err)
	}
	bound := mut.Roots[filepath.Clean(root)].RootID
	if !bound.known() {
		t.Fatal("the mutation wrote through a root whose identity it never recorded")
	}
	actual, err := identityOfName(root)
	if err != nil {
		t.Fatal(err)
	}
	if bound != actual {
		t.Fatalf("the bound identity %q is not the directory that was written into (%q)", bound, actual)
	}
	if got := read(t, target); got != "planned before the root existed\n" {
		t.Fatalf("the artifact was not written: %q", got)
	}
	if got := read(t, theirs); got != "not temper' file\n" {
		t.Fatalf("the mutation disturbed a file it found in the directory: %q", got)
	}
	// and the rollback proves the same object again rather than re-deriving it
	if err := unwind(&mut); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("the rollback left the artifact behind: %v", err)
	}
	if got := read(t, theirs); got != "not temper' file\n" {
		t.Fatalf("the rollback deleted a file it never wrote: %q", got)
	}
}

// The crash lands BEFORE the identity became durable. The operation says "applying", so bytes may
// have moved — but the binding precedes the first target write, so its absence proves nothing was
// written under that root. A directory sitting at the root's name now was made by someone else,
// and finishing the mutation into it is the one thing this path must not do.
func TestReconcileDoesNotAdoptARootItNeverBound(t *testing.T) {
	st, _, g := journalRepo(t)
	mut, _, root, target := absentRootMutation(t)

	op, err := journalEntry(g.ID, "accept", &mut)
	if err != nil {
		t.Fatal(err)
	}
	op.TargetStatus, op.TargetBody, op.TargetPath = "accepted", g.Body, mut.WrittenPath
	resources, err := mut.resources()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.BeginOperation(op, "pending", resources); err != nil {
		t.Fatal(err)
	}
	// the process dies here: past the durable line, before the root was created and bound
	if err := st.MarkApplying(op.ID); err != nil {
		t.Fatal(err)
	}

	// someone else creates that very directory in the meantime and puts their own work in it
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	stranger := filepath.Join(root, "someone-elses.md")
	if err := os.WriteFile(stranger, []byte("made by a human, not by temper\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Reconcile(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 || !strings.Contains(report[0], "restored") {
		t.Fatalf("an operation that never bound its root was not abandoned: %v", report)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("reconciliation finished the mutation inside a directory it never created: %v", err)
	}
	if got := read(t, stranger); got != "made by a human, not by temper\n" {
		t.Fatalf("reconciliation disturbed the directory it found: %q", got)
	}
	stored, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "pending" {
		t.Fatalf("an abandoned operation decided the suggestion anyway: %+v", stored)
	}
	if open, err := st.IncompleteOperations(); err != nil || len(open) != 0 {
		t.Fatalf("the abandoned operation kept its claims: %+v (%v)", open, err)
	}
}

// The crash lands AFTER the identity became durable, which is the whole reason it is made durable
// before the first write: the restart can prove it is finishing its own directory, so the replay
// is allowed to complete.
func TestReconcileCompletesAnOperationWhoseRootWasBoundBeforeItDied(t *testing.T) {
	st, _, g := journalRepo(t)
	mut, _, root, target := absentRootMutation(t)

	op, err := journalEntry(g.ID, "accept", &mut)
	if err != nil {
		t.Fatal(err)
	}
	op.TargetStatus, op.TargetBody, op.TargetPath = "accepted", g.Body, mut.WrittenPath
	resources, err := mut.resources()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.BeginOperation(op, "pending", resources); err != nil {
		t.Fatal(err)
	}

	setApplyHook(t, func(int, FileOp) error {
		panic("simulated process death once the file was written")
	})
	mustCrash(t, func() { _ = runJournaled(st, op.ID, &mut) })
	clearApplyHook()

	// the binding is durable: the journal, not the in-memory manifest, carries the identity
	persisted, err := st.GetOperation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != store.OpApplying {
		t.Fatalf("setup wrong: the operation is %s", persisted.State)
	}
	journaled, err := decodeManifest(persisted.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	bound := journaled.Roots[filepath.Clean(root)].RootID
	if !bound.known() {
		t.Fatalf("the root this operation created was written into without its identity being journaled: %+v", journaled.Roots)
	}
	actual, err := identityOfName(root)
	if err != nil {
		t.Fatal(err)
	}
	if bound != actual {
		t.Fatalf("the journaled identity %q is not the directory on disk (%q)", bound, actual)
	}

	report, err := Reconcile(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 || !strings.Contains(report[0], "completed") {
		t.Fatalf("an operation that had bound its root was not completed: %v", report)
	}
	if got := read(t, target); got != "planned before the root existed\n" {
		t.Fatalf("the replayed mutation lost its content: %q", got)
	}
	stored, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "accepted" {
		t.Fatalf("the replayed decision was not recorded: %+v", stored)
	}
	if open, err := st.IncompleteOperations(); err != nil || len(open) != 0 {
		t.Fatalf("the completed operation kept its claims: %+v (%v)", open, err)
	}
}
