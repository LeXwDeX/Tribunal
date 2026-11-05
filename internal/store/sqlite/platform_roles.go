package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/LeXwDeX/tribunal/internal/store"
)

const configuredAdminSQL = `(u.id IN (SELECT value FROM json_each(?)) OR EXISTS (
	SELECT 1 FROM admin_bindings b WHERE b.user_id = u.id AND b.username = lower(trim(u.username))
	AND b.username IN (SELECT value FROM json_each(?))))`

func adminConfig(ids []int64, names []string) (string, string) {
	if ids == nil {
		ids = []int64{}
	}
	if names == nil {
		names = []string{}
	}
	i, _ := json.Marshal(ids)
	n, _ := json.Marshal(names)
	return string(i), string(n)
}

// RefreshPlatformRole uses the current username, configuration and grant in
// one statement so an old session cannot overwrite a newer role mirror.
func (s *Store) RefreshPlatformRole(ctx context.Context, userID int64, ids []int64, names []string) error {
	i, n := adminConfig(ids, names)
	_, err := s.db.ExecContext(ctx, `UPDATE users AS u SET revision=revision+1,
		role=CASE WHEN u.disabled=0 AND (`+configuredAdminSQL+` OR u.granted_role='admin') THEN 'admin' ELSE 'viewer' END
		WHERE u.id=? AND u.role != CASE WHEN u.disabled=0 AND (`+configuredAdminSQL+` OR u.granted_role='admin') THEN 'admin' ELSE 'viewer' END`, i, n, userID, i, n)
	return err
}

// GrantedPlatformAdmin checks the durable grant instead of the role mirror.
func (s *Store) GrantedPlatformAdmin(ctx context.Context, userID int64) (bool, error) {
	var granted bool
	err := s.db.QueryRowContext(ctx, `SELECT disabled = 0 AND COALESCE(granted_role = 'admin', 0) FROM users WHERE id = ?`, userID).Scan(&granted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlite: read platform grant: %w", err)
	}
	return granted, nil
}

func (s *Store) ListPlatformUsers(ctx context.Context, cursor string, limit int, ids []int64, names []string) ([]store.User, string, error) {
	users, next, err := s.ListUsers(ctx, cursor, limit)
	if err != nil {
		return nil, "", err
	}
	i, n := adminConfig(ids, names)
	userIDs := make([]int64, 0, len(users))
	for _, u := range users {
		userIDs = append(userIDs, u.ID)
	}
	selected, _ := json.Marshal(userIDs)
	rows, err := s.db.QueryContext(ctx, `SELECT u.id FROM users u WHERE u.id IN (SELECT value FROM json_each(?)) AND `+configuredAdminSQL, string(selected), i, n)
	if err != nil {
		return nil, "", err
	}
	configured := make(map[int64]bool, len(users))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, "", err
		}
		configured[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, "", err
	}
	if err := rows.Close(); err != nil {
		return nil, "", err
	}
	for j := range users {
		u := &users[j]
		u.RoleSource = "none"
		if configured[u.ID] {
			u.RoleSource = "config"
		} else if u.GrantedRole == "admin" {
			u.RoleSource = "granted"
		}
		u.Role = "viewer"
		if !u.Disabled && u.RoleSource != "none" {
			u.Role = "admin"
		}
	}
	return users, next, nil
}

