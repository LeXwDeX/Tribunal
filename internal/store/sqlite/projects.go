package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func projectFilterClauses(filter store.ProjectFilter) ([]string, []any) {
	where := []string{"1 = 1"}
	args := make([]any, 0, 3)
	if query := strings.TrimSpace(filter.Query); query != "" {
		where = append(where,
			"(instr(lower(path_with_namespace), lower(?)) > 0 OR instr(lower(web_url), lower(?)) > 0)")
		args = append(args, query, query)
	}
	if filter.Enabled != nil {
		where = append(where, "enabled = ?")
		if *filter.Enabled {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	if clause, accessArgs := projectAccessClause("projects.id", filter.AccessUserID); clause != "" {
		where = append(where, clause)
		args = append(args, accessArgs...)
	}
	return where, args
}

// sameAccessUserID binds a project list cursor to its access scope. A nil
// filter encodes zero so the cursor cannot be replayed across scopes.
func sameAccessUserID(userID *int64, encoded int64) bool {
	if userID == nil {
		return encoded == 0
	}
	return *userID == encoded
}

// ListProjectsFiltered returns projects matching the admin list selector.
// Query matches the path or web URL case-insensitively.
func (s *Store) ListProjectsFiltered(ctx context.Context, filter store.ProjectFilter) ([]store.Project, error) {
	where, args := projectFilterClauses(filter)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE `+strings.Join(where, " AND ")+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list projects: %w", err)
	}
	defer rows.Close()
	projects := make([]store.Project, 0)
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan project: %w", err)
		}
		projects = append(projects, *project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list projects: %w", err)
	}
	return projects, nil
}

// ListProjectsFilteredPage returns a bounded page ordered by project ID. The
// filter applies before the cursor so pages never mix different selectors.
func (s *Store) ListProjectsFilteredPage(ctx context.Context, filter store.ProjectFilter, cursor string, limit int) ([]store.Project, string, error) {
	limit = normalizeLimit(limit)
	filter.Query = strings.TrimSpace(filter.Query)
	where, args := projectFilterClauses(filter)
	if cursor != "" {
		bound, err := decodeListCursor(cursor, listCursorProjects)
		if err != nil {
			return nil, "", err
		}
		id, err := strconv.ParseInt(bound.ID, 10, 64)
		if err != nil || id <= 0 ||
			bound.Query != filter.Query || !sameBool(bound.Enabled, filter.Enabled) ||
			!sameAccessUserID(filter.AccessUserID, bound.UserID) {
			return nil, "", fmt.Errorf("%w: projects cursor filter", store.ErrInvalidCursor)
		}
		where = append(where, "id > ?")
		args = append(args, id)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE `+strings.Join(where, " AND ")+`
		ORDER BY id LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list projects: %w", err)
	}
	defer rows.Close()
	projects := make([]store.Project, 0, limit)
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, "", fmt.Errorf("sqlite: scan project: %w", err)
		}
		projects = append(projects, *project)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: list projects: %w", err)
	}
	next := ""
	if len(projects) > limit {
		projects = projects[:limit]
		last := projects[len(projects)-1]
		accessUser := int64(0)
		if filter.AccessUserID != nil {
			accessUser = *filter.AccessUserID
		}
		next = encodeListCursor(listCursor{
			Kind: listCursorProjects, Query: filter.Query, Enabled: filter.Enabled,
			UserID: accessUser, ID: strconv.FormatInt(last.ID, 10),
		})
	}
	return projects, next, nil
}

// UpdateProject replaces the provided profile, settings and enabled fields.
func (s *Store) UpdateProject(ctx context.Context, id int64, update store.ProjectUpdate) (*store.Project, error) {
	if id <= 0 {
		return nil, errors.New("sqlite: project id is required")
	}
	sets := make([]string, 0, 4)
	args := make([]any, 0, 5)
	if update.Profile != nil {
		if strings.TrimSpace(*update.Profile) == "" {
			return nil, errors.New("sqlite: project profile is required")
		}
		sets = append(sets, "profile = ?")
		args = append(args, *update.Profile)
	}
	if len(update.Settings) > 0 {
		if !json.Valid(update.Settings) {
			return nil, errors.New("sqlite: project settings is not valid JSON")
		}
		sets = append(sets, "settings = ?")
		args = append(args, string(update.Settings))
	}
	if update.Enabled != nil {
		sets = append(sets, "enabled = ?")
		if *update.Enabled {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	if len(sets) == 0 {
		return nil, errors.New("sqlite: project update has no fields")
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, formatTime(s.now().UTC()), id)
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: update project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("sqlite: update project: %w", err)
	}
	if n != 1 {
		return nil, fmt.Errorf("%w: project %d", store.ErrNotFound, id)
	}
	return s.GetProject(ctx, id)
}

// UpdateProjectToken stores a rotated encrypted access token.
func (s *Store) UpdateProjectToken(ctx context.Context, id int64, update store.ProjectTokenUpdate) error {
	if id <= 0 {
		return errors.New("sqlite: project id is required")
	}
	if len(update.TokenEnc) == 0 {
		return errors.New("sqlite: encrypted token is required")
	}
	scopes := update.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		return fmt.Errorf("sqlite: encode token scopes: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE projects
		SET token_enc = ?, token_expires_at = ?, token_scopes = ?, updated_at = ?
		WHERE id = ?`,
		update.TokenEnc, nullableTime(update.ExpiresAt), string(scopesJSON),
		formatTime(s.now().UTC()), id)
	if err != nil {
		return fmt.Errorf("sqlite: update project token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: update project token: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("%w: project %d", store.ErrNotFound, id)
	}
	return nil
}

// UpdateProjectHook stores the service-created hook ID and its secret.
func (s *Store) UpdateProjectHook(ctx context.Context, id int64, hookID *int64, secretEnc []byte) error {
	if id <= 0 {
		return errors.New("sqlite: project id is required")
	}
	if len(secretEnc) == 0 {
		return errors.New("sqlite: encrypted webhook secret is required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET hook_id = ?, webhook_secret_enc = ?, updated_at = ? WHERE id = ?`,
		nullableInt(hookID), secretEnc, formatTime(s.now().UTC()), id)
	if err != nil {
		return fmt.Errorf("sqlite: update project hook: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: update project hook: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("%w: project %d", store.ErrNotFound, id)
	}
	return nil
}

// UpdateProjectWebhook atomically records provisioning state, sanitized error,
// events, and (when provided) the managed hook ID and secret.
func (s *Store) UpdateProjectWebhook(ctx context.Context, id int64, update store.ProjectWebhookUpdate) error {
	if id <= 0 {
		return errors.New("sqlite: project id is required")
	}
	if update.HookID != nil && update.ClearHookID {
		return errors.New("sqlite: cannot set and clear the webhook hook ID")
	}
	switch update.State {
	case store.WebhookStateDisabled, store.WebhookStatePending,
		store.WebhookStateEnabled, store.WebhookStateFailed, store.WebhookStateUnknown:
	default:
		return errors.New("sqlite: invalid webhook state")
	}
	events := update.Events
	if events == nil {
		events = []string{}
	}
	eventsJSON, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("sqlite: encode webhook events: %w", err)
	}
	now := formatTime(s.now().UTC())
	sets := []string{"webhook_state = ?", "webhook_error = ?", "webhook_events = ?"}
	args := []any{update.State, truncateWebhookError(update.Error), string(eventsJSON)}
	if update.HookID != nil {
		sets = append(sets, "hook_id = ?", "webhook_created_at = COALESCE(webhook_created_at, ?)")
		args = append(args, *update.HookID, now)
	}
	if update.ClearHookID {
		sets = append(sets, "hook_id = NULL")
	}
	if len(update.SecretEnc) > 0 {
		sets = append(sets, "webhook_secret_enc = ?")
		args = append(args, update.SecretEnc)
	}
	if update.ManagedBy != "" {
		sets = append(sets, "webhook_managed_by = ?")
		args = append(args, update.ManagedBy)
	}
	sets = append(sets, "webhook_updated_at = ?", "updated_at = ?")
	args = append(args, now, now, id)
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return fmt.Errorf("sqlite: update project webhook: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: update project webhook: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("%w: project %d", store.ErrNotFound, id)
	}
	return nil
}

// truncateWebhookError bounds stored diagnostics; callers already sanitize the
// text so it never contains an upstream body or credential.
func truncateWebhookError(text string) string {
	const limit = 512
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

// DeleteProject removes one project and the rows outside database cascades.
// Foreign keys cover workspaces, context docs, workflows, nodes and findings;
// the statements below cover block bindings, deliveries, review state and
// observability rows that have no foreign key.
func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("sqlite: project id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin delete project: %w", err)
	}
	defer tx.Rollback()

	var found int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, id).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: project %d", store.ErrNotFound, id)
		}
		return fmt.Errorf("sqlite: find project: %w", err)
	}

	const workflowScope = `SELECT id FROM workflows WHERE project_id = ?`
	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM feedback WHERE project_id = ?`, []any{id}},
		{`DELETE FROM publications WHERE project_id = ?`, []any{id}},
		{`DELETE FROM webhook_deliveries WHERE project_id = ?`, []any{id}},
		{`DELETE FROM block_bindings WHERE project_id = ?`, []any{id}},
		{`DELETE FROM review_controls WHERE project_id = ?`, []any{id}},
		{`DELETE FROM review_command_replays WHERE project_id = ?`, []any{id}},
		{`DELETE FROM review_command_quarantine WHERE project_id = ?`, []any{id}},
		{`DELETE FROM review_status WHERE project_id = ?`, []any{id}},
		{`DELETE FROM review_reactions WHERE project_id = ?`, []any{id}},
		{`DELETE FROM llm_calls WHERE workflow_id IN (` + workflowScope + `)`, []any{id}},
		{`DELETE FROM events WHERE workflow_id IN (` + workflowScope + `)`, []any{id}},
		{`DELETE FROM workflow_dossiers WHERE project_id = ? OR workflow_id IN (` + workflowScope + `)`, []any{id, id}},
		{`DELETE FROM jobs WHERE "key" IN (
			SELECT 'review.publish:' || id FROM workflows WHERE project_id = ?
		)`, []any{id}},
		{`DELETE FROM jobs
		  WHERE "key" = ? OR "key" LIKE ? OR "key" LIKE ? OR "key" LIKE ?
		     OR "key" = ? OR "key" LIKE ?
		     OR "key" LIKE ? OR "key" LIKE ? OR "key" LIKE ?`,
			[]any{fmt.Sprintf("workspace.onboard:%d", id),
				fmt.Sprintf("review:%d:%%", id), fmt.Sprintf("review.cancel:%d:%%", id),
				fmt.Sprintf("review.command:%d:%%", id),
				fmt.Sprintf("ws:%d", id), fmt.Sprintf("ws:%d:%%", id),
				fmt.Sprintf("review.candidate:%d:%%", id),
				fmt.Sprintf("review.status:%d:%%", id),
				fmt.Sprintf("review.reaction:%d:%%", id)}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return fmt.Errorf("sqlite: delete project data: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id); err != nil {
		return fmt.Errorf("sqlite: delete project: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit delete project: %w", err)
	}
	return nil
}

// RewrapProjectSecrets replaces both encrypted columns only while they still
// hold the expected values, so a concurrent token rotation or webhook rebuild
// is never overwritten. It reports whether the row changed. The ciphertext
// format is a storage detail, so updated_at is left alone.
func (s *Store) RewrapProjectSecrets(ctx context.Context, id int64, oldToken, newToken, oldSecret, newSecret []byte) (bool, error) {
	if len(newToken) == 0 {
		return false, errors.New("sqlite: encrypted token is required")
	}
	if newSecret == nil {
		newSecret = []byte{}
	}
	if oldSecret == nil {
		oldSecret = []byte{}
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET token_enc = ?, webhook_secret_enc = ?
		 WHERE id = ? AND token_enc = ? AND webhook_secret_enc = ?`,
		newToken, newSecret, id, oldToken, oldSecret)
	if err != nil {
		return false, fmt.Errorf("sqlite: rewrap project secrets: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlite: rewrap project secrets: %w", err)
	}
	return n == 1, nil
}
