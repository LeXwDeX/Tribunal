package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ProtectedContextSHAs returns every base SHA that still backs a context
// revision, the current workspace base, or active and unpublished workflow
// work for one project. Periodic context-directory cleanup must keep all of
// them, including manual and history revisions.
func (s *Store) ProtectedContextSHAs(ctx context.Context, projectID int64) (map[string]struct{}, error) {
	if projectID <= 0 {
		return nil, errors.New("sqlite: project id is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT base_sha FROM workspaces
			WHERE project_id = ? AND base_sha IS NOT NULL AND trim(base_sha) <> ''
		UNION
		SELECT context_sha FROM workspaces
			WHERE project_id = ? AND context_sha IS NOT NULL AND trim(context_sha) <> ''
		UNION
		SELECT base_sha FROM context_docs
			WHERE project_id = ?
		UNION
		SELECT base_sha FROM workflows
			WHERE project_id = ? AND status IN ('pending', 'running')
				AND base_sha IS NOT NULL AND trim(base_sha) <> ''
		UNION
		SELECT w.base_sha FROM workflows w
			JOIN workflow_dossiers d ON d.workflow_id = w.id
			WHERE w.project_id = ? AND d.publication_state = 'pending'
				AND w.base_sha IS NOT NULL AND trim(w.base_sha) <> ''`,
		projectID, projectID, projectID, projectID, projectID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list protected context shas: %w", err)
	}
	defer rows.Close()
	protected := make(map[string]struct{})
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, fmt.Errorf("sqlite: scan protected context sha: %w", err)
		}
		sha = strings.ToLower(strings.TrimSpace(sha))
		if sha == "" {
			continue
		}
		protected[sha] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate protected context shas: %w", err)
	}
	return protected, nil
}
