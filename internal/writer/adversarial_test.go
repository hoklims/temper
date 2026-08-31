package writer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hoklims/temper/internal/store"
)

// The tests in this file are the hostile half of HOK-540. Each one describes a world that moved
// under a mutation — a concurrent edit, a second acceptance, a swapped directory, a stale
// rejection — and asserts the refusal rather than the happy path. They are written to fail on a
// writer that treats the captured checksums as diagnostics, the suggestion id as the only lock, or
// a persisted absolute path as an authority.

// The captured preimage is a precondition, not a record. A file edited between capture and apply
// is in a third state: neither what the manifest saw nor what it would produce, so writing over it
// would destroy an edit this operation never had a claim on.
func TestApplyRefusesAnEditMadeAfterCapture(t *testing.T) {
	repo := t.TempDir()
	agents := filepath.Join(repo, "AGENTS.md")
	if err := os.WriteFile(agents, []byte("# AGENTS.md\n\noriginal user text\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mut, err := BuildMutation(suggestion(repo))
	if err != nil {
		t.Fatal(err)
	}
	if err := capture(&mut); err != nil {
		t.Fatal(err)
	}

	edit := "# AGENTS.md\n\nuser edit made after capture\n"
	if err := os.WriteFile(agents, []byte(edit), 0o644); err != nil {
		t.Fatal(err)
	}

	err = applyOps(&mut)
	if err == nil {
		t.Fatal("applying over a file changed since capture must be refused")
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("the refusal must be classifiable as a conflict, got %v", err)
	}
	if got := read(t, agents); got != edit {
		t.Fatalf("the edit was overwritten:\nwant %q\ngot  %q", edit, got)
	}
}

// The same precondition guards the rollback path. A file edited while the mutation was in flight
// must NOT be restored to the preimage: the failing operation no longer knows what those bytes
// mean. It stays open — its files stay reserved — and the failure names it.
func TestRollbackPreservesAnEditMadeAfterApply(t *testing.T) {
	st, repo, g := journalRepo(t)
	agents := filepath.Join(repo, "AGENTS.md")
	edit := "# AGENTS.md\n\nuser edit made while temper was mid-mutation\n"

	setApplyHook(t, func(_ int, op FileOp) error {
		switch filepath.Base(op.Path) {
		case "AGENTS.md":
			return os.WriteFile(op.Path, []byte(edit), 0o644)
		case "CLAUDE.md":
			return os.ErrPermission
		}
		return nil
	})
	_, err := Accept(st, g)
	clearApplyHook()

	if err == nil {
		t.Fatal("the injected failure must fail the acceptance")
	}
	if got := read(t, agents); got != edit {
		t.Fatalf("rollback overwrote a concurrent edit:\nwant %q\ngot  %q", edit, got)
	}
	if !strings.Contains(err.Error(), "AGENTS.md") {
		t.Fatalf("the failure must name the file it refused to restore: %v", err)
	}
	// a mutation that could neither complete nor unwind keeps its claims: the file is contested,
	// and handing it to the next acceptance would lose the edit the rollback just protected
	open, err := st.IncompleteOperations()
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("an unresolvable operation must stay open for reconciliation, got %+v", open)
	}
	stored, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "pending" {
		t.Fatalf("status = %q, a failed acceptance must not decide", stored.Status)
	}
}

// Two DIFFERENT suggestions both writing the repository's AGENTS.md are one conflict on one
// resource. The second one planned its content from a snapshot the first invalidates, so applying
// it as planned would rewrite the section without the first one's block — a silently lost skill.
func TestASecondAcceptCannotLoseTheFirstBlock(t *testing.T) {
	st, repo, first := journalRepo(t)
	agents := filepath.Join(repo, "AGENTS.md")

	second := first
	second.ID = "sg_test02"
	second.Title = "Second independently accepted rule"
	second.Body = "- second body"
	if err := st.InsertSuggestion(second); err != nil {
		t.Fatal(err)
	}

	// the second acceptance is planned BEFORE the first one lands, exactly as two review clients
	// holding the same page would
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

	// the stale plan is now a conflict: AGENTS.md holds neither what it captured nor what it would
	// write, so it is refused instead of erasing the first block
	if err := applyOps(&stale); err == nil {
		t.Fatal("a mutation planned against a superseded AGENTS.md must be refused")
	} else if !errors.Is(err, ErrConflict) {
		t.Fatalf("the refusal must be classifiable as a conflict, got %v", err)
	}
	if got := read(t, agents); !strings.Contains(got, "id="+first.ID) {
		t.Fatalf("the first block was lost by a stale plan:\n%s", got)
	}

	// re-planned against the current file, the second acceptance keeps both
	if _, err := Accept(st, second); err != nil {
		t.Fatal(err)
	}
	got := read(t, agents)
	if !strings.Contains(got, "id="+first.ID) || !strings.Contains(got, "id="+second.ID) {
		t.Fatalf("two committed accepts lost a block:\n%s", got)
	}
}

// The resource reservation is a row, not a mutex: it must still hold after the process that took
// it is gone, or a restart would hand a contested file to the next acceptance.
func TestResourceClaimsSurviveARestartAndBlockAnotherSuggestion(t *testing.T) {
	st, repo, first := journalRepo(t)
	agents := filepath.Join(repo, "AGENTS.md")

	second := first
	second.ID = "sg_test02"
	second.Title = "Second rule aimed at the same file"
	second.Body = "- second body"
	if err := st.InsertSuggestion(second); err != nil {
		t.Fatal(err)
	}

	setApplyHook(t, func(_ int, op FileOp) error {
		if filepath.Base(op.Path) == "AGENTS.md" {
			panic("simulated process death mid-mutation")
		}
		return nil
	})
	mustCrash(t, func() { _, _ = Accept(st, first) })
	clearApplyHook()

	// the process restarts: a new store handle over the same file
	dbPath := st.Path()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// a DIFFERENT suggestion, so suggestion-level single-flight says nothing; only the claim on
	// AGENTS.md itself can refuse this
	_, err = Accept(st, second)
	if !errors.Is(err, store.ErrResourceBusy) {
		t.Fatalf("a file reserved by an unfinished operation must refuse another acceptance, got %v", err)
	}
	if got := read(t, agents); strings.Contains(got, "id="+second.ID) {
		t.Fatalf("the refused acceptance wrote anyway:\n%s", got)
	}

	// once the interrupted operation is resolved, the file is free again and both land
	if _, err := Reconcile(st); err != nil {
		t.Fatal(err)
	}
	if _, err := Accept(st, second); err != nil {
		t.Fatalf("the claim was not released by reconciliation: %v", err)
	}
	got := read(t, agents)
	if !strings.Contains(got, "id="+first.ID) || !strings.Contains(got, "id="+second.ID) {
		t.Fatalf("reconciliation plus a second accept lost a block:\n%s", got)
	}
}

// A rejection is a decision with no filesystem effect, which is exactly why it is dangerous: it
// can be recorded against a suggestion whose artifact is already on disk. It must lose the race
// instead, and it must refuse outright while an operation is unfinished.
func TestRejectCannotContradictAnAcceptance(t *testing.T) {
	st, _, g := journalRepo(t)

	written, err := Accept(st, g)
	if err != nil {
		t.Fatal(err)
	}
	// the stale review client, still holding a page that said "pending", rejects
	err = st.Reject(g.ID)
	if !errors.Is(err, store.ErrNotPending) {
		t.Fatalf("rejecting an accepted suggestion must lose the compare-and-set, got %v", err)
	}
	stored, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "accepted" {
		t.Fatalf("status = %q: a rejection overwrote a committed acceptance", stored.Status)
	}
	if got := read(t, written); !strings.Contains(got, "id="+g.ID) {
		t.Fatalf("the artifact of the acceptance is gone:\n%s", got)
	}
}

func TestRejectRefusesWhileAnOperationIsUnfinished(t *testing.T) {
	st, _, g := journalRepo(t)

	setApplyHook(t, func(_ int, op FileOp) error {
		if filepath.Base(op.Path) == "AGENTS.md" {
			panic("simulated process death mid-mutation")
		}
		return nil
	})
	mustCrash(t, func() { _, _ = Accept(st, g) })
	clearApplyHook()

	// the suggestion still reads "pending", but its acceptance is half on disk: rejecting now would
	// mean "rejected" next to an artifact reconciliation is about to complete
	err := st.Reject(g.ID)
	if !errors.Is(err, store.ErrOperationInFlight) {
		t.Fatalf("rejecting a suggestion with an unfinished operation must be refused, got %v", err)
	}
	stored, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "pending" {
		t.Fatalf("status = %q after a refused rejection", stored.Status)
	}

	if _, err := Reconcile(st); err != nil {
		t.Fatal(err)
	}
	if stored, err = st.GetSuggestion(g.ID); err != nil || stored.Status != "accepted" {
		t.Fatalf("reconciliation did not complete the acceptance: %+v (%v)", stored, err)
	}
}

// An undo compensates the acceptance that is on disk, not a deletion recomputed from today's
// suggestion row. The difference only shows on an acceptance with side effects: a file that
// existed before, an import line appended to CLAUDE.md, a skill demoted by the budget. All three
// have to come back, and the repository has to be byte-identical to what it was.
func TestUndoCompensatesTheWholeAcceptedManifest(t *testing.T) {
	old := SectionBudgetBytes
	SectionBudgetBytes = 900 // small enough that accepting one more block demotes another
	t.Cleanup(func() { SectionBudgetBytes = old })

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("# AGENTS.md\n\nHand-written intro.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	padding := strings.Repeat("- a fairly long instruction line for padding purposes\n", 8)
	weak := suggestion(repo)
	weak.ID = "sg_weak"
	weak.Title = "Weak low-confidence skill"
	weak.Confidence = 0.55
	weak.Body = padding
	if _, err := writeUnjournaled(weak); err != nil {
		t.Fatal(err)
	}
	// CLAUDE.md appears only now, so the acceptance below is what adds the import line
	if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("# CLAUDE.md\n\nHand-written too.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, repo)

	st, err := store.Open(filepath.Join(t.TempDir(), "temper.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	strong := suggestion(repo)
	strong.ID = "sg_strong"
	strong.Title = "Strong high-confidence skill"
	strong.Confidence = 0.95
	strong.Body = padding
	strong.Status = "pending"
	if err := st.InsertSuggestion(strong); err != nil {
		t.Fatal(err)
	}

	if _, err := Accept(st, strong); err != nil {
		t.Fatal(err)
	}

	// the acceptance really did all three things, or the undo below proves nothing
	after := snapshotTree(t, repo)
	demoted := ".cursor/skills/temper-weak-low-confidence-skill/SKILL.md"
	if _, ok := after[demoted]; !ok {
		t.Fatalf("setup wrong: the budget did not demote anything: %v", keys(after))
	}
	if !strings.Contains(after["CLAUDE.md"], "@AGENTS.md") {
		t.Fatalf("setup wrong: no import line was added:\n%s", after["CLAUDE.md"])
	}
	if !strings.Contains(after["AGENTS.md"], "id=sg_strong") {
		t.Fatalf("setup wrong: the block was not written:\n%s", after["AGENTS.md"])
	}

	accepted, err := st.GetSuggestion(strong.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := Undo(st, accepted); err != nil {
		t.Fatal(err)
	}

	// a removal recomputed from the suggestion would only prune the block: it would leave the
	// demoted file behind and the import line in CLAUDE.md
	assertSameTree(t, "undo", before, snapshotTree(t, repo))
	stored, err := st.GetSuggestion(strong.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "pending" || stored.WrittenPath != "" {
		t.Fatalf("undo did not return the suggestion to the inbox: %+v", stored)
	}
	if open, err := st.IncompleteOperations(); err != nil || len(open) != 0 {
		t.Fatalf("undo left an operation open: %+v (%v)", open, err)
	}
}

// The confinement check and the write are two moments. Between them a directory can become a link
// out of the repository, and a check that has already passed cannot catch it — only performing the
// write through a handle on the authorized root can.
func TestDirectorySwappedAfterTheCheckCannotRedirectTheWrite(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()

	mut := Mutation{
		Ops: []FileOp{
			{Root: repo, Path: filepath.Join(repo, "AGENTS.md"), Content: "# AGENTS.md\n"},
			{Root: repo, Path: filepath.Join(repo, ".cursor", "skills", "temper-x", "SKILL.md"), Content: "demoted skill\n"},
		},
		WrittenPath: filepath.Join(repo, "AGENTS.md"),
	}
	if err := capture(&mut); err != nil {
		t.Fatal(err)
	}

	// every destination has now been confined; the swap lands after that proof and before the
	// second file is created. The hook never returns an error itself, so whatever applyOps reports
	// comes from the write path and not from the injection.
	var swapErr error
	setApplyHook(t, func(index int, _ FileOp) error {
		if index == 0 {
			swapErr = os.Symlink(outside, filepath.Join(repo, ".cursor"))
		}
		return nil
	})
	err := applyOps(&mut)
	clearApplyHook()
	if swapErr != nil {
		t.Fatalf("create directory symlink witness: %v", swapErr)
	}
	// the first file really was written, so the confinement checks had all passed
	if got := read(t, filepath.Join(repo, "AGENTS.md")); got != "# AGENTS.md\n" {
		t.Fatalf("setup wrong: the mutation had not started when the swap landed: %q", got)
	}
	if err == nil {
		t.Fatal("a parent directory swapped after the check must not redirect the write")
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("the mutation escaped the repository: %v", entries)
	}
}

// A manifest is data read back from a database. Its absolute paths are not an authority: a row
// edited to aim outside the authorized root must be refused before anything is read or written,
// on the apply path and on the rollback path alike.
func TestATamperedManifestPathIsNotAnAuthority(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "important.txt")
	if err := os.WriteFile(victim, []byte("not temper' file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mut, err := BuildMutation(suggestion(repo))
	if err != nil {
		t.Fatal(err)
	}
	if err := capture(&mut); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(mut)
	if err != nil {
		t.Fatal(err)
	}

	var tampered Mutation
	if err := json.Unmarshal(raw, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered.Ops[0].Path = victim // Root still says repo; only the stored path was rewritten

	if err := applyOps(&tampered); err == nil {
		t.Fatal("a manifest path outside its authorized root must be refused")
	}
	if err := unwind(&tampered); err == nil {
		t.Fatal("rolling back through a path outside its authorized root must be refused")
	}
	if got := read(t, victim); got != "not temper' file\n" {
		t.Fatalf("a file outside the repository was touched: %q", got)
	}
}

// The manifest half of the same invariant, provable on every platform: a replacement records the
// mode it found, and the compensation carries it back. Without this, a rollback would restore the
// bytes and re-create the file with whatever the umask happened to allow.
func TestManifestCarriesTheModeIntoItsCompensation(t *testing.T) {
	repo := t.TempDir()
	agents := filepath.Join(repo, "AGENTS.md")
	if err := os.WriteFile(agents, []byte("# AGENTS.md\n\nHand-written intro.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mut, err := BuildMutation(suggestion(repo))
	if err != nil {
		t.Fatal(err)
	}
	if err := capture(&mut); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(agents)
	if err != nil {
		t.Fatal(err)
	}
	var captured *FileOp
	for i := range mut.Ops {
		if filepath.Base(mut.Ops[i].Path) == "AGENTS.md" {
			captured = &mut.Ops[i]
		}
	}
	if captured == nil {
		t.Fatal("the mutation does not touch AGENTS.md")
	}
	if !captured.Existed {
		t.Fatal("an existing destination was captured as new")
	}
	if captured.Mode != uint32(info.Mode().Perm()) {
		t.Fatalf("captured mode %#o, file is %#o", captured.Mode, info.Mode().Perm())
	}

	for _, back := range invert(mut).Ops {
		if filepath.Base(back.Path) != "AGENTS.md" {
			continue
		}
		if back.Mode != captured.Mode {
			t.Fatalf("the compensation lost the mode: %#o vs %#o", back.Mode, captured.Mode)
		}
		if back.Content != captured.Preimage || back.PostSum != captured.PreimageSum {
			t.Fatal("the compensation does not write back the captured preimage")
		}
		return
	}
	t.Fatal("the compensation does not cover AGENTS.md")
}

// Restoring "the previous state" means the previous permissions too: a file that was owner-only
// before an acceptance must not come back world-readable.
func TestFileModeIsCapturedAndRestored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not carry POSIX permission bits")
	}
	st, repo, g := journalRepo(t)
	agents := filepath.Join(repo, "AGENTS.md")
	if err := os.Chmod(agents, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Accept(st, g); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(agents)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the acceptance widened the file mode to %v", info.Mode().Perm())
	}

	accepted, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := Undo(st, accepted); err != nil {
		t.Fatal(err)
	}
	if info, err = os.Stat(agents); err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("rollback restored the bytes but not the mode: %v", info.Mode().Perm())
	}
	if got := read(t, agents); !strings.Contains(got, "Hand-written intro.") || strings.Contains(got, "id="+g.ID) {
		t.Fatalf("rollback did not restore the previous content:\n%s", got)
	}
}

// A plan that is not a function of its input cannot be reviewed before it is applied nor replayed
// after a crash. Two blocks with the same confidence are the case where the choice used to come
// from Go's randomized map iteration.
func TestEqualConfidenceDemotionIsDeterministic(t *testing.T) {
	old := SectionBudgetBytes
	t.Cleanup(func() { SectionBudgetBytes = old })
	SectionBudgetBytes = 1 << 20 // no demotion while the fixture is being built

	repo := t.TempDir()
	padding := strings.Repeat("- equal-sized padding for deterministic budget selection\n", 8)
	a := suggestion(repo)
	a.ID = "sg_equal_a"
	a.Title = "Equal candidate A"
	a.Body = padding
	a.Confidence = 0.5
	if _, err := writeUnjournaled(a); err != nil {
		t.Fatal(err)
	}
	b := a
	b.ID = "sg_equal_b"
	b.Title = "Equal candidate B"
	if _, err := writeUnjournaled(b); err != nil {
		t.Fatal(err)
	}

	target := a
	target.ID = "sg_explicit_target"
	target.Title = "Explicit target"
	target.Confidence = 0.9

	// find the budget at which exactly one of the two equal blocks has to go — the moment the
	// tie-break decides
	full, err := BuildMutation(target)
	if err != nil {
		t.Fatal(err)
	}
	size := 0
	for _, op := range full.Ops {
		if filepath.Base(op.Path) == "AGENTS.md" {
			size = len(op.Content)
		}
	}
	chosen := 0
	for budget := size - 1; budget > 0; budget-- {
		SectionBudgetBytes = budget
		mut, err := BuildMutation(target)
		if err != nil {
			t.Fatal(err)
		}
		if len(mut.Notices) == 1 {
			chosen = budget
			break
		}
	}
	if chosen == 0 {
		t.Fatal("could not construct a one-demotion budget")
	}

	victims := map[string]int{}
	for i := 0; i < 1000; i++ {
		mut, err := BuildMutation(target)
		if err != nil {
			t.Fatal(err)
		}
		if len(mut.Notices) != 1 {
			t.Fatalf("budget %d produced %d demotions", chosen, len(mut.Notices))
		}
		victims[mut.Notices[0]]++
	}
	if len(victims) != 1 {
		t.Fatalf("equal-confidence demotion is nondeterministic at budget %d: %v", chosen, victims)
	}
	for notice := range victims {
		// the tie-break is the block id, so the lower one is evicted every time — a stable rule a
		// reviewer can predict, not merely a repeatable accident
		if !strings.Contains(notice, "Equal candidate A") {
			t.Fatalf("tie-break did not follow block id order: %q", notice)
		}
	}
}
