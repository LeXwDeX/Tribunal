package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// ListExtensionDefaults returns the whole default extension ledger sorted by
// key. The ledger holds a few rows per shipped default.
func (s *Store) ListExtensionDefaults(ctx context.Context) ([]store.ExtensionDefault, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, outcome, detail, applied_at FROM extension_defaults ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list extension defaults: %w", err)
	}
	defer rows.Close()
	var out []store.ExtensionDefault
	for rows.Next() {
		var row store.ExtensionDefault
		var appliedAt string
		if err := rows.Scan(&row.Key, &row.Outcome, &row.Detail, &appliedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan extension default: %w", err)
		}
		if row.AppliedAt, err = parseTime(appliedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list extension defaults: %w", err)
	}
	return out, nil
}

// PutExtensionDefault records or replaces one ledger row.
func (s *Store) PutExtensionDefault(ctx context.Context, row store.ExtensionDefault) error {
	row.Key = strings.TrimSpace(row.Key)
	if row.Key == "" {
		return errors.New("sqlite: extension default key is required")
	}
	if row.Outcome != store.ExtensionDefaultApplied && row.Outcome != store.ExtensionDefaultSkipped {
		return fmt.Errorf("sqlite: invalid extension default outcome %q", row.Outcome)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO extension_defaults (key, outcome, detail, applied_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET
			outcome = excluded.outcome,
			detail = excluded.detail,
			applied_at = excluded.applied_at`,
		row.Key, row.Outcome, row.Detail, formatTime(s.now())); err != nil {
		return fmt.Errorf("sqlite: put extension default: %w", err)
	}
	return nil
}
