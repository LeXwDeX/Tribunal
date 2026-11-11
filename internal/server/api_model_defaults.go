package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// modelDefaultsView is the API shape of the single platform-level defaults
// row. updated_by and updated_at are omitted while no row has been saved.
type modelDefaultsView struct {
	Params    store.ModelParams `json:"params"`
	Revision  int64             `json:"revision"`
	UpdatedBy int64             `json:"updated_by,omitempty"`
	UpdatedAt *time.Time        `json:"updated_at,omitempty"`
}

func modelDefaultsViewOf(defaults *store.ModelDefaults) modelDefaultsView {
	view := modelDefaultsView{Params: defaults.Params, Revision: defaults.Revision, UpdatedBy: defaults.UpdatedBy}
	if !defaults.UpdatedAt.IsZero() {
		updated := defaults.UpdatedAt
		view.UpdatedAt = &updated
	}
	return view
}

func (s *Server) requireModelDefaults(w http.ResponseWriter, r *http.Request) bool {
	if s.modelDefaults == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "model defaults are unavailable", nil)
		return false
	}
	return true
}

func (s *Server) handleGetModelDefaults(w http.ResponseWriter, r *http.Request) {
	if !s.requireModelDefaults(w, r) {
		return
	}
	defaults, err := s.modelDefaults.GetModelDefaults(r.Context())
	if err != nil {
		s.writeModelDefaultsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, modelDefaultsViewOf(defaults))
}

// modelDefaultsRequest replaces the whole defaults document. Params must be
// present; an empty object clears every default.
type modelDefaultsRequest struct {
	Revision *int64                      `json:"revision"`
	Params   *map[string]json.RawMessage `json:"params"`
}

func (s *Server) handlePutModelDefaults(w http.ResponseWriter, r *http.Request) {
	if !s.requireModelDefaults(w, r) {
		return
	}
	var request modelDefaultsRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		if modelDefaultsDecodeError(w, r, err) {
			return
		}
		decodeRequestError(w, r, err)
		return
	}
	if request.Revision == nil || *request.Revision < 0 {
		validationFailed(w, r, "revision", "revision is required and must not be negative")
		return
	}
	if request.Params == nil {
		validationFailed(w, r, "params", "params is required")
		return
	}
	patch, details := decodeModelParamsPatch(*request.Params)
	if len(details) > 0 {
		writeError(w, r, http.StatusBadRequest, "validation_failed", "invalid model defaults", details)
		return
	}
	// PUT replaces the document, so applying the patch to a zero base both
	// validates and stores exactly the submitted keys.
	params := patch.Apply(store.ModelParams{})
	if details := modelDefaultDetails(params); len(details) > 0 {
		writeError(w, r, http.StatusBadRequest, "validation_failed", "invalid model defaults", details)
		return
	}
	// Defaults are protocol-agnostic, so a value that is valid on its own can
	// make an existing model of another protocol unavailable. Refuse the save
	// instead of silently breaking the catalog.
	crossDetails, err := s.modelDefaultCrossModelDetails(r.Context(), params)
	if err != nil {
		s.writeModelDefaultsError(w, r, err)
		return
	}
	if len(crossDetails) > 0 {
		writeError(w, r, http.StatusBadRequest, "validation_failed", "model defaults would make existing models invalid", crossDetails)
		return
	}
	defaults, err := s.modelDefaults.PutModelDefaults(r.Context(), params, *request.Revision, currentUserID(r))
	if err != nil {
		s.writeModelDefaultsError(w, r, err)
		return
	}
	s.auditModelDefaults(r, "update", patch.Fields())
	s.refreshModelCatalogAfterDefaults(r)
	writeJSON(w, http.StatusOK, modelDefaultsViewOf(defaults))
}

func (s *Server) handleDeleteModelDefaults(w http.ResponseWriter, r *http.Request) {
	if !s.requireModelDefaults(w, r) {
		return
	}
	revision, err := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if err != nil || revision < 0 {
		validationFailed(w, r, "revision", "revision is required and must not be negative")
		return
	}
	defaults, err := s.modelDefaults.DeleteModelDefaults(r.Context(), revision, currentUserID(r))
	if err != nil {
		s.writeModelDefaultsError(w, r, err)
		return
	}
	s.auditModelDefaults(r, "reset", nil)
	s.refreshModelCatalogAfterDefaults(r)
	writeJSON(w, http.StatusOK, modelDefaultsViewOf(defaults))
}

// modelDefaultsDecodeError maps an unknown top-level request field to its own
// field path. The strict decoder only reports the leaf name, so only this one
// error class is translated; other decode failures keep the generic body
// error.
func modelDefaultsDecodeError(w http.ResponseWriter, r *http.Request, err error) bool {
	const prefix = `json: unknown field "`
	if !strings.HasPrefix(err.Error(), prefix) || !strings.HasSuffix(err.Error(), `"`) {
		return false
	}
	field := strings.TrimSuffix(strings.TrimPrefix(err.Error(), prefix), `"`)
	validationFailed(w, r, field, field+" is not a supported field")
	return true
}

