package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

const (
	reviewStatusKind              = "review.status"
	reviewReactionKind            = "review.reaction"
	ReviewSideEffectMaxAttempts   = 5
	maxReviewStatusDetailRunes    = 500
	maxReviewSideEffectList       = 100
	maxRecentReviewDurations      = 20
	maxReviewStatusLastErrorRunes = 300
)

func reviewStatusRank(state string) int {
	switch state {
	case store.ReviewStatusQueued:
		return 1
	case store.ReviewStatusRunning:
		return 2
	case store.ReviewStatusPublishing:
		return 3
	case store.ReviewStatusPass, store.ReviewStatusBlocking, store.ReviewStatusIncomplete,
		store.ReviewStatusFailed, store.ReviewStatusCancelled, store.ReviewStatusStale:
		return 4
	}
	return 0
}

func validReviewStatusState(state string) bool {
	return reviewStatusRank(state) > 0
}

func reviewStatusTerminal(state string) bool {
	return reviewStatusRank(state) == 4
}

func validReviewReaction(desired string) bool {
	switch desired {
	case store.ReviewReactionEyes, store.ReviewReactionPass, store.ReviewReactionFail, store.ReviewReactionNone:
		return true
	}
	return false
}

func validateReviewStatusGoal(goal store.ReviewStatusGoal) error {
	if goal.ProjectID <= 0 || goal.MRIID <= 0 {
		return errors.New("sqlite: review status project id and mr iid are required")
	}
	if goal.Generation <= 0 {
		return errors.New("sqlite: review status generation is required")
	}
	if goal.Attempt < 0 {
		return errors.New("sqlite: review status attempt must not be negative")
	}
	if !validReviewStatusState(goal.State) {
		return fmt.Errorf("sqlite: invalid review status state %q", goal.State)
	}
	return nil
}

func validateReviewReactionGoal(goal store.ReviewReactionGoal) error {
	if goal.ProjectID <= 0 || goal.MRIID <= 0 || goal.NoteID <= 0 {
		return errors.New("sqlite: review reaction project id, mr iid, and note id are required")
	}
	if goal.Generation <= 0 {
		return errors.New("sqlite: review reaction generation is required")
	}
	if !validReviewReaction(goal.Desired) {
		return fmt.Errorf("sqlite: invalid review reaction %q", goal.Desired)
	}
	return nil
}

// SetReviewStatus writes one desired status and enqueues its publishing job in
// the same transaction. A newer generation always wins; a stale generation or
// a changed head is rejected with ErrSuperseded. Within one attempt a terminal
// state is final and phases are monotonic; a higher attempt may retry.
func (s *Store) SetReviewStatus(ctx context.Context, goal store.ReviewStatusGoal) (bool, error) {
	if err := validateReviewStatusGoal(goal); err != nil {
		return false, err
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("sqlite: begin review status write: %w", err)
	}
	defer tx.Rollback()

	written, err := s.setReviewStatusTx(ctx, tx, goal, now)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("sqlite: commit review status: %w", err)
	}
	return written, nil
}

