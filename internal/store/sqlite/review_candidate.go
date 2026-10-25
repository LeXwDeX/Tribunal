package sqlite

import (
	"bytes"
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

// RecordReviewCandidateDelivery records one deferred review webhook. The
// delivery, candidate sequence allocation, and queue write share a transaction.
// A replayed (project_id, uuid) returns the original job ID and allocates no
// new sequence.
func (s *Store) RecordReviewCandidateDelivery(ctx context.Context, d store.Delivery, candidate *store.ReviewCandidate) (store.ReviewEnqueueResult, error) {
	if d.UUID == "" {
		return store.ReviewEnqueueResult{}, errors.New("sqlite: delivery uuid is required")
	}
	if d.ProjectID <= 0 {
		return store.ReviewEnqueueResult{}, errors.New("sqlite: delivery project id is required")
	}
	if d.Event == "" {
		return store.ReviewEnqueueResult{}, errors.New("sqlite: delivery event is required")
	}
	if d.Outcome != store.DeliveryEnqueued {
		return store.ReviewEnqueueResult{}, errors.New("sqlite: candidate delivery must be enqueued")
	}
	if candidate == nil {
		return store.ReviewEnqueueResult{}, errors.New("sqlite: enqueued candidate delivery requires a candidate")
	}
	if candidate.ProjectID != d.ProjectID {
		return store.ReviewEnqueueResult{}, errors.New("sqlite: delivery and review candidate project ids differ")
	}

	now := s.now().UTC()
	if d.ReceivedAt.IsZero() {
		d.ReceivedAt = now
	} else {
		d.ReceivedAt = d.ReceivedAt.UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: begin review candidate delivery: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `INSERT INTO webhook_deliveries
		(project_id, uuid, event, outcome, reason, job_id, received_at)
		VALUES (?, ?, ?, ?, ?, NULL, ?)
		ON CONFLICT (project_id, uuid) DO NOTHING`,
		d.ProjectID, d.UUID, d.Event, d.Outcome, nullString(d.Reason), formatTime(d.ReceivedAt))
	if err != nil {
		return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: insert delivery: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: insert delivery: %w", err)
	}
	if inserted == 0 {
		var existing sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT job_id FROM webhook_deliveries
			WHERE project_id = ? AND uuid = ?`, d.ProjectID, d.UUID).Scan(&existing); err != nil {
			return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: read delivery: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: commit delivery: %w", err)
		}
		return store.ReviewEnqueueResult{JobID: existing.String, Duplicate: true}, nil
	}

	if err := requireProjectTx(ctx, tx, candidate.ProjectID); err != nil {
		return store.ReviewEnqueueResult{}, err
	}
	head, err := normalizeReviewHead(candidate.HeadSHA, false)
	if err != nil {
		return store.ReviewEnqueueResult{}, err
	}
	sequence, err := allocateCandidateTx(ctx, tx, candidate, now)
	if err != nil {
		return store.ReviewEnqueueResult{}, err
	}
	payload, err := reviewCandidatePayload(candidate, head, sequence)
	if err != nil {
		return store.ReviewEnqueueResult{}, err
	}
	maxAttempts := candidate.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = defaultReviewMaxAttempts
	}
	job := store.Job{
		Kind:        reviewCandidateKind,
		Key:         reviewCandidateKey(candidate.ProjectID, candidate.MRIID),
		Payload:     payload,
		MaxAttempts: maxAttempts,
	}
	if err := s.prepareJob(&job, now); err != nil {
		return store.ReviewEnqueueResult{}, err
	}
	jobID, _, err := enqueueTx(ctx, tx, &job, true)
	if err != nil {
		return store.ReviewEnqueueResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE webhook_deliveries SET job_id = ?
		WHERE project_id = ? AND uuid = ?`, jobID, d.ProjectID, d.UUID); err != nil {
		return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: attach delivery job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: commit review candidate delivery: %w", err)
	}
	return store.ReviewEnqueueResult{JobID: jobID}, nil
}

// allocateCandidateTx advances only the candidate high-water mark. It creates a
// generation 0 control row when none exists and never changes the accepted
// generation, head, cancelled flag, or barrier.
func allocateCandidateTx(ctx context.Context, tx *sql.Tx, candidate *store.ReviewCandidate, now time.Time) (int64, error) {
	var sequence int64
	err := tx.QueryRowContext(ctx, `INSERT INTO review_controls
		(project_id, mr_iid, generation, head_sha, cancelled, candidate_seq, barrier_seq, updated_at)
		VALUES (?, ?, 0, '', 0, 1, 0, ?)
		ON CONFLICT (project_id, mr_iid) DO UPDATE SET
			candidate_seq = review_controls.candidate_seq + 1,
			updated_at = excluded.updated_at
		RETURNING candidate_seq`,
		candidate.ProjectID, candidate.MRIID, formatTime(now)).Scan(&sequence)
	if err != nil {
		return 0, fmt.Errorf("sqlite: allocate review candidate: %w", err)
	}
	return sequence, nil
}

func reviewCandidatePayload(candidate *store.ReviewCandidate, head string, sequence int64) (json.RawMessage, error) {
	fields := make(map[string]any)
	if raw := bytes.TrimSpace(candidate.Payload); len(raw) > 0 {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("sqlite: review candidate payload must be a JSON object: %w", err)
		}
		if fields == nil {
			fields = make(map[string]any)
		}
	}
	for _, reserved := range []string{"generation", "candidate_seq", "activation_seq"} {
		if _, ok := fields[reserved]; ok {
			return nil, fmt.Errorf("sqlite: review candidate payload must not contain %s", reserved)
		}
	}
	fields["project_id"] = candidate.ProjectID
	fields["mr_iid"] = candidate.MRIID
	fields["head_sha"] = head
	fields["candidate_seq"] = sequence
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("sqlite: encode review candidate payload: %w", err)
	}
	return encoded, nil
}

