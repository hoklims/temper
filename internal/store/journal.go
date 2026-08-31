package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The acceptance journal. A decision is not "the file was written" plus "the row was updated" —
// those two can be separated by a power cut. It is a durable saga: the intent is recorded first
// (prepared), the filesystem is mutated second (applying), and the decision lands with the
// journal in one transaction last (committed). Anything found in prepared or applying at startup
// is finished or restored by writer.Reconcile, so no crash can leave a half-accepted suggestion.
const (
	OpPrepared = "prepared"
	OpApplying = "applying"
	// OpRollingBack is the durable intent to give an operation up. It is recorded BEFORE the
	// filesystem is restored, so an interrupted rollback is resumed as a rollback. Without it, an
	// operation whose restoration died halfway would still read "applying" and the next
	// reconciliation would replay it forward — completing a decision that had been abandoned.
	OpRollingBack = "rolling_back"
	OpCommitted   = "committed"
	OpRolledBack  = "rolled_back"
)

// unfinishedStates are the states in which an operation still owns its resource claims and may
// still disagree with the filesystem. Anything that asks "is something in flight here?" asks about
// exactly these, so they are named once.
var unfinishedStates = []any{OpPrepared, OpApplying, OpRollingBack}

const unfinishedPlaceholders = `(?,?,?)`

// The three ways a decision can be refused because the world moved. They are sentinels because
// callers must be able to tell "you lost a race" (retry after a refresh) from "the database
// broke" — the review API answers 409 for the first and 500 for the second.
var (
	// ErrNotPending is a lost compare-and-set on suggestions.status: the suggestion left the
	// state this decision was computed for, so applying it would overwrite someone else's outcome.
	ErrNotPending = errors.New("store: the suggestion is no longer in the state this decision requires")
	// ErrOperationInFlight means an unfinished operation already owns this suggestion.
	ErrOperationInFlight = errors.New("store: the suggestion already has an unfinished operation")
	// ErrResourceBusy means another unfinished operation holds a file this one would touch.
	ErrResourceBusy = errors.New("store: a file this operation needs is claimed by an unfinished operation")
)

// Operation is one journaled mutation. Manifest is opaque here — the writer owns its shape — but
// the target fields belong to the store because they are what CommitOperation applies, including
// when the original request no longer exists because the process died.
type Operation struct {
	ID           string
	SuggestionID string
	Kind         string // accept | undo
	State        string
	Manifest     string
	// FromStatus is the suggestion status this operation was planned against. It is asserted
	// again when the operation commits, so an operation prepared against a stale read cannot
	// decide a suggestion someone else has since decided.
	FromStatus   string
	TargetStatus string
	TargetBody   string
	TargetPath   string
	Note         string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const operationColumns = `id, suggestion_id, kind, state, manifest_json, from_status, target_status, target_body, target_path, note, created_at, updated_at`

// NewOperationID mints an identifier for a journal entry.
func NewOperationID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "op_" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return "op_" + hex.EncodeToString(b[:])
}

// BeginOperation records the intent to mutate before anything on disk moves. Three things are
// proved in one transaction, because any of them checked earlier is already stale by the time the
// first byte moves:
//
//   - the suggestion is still in fromStatus (the pending-only rule, enforced by the database);
//   - no other operation on this suggestion is unfinished;
//   - every resource in the manifest is free, and becomes claimed by this operation.
//
// The third is what suggestion-level locking cannot do: two DIFFERENT suggestions both appending
// to the same AGENTS.md are the same conflict, and each planned its content from a snapshot the
// other is about to invalidate. The claims are rows, so they survive a crash and keep the resource
// reserved until reconciliation finishes or releases the operation that holds them.
func (s *Store) BeginOperation(op Operation, fromStatus string, resources []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRow(`SELECT status FROM suggestions WHERE id = ?`, op.SuggestionID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("suggestion %s not found", op.SuggestionID)
		}
		return err
	}
	if status != fromStatus {
		return fmt.Errorf("%w: suggestion %s is %s, not %s", ErrNotPending, op.SuggestionID, status, fromStatus)
	}

	var inflight int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE suggestion_id = ? AND state IN `+unfinishedPlaceholders,
		append([]any{op.SuggestionID}, unfinishedStates...)...).Scan(&inflight); err != nil {
		return err
	}
	if inflight > 0 {
		return fmt.Errorf("%w: suggestion %s; restart temper to reconcile it", ErrOperationInFlight, op.SuggestionID)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := claimResources(tx, op.ID, resources, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO operations
(`+operationColumns+`)
VALUES (?,?,?,?,?,?,?,?,?,'',?,?)`,
		op.ID, op.SuggestionID, op.Kind, OpPrepared, op.Manifest, fromStatus,
		op.TargetStatus, op.TargetBody, op.TargetPath, now, now); err != nil {
		return err
	}
	return tx.Commit()
}