// modelDefaultDetails validates the defaults without binding them to one
// protocol. A neutral OpenAI-compatible provider exercises the shared field
// rules (non-negative counters, temperature range, effort and enum values)
// while protocol-specific requirements stay on the model itself.
func modelDefaultDetails(params store.ModelParams) []app.Detail {
	cfg := config.ProviderConfig{
		Protocol: "openai-chat", BaseURL: "https://model-defaults.invalid", Model: "defaults",
		MaxTokens: params.MaxTokens, ContextWindow: params.ContextWindow, Temperature: params.Temperature,
		Reasoning: params.Reasoning, Thinking: params.Thinking, ForcedToolChoice: params.ForcedToolChoice,
		StrictRoleAlternation: params.StrictRoleAlternation, MaxConcurrency: params.MaxConcurrency, RPM: params.RPM,
		AllowReasoningEffortNone: params.AllowReasoningEffortNone,
		PreserveReasoningContent: params.PreserveReasoningContent,
		OutputConfigEffort:       params.OutputConfigEffort,
		ResponsesParams:          params.ResponsesParams,
	}
	err := config.ValidateProviderConfig("model_defaults", cfg, config.ProviderValidationOptions{})
	if err == nil {
		return nil
	}
	var joined interface{ Unwrap() []error }
	problems := []error{err}
	if errors.As(err, &joined) {
		problems = joined.Unwrap()
	}
	details := make([]app.Detail, 0, len(problems))
	for _, problem := range problems {
		message := strings.TrimPrefix(problem.Error(), "model_defaults.")
		field, _, _ := strings.Cut(message, " ")
		details = append(details, app.Detail{Field: "params." + field, Message: message})
	}
	return details
}

// modelDefaultCrossModelDetails validates the candidate defaults against every
// existing user model that is currently usable. A value valid for one
// protocol can be invalid for another; this check keeps a save from turning a
// working model unavailable. Already unavailable records are skipped so their
// pre-existing problems do not block the defaults.
func (s *Server) modelDefaultCrossModelDetails(ctx context.Context, params store.ModelParams) ([]app.Detail, error) {
	if s.modelsStore == nil {
		return nil, nil
	}
	models, err := s.modelsStore.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	var details []app.Detail
	for _, model := range models {
		if !strings.HasPrefix(model.ID, "user:") || model.DeletedAt != nil || model.Unavailable {
			continue
		}
		effective, _ := store.MergeModelParams(model.Params, params)
		candidate := model
		candidate.Params = effective
		err := config.ValidateProviderConfig("model", llm.ModelConfig(candidate), config.ProviderValidationOptions{})
		if err == nil {
			continue
		}
		var joined interface{ Unwrap() []error }
		problems := []error{err}
		if errors.As(err, &joined) {
			problems = joined.Unwrap()
		}
		for _, problem := range problems {
			message := strings.TrimPrefix(problem.Error(), "model.")
			field, _, _ := strings.Cut(message, " ")
			path := "models." + model.ID + ".params." + field
			switch field {
			case "protocol", "base_url", "model":
				path = "models." + model.ID + "." + field
			}
			details = append(details, app.Detail{Field: path, Message: model.DisplayName + ": " + message})
		}
	}
	return details, nil
}

func (s *Server) writeModelDefaultsError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrStaleRevision) {
		writeError(w, r, http.StatusConflict, "stale_revision", "revision does not match", nil)
		return
	}
	writeError(w, r, http.StatusServiceUnavailable, "unavailable", "model defaults are unavailable", nil)
}

// refreshModelCatalogAfterDefaults rebuilds the catalog because the effective
// parameters of every user model may have changed.
func (s *Server) refreshModelCatalogAfterDefaults(r *http.Request) {
	if s.refreshModels == nil {
		return
	}
	if err := s.refreshModels(r.Context()); err != nil {
		s.logger.Error("refresh models failed", "event", "model_refresh_failed", "target", "model_defaults")
	}
}

func (s *Server) auditModelDefaults(r *http.Request, action string, fields []string) {
	s.logger.Info("model defaults changed", "event", "platform_audit", "actor_id", currentUserID(r),
		"target_id", "model-defaults", "action", action, "fields", fields)
}

// modelDefaultParams reads the current defaults for request validation. A nil
// store means no defaults, which keeps test servers and older wiring working.
func (s *Server) modelDefaultParams(ctx context.Context) (store.ModelParams, error) {
	if s.modelDefaults == nil {
		return store.ModelParams{}, nil
	}
	defaults, err := s.modelDefaults.GetModelDefaults(ctx)
	if err != nil {
		return store.ModelParams{}, err
	}
	if defaults == nil {
		return store.ModelParams{}, nil
	}
	return defaults.Params, nil
}

// modelValidationParams returns the parameters used to validate a model
// write: the candidate's explicit values with platform defaults filled in.
func (s *Server) modelValidationParams(ctx context.Context, params store.ModelParams) (store.ModelParams, error) {
	defaults, err := s.modelDefaultParams(ctx)
	if err != nil {
		return store.ModelParams{}, err
	}
	merged, _ := store.MergeModelParams(params, defaults)
	return merged, nil
}
