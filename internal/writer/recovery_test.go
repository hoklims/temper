package writer

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoklims/temper/internal/store"
)

// This file holds the oracles for what happens after a decision stops going forward: a manifest
// that was already stale when it finally got its claim, an operation that was given up on before
// the process died, and an undo that has to restore what it overwrote rather than delete it.

// The other legal interleaving of two acceptances: both captured the same bytes, but the first
// commits before the second takes the durable claim. The second is stale, it refuses before its
// first write — so it provably touched nothing, and leaving it "applying" would reserve its files
// forever and demand a human reconciliation for a decision that never happened.
func TestStaleCaptureRefusedBeforeAnyWriteDoesNotStrandTheClaim(t *testing.T) {
	st, repo, first := journalRepo(t)
	agents := filepath.Join(repo, "AGENTS.md")

	second := first
	second.ID = "sg_stale_capture"
	second.Title = "Rule planned before the other one committed"
	second.Body = "- planned against the older AGENTS.md"
	if err := st.InsertSuggestion(second); err != nil {
		t.Fatal(err)
	}

	// captured while AGENTS.md still held the pre-acceptance bytes
	stale, err := BuildMutation(second)
	if err != nil {
		t.Fatal(err)
	}
	if err := capture(&stale); err != nil {
		t.Fatal(err)
	}
	if _, err := Accept(st, first); err != nil {
		t.Fatal(err)
	}

	// the claim is free again, so the stale operation is admitted into the journal: the conflict is
	// only discovered once its manifest is checked against the disk
	op, err := journalEntry(second.ID, "accept", &stale)
	if err != nil {
		t.Fatal(err)
	}
	op.TargetStatus, op.TargetBody, op.TargetPath = "accepted", second.Body, stale.WrittenPath
	resources, err := stale.resources()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.BeginOperation(op, "pending", resources); err != nil {
		t.Fatalf("setup: the committed acceptance did not release its claim: %v", err)
	}

	if err := runJournaled(st, op.ID, &stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale manifest must be refused as a conflict, got %v", err)
	}
	open, err := st.IncompleteOperations()
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("a pre-write refusal stranded an operation and its resource claims: %+v", open)
	}
	if got := read(t, agents); !strings.Contains(got, "id="+first.ID) || strings.Contains(got, "id="+second.ID) {
		t.Fatalf("the refused operation wrote anyway:\n%s", got)
	}

	// and a fresh retry moves without any manual reconciliation
	if _, err := Accept(st, second); err != nil {
		t.Fatalf("a retry after a clean refusal still needs reconciliation: %v", err)
	}
	got := read(t, agents)
	if !strings.Contains(got, "id="+first.ID) || !strings.Contains(got, "id="+second.ID) {
		t.Fatalf("the retry lost a block:\n%s", got)
	}
}

