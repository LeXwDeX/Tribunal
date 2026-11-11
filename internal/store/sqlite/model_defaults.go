package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/LeXwDeX/tribunal/internal/store"
)

var _ store.ModelDefaultsStore = (*Store)(nil)

const modelDefaultsColumns = `params_json, revision, updated_by, updated_at`

// GetModelDefaults returns the platform-level defaults. A missing row is an
// empty document at revision 0 so callers never need a not-found branch.
func (s *Store) GetModelDefaults(ctx context.Context) (*store.ModelDefaults, error) {
	return scanModelDefaults(s.db.QueryRowContext(ctx, `SELECT `+modelDefaultsColumns+` FROM llm_model_defaults WHERE id=1`))
}

func scanModelDefaults(row rowScanner) (*store.ModelDefaults, error) {
	var defaults store.ModelDefaults
	var params, updated string
	err := row.Scan(&params, &defaults.Revision, &defaults.UpdatedBy, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return &store.ModelDefaults{}, nil
	}
	if err != nil {
		return nil, errors.New("sqlite: read model defaults failed")
	}
	if err := json.Unmarshal([]byte(params), &defaults.Params); err != nil {
		return nil, errors.New("sqlite: invalid model defaults")
	}
	if defaults.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, errors.New("sqlite: invalid model defaults time")
	}
	return &defaults, nil
}

// PutModelDefaults replaces the whole document. Revision 0 creates the row;
// any other revision must match the stored one.
func (s *Store) PutModelDefaults(ctx context.Context, params store.ModelParams, revision, userID int64) (*store.ModelDefaults, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, errors.New("sqlite: invalid model defaults")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("sqlite: begin model defaults update failed")
	}
	defer tx.Rollback()
	now := formatTime(s.now())
	res, err := tx.ExecContext(ctx, `INSERT INTO llm_model_defaults (id, params_json, revision, updated_by, updated_at)
		SELECT 1, ?, 1, ?, ? WHERE ?=0 ON CONFLICT(id) DO NOTHING`, string(body), userID, now, revision)
	if err != nil {
		return nil, errors.New("sqlite: create model defaults failed")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, errors.New("sqlite: update model defaults failed")
	}
	if n == 0 {
		res, err = tx.ExecContext(ctx, `UPDATE llm_model_defaults SET params_json=?, revision=revision+1, updated_by=?, updated_at=? WHERE id=1 AND revision=?`, string(body), userID, now, revision)
		if err != nil {
			return nil, errors.New("sqlite: update model defaults failed")
		}
		n, err = res.RowsAffected()
		if err != nil {
			return nil, errors.New("sqlite: update model defaults failed")
		}
		if n == 0 {
			return nil, store.ErrStaleRevision
		}
	}
	result, err := scanModelDefaults(tx.QueryRowContext(ctx, `SELECT `+modelDefaultsColumns+` FROM llm_model_defaults WHERE id=1`))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.New("sqlite: commit model defaults update failed")
	}
	return result, nil
}

// DeleteModelDefaults clears the document without deleting the row, so the
// revision stays monotonic and a stale writer cannot resurrect the old values.
func (s *Store) DeleteModelDefaults(ctx context.Context, revision, userID int64) (*store.ModelDefaults, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("sqlite: begin model defaults reset failed")
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE llm_model_defaults SET params_json='{}', revision=revision+1, updated_by=?, updated_at=? WHERE id=1 AND revision=?`, userID, formatTime(s.now()), revision)
	if err != nil {
		return nil, errors.New("sqlite: reset model defaults failed")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, errors.New("sqlite: reset model defaults failed")
	}
	if n == 0 {
		return nil, store.ErrStaleRevision
	}
	result, err := scanModelDefaults(tx.QueryRowContext(ctx, `SELECT `+modelDefaultsColumns+` FROM llm_model_defaults WHERE id=1`))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.New("sqlite: commit model defaults reset failed")
	}
	return result, nil
}
