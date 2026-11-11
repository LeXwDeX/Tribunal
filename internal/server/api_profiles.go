package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// profileNodeParams are the request parameters a node effectively sends: the
// model's own values with the node's overrides applied.
type profileNodeParams struct {
	MaxTokens   int                     `json:"max_tokens"`
	Temperature *float64                `json:"temperature"`
	Reasoning   *config.ReasoningConfig `json:"reasoning"`
	Thinking    *config.ThinkingConfig  `json:"thinking"`
}

// profileNodeView adds what the model catalog knows to a resolved node.
type profileNodeView struct {
	app.ProfileNodeView
	// EffectiveParams are the model's parameters with the node's overrides.
	EffectiveParams *profileNodeParams `json:"effective_params,omitempty"`
	// VoteIdentity is the endpoint and model identity votes are grouped by;
	// nodes with the same value count as one model.
	VoteIdentity string `json:"vote_identity"`
}

type profileOverrideView struct {
	Profile   string               `json:"profile"`
	Overrides json.RawMessage      `json:"overrides"`
	Revision  int64                `json:"revision"`
	Effective config.ProfileConfig `json:"effective"`
	Nodes     []profileNodeView    `json:"nodes"`
}

func (s *Server) defaultProfile() (config.ProfileConfig, bool) {
	if s.platform == nil {
		return config.ProfileConfig{}, false
	}
	profile, ok := s.platform.Profiles["default"]
	return profile, ok
}

// resolveDefaultProfile merges a stored override document into the default
// profile, the same way a review does.
func (s *Server) resolveDefaultProfile(overrides string) (app.ResolvedProfile, bool) {
	normalized, ok := s.defaultProfile()
	if !ok {
		return app.ResolvedProfile{}, false
	}
	raw, _ := s.platform.RawProfile("default")
	return app.ResolveProfile(raw, normalized, overrides), true
}

func (s *Server) profileView(overrides string, revision int64) (profileOverrideView, bool) {
	resolved, ok := s.resolveDefaultProfile(overrides)
	if !ok {
		return profileOverrideView{}, false
	}
	view := profileOverrideView{
		Profile: "default", Overrides: json.RawMessage(overrides), Revision: revision,
		Effective: resolved.Effective, Nodes: make([]profileNodeView, 0, len(resolved.Nodes)),
	}
	for _, node := range resolved.Nodes {
		view.Nodes = append(view.Nodes, s.profileNode(node))
	}
	return view, true
}

func (s *Server) profileNode(node app.ProfileNodeView) profileNodeView {
	out := profileNodeView{ProfileNodeView: node}
	if s.models == nil {
		return out
	}
	model, ok := s.models.Resolve(node.Model)
	if !ok {
		return out
	}
	out.VoteIdentity = llm.ProviderInfo{BaseURL: model.BaseURL, Model: model.Model.Model}.ModelIdentity()
	params := profileNodeParams{
		MaxTokens: model.EffectiveParams.MaxTokens, Temperature: model.EffectiveParams.Temperature,
		Reasoning: model.EffectiveParams.Reasoning, Thinking: model.EffectiveParams.Thinking,
	}
	if node.Params != nil {
		if node.Params.MaxTokens != nil {
			params.MaxTokens = *node.Params.MaxTokens
		}
		if node.Params.Temperature != nil {
			params.Temperature = node.Params.Temperature
		}
		if node.Params.Reasoning != nil && node.Params.Reasoning.Effort != "" {
			params.Reasoning = node.Params.Reasoning
		}
		if node.Params.Thinking != nil && (node.Params.Thinking.BudgetTokens != 0 || node.Params.Thinking.Type != "") {
			params.Thinking = node.Params.Thinking
		}
	}
	out.EffectiveParams = &params
	return out
}

func (s *Server) handleGetDefaultProfile(w http.ResponseWriter, r *http.Request) {
	body := `{}`
	var revision int64
	if s.profileOverrides != nil {
		overrides, err := s.profileOverrides.GetProfileOverrides(r.Context(), "default")
		if err == nil && overrides != nil {
			body = overrides.OverridesJSON
			revision = overrides.Revision
		}
	}
	view, ok := s.profileView(body, revision)
	if !ok {
		writeError(w, r, http.StatusNotFound, "not_found", "profile not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handlePutProfileOverrides(w http.ResponseWriter, r *http.Request) {
	if !s.requireProfileOverrides(w, r) {
		return
	}
	var request struct {
		Overrides json.RawMessage `json:"overrides"`
		Revision  int64           `json:"revision"`
	}
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if len(request.Overrides) == 0 || string(request.Overrides) == "null" {
		validationFailed(w, r, "overrides", "overrides is required")
		return
	}
	if request.Revision < 0 {
		validationFailed(w, r, "revision", "revision must not be negative")
		return
	}
	normalized, _ := s.defaultProfile()
	raw, _ := s.platform.RawProfile("default")
	if details := app.ValidateProfileOverrides(request.Overrides, raw, normalized, s.modelLookup()); len(details) > 0 {
		writeError(w, r, http.StatusBadRequest, "validation_failed", "invalid overrides", details)
		return
	}
	overrides, err := s.profileOverrides.PutProfileOverrides(r.Context(), "default", string(request.Overrides), request.Revision, currentUserID(r))
	if err != nil {
		s.writeProfileOverrideError(w, r, err)
		return
	}
	s.auditProfileOverrides(r, "update")
	view, _ := s.profileView(overrides.OverridesJSON, overrides.Revision)
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleDeleteProfileOverrides(w http.ResponseWriter, r *http.Request) {
	if !s.requireProfileOverrides(w, r) {
		return
	}
	revision, err := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if err != nil || revision <= 0 {
		validationFailed(w, r, "revision", "revision is required")
		return
	}
	overrides, err := s.profileOverrides.DeleteProfileOverrides(r.Context(), "default", revision, currentUserID(r))
	if err != nil {
		s.writeProfileOverrideError(w, r, err)
		return
	}
	s.auditProfileOverrides(r, "delete")
	view, _ := s.profileView(overrides.OverridesJSON, overrides.Revision)
	writeJSON(w, http.StatusOK, view)
}

// modelLookup reads a model from the current catalog, which holds configured
// providers and user models that are not deleted. Validation uses the
// effective parameters so a capability or max_tokens inherited from the
// platform defaults applies to profile overrides too.
func (s *Server) modelLookup() func(string) (app.OverrideModel, bool) {
	if s.models == nil {
		return nil
	}
	return func(id string) (app.OverrideModel, bool) {
		model, ok := s.models.Resolve(id)
		return app.OverrideModel{Unavailable: model.Unavailable, Protocol: model.Protocol, MaxTokens: model.EffectiveParams.MaxTokens,
			AllowReasoningEffortNone: model.EffectiveParams.AllowReasoningEffortNone}, ok
	}
}

func (s *Server) requireProfileOverrides(w http.ResponseWriter, r *http.Request) bool {
	if s.profileOverrides == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "profile overrides are unavailable", nil)
		return false
	}
	return true
}

func (s *Server) auditProfileOverrides(r *http.Request, action string) {
	s.logger.Info("profile overrides changed", "event", "platform_audit", "actor_id", currentUserID(r),
		"target_id", "default", "action", action)
}

func (s *Server) writeProfileOverrideError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrStaleRevision):
		writeError(w, r, http.StatusConflict, "stale_revision", "revision does not match", nil)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, http.StatusBadRequest, "invalid_model", "referenced model does not exist", nil)
	default:
		writeError(w, r, http.StatusBadRequest, "validation_failed", "invalid overrides", nil)
	}
}