// claimResources reserves each resource for this operation, refusing any that an unfinished
// operation already holds. A claim left behind by an operation that has since committed or rolled
// back is dead by construction — its owner can no longer touch anything — so it is taken over
// rather than treated as a conflict.
func claimResources(tx *sql.Tx, opID string, resources []string, now string) error {
	for _, r := range resources {
		var holder, state string
		err := tx.QueryRow(`SELECT c.operation_id, o.state FROM resource_claims c
JOIN operations o ON o.id = c.operation_id WHERE c.resource = ?`, r).Scan(&holder, &state)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case isUnfinished(state):
			return fmt.Errorf("%w: %s is held by operation %s (%s); restart temper to reconcile it",
				ErrResourceBusy, r, holder, state)
		}
		if _, err := tx.Exec(`INSERT INTO resource_claims(resource, operation_id, created_at) VALUES(?,?,?)
ON CONFLICT(resource) DO UPDATE SET operation_id = excluded.operation_id, created_at = excluded.created_at`,
			r, opID, now); err != nil {
			return err
		}
	}
	return nil
}

func releaseClaims(tx *sql.Tx, opID string) error {
	_, err := tx.Exec(`DELETE FROM resource_claims WHERE operation_id = ?`, opID)
	return err
}

func isUnfinished(state string) bool {
	for _, s := range unfinishedStates {
		if s == state {
			return true
		}
	}
	return false
}

// MarkApplying is the durable line between "nothing was written" and "something may have been".
// It commits before the first byte moves, so an operation found in prepared provably never
// touched the filesystem.
func (s *Store) MarkApplying(id string) error {
	return s.setOperationState(id, OpApplying, "", OpPrepared)
}

// RebindRoot replaces the manifest of an operation that is STILL applying, and only then. It
// exists for one thing: an authorized root that did not exist when the mutation was planned is
// created during the mutation, and the identity of the directory this operation created has to
// become durable before the first byte is written into it. Without that, a restart would find an
// operation that may have written under a directory it can no longer prove it made.
//
// It is deliberately not a general "edit the journal" door: prepared has nothing to bind yet, and
// anything terminal is a decision that has already been made.
func (s *Store) RebindRoot(id, manifest string) error {
	res, err := s.db.Exec(`UPDATE operations SET manifest_json = ?, updated_at = ? WHERE id = ? AND state = ?`,
		manifest, time.Now().UTC().Format(time.RFC3339Nano), id, OpApplying)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("operation %s is not applying, so its authorized roots cannot be bound", id)
	}
	return nil
}

// MarkRollingBack records that an operation has been given up on, before a single preimage is put
// back. It keeps the resource claims — the files are still mid-restoration and handing them to
// another operation would race the rollback — and it is what makes an interrupted rollback resume
// as a rollback instead of being replayed forward.
//
// Only an operation that may have written can enter it. A prepared one provably touched nothing,
// so it has nothing to roll back and goes straight to AbandonOperation; routing it through here
// would manufacture a restoration state for a mutation that never started.
func (s *Store) MarkRollingBack(id, note string) error {
	return s.setOperationState(id, OpRollingBack, note, OpApplying, OpRollingBack)
}

