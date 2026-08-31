package writer

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hoklims/temper/internal/store"
)

// This file holds the oracles for the authority a mutation acts under: which directory it is
// allowed to write through, when that permission was last proved, and what counts as "the state
// the manifest captured". Each test describes a world that moved between two moments the writer
// used to treat as one.

// Pinning every child lookup to an os.Root is not enough on its own: capture closes its handles
// and a later apply reopens the root BY NAME. Renaming the root away and leaving a link to an
// attacker-controlled directory in its place hands that directory the authority — and
// re-canonicalizing the new (root, path) pair agrees with itself, so only an identity recorded
// BEFORE the swap can contradict it.
func TestAuthorizedRootCannotBeSwappedAfterCapture(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	outside := filepath.Join(parent, "outside")
	parked := filepath.Join(parent, "repo-original")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	// the same preimage on both sides, so a checksum precondition cannot tell them apart: the only
	// thing that distinguishes them is which directory this mutation was captured against
	original := "# AGENTS.md\n\nidentical preimage\n"
	insidePath := filepath.Join(repo, "AGENTS.md")
	outsidePath := filepath.Join(outside, "AGENTS.md")
	if err := os.WriteFile(insidePath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsidePath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	mut := Mutation{
		Ops:         []FileOp{{Root: repo, Path: insidePath, Content: "temper replacement\n"}},
		WrittenPath: insidePath,
	}
	if err := capture(&mut); err != nil {
		t.Fatal(err)
	}
	if !mut.Roots[filepath.Clean(repo)].RootID.known() {
		t.Fatalf("capture recorded no identity for the authorized root: %+v", mut.Roots)
	}

	if err := os.Rename(repo, parked); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, repo); err != nil {
		t.Fatalf("create directory symlink witness: %v", err)
	}

	if err := applyOps(&mut); err == nil {
		t.Fatal("reopening a swapped authorized root by name wrote through the replacement link")
	}
	if got := read(t, outsidePath); got != original {
		t.Fatalf("the root swap redirected the mutation outside the captured authority: %q", got)
	}

	// the rollback path reopens the same root by the same name, so it needs the same proof
	if err := unwind(&mut); err == nil {
		t.Fatal("rolling back through a swapped authorized root was allowed")
	}
	if got := read(t, outsidePath); got != original {
		t.Fatalf("the rollback destroyed a file outside the captured authority: %q", got)
	}
	// and a manifest that carries no authority at all cannot borrow one from the filesystem
	mut.Roots = nil
	if err := applyOps(&mut); err == nil {
		t.Fatal("a manifest with no recorded root authority must not be applied")
	}
}

// The substitution a canonical pathname cannot see: not a symlink, a genuinely different real
// directory renamed into the root's place. Both sides re-canonicalize to the same string and both
// hold the same preimage bytes, so path comparison, checksum comparison and any stat-by-name
// comparison all agree — with the attacker's directory.
func TestSamePathRootObjectReplacementIsRefused(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	parked := filepath.Join(parent, "repo-original")
	replacement := filepath.Join(parent, "replacement")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(replacement, 0o755); err != nil {
		t.Fatal(err)
	}
	original := "identical preimage\n"
	target := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(replacement, "AGENTS.md"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	mut := Mutation{Ops: []FileOp{{Root: root, Path: target, Content: "temper wrote here\n"}}, WrittenPath: target}
	if err := capture(&mut); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, parked); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, root); err != nil {
		t.Fatal(err)
	}

	if err := applyOps(&mut); err == nil {
		t.Fatal("a different directory at the captured root's name was accepted as the authority")
	}
	if got := read(t, filepath.Join(root, "AGENTS.md")); got != original {
		t.Fatalf("the mutation wrote into the directory that replaced its root: %q", got)
	}
	// the directory that WAS the root is equally untouched: refusing means both objects survive
	if got := read(t, filepath.Join(parked, "AGENTS.md")); got != original {
		t.Fatalf("the mutation wrote into the parked original root: %q", got)
	}
	// the rollback path reopens the same name and needs the same refusal
	if err := unwind(&mut); err == nil {
		t.Fatal("rolling back through the replacement directory was allowed")
	}
	if got := read(t, filepath.Join(root, "AGENTS.md")); got != original {
		t.Fatalf("the rollback wrote into the directory that replaced its root: %q", got)
	}
}