// UpdatePlatformUser serializes the safety checks and the role/disabled write.
func (s *Store) UpdatePlatformUser(ctx context.Context, userID int64, in store.PlatformUserUpdate) (string, string, error) {
	if userID <= 0 || (in.Role != nil && *in.Role != "admin" && *in.Role != "viewer") || (in.Role == nil && in.Disabled == nil) || in.Revision == nil || *in.Revision <= 0 {
		return "", "", errors.New("sqlite: invalid platform user update")
	}
	i, n := adminConfig(in.AdminIDs, in.AdminUsernames)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	if in.ActorID > 0 {
		var allowed bool
		err := tx.QueryRowContext(ctx, `SELECT u.disabled=0 AND (`+configuredAdminSQL+` OR COALESCE(u.granted_role='admin',0)) FROM users u WHERE u.id=?`, i, n, in.ActorID).Scan(&allowed)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !allowed) {
			return "", "", store.ErrPlatformUserProtected
		}
		if err != nil {
			return "", "", err
		}
	}
	var disabled, configured bool
	var granted string
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT u.disabled, COALESCE(u.granted_role,''), u.revision, `+configuredAdminSQL+` FROM users u WHERE u.id = ?`, i, n, userID).Scan(&disabled, &granted, &revision, &configured)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", store.ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	if in.Revision != nil && *in.Revision != revision {
		return "", "", store.ErrStaleRevision
	}
	oldRole := "viewer"
	if !disabled && (configured || granted == "admin") {
		oldRole = "admin"
	}
	newDisabled := disabled
	if in.Disabled != nil {
		newDisabled = *in.Disabled
	}
	newGranted := granted
	if in.Role != nil {
		if *in.Role == "admin" {
			newGranted = "admin"
		} else {
			newGranted = ""
		}
	}
	newRole := "viewer"
	if !newDisabled && (configured || newGranted == "admin") {
		newRole = "admin"
	}
	if (userID == in.ActorID && in.ActorID > 0 && ((oldRole == "admin" && newRole != "admin") || (!disabled && newDisabled))) ||
		(configured && ((in.Role != nil && *in.Role == "viewer") || (!disabled && newDisabled))) {
		return "", "", store.ErrPlatformUserProtected
	}
	if oldRole == "admin" && newRole != "admin" {
		var count int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users u WHERE u.disabled = 0 AND (`+configuredAdminSQL+` OR u.granted_role = 'admin')`, i, n).Scan(&count)
		if err != nil {
			return "", "", err
		}
		if count <= 1 {
			return "", "", store.ErrPlatformUserProtected
		}
	}
	if in.Role != nil && newGranted != granted {
		if newGranted == "admin" {
			_, err = tx.ExecContext(ctx, `UPDATE users SET granted_role='admin', granted_by=NULLIF(?,0), granted_at=? WHERE id=?`, in.ActorID, formatTime(s.now()), userID)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE users SET granted_role=NULL, granted_by=NULL, granted_at=NULL WHERE id=?`, userID)
		}
		if err != nil {
			return "", "", err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE users SET disabled=?, role=?, revision=revision+1 WHERE id=?`, boolInt(newDisabled), newRole, userID)
	if err != nil {
		return "", "", err
	}
	if !disabled && newDisabled {
		if err := revokeUserCredentialsTx(ctx, tx, userID); err != nil {
			return "", "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return oldRole, newRole, nil
}

func (s *Store) ListUserGrants(ctx context.Context, userID int64, cursor string, limit int) ([]store.ProjectGrant, string, error) {
	if _, err := s.GetUser(ctx, userID); err != nil {
		return nil, "", err
	}
	limit = normalizeLimit(limit)
	var after int64
	if cursor != "" {
		c, err := decodeListCursor(cursor, "user-grants")
		if err != nil || c.UserID != userID || c.RowProjectID <= 0 {
			return nil, "", store.ErrInvalidCursor
		}
		after = c.RowProjectID
	}
	rows, err := s.db.QueryContext(ctx, `SELECT project_id, user_id, role FROM project_grants WHERE user_id=? AND project_id>? ORDER BY project_id LIMIT ?`, userID, after, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	grants := make([]store.ProjectGrant, 0, limit)
	for rows.Next() {
		var g store.ProjectGrant
		if err := rows.Scan(&g.ProjectID, &g.UserID, &g.Role); err != nil {
			return nil, "", err
		}
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(grants) > limit {
		grants = grants[:limit]
		next = encodeListCursor(listCursor{Kind: "user-grants", UserID: userID, RowProjectID: grants[len(grants)-1].ProjectID})
	}
	return grants, next, nil
}
