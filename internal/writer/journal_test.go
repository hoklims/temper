package writer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoklims/temper/internal/store"
)

// journalRepo builds a repository with hand-written context files and a store holding one pending
// suggestion, so accepting it is a genuinely multi-file mutation: the AGENTS.md section plus the
// CLAUDE.md import line.
func journalRepo(t *testing.T) (*store.Store, string, store.Suggestion) {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("# AGENTS.md\n\nHand-written intro.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("# CLAUDE.md\n\nHand-written too.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "temper.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	g := suggestion(repo)
	g.CreatedAt = time.Now()
	g.Status = "pending"
	g.Signal = "convention"
	g.Confidence = 0.9
	if err := st.InsertSuggestion(g); err != nil {
		t.Fatal(err)
	}
	return st, repo, g
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// setApplyHook installs the interruption hook for one test and always removes it — including when
// the test body panics, which is exactly what the crash-injection tests do. A hook that outlived
// its test would inject failures into an unrelated one, and it is set through the package's own
// accessor so a concurrent acceptance reading it is not a data race.
func setApplyHook(t *testing.T, fn func(index int, op FileOp) error) {
	t.Helper()
	installApplyHook(fn)
	t.Cleanup(func() { installApplyHook(nil) })
}

// clearApplyHook removes the hook before the end of a test, for the cases that go on to assert
// behaviour that must not be interrupted.
func clearApplyHook() { installApplyHook(nil) }

func mustCrash(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Error("expected the injected crash to abort the mutation")
		}
	}()
	fn()
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The invariant this whole slice exists for: a process that dies after one file of a multi-file
// acceptance leaves a partially-accepted state on disk, and reopening the store must resolve it
// to a fully committed one — files, journal and status together.
func TestCrashMidMutationConvergesOnReconcile(t *testing.T) {
	st, repo, g := journalRepo(t)
	agents := filepath.Join(repo, "AGENTS.md")
	claude := filepath.Join(repo, "CLAUDE.md")

	setApplyHook(t, func(index int, op FileOp) error {
		if filepath.Base(op.Path) == "AGENTS.md" {
			panic("simulated process death after the first file was replaced")
		}
		return nil
	})
	mustCrash(t, func() { _, _ = Accept(st, g) })
	clearApplyHook()

	// the state HOK-540 has to be able to end: file mutated, decision not recorded
	if !strings.Contains(read(t, agents), "temper:begin id=sg_test01") {
		t.Fatal("setup wrong: the first file was not written before the crash")
	}
	if strings.Contains(read(t, claude), "@AGENTS.md") {
		t.Fatal("setup wrong: the crash landed after the whole mutation")
	}
	stored, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "pending" {
		t.Fatalf("status = %q, the crash should have preceded the decision", stored.Status)
	}
	open, err := st.IncompleteOperations()
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].State != store.OpApplying {
		t.Fatalf("expected one applying operation, got %+v", open)
	}

	report, err := Reconcile(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 || !strings.Contains(report[0], "completed") {
		t.Fatalf("reconcile report = %v", report)
	}

	if !strings.Contains(read(t, agents), "temper:begin id=sg_test01") {
		t.Fatalf("AGENTS.md lost its block:\n%s", read(t, agents))
	}
	if !strings.Contains(read(t, claude), "@AGENTS.md") {
		t.Fatalf("the interrupted second file was never finished:\n%s", read(t, claude))
	}
	if !strings.Contains(read(t, agents), "Hand-written intro.") || !strings.Contains(read(t, claude), "Hand-written too.") {
		t.Fatal("hand-written content lost while reconciling")
	}
	stored, err = st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "accepted" || stored.WrittenPath != agents {
		t.Fatalf("store did not converge with the filesystem: %+v", stored)
	}
	if open, err = st.IncompleteOperations(); err != nil || len(open) != 0 {
		t.Fatalf("operation still open after reconcile: %+v (%v)", open, err)
	}

	// reconciling again is a no-op, not a second write
	if report, err = Reconcile(st); err != nil || len(report) != 0 {
		t.Fatalf("second reconcile did work: %v (%v)", report, err)
	}
}

// A failing step is not a crash: the mutation unwinds to the exact preimages of every file it had
// already replaced, and the suggestion stays available for the human to decide again.
func TestFailedMutationRollsBackEveryFile(t *testing.T) {
	st, repo, g := journalRepo(t)
	before := snapshotTree(t, repo)

	setApplyHook(t, func(index int, op FileOp) error {
		if filepath.Base(op.Path) == "CLAUDE.md" {
			return os.ErrPermission // AGENTS.md is already replaced at this point
		}
		return nil
	})
	_, err := Accept(st, g)
	clearApplyHook()
	if err == nil {
		t.Fatal("a failing mutation must not report success")
	}

	after := snapshotTree(t, repo)
	if len(after) != len(before) {
		t.Fatalf("rollback left extra or missing files: %v", after)
	}
	for name, content := range before {
		if after[name] != content {
			t.Fatalf("%s was not restored byte-for-byte:\nwant %q\ngot  %q", name, content, after[name])
		}
	}
	stored, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "pending" || stored.WrittenPath != "" {
		t.Fatalf("a rolled-back acceptance decided anyway: %+v", stored)
	}
	if open, err := st.IncompleteOperations(); err != nil || len(open) != 0 {
		t.Fatalf("rolled-back operation left open: %+v (%v)", open, err)
	}
}

// An operation interrupted before its first write provably touched nothing, so reconciliation
// releases it without restoring anything — writing preimages back over files the user may have
// edited since would be a mutation nobody asked for.
func TestPreparedOperationIsReleasedWithoutTouchingFiles(t *testing.T) {
	st, repo, g := journalRepo(t)
	before := snapshotTree(t, repo)

	mut, err := BuildMutation(g)
	if err != nil {
		t.Fatal(err)
	}
	if err := capture(&mut); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(mut)
	if err != nil {
		t.Fatal(err)
	}
	resources, err := mut.resources()
	if err != nil {
		t.Fatal(err)
	}
	op := store.Operation{ID: store.NewOperationID(), SuggestionID: g.ID, Kind: "accept",
		Manifest: string(manifest), TargetStatus: "accepted", TargetPath: mut.WrittenPath}
	if err := st.BeginOperation(op, "pending", resources); err != nil {
		t.Fatal(err)
	}
	// the process dies here, between the journal entry and MarkApplying

	// a human edit lands before the next start; releasing the operation must not undo it
	edited := "# AGENTS.md\n\nHand-written intro, edited after the crash.\n"
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	before["AGENTS.md"] = edited

	report, err := Reconcile(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 || !strings.Contains(report[0], "nothing had been written") {
		t.Fatalf("reconcile report = %v", report)
	}
	for name, content := range snapshotTree(t, repo) {
		if before[name] != content {
			t.Fatalf("%s changed while releasing an unstarted operation:\nwant %q\ngot  %q", name, before[name], content)
		}
	}
	stored, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "pending" {
		t.Fatalf("status = %q, an unstarted operation must not decide", stored.Status)
	}
}

// BuildPlan proves where a suggestion belongs; that proof expires. A junction or symlink planted
// between planning and writing must be caught at the moment of mutation, with nothing landing
// outside the repository.
func TestSymlinkPlantedAfterPlanningIsRefusedAtMutationTime(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()

	g := suggestion(repo)
	g.Placement = "skill"
	mut, err := BuildMutation(g)
	if err != nil {
		t.Fatal(err)
	}
	if err := capture(&mut); err != nil {
		t.Fatal(err)
	}

	// the destination's ancestor becomes a link out of the repository AFTER the plan was accepted
	if err := os.Symlink(outside, filepath.Join(repo, ".cursor")); err != nil {
		t.Fatalf("create directory symlink witness: %v", err)
	}
	if err := applyOps(&mut); err == nil {
		t.Fatal("a destination redirected out of the repository must be refused at mutation time")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the mutation escaped the repository: %v", entries)
	}
}

// A rollback path is a deletion path: it must revalidate just as hard as a write.
func TestRollbackRefusesARedirectedDestination(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "SKILL.md")
	if err := os.WriteFile(victim, []byte("someone else's file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := suggestion(repo)
	g.Placement = "skill"
	mut, err := BuildMutation(g)
	if err != nil {
		t.Fatal(err)
	}
	if err := capture(&mut); err != nil {
		t.Fatal(err)
	}
	skills := filepath.Join(repo, ".cursor", "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(skills, "temper-use-pnpm-never-npm")); err != nil {
		t.Fatalf("create directory symlink witness: %v", err)
	}
	if err := unwind(&mut); err == nil {
		t.Fatal("unwinding through a redirected destination must be refused")
	}
	if got := read(t, victim); got != "someone else's file\n" {
		t.Fatalf("rollback destroyed a file outside the repository: %q", got)
	}
}

// write(write(x)) == write(x), including when the second write is a reconciliation replaying the
// journal, and including the user's own text around the managed section.
func TestAcceptIsByteIdenticalWhenReplayed(t *testing.T) {
	st, repo, g := journalRepo(t)

	// crash once every file has landed but before the decision is recorded — the case where
	// reconciliation replays a manifest over files that are already correct
	setApplyHook(t, func(index int, op FileOp) error {
		if filepath.Base(op.Path) == "CLAUDE.md" {
			panic("simulated process death after the last file was replaced")
		}
		return nil
	})
	mustCrash(t, func() { _, _ = Accept(st, g) })
	clearApplyHook()

	first := snapshotTree(t, repo)
	stored, err := st.GetSuggestion(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "pending" {
		t.Fatalf("setup wrong: status is already %q", stored.Status)
	}

	if _, err := Reconcile(st); err != nil {
		t.Fatal(err)
	}
	assertSameTree(t, "journal replay", first, snapshotTree(t, repo))
	if stored, err = st.GetSuggestion(g.ID); err != nil || stored.Status != "accepted" {
		t.Fatalf("replay did not commit the decision: %+v (%v)", stored, err)
	}

	// and re-deriving the same acceptance from scratch converges on the same bytes
	if _, err := writeUnjournaled(g); err != nil {
		t.Fatal(err)
	}
	assertSameTree(t, "second write", first, snapshotTree(t, repo))

	if !strings.Contains(first["AGENTS.md"], "Hand-written intro.") || !strings.Contains(first["CLAUDE.md"], "Hand-written too.") {
		t.Fatalf("user content outside the managed section was not preserved: %v", first)
	}
}

func assertSameTree(t *testing.T, what string, want, got map[string]string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s changed the file set: %v vs %v", what, keys(want), keys(got))
	}
	for name, content := range want {
		if got[name] != content {
			t.Fatalf("%s is not idempotent on %s:\nwant %q\ngot  %q", what, name, content, got[name])
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Undo deletes files. The path it deletes is recomputed from the suggestion, never taken from the
// row: a tampered or stale written_path must not aim a deletion anywhere else.
func TestUndoRefusesATamperedWrittenPath(t *testing.T) {
	repo := t.TempDir()
	elsewhere := t.TempDir()
	victim := filepath.Join(elsewhere, "important.txt")
	if err := os.WriteFile(victim, []byte("not temper' file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := suggestion(repo)
	g.Placement = "skill"
	written, err := writeUnjournaled(g)
	if err != nil {
		t.Fatal(err)
	}

	g.WrittenPath = victim
	if err := removeUnjournaled(g); err == nil {
		t.Fatal("removing a path this suggestion never wrote must be refused")
	}
	if got := read(t, victim); got != "not temper' file\n" {
		t.Fatalf("an unrelated file was deleted or altered: %q", got)
	}
	if _, err := os.Stat(written); err != nil {
		t.Fatalf("the real artifact was disturbed by the refusal: %v", err)
	}

	// Naming the stored path "AGENTS.md" must not turn a skill removal into a section rewrite:
	// the shape of the mutation comes from the recomputed plan, not from the row.
	g.WrittenPath = filepath.Join(elsewhere, "AGENTS.md")
	_, err = BuildRemoval(g)
	if err == nil {
		t.Fatal("a removal aimed at a file this suggestion never wrote must be refused")
	}
	if !strings.Contains(err.Error(), "this suggestion's artifact is") {
		t.Fatalf("refusal should name the artifact mismatch, not a downstream symptom: %v", err)
	}
	if got := read(t, written); !strings.Contains(got, "Use pnpm") {
		t.Fatalf("the real artifact was disturbed by the refusal: %q", got)
	}

	// and for a suggestion that genuinely owns an AGENTS.md block, a written_path pointing out of
	// the repository still cannot redirect the section rewrite: the destination is recomputed.
	block := suggestion(repo)
	if _, err := writeUnjournaled(block); err != nil {
		t.Fatal(err)
	}
	block.WrittenPath = filepath.Join(elsewhere, "AGENTS.md")
	mut, err := BuildRemoval(block)
	if err != nil {
		t.Fatal(err)
	}
	if len(mut.Ops) == 0 {
		t.Fatal("removing an accepted block must produce a mutation")
	}
	for _, op := range mut.Ops {
		if !strings.HasPrefix(filepath.Clean(op.Path), filepath.Clean(repo)+string(filepath.Separator)) {
			t.Fatalf("removal targets %s, outside the repository", op.Path)
		}
	}
	if err := removeUnjournaled(block); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(repo, "AGENTS.md")); strings.Contains(got, "temper:begin id=sg_test01") {
		t.Fatalf("block not pruned:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("the tampered path was written to outside the repository: %v", err)
	}
}
