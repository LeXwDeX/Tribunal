package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/store"
)

var _ store.ProfileOverrideStore = (*Store)(nil)

func (s *Store) GetProfileOverrides(ctx context.Context, profile string) (*store.ProfileOverrides, error) {
	var result store.ProfileOverrides
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT profile, overrides_json, revision, updated_by, updated_at FROM review_profile_overrides WHERE profile=?`, profile).Scan(&result.Profile, &result.OverridesJSON, &result.Revision, &result.UpdatedBy, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return &store.ProfileOverrides{Profile: profile, OverridesJSON: `{}`}, nil
	}
	if err != nil {
		return nil, errors.New("sqlite: read profile overrides failed")
	}
	result.UpdatedAt, err = parseTime(updated)
	if err != nil {
		return nil, errors.New("sqlite: invalid profile override time")
	}
	return &result, nil
}

// profileModelRefs reads the model references from the same document that is
// stored, so callers cannot accidentally omit a reference from the index.
func profileModelRefs(body string) (map[string]string, error) {
	type entry struct {
		Model string `json:"model"`
	}
	var doc struct {
		Default entry `json:"default"`
		Explore entry `json:"explore"`
		Lanes   struct {
			CodeReview entry `json:"code_review"`
			Review     entry `json:"review"`
			Reasoner   entry `json:"reasoner"`
			Validate   entry `json:"validate"`
		} `json:"lanes"`
		Challenge entry `json:"challenge"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil || !strings.HasPrefix(strings.TrimSpace(body), "{") {
		return nil, errors.New("sqlite: invalid profile overrides")
	}
	all := map[string]string{"default": doc.Default.Model, "explore": doc.Explore.Model, "lanes.code_review": doc.Lanes.CodeReview.Model, "lanes.review": doc.Lanes.Review.Model, "lanes.reasoner": doc.Lanes.Reasoner.Model, "lanes.validate": doc.Lanes.Validate.Model, "challenge": doc.Challenge.Model}
	for node, id := range all {
		if id == "" {
			delete(all, node)
		}
	}
	return all, nil
}

func (s *Store) PutProfileOverrides(ctx context.Context, profile, body string, revision, userID int64) (*store.ProfileOverrides, error) {
	refs, err := profileModelRefs(body)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("sqlite: begin profile override update failed")
	}
	defer tx.Rollback()
	now := formatTime(s.now())
	res, err := tx.ExecContext(ctx, `INSERT INTO review_profile_overrides (profile, overrides_json, revision, updated_by, updated_at) SELECT ?, ?, 1, ?, ? WHERE ?=0 ON CONFLICT(profile) DO NOTHING`, profile, body, userID, now, revision)
	if err != nil {
		return nil, errors.New("sqlite: create profile overrides failed")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, errors.New("sqlite: update profile overrides failed")
	}
	if n == 0 {
		res, err = tx.ExecContext(ctx, `UPDATE review_profile_overrides SET overrides_json=?, revision=revision+1, updated_by=?, updated_at=? WHERE profile=? AND revision=?`, body, userID, now, profile, revision)
		if err != nil {
			return nil, errors.New("sqlite: update profile overrides failed")
		}
		n, err = res.RowsAffected()
		if err != nil {
			return nil, errors.New("sqlite: update profile overrides failed")
		}
		if n == 0 {
			return nil, store.ErrStaleRevision
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM review_profile_model_refs WHERE profile=?`, profile); err != nil {
		return nil, errors.New("sqlite: clear profile model references failed")
	}
	for node, id := range refs {
		if strings.HasPrefix(id, "user:") {
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM llm_models WHERE id=? AND deleted_at IS NULL)`, id).Scan(&exists); err != nil {
				return nil, errors.New("sqlite: check profile model failed")
			}
			if !exists {
				return nil, store.ErrNotFound
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_profile_model_refs (profile, node, model_id) VALUES (?, ?, ?)`, profile, node, id); err != nil {
			return nil, errors.New("sqlite: save profile model reference failed")
		}
	}
	var result store.ProfileOverrides
	var updated string
	err = tx.QueryRowContext(ctx, `SELECT profile, overrides_json, revision, updated_by, updated_at FROM review_profile_overrides WHERE profile=?`, profile).Scan(&result.Profile, &result.OverridesJSON, &result.Revision, &result.UpdatedBy, &updated)
	if err != nil {
		return nil, errors.New("sqlite: read profile overrides failed")
	}
	result.UpdatedAt, err = parseTime(updated)
	if err != nil {
		return nil, errors.New("sqlite: invalid profile override time")
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.New("sqlite: commit profile overrides failed")
	}
	return &result, nil
}

// Reset retains a monotonic revision to prevent stale writes after a reset.
func (s *Store) DeleteProfileOverrides(ctx context.Context, profile string, userID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("sqlite: begin profile override reset failed")
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE review_profile_overrides SET overrides_json='{}', revision=revision+1, updated_by=?, updated_at=? WHERE profile=?`, userID, formatTime(s.now()), profile); err != nil {
		return errors.New("sqlite: reset profile overrides failed")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM review_profile_model_refs WHERE profile=?`, profile); err != nil {
		return errors.New("sqlite: clear profile model references failed")
	}
	if err := tx.Commit(); err != nil {
		return errors.New("sqlite: commit profile override reset failed")
	}
	return nil
}

func (s *Store) ListProfileModelReferences(ctx context.Context, modelID string) ([]store.ProfileModelReference, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT profile, node FROM review_profile_model_refs WHERE model_id=? ORDER BY profile, node`, modelID)
	if err != nil {
		return nil, errors.New("sqlite: list model references failed")
	}
	defer rows.Close()
	refs := []store.ProfileModelReference{}
	for rows.Next() {
		var ref store.ProfileModelReference
		if err := rows.Scan(&ref.Profile, &ref.Node); err != nil {
			return nil, errors.New("sqlite: scan model reference failed")
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}
