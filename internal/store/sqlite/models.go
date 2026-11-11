package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
)

var _ store.ModelStore = (*Store)(nil)

var errModelCorrupt = errors.New("sqlite: corrupt model record")

const modelColumns = `id, display_name, protocol, base_url, model, params_json, api_key_sealed, revision, created_by, created_at, updated_by, updated_at, deleted_at`

// ConfigureModelSecrets sets the cipher used to check credentials on reads.
// Invalid credentials are isolated per model and do not fail startup.
func (s *Store) ConfigureModelSecrets(ctx context.Context, cipher *secret.Cipher) error {
	if cipher == nil {
		return errors.New("sqlite: model cipher is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.modelCipher.Store(cipher)
	_, err := s.ListModels(ctx)
	if err != nil {
		s.modelCipher.Store(nil)
		return errors.New("sqlite: check model credentials failed")
	}
	return nil
}

func validateModel(m store.Model) error {
	if n := utf8.RuneCountInString(m.DisplayName); n < 1 || n > 128 || strings.TrimSpace(m.DisplayName) == "" || strings.IndexFunc(m.DisplayName, unicode.IsControl) >= 0 {
		return errors.New("sqlite: invalid model display name")
	}
	p := m.Params
	return config.ValidateProviderConfig("model", config.ProviderConfig{Protocol: m.Protocol, BaseURL: m.BaseURL, Model: m.Model, MaxTokens: p.MaxTokens, ContextWindow: p.ContextWindow, Temperature: p.Temperature, Reasoning: p.Reasoning, Thinking: p.Thinking, ForcedToolChoice: p.ForcedToolChoice, StrictRoleAlternation: p.StrictRoleAlternation, MaxConcurrency: p.MaxConcurrency, RPM: p.RPM, Price: p.Price,
		AllowReasoningEffortNone: p.AllowReasoningEffortNone, PreserveReasoningContent: p.PreserveReasoningContent, OutputConfigEffort: p.OutputConfigEffort, ResponsesParams: p.ResponsesParams}, config.ProviderValidationOptions{})
}

func (s *Store) scanModel(row rowScanner) (*store.Model, error) {
	var m store.Model
	var params, created, updated string
	var deleted sql.NullString
	var sealed []byte
	if err := row.Scan(&m.ID, &m.DisplayName, &m.Protocol, &m.BaseURL, &m.Model, &params, &sealed, &m.Revision, &m.CreatedBy, &created, &m.UpdatedBy, &updated, &deleted); err != nil {
		return nil, err
	}
	corrupt := false
	if err := json.Unmarshal([]byte(params), &m.Params); err != nil {
		m.Params = store.ModelParams{}
		corrupt = true
	}
	var err error
	if m.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if m.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	if deleted.Valid {
		t, err := parseTime(deleted.String)
		if err != nil {
			return nil, err
		}
		m.DeletedAt = &t
	}
	m.HasAPIKey = len(sealed) != 0
	if m.HasAPIKey {
		cipher := s.modelCipher.Load()
		if cipher == nil {
			m.Unavailable = true
		} else {
			_, err := cipher.Open(sealed, secret.ModelCredentialAAD(m.ID, "api_key"))
			m.Unavailable = err != nil
		}
	}
	if corrupt {
		m.Unavailable = true
		return &m, errModelCorrupt
	}
	return &m, nil
}

func (s *Store) ListModels(ctx context.Context) ([]store.Model, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+modelColumns+` FROM llm_models WHERE deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, errors.New("sqlite: list models failed")
	}
	defer rows.Close()
	models := []store.Model{}
	for rows.Next() {
		m, err := s.scanModel(rows)
		if errors.Is(err, errModelCorrupt) {
			models = append(models, *m)
			continue
		}
		if err != nil {
			return nil, errors.New("sqlite: scan model failed")
		}
		models = append(models, *m)
	}
	if rows.Err() != nil {
		return nil, errors.New("sqlite: iterate models failed")
	}
	return models, nil
}

func (s *Store) GetModel(ctx context.Context, id string) (*store.Model, error) {
	m, err := s.scanModel(s.db.QueryRowContext(ctx, `SELECT `+modelColumns+` FROM llm_models WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, errors.New("sqlite: get model failed")
	}
	return m, nil
}

// GetModelEndpoint reads only the endpoint and credential-presence columns, so
// a corrupt params_json does not hide whether a stored key exists. A full
// record update can still replace the corrupt parameters.
func (s *Store) GetModelEndpoint(ctx context.Context, id string) (store.ModelEndpoint, error) {
	var endpoint store.ModelEndpoint
	err := s.db.QueryRowContext(ctx, `SELECT protocol, base_url, CASE WHEN api_key_sealed IS NOT NULL AND length(api_key_sealed) > 0 THEN 1 ELSE 0 END FROM llm_models WHERE id = ? AND deleted_at IS NULL`, id).Scan(&endpoint.Protocol, &endpoint.BaseURL, &endpoint.HasAPIKey)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ModelEndpoint{}, store.ErrNotFound
	}
	if err != nil {
		return store.ModelEndpoint{}, errors.New("sqlite: get model endpoint failed")
	}
	return endpoint, nil
}

func (s *Store) sealModelCredential(id, value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	cipher := s.modelCipher.Load()
	if cipher == nil {
		return nil, errors.New("sqlite: model cipher is not configured")
	}
	sealed, err := cipher.Seal([]byte(value), secret.ModelCredentialAAD(id, "api_key"))
	if err != nil {
		return nil, errors.New("sqlite: seal model credential failed")
	}
	return sealed, nil
}

