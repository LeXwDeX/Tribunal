package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

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
	return putExtensionDefault(ctx, s.db, row, s.now())
}

// SetDefaultBlockBinding writes one platform block binding owned by the
// default extension seeder and records the given ledger rows in the same
// transaction. A nil binding removes the platform row of block instead. Either
// both the binding and the ledger change or neither does, so a failed start
// never leaves a default-written row that looks administrator-owned.
func (s *Store) SetDefaultBlockBinding(ctx context.Context, block string, binding *store.BlockBinding,
	ledger ...store.ExtensionDefault) error {
	block = strings.TrimSpace(block)
	if block == "" {
		return errors.New("sqlite: block is required")
	}
	if binding != nil && (binding.Scope != store.ScopePlatform || binding.ProjectID != nil || strings.TrimSpace(binding.Block) != block) {
		return fmt.Errorf("sqlite: default block binding %q must be the platform row of that block", block)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin default block binding: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if binding != nil {
		if _, err := upsertBlockBinding(ctx, tx, *binding); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `DELETE FROM block_bindings
		WHERE scope = ? AND project_id = 0 AND block = ?`, store.ScopePlatform, block); err != nil {
		return fmt.Errorf("sqlite: delete default block binding: %w", err)
	}
	now := s.now()
	for _, row := range ledger {
		if err := putExtensionDefault(ctx, tx, row, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit default block binding: %w", err)
	}
	return nil
}

func putExtensionDefault(ctx context.Context, db execer, row store.ExtensionDefault, now time.Time) error {
	row.Key = strings.TrimSpace(row.Key)
	if row.Key == "" {
		return errors.New("sqlite: extension default key is required")
	}
	if row.Outcome != store.ExtensionDefaultApplied && row.Outcome != store.ExtensionDefaultSkipped {
		return fmt.Errorf("sqlite: invalid extension default outcome %q", row.Outcome)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO extension_defaults (key, outcome, detail, applied_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET
			outcome = excluded.outcome,
			detail = excluded.detail,
			applied_at = excluded.applied_at`,
		row.Key, row.Outcome, row.Detail, formatTime(now)); err != nil {
		return fmt.Errorf("sqlite: put extension default: %w", err)
	}
	return nil
}

// execer is satisfied by both *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
