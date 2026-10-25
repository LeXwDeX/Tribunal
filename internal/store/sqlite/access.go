package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

const (
	listCursorUsers  = "users"
	listCursorGrants = "grants"
)

// projectAccessClause returns a SQL predicate that restricts column to rows
// owned by projects granted to userID. A nil user keeps the trusted internal
// unrestricted path; a non-positive value matches no row. column is always a
// caller-supplied identifier, never user input.
func projectAccessClause(column string, userID *int64) (string, []any) {
	if userID == nil {
		return "", nil
	}
	if *userID <= 0 {
		return "0 = 1", nil
	}
	return `EXISTS (SELECT 1 FROM project_grants pg
		JOIN users pu ON pu.id = pg.user_id
		WHERE pg.project_id = ` + column + ` AND pg.user_id = ? AND pu.disabled = 0)`, []any{*userID}
}

func scanUser(row rowScanner) (*store.User, error) {
	var (
		user        store.User
		disabled    int
		lastLoginAt sql.NullString
	)
	if err := row.Scan(&user.ID, &user.Username, &user.Name, &disabled, &lastLoginAt); err != nil {
		return nil, err
	}
	user.Disabled = disabled != 0
	if lastLoginAt.Valid {
		parsed, err := parseTime(lastLoginAt.String)
		if err != nil {
			return nil, err
		}
		user.LastLoginAt = &parsed
	}
	return &user, nil
}

// GetUser returns one locally known user.
func (s *Store) GetUser(ctx context.Context, userID int64) (*store.User, error) {
	if userID <= 0 {
		return nil, errors.New("sqlite: user id is required")
	}
	user, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT id, username, name, disabled, last_login_at FROM users WHERE id = ?`, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: user %d", store.ErrNotFound, userID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get user: %w", err)
	}
	return user, nil
}

// ListUsers returns users ordered by ID. The opaque cursor continues after the
// last returned user.
func (s *Store) ListUsers(ctx context.Context, cursor string, limit int) ([]store.User, string, error) {
	limit = normalizeLimit(limit)
	where := []string{"1 = 1"}
	args := make([]any, 0, 1)
	if cursor != "" {
		bound, err := decodeListCursor(cursor, listCursorUsers)
		if err != nil {
			return nil, "", err
		}
		if bound.UserID <= 0 {
			return nil, "", fmt.Errorf("%w: users cursor", store.ErrInvalidCursor)
		}
		where = append(where, "id > ?")
		args = append(args, bound.UserID)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, name, disabled, last_login_at FROM users
		 WHERE `+strings.Join(where, " AND ")+` ORDER BY id LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list users: %w", err)
	}
	defer rows.Close()
	users := make([]store.User, 0, limit)
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, "", fmt.Errorf("sqlite: scan user: %w", err)
		}
		users = append(users, *user)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: iterate users: %w", err)
	}
	next := ""
	if len(users) > limit {
		users = users[:limit]
		next = encodeListCursor(listCursor{Kind: listCursorUsers, UserID: users[len(users)-1].ID})
	}
	return users, next, nil
}

// SetUserDisabled updates the disabled flag and, when disabling, deletes all
// sessions and API tokens in the same transaction. Re-enabling never restores
// revoked credentials. A disabled user cannot receive new credentials because
// issuance checks this flag inside its own write transaction.
func (s *Store) SetUserDisabled(ctx context.Context, userID int64, disabled bool) error {
	if userID <= 0 {
		return errors.New("sqlite: user id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin set user disabled: %w", err)
	}
	defer tx.Rollback()

	var current int
	if err := tx.QueryRowContext(ctx,
		`SELECT disabled FROM users WHERE id = ?`, userID).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: user %d", store.ErrNotFound, userID)
		}
		return fmt.Errorf("sqlite: check user: %w", err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE users SET disabled = ? WHERE id = ?`,
		boolInt(disabled), userID)
	if err != nil {
		return fmt.Errorf("sqlite: set user disabled: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sqlite: set user disabled: %w", err)
	} else if n != 1 {
		return fmt.Errorf("%w: user %d", store.ErrNotFound, userID)
	}
	if disabled {
		if err := revokeUserCredentialsTx(ctx, tx, userID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit set user disabled: %w", err)
	}
	return nil
}

// RevokeUserCredentials deletes every session and API token of one user without
// changing the disabled flag.
func (s *Store) RevokeUserCredentials(ctx context.Context, userID int64) error {
	if userID <= 0 {
		return errors.New("sqlite: user id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin revoke credentials: %w", err)
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, userID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: user %d", store.ErrNotFound, userID)
		}
		return fmt.Errorf("sqlite: check user: %w", err)
	}
	if err := revokeUserCredentialsTx(ctx, tx, userID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit revoke credentials: %w", err)
	}
	return nil
}

func revokeUserCredentialsTx(ctx context.Context, tx *sql.Tx, userID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("sqlite: revoke sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("sqlite: revoke api tokens: %w", err)
	}
	return nil
}