// CommitOperation is the atomic pivot of the saga: the suggestion's decision, the release of the
// resource claims and the journal entry all close in the same transaction. The decision is a
// compare-and-set on the status this operation was planned against, so an operation that raced
// with another decision fails instead of overwriting it.
//
// Only an applying operation commits. A prepared one has not been through the durable line that
// says "bytes may have moved", so committing it would record a decision whose filesystem half
// nobody ever performed — and would leave the artifact missing next to a row that says accepted.
func (s *Store) CommitOperation(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var op Operation
	if err := tx.QueryRow(`SELECT suggestion_id, state, from_status, target_status, target_body, target_path
FROM operations WHERE id = ?`, id).Scan(&op.SuggestionID, &op.State, &op.FromStatus,
		&op.TargetStatus, &op.TargetBody, &op.TargetPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("operation %s not found", id)
		}
		return err
	}
	if op.State == OpCommitted {
		return nil // replayed reconciliation: already durable
	}
	if op.State != OpApplying {
		return fmt.Errorf("operation %s is %s and cannot be committed", id, op.State)
	}
	if err := decide(tx, op.SuggestionID, op.FromStatus, op.TargetStatus, op.TargetBody, op.TargetPath); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE operations SET state = ?, updated_at = ? WHERE id = ?`, OpCommitted, now, id); err != nil {
		return err
	}
	if err := releaseClaims(tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// AbandonOperation closes an operation that has nothing to restore, releasing its resource claims
// in the same transaction. The suggestion is left exactly as it was, so the human can decide again.
//
// The caller carries the proof that no target byte moved — a manifest refused before the first
// write, or a mutation that failed on its first operation. From applying, that proof is the
// caller's; the store can only refuse the states where it is certainly false.
func (s *Store) AbandonOperation(id, note string) error {
	return s.setOperationState(id, OpRolledBack, note, OpPrepared, OpApplying)
}

// FinishRollback closes an operation whose filesystem effects have been undone. It is separate
// from AbandonOperation because the two say different things: one ends a mutation that never
// started, the other ends one whose restoration is complete — and only the second may close a
// rolling_back entry, so an interrupted restoration cannot be signed off as "nothing happened".
func (s *Store) FinishRollback(id, note string) error {
	return s.setOperationState(id, OpRolledBack, note, OpRollingBack)
}

func (s *Store) setOperationState(id, state, note string, from ...string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	args := []any{state, note, now, id}
	q := `UPDATE operations SET state = ?, note = ?, updated_at = ? WHERE id = ?`
	if len(from) > 0 {
		q += ` AND state IN (?` + strings.Repeat(",?", len(from)-1) + `)`
		for _, f := range from {
			args = append(args, f)
		}
	}
	res, err := tx.Exec(q, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("operation %s is not in a state that can become %s", id, state)
	}
	// a terminal operation owns nothing: claims go with the state change, never after it
	if state == OpCommitted || state == OpRolledBack {
		if err := releaseClaims(tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// IncompleteOperations returns every operation that was interrupted before it closed, oldest
// first. Their existence is the signal that the filesystem and the store may disagree.
func (s *Store) IncompleteOperations() ([]Operation, error) {
	rows, err := s.db.Query(`SELECT `+operationColumns+`
FROM operations WHERE state IN `+unfinishedPlaceholders+` ORDER BY created_at, rowid`, unfinishedStates...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func (s *Store) GetOperation(id string) (Operation, error) {
	row := s.db.QueryRow(`SELECT `+operationColumns+` FROM operations WHERE id = ?`, id)
	return scanOperation(row)
}

// LastCommittedAccept returns the acceptance that is actually on disk for a suggestion, so an undo
// compensates the manifest that was applied rather than a removal recomputed from today's
// suggestion row. Reports false when the suggestion was accepted before the journal existed.
func (s *Store) LastCommittedAccept(suggestionID string) (Operation, bool, error) {
	row := s.db.QueryRow(`SELECT `+operationColumns+`
FROM operations WHERE suggestion_id = ? AND kind = 'accept' AND state = ?
ORDER BY rowid DESC LIMIT 1`, suggestionID, OpCommitted)
	op, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, false, nil
	}
	if err != nil {
		return Operation{}, false, err
	}
	return op, true, nil
}

func scanOperation(r rowScanner) (Operation, error) {
	var op Operation
	var created, updated string
	err := r.Scan(&op.ID, &op.SuggestionID, &op.Kind, &op.State, &op.Manifest, &op.FromStatus,
		&op.TargetStatus, &op.TargetBody, &op.TargetPath, &op.Note, &created, &updated)
	if err != nil {
		return op, err
	}
	op.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	op.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return op, nil
}
