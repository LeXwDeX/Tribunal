package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/config"
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
		AccessUserID: s.projectScopeUserID(r),
	})
	if err != nil {
		return nil, err
	}
	aggregated := aggregateProviderStats(stats.ByModel)
	models := s.models.List()
	views := make([]modelView, 0, len(models))
	for _, model := range models {
		view := s.modelView(model, aggregated[model.ID])
		if s.profileOverrides != nil && strings.HasPrefix(model.ID, "user:") {
			if refs, err := s.profileOverrides.ListProfileModelReferences(r.Context(), model.ID); err == nil {
				for _, ref := range refs {
					view.ReferencedBy = append(view.ReferencedBy, ref.Profile+"/"+ref.Node)
				}
			}
		}
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
		if modelWriteDecodeError(w, r, err) {
			return
		}
		decodeRequestError(w, r, err)
		return
	}
	credential := ""
	if request.APIKey != nil {
		credential = *request.APIKey
	}
	candidate := store.Model{
		DisplayName: request.DisplayName, Protocol: request.Protocol, BaseURL: request.BaseURL,
		Model: request.Model, Params: request.Params, CreatedBy: currentUserID(r),
	}
	if details := modelDetails(candidate); len(details) > 0 {
		writeError(w, r, http.StatusBadRequest, "validation_failed", "invalid model", details)
		return
	}
	model, err := s.modelsStore.CreateModel(r.Context(), candidate, credential)
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
		if modelWriteDecodeError(w, r, err) {
			return
		}
		decodeRequestError(w, r, err)
		return
	}
	if request.Revision == nil || *request.Revision <= 0 {
		validationFailed(w, r, "revision", "revision is required")
		return
	}
	candidate := store.Model{
		ID: id, DisplayName: request.DisplayName, Protocol: request.Protocol, BaseURL: request.BaseURL,
		Model: request.Model, Params: request.Params, UpdatedBy: currentUserID(r),
	}
	if details := modelDetails(candidate); len(details) > 0 {
		writeError(w, r, http.StatusBadRequest, "validation_failed", "invalid model", details)
		return
	}
	// Read only the endpoint columns so a corrupt params_json does not block a
	// full-record replacement that repairs the record.
	stored, err := s.modelsStore.GetModelEndpoint(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "not_found", "model not found", nil)
		return
	}
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "model is unavailable", nil)
		return
	}
	// The stored credential may only be sent to the endpoint it was saved for,
	// matching the draft test rule. An explicit key (including an empty value
	// that clears it) allows the change. A model without a stored key has
	// nothing to forward.
	if request.APIKey == nil && stored.HasAPIKey && endpointChanged(stored.Protocol, stored.BaseURL, candidate.Protocol, candidate.BaseURL) {
		validationFailed(w, r, "api_key", "api_key is required when the endpoint changes")
		return
	}
	updated, err := s.modelsStore.UpdateModel(r.Context(), candidate, *request.Revision, request.APIKey)
	if err != nil {
		s.writeModelError(w, r, err)
		return
	}
	s.auditModel(r, updated.ID, "update", request)
	s.finishModelWrite(w, r, updated.ID)
}

// modelParamsRequest updates individual parameter keys of a saved model. Only
// keys present in params change; an explicit null clears a nullable parameter.
type modelParamsRequest struct {
	Revision *int64                     `json:"revision"`
	Params   map[string]json.RawMessage `json:"params"`
}