// GetProjectAccess returns the role granting userID access to projectID. A
// missing grant, a disabled user, or an unknown user or project is ErrNotFound.
func (s *Store) GetProjectAccess(ctx context.Context, userID, projectID int64) (string, error) {
	if userID <= 0 || projectID <= 0 {
		return "", errors.New("sqlite: user id and project id are required")
	}
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT g.role FROM project_grants g
		JOIN users u ON u.id = g.user_id
		WHERE g.project_id = ? AND g.user_id = ? AND u.disabled = 0`,
		projectID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: project %d grant for user %d", store.ErrNotFound, projectID, userID)
	}
	if err != nil {
		return "", fmt.Errorf("sqlite: get project access: %w", err)
	}
	return role, nil
}

// ListProjectGrants returns one project's grants ordered by user ID.
func (s *Store) ListProjectGrants(ctx context.Context, projectID int64, cursor string, limit int) ([]store.ProjectGrant, string, error) {
	if projectID <= 0 {
		return nil, "", errors.New("sqlite: project id is required")
	}
	if err := s.requireProject(ctx, projectID); err != nil {
		return nil, "", err
	}
	limit = normalizeLimit(limit)
	where := []string{"g.project_id = ?"}
	args := []any{projectID}
	if cursor != "" {
		bound, err := decodeListCursor(cursor, listCursorGrants)
		if err != nil {
			return nil, "", err
		}
		if bound.ProjectID == nil || *bound.ProjectID != projectID || bound.UserID <= 0 {
			return nil, "", fmt.Errorf("%w: project grants cursor", store.ErrInvalidCursor)
		}
		where = append(where, "g.user_id > ?")
		args = append(args, bound.UserID)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT g.project_id, g.user_id, g.role, u.username
		FROM project_grants g JOIN users u ON u.id = g.user_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY g.user_id LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list project grants: %w", err)
	}
	defer rows.Close()
	grants := make([]store.ProjectGrant, 0, limit)
	for rows.Next() {
		var grant store.ProjectGrant
		if err := rows.Scan(&grant.ProjectID, &grant.UserID, &grant.Role, &grant.Username); err != nil {
			return nil, "", fmt.Errorf("sqlite: scan project grant: %w", err)
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: iterate project grants: %w", err)
	}
	next := ""
	if len(grants) > limit {
		grants = grants[:limit]
		nextProject := projectID
		next = encodeListCursor(listCursor{
			Kind: listCursorGrants, ProjectID: &nextProject, UserID: grants[len(grants)-1].UserID,
		})
	}
	return grants, next, nil
}

func (s *Store) requireProject(ctx context.Context, projectID int64) error {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, projectID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: project %d", store.ErrNotFound, projectID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: check project: %w", err)
	}
	return nil
}

// SetProjectGrant inserts or replaces one user's role on one project. The user
// and project must exist; a disabled user may be granted because authentication
// is rejected separately.
func (s *Store) SetProjectGrant(ctx context.Context, projectID, userID int64, role string) error {
	if projectID <= 0 || userID <= 0 {
		return errors.New("sqlite: project id and user id are required")
	}
	switch role {
	case store.ProjectAccessRead, store.ProjectAccessManage:
	default:
		return fmt.Errorf("sqlite: invalid project access role %q", role)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin set project grant: %w", err)
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, projectID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: project %d", store.ErrNotFound, projectID)
		}
		return fmt.Errorf("sqlite: check project: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, userID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: user %d", store.ErrNotFound, userID)
		}
		return fmt.Errorf("sqlite: check user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO project_grants (project_id, user_id, role)
		VALUES (?, ?, ?)
		ON CONFLICT (project_id, user_id) DO UPDATE SET role = excluded.role`,
		projectID, userID, role); err != nil {
		return fmt.Errorf("sqlite: set project grant: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit project grant: %w", err)
	}
	return nil
}

// DeleteProjectGrant removes one grant. A missing grant is ErrNotFound.
func (s *Store) DeleteProjectGrant(ctx context.Context, projectID, userID int64) error {
	if projectID <= 0 || userID <= 0 {
		return errors.New("sqlite: project id and user id are required")
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM project_grants WHERE project_id = ? AND user_id = ?`, projectID, userID)
	if err != nil {
		return fmt.Errorf("sqlite: delete project grant: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: delete project grant: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("%w: project %d grant for user %d", store.ErrNotFound, projectID, userID)
	}
	return nil
}

// userDisabledTx reports whether userID is disabled or absent. It is used by
// credential issuance inside the same transaction as the user upsert.
func userDisabledTx(ctx context.Context, tx *sql.Tx, userID int64) (bool, error) {
	var disabled int
	err := tx.QueryRowContext(ctx, `SELECT disabled FROM users WHERE id = ?`, userID).Scan(&disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: user %d", store.ErrNotFound, userID)
	}
	if err != nil {
		return false, fmt.Errorf("sqlite: check user disabled: %w", err)
	}
	return disabled != 0, nil
}

// requireEnabledUserTx rejects credential creation for a disabled user. Reading
// inside the issuance write transaction makes disable and issuance serializable.
func requireEnabledUserTx(ctx context.Context, tx *sql.Tx, userID int64) error {
	disabled, err := userDisabledTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	if disabled {
		return fmt.Errorf("%w: user %d is disabled", store.ErrNotFound, userID)
	}
	return nil
}