func reviewCandidateKey(projectID, mrIID int64) string {
	return "review.candidate:" + strconv.FormatInt(projectID, 10) + ":" + strconv.FormatInt(mrIID, 10)
}

// ActivateReviewCandidate verifies the claimed job lease and payload, then
// consumes the candidate and enqueues review.run atomically. A candidate at or
// below barrier_seq is already consumed or invalidated and returns
// ErrSuperseded without a generation bump.
func (s *Store) ActivateReviewCandidate(ctx context.Context, activation store.ReviewCandidateActivation) (string, int64, error) {
	if strings.TrimSpace(activation.JobID) == "" || strings.TrimSpace(activation.LeaseOwner) == "" {
		return "", 0, errors.New("sqlite: candidate job id and lease owner are required")
	}
	if activation.Attempts <= 0 {
		return "", 0, errors.New("sqlite: candidate job attempt is required")
	}
	if activation.ProjectID <= 0 || activation.MRIID <= 0 || activation.CandidateSeq <= 0 {
		return "", 0, errors.New("sqlite: candidate project, MR, and sequence are required")
	}
	head, err := normalizeReviewHead(activation.HeadSHA, false)
	if err != nil {
		return "", 0, err
	}

	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, fmt.Errorf("sqlite: begin candidate activation: %w", err)
	}
	defer tx.Rollback()

	var raw string
	err = tx.QueryRowContext(ctx, `SELECT payload FROM jobs
		WHERE id = ? AND kind = ? AND state = ? AND lease_owner = ? AND attempts = ?
		  AND lease_until IS NOT NULL AND lease_until >= ?`,
		activation.JobID, reviewCandidateKind, store.JobRunning, activation.LeaseOwner,
		activation.Attempts, formatTime(now)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, store.ErrLeaseLost
	}
	if err != nil {
		return "", 0, fmt.Errorf("sqlite: read candidate job: %w", err)
	}
	var rowPayload struct {
		ProjectID    int64  `json:"project_id"`
		MRIID        int64  `json:"mr_iid"`
		HeadSHA      string `json:"head_sha"`
		CandidateSeq int64  `json:"candidate_seq"`
	}
	if err := json.Unmarshal([]byte(raw), &rowPayload); err != nil {
		return "", 0, fmt.Errorf("sqlite: decode candidate job payload: %w", err)
	}
	if rowPayload.ProjectID != activation.ProjectID || rowPayload.MRIID != activation.MRIID ||
		rowPayload.CandidateSeq != activation.CandidateSeq ||
		!strings.EqualFold(strings.TrimSpace(rowPayload.HeadSHA), head) {
		return "", 0, errors.New("sqlite: candidate job payload does not match the activation request")
	}

	var generation int64
	err = tx.QueryRowContext(ctx, `UPDATE review_controls
		SET generation = generation + 1,
		    head_sha = ?,
		    cancelled = 0,
		    barrier_seq = ?,
		    updated_at = ?
		WHERE project_id = ? AND mr_iid = ?
		  AND barrier_seq < ?
		  AND candidate_seq >= ?
		RETURNING generation`,
		head, activation.CandidateSeq, formatTime(now),
		activation.ProjectID, activation.MRIID,
		activation.CandidateSeq, activation.CandidateSeq).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		var one int
		lookupErr := tx.QueryRowContext(ctx, `SELECT 1 FROM review_controls
			WHERE project_id = ? AND mr_iid = ?`, activation.ProjectID, activation.MRIID).Scan(&one)
		switch {
		case errors.Is(lookupErr, sql.ErrNoRows):
			return "", 0, fmt.Errorf("%w: review control for project %d MR %d",
				store.ErrNotFound, activation.ProjectID, activation.MRIID)
		case lookupErr != nil:
			return "", 0, fmt.Errorf("sqlite: check review control: %w", lookupErr)
		default:
			return "", 0, store.ErrSuperseded
		}
	}
	if err != nil {
		return "", 0, fmt.Errorf("sqlite: activate review candidate: %w", err)
	}

	var sideEffects struct {
		Profile string `json:"profile"`
	}
	if err := json.Unmarshal([]byte(raw), &sideEffects); err != nil {
		return "", 0, fmt.Errorf("sqlite: decode candidate side effects: %w", err)
	}
	if _, err := s.setReviewStatusTx(ctx, tx, store.ReviewStatusGoal{
		ProjectID:  activation.ProjectID,
		MRIID:      activation.MRIID,
		Generation: generation,
		HeadSHA:    head,
		Profile:    sideEffects.Profile,
		State:      store.ReviewStatusQueued,
	}, now); err != nil {
		return "", 0, err
	}
	if _, err := s.clearReviewReactionsTx(ctx, tx, activation.ProjectID, activation.MRIID, generation, true, now); err != nil {
		return "", 0, err
	}

	fields := make(map[string]any)
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return "", 0, fmt.Errorf("sqlite: decode candidate job payload: %w", err)
	}
	delete(fields, "candidate_seq")
	fields["activation_seq"] = activation.CandidateSeq
	payload, err := json.Marshal(fields)
	if err != nil {
		return "", 0, fmt.Errorf("sqlite: encode review payload: %w", err)
	}
	command := store.ReviewCommand{
		ProjectID:   activation.ProjectID,
		MRIID:       activation.MRIID,
		HeadSHA:     head,
		Payload:     payload,
		MaxAttempts: defaultReviewMaxAttempts,
	}
	runPayload, err := reviewJobPayload(command, head, generation)
	if err != nil {
		return "", 0, err
	}
	job := store.Job{
		Kind:        reviewRunKind,
		Key:         "review:" + strconv.FormatInt(activation.ProjectID, 10) + ":" + strconv.FormatInt(activation.MRIID, 10),
		Payload:     runPayload,
		MaxAttempts: command.MaxAttempts,
	}
	if err := s.prepareJob(&job, now); err != nil {
		return "", 0, err
	}
	jobID, _, err := enqueueTx(ctx, tx, &job, true)
	if err != nil {
		return "", 0, err
	}
	if err := tx.Commit(); err != nil {
		return "", 0, fmt.Errorf("sqlite: commit candidate activation: %w", err)
	}
	return jobID, generation, nil
}
