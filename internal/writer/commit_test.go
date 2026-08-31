package writer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoklims/temper/internal/store"
)

// The last ordering point of the saga: the files are on disk and proved, and the journal
// transaction that records the decision has yet to return. It can fail in two shapes that look
// identical from the caller's side, and treating them the same is how a decision gets lost or an
// artifact gets deleted out from under one that was actually made.
//
//   - the transaction never landed. The files must go back, the claims must be released, and the
//     suggestion must be decidable again with no manual step.
//   - the transaction landed and only the ANSWER was lost. Undoing then would delete the artifact
//     of an acceptance the store already considers made.
//
// The store is reached through the small journal interface for exactly this: the boundary can be
// failed on purpose, on either side, without a lock taken from outside the process.

// flakyCommit fails the commit boundary once, on the side chosen by afterTransaction.
type flakyCommit struct {
	*store.Store
	afterTransaction bool
	before           func()
	fired            bool
}

var errInjectedCommit = errors.New("injected: the journal transaction did not report success")

func (f *flakyCommit) CommitOperation(id string) error {
	if f.fired {
		return f.Store.CommitOperation(id)
	}
	f.fired = true
	if f.afterTransaction {
		if err := f.Store.CommitOperation(id); err != nil {
			return err
		}
	}
	if f.before != nil {
		f.before()
	}
	return errInjectedCommit
}

// beginAccept plans an acceptance and journals its intent, stopping short of driving it. It is the
// same sequence Accept performs, split so a test can supply its own journal.
func beginAccept(t *testing.T, st *store.Store, g store.Suggestion) (Mutation, string) {
	t.Helper()
	mut, err := BuildMutation(g)
	if err != nil {
		t.Fatal(err)
	}
	if err := capture(&mut); err != nil {
		t.Fatal(err)
	}
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
	return mut, op.ID
}

// The transaction never landed. Everything the mutation wrote goes back, the claims are released,
// and a retry needs no reconciliation — the failure is a lost race, not a broken machine.
func TestCommitFailureBeforeTheDecisionRollsEverythingBack(t *testing.T) {
	st, repo, g := journalRepo(t)
	before := snapshotTree(t, repo)
	mut, opID := beginAccept(t, st, g)

	err := runJournaled(&flakyCommit{Store: st}, opID, &mut)
	if !errors.Is(err, errInjectedCommit) {
		t.Fatalf("the injected commit failure was not reported, got %v", err)
	}

	assertSameTree(t, "rolled-back commit failure", before, snapshotTree(t, repo))
	stored, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "pending" || stored.WrittenPath != "" {
		t.Fatalf("a failed commit decided the suggestion anyway: %+v", stored)
	}
	op, err := st.GetOperation(opID)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != store.OpRolledBack {
		t.Fatalf("a failed commit left the operation %s", op.State)
	}
	if open, err := st.IncompleteOperations(); err != nil || len(open) != 0 {
		t.Fatalf("a rolled-back commit failure kept its claims: %+v (%v)", open, err)
	}
	// a fresh retry moves, with no manual step in between
	if _, err := Accept(st, stored); err != nil {
		t.Fatalf("a retry after a rolled-back commit failure still needs reconciliation: %v", err)
	}
	got := read(t, filepath.Join(repo, "AGENTS.md"))
	if !strings.Contains(got, "id="+g.ID) {
		t.Fatalf("the retry did not write the block:\n%s", got)
	}
}

