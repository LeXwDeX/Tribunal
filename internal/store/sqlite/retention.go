package sqlite

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// retentionBatch bounds one DELETE statement so a large cleanup never holds
// the single writer lock for the whole table.
const retentionBatch = 500

// The batch statements are shared with the plan tests so a query rewrite
// cannot silently drift away from the retention indexes.
const (
	deleteLLMCallsBatch = `DELETE FROM llm_calls
			WHERE id IN (
				SELECT id FROM llm_calls WHERE created_at < ?
				ORDER BY created_at, id LIMIT ?
			)`
	deleteDeliveriesBatch = `DELETE FROM webhook_deliveries
			WHERE rowid IN (
				SELECT rowid FROM webhook_deliveries WHERE received_at < ?
				ORDER BY received_at, rowid LIMIT ?
			)`
	// Without ANALYZE the planner prefers jobs_claim and sorts every terminal
	// row per batch. INDEXED BY pins the partial index whose order matches the
	// bounded DELETE; 0005 creates it before this store serves requests.
	deleteTerminalJobsBatch = `DELETE FROM jobs
			WHERE id IN (
				SELECT id FROM jobs INDEXED BY jobs_terminal_updated
				WHERE state IN ('done', 'failed', 'cancelled') AND updated_at < ?
				ORDER BY updated_at, id LIMIT ?
			)`
)

// DeleteLLMCallsBefore deletes whole audit rows older than the cutoff. Expired
// rows are removed in bounded batches until none remain.
func (s *Store) DeleteLLMCallsBefore(ctx context.Context, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, errors.New("sqlite: retention cutoff is required")
	}
	cutoff := formatTime(before.UTC())
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, deleteLLMCallsBatch, cutoff, retentionBatch)
		if err != nil {
			return total, fmt.Errorf("sqlite: delete llm calls: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("sqlite: delete llm calls: %w", err)
		}
		total += n
		if n < retentionBatch {
			return total, nil
		}
	}
}

// DeleteDeliveriesBefore deletes webhook delivery records received before the
// cutoff in bounded batches.
func (s *Store) DeleteDeliveriesBefore(ctx context.Context, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, errors.New("sqlite: retention cutoff is required")
	}
	cutoff := formatTime(before.UTC())
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, deleteDeliveriesBatch, cutoff, retentionBatch)
		if err != nil {
			return total, fmt.Errorf("sqlite: delete webhook deliveries: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("sqlite: delete webhook deliveries: %w", err)
		}
		total += n
		if n < retentionBatch {
			return total, nil
		}
	}
}

// DeleteTerminalJobsBefore deletes done, failed, and cancelled jobs last
// updated before the cutoff. Queued and running jobs are never deleted.
func (s *Store) DeleteTerminalJobsBefore(ctx context.Context, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, errors.New("sqlite: retention cutoff is required")
	}
	cutoff := formatTime(before.UTC())
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, deleteTerminalJobsBatch, cutoff, retentionBatch)
		if err != nil {
			return total, fmt.Errorf("sqlite: delete terminal jobs: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("sqlite: delete terminal jobs: %w", err)
		}
		total += n
		if n < retentionBatch {
			return total, nil
		}
	}
}

// PruneContextDocs keeps the newest keep non-active, unlocked context versions
// for each (project_id, kind) pair and deletes older ones in bounded batches.
// Locked rows and the active revision are never deleted and do not consume a
// retained slot. Several revisions of one base are pruned like any other
// history; only the active revision is protected.
func (s *Store) PruneContextDocs(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		return 0, errors.New("sqlite: context versions to keep must be positive")
	}
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, `DELETE FROM context_docs
			WHERE locked = 0 AND active = 0 AND id IN (
				SELECT id FROM (
					SELECT id, project_id, kind, ROW_NUMBER() OVER (
						PARTITION BY project_id, kind
						ORDER BY created_at DESC, rowid DESC
					) AS rn
					FROM context_docs
					WHERE locked = 0 AND active = 0
				) AS ranked
				WHERE ranked.rn > ?
				ORDER BY ranked.rn DESC, ranked.id
				LIMIT ?
			)`, keep, retentionBatch)
		if err != nil {
			return total, fmt.Errorf("sqlite: prune context documents: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("sqlite: prune context documents: %w", err)
		}
		total += n
		if n < retentionBatch {
			return total, nil
		}
	}
}
