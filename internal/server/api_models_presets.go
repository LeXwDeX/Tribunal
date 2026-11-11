package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// modelPresetRequest applies one shipped preset to a saved user model. A
// request previews by default; execution requires an explicit dry_run false,
// the current revision and confirmation when profile node overrides conflict.
type modelPresetRequest struct {
	Preset           string `json:"preset"`
	Revision         *int64 `json:"revision"`
	DryRun           *bool  `json:"dry_run"`
	ConfirmOverrides bool   `json:"confirm_overrides"`
}

// modelPresetConflict names a stored profile node override that shadows a
// preset parameter. Values are not returned.
type modelPresetConflict struct {
	Profile string   `json:"profile"`
	Node    string   `json:"node"`
	Source  string   `json:"source"`
	Fields  []string `json:"fields"`
}

type modelPresetResponse struct {
	ModelID   string                `json:"model_id"`
	Preset    string                `json:"preset"`
	DryRun    bool                  `json:"dry_run"`
	Applied   bool                  `json:"applied"`
	Revision  int64                 `json:"revision"`
	Changes   []llm.PresetChange    `json:"changes"`
	Conflicts []modelPresetConflict `json:"conflicts"`
	Effective string                `json:"effective,omitempty"`
}

func (s *Server) handleListModelPresets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"presets": llm.Presets()})
}

// handleApplyModelPreset previews or applies a preset to a user model. It
// merges only the parameters the preset owns, under the model revision lock;
// the endpoint, model name, unchanged parameters and the saved credential are
// never part of the update.
func (s *Server) handleApplyModelPreset(w http.ResponseWriter, r *http.Request) {
	if !s.requireModelStore(w, r) {
		return
	}
	id := r.PathValue("id")
	if !strings.HasPrefix(id, "user:") {
		writeError(w, r, http.StatusForbidden, "read_only", "only saved user models accept a preset", nil)
		return
	}
	var request modelPresetRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	preset, ok := llm.Preset(request.Preset)
	if !ok {
		validationFailed(w, r, "preset", "preset is not supported")
		return
	}
	dryRun := request.DryRun == nil || *request.DryRun
	if !dryRun && (request.Revision == nil || *request.Revision <= 0) {
		validationFailed(w, r, "revision", "revision is required")
		return
	}
	stored, err := s.modelsStore.GetModel(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && stored.DeletedAt != nil) {
		writeError(w, r, http.StatusNotFound, "not_found", "model not found", nil)
		return
	}
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "model is unavailable", nil)
		return
	}
	patch, changes := preset.PatchFor(stored.Params)
	conflicts, err := s.presetConflicts(r, id)
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "profile overrides are unavailable", nil)
		return
	}
	response := modelPresetResponse{
		ModelID: id, Preset: preset.ID, DryRun: dryRun, Revision: stored.Revision,
		Changes: changes, Conflicts: conflicts,
	}
	// A preview always succeeds; an execution with nothing to change is not an
	// error and does not bump the revision.
	if dryRun || len(changes) == 0 {
		writeJSON(w, http.StatusOK, response)
		return
	}
	// Stored node overrides are reported and kept. Applying the preset with
	// them requires explicit confirmation; it never clears them.
	if len(conflicts) > 0 && !request.ConfirmOverrides {
		writeError(w, r, http.StatusConflict, "override_conflict", "preset conflicts with profile node overrides", presetConflictDetails(conflicts))
		return
	}
	updated, err := s.modelsStore.UpdateModelParams(r.Context(), id, *request.Revision, patch, currentUserID(r))
	if err != nil {
		s.writeModelError(w, r, err)
		return
	}
	// Audit only the preset ID and the changed parameter names; values and
	// credentials stay out.
	s.logger.Info("model changed", "event", "platform_audit", "actor_id", currentUserID(r),
		"target_id", id, "action", "apply_preset", "preset", preset.ID, "fields", patch.Fields())
	if s.refreshModels != nil {
		if err := s.refreshModels(r.Context()); err != nil {
			s.logger.Error("refresh models failed", "event", "model_refresh_failed", "model_id", id)
		}
	}
	response.Applied = true
	response.Revision = updated.Revision
	response.Effective = "new_workflows"
	writeJSON(w, http.StatusOK, response)
}

// presetConflicts collects the stored default profile node overrides that
// currently shadow a preset parameter on nodes bound to the model. It reads
// the same merged view the review uses; an unreadable override document is an
// error instead of an empty result.
func (s *Server) presetConflicts(r *http.Request, modelID string) ([]modelPresetConflict, error) {
	conflicts := []modelPresetConflict{}
	if s.profileOverrides == nil || s.platform == nil {
		return conflicts, nil
	}
	normalized, ok := s.defaultProfile()
	if !ok {
		return conflicts, nil
	}
	overrides, err := s.profileOverrides.GetProfileOverrides(r.Context(), "default")
	if err != nil {
		return nil, err
	}
	if overrides == nil || strings.TrimSpace(overrides.OverridesJSON) == "" {
		return conflicts, nil
	}
	raw, _ := s.platform.RawProfile("default")
	resolved := app.ResolveProfile(raw, normalized, overrides.OverridesJSON)
	for _, node := range resolved.Nodes {
		if node.Node == "default" || node.Model != modelID || node.Params == nil {
			continue
		}
		fields := []string{}
		for _, field := range llm.PresetNodeOverrideFields() {
			switch field {
			case "max_tokens":
				if node.Params.MaxTokens != nil {
					fields = append(fields, field)
				}
			case "reasoning":
				if node.Params.Reasoning != nil && node.Params.Reasoning.Effort != "" {
					fields = append(fields, field)
				}
			case "thinking":
				if node.Params.Thinking != nil && (node.Params.Thinking.BudgetTokens != 0 || node.Params.Thinking.Type != "") {
					fields = append(fields, field)
				}
			}
		}
		if len(fields) == 0 {
			continue
		}
		conflicts = append(conflicts, modelPresetConflict{
			Profile: "default", Node: node.Node, Source: node.ParamSource, Fields: fields,
		})
	}
	return conflicts, nil
}

// presetConflictDetails renders conflicts as field-level details without
// parameter values.
func presetConflictDetails(conflicts []modelPresetConflict) []app.Detail {
	details := make([]app.Detail, 0, len(conflicts))
	for _, conflict := range conflicts {
		details = append(details, app.Detail{
			Field:   "overrides." + conflict.Node,
			Message: "node overrides " + strings.Join(conflict.Fields, ", "),
		})
	}
	return details
}
