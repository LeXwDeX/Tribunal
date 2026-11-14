package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// The collector and capacity rows are singletons created by migration 0033.
const (
	feedbackCollectionSingleton = 1
	workspaceCapacitySingleton  = 1
)

// FeedbackCollectionState returns the latest collector health. It is only nil
// when the singleton row is missing from a broken database.
func (s *Store) FeedbackCollectionState(ctx context.Context) (*store.FeedbackCollectionState, error) {
	var (
		collected   sql.NullString
		errorClass  string
		lastErrorAt sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `SELECT last_collected_at, last_error_class, last_error_at
		FROM feedback_collection_state WHERE id = ?`, feedbackCollectionSingleton).
		Scan(&collected, &errorClass, &lastErrorAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: read feedback collection state: %w", err)
	}
	state := &store.FeedbackCollectionState{LastErrorClass: errorClass}
	if collected.Valid && collected.String != "" {
		value, err := parseTime(collected.String)
		if err != nil {
			return nil, err
		}
		state.LastCollectedAt = &value
	}
	if lastErrorAt.Valid && lastErrorAt.String != "" {
		value, err := parseTime(lastErrorAt.String)
		if err != nil {
			return nil, err
		}
		state.LastErrorAt = &value
	}
	return state, nil
}

// RecordFeedbackCollection updates collector health. A success advances
// last_collected_at but deliberately keeps the most recent error visible: a
// LastCollectedAt after LastErrorAt shows recovery, while a stale collection
// clock shows a collector that keeps failing.
func (s *Store) RecordFeedbackCollection(ctx context.Context, at time.Time, errorClass string) error {
	if at.IsZero() {
		return errors.New("sqlite: feedback collection time is required")
	}
	if errorClass == "" {
		_, err := s.db.ExecContext(ctx, `UPDATE feedback_collection_state
			SET last_collected_at = ? WHERE id = ?`,
			formatTime(at.UTC()), feedbackCollectionSingleton)
		if err != nil {
			return fmt.Errorf("sqlite: record feedback collection: %w", err)
		}
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE feedback_collection_state
		SET last_error_class = ?, last_error_at = ? WHERE id = ?`,
		errorClass, formatTime(at.UTC()), feedbackCollectionSingleton)
	if err != nil {
		return fmt.Errorf("sqlite: record feedback collection error: %w", err)
	}
	return nil
}

// WorkspaceCapacity returns the latest measured workspace root usage.
func (s *Store) WorkspaceCapacity(ctx context.Context) (*store.WorkspaceCapacity, error) {
	var (
		measured  sql.NullString
		total     int64
		limit     int64
		overLimit int
	)
	err := s.db.QueryRowContext(ctx, `SELECT measured_at, total_bytes, limit_bytes, over_limit
		FROM workspace_capacity WHERE id = ?`, workspaceCapacitySingleton).
		Scan(&measured, &total, &limit, &overLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: read workspace capacity: %w", err)
	}
	capacity := &store.WorkspaceCapacity{
		TotalBytes: total,
		LimitBytes: limit,
		OverLimit:  overLimit != 0,
	}
	if measured.Valid && measured.String != "" {
		value, err := parseTime(measured.String)
		if err != nil {
			return nil, err
		}
		capacity.MeasuredAt = value
	}
	return capacity, nil
}

// RecordWorkspaceCapacity stores one measurement. It is a single-row upsert so
// a concurrent cleanup pass cannot leave a partial update.
func (s *Store) RecordWorkspaceCapacity(ctx context.Context, capacity store.WorkspaceCapacity) error {
	if capacity.MeasuredAt.IsZero() {
		return errors.New("sqlite: workspace capacity measurement time is required")
	}
	overLimit := 0
	if capacity.OverLimit {
		overLimit = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspace_capacity
		(id, measured_at, total_bytes, limit_bytes, over_limit)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			measured_at = excluded.measured_at,
			total_bytes = excluded.total_bytes,
			limit_bytes = excluded.limit_bytes,
			over_limit = excluded.over_limit`,
		workspaceCapacitySingleton, formatTime(capacity.MeasuredAt.UTC()),
		capacity.TotalBytes, capacity.LimitBytes, overLimit)
	if err != nil {
		return fmt.Errorf("sqlite: record workspace capacity: %w", err)
	}
	return nil
}

