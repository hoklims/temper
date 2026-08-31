// Package store persists ingest progress and suggestions in a local SQLite database at
// ~/.temper/temper.db. Everything Temper knows lives here — there is no cloud service.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hoklims/temper/internal/migrate"
	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	path string
}

// ErrCorrupt marks a database that cannot be trusted to hold the acceptance journal. It is never
// repaired in place and never silently recreated: losing the journal silently would turn a crash
// into an undetectable partially-accepted state.
var ErrCorrupt = errors.New("store: database is corrupt")

type Evidence struct {
	Excerpt   string `json:"excerpt"`
	SessionID string `json:"sessionId"`
	Tool      string `json:"tool"`
}

type Suggestion struct {
	ID          string     `json:"id"`
	CreatedAt   time.Time  `json:"createdAt"`
	Status      string     `json:"status"` // pending | accepted | rejected
	Title       string     `json:"title"`
	Signal      string     `json:"signal"`    // correction | rediscovery | failure_fix | convention | workflow
	Scope       string     `json:"scope"`     // machine | repo
	Placement   string     `json:"placement"` // always_on | path_scoped | skill
	Sensitivity bool       `json:"sensitivity"`
	Confidence  float64    `json:"confidence"`
	Project     string     `json:"project"`
	RepoRoot    string     `json:"repoRoot"`
	TargetPath  string     `json:"targetPath"` // predicted (pending) or actual (accepted) destination, repo-relative
	Globs       string     `json:"globs"`      // comma-separated path globs for path_scoped placements
	Body        string     `json:"body"`
	Rationale   string     `json:"rationale"`
	Evidence    []Evidence `json:"evidence"`
	SessionID   string     `json:"sessionId"`
	Tool        string     `json:"tool"`
	WrittenPath string     `json:"writtenPath,omitempty"`
	// BlockID is the managed-block id this suggestion writes to. Empty means "own id" (new
	// skill). Gardener suggestions set it to an EXISTING block id to amend it — or, with an
	// empty Body, to prune it.
	BlockID string `json:"blockId,omitempty"`
}

func DefaultPath() string {
	return filepath.Join(migrate.CurrentDir(), migrate.CurrentDBName)
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, path: path}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Path is the database file this store was opened from.
func (s *Store) Path() string { return s.path }

// init proves the file is a usable database before any statement can half-succeed against it,
// then brings the schema to the version this binary understands.
func (s *Store) init() error {
	var quick string
	if err := s.db.QueryRow(`PRAGMA quick_check(1)`).Scan(&quick); err != nil {
		return fmt.Errorf("%w: %s is not a readable SQLite database (%v); move it aside and rerun to start a fresh one, or restore a .backup-v*.db file next to it", ErrCorrupt, s.path, err)
	}
	if quick != "ok" {
		return fmt.Errorf("%w: %s failed quick_check (%s); restore a .backup-v*.db file next to it, or move it aside to start a fresh one", ErrCorrupt, s.path, quick)
	}
	if err := s.migrate(); err != nil {
		return err
	}
	// quick_check answers "are these pages readable", not "is this the schema this binary needs".
	// A database stamped at the latest version whose operations table is missing a column passes
	// every physical check and then breaks at the first acceptance, halfway through a decision.
	return s.verifySchema()
}

// requiredSchema is the shape this binary needs at latestVersion. It is checked at open time
// because the alternative is discovering the gap during a mutation, which is exactly the moment
// there is no safe answer left.
var requiredSchema = []struct {
	table   string
	columns []string
}{
	{"ingest_files", []string{"path", "bytes_processed", "updated_at"}},
	{"suggestions", []string{"id", "created_at", "status", "title", "signal", "scope", "placement",
		"sensitivity", "confidence", "project", "repo_root", "target_path", "globs", "body",
		"rationale", "evidence_json", "session_id", "tool", "written_path", "block_id", "decided_at"}},
	{"operations", []string{"id", "suggestion_id", "kind", "state", "manifest_json", "from_status",
		"target_status", "target_body", "target_path", "note", "created_at", "updated_at"}},
	{"resource_claims", []string{"resource", "operation_id", "created_at"}},
}

