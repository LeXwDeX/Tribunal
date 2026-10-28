package sqlite

import (
	"context"
	"fmt"
)

// ListWorkspaceProjectIDs returns the projects that have a registered
// workspace row. The workspaces table is the authority for periodic run
// cleanup: a leftover directory under the configured workspace root without a
// row is never traversed or deleted.
func (s *Store) ListWorkspaceProjectIDs(ctx context.Context) ([]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT project_id FROM workspaces ORDER BY project_id`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list workspace projects: %w", err)
	}
	defer rows.Close()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("sqlite: scan workspace project: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate workspace projects: %w", err)
	}
	return ids, nil
}

// SettleOrphanDossiers releases reservations that cannot be retried by a job.
// Missing jobs are safe here: retention only removes terminal jobs, and live
// workers require a queued or running job row. Keep the dossier for audit.
func (s *Store) SettleOrphanDossiers(ctx context.Context, projectID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE workflow_dossiers
		SET publication_state = 'skipped', publication_error = 'review job no longer active', updated_at = ?
		WHERE project_id = ? AND project_id > 0 AND publication_state = 'pending'
		  AND job_id IS NOT NULL AND job_id != ''
		  AND NOT EXISTS (SELECT 1 FROM workflows w WHERE w.id = workflow_dossiers.workflow_id)
		  AND (NOT EXISTS (SELECT 1 FROM jobs j WHERE j.id = workflow_dossiers.job_id)
		       OR EXISTS (SELECT 1 FROM jobs j WHERE j.id = workflow_dossiers.job_id AND j.state IN ('done', 'failed', 'cancelled')))`,
		formatTime(s.now().UTC()), projectID)
	if err != nil {
		return fmt.Errorf("sqlite: settle orphan dossiers: %w", err)
	}
	return nil
}

// ProtectedRunRefs returns every run directory name that periodic cleanup
// must keep: unfinished workflows, queued or running jobs, and dossiers whose
// publication is still pending (both workflow and job IDs, since either can
// name the runs/ directory). The query is global; over-protecting a name in
// another project is safe.
func (s *Store) ProtectedRunRefs(ctx context.Context) (map[string]struct{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM workflows
			WHERE status IN ('pending', 'running')
		UNION
		SELECT id FROM jobs
			WHERE state IN ('queued', 'running')
		UNION
		SELECT workflow_id FROM workflow_dossiers
			WHERE publication_state = 'pending'
		UNION
		SELECT job_id FROM workflow_dossiers
			WHERE publication_state = 'pending' AND job_id IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list protected runs: %w", err)
	}
	defer rows.Close()
	protected := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("sqlite: scan protected run: %w", err)
		}
		protected[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate protected runs: %w", err)
	}
	return protected, nil
}