func (s *Store) setReviewStatusTx(ctx context.Context, tx *sql.Tx, goal store.ReviewStatusGoal, now time.Time) (bool, error) {
	goal.Detail = truncateRunes(goal.Detail, maxReviewStatusDetailRunes)
	control, err := getReviewControlTx(ctx, tx, goal.ProjectID, goal.MRIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, fmt.Errorf("%w: review control for status goal", store.ErrSuperseded)
		}
		return false, err
	}
	if control.Generation != goal.Generation {
		return false, fmt.Errorf("%w: status generation %d is not current", store.ErrSuperseded, goal.Generation)
	}
	if goal.HeadSHA != "" && control.HeadSHA != "" && !strings.EqualFold(control.HeadSHA, goal.HeadSHA) {
		return false, fmt.Errorf("%w: status head %s is not current", store.ErrSuperseded, goal.HeadSHA)
	}

	existing, found, err := getReviewStatusTx(ctx, tx, goal.ProjectID, goal.MRIID)
	if err != nil {
		return false, err
	}
	if found {
		if goal.Generation < existing.Generation {
			return false, nil
		}
		if goal.Generation == existing.Generation {
			if goal.HeadSHA != "" && !strings.EqualFold(existing.HeadSHA, goal.HeadSHA) {
				return false, fmt.Errorf("%w: status head %s does not match the stored head", store.ErrSuperseded, goal.HeadSHA)
			}
			attempt := goal.Attempt
			if attempt == 0 {
				attempt = existing.Attempt
			}
			if attempt < existing.Attempt {
				return false, nil
			}
			if attempt == existing.Attempt {
				if reviewStatusTerminal(existing.State) {
					return false, nil
				}
				if reviewStatusRank(goal.State) < reviewStatusRank(existing.State) {
					return false, nil
				}
			}
		}
	}
	attempt := goal.Attempt
	if found && attempt == 0 {
		attempt = existing.Attempt
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_status
		(project_id, mr_iid, generation, head_sha, attempt, profile, state, detail,
		 note_id, body_hash, version, applied_version, last_error, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, '', 1, 0, '', ?)
		ON CONFLICT (project_id, mr_iid) DO UPDATE SET
			generation = excluded.generation,
			head_sha = CASE WHEN excluded.head_sha <> '' THEN excluded.head_sha ELSE review_status.head_sha END,
			attempt = excluded.attempt,
			profile = CASE WHEN excluded.profile <> '' THEN excluded.profile ELSE review_status.profile END,
			state = excluded.state,
			detail = excluded.detail,
			note_id = NULL,
			body_hash = '',
			version = review_status.version + 1,
			last_error = '',
			updated_at = excluded.updated_at`,
		goal.ProjectID, goal.MRIID, goal.Generation,
		strings.ToLower(strings.TrimSpace(goal.HeadSHA)), attempt, goal.Profile,
		goal.State, goal.Detail, formatTime(now)); err != nil {
		return false, fmt.Errorf("sqlite: write review status: %w", err)
	}
	if err := s.enqueueReviewStatusJobTx(ctx, tx, goal.ProjectID, goal.MRIID, now); err != nil {
		return false, err
	}
	return true, nil
}

// SetReviewReaction writes one desired reaction and enqueues its publishing
// job. A stale generation may remove its own eyes (desired none), but may not
// publish a success or failure over a newer generation.
func (s *Store) SetReviewReaction(ctx context.Context, goal store.ReviewReactionGoal) (bool, error) {
	if err := validateReviewReactionGoal(goal); err != nil {
		return false, err
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("sqlite: begin review reaction write: %w", err)
	}
	defer tx.Rollback()

	written, err := s.setReviewReactionTx(ctx, tx, goal, now)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("sqlite: commit review reaction: %w", err)
	}
	return written, nil
}

func (s *Store) setReviewReactionTx(ctx context.Context, tx *sql.Tx, goal store.ReviewReactionGoal, now time.Time) (bool, error) {
	if goal.Desired != store.ReviewReactionNone {
		control, err := getReviewControlTx(ctx, tx, goal.ProjectID, goal.MRIID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return false, fmt.Errorf("%w: review control for reaction goal", store.ErrSuperseded)
			}
			return false, err
		}
		if control.Generation != goal.Generation {
			return false, nil
		}
	}

	existing, found, err := getReviewReactionTx(ctx, tx, goal.ProjectID, goal.MRIID, goal.NoteID)
	if err != nil {
		return false, err
	}
	generation := goal.Generation
	if found {
		// A stale generation may never rewrite a newer row, including a
		// removal: the newer row may hold the successor's eyes or result.
		if goal.Generation < existing.Generation {
			return false, nil
		}
	} else if goal.Desired == store.ReviewReactionNone {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_reactions
		(project_id, mr_iid, note_id, generation, desired, version, applied_version, last_error, updated_at)
		VALUES (?, ?, ?, ?, ?, 1, 0, '', ?)
		ON CONFLICT (project_id, mr_iid, note_id) DO UPDATE SET
			generation = excluded.generation,
			desired = excluded.desired,
			version = review_reactions.version + 1,
			last_error = '',
			updated_at = excluded.updated_at`,
		goal.ProjectID, goal.MRIID, goal.NoteID, generation, goal.Desired,
		formatTime(now)); err != nil {
		return false, fmt.Errorf("sqlite: write review reaction: %w", err)
	}
	if err := s.enqueueReviewReactionJobTx(ctx, tx, goal.ProjectID, goal.MRIID, goal.NoteID, now); err != nil {
		return false, err
	}
	return true, nil
}

