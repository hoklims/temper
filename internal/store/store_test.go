package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "temper.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func pending(id string) Suggestion {
	return Suggestion{
		ID: id, CreatedAt: time.Now(), Status: "pending", Title: "title " + id,
		Signal: "convention", Scope: "repo", Placement: "always_on", RepoRoot: "/repo",
		Body: "- body", Confidence: 0.9,
	}
}

func backupsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".backup-v") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestFreshDatabaseIsStampedAtLatestVersion(t *testing.T) {
	st := openTemp(t)
	v, err := st.userVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != latestVersion() {
		t.Fatalf("user_version = %d, want %d", v, latestVersion())
	}
	// nothing to protect on an empty database: no backup noise next to it
	if got := backupsIn(t, filepath.Dir(st.Path())); len(got) != 0 {
		t.Fatalf("fresh database produced backups: %v", got)
	}
}

// A database created before schema versioning carries user_version 0 and, if it is old enough,
// lacks the columns later binaries added. Adopting it must preserve its rows, add what is
// missing, and leave a backup of the pre-migration file.
func TestLegacyDatabaseIsMigratedWithBackupAndKeepsData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "temper.db")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
CREATE TABLE ingest_files (
  path TEXT PRIMARY KEY, bytes_processed INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL);
CREATE TABLE suggestions (
  id TEXT PRIMARY KEY, created_at TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
  title TEXT NOT NULL, signal TEXT NOT NULL DEFAULT '', scope TEXT NOT NULL DEFAULT 'repo',
  placement TEXT NOT NULL DEFAULT 'always_on', sensitivity INTEGER NOT NULL DEFAULT 0,
  confidence REAL NOT NULL DEFAULT 0, project TEXT NOT NULL DEFAULT '',
  repo_root TEXT NOT NULL DEFAULT '', target_path TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL DEFAULT '', rationale TEXT NOT NULL DEFAULT '',
  evidence_json TEXT NOT NULL DEFAULT '[]', session_id TEXT NOT NULL DEFAULT '',
  tool TEXT NOT NULL DEFAULT '', written_path TEXT NOT NULL DEFAULT '', decided_at TEXT);
INSERT INTO suggestions(id, created_at, status, title) VALUES('sg_legacy','2026-01-01T00:00:00Z','pending','from an older binary');
INSERT INTO ingest_files(path, bytes_processed, updated_at) VALUES('/t/a.jsonl', 4096, '2026-01-01T00:00:00Z');
`); err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("legacy database rejected: %v", err)
	}
	defer st.Close()

	v, err := st.userVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != latestVersion() {
		t.Fatalf("user_version = %d, want %d", v, latestVersion())
	}
	g, err := st.GetSuggestion("sg_legacy")
	if err != nil {
		t.Fatalf("legacy row lost: %v", err)
	}
	if g.Title != "from an older binary" || g.Globs != "" || g.BlockID != "" {
		t.Fatalf("legacy row not readable through the new columns: %+v", g)
	}
	n, err := st.BytesProcessed("/t/a.jsonl")
	if err != nil || n != 4096 {
		t.Fatalf("checkpoint lost: %d (%v)", n, err)
	}
	if got := backupsIn(t, dir); len(got) != 1 || !strings.Contains(got[0], "backup-v0") {
		t.Fatalf("expected exactly one v0 backup, got %v", got)
	}

	// reopening an already-current database is a no-op: no second migration, no second backup
	st.Close()
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if got := backupsIn(t, dir); len(got) != 1 {
		t.Fatalf("reopen produced another backup: %v", got)
	}
}

func TestOpenRefusesCorruptDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temper.db")
	if err := os.WriteFile(path, []byte("this is not a SQLite database, it is a text file"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatal("a corrupt database must be refused, not silently used")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("error should be classified as corruption: %v", err)
	}
	if !strings.Contains(err.Error(), "backup") {
		t.Fatalf("error should name the recovery path: %v", err)
	}
	// refusing must not destroy the evidence
	raw, readErr := os.ReadFile(path)
	if readErr != nil || !strings.HasPrefix(string(raw), "this is not a SQLite database") {
		t.Fatalf("the refused file was modified: %q (%v)", raw, readErr)
	}
}

func TestOpenRefusesFutureSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temper.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE suggestions(id TEXT PRIMARY KEY); PRAGMA user_version = 999`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	opened, err := Open(path)
	if err == nil {
		opened.Close()
		t.Fatal("a database from a newer temper must be refused")
	}
	if !strings.Contains(err.Error(), "999") || !strings.Contains(err.Error(), "newer temper") {
		t.Fatalf("error should name the version conflict: %v", err)
	}
}