// The transaction landed and the answer was lost. Re-reading the operation is what distinguishes
// this from the case above, and it is the difference between reporting a decision that was made
// and deleting the artifact of one.
func TestCommitErrorAfterTheDecisionLandedIsRecognisedAsCommitted(t *testing.T) {
	st, repo, g := journalRepo(t)
	mut, opID := beginAccept(t, st, g)

	if err := runJournaled(&flakyCommit{Store: st, afterTransaction: true}, opID, &mut); err != nil {
		t.Fatalf("a decision that is durable was reported as a failure: %v", err)
	}

	stored, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "accepted" || stored.WrittenPath != mut.WrittenPath {
		t.Fatalf("the durable decision was not the one reported: %+v", stored)
	}
	got := read(t, filepath.Join(repo, "AGENTS.md"))
	if !strings.Contains(got, "id="+g.ID) {
		t.Fatalf("the accepted artifact was rolled back under a decision that had landed:\n%s", got)
	}
	if !strings.Contains(read(t, filepath.Join(repo, "CLAUDE.md")), "@AGENTS.md") {
		t.Fatal("the acceptance's second file was rolled back under a decision that had landed")
	}
	op, err := st.GetOperation(opID)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != store.OpCommitted {
		t.Fatalf("the operation is %s after a decision that landed", op.State)
	}
	if open, err := st.IncompleteOperations(); err != nil || len(open) != 0 {
		t.Fatalf("a committed operation kept its claims: %+v (%v)", open, err)
	}
}

// The compensation is conditional, and it stays conditional under failure. A file changed after
// the mutation finished writing it is in a third state: the rollback no longer knows what those
// bytes mean, so it refuses to restore it, says which file it stopped on, and keeps the operation
// open with its claims — the file is genuinely contested and handing it to the next acceptance
// would lose the change the refusal just protected.
func TestCommitFailureDoesNotOverwriteAThirdImageAndKeepsItsClaims(t *testing.T) {
	st, repo, g := journalRepo(t)
	agents := filepath.Join(repo, "AGENTS.md")
	claude := filepath.Join(repo, "CLAUDE.md")
	edit := "# AGENTS.md\n\nchanged by someone else while the decision was being recorded\n"
	mut, opID := beginAccept(t, st, g)

	j := &flakyCommit{Store: st, before: func() {
		if err := os.WriteFile(agents, []byte(edit), 0o644); err != nil {
			t.Error(err)
		}
	}}
	err := runJournaled(j, opID, &mut)
	if err == nil {
		t.Fatal("a rollback that could not restore a file reported success")
	}
	if !strings.Contains(err.Error(), "AGENTS.md") {
		t.Fatalf("the failure must name the file it refused to restore: %v", err)
	}

	if got := read(t, agents); got != edit {
		t.Fatalf("the rollback overwrote a change it did not make:\n%s", got)
	}
	// the file it COULD account for was restored; only the contested one was left
	if got := read(t, claude); strings.Contains(got, "@AGENTS.md") {
		t.Fatalf("a file the rollback could restore was left mutated:\n%s", got)
	}
	stored, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "pending" {
		t.Fatalf("a failed commit decided the suggestion anyway: %+v", stored)
	}
	open, err := st.IncompleteOperations()
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ID != opID || open[0].State != store.OpRollingBack {
		t.Fatalf("an unresolvable rollback must stay open as a rollback with its claims, got %+v", open)
	}
	// and nothing else may take the contested file while it is open
	second := g
	second.ID = "sg_third_image_contender"
	second.Title = "Second rule aimed at the same file"
	second.Body = "- second body"
	if err := st.InsertSuggestion(second); err != nil {
		t.Fatal(err)
	}
	if _, err := Accept(st, second); !errors.Is(err, store.ErrResourceBusy) {
		t.Fatalf("a contested file was handed to another acceptance, got %v", err)
	}
}

// editingJournal makes a change immediately before the journal transaction — that is, after the
// filesystem half of the mutation has already committed.
type editingJournal struct {
	*store.Store
	edit func()
	once bool
}

func (e *editingJournal) CommitOperation(id string) error {
	if !e.once {
		e.once = true
		e.edit()
	}
	return e.Store.CommitOperation(id)
}