func (s *Server) handleUpdateModelParams(w http.ResponseWriter, r *http.Request) {
	if !s.requireModelStore(w, r) {
		return
	}
	id := r.PathValue("id")
	if strings.HasPrefix(id, "config:") {
		writeError(w, r, http.StatusForbidden, "read_only", "configured models are read-only", nil)
		return
	}
	var request modelParamsRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if request.Revision == nil || *request.Revision <= 0 {
		validationFailed(w, r, "revision", "revision is required")
		return
	}
	if len(request.Params) == 0 {
		validationFailed(w, r, "params", "params must include at least one parameter")
		return
	}
	patch, details := decodeModelParamsPatch(request.Params)
	if len(details) > 0 {
		writeError(w, r, http.StatusBadRequest, "validation_failed", "invalid model", details)
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
	candidate := *stored
	candidate.Params = patch.Apply(stored.Params)
	if details := modelDetails(candidate); len(details) > 0 {
		writeError(w, r, http.StatusBadRequest, "validation_failed", "invalid model", details)
		return
	}
	updated, err := s.modelsStore.UpdateModelParams(r.Context(), id, *request.Revision, patch, currentUserID(r))
	if err != nil {
		s.writeModelError(w, r, err)
		return
	}
	// Audit only the changed parameter names; values and credentials stay out.
	s.logger.Info("model changed", "event", "platform_audit", "actor_id", currentUserID(r),
		"target_id", updated.ID, "action", "update_params", "fields", patch.Fields())
	s.finishModelWrite(w, r, updated.ID)
}

// modelWriteDecodeError maps the removed params.price key in a whole-record
// model write to its field path. The strict decoder only reports the leaf
// name, so this is limited to the one removed parameter; every other decode
// failure keeps the generic body error.
func modelWriteDecodeError(w http.ResponseWriter, r *http.Request, err error) bool {
	if !strings.Contains(err.Error(), `unknown field "price"`) {
		return false
	}
	validationFailed(w, r, "params.price", "params.price is not a supported parameter")
	return true
}

// decodeModelParamsPatch turns the request's parameter object into a merge
// patch. It distinguishes an absent key (unchanged) from an explicit null
// (clear) and reports failures per field without echoing values.
func decodeModelParamsPatch(raw map[string]json.RawMessage) (store.ModelParamsPatch, []app.Detail) {
	patch := store.ModelParamsPatch{}
	details := []app.Detail{}
	detail := func(name, message string) {
		details = append(details, app.Detail{Field: "params." + name, Message: "params." + name + " " + message})
	}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := bytes.TrimSpace(raw[name])
		if string(value) == "null" {
			switch name {
			case "temperature":
				patch.Temperature.Set = true
			case "reasoning":
				patch.Reasoning.Set = true
			case "thinking":
				patch.Thinking.Set = true
			case "max_tokens", "context_window", "forced_tool_choice", "strict_role_alternation", "max_concurrency", "rpm",
				"allow_reasoning_effort_none", "preserve_reasoning_content", "output_config_effort", "responses_params":
				detail(name, "must not be null")
			default:
				detail(name, "is not a supported parameter")
			}
			continue
		}
		decode := func(name string, target any) bool {
			if err := json.Unmarshal(value, target); err != nil {
				detail(name, "has an invalid value")
				return false
			}
			return true
		}
		// Object parameters are replaced wholesale: unsubmitted nested fields
		// are written as zero values. Unknown nested keys and null values are
		// rejected so a typo cannot silently reset stored settings.
		decodeObject := func(name string, target any) bool {
			if containsJSONNull(value) {
				detail(name, "must not contain null values; use a top-level null to clear the object")
				return false
			}
			dec := json.NewDecoder(bytes.NewReader(value))
			dec.DisallowUnknownFields()
			if err := dec.Decode(target); err != nil {
				detail(name, "must be an object with only known fields")
				return false
			}
			var extra any
			if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
				detail(name, "must be a single object")
				return false
			}
			return true
		}
		switch name {
		case "max_tokens":
			var v int
			if decode(name, &v) {
				patch.MaxTokens = &v
			}
		case "context_window":
			var v int
			if decode(name, &v) {
				patch.ContextWindow = &v
			}
		case "max_concurrency":
			var v int
			if decode(name, &v) {
				patch.MaxConcurrency = &v
			}
		case "rpm":
			var v int
			if decode(name, &v) {
				patch.RPM = &v
			}
		case "temperature":
			var v float64
			if decode(name, &v) {
				patch.Temperature = store.ParamUpdate[float64]{Set: true, Value: &v}
			}
		case "reasoning":
			var v config.ReasoningConfig
			if decodeObject(name, &v) {
				patch.Reasoning = store.ParamUpdate[config.ReasoningConfig]{Set: true, Value: &v}
			}
		case "thinking":
			var v config.ThinkingConfig
			if decodeObject(name, &v) {
				patch.Thinking = store.ParamUpdate[config.ThinkingConfig]{Set: true, Value: &v}
			}
		case "forced_tool_choice":
			var v string
			if decode(name, &v) {
				patch.ForcedToolChoice = &v
			}
		case "strict_role_alternation":
			var v bool
			if decode(name, &v) {
				patch.StrictRoleAlternation = &v
			}
		case "allow_reasoning_effort_none":
			var v bool
			if decode(name, &v) {
				patch.AllowReasoningEffortNone = &v
			}
		case "preserve_reasoning_content":
			var v bool
			if decode(name, &v) {
				patch.PreserveReasoningContent = &v
			}
		case "output_config_effort":
			var v bool
			if decode(name, &v) {
				patch.OutputConfigEffort = &v
			}
		case "responses_params":
			var v string
			if decode(name, &v) {
				patch.ResponsesParams = &v
			}
		default:
			detail(name, "is not a supported parameter")
		}
	}
	return patch, details
}

