package server

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type modelView struct {
	ID                string            `json:"id"`
	DisplayName       string            `json:"display_name"`
	Source            string            `json:"source"`
	ReadOnly          bool              `json:"read_only"`
	Protocol          string            `json:"protocol"`
	BaseURL           string            `json:"base_url"`
	Model             string            `json:"model"`
	Params            store.ModelParams `json:"params"`
	HasAPIKey         bool              `json:"has_api_key"`
	Available         bool              `json:"available"`
	UnavailableReason string            `json:"unavailable_reason,omitempty"`
	Revision          int64             `json:"revision"`
	ReferencedBy      []string          `json:"referenced_by"`
	VoteIdentity      string            `json:"vote_identity"`
	Stats24h          providerStatsView `json:"stats_24h"`
}

type modelWriteRequest struct {
	DisplayName string            `json:"display_name"`
	Protocol    string            `json:"protocol"`
	BaseURL     string            `json:"base_url"`
	Model       string            `json:"model"`
	Params      store.ModelParams `json:"params"`
	APIKey      *string           `json:"api_key"`
	Revision    *int64            `json:"revision"`
}

func (s *Server) modelViews(r *http.Request) ([]modelView, error) {
	if s.models == nil || s.store == nil {
		return nil, errors.New("model catalog is not configured")
	}
	now := s.now()
	stats, err := s.store.Stats(r.Context(), store.StatsFilter{
		From: now.Add(-24 * time.Hour), To: now,
		AccessUserID: accessUserIDPtr(currentUserID(r)),
	})
	if err != nil {
		return nil, err
	}
	aggregated := aggregateProviderStats(stats.ByModel)
	models := s.models.List()
	views := make([]modelView, 0, len(models))
	for _, model := range models {
		view := s.modelView(model, aggregated[model.ID])
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	return views, nil
}

func (s *Server) modelView(model llm.CatalogModel, stats providerStatsView) modelView {
	source := "user"
	readOnly := false
	if !strings.HasPrefix(model.ID, "user:") {
		source = "config"
		readOnly = true
	}
	identity := ""
	if client, err := s.models.Client(model.ID); err == nil {
		identity = client.Info().ModelIdentity()
	}
	return modelView{
		ID:                model.ID,
		DisplayName:       model.DisplayName,
		Source:            source,
		ReadOnly:          readOnly,
		Protocol:          model.Protocol,
		BaseURL:           model.BaseURL,
		Model:             model.Model.Model,
		Params:            model.Params,
		HasAPIKey:         model.HasAPIKey,
		Available:         !model.Unavailable,
		UnavailableReason: model.UnavailableReason,
		Revision:          model.Revision,
		ReferencedBy:      []string{},
		VoteIdentity:      identity,
		Stats24h:          stats,
	}
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	views, err := s.modelViews(r)
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "model catalog is unavailable", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": views})
}

func (s *Server) handleGetModel(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "model catalog is unavailable", nil)
		return
	}
	model, ok := s.models.Resolve(r.PathValue("id"))
	if !ok {
		writeError(w, r, http.StatusNotFound, "not_found", "model not found", nil)
		return
	}
	views, err := s.modelViews(r)
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "model catalog is unavailable", nil)
		return
	}
	for _, view := range views {
		if view.ID == model.ID {
			writeJSON(w, http.StatusOK, view)
			return
		}
	}
	writeError(w, r, http.StatusNotFound, "not_found", "model not found", nil)
}

func (s *Server) handleCreateModel(w http.ResponseWriter, r *http.Request) {
	if !s.requireModelStore(w, r) {
		return
	}
	var request modelWriteRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	credential := ""
	if request.APIKey != nil {
		credential = *request.APIKey
	}
	model, err := s.modelsStore.CreateModel(r.Context(), store.Model{
		DisplayName: request.DisplayName, Protocol: request.Protocol, BaseURL: request.BaseURL,
		Model: request.Model, Params: request.Params, CreatedBy: currentUserID(r),
	}, credential)
	if err != nil {
		s.writeModelError(w, r, err)
		return
	}
	s.auditModel(r, model.ID, "create", request)
	s.finishModelWrite(w, r, model.ID)
}