// ClearOlderReviewReactions turns every older generation's eyes into none and
// enqueues removal jobs. It returns the affected note IDs. It only touches
// eyes, so a user reaction and a newer desired state stay intact.
func (s *Store) ClearOlderReviewReactions(ctx context.Context, projectID, mrIID, generation int64) ([]int64, error) {
	return s.clearReviewReactions(ctx, projectID, mrIID, generation, true)
}

// ClearReviewReactionsForGeneration removes the eyes of exactly one
// generation. Cancellation uses it after the cancel job settles.
func (s *Store) ClearReviewReactionsForGeneration(ctx context.Context, projectID, mrIID, generation int64) ([]int64, error) {
	return s.clearReviewReactions(ctx, projectID, mrIID, generation, false)
}

func (s *Store) clearReviewReactions(ctx context.Context, projectID, mrIID, generation int64, older bool) ([]int64, error) {
	if projectID <= 0 || mrIID <= 0 || generation <= 0 {
		return nil, errors.New("sqlite: review reaction project id, mr iid, and generation are required")
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin review reaction cleanup: %w", err)
	}
	defer tx.Rollback()

	notes, err := s.clearReviewReactionsTx(ctx, tx, projectID, mrIID, generation, older, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit review reaction cleanup: %w", err)
	}
	return notes, nil
}

func (s *Store) clearReviewReactionsTx(ctx context.Context, tx *sql.Tx, projectID, mrIID, generation int64, older bool, now time.Time) ([]int64, error) {
	query := `SELECT note_id FROM review_reactions
		WHERE project_id = ? AND mr_iid = ? AND desired = ? AND generation = ?`
	args := []any{projectID, mrIID, store.ReviewReactionEyes, generation}
	if older {
		query = `SELECT note_id FROM review_reactions
			WHERE project_id = ? AND mr_iid = ? AND desired = ? AND generation < ?`
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list review reactions to clear: %w", err)
	}
	var notes []int64
	for rows.Next() {
		var noteID int64
		if err := rows.Scan(&noteID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("sqlite: scan review reaction to clear: %w", err)
		}
		notes = append(notes, noteID)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("sqlite: close review reactions to clear: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate review reactions to clear: %w", err)
	}
	generationWhere := "generation = ?"
	if older {
		generationWhere = "generation < ?"
	}
	for _, noteID := range notes {
		if _, err := tx.ExecContext(ctx, `UPDATE review_reactions
			SET desired = ?, version = version + 1, last_error = '', updated_at = ?
			WHERE project_id = ? AND mr_iid = ? AND note_id = ? AND `+generationWhere,
			store.ReviewReactionNone, formatTime(now), projectID, mrIID, noteID, generation); err != nil {
			return nil, fmt.Errorf("sqlite: clear review reaction: %w", err)
		}
		if err := s.enqueueReviewReactionJobTx(ctx, tx, projectID, mrIID, noteID, now); err != nil {
			return nil, err
		}
	}
	return notes, nil
}

func getReviewControlTx(ctx context.Context, tx *sql.Tx, projectID, mrIID int64) (*store.ReviewControl, error) {
	var (
		control    store.ReviewControl
		cancelled  int
		updatedRaw string
	)
	err := tx.QueryRowContext(ctx, `SELECT project_id, mr_iid, generation, head_sha,
		cancelled, candidate_seq, barrier_seq, updated_at
		FROM review_controls WHERE project_id = ? AND mr_iid = ?`,
		projectID, mrIID).Scan(&control.ProjectID, &control.MRIID, &control.Generation,
		&control.HeadSHA, &cancelled, &control.CandidateSeq, &control.BarrierSeq, &updatedRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: review control for project %d MR %d", store.ErrNotFound, projectID, mrIID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get review control: %w", err)
	}
	control.Cancelled = cancelled != 0
	if control.UpdatedAt, err = parseTime(updatedRaw); err != nil {
		return nil, err
	}
	return &control, nil
}