// containsJSONNull reports whether raw contains a JSON null token at any
// depth. Only a top-level null clears a nullable parameter object.
func containsJSONNull(raw json.RawMessage) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		token, err := dec.Token()
		if err != nil {
			return false
		}
		if token == nil {
			return true
		}
	}
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
		if errors.Is(err, store.ErrConflict) && s.profileOverrides != nil {
			details := []app.Detail{}
			if refs, refErr := s.profileOverrides.ListProfileModelReferences(r.Context(), id); refErr == nil {
				for _, ref := range refs {
					details = append(details, app.Detail{Field: "references", Message: ref.Profile + "/" + ref.Node})
				}
			}
			writeError(w, r, http.StatusConflict, "referenced", "model is referenced", details)
			return
		}
		s.writeModelError(w, r, err)
		return
	}
	s.auditModel(r, id, "delete", modelWriteRequest{})
	s.finishModelWrite(w, r, id)
}

func (s *Server) handleTestModel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.PathValue("id"), "config:")
	if s.llm == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "provider testing is unavailable", nil)
		return
	}
	result, err := s.llm.TestProvider(r.Context(), id)
	if err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "provider_test_failed", "provider test failed", nil)
		return
	}
	writeJSON(w, http.StatusOK, modelTestResponse{OK: true, Model: result.Model, LatencyMS: result.LatencyMS})
}

// draftTestTimeout bounds one draft probe, including connection setup.
const draftTestTimeout = 30 * time.Second

type draftModelRequest struct {
	modelWriteRequest
	ID string `json:"id"`
}

type modelTestResponse struct {
	OK        bool   `json:"ok"`
	Model     string `json:"model"`
	LatencyMS int64  `json:"latency_ms"`
}

