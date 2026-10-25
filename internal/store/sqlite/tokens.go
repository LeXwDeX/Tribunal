package sqlite

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

// CreateAPIToken persists one API token and upserts its owning user in the
// same transaction. The users table normally only sees OAuth logins, but an
// admin session can mint a token before any OAuth user row exists.
func (s *Store) CreateAPIToken(ctx context.Context, user store.TokenUser, token store.APIToken) (*store.APIToken, error) {
	if user.ID <= 0 {
		return nil, errors.New("sqlite: api token user id is required")
	}
	if strings.TrimSpace(user.Username) == "" {
		return nil, errors.New("sqlite: api token username is required")
	}
	if user.Role != "admin" && user.Role != "viewer" {
		return nil, fmt.Errorf("sqlite: invalid api token user role %q", user.Role)
	}
	if strings.TrimSpace(token.Name) == "" {
		return nil, errors.New("sqlite: api token name is required")
	}
	if len(token.TokenHash) != 32 {
		return nil, errors.New("sqlite: api token hash must be 32 bytes")
	}
	token.UserID = user.ID
	if token.ID == "" {
		id, err := newID()
		if err != nil {
			return nil, err
		}
		token.ID = id
	}
	if token.CreatedAt.IsZero() {
		token.CreatedAt = s.now().UTC()
	}
	name := user.Name
	if strings.TrimSpace(name) == "" {
		name = user.Username
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin create api token: %w", err)
	}
	defer tx.Rollback()
	// last_login_at is owned by the login path; a token creation must not
	// overwrite it.
	if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, username, name, role, last_login_at)
		VALUES (?, ?, ?, ?, NULL)
		ON CONFLICT (id) DO UPDATE SET
			username = excluded.username,
			name = excluded.name,
			role = excluded.role`,
		user.ID, user.Username, name, user.Role); err != nil {
		return nil, fmt.Errorf("sqlite: upsert api token user: %w", err)
	}
	if err := requireEnabledUserTx(ctx, tx, user.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO api_tokens
		(id, user_id, name, token_hash, last_used_at, expires_at, created_at)
		VALUES (?, ?, ?, ?, NULL, ?, ?)`,
		token.ID, token.UserID, token.Name, token.TokenHash,
		nullableTime(token.ExpiresAt), formatTime(token.CreatedAt)); err != nil {
		if isConstraintErr(err) {
			return nil, fmt.Errorf("%w: api token %s", store.ErrConflict, token.ID)
		}
		return nil, fmt.Errorf("sqlite: insert api token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit api token: %w", err)
	}
	return &token, nil
}

const apiTokenColumns = `id, user_id, name, token_hash, last_used_at, expires_at, created_at`

func scanAPIToken(row rowScanner) (*store.APIToken, error) {
	var (
		token      store.APIToken
		lastUsedAt sql.NullString
		expiresAt  sql.NullString
		createdAt  string
	)
	if err := row.Scan(&token.ID, &token.UserID, &token.Name, &token.TokenHash,
		&lastUsedAt, &expiresAt, &createdAt); err != nil {
		return nil, err
	}
	var err error
	if token.LastUsedAt, err = parseOptionalTime(lastUsedAt); err != nil {
		return nil, err
	}
	if token.ExpiresAt, err = parseOptionalTime(expiresAt); err != nil {
		return nil, err
	}
	if token.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	return &token, nil
}

func parseOptionalTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