func scanReviewStatus(row rowScanner) (*store.ReviewStatus, error) {
	var (
		status     store.ReviewStatus
		noteID     sql.NullInt64
		updatedRaw string
	)
	if err := row.Scan(&status.ProjectID, &status.MRIID, &status.Generation, &status.HeadSHA,
		&status.Attempt, &status.Profile, &status.State, &status.Detail, &noteID,
		&status.BodyHash, &status.Version, &status.AppliedVersion, &status.LastError,
		&updatedRaw); err != nil {
		return nil, err
	}
	status.NoteID = noteID.Int64
	updated, err := parseTime(updatedRaw)
	if err != nil {
		return nil, err
	}
	status.UpdatedAt = updated
	return &status, nil
}

const reviewStatusColumns = `project_id, mr_iid, generation, head_sha, attempt, profile,
	state, detail, note_id, body_hash, version, applied_version, last_error, updated_at`

func getReviewStatusTx(ctx context.Context, tx *sql.Tx, projectID, mrIID int64) (*store.ReviewStatus, bool, error) {
	status, err := scanReviewStatus(tx.QueryRowContext(ctx,
		`SELECT `+reviewStatusColumns+` FROM review_status WHERE project_id = ? AND mr_iid = ?`,
		projectID, mrIID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: get review status: %w", err)
	}
	return status, true, nil
}

// GetReviewStatus returns the current desired status row.
func (s *Store) GetReviewStatus(ctx context.Context, projectID, mrIID int64) (*store.ReviewStatus, error) {
	if projectID <= 0 || mrIID <= 0 {
		return nil, errors.New("sqlite: review status project id and mr iid are required")
	}
	status, err := scanReviewStatus(s.db.QueryRowContext(ctx,
		`SELECT `+reviewStatusColumns+` FROM review_status WHERE project_id = ? AND mr_iid = ?`,
		projectID, mrIID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: review status for project %d MR %d", store.ErrNotFound, projectID, mrIID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get review status: %w", err)
	}
	return status, nil
}

func scanReviewReaction(row rowScanner) (*store.ReviewReaction, error) {
	var (
		reaction   store.ReviewReaction
		updatedRaw string
	)
	if err := row.Scan(&reaction.ProjectID, &reaction.MRIID, &reaction.NoteID,
		&reaction.Generation, &reaction.Desired, &reaction.Version,
		&reaction.AppliedVersion, &reaction.LastError, &updatedRaw); err != nil {
		return nil, err
	}
	updated, err := parseTime(updatedRaw)
	if err != nil {
		return nil, err
	}
	reaction.UpdatedAt = updated
	return &reaction, nil
}

const reviewReactionColumns = `project_id, mr_iid, note_id, generation, desired,
	version, applied_version, last_error, updated_at`

func getReviewReactionTx(ctx context.Context, tx *sql.Tx, projectID, mrIID, noteID int64) (*store.ReviewReaction, bool, error) {
	reaction, err := scanReviewReaction(tx.QueryRowContext(ctx,
		`SELECT `+reviewReactionColumns+` FROM review_reactions
		 WHERE project_id = ? AND mr_iid = ? AND note_id = ?`, projectID, mrIID, noteID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: get review reaction: %w", err)
	}
	return reaction, true, nil
}

// GetReviewReaction returns the current desired reaction for one note.
func (s *Store) GetReviewReaction(ctx context.Context, projectID, mrIID, noteID int64) (*store.ReviewReaction, error) {
	if projectID <= 0 || mrIID <= 0 || noteID <= 0 {
		return nil, errors.New("sqlite: review reaction project id, mr iid, and note id are required")
	}
	reaction, err := scanReviewReaction(s.db.QueryRowContext(ctx,
		`SELECT `+reviewReactionColumns+` FROM review_reactions
		 WHERE project_id = ? AND mr_iid = ? AND note_id = ?`, projectID, mrIID, noteID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: review reaction for project %d MR %d note %d",
			store.ErrNotFound, projectID, mrIID, noteID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get review reaction: %w", err)
	}
	return reaction, nil
}

// MarkReviewStatusApplied records the outcome for exactly the version the
// writer started from. A newer version means another desired state replaced
// it; the stale outcome must not mark that state applied.
func (s *Store) MarkReviewStatusApplied(ctx context.Context, projectID, mrIID int64, version, noteID int64, bodyHash, lastError string) error {
	if projectID <= 0 || mrIID <= 0 || version <= 0 {
		return errors.New("sqlite: review status project id, mr iid, and version are required")
	}
	lastError = truncateRunes(strings.TrimSpace(lastError), maxReviewStatusLastErrorRunes)
	res, err := s.db.ExecContext(ctx, `UPDATE review_status
		SET applied_version = version,
		    note_id = COALESCE(NULLIF(?, 0), note_id),
		    body_hash = COALESCE(NULLIF(?, ''), body_hash),
		    last_error = ?, updated_at = ?
		WHERE project_id = ? AND mr_iid = ? AND version = ? AND applied_version < ?`,
		noteID, bodyHash, lastError, formatTime(s.now().UTC()),
		projectID, mrIID, version, version)
	if err != nil {
		return fmt.Errorf("sqlite: mark review status applied: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: mark review status applied: %w", err)
	}
	if n == 1 {
		return nil
	}
	return s.reviewStatusWriteMiss(ctx, projectID, mrIID, version)
}

func (s *Store) reviewStatusWriteMiss(ctx context.Context, projectID, mrIID, version int64) error {
	var current int
	err := s.db.QueryRowContext(ctx,
		`SELECT version FROM review_status WHERE project_id = ? AND mr_iid = ?`,
		projectID, mrIID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: review status for project %d MR %d", store.ErrNotFound, projectID, mrIID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: check review status version: %w", err)
	}
	if current != int(version) {
		return fmt.Errorf("%w: review status version %d replaced by %d", store.ErrConflict, version, current)
	}
	// Same version and already applied: an idempotent duplicate.
	return nil
}

// MarkReviewReactionApplied records the outcome for exactly the version the
// writer started from.
func (s *Store) MarkReviewReactionApplied(ctx context.Context, projectID, mrIID, noteID int64, version int64, lastError string) error {
	if projectID <= 0 || mrIID <= 0 || noteID <= 0 || version <= 0 {
		return errors.New("sqlite: review reaction ids and version are required")
	}
	lastError = truncateRunes(strings.TrimSpace(lastError), maxReviewStatusLastErrorRunes)
	res, err := s.db.ExecContext(ctx, `UPDATE review_reactions
		SET applied_version = version, last_error = ?, updated_at = ?
		WHERE project_id = ? AND mr_iid = ? AND note_id = ? AND version = ? AND applied_version < ?`,
		lastError, formatTime(s.now().UTC()), projectID, mrIID, noteID, version, version)
	if err != nil {
		return fmt.Errorf("sqlite: mark review reaction applied: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: mark review reaction applied: %w", err)
	}
	if n == 1 {
		return nil
	}
	var current int
	err = s.db.QueryRowContext(ctx, `SELECT version FROM review_reactions
		WHERE project_id = ? AND mr_iid = ? AND note_id = ?`,
		projectID, mrIID, noteID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: review reaction for project %d MR %d note %d",
			store.ErrNotFound, projectID, mrIID, noteID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: check review reaction version: %w", err)
	}
	if current != int(version) {
		return fmt.Errorf("%w: review reaction version %d replaced by %d", store.ErrConflict, version, current)
	}
	return nil
}

// ListPendingReviewStatuses returns bounded rows whose desired version has not
// been applied.
func (s *Store) ListPendingReviewStatuses(ctx context.Context) ([]store.ReviewStatus, error) {
	return s.listReviewStatuses(ctx, `SELECT `+reviewStatusColumns+` FROM review_status
		WHERE applied_version < version ORDER BY updated_at, project_id, mr_iid LIMIT ?`, maxReviewSideEffectList)
}

// ListReviewStatusesByState returns bounded rows in the given states, oldest
// first, for reconciliation.
func (s *Store) ListReviewStatusesByState(ctx context.Context, states []string, updatedBefore time.Time) ([]store.ReviewStatus, error) {
	if len(states) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(states))
	args := make([]any, 0, len(states)+2)
	for i, state := range states {
		if !validReviewStatusState(state) {
			return nil, fmt.Errorf("sqlite: invalid review status state %q", state)
		}
		placeholders[i] = "?"
		args = append(args, state)
	}
	args = append(args, formatTime(updatedBefore), maxReviewSideEffectList)
	return s.listReviewStatuses(ctx, `SELECT `+reviewStatusColumns+` FROM review_status
		WHERE state IN (`+strings.Join(placeholders, ", ")+`)
		  AND updated_at < ? ORDER BY updated_at, project_id, mr_iid LIMIT ?`, args...)
}

func (s *Store) listReviewStatuses(ctx context.Context, query string, args ...any) ([]store.ReviewStatus, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list review statuses: %w", err)
	}
	defer rows.Close()
	out := make([]store.ReviewStatus, 0)
	for rows.Next() {
		status, err := scanReviewStatus(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan review status: %w", err)
		}
		out = append(out, *status)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate review statuses: %w", err)
	}
	return out, nil
}