// ErrSchemaIncomplete marks a database whose version claims a shape it does not have. It is
// distinct from ErrCorrupt because the recovery is different: the pages are fine, it is the schema
// that was truncated or hand-edited, and the way out is a backup or a fresh database — never a
// silent re-migration, which would stamp the same version over the same gap.
var ErrSchemaIncomplete = errors.New("store: database does not have the schema its version claims")

func (s *Store) verifySchema() error {
	for _, want := range requiredSchema {
		have, err := s.tableColumns(want.table)
		if err != nil {
			return err
		}
		if len(have) == 0 {
			return fmt.Errorf("%w: %s is stamped v%d but has no %s table; restore a .backup-v*.db file next to it, or move it aside to start a fresh one",
				ErrSchemaIncomplete, s.path, latestVersion(), want.table)
		}
		for _, col := range want.columns {
			if !have[col] {
				return fmt.Errorf("%w: %s is stamped v%d but %s.%s is missing; restore a .backup-v*.db file next to it, or move it aside to start a fresh one",
					ErrSchemaIncomplete, s.path, latestVersion(), want.table, col)
			}
		}
	}
	return nil
}

func (s *Store) tableColumns(table string) (map[string]bool, error) {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read the shape of %s in %s: %v", ErrSchemaIncomplete, table, s.path, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultVal, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func (s *Store) userVersion() (int, error) {
	var v int
	err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}

// migrate walks user_version up to latestVersion. Every step is a single transaction that also
// bumps user_version, so an interrupted or failing migration leaves the previous version intact
// rather than a half-shaped schema.
func (s *Store) migrate() error {
	v, err := s.userVersion()
	if err != nil {
		return fmt.Errorf("%w: cannot read schema version of %s: %v", ErrCorrupt, s.path, err)
	}
	target := latestVersion()
	if v > target {
		return fmt.Errorf("store: %s has schema v%d, written by a newer temper (this build understands v%d); upgrade temper instead of downgrading the database", s.path, v, target)
	}
	if v == target {
		return nil
	}

	// An existing database is checked in full and copied before its shape changes: a failed
	// migration must leave a recoverable artifact, not a judgement call.
	backup := ""
	if v > 0 || s.hasUserTables() {
		if err := s.integrityCheck(); err != nil {
			return err
		}
		if backup, err = s.backup(v); err != nil {
			return fmt.Errorf("store: refusing to migrate %s without a backup: %w", s.path, err)
		}
	}

	for _, m := range migrations {
		if m.version <= v {
			continue
		}
		if err := s.applyMigration(m); err != nil {
			return fmt.Errorf("store: migration v%d→v%d failed and was rolled back; %s stays at v%d%s: %w",
				v, m.version, s.path, v, backupHint(backup), err)
		}
		v = m.version
	}
	return nil
}

func backupHint(backup string) string {
	if backup == "" {
		return ""
	}
	return " (backup: " + backup + ")"
}

func (s *Store) hasUserTables() bool {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('suggestions','ingest_files','operations')`).Scan(&n)
	return err == nil && n > 0
}

func (s *Store) integrityCheck() error {
	rows, err := s.db.Query(`PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("%w: integrity_check on %s failed: %v", ErrCorrupt, s.path, err)
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("%w: integrity_check on %s failed: %v", ErrCorrupt, s.path, err)
		}
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: integrity_check on %s failed: %v", ErrCorrupt, s.path, err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s failed integrity_check (%s); restore a .backup-v*.db file next to it before migrating", ErrCorrupt, s.path, problems[0])
	}
	return nil
}

// backup writes a consistent copy of the database (VACUUM INTO reads a snapshot, so the WAL is
// included) next to it, named after the version it is a backup of.
func (s *Store) backup(version int) (string, error) {
	dest := fmt.Sprintf("%s.backup-v%d-%s.db", s.path, version, time.Now().UTC().Format("20060102T150405.000"))
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("backup %s already exists", dest)
	}
	if _, err := s.db.Exec(`VACUUM INTO ?`, dest); err != nil {
		return "", err
	}
	return dest, nil
}