// quick_check answers "are these pages readable", not "is this the schema this binary needs". A
// database stamped at the latest version whose shape was truncated or hand-edited passes every
// physical check, then breaks at the first acceptance — halfway through a decision, which is the
// one moment with no safe answer left. It has to be refused at open time instead, and never
// re-migrated: a re-migration would stamp the same version over the same gap.
func TestOpenRefusesLatestVersionWithAMissingTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temper.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE resource_claims`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err == nil {
		reopened.Close()
		t.Fatal("a database stamped at the latest version without its resource_claims table must be refused")
	}
	if !errors.Is(err, ErrSchemaIncomplete) {
		t.Fatalf("the refusal must be classifiable as an incomplete schema, not corruption: %v", err)
	}
	if !strings.Contains(err.Error(), "resource_claims") {
		t.Fatalf("the refusal must name what is missing: %v", err)
	}
	if !strings.Contains(err.Error(), "backup") {
		t.Fatalf("the refusal must name the recovery path: %v", err)
	}
}

// The same refusal at column granularity: a v2-shaped operations table stamped v3 is exactly what
// an interrupted hand-repair leaves, and it is physically valid.
func TestOpenRefusesLatestVersionWithAMissingColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temper.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE operations`); err != nil {
		t.Fatal(err)
	}
	// the v2 shape: everything the journal needs except the from_status v3 added
	if _, err := raw.Exec(`CREATE TABLE operations (
  id            TEXT PRIMARY KEY,
  suggestion_id TEXT NOT NULL,
  kind          TEXT NOT NULL,
  state         TEXT NOT NULL,
  manifest_json TEXT NOT NULL DEFAULT '{}',
  target_status TEXT NOT NULL DEFAULT '',
  target_body   TEXT NOT NULL DEFAULT '',
  target_path   TEXT NOT NULL DEFAULT '',
  note          TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// the file itself is fine: this is a schema failure, not a corruption
	reopened, err := Open(path)
	if err == nil {
		reopened.Close()
		t.Fatal("a database stamped v3 with a v2-shaped operations table must be refused")
	}
	if !errors.Is(err, ErrSchemaIncomplete) {
		t.Fatalf("the refusal must be classifiable as an incomplete schema: %v", err)
	}
	if !strings.Contains(err.Error(), "from_status") {
		t.Fatalf("the refusal must name the missing column: %v", err)
	}

	// and the version is left alone: refusing must not silently re-stamp the gap
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var v int
	if err := check.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != latestVersion() {
		t.Fatalf("the refused database was re-stamped to v%d", v)
	}
}

// A migration that fails halfway must leave the database exactly at its previous version, with
// none of the failed step's schema changes and a backup to fall back on.
func TestFailingMigrationRollsBackAndKeepsVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "temper.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertSuggestion(pending("sg_before")); err != nil {
		t.Fatal(err)
	}
	st.Close()

	orig := migrations
	t.Cleanup(func() { migrations = orig })
	migrations = append(append([]migration{}, orig...), migration{version: latestVersion() + 1, apply: func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE half_applied (x TEXT)`); err != nil {
			return err
		}
		return errors.New("injected failure after a DDL statement")
	}})

	broken, err := Open(path)
	if err == nil {
		broken.Close()
		t.Fatal("a failing migration must abort the open")
	}
	if !strings.Contains(err.Error(), "rolled back") || !strings.Contains(err.Error(), "backup") {
		t.Fatalf("error should say it rolled back and where the backup is: %v", err)
	}
	if got := backupsIn(t, dir); len(got) != 1 {
		t.Fatalf("expected one backup, got %v", got)
	}

	migrations = orig
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("database not usable after a rolled-back migration: %v", err)
	}
	defer st2.Close()
	v, err := st2.userVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != latestVersion() {
		t.Fatalf("user_version = %d after rollback, want %d", v, latestVersion())
	}
	var n int
	if err := st2.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='half_applied'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the failed migration left its table behind")
	}
	if _, err := st2.GetSuggestion("sg_before"); err != nil {
		t.Fatalf("pre-migration data lost: %v", err)
	}
}