// ListPendingReviewReactions returns bounded rows whose desired version has
// not been applied.
func (s *Store) ListPendingReviewReactions(ctx context.Context) ([]store.ReviewReaction, error) {
	return s.listReviewReactions(ctx, `SELECT `+reviewReactionColumns+` FROM review_reactions
		WHERE applied_version < version ORDER BY updated_at, project_id, mr_iid, note_id LIMIT ?`, maxReviewSideEffectList)
}

// ListReviewReactionsByDesired returns bounded rows with one desired state,
// oldest first, for stale-eyes reconciliation.
func (s *Store) ListReviewReactionsByDesired(ctx context.Context, desired string, updatedBefore time.Time) ([]store.ReviewReaction, error) {
	if !validReviewReaction(desired) {
		return nil, fmt.Errorf("sqlite: invalid review reaction %q", desired)
	}
	return s.listReviewReactions(ctx, `SELECT `+reviewReactionColumns+` FROM review_reactions
		WHERE desired = ? AND updated_at < ?
		ORDER BY updated_at, project_id, mr_iid, note_id LIMIT ?`,
		desired, formatTime(updatedBefore), maxReviewSideEffectList)
}

func (s *Store) listReviewReactions(ctx context.Context, query string, args ...any) ([]store.ReviewReaction, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list review reactions: %w", err)
	}
	defer rows.Close()
	out := make([]store.ReviewReaction, 0)
	for rows.Next() {
		reaction, err := scanReviewReaction(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan review reaction: %w", err)
		}
		out = append(out, *reaction)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate review reactions: %w", err)
	}
	return out, nil
}