func (s *Server) handleUpdateModel(w http.ResponseWriter, r *http.Request) {
	if !s.requireModelStore(w, r) {
		return
	}
	id := r.PathValue("id")
	if strings.HasPrefix(id, "config:") {
		writeError(w, r, http.StatusForbidden, "read_only", "configured models are read-only", nil)
		return
	}
	var request modelWriteRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if request.Revision == nil || *request.Revision <= 0 {
		validationFailed(w, r, "revision", "revision is required")
		return
	}
	updated, err := s.modelsStore.UpdateModel(r.Context(), store.Model{
		ID: id, DisplayName: request.DisplayName, Protocol: request.Protocol, BaseURL: request.BaseURL,
		Model: request.Model, Params: request.Params, UpdatedBy: currentUserID(r),
	}, *request.Revision, request.APIKey)
	if err != nil {
		s.writeModelError(w, r, err)
		return
	}
	s.auditModel(r, updated.ID, "update", request)
	s.finishModelWrite(w, r, updated.ID)
}

func (s *Server) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	if !s.requireModelStore(w, r) {
		return
	}
	id := r.PathValue("id")
	if strings.HasPrefix(id, "config:") {
		writeError(w, r, http.StatusForbidden, "read_only", "configured models are read-only", nil)
		return
	}
	revision, err := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if err != nil || revision <= 0 {
		validationFailed(w, r, "revision", "revision is required")
		return
	}
	if err := s.modelsStore.SoftDeleteModel(r.Context(), id, revision, currentUserID(r)); err != nil {
		s.writeModelError(w, r, err)
		return
	}
	s.auditModel(r, id, "delete", modelWriteRequest{})
	s.finishModelWrite(w, r, id)
}

func (s *Server) handleTestModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.HasPrefix(id, "config:") {
		id = strings.TrimPrefix(id, "config:")
	}
	if s.llm == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "provider testing is unavailable", nil)
		return
	}
	result, err := s.llm.TestProvider(r.Context(), id)
	if err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "provider_test_failed", "provider test failed", nil)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleTestModelDraft(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotImplemented, "not_implemented", "draft model testing is not available", nil)
}

func (s *Server) requireModelStore(w http.ResponseWriter, r *http.Request) bool {
	if s.models == nil || s.modelsStore == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "model catalog is unavailable", nil)
		return false
	}
	return true
}

func (s *Server) finishModelWrite(w http.ResponseWriter, r *http.Request, id string) {
	if s.refreshModels != nil {
		if err := s.refreshModels(r.Context()); err != nil {
			s.logger.Error("refresh models failed", "event", "model_refresh_failed", "model_id", id)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "effective": "new_workflows"})
}

func (s *Server) auditModel(r *http.Request, id, action string, request modelWriteRequest) {
	fields := make([]string, 0, 6)
	for name, changed := range map[string]bool{
		"display_name": request.DisplayName != "", "protocol": request.Protocol != "",
		"base_url": request.BaseURL != "", "model": request.Model != "",
		"params": true, "api_key": request.APIKey != nil,
	} {
		if changed {
			fields = append(fields, name)
		}
	}
	sort.Strings(fields)
	s.logger.Info("model changed", "event", "platform_audit", "actor_id", currentUserID(r),
		"target_id", id, "action", action, "fields", fields)
}

func (s *Server) writeModelError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrStaleRevision):
		writeError(w, r, http.StatusConflict, "stale_revision", "revision does not match", nil)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "model not found", nil)
	default:
		writeError(w, r, http.StatusBadRequest, "validation_failed", "invalid model", nil)
	}
}
