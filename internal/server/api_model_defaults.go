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
	// instead of silently breaking the catalog. The current document is the
	// baseline: models already invalid with it are skipped.
	current, err := s.modelDefaultParams(r.Context())
	if err != nil {
		s.writeModelDefaultsError(w, r, err)
		return
	}
	crossDetails, err := s.modelDefaultCrossModelDetails(r.Context(), current, params)
	if err != nil {
		s.writeModelDefaultsError(w, r, err)
		return
	}
	if len(crossDetails) > 0 {
		writeError(w, r, http.StatusBadRequest, "validation_failed", "model defaults would make existing models invalid", crossDetails)
		return
	}
	// Stored profile overrides inherit the candidate defaults. Revalidate them
	// now instead of letting a review fail later with an unretryable
	// bad_request from the provider.
	overrideDetails, err := s.profileOverrideDefaultDetails(r.Context(), params)
	if err != nil {
		s.writeModelDefaultsError(w, r, err)
		return
	}
	if len(overrideDetails) > 0 {
		writeError(w, r, http.StatusBadRequest, "validation_failed", "model defaults would invalidate stored profile overrides", overrideDetails)
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
	force := false
	switch raw := r.URL.Query().Get("force"); raw {
	case "":
	case "true":
		force = true
	case "false":
	default:
		validationFailed(w, r, "force", "force must be true or false")
		return
	}
	// The revision check precedes the cross-model check: a stale writer learns
	// about the conflict instead of the effect of a reset it cannot perform.
	current, err := s.modelDefaults.GetModelDefaults(r.Context())
	if err != nil {
		s.writeModelDefaultsError(w, r, err)
		return
	}
	if current.Revision != revision {
		writeError(w, r, http.StatusConflict, "stale_revision", "revision does not match", nil)
		return
	}
	// A reset removes every value a user model may inherit, so it runs the same
	// cross-model check as PUT with an empty candidate. Without force it fails
	// before writing; force is the explicit administrator override and its audit
	// records the affected model fields. An empty document resets to the same
	// state, so it succeeds without reporting models as in use.
	crossDetails, err := s.modelDefaultCrossModelDetails(r.Context(), current.Params, store.ModelParams{})
	if err != nil {
		s.writeModelDefaultsError(w, r, err)
		return
	}
	if len(crossDetails) > 0 && !force {
		writeError(w, r, http.StatusConflict, "defaults_in_use", "model defaults are required by existing models", crossDetails)
		return
	}
	// A reset also removes capabilities stored overrides may rely on, such as
	// allow_reasoning_effort_none for effort:none. Report the affected nodes
	// instead of failing during a review.
	overrideDetails, err := s.profileOverrideDefaultDetails(r.Context(), store.ModelParams{})
	if err != nil {
		s.writeModelDefaultsError(w, r, err)
		return
	}
	if len(overrideDetails) > 0 && !force {
		writeError(w, r, http.StatusConflict, "overrides_in_use", "model defaults are required by stored profile overrides", overrideDetails)
		return
	}
	defaults, err := s.modelDefaults.DeleteModelDefaults(r.Context(), revision, currentUserID(r))
	if err != nil {
		s.writeModelDefaultsError(w, r, err)
		return
	}
	var fields []string
	if force {
		fields = append(fields, "force")
		for _, detail := range crossDetails {
			fields = append(fields, detail.Field)
		}
		for _, detail := range overrideDetails {
			fields = append(fields, detail.Field)
		}
	}
	s.auditModelDefaults(r, "reset", fields)
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
// working model unavailable.
//
// current is the stored document. A model that is already invalid with the
// current defaults is unavailable in the catalog for validation reasons even
// when the stored model row is not flagged, so its pre-existing problem is
// skipped instead of blocking this write. The same rule lets an empty document
// reset succeed as a no-op.
func (s *Server) modelDefaultCrossModelDetails(ctx context.Context, current, params store.ModelParams) ([]app.Detail, error) {
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
		effective, _ := store.MergeModelParams(model.Params, current)
		if len(modelParamsDetails(model, effective)) > 0 {
			continue
		}
		effective, _ = store.MergeModelParams(model.Params, params)
		details = append(details, modelParamsDetails(model, effective)...)
	}
	return details, nil
}

// modelParamsDetails validates one model with the given effective parameters
// and returns API details on failure.
func modelParamsDetails(model store.Model, params store.ModelParams) []app.Detail {
	candidate := model
	candidate.Params = params
	err := config.ValidateProviderConfig("model", llm.ModelConfig(candidate), config.ProviderValidationOptions{})
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
		message := strings.TrimPrefix(problem.Error(), "model.")
		field, _, _ := strings.Cut(message, " ")
		path := "models." + model.ID + ".params." + field
		switch field {
		case "protocol", "base_url", "model":
			path = "models." + model.ID + "." + field
		}
		details = append(details, app.Detail{Field: path, Message: model.DisplayName + ": " + message})
	}
	return details
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

// profileOverrideDefaultDetails revalidates the stored profile overrides
// against candidate defaults. User models inherit the candidate values, while
// configured models keep their file parameters. Each detail names the affected
// override path and therefore the node.
func (s *Server) profileOverrideDefaultDetails(ctx context.Context, params store.ModelParams) ([]app.Detail, error) {
	if s.profileOverrides == nil || s.platform == nil {
		return nil, nil
	}
	overrides, err := s.profileOverrides.GetProfileOverrides(ctx, "default")
	if errors.Is(err, store.ErrNotFound) || overrides == nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lookup, err := s.candidateModelLookup(ctx, params)
	if err != nil {
		return nil, err
	}
	raw, _ := s.platform.RawProfile("default")
	normalized, _ := s.defaultProfile()
	return app.ValidateProfileOverrides([]byte(overrides.OverridesJSON), raw, normalized, lookup), nil
}

// candidateModelLookup resolves catalog capabilities with the candidate
// defaults applied to user models. Configured models ignore defaults, so the
// current catalog value stays authoritative.
func (s *Server) candidateModelLookup(ctx context.Context, params store.ModelParams) (func(string) (app.OverrideModel, bool), error) {
	if s.models == nil {
		return nil, nil
	}
	candidates := make(map[string]app.OverrideModel)
	if s.modelsStore != nil {
		models, err := s.modelsStore.ListModels(ctx)
		if err != nil {
			return nil, err
		}
		for _, model := range models {
			if !strings.HasPrefix(model.ID, "user:") || model.DeletedAt != nil {
				continue
			}
			effective, _ := store.MergeModelParams(model.Params, params)
			candidates[model.ID] = app.OverrideModel{
				Protocol: model.Protocol, MaxTokens: effective.MaxTokens,
				AllowReasoningEffortNone: effective.AllowReasoningEffortNone,
			}
		}
	}
	return func(id string) (app.OverrideModel, bool) {
		if candidate, ok := candidates[id]; ok {
			return candidate, true
		}
		model, ok := s.models.Resolve(id)
		return app.OverrideModel{
			Unavailable: model.Unavailable, Protocol: model.Protocol, MaxTokens: model.EffectiveParams.MaxTokens,
			AllowReasoningEffortNone: model.EffectiveParams.AllowReasoningEffortNone,
		}, ok
	}, nil
}