// WorkspaceReclaimStatus returns the fail-closed evidence for reclaiming an
// inactive project workspace. Any unfinished workflow, queued or running
// project job, pending publication dossier, or non-terminal review status marks
// the project protected. A project with no activity evidence is protected too:
// missing metadata must never authorize a delete.
func (s *Store) WorkspaceReclaimStatus(ctx context.Context, projectID int64) (store.WorkspaceReclaimStatus, error) {
	if projectID <= 0 {
		return store.WorkspaceReclaimStatus{}, errors.New("sqlite: project id is required")
	}
	var (
		activeWorkflows   int
		pendingDossiers   int
		activeStatuses    int
		activeProjectJobs int
		timestamps        [8]string
	)
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM workflows WHERE project_id = ? AND status IN ('pending', 'running')),
		(SELECT COUNT(*) FROM workflow_dossiers WHERE project_id = ? AND publication_state = 'pending'),
		(SELECT COUNT(*) FROM review_status WHERE project_id = ? AND state IN ('queued', 'running', 'publishing')),
		(SELECT COUNT(*) FROM jobs j WHERE j.state IN ('queued', 'running') AND (
			j.id IN (SELECT id FROM workflows WHERE project_id = ?)
			OR j.id IN (SELECT workflow_id FROM workflow_dossiers WHERE project_id = ? AND workflow_id IS NOT NULL)
			OR j."key" = 'workspace.onboard:' || ?
			OR j."key" = 'workspace.onboard:' || ? || ':retry'
			OR j."key" = 'ws:' || ?
			OR j."key" LIKE 'review%:' || ? || ':%'
			OR j."key" LIKE 'ws:' || ? || ':%'
		)),
		COALESCE((SELECT MAX(created_at) FROM workflows WHERE project_id = ?), ''),
		COALESCE((SELECT MAX(COALESCE(finished_at, started_at, created_at)) FROM workflows WHERE project_id = ?), ''),
		COALESCE((SELECT MAX(updated_at) FROM workflow_dossiers WHERE project_id = ?), ''),
		COALESCE((SELECT MAX(updated_at) FROM review_status WHERE project_id = ?), ''),
		COALESCE((SELECT MAX(observed_at) FROM feedback WHERE project_id = ?), ''),
		COALESCE((SELECT MAX(last_refresh_at) FROM workspaces WHERE project_id = ?), ''),
		COALESCE((SELECT MAX(updated_at) FROM workspaces WHERE project_id = ?), ''),
		COALESCE((SELECT MAX(updated_at) FROM projects WHERE id = ?), '')`,
		projectID, projectID, projectID, projectID, projectID, projectID, projectID, projectID,
		projectID, projectID, projectID, projectID, projectID, projectID, projectID, projectID,
		projectID, projectID).
		Scan(&activeWorkflows, &pendingDossiers, &activeStatuses, &activeProjectJobs,
			&timestamps[0], &timestamps[1], &timestamps[2], &timestamps[3],
			&timestamps[4], &timestamps[5], &timestamps[6], &timestamps[7])
	if err != nil {
		return store.WorkspaceReclaimStatus{}, fmt.Errorf("sqlite: workspace reclaim status: %w", err)
	}
	status := store.WorkspaceReclaimStatus{
		Protected: activeWorkflows > 0 || pendingDossiers > 0 ||
			activeStatuses > 0 || activeProjectJobs > 0,
	}
	for _, raw := range timestamps {
		if raw == "" {
			continue
		}
		value, err := parseTime(raw)
		if err != nil {
			return store.WorkspaceReclaimStatus{}, err
		}
		if value.After(status.LastActivity) {
			status.LastActivity = value
		}
	}
	if status.LastActivity.IsZero() {
		status.Protected = true
	}
	return status, nil
}

// ProtectedMergeRequestIIDs returns merge requests whose review has not reached
// a terminal state. Their fetch refs stay in the mirror during cleanup.
func (s *Store) ProtectedMergeRequestIIDs(ctx context.Context, projectID int64) (map[int64]struct{}, error) {
	if projectID <= 0 {
		return nil, errors.New("sqlite: project id is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT mr_iid FROM review_status
			WHERE project_id = ? AND state IN ('queued', 'running', 'publishing')
		UNION
		SELECT mr_iid FROM workflows
			WHERE project_id = ? AND status IN ('pending', 'running') AND mr_iid IS NOT NULL`,
		projectID, projectID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: protected merge requests: %w", err)
	}
	defer rows.Close()
	protected := make(map[int64]struct{})
	for rows.Next() {
		var iid int64
		if err := rows.Scan(&iid); err != nil {
			return nil, fmt.Errorf("sqlite: scan protected merge request: %w", err)
		}
		protected[iid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate protected merge requests: %w", err)
	}
	return protected, nil
}