// HasActiveReviewJob reports whether a run, cancel, or candidate job for the
// MR is queued or running. A failed or completed job is not active.
func (s *Store) HasActiveReviewJob(ctx context.Context, projectID, mrIID int64) (bool, error) {
	if projectID <= 0 || mrIID <= 0 {
		return false, errors.New("sqlite: project id and mr iid are required")
	}
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM jobs
		WHERE "key" IN (?, ?, ?) AND state IN (?, ?) LIMIT 1`,
		"review:"+strconv.FormatInt(projectID, 10)+":"+strconv.FormatInt(mrIID, 10),
		"review.cancel:"+strconv.FormatInt(projectID, 10)+":"+strconv.FormatInt(mrIID, 10),
		"review.candidate:"+strconv.FormatInt(projectID, 10)+":"+strconv.FormatInt(mrIID, 10),
		store.JobQueued, store.JobRunning).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlite: has active review job: %w", err)
	}
	return true, nil
}

// RecentReviewDurations returns bounded completed review durations for one
// project and profile. It is used only for a coarse product estimate.
func (s *Store) RecentReviewDurations(ctx context.Context, projectID int64, profile string, since time.Time, limit int) ([]time.Duration, error) {
	if projectID <= 0 {
		return nil, errors.New("sqlite: project id is required")
	}
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return nil, nil
	}
	if limit <= 0 || limit > maxRecentReviewDurations {
		limit = maxRecentReviewDurations
	}
	rows, err := s.db.QueryContext(ctx, `SELECT started_at, finished_at FROM workflows
		WHERE kind = 'review' AND project_id = ? AND profile = ?
		  AND status = 'completed' AND verdict IN (?, ?, ?)
		  AND started_at IS NOT NULL AND finished_at IS NOT NULL
		  AND finished_at >= ?
		ORDER BY finished_at DESC, id DESC LIMIT ?`,
		projectID, profile, store.ReviewStatusPass, store.ReviewStatusBlocking,
		store.ReviewStatusIncomplete, formatTime(since), limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list recent review durations: %w", err)
	}
	defer rows.Close()
	durations := make([]time.Duration, 0, limit)
	for rows.Next() {
		var startedRaw, finishedRaw string
		if err := rows.Scan(&startedRaw, &finishedRaw); err != nil {
			return nil, fmt.Errorf("sqlite: scan recent review duration: %w", err)
		}
		started, err := parseTime(startedRaw)
		if err != nil {
			return nil, err
		}
		finished, err := parseTime(finishedRaw)
		if err != nil {
			return nil, err
		}
		if finished.After(started) {
			durations = append(durations, finished.Sub(started))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate recent review durations: %w", err)
	}
	return durations, nil
}

func (s *Store) enqueueReviewStatusJobTx(ctx context.Context, tx *sql.Tx, projectID, mrIID int64, now time.Time) error {
	payload, err := json.Marshal(map[string]any{"project_id": projectID, "mr_iid": mrIID})
	if err != nil {
		return fmt.Errorf("sqlite: encode review.status payload: %w", err)
	}
	job := store.Job{
		Kind:        reviewStatusKind,
		Key:         "review.status:" + strconv.FormatInt(projectID, 10) + ":" + strconv.FormatInt(mrIID, 10),
		Payload:     payload,
		MaxAttempts: ReviewSideEffectMaxAttempts,
	}
	if err := s.prepareJob(&job, now); err != nil {
		return err
	}
	if _, _, err := enqueueTx(ctx, tx, &job, true); err != nil {
		return err
	}
	return nil
}

func (s *Store) enqueueReviewReactionJobTx(ctx context.Context, tx *sql.Tx, projectID, mrIID, noteID int64, now time.Time) error {
	payload, err := json.Marshal(map[string]any{
		"project_id": projectID, "mr_iid": mrIID, "note_id": noteID,
	})
	if err != nil {
		return fmt.Errorf("sqlite: encode review.reaction payload: %w", err)
	}
	job := store.Job{
		Kind:        reviewReactionKind,
		Key:         "review.reaction:" + strconv.FormatInt(projectID, 10) + ":" + strconv.FormatInt(mrIID, 10) + ":" + strconv.FormatInt(noteID, 10),
		Payload:     payload,
		MaxAttempts: ReviewSideEffectMaxAttempts,
	}
	if err := s.prepareJob(&job, now); err != nil {
		return err
	}
	if _, _, err := enqueueTx(ctx, tx, &job, true); err != nil {
		return err
	}
	return nil
}

// LatestReviewWorkflow returns the newest review workflow for one MR. Status
// reconciliation uses it to recover the terminal state after a worker crash
// instead of declaring a completed publication failed.
func (s *Store) LatestReviewWorkflow(ctx context.Context, projectID, mrIID int64) (*store.Workflow, error) {
	if projectID <= 0 || mrIID <= 0 {
		return nil, errors.New("sqlite: project id and mr iid are required")
	}
	wf, err := scanWorkflow(s.db.QueryRowContext(ctx, `SELECT `+workflowColumns+` FROM workflows
		WHERE project_id = ? AND mr_iid = ? AND kind = 'review'
		ORDER BY created_at DESC, id DESC LIMIT 1`, projectID, mrIID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: latest review workflow for project %d MR %d", store.ErrNotFound, projectID, mrIID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get latest review workflow: %w", err)
	}
	return wf, nil
}
