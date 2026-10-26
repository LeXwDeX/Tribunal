package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// BindAdminUsername returns the GitLab user ID that owns a username-form
// bootstrap admin entry. The first caller binds the username to userID; the
// binding never changes afterwards, so a different account that later takes
// the same GitLab username gets the original owner's ID back.
func (s *Store) BindAdminUsername(ctx context.Context, username string, userID int64, now time.Time) (int64, error) {
	name := strings.ToLower(strings.TrimSpace(username))
	if name == "" {
		return 0, errors.New("sqlite: admin binding username is required")
	}
	if userID <= 0 {
		return 0, errors.New("sqlite: admin binding user id is required")
	}
	// Most calls find an existing binding; only the first one writes.
	owner, err := s.adminBindingOwner(ctx, name)
	if err == nil {
		return owner, nil
	}
	if !errors.Is(err, errNoAdminBinding) {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO admin_bindings (username, user_id, bound_at)
		VALUES (?, ?, ?) ON CONFLICT (username) DO NOTHING`,
		name, userID, formatTime(now.UTC())); err != nil {
		return 0, fmt.Errorf("sqlite: insert admin binding: %w", err)
	}
	// A concurrent first login may have won the insert; report its owner.
	return s.adminBindingOwner(ctx, name)
}

var errNoAdminBinding = errors.New("sqlite: no admin binding")

func (s *Store) adminBindingOwner(ctx context.Context, name string) (int64, error) {
	var owner int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id FROM admin_bindings WHERE username = ?`, name).Scan(&owner)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, errNoAdminBinding
		}
		return 0, fmt.Errorf("sqlite: find admin binding: %w", err)
	}
	return owner, nil
}
