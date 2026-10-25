package sqlite

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

// CreateSession persists one browser session and upserts its user in the same
// transaction. IDHash is the SHA-256 digest of the cookie token; the plaintext
// token is never stored. Expired rows are removed first, then the oldest
// sessions are evicted until the new row fits within limit.
func (s *Store) CreateSession(ctx context.Context, session store.Session, limit int) error {
	id, err := sessionID(session.IDHash)
	if err != nil {
		return err
	}
	if session.UserID <= 0 {
		return errors.New("sqlite: session user id is required")
	}
	if strings.TrimSpace(session.Username) == "" {
		return errors.New("sqlite: session username is required")
	}
	if session.Role != "admin" && session.Role != "viewer" {
		return fmt.Errorf("sqlite: invalid session role %q", session.Role)
	}
	if limit <= 0 {
		limit = 4096
	}
	created := session.CreatedAt
	if created.IsZero() {
		created = s.now().UTC()
	}
	// Callers own the session clock (tests may use a fake clock), so expiry
	// cleanup is relative to the session's creation time.
	if !created.Before(session.ExpiresAt) {
		return errors.New("sqlite: session expiry must be after creation")
	}
	name := session.Name
	if strings.TrimSpace(name) == "" {
		name = session.Username
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin create session: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, username, name, role, last_login_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			username = excluded.username,
			name = excluded.name,
			role = excluded.role,
			last_login_at = excluded.last_login_at`,
		session.UserID, session.Username, name, session.Role, formatTime(created)); err != nil {
		return fmt.Errorf("sqlite: upsert session user: %w", err)
	}
	if err := requireEnabledUserTx(ctx, tx, session.UserID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`,
		formatTime(created)); err != nil {
		return fmt.Errorf("sqlite: delete expired sessions: %w", err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
		return fmt.Errorf("sqlite: count sessions: %w", err)
	}
	if count >= limit {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id IN (
			SELECT id FROM sessions ORDER BY expires_at ASC, created_at ASC, id ASC LIMIT ?)`,
			count-limit+1); err != nil {
			return fmt.Errorf("sqlite: evict old sessions: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, user_id, expires_at, created_at)
		VALUES (?, ?, ?, ?)`,
		id, session.UserID, formatTime(session.ExpiresAt), formatTime(created)); err != nil {
		if isConstraintErr(err) {
			return fmt.Errorf("%w: session", store.ErrConflict)
		}
		return fmt.Errorf("sqlite: insert session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit session: %w", err)
	}
	return nil
}

// GetSession resolves a cookie token digest. The stored digest and the query
// digest are compared in constant time. An expired row is deleted on access
// and reported as not found, so expiry is persistent.
func (s *Store) GetSession(ctx context.Context, idHash []byte, now time.Time) (*store.Session, error) {
	id, err := sessionID(idHash)
	if err != nil {
		return nil, err
	}
	var (
		storedID  string
		user      store.TokenUser
		expiresAt string
		createdAt string
	)
	err = s.db.QueryRowContext(ctx, `SELECT s.id, s.user_id, s.expires_at, s.created_at,
		u.username, u.name, u.role
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id = ? AND u.disabled = 0`, id).Scan(
		&storedID, &user.ID, &expiresAt, &createdAt,
		&user.Username, &user.Name, &user.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: session", store.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: find session: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(storedID), []byte(id)) != 1 {
		return nil, fmt.Errorf("%w: session", store.ErrNotFound)
	}
	expires, err := parseTime(expiresAt)
	if err != nil {
		return nil, err
	}
	created, err := parseTime(createdAt)
	if err != nil {
		return nil, err
	}
	if !now.Before(expires) {
		// Best effort: the expiry check already rejects this session, and a
		// failed delete must not authenticate it.
		_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, storedID)
		return nil, fmt.Errorf("%w: session expired", store.ErrNotFound)
	}
	return &store.Session{
		IDHash:    idHash,
		UserID:    user.ID,
		Username:  user.Username,
		Name:      user.Name,
		Role:      user.Role,
		ExpiresAt: expires,
		CreatedAt: created,
	}, nil
}

// DeleteSession revokes one session. Missing rows are not an error so logout
// stays idempotent.
func (s *Store) DeleteSession(ctx context.Context, idHash []byte) error {
	id, err := sessionID(idHash)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("sqlite: delete session: %w", err)
	}
	return nil
}

func sessionID(idHash []byte) (string, error) {
	if len(idHash) != 32 {
		return "", errors.New("sqlite: session id hash must be 32 bytes")
	}
	return hex.EncodeToString(idHash), nil
}