// The high-water mark claims "everything up to here is persisted". If an insert fails, the claim
// must not be made — otherwise those suggestions are skipped by every future scan.
func TestCheckpointAndSuggestionsCommitTogether(t *testing.T) {
	st := openTemp(t)
	const file = "/transcripts/a.jsonl"
	if err := st.InsertSuggestion(pending("sg_dup")); err != nil {
		t.Fatal(err)
	}

	// second suggestion collides on the primary key: the whole batch must fail
	err := st.AdvanceCheckpoint(file, 4096, []Suggestion{pending("sg_fresh"), pending("sg_dup")})
	if err == nil {
		t.Fatal("a colliding suggestion must fail the batch")
	}
	n, err := st.BytesProcessed(file)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("checkpoint advanced to %d past a failed insert", n)
	}
	if _, err := st.GetSuggestion("sg_fresh"); err == nil {
		t.Fatal("a suggestion from the failed batch was persisted alone")
	}

	if err := st.AdvanceCheckpoint(file, 4096, []Suggestion{pending("sg_fresh")}); err != nil {
		t.Fatal(err)
	}
	if n, err = st.BytesProcessed(file); err != nil || n != 4096 {
		t.Fatalf("checkpoint = %d (%v), want 4096", n, err)
	}
	if _, err := st.GetSuggestion("sg_fresh"); err != nil {
		t.Fatalf("suggestion not persisted with its checkpoint: %v", err)
	}
}