type migration struct {
	version int
	apply   func(tx *sql.Tx) error
}

func latestVersion() int {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].version
}

// applyMigration runs one step inside a transaction. SQLite keeps DDL and user_version
// transactional, so a failure anywhere leaves the database exactly at the previous version.
func (s *Store) applyMigration(m migration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := m.apply(tx); err != nil {
		return err
	}
	// user_version takes no bind parameter; m.version is an int constant from this file
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
		return err
	}
	return tx.Commit()
}

// migrations is ordered and append-only. v1 is the schema every pre-versioning database already
// has on disk, so adopting one is a no-op that only stamps its version.
var migrations = []migration{
	{version: 1, apply: func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS ingest_files (
  path            TEXT PRIMARY KEY,
  bytes_processed INTEGER NOT NULL DEFAULT 0,
  updated_at      TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS suggestions (
  id            TEXT PRIMARY KEY,
  created_at    TEXT NOT NULL,
  status        TEXT NOT NULL DEFAULT 'pending',
  title         TEXT NOT NULL,
  signal        TEXT NOT NULL DEFAULT '',
  scope         TEXT NOT NULL DEFAULT 'repo',
  placement     TEXT NOT NULL DEFAULT 'always_on',
  sensitivity   INTEGER NOT NULL DEFAULT 0,
  confidence    REAL NOT NULL DEFAULT 0,
  project       TEXT NOT NULL DEFAULT '',
  repo_root     TEXT NOT NULL DEFAULT '',
  target_path   TEXT NOT NULL DEFAULT '',
  globs         TEXT NOT NULL DEFAULT '',
  body          TEXT NOT NULL DEFAULT '',
  rationale     TEXT NOT NULL DEFAULT '',
  evidence_json TEXT NOT NULL DEFAULT '[]',
  session_id    TEXT NOT NULL DEFAULT '',
  tool          TEXT NOT NULL DEFAULT '',
  written_path  TEXT NOT NULL DEFAULT '',
  block_id      TEXT NOT NULL DEFAULT '',
  decided_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_suggestions_status ON suggestions(status);
`); err != nil {
			return err
		}
		// columns added by older binaries with unconditional ALTERs; here the check is explicit
		// because a duplicate-column error would abort the whole migration transaction
		for _, c := range []struct{ name, ddl string }{
			{"globs", `ALTER TABLE suggestions ADD COLUMN globs TEXT NOT NULL DEFAULT ''`},
			{"block_id", `ALTER TABLE suggestions ADD COLUMN block_id TEXT NOT NULL DEFAULT ''`},
		} {
			has, err := hasColumn(tx, "suggestions", c.name)
			if err != nil {
				return err
			}
			if !has {
				if _, err := tx.Exec(c.ddl); err != nil {
					return err
				}
			}
		}
		return nil
	}},
	{version: 2, apply: func(tx *sql.Tx) error {
		// The acceptance journal. manifest_json is opaque to the store — the writer owns its shape —
		// but target_status/body/path are read back by CommitOperation so a reconciled operation can
		// finish the decision without the original request.
		_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS operations (
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
);
CREATE INDEX IF NOT EXISTS idx_operations_state ON operations(state);
`)
		return err
	}},
	{version: 3, apply: func(tx *sql.Tx) error {
		// resource_claims widens the reservation from "this suggestion" to "every file this
		// manifest touches": two different suggestions writing the same AGENTS.md are the same
		// conflict, and the second one planned its content from a snapshot the first invalidates.
		if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS resource_claims (
  resource     TEXT PRIMARY KEY,
  operation_id TEXT NOT NULL,
  created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_resource_claims_operation ON resource_claims(operation_id);
`); err != nil {
			return err
		}
		has, err := hasColumn(tx, "operations", "from_status")
		if err != nil {
			return err
		}
		if !has {
			if _, err := tx.Exec(`ALTER TABLE operations ADD COLUMN from_status TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
		// v2 rows carried their expected source status implicitly in `kind`; CommitOperation now
		// asserts it explicitly, so an interrupted v2 operation must not become uncommittable.
		_, err = tx.Exec(`UPDATE operations SET from_status =
CASE kind WHEN 'undo' THEN 'accepted' ELSE 'pending' END WHERE from_status = ''`)
		return err
	}},
}

func hasColumn(tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultVal, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

// BytesProcessed returns the high-water mark for a transcript file (0 if never seen).
func (s *Store) BytesProcessed(path string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT bytes_processed FROM ingest_files WHERE path = ?`, path).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

// SetBytesProcessed advances the high-water mark for a file that produced no suggestion (skipped,
// unparseable, filtered). Use AdvanceCheckpoint when suggestions come with it.
func (s *Store) SetBytesProcessed(path string, n int64) error {
	return setBytesProcessed(s.db, path, n)
}

// AdvanceCheckpoint stores a distilled file's suggestions and its high-water mark in one
// transaction. The mark is the claim "everything up to here is persisted"; letting it advance
// past a failed insert would erase those suggestions from every future scan.
func (s *Store) AdvanceCheckpoint(path string, n int64, suggestions []Suggestion) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, g := range suggestions {
		if err := insertSuggestion(tx, g); err != nil {
			return fmt.Errorf("store: suggestion %s not persisted, checkpoint for %s not advanced: %w", g.ID, path, err)
		}
	}
	if err := setBytesProcessed(tx, path, n); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) InsertSuggestion(g Suggestion) error { return insertSuggestion(s.db, g) }

// execer is satisfied by both *sql.DB and *sql.Tx, so a statement reads the same whether it runs
// alone or inside a checkpoint transaction.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func setBytesProcessed(e execer, path string, n int64) error {
	_, err := e.Exec(`INSERT INTO ingest_files(path, bytes_processed, updated_at) VALUES(?,?,?)
ON CONFLICT(path) DO UPDATE SET bytes_processed=excluded.bytes_processed, updated_at=excluded.updated_at`,
		path, n, time.Now().UTC().Format(time.RFC3339))
	return err
}

func insertSuggestion(e execer, g Suggestion) error {
	ev, err := json.Marshal(g.Evidence)
	if err != nil {
		return err
	}
	_, err = e.Exec(`INSERT INTO suggestions
(id, created_at, status, title, signal, scope, placement, sensitivity, confidence, project, repo_root, target_path, globs, body, rationale, evidence_json, session_id, tool, block_id)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.ID, g.CreatedAt.UTC().Format(time.RFC3339), g.Status, g.Title, g.Signal, g.Scope, g.Placement,
		boolToInt(g.Sensitivity), g.Confidence, g.Project, g.RepoRoot, g.TargetPath, g.Globs, g.Body, g.Rationale,
		string(ev), g.SessionID, g.Tool, g.BlockID)
	return err
}

// TitleExists reports whether a suggestion with this exact title already exists for the repo —
// the cheap layer of dedupe beneath the prompt-level dedupe.
func (s *Store) TitleExists(repoRoot, title string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM suggestions WHERE repo_root = ? AND title = ?`, repoRoot, title).Scan(&n)
	return n > 0, err
}

// ExistingTitles returns all suggestion titles for a repo (any status) for prompt-level dedupe.
func (s *Store) ExistingTitles(repoRoot string) ([]string, error) {
	rows, err := s.db.Query(`SELECT title FROM suggestions WHERE repo_root = ?`, repoRoot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) ListSuggestions(status string) ([]Suggestion, error) {
	q := `SELECT id, created_at, status, title, signal, scope, placement, sensitivity, confidence,
project, repo_root, target_path, globs, body, rationale, evidence_json, session_id, tool, written_path, block_id
FROM suggestions`
	var args []any
	if status != "" && status != "all" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Suggestion
	for rows.Next() {
		g, err := scanSuggestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) GetSuggestion(id string) (Suggestion, error) {
	row := s.db.QueryRow(`SELECT id, created_at, status, title, signal, scope, placement, sensitivity, confidence,
project, repo_root, target_path, globs, body, rationale, evidence_json, session_id, tool, written_path, block_id
FROM suggestions WHERE id = ?`, id)
	return scanSuggestion(row)
}

// Reject records the one decision that has no filesystem effect to journal. It is a
// compare-and-set from pending inside a transaction, and refuses while any operation for this
// suggestion is unfinished: a rejection racing a committed acceptance would leave the row saying
// "rejected" next to an artifact that is still on disk. An accept mutates files, so it goes
// through the operation journal (BeginOperation → CommitOperation) instead — recording it here
// would put the decision and the write in two separate commits.
func (s *Store) Reject(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRow(`SELECT status FROM suggestions WHERE id = ?`, id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("suggestion %s not found", id)
		}
		return err
	}
	var inflight int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE suggestion_id = ? AND state IN `+unfinishedPlaceholders,
		append([]any{id}, unfinishedStates...)...).Scan(&inflight); err != nil {
		return err
	}
	if inflight > 0 {
		return fmt.Errorf("%w: suggestion %s; restart temper to reconcile it", ErrOperationInFlight, id)
	}
	if err := decide(tx, id, "pending", "rejected", "", ""); err != nil {
		return err
	}
	return tx.Commit()
}