func (s *Store) CreateModel(ctx context.Context, m store.Model, credential string) (*store.Model, error) {
	if err := validateModel(m); err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	m.ID = "user:" + id
	sealed, err := s.sealModelCredential(m.ID, credential)
	if err != nil {
		return nil, err
	}
	params, err := json.Marshal(m.Params)
	if err != nil {
		return nil, errors.New("sqlite: invalid model parameters")
	}
	now := formatTime(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("sqlite: begin model create failed")
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO llm_models (id, display_name, protocol, base_url, model, params_json, api_key_sealed, revision, created_by, created_at, updated_by, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`, m.ID, m.DisplayName, m.Protocol, m.BaseURL, m.Model, string(params), sealed, m.CreatedBy, now, m.CreatedBy, now); err != nil {
		return nil, errors.New("sqlite: create model failed")
	}
	created, err := s.scanModel(tx.QueryRowContext(ctx, `SELECT `+modelColumns+` FROM llm_models WHERE id = ?`, m.ID))
	if err != nil {
		return nil, errors.New("sqlite: read created model failed")
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.New("sqlite: commit model create failed")
	}
	return created, nil
}

func (s *Store) UpdateModel(ctx context.Context, m store.Model, revision int64, credential *string) (*store.Model, error) {
	if err := validateModel(m); err != nil {
		return nil, err
	}
	params, err := json.Marshal(m.Params)
	if err != nil {
		return nil, errors.New("sqlite: invalid model parameters")
	}
	var sealed []byte
	if credential != nil {
		sealed, err = s.sealModelCredential(m.ID, *credential)
		if err != nil {
			return nil, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("sqlite: begin model update failed")
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE llm_models SET display_name=?, protocol=?, base_url=?, model=?, params_json=?, api_key_sealed=CASE WHEN ? THEN ? ELSE api_key_sealed END, revision=revision+1, updated_by=?, updated_at=? WHERE id=? AND revision=? AND deleted_at IS NULL`, m.DisplayName, m.Protocol, m.BaseURL, m.Model, string(params), credential != nil, sealed, m.UpdatedBy, formatTime(s.now()), m.ID, revision)
	if err != nil {
		return nil, errors.New("sqlite: update model failed")
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, errors.New("sqlite: update model failed")
	} else if n == 0 {
		return nil, modelWriteError(ctx, tx, m.ID)
	}
	updated, err := s.scanModel(tx.QueryRowContext(ctx, `SELECT `+modelColumns+` FROM llm_models WHERE id = ?`, m.ID))
	if err != nil {
		return nil, errors.New("sqlite: read updated model failed")
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.New("sqlite: commit model update failed")
	}
	return updated, nil
}

// UpdateModelParams merges patch into the stored parameters. It reads the
// current record, applies only the named keys and writes the result under the
// caller's revision, so a concurrent change reports ErrStaleRevision instead
// of losing an update.
func (s *Store) UpdateModelParams(ctx context.Context, id string, revision int64, patch store.ModelParamsPatch, updatedBy int64) (*store.Model, error) {
	current, err := s.GetModel(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.DeletedAt != nil {
		return nil, store.ErrNotFound
	}
	current.Params = patch.Apply(current.Params)
	current.UpdatedBy = updatedBy
	return s.UpdateModel(ctx, *current, revision, nil)
}

func modelWriteError(ctx context.Context, tx *sql.Tx, id string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM llm_models WHERE id = ? AND deleted_at IS NULL)`, id).Scan(&exists); err != nil {
		return errors.New("sqlite: check model failed")
	}
	if !exists {
		return store.ErrNotFound
	}
	return store.ErrStaleRevision
}

func (s *Store) SoftDeleteModel(ctx context.Context, id string, revision, userID int64) error {
	now := formatTime(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("sqlite: begin model delete failed")
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE llm_models SET api_key_sealed=NULL, deleted_at=?, updated_at=?, updated_by=?, revision=revision+1 WHERE id=? AND revision=? AND deleted_at IS NULL`, now, now, userID, id, revision)
	if err != nil {
		return errors.New("sqlite: delete model failed")
	}
	if n, err := res.RowsAffected(); err != nil {
		return errors.New("sqlite: delete model failed")
	} else if n == 0 {
		return modelWriteError(ctx, tx, id)
	}
	var referenced bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_profile_model_refs WHERE model_id=?)`, id).Scan(&referenced); err != nil {
		return errors.New("sqlite: check model references failed")
	}
	if referenced {
		return store.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return errors.New("sqlite: commit model delete failed")
	}
	return nil
}

func (s *Store) OpenModelCredential(ctx context.Context, id string) ([]byte, error) {
	var sealed []byte
	err := s.db.QueryRowContext(ctx, `SELECT api_key_sealed FROM llm_models WHERE id=? AND deleted_at IS NULL`, id).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, errors.New("sqlite: read model credential failed")
	}
	if len(sealed) == 0 {
		return nil, nil
	}
	cipher := s.modelCipher.Load()
	if cipher == nil {
		return nil, errors.New("sqlite: model credential unavailable")
	}
	plain, err := cipher.Open(sealed, secret.ModelCredentialAAD(id, "api_key"))
	if err != nil {
		return nil, errors.New("sqlite: model credential unavailable")
	}
	return plain, nil
}