// ListAPITokens returns one user's tokens newest first. Hashes are included
// for internal use but never serialized.
func (s *Store) ListAPITokens(ctx context.Context, userID int64) ([]store.APIToken, error) {
	if userID <= 0 {
		return nil, errors.New("sqlite: api token user id is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens
		WHERE user_id = ? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list api tokens: %w", err)
	}
	defer rows.Close()
	tokens := make([]store.APIToken, 0)
	for rows.Next() {
		token, err := scanAPIToken(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan api token: %w", err)
		}
		tokens = append(tokens, *token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate api tokens: %w", err)
	}
	return tokens, nil
}

// ListAPITokensPage returns one bounded page of one user's tokens. The cursor
// binds to that user so a token ID cannot be resumed across accounts.
func (s *Store) ListAPITokensPage(ctx context.Context, userID int64, cursor string, limit int) ([]store.APIToken, string, error) {
	if userID <= 0 {
		return nil, "", errors.New("sqlite: api token user id is required")
	}
	limit = normalizeLimit(limit)
	where := []string{"user_id = ?"}
	args := []any{userID}
	if cursor != "" {
		bound, err := decodeListCursor(cursor, listCursorTokens)
		if err != nil {
			return nil, "", err
		}
		if bound.UserID != userID || bound.ID == "" {
			return nil, "", fmt.Errorf("%w: api token cursor scope", store.ErrInvalidCursor)
		}
		at, err := parseTime(bound.At)
		if err != nil {
			return nil, "", fmt.Errorf("%w: api token cursor", store.ErrInvalidCursor)
		}
		where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, formatTime(at), formatTime(at), bound.ID)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY created_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list api tokens: %w", err)
	}
	defer rows.Close()
	tokens := make([]store.APIToken, 0, limit)
	for rows.Next() {
		token, err := scanAPIToken(rows)
		if err != nil {
			return nil, "", fmt.Errorf("sqlite: scan api token: %w", err)
		}
		tokens = append(tokens, *token)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: iterate api tokens: %w", err)
	}
	next := ""
	if len(tokens) > limit {
		tokens = tokens[:limit]
		last := tokens[len(tokens)-1]
		next = encodeListCursor(listCursor{
			Kind: listCursorTokens, UserID: userID,
			At: formatTime(last.CreatedAt), ID: last.ID,
		})
	}
	return tokens, next, nil
}

// FindAPITokenByHash resolves a bearer token digest to its token and owning
// user. The digest comparison is constant time; expired tokens are reported
// as not found and never update last_used_at.
func (s *Store) FindAPITokenByHash(ctx context.Context, hash []byte, now time.Time) (*store.APIToken, *store.TokenUser, error) {
	if len(hash) != 32 {
		return nil, nil, errors.New("sqlite: api token hash must be 32 bytes")
	}
	var (
		token      store.APIToken
		lastUsedAt sql.NullString
		expiresAt  sql.NullString
		createdAt  string
		user       store.TokenUser
	)
	err := s.db.QueryRowContext(ctx, `SELECT t.id, t.user_id, t.name, t.token_hash,
		t.last_used_at, t.expires_at, t.created_at, u.username, u.name, u.role
		FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ? AND u.disabled = 0`, hash).Scan(
		&token.ID, &token.UserID, &token.Name, &token.TokenHash,
		&lastUsedAt, &expiresAt, &createdAt,
		&user.Username, &user.Name, &user.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: api token", store.ErrNotFound)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("sqlite: find api token: %w", err)
	}
	if subtle.ConstantTimeCompare(token.TokenHash, hash) != 1 {
		return nil, nil, fmt.Errorf("%w: api token", store.ErrNotFound)
	}
	if token.LastUsedAt, err = parseOptionalTime(lastUsedAt); err != nil {
		return nil, nil, fmt.Errorf("sqlite: scan api token: %w", err)
	}
	if token.ExpiresAt, err = parseOptionalTime(expiresAt); err != nil {
		return nil, nil, fmt.Errorf("sqlite: scan api token: %w", err)
	}
	if token.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, nil, fmt.Errorf("sqlite: scan api token: %w", err)
	}
	if token.ExpiresAt != nil && !now.Before(*token.ExpiresAt) {
		return nil, nil, fmt.Errorf("%w: api token expired", store.ErrNotFound)
	}
	used := now.UTC()
	if _, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`,
		formatTime(used), token.ID); err != nil {
		return nil, nil, fmt.Errorf("sqlite: update api token last use: %w", err)
	}
	token.LastUsedAt = &used
	user.ID = token.UserID
	return &token, &user, nil
}

// DeleteAPIToken revokes one token owned by userID. A token owned by another
// user is reported as not found so IDs cannot be probed.
func (s *Store) DeleteAPIToken(ctx context.Context, userID int64, id string) error {
	if userID <= 0 || strings.TrimSpace(id) == "" {
		return errors.New("sqlite: api token user id and token id are required")
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("sqlite: delete api token: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sqlite: delete api token: %w", err)
	} else if n != 1 {
		return fmt.Errorf("%w: api token %s", store.ErrNotFound, id)
	}
	return nil
}
