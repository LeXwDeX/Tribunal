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

// BuiltinExtension is the identity and enabled state of one managed platform
// resource. Kind is "skill" or "mcp"; Builtin is the shipped name the row was
// installed from, and is empty for rows an administrator created.
type BuiltinExtension struct {
	Kind    string
	Name    string
	Builtin string
	Enabled bool
}

// DefaultRestoreState is the platform extension state a default restore reads
// and validates inside one transaction.
type DefaultRestoreState struct {
	Bindings []store.BlockBinding
	Ledger   []store.ExtensionDefault
	Builtins []BuiltinExtension
}

// DefaultRestorePlan is the set of rows a restore writes in one transaction.
type DefaultRestorePlan struct {
	Bindings []store.BlockBinding
	Ledger   []store.ExtensionDefault
}

// ListExtensionDefaults returns the whole default extension ledger sorted by
// key. The ledger holds a few rows per shipped default.
func (s *Store) ListExtensionDefaults(ctx context.Context) ([]store.ExtensionDefault, error) {
	return listExtensionDefaults(ctx, s.db)
}

func listExtensionDefaults(ctx context.Context, q queryer) ([]store.ExtensionDefault, error) {
	rows, err := q.QueryContext(ctx,
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

// ListPlatformBuiltinExtensions returns the identity and enabled state of
// every platform skill and MCP server, without decrypting server configs.
func (s *Store) ListPlatformBuiltinExtensions(ctx context.Context) ([]BuiltinExtension, error) {
	return listPlatformBuiltinExtensions(ctx, s.db)
}

func listPlatformBuiltinExtensions(ctx context.Context, q queryer) ([]BuiltinExtension, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT 'skill', name, builtin, enabled FROM skills
		WHERE scope = ? AND project_id IS NULL
		UNION ALL
		SELECT 'mcp', name, builtin, enabled FROM mcp_servers
		WHERE scope = ? AND project_id IS NULL
		ORDER BY 1, 2`, store.ScopePlatform, store.ScopePlatform)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list platform built-in extensions: %w", err)
	}
	defer rows.Close()
	out := make([]BuiltinExtension, 0)
	for rows.Next() {
		var row BuiltinExtension
		if err := rows.Scan(&row.Kind, &row.Name, &row.Builtin, &row.Enabled); err != nil {
			return nil, fmt.Errorf("sqlite: scan built-in extension: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list platform built-in extensions: %w", err)
	}
	return out, nil
}

// ReadDefaultRestoreState reads the platform bindings, ledger and built-in
// identities in one read transaction. A preview uses it so it cannot show a
// combination of rows that no single write ever produced.
func (s *Store) ReadDefaultRestoreState(ctx context.Context) (DefaultRestoreState, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DefaultRestoreState{}, fmt.Errorf("sqlite: begin read restore state: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	bindings, err := listPlatformBlockBindings(ctx, tx)
	if err != nil {
		return DefaultRestoreState{}, err
	}
	ledger, err := listExtensionDefaults(ctx, tx)
	if err != nil {
		return DefaultRestoreState{}, err
	}
	builtins, err := listPlatformBuiltinExtensions(ctx, tx)
	if err != nil {
		return DefaultRestoreState{}, err
	}
	if err := tx.Commit(); err != nil {
		return DefaultRestoreState{}, fmt.Errorf("sqlite: commit read restore state: %w", err)
	}
	return DefaultRestoreState{Bindings: bindings, Ledger: ledger, Builtins: builtins}, nil
}

// RestoreDefaultBlockBindings reads the platform extension state, lets plan
// compute the rows to write from that state, and applies them in one
// transaction. The callback runs inside the transaction and must only use the
// state it receives; it must not call Store methods that use s.db, because
// the store runs one connection.
func (s *Store) RestoreDefaultBlockBindings(ctx context.Context,
	plan func(DefaultRestoreState) (DefaultRestorePlan, error)) (DefaultRestorePlan, error) {
	if plan == nil {
		return DefaultRestorePlan{}, errors.New("sqlite: restore plan is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DefaultRestorePlan{}, fmt.Errorf("sqlite: begin restore defaults: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	bindings, err := listPlatformBlockBindings(ctx, tx)
	if err != nil {
		return DefaultRestorePlan{}, err
	}
	ledger, err := listExtensionDefaults(ctx, tx)
	if err != nil {
		return DefaultRestorePlan{}, err
	}
	builtins, err := listPlatformBuiltinExtensions(ctx, tx)
	if err != nil {
		return DefaultRestorePlan{}, err
	}
	result, err := plan(DefaultRestoreState{
		Bindings: bindings,
		Ledger:   ledger,
		Builtins: builtins,
	})
	if err != nil {
		return DefaultRestorePlan{}, err
	}
	for _, binding := range result.Bindings {
		if binding.Scope != store.ScopePlatform || binding.ProjectID != nil || strings.TrimSpace(binding.Block) == "" {
			return DefaultRestorePlan{}, errors.New("sqlite: restore writes only platform block bindings")
		}
		if _, err := upsertBlockBinding(ctx, tx, binding); err != nil {
			return DefaultRestorePlan{}, err
		}
	}
	now := s.now()
	for _, row := range result.Ledger {
		if err := putExtensionDefault(ctx, tx, row, now); err != nil {
			return DefaultRestorePlan{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return DefaultRestorePlan{}, fmt.Errorf("sqlite: commit restore defaults: %w", err)
	}
	return result, nil
}

func listPlatformBlockBindings(ctx context.Context, q queryer) ([]store.BlockBinding, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+blockBindingColumns+` FROM block_bindings
		WHERE scope = ? AND project_id = 0 ORDER BY block`, store.ScopePlatform)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list platform block bindings: %w", err)
	}
	defer rows.Close()
	out := make([]store.BlockBinding, 0)
	for rows.Next() {
		binding, err := scanBlockBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan block binding: %w", err)
		}
		out = append(out, *binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate platform block bindings: %w", err)
	}
	return out, nil
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

// queryer is satisfied by both *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