// An operation that had already been given up on when the process died must be restored, never
// completed. Without a durable rolling_back state it would still read "applying", and the next
// reconciliation would replay it forward — finishing, from a crash, a decision that had been
// explicitly abandoned.
func TestReconcileNeverForwardCommitsAnAbandonedOperation(t *testing.T) {
	st, repo, g := journalRepo(t)
	before := snapshotTree(t, repo)

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
	if err := st.MarkApplying(op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := applyMutation(&mut, nil); err != nil {
		t.Fatal(err)
	}
	// every file is on disk, then the operation is abandoned and the process dies between recording
	// that intent and finishing the restoration
	if err := st.MarkRollingBack(op.ID, "given up on, then interrupted"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(repo, "AGENTS.md")); !strings.Contains(got, "id="+g.ID) {
		t.Fatalf("setup wrong: the abandoned mutation had not been applied:\n%s", got)
	}

	report, err := Reconcile(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 || !strings.Contains(report[0], "restored") {
		t.Fatalf("an abandoned operation was not restored: %v", report)
	}
	assertSameTree(t, "interrupted rollback", before, snapshotTree(t, repo))
	stored, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "pending" || stored.WrittenPath != "" {
		t.Fatalf("reconciliation completed a decision that had been abandoned: %+v", stored)
	}
	if open, err := st.IncompleteOperations(); err != nil || len(open) != 0 {
		t.Fatalf("the restored operation kept its claims: %+v (%v)", open, err)
	}
	// and the suggestion is decidable again, from the state the human left it in
	if _, err := Accept(st, stored); err != nil {
		t.Fatalf("a restored suggestion could not be accepted again: %v", err)
	}
}

// A gardener acceptance REPLACES an existing block. Undoing it is a restoration, so it is available
// exactly when the acceptance recorded what it overwrote — and refused, by name, when it did not.
// Recomputing a removal in that second case would delete a block instead of putting back the one it
// replaced, which is why "gardener" used to be refused outright.
func TestGardenerUndoRestoresAJournaledAcceptanceAndRefusesALegacyOne(t *testing.T) {
	repo := t.TempDir()
	agents := filepath.Join(repo, "AGENTS.md")

	original := suggestion(repo)
	original.ID = "sg_gardened_original"
	original.Title = "Use pnpm, never npm"
	if _, err := writeUnjournaled(original); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, repo)

	st, err := store.Open(filepath.Join(t.TempDir(), "temper.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	amend := store.Suggestion{
		ID: "sg_gardener_amend", BlockID: original.ID, CreatedAt: time.Now(), Status: "pending",
		Title: "amend: Use pnpm everywhere", Signal: "convention", Scope: "repo",
		Placement: "always_on", RepoRoot: repo, Confidence: 0.9, Tool: "gardener",
		Body: "- the gardener rewrote this block",
	}
	if err := st.InsertSuggestion(amend); err != nil {
		t.Fatal(err)
	}
	if _, err := Accept(st, amend); err != nil {
		t.Fatal(err)
	}
	// the acceptance really replaced content that was already there, or the undo below proves nothing
	got := read(t, agents)
	if !strings.Contains(got, "the gardener rewrote this block") || strings.Contains(got, "always `pnpm install`") {
		t.Fatalf("setup wrong: the gardener action did not replace the original block:\n%s", got)
	}

	journaled, err := HasJournaledAcceptance(st, amend.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !journaled {
		t.Fatal("a journaled gardener acceptance must be recognised as undoable: this is the CLI's precondition")
	}

	accepted, err := st.GetSuggestion(amend.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := Undo(st, accepted); err != nil {
		t.Fatalf("undoing a journaled gardener acceptance was refused: %v", err)
	}
	// a removal recomputed from the suggestion would have deleted the block; the compensation puts
	// back the exact bytes the acceptance overwrote
	assertSameTree(t, "gardener undo", before, snapshotTree(t, repo))
	if got := read(t, agents); !strings.Contains(got, "id="+original.ID) {
		t.Fatalf("the undo deleted the block instead of restoring it:\n%s", got)
	}
	stored, err := st.GetSuggestion(amend.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "pending" {
		t.Fatalf("undo did not return the gardener action to the inbox: %+v", stored)
	}

	// the legacy half: accepted before the journal existed, so nothing recorded what it overwrote
	legacy := amend
	legacy.ID = "sg_gardener_legacy"
	legacy.Status = "accepted"
	legacy.WrittenPath = agents
	if err := st.InsertSuggestion(legacy); err != nil {
		t.Fatal(err)
	}
	if journaled, err = HasJournaledAcceptance(st, legacy.ID); err != nil || journaled {
		t.Fatalf("an acceptance with no journal entry must not look journaled: %v (%v)", journaled, err)
	}
	err = Undo(st, legacy)
	if !errors.Is(err, ErrLegacyAcceptance) {
		t.Fatalf("a legacy gardener acceptance must refuse by name rather than approximate a removal, got %v", err)
	}
	if !strings.Contains(err.Error(), "git history") {
		t.Fatalf("the refusal must name a recovery path: %v", err)
	}
	assertSameTree(t, "refused legacy undo", before, snapshotTree(t, repo))
	if open, err := st.IncompleteOperations(); err != nil || len(open) != 0 {
		t.Fatalf("the refused undo opened an operation: %+v (%v)", open, err)
	}
}