func TestBeginOperationEnforcesPendingAndSingleFlight(t *testing.T) {
	st := openTemp(t)
	if err := st.InsertSuggestion(pending("sg_one")); err != nil {
		t.Fatal(err)
	}
	op := Operation{ID: NewOperationID(), SuggestionID: "sg_one", Kind: "accept",
		Manifest: "{}", TargetStatus: "accepted", TargetPath: "/repo/AGENTS.md"}
	if err := st.BeginOperation(op, "pending", []string{"/repo/AGENTS.md"}); err != nil {
		t.Fatal(err)
	}

	// a second operation on the same suggestion cannot start while the first is open
	second := op
	second.ID = NewOperationID()
	if err := st.BeginOperation(second, "pending", []string{"/repo/AGENTS.md"}); err == nil {
		t.Fatal("two concurrent operations on one suggestion must not both start")
	}

	if err := st.MarkApplying(op.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.CommitOperation(op.ID); err != nil {
		t.Fatal(err)
	}
	g, err := st.GetSuggestion("sg_one")
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != "accepted" || g.WrittenPath != "/repo/AGENTS.md" {
		t.Fatalf("commit did not apply the decision: %+v", g)
	}
	if open, err := st.IncompleteOperations(); err != nil || len(open) != 0 {
		t.Fatalf("operation still open after commit: %v (%v)", open, err)
	}

	// and an already-decided suggestion cannot be re-accepted
	third := op
	third.ID = NewOperationID()
	if err := st.BeginOperation(third, "pending", []string{"/repo/AGENTS.md"}); err == nil {
		t.Fatal("an accepted suggestion must not start a new accept")
	}
}

func TestRollbackLeavesTheSuggestionUndecided(t *testing.T) {
	st := openTemp(t)
	if err := st.InsertSuggestion(pending("sg_rb")); err != nil {
		t.Fatal(err)
	}
	op := Operation{ID: NewOperationID(), SuggestionID: "sg_rb", Kind: "accept",
		Manifest: "{}", TargetStatus: "accepted", TargetPath: "/repo/AGENTS.md"}
	if err := st.BeginOperation(op, "pending", []string{"/repo/AGENTS.md"}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkApplying(op.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.AbandonOperation(op.ID, "disk full"); err != nil {
		t.Fatal(err)
	}
	g, err := st.GetSuggestion("sg_rb")
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != "pending" || g.WrittenPath != "" {
		t.Fatalf("a rolled-back operation decided the suggestion anyway: %+v", g)
	}
	if err := st.CommitOperation(op.ID); err == nil {
		t.Fatal("a rolled-back operation must not be committable")
	}
	if open, err := st.IncompleteOperations(); err != nil || len(open) != 0 {
		t.Fatalf("rolled-back operation still needs reconciliation: %v (%v)", open, err)
	}
}

func acceptOp(suggestionID string) Operation {
	return Operation{ID: NewOperationID(), SuggestionID: suggestionID, Kind: "accept",
		Manifest: "{}", TargetStatus: "accepted", TargetPath: "/repo/AGENTS.md"}
}

// Single-flight on the suggestion is not enough. Two DIFFERENT suggestions writing the same
// AGENTS.md are one conflict on one file, and each planned its content from a snapshot the other
// is about to invalidate — so the reservation has to be the resource, not the suggestion id.
func TestResourceClaimRefusesAnotherSuggestionOnTheSameFile(t *testing.T) {
	st := openTemp(t)
	for _, id := range []string{"sg_a", "sg_b"} {
		if err := st.InsertSuggestion(pending(id)); err != nil {
			t.Fatal(err)
		}
	}

	first := acceptOp("sg_a")
	if err := st.BeginOperation(first, "pending", []string{"/repo/AGENTS.md"}); err != nil {
		t.Fatal(err)
	}

	second := acceptOp("sg_b")
	err := st.BeginOperation(second, "pending", []string{"/repo/AGENTS.md", "/repo/CLAUDE.md"})
	if !errors.Is(err, ErrResourceBusy) {
		t.Fatalf("a file held by an unfinished operation must refuse another one, got %v", err)
	}
	if !strings.Contains(err.Error(), "/repo/AGENTS.md") {
		t.Fatalf("the refusal must name the contested file: %v", err)
	}
	// the refused operation left nothing behind, including no claim on the file it did NOT get
	if _, err := st.GetOperation(second.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a refused BeginOperation persisted its journal entry: %v", err)
	}

	// a suggestion touching a different file is unaffected
	third := acceptOp("sg_b")
	if err := st.BeginOperation(third, "pending", []string{"/repo/docs/AGENTS.md"}); err != nil {
		t.Fatalf("an uncontested file must not be blocked: %v", err)
	}

	// and closing the first operation frees its file for the next one
	if err := st.MarkApplying(first.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.CommitOperation(first.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.AbandonOperation(third.ID, "done with it"); err != nil {
		t.Fatal(err)
	}
	fourth := acceptOp("sg_b")
	if err := st.BeginOperation(fourth, "pending", []string{"/repo/AGENTS.md"}); err != nil {
		t.Fatalf("the claim was not released when its operation closed: %v", err)
	}
}

// The reservation is a row, not a lock held in memory: a process that dies mid-mutation must leave
// the contested file reserved for the reconciliation that follows the restart.
func TestResourceClaimSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "temper.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"sg_a", "sg_b"} {
		if err := st.InsertSuggestion(pending(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.BeginOperation(acceptOp("sg_a"), "pending", []string{"/repo/AGENTS.md"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil { // the process dies here
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	err = reopened.BeginOperation(acceptOp("sg_b"), "pending", []string{"/repo/AGENTS.md"})
	if !errors.Is(err, ErrResourceBusy) {
		t.Fatalf("a claim must outlive the process that took it, got %v", err)
	}
	open, err := reopened.IncompleteOperations()
	if err != nil || len(open) != 1 {
		t.Fatalf("the interrupted operation is not visible for reconciliation: %+v (%v)", open, err)
	}
}

// CommitOperation carries a decision computed before the filesystem moved. If the suggestion left
// that state in the meantime, applying the decision would overwrite whoever decided it — so the
// commit asserts the source status it was planned against and fails instead.
func TestCommitRefusesWhenTheSourceStatusMoved(t *testing.T) {
	st := openTemp(t)
	if err := st.InsertSuggestion(pending("sg_cas")); err != nil {
		t.Fatal(err)
	}
	op := acceptOp("sg_cas")
	if err := st.BeginOperation(op, "pending", []string{"/repo/AGENTS.md"}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkApplying(op.ID); err != nil {
		t.Fatal(err)
	}

	// simulate a decision landing between prepare and commit through some other path; the in-flight
	// guard closes the paths this package exposes, so the assertion below is the last line of defence
	if err := decide(st.db, "sg_cas", "pending", "rejected", "", ""); err != nil {
		t.Fatal(err)
	}

	err := st.CommitOperation(op.ID)
	if !errors.Is(err, ErrNotPending) {
		t.Fatalf("committing against a moved status must fail the compare-and-set, got %v", err)
	}
	g, err := st.GetSuggestion("sg_cas")
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != "rejected" {
		t.Fatalf("status = %q: the commit overwrote a decision it did not make", g.Status)
	}
	// the operation is still unfinished, so its files stay reserved until it is resolved by hand
	open, err := st.IncompleteOperations()
	if err != nil || len(open) != 1 {
		t.Fatalf("a failed commit must leave the operation open: %+v (%v)", open, err)
	}
}

// claimsHeldBy reports how many resources an operation still reserves. Claims are the durable half
// of the state machine: an operation that has not reached a terminal state must still own its
// files, or the next acceptance is handed a file whose fate is undecided.
func claimsHeldBy(t *testing.T, st *Store, opID string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM resource_claims WHERE operation_id = ?`, opID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func stateOf(t *testing.T, st *Store, opID string) string {
	t.Helper()
	op, err := st.GetOperation(opID)
	if err != nil {
		t.Fatal(err)
	}
	return op.State
}

// The transitions are the invariant, not a side effect of which helper a caller happened to reach
// for. Each one below is either the only way to reach a state or a refusal, and every refusal is
// checked twice: the state did not move, and the operation still owns its files.
//
// The one that matters most is prepared → committed. A prepared operation has not crossed the
// durable line that says bytes may have moved, so committing it would record "accepted" for a
// mutation whose filesystem half nobody performed.
func TestOperationStateMachineRefusesTheTransitionsItDoesNotDefine(t *testing.T) {
	newOp := func(t *testing.T, st *Store, suggestionID, resource string) Operation {
		t.Helper()
		if err := st.InsertSuggestion(pending(suggestionID)); err != nil {
			t.Fatal(err)
		}
		op := acceptOp(suggestionID)
		if err := st.BeginOperation(op, "pending", []string{resource}); err != nil {
			t.Fatal(err)
		}
		return op
	}

	t.Run("prepared cannot commit", func(t *testing.T) {
		st := openTemp(t)
		op := newOp(t, st, "sg_prep_commit", "/repo/a.md")
		if err := st.CommitOperation(op.ID); err == nil {
			t.Fatal("a prepared operation committed, so a decision was recorded for a mutation that never started")
		}
		if got := stateOf(t, st, op.ID); got != OpPrepared {
			t.Fatalf("the refused commit moved the operation to %s", got)
		}
		if n := claimsHeldBy(t, st, op.ID); n != 1 {
			t.Fatalf("a still-open operation released its claims: %d", n)
		}
		g, err := st.GetSuggestion("sg_prep_commit")
		if err != nil {
			t.Fatal(err)
		}
		if g.Status != "pending" {
			t.Fatalf("the refused commit decided the suggestion anyway: %s", g.Status)
		}
	})

	t.Run("prepared cannot enter rolling back", func(t *testing.T) {
		st := openTemp(t)
		op := newOp(t, st, "sg_prep_rolling", "/repo/b.md")
		if err := st.MarkRollingBack(op.ID, "nothing was written"); err == nil {
			t.Fatal("a prepared operation entered rolling_back, inventing a restoration for a mutation that never started")
		}
		if got := stateOf(t, st, op.ID); got != OpPrepared {
			t.Fatalf("the refused transition moved the operation to %s", got)
		}
		// the way out of prepared without writing is abandonment, and it releases
		if err := st.AbandonOperation(op.ID, "refused before any write"); err != nil {
			t.Fatal(err)
		}
		if got := stateOf(t, st, op.ID); got != OpRolledBack {
			t.Fatalf("abandoning a prepared operation left it %s", got)
		}
		if n := claimsHeldBy(t, st, op.ID); n != 0 {
			t.Fatalf("a terminal operation kept %d claims", n)
		}
	})

	t.Run("applying binds its roots and commits", func(t *testing.T) {
		st := openTemp(t)
		op := newOp(t, st, "sg_applying", "/repo/c.md")
		if err := st.RebindRoot(op.ID, `{"bound":true}`); err == nil {
			t.Fatal("a prepared operation bound a root it cannot have created")
		}
		if err := st.MarkApplying(op.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.RebindRoot(op.ID, `{"bound":true}`); err != nil {
			t.Fatalf("an applying operation could not bind the root it created: %v", err)
		}
		if got := stateOf(t, st, op.ID); got != OpApplying {
			t.Fatalf("binding a root moved the operation to %s", got)
		}
		stored, err := st.GetOperation(op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Manifest != `{"bound":true}` {
			t.Fatalf("the bound manifest was not persisted: %q", stored.Manifest)
		}
		if n := claimsHeldBy(t, st, op.ID); n != 1 {
			t.Fatalf("binding a root released the operation's claims: %d", n)
		}
		if err := st.CommitOperation(op.ID); err != nil {
			t.Fatal(err)
		}
		if n := claimsHeldBy(t, st, op.ID); n != 0 {
			t.Fatalf("a committed operation kept %d claims", n)
		}
	})

	t.Run("rolling back only ends as a finished rollback", func(t *testing.T) {
		st := openTemp(t)
		op := newOp(t, st, "sg_rolling", "/repo/d.md")
		if err := st.MarkApplying(op.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkRollingBack(op.ID, "given up on"); err != nil {
			t.Fatal(err)
		}
		if n := claimsHeldBy(t, st, op.ID); n != 1 {
			t.Fatalf("an operation mid-restoration released its claims: %d", n)
		}
		if err := st.CommitOperation(op.ID); err == nil {
			t.Fatal("an abandoned operation was forward-committed")
		}
		if err := st.AbandonOperation(op.ID, "pretend nothing happened"); err == nil {
			t.Fatal("a rolling-back operation was closed as if it had never written")
		}
		if got := stateOf(t, st, op.ID); got != OpRollingBack {
			t.Fatalf("a refused transition moved the operation to %s", got)
		}
		if err := st.FinishRollback(op.ID, "restored"); err != nil {
			t.Fatal(err)
		}
		if got := stateOf(t, st, op.ID); got != OpRolledBack {
			t.Fatalf("finishing a rollback left the operation %s", got)
		}
		if n := claimsHeldBy(t, st, op.ID); n != 0 {
			t.Fatalf("a rolled-back operation kept %d claims", n)
		}
		g, err := st.GetSuggestion("sg_rolling")
		if err != nil {
			t.Fatal(err)
		}
		if g.Status != "pending" {
			t.Fatalf("a rolled-back operation decided the suggestion anyway: %s", g.Status)
		}
	})

	t.Run("terminal states are terminal", func(t *testing.T) {
		st := openTemp(t)
		op := newOp(t, st, "sg_terminal", "/repo/e.md")
		if err := st.MarkApplying(op.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.CommitOperation(op.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkRollingBack(op.ID, "second thoughts"); err == nil {
			t.Fatal("a committed operation was reopened for rollback")
		}
		if err := st.AbandonOperation(op.ID, "second thoughts"); err == nil {
			t.Fatal("a committed operation was abandoned")
		}
		if err := st.RebindRoot(op.ID, `{"bound":true}`); err == nil {
			t.Fatal("a committed operation rebound a root")
		}
		if got := stateOf(t, st, op.ID); got != OpCommitted {
			t.Fatalf("a committed operation moved to %s", got)
		}
	})
}

// Rejecting has no filesystem effect, which is why it needs the guard: recorded against a
// suggestion whose acceptance is half on disk, it would mean "rejected" next to an artifact.
func TestRejectIsPendingOnlyAndRefusesInFlight(t *testing.T) {
	st := openTemp(t)
	if err := st.InsertSuggestion(pending("sg_rj")); err != nil {
		t.Fatal(err)
	}
	op := acceptOp("sg_rj")
	if err := st.BeginOperation(op, "pending", []string{"/repo/AGENTS.md"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Reject("sg_rj"); !errors.Is(err, ErrOperationInFlight) {
		t.Fatalf("rejecting a suggestion with an unfinished operation must be refused, got %v", err)
	}

	if err := st.MarkApplying(op.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.CommitOperation(op.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Reject("sg_rj"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("rejecting an accepted suggestion must lose the compare-and-set, got %v", err)
	}
	g, err := st.GetSuggestion("sg_rj")
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != "accepted" {
		t.Fatalf("status = %q: a rejection contradicted a committed acceptance", g.Status)
	}
}