// decide applies a decision only if the suggestion is still in the status it was planned against.
// The WHERE clause is the whole guarantee: two clients cannot both succeed, and the loser is told
// it lost rather than silently overwriting the winner.
func decide(e execer, id, fromStatus, status, body, writtenPath string) error {
	res, err := e.Exec(`UPDATE suggestions SET status = ?, body = COALESCE(NULLIF(?, ''), body),
written_path = ?, decided_at = ? WHERE id = ? AND status = ?`,
		status, body, writtenPath, time.Now().UTC().Format(time.RFC3339), id, fromStatus)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: suggestion %s is no longer %s", ErrNotPending, id, fromStatus)
	}
	return nil
}

type Stats struct {
	Pending  int `json:"pending"`
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
	Sessions int `json:"sessions"`
	Projects int `json:"projects"`
}

func (s *Store) Stats() (Stats, error) {
	var st Stats
	err := s.db.QueryRow(`SELECT
COUNT(CASE WHEN status='pending' THEN 1 END),
COUNT(CASE WHEN status='accepted' THEN 1 END),
COUNT(CASE WHEN status='rejected' THEN 1 END),
COUNT(DISTINCT session_id),
COUNT(DISTINCT project)
FROM suggestions`).Scan(&st.Pending, &st.Accepted, &st.Rejected, &st.Sessions, &st.Projects)
	return st, err
}