// Where the transaction actually ends, stated as an oracle rather than as a hope.
//
// The last whole-manifest validation is the commit of the filesystem half: every destination was
// proved to hold exactly what this mutation wrote. A change landing AFTER that is somebody editing
// a file temper had finished writing, and the journal transaction that follows does not hold
// the user's disk still. This package does not claim otherwise — the decision is recorded, because
// it was true when it was made.
//
// What it does guarantee is the other half, and this test asserts both together: the edit is never
// overwritten, and the compensation that could overwrite it later refuses instead.
func TestAnEditAfterTheFilesystemCommitIsAPostCommitChange(t *testing.T) {
	st, repo, g := journalRepo(t)
	agents := filepath.Join(repo, "AGENTS.md")
	edit := "# AGENTS.md\n\nedited after temper had finished writing every file\n"
	mut, opID := beginAccept(t, st, g)

	j := &editingJournal{Store: st, edit: func() {
		if err := os.WriteFile(agents, []byte(edit), 0o644); err != nil {
			t.Error(err)
		}
	}}
	if err := runJournaled(j, opID, &mut); err != nil {
		t.Fatalf("a mutation that finished and proved every destination was not recorded: %v", err)
	}

	stored, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "accepted" {
		t.Fatalf("the decision was not recorded: %+v", stored)
	}
	if open, err := st.IncompleteOperations(); err != nil || len(open) != 0 {
		t.Fatalf("the committed operation kept its claims: %+v (%v)", open, err)
	}
	// the post-commit change is a change to a finished file, and it is left exactly as it is
	if got := read(t, agents); got != edit {
		t.Fatalf("the commit path overwrote a change made after the last validation:\n%s", got)
	}

	// and the undo that could destroy it does not: the file is in a third state, so the
	// compensation refuses rather than restoring bytes whose meaning it no longer knows
	accepted, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := Undo(st, accepted); !errors.Is(err, ErrConflict) {
		t.Fatalf("undoing over a file changed after the acceptance must conflict, got %v", err)
	}
	if got := read(t, agents); got != edit {
		t.Fatalf("the undo overwrote the post-commit change:\n%s", got)
	}
	if open, err := st.IncompleteOperations(); err != nil || len(open) != 0 {
		t.Fatalf("an undo refused before any write stranded its claims: %+v (%v)", open, err)
	}
}

// A rollback that was interrupted halfway is resumed as a rollback, never replayed forward. One
// preimage is already back when the process dies; reconciliation must skip that one, restore the
// rest, and release the claims without resurrecting the acceptance that had been given up on.
func TestInterruptedRollbackAfterOneRestoreConverges(t *testing.T) {
	st, repo, g := journalRepo(t)
	before := snapshotTree(t, repo)
	mut, opID := beginAccept(t, st, g)

	if err := st.MarkApplying(opID); err != nil {
		t.Fatal(err)
	}
	if _, err := applyMutation(&mut, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRollingBack(opID, "given up on, then interrupted mid-restoration"); err != nil {
		t.Fatal(err)
	}

	// exactly one preimage is put back, then the process dies
	last := mut.Ops[len(mut.Ops)-1]
	a := newFSAuthority(&mut, nil)
	var restoreErr error
	if last.Existed {
		restoreErr = a.writeFile(last, []byte(last.Preimage), restoreMode(last))
	} else {
		restoreErr = a.removeFile(last)
	}
	a.close()
	if restoreErr != nil {
		t.Fatal(restoreErr)
	}

	report, err := Reconcile(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 || !strings.Contains(report[0], "restored") {
		t.Fatalf("a partially restored rollback was not completed: %v", report)
	}
	assertSameTree(t, "resumed rollback", before, snapshotTree(t, repo))
	stored, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "pending" || stored.WrittenPath != "" {
		t.Fatalf("an interrupted rollback was forward-committed: %+v", stored)
	}
	op, err := st.GetOperation(opID)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != store.OpRolledBack {
		t.Fatalf("the resumed rollback left the operation %s", op.State)
	}
	if open, err := st.IncompleteOperations(); err != nil || len(open) != 0 {
		t.Fatalf("the resumed rollback kept its claims: %+v (%v)", open, err)
	}
}