// handleTestModelDraft probes an unsaved model definition. Nothing is stored,
// the catalog is not refreshed, and the probe records no llm_calls.
func (s *Server) handleTestModelDraft(w http.ResponseWriter, r *http.Request) {
	if !s.requireModelStore(w, r) {
		return
	}
	var request draftModelRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	model := store.Model{
		DisplayName: request.DisplayName, Protocol: request.Protocol, BaseURL: request.BaseURL,
		Model: request.Model, Params: request.Params,
	}
	if details := modelDetails(model); len(details) > 0 {
		writeError(w, r, http.StatusBadRequest, "validation_failed", "invalid model", details)
		return
	}
	credential := ""
	storedCredential := false
	var stored *store.Model
	if request.ID != "" {
		if !strings.HasPrefix(request.ID, "user:") {
			validationFailed(w, r, "id", "only saved user models can be referenced")
			return
		}
		var err error
		stored, err = s.modelsStore.GetModel(r.Context(), request.ID)
		if err == nil && stored.DeletedAt != nil {
			err = store.ErrNotFound
		}
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "model not found", nil)
			return
		}
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "unavailable", "model is unavailable", nil)
			return
		}
	}
	switch {
	case request.APIKey != nil:
		credential = *request.APIKey
	case stored != nil:
		// The stored key may only be sent to the endpoint it was saved for.
		if endpointChanged(stored.Protocol, stored.BaseURL, model.Protocol, model.BaseURL) {
			validationFailed(w, r, "api_key", "api_key is required when the endpoint changes")
			return
		}
		key, err := s.modelsStore.OpenModelCredential(r.Context(), request.ID)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "model not found", nil)
			return
		}
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "unavailable", "model credential is unavailable", nil)
			return
		}
		credential = string(key)
		storedCredential = true
	}
	s.logger.Info("model changed", "event", "platform_audit", "actor_id", currentUserID(r),
		"target_id", request.ID, "action", "test_draft", "fields", modelAuditFields(request.modelWriteRequest),
		"stored_credential", storedCredential, "host", endpointHost(model.BaseURL))
	client, closeClient, err := s.models.NewDraftClient(model, credential)
	if err == nil {
		defer closeClient()
		ctx, cancel := context.WithTimeout(r.Context(), draftTestTimeout)
		defer cancel()
		var result app.ProviderTestResult
		if result, err = app.ProbeProvider(ctx, client); err == nil {
			writeJSON(w, http.StatusOK, modelTestResponse{OK: true, Model: result.Model, LatencyMS: result.LatencyMS})
			return
		}
	}
	if errors.Is(err, llm.ErrEgressDenied) {
		writeError(w, r, http.StatusUnprocessableEntity, "egress_denied", "provider destination denied", nil)
		return
	}
	writeError(w, r, http.StatusUnprocessableEntity, "provider_test_failed", "provider test failed", nil)
}

// modelDetails applies the stored-model validation used by create, update and
// draft test, and reports failures per field without echoing secrets.
func modelDetails(model store.Model) []app.Detail {
	details := []app.Detail{}
	if n := utf8.RuneCountInString(model.DisplayName); n < 1 || n > 128 || strings.TrimSpace(model.DisplayName) == "" ||
		strings.IndexFunc(model.DisplayName, unicode.IsControl) >= 0 {
		details = append(details, app.Detail{Field: "display_name", Message: "display_name must be 1-128 printable characters"})
	}
	err := config.ValidateProviderConfig("model", llm.ModelConfig(model), config.ProviderValidationOptions{})
	if err == nil {
		return details
	}
	var joined interface{ Unwrap() []error }
	problems := []error{err}
	if errors.As(err, &joined) {
		problems = joined.Unwrap()
	}
	for _, problem := range problems {
		message := strings.TrimPrefix(problem.Error(), "model.")
		field, _, _ := strings.Cut(message, " ")
		switch field {
		case "protocol", "base_url", "model":
		default:
			field = "params." + field
		}
		details = append(details, app.Detail{Field: field, Message: message})
	}
	return details
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
	s.logger.Info("model changed", "event", "platform_audit", "actor_id", currentUserID(r),
		"target_id", id, "action", action, "fields", modelAuditFields(request))
}

func modelAuditFields(request modelWriteRequest) []string {
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
	return fields
}

// endpointChanged reports whether a requested model definition points at a
// different protocol or endpoint (scheme, host and port) than the stored one.
// A path-only change keeps the stored credential reusable.
func endpointChanged(storedProtocol, storedBaseURL, requestedProtocol, requestedBaseURL string) bool {
	return storedProtocol != requestedProtocol || !sameEndpoint(storedBaseURL, requestedBaseURL)
}

// sameEndpoint reports whether two base URLs share scheme, host and port.
func sameEndpoint(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Hostname(), ub.Hostname()) &&
		endpointPort(ua) == endpointPort(ub)
}

func endpointPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// endpointHost returns host[:port] only, for audit logs.
func endpointHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
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