type ProjectCount struct {
	Name     string `json:"name"`
	RepoRoot string `json:"repoRoot"`
	Pending  int    `json:"pending"`
}

func (s *Store) Projects() ([]ProjectCount, error) {
	rows, err := s.db.Query(`SELECT project, repo_root, COUNT(CASE WHEN status='pending' THEN 1 END)
FROM suggestions GROUP BY project, repo_root ORDER BY project`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectCount
	for rows.Next() {
		var p ProjectCount
		if err := rows.Scan(&p.Name, &p.RepoRoot, &p.Pending); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanSuggestion(r rowScanner) (Suggestion, error) {
	var g Suggestion
	var created, evidenceJSON string
	var sens int
	err := r.Scan(&g.ID, &created, &g.Status, &g.Title, &g.Signal, &g.Scope, &g.Placement, &sens,
		&g.Confidence, &g.Project, &g.RepoRoot, &g.TargetPath, &g.Globs, &g.Body, &g.Rationale, &evidenceJSON,
		&g.SessionID, &g.Tool, &g.WrittenPath, &g.BlockID)
	if err != nil {
		return g, err
	}
	g.Sensitivity = sens != 0
	g.CreatedAt, _ = time.Parse(time.RFC3339, created)
	_ = json.Unmarshal([]byte(evidenceJSON), &g.Evidence)
	if g.Evidence == nil {
		g.Evidence = []Evidence{} // never serialize null to the frontend
	}
	return g, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