// The same substitution, but staged after the authority has already been opened and cached. A
// handle proved once is a proof about the world as it was then, and a mutation revalidates every
// destination after its last write; a cached handle that skips the proof would let the second half
// of the mutation run against a directory the first half never saw.
func TestCachedRootAuthorityIsReprovedAfterTheNameIsSwapped(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	parked := filepath.Join(parent, "repo-original")
	replacement := filepath.Join(parent, "replacement")
	for _, dir := range []string{root, replacement} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	first := filepath.Join(root, "AGENTS.md")
	second := filepath.Join(root, "CLAUDE.md")
	if err := os.WriteFile(first, []byte("first old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// the replacement is a perfect double: same names, same bytes, so only identity can tell them
	// apart
	if err := os.WriteFile(filepath.Join(replacement, "AGENTS.md"), []byte("first old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(replacement, "CLAUDE.md"), []byte("second old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mut := Mutation{Ops: []FileOp{
		{Root: root, Path: first, Content: "first new\n"},
		{Root: root, Path: second, Content: "second new\n"},
	}, WrittenPath: first}
	if err := capture(&mut); err != nil {
		t.Fatal(err)
	}

	// the swap lands after the authority has been opened, used and cached for the first write
	var swapErr error
	setApplyHook(t, func(index int, _ FileOp) error {
		if index == 0 {
			if swapErr = os.Rename(root, parked); swapErr != nil {
				return nil
			}
			swapErr = os.Rename(replacement, root)
		}
		return nil
	})
	err := applyOps(&mut)
	clearApplyHook()
	if swapErr != nil {
		t.Skipf("this platform did not allow staging the swap while the authority was held: %v", swapErr)
	}
	if err == nil {
		t.Fatal("a root swapped while its authority was cached was still used for the rest of the mutation")
	}
	if got := read(t, filepath.Join(root, "CLAUDE.md")); got != "second old\n" {
		t.Fatalf("the mutation continued into the replacement directory: %q", got)
	}
	if got := read(t, filepath.Join(parked, "AGENTS.md")); got != "first new\n" {
		t.Fatalf("setup wrong: the first write had not landed in the original root when the swap happened: %q", got)
	}
}

// A preflight that classifies every destination is already stale for every file after the first
// write. An editor that changes destination N+1 while destination N is being replaced must be
// refused at N+1's turn, not overwritten by a proof taken before the mutation started.
func TestLaterDestinationEditedAfterThePreflightIsNotOverwritten(t *testing.T) {
	repo := t.TempDir()
	first := filepath.Join(repo, "AGENTS.md")
	second := filepath.Join(repo, "CLAUDE.md")
	if err := os.WriteFile(first, []byte("first old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mut := Mutation{Ops: []FileOp{
		{Root: repo, Path: first, Content: "first new\n"},
		{Root: repo, Path: second, Content: "second new\n"},
	}, WrittenPath: first}
	if err := capture(&mut); err != nil {
		t.Fatal(err)
	}

	thirdParty := "second changed after the global preflight\n"
	var editErr error
	setApplyHook(t, func(index int, _ FileOp) error {
		if index == 0 {
			editErr = os.WriteFile(second, []byte(thirdParty), 0o644)
		}
		return nil
	})
	err := applyOps(&mut)
	clearApplyHook()
	if editErr != nil {
		t.Fatalf("inject the concurrent edit: %v", editErr)
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a later destination changed after the preflight must conflict, got %v", err)
	}
	if got := read(t, second); got != thirdParty {
		t.Fatalf("the later destination's edit was overwritten: %q", got)
	}
	// the first file really had been replaced, so the refusal came from the revalidation and not
	// from a mutation that never started
	if got := read(t, first); got != "first new\n" {
		t.Fatalf("setup wrong: the mutation had not started when the edit landed: %q", got)
	}
}

// The last write leaves the manifest unproved as a whole again. An edit landing between it and the
// decision must not be committed over: the journal would say "accepted" about a file that no
// longer holds what the acceptance wrote.
func TestEditAfterTheLastWriteIsNotCommittedOver(t *testing.T) {
	st, repo, g := journalRepo(t)
	agents := filepath.Join(repo, "AGENTS.md")
	claude := filepath.Join(repo, "CLAUDE.md")
	claudeBefore := read(t, claude)
	edit := "# AGENTS.md\n\nedited after temper wrote its last file\n"

	// the edit has to land after the LAST write, so the test asserts which operation that is
	// instead of assuming it
	planned, err := BuildMutation(g)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(planned.Ops); n < 2 || filepath.Base(planned.Ops[n-1].Path) != "CLAUDE.md" {
		t.Fatalf("setup wrong: CLAUDE.md is not this mutation's last operation: %+v", planned.Ops)
	}

	var editErr error
	setApplyHook(t, func(_ int, op FileOp) error {
		if filepath.Base(op.Path) == "CLAUDE.md" {
			editErr = os.WriteFile(agents, []byte(edit), 0o644)
		}
		return nil
	})
	_, err = Accept(st, g)
	clearApplyHook()
	if editErr != nil {
		t.Fatalf("inject the concurrent edit: %v", editErr)
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("an edit landing after the last write must refuse the decision, got %v", err)
	}

	if got := read(t, agents); got != edit {
		t.Fatalf("the edit was overwritten by the commit path:\n%s", got)
	}
	// the file the mutation could still restore was restored; the contested one was not touched
	if got := read(t, claude); got != claudeBefore {
		t.Fatalf("CLAUDE.md was left mutated by a decision that was refused:\n%s", got)
	}
	stored, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "pending" || stored.WrittenPath != "" {
		t.Fatalf("the decision was committed over a file that had changed: %+v", stored)
	}
	// the operation could not finish its restoration, so it stays open — and it stays open as a
	// rollback, which is what stops the next reconciliation from replaying it forward
	open, err := st.IncompleteOperations()
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].State != store.OpRollingBack {
		t.Fatalf("an abandoned mutation must stay open as a rollback, got %+v", open)
	}
}

// Permissions are part of a destination's state, not metadata about it. A concurrent chmod leaves
// the bytes untouched, so a writer that compares only checksums replaces the file and silently
// discards the permission change.
func TestConcurrentModeChangeIsRefusedNotOverwritten(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reflects only a read-only attribute; the portable half of this invariant is TestManifestDistinguishesAnAbsentFileFromModeZero")
	}
	repo := t.TempDir()
	agents := filepath.Join(repo, "AGENTS.md")
	original := "# AGENTS.md\n\noriginal user text\n"
	if err := os.WriteFile(agents, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	mut, err := BuildMutation(suggestion(repo))
	if err != nil {
		t.Fatal(err)
	}
	if err := capture(&mut); err != nil {
		t.Fatal(err)
	}

	// only the permissions move: every captured checksum still matches
	if err := os.Chmod(agents, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := applyOps(&mut); !errors.Is(err, ErrConflict) {
		t.Fatalf("a concurrent permission change must be classified as a conflict, got %v", err)
	}
	info, err := os.Stat(agents)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the refused mutation widened the mode to %v", info.Mode().Perm())
	}
	if got := read(t, agents); got != original {
		t.Fatalf("the refused mutation wrote anyway: %q", got)
	}
}

// The portable half of the same invariant, provable on every platform: a mode of zero and an
// absent mode are two different facts in the manifest. A single zero value used to mean both,
// which made a concurrent chmod to 000 invisible and restored an owner-only file world-readable.
func TestManifestDistinguishesAnAbsentFileFromModeZero(t *testing.T) {
	repo := t.TempDir()
	agents := filepath.Join(repo, "AGENTS.md")
	if err := os.WriteFile(agents, []byte("# AGENTS.md\n\nHand-written intro.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	existing, err := BuildMutation(suggestion(repo))
	if err != nil {
		t.Fatal(err)
	}
	if err := capture(&existing); err != nil {
		t.Fatal(err)
	}
	var replaced *FileOp
	for i := range existing.Ops {
		if filepath.Base(existing.Ops[i].Path) == "AGENTS.md" {
			replaced = &existing.Ops[i]
		}
	}
	if replaced == nil {
		t.Fatal("the mutation does not touch AGENTS.md")
	}
	if !replaced.Existed || !replaced.ModeKnown {
		t.Fatalf("an existing destination's mode was not captured: %+v", *replaced)
	}
	if !replaced.PostModeKnown || replaced.PostMode != replaced.Mode {
		t.Fatalf("replacing a file must keep the permissions it had: %+v", *replaced)
	}

	fresh := suggestion(repo)
	fresh.Placement = "skill"
	created, err := BuildMutation(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := capture(&created); err != nil {
		t.Fatal(err)
	}
	if len(created.Ops) != 1 {
		t.Fatalf("expected a single-file mutation, got %+v", created.Ops)
	}
	op := created.Ops[0]
	if op.Existed || op.ModeKnown || op.Mode != 0 {
		t.Fatalf("an absent destination must record no mode at all, not mode zero: %+v", op)
	}
	// and "no captured mode" is never the write contract: the postimage carries a mode of its own
	if !op.PostModeKnown || op.PostMode == 0 {
		t.Fatalf("a created file has no defined postimage mode: %+v", op)
	}
}

// A rollback restores files. It does not remove directories — not the ones the user had, and not
// the ones this mutation created either.
//
// The deliberate part is the second half. A directory this mutation made and left empty looks safe
// to delete, but the record that says "I created it" is written before the mutation runs, and
// between that record and the rollback anything can have been put inside — including by a user who
// created that very directory a second later for their own reasons. An empty directory left behind
// is untidy; a deleted one is not recoverable, and no manifest can tell the two cases apart after
// a crash. So the trade is made once, in the safe direction.
func TestRollbackRestoresFilesAndRemovesNoDirectory(t *testing.T) {
	repo := t.TempDir()
	preexisting := filepath.Join(repo, ".cursor")
	if err := os.Mkdir(preexisting, 0o755); err != nil {
		t.Fatal(err)
	}

	g := suggestion(repo)
	g.Placement = "skill" // <repo>/.cursor/skills/temper-<slug>/SKILL.md
	mut, err := BuildMutation(g)
	if err != nil {
		t.Fatal(err)
	}
	if err := capture(&mut); err != nil {
		t.Fatal(err)
	}
	if err := applyOps(&mut); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mut.WrittenPath); err != nil {
		t.Fatalf("setup wrong: the artifact was not written: %v", err)
	}

	// a directory the user creates AFTER the capture, inside the tree this mutation is about to
	// unwind: nothing in the manifest knows about it, and nothing may remove it
	userDir := filepath.Join(repo, ".cursor", "skills", "handwritten")
	if err := os.Mkdir(userDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := unwind(&mut); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mut.WrittenPath); !os.IsNotExist(err) {
		t.Fatalf("the rollback left the file it had written: %v", err)
	}
	for _, dir := range []string{repo, preexisting, filepath.Join(repo, ".cursor", "skills"), userDir, filepath.Dir(mut.WrittenPath)} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("the rollback removed the directory %s: %v", dir, err)
		}
	}
	// nothing this mutation wrote is left as a file, including its own temporaries
	rest, err := os.ReadDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rest {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			t.Fatalf("the rollback left a temporary file behind: %s", e.Name())
		}
	}
}
