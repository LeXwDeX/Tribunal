package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/errclass"
)

const configOverlayRestartNotice = "Saved to the overlay file. Restart the service to apply it; restarting may cancel in-flight reviews."

type configOverlayPatchRequest struct {
	BaseRevision    string                     `json:"base_revision"`
	OverlayRevision string                     `json:"overlay_revision"`
	DryRun          bool                       `json:"dry_run"`
	Changes         []configOverlayPatchChange `json:"changes"`
}

type configOverlayPatchChange struct {
	Path   []string        `json:"path"`
	Value  json.RawMessage `json:"value"`
	Delete bool            `json:"delete"`
}

type configOverlayWriteResponse struct {
	DryRun                  bool                     `json:"dry_run"`
	Applied                 bool                     `json:"applied"`
	Changed                 bool                     `json:"changed"`
	BaseRevision            string                   `json:"base_revision"`
	PreviousOverlayRevision string                   `json:"previous_overlay_revision"`
	OverlayRevision         string                   `json:"overlay_revision"`
	RestartRequired         bool                     `json:"restart_required"`
	Notice                  string                   `json:"notice,omitempty"`
	ReviewDefaults          configReviewDefaultsView `json:"review_defaults"`
}

// configReviewDefaultsView shows raw overlay values and the inherited ignore
// list. It never contains expanded environment values, so the write response
// cannot be used to read environment variables.
type configReviewDefaultsView struct {
	Overlay         map[string]any `json:"overlay"`
	EffectiveIgnore []string       `json:"effective_ignore"`
	IgnoreOrigin    string         `json:"ignore_origin"`
	BuiltinIgnore   []string       `json:"builtin_ignore"`
}

// configOverlayAdmin logs a rejected platform-admin check before delegating to
// the standard guard, which still makes the authorization decision.
func (s *Server) configOverlayAdmin(next http.HandlerFunc) http.HandlerFunc {
	guarded := s.guard(policyPlatformAdmin, next)
	return func(w http.ResponseWriter, r *http.Request) {
		if value, ok := principalFrom(r.Context()); !ok || value.user.Role != roleAdmin {
			s.auditConfigOverlayRejected(r, nil, "forbidden", false)
		}
		guarded(w, r)
	}
}

// handleConfigOverlayPatch applies allowlisted leaf changes to the overlay
// file. The static bootstrap bearer cannot use it because that token is not
// bound to a person and has no CSRF protection.
func (s *Server) handleConfigOverlayPatch(w http.ResponseWriter, r *http.Request) {
	if value, ok := principalFrom(r.Context()); ok && value.bootstrapBearer {
		s.auditConfigOverlayRejected(r, nil, "forbidden", false)
		writeError(w, r, http.StatusForbidden, "forbidden", "the static bootstrap token cannot write configuration", nil)
		return
	}
	var request configOverlayPatchRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		s.auditConfigOverlayRejected(r, nil, decodeRequestErrorCode(err), false)
		decodeRequestError(w, r, err)
		return
	}
	changes, err := request.overlayChanges()
	if err != nil {
		s.auditConfigOverlayRejectedPaths(r, configOverlayRequestPaths(request), config.OverlayErrorInvalidChange, request.DryRun)
		writeError(w, r, http.StatusBadRequest, config.OverlayErrorInvalidChange, err.Error(), nil)
		return
	}
	if s.overlayWriter == nil {
		s.auditConfigOverlayRejected(r, changes, config.OverlayErrorNotConfigured, request.DryRun)
		writeError(w, r, http.StatusConflict, config.OverlayErrorNotConfigured, "configuration overlay is not configured", nil)
		return
	}
	result, err := s.overlayWriter.Apply(r.Context(), config.OverlayWriteRequest{
		BaseRevision:          request.BaseRevision,
		OverlayRevision:       request.OverlayRevision,
		Changes:               changes,
		DryRun:                request.DryRun,
		CheckRemovedProviders: s.checkOverlayProviderReferences,
	})
	if err != nil {
		s.auditConfigOverlayRejected(r, changes, configOverlayErrorCode(err), request.DryRun)
		s.writeConfigOverlayError(w, r, err)
		return
	}
	if !result.DryRun {
		s.auditConfigOverlay(r, changes, result)
	}
	response := configOverlayWriteResponse{
		DryRun:                  result.DryRun,
		Applied:                 result.Applied,
		Changed:                 result.Changed,
		BaseRevision:            result.BaseRevision,
		PreviousOverlayRevision: result.PreviousOverlayRevision,
		OverlayRevision:         result.OverlayRevision,
		// A no-op write changed nothing, so there is nothing to restart for.
		RestartRequired: result.Applied && result.Changed,
		ReviewDefaults:  configReviewDefaultsViewFor(result),
	}
	if response.RestartRequired {
		response.Notice = configOverlayRestartNotice
	}
	writeJSON(w, http.StatusOK, response)
}

// overlayChanges converts the transport shape into config changes. An absent
// value and an explicit null are distinct: null clears a nullable leaf.
func (request configOverlayPatchRequest) overlayChanges() ([]config.OverlayChange, error) {
	changes := make([]config.OverlayChange, 0, len(request.Changes))
	for i, change := range request.Changes {
		if change.Delete {
			if len(change.Value) != 0 {
				return nil, fmt.Errorf("changes[%d]: delete must not include a value", i)
			}
			changes = append(changes, config.OverlayChange{Path: change.Path, Delete: true})
			continue
		}
		if len(change.Value) == 0 {
			return nil, fmt.Errorf("changes[%d]: value or delete is required", i)
		}
		value, err := decodeConfigOverlayValue(change.Value)
		if err != nil {
			return nil, fmt.Errorf("changes[%d]: invalid JSON value", i)
		}
		changes = append(changes, config.OverlayChange{Path: change.Path, Value: value})
	}
	return changes, nil
}

func decodeConfigOverlayValue(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

func decodeRequestErrorCode(err error) string {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return "bad_request"
	}
	return "validation_failed"
}

func configOverlayErrorCode(err error) string {
	var writeErr *config.OverlayWriteError
	if errors.As(err, &writeErr) {
		return writeErr.Code
	}
	return "internal"
}

func (s *Server) writeConfigOverlayError(w http.ResponseWriter, r *http.Request, err error) {
	var writeErr *config.OverlayWriteError
	if !errors.As(err, &writeErr) {
		s.logger.Warn("configuration overlay write failed", "error_class", errclass.Of(err))
		writeError(w, r, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	status := http.StatusInternalServerError
	switch writeErr.Code {
	case config.OverlayErrorInvalidChange, config.OverlayErrorInvalidConfig:
		status = http.StatusBadRequest
	case config.OverlayErrorNotConfigured, config.OverlayErrorReadOnly,
		config.OverlayErrorSymlink, config.OverlayErrorStaleConfig,
		config.OverlayErrorProviderInUse, config.OverlayErrorPathConflict,
		config.OverlayErrorBusy:
		status = http.StatusConflict
	case config.OverlayErrorTooLarge:
		status = http.StatusRequestEntityTooLarge
	case config.OverlayErrorWriteFailed:
		status = http.StatusInternalServerError
	}
	details := make([]app.Detail, 0, len(writeErr.Details))
	for _, detail := range writeErr.Details {
		details = append(details, app.Detail{Field: "changes", Message: detail})
	}
	writeError(w, r, status, writeErr.Code, writeErr.Message, details)
}

// checkOverlayProviderReferences rejects deleting a provider that a stored
// profile override still names. The overrides are not modified.
func (s *Server) checkOverlayProviderReferences(ctx context.Context, providers []string) error {
	if s.profileOverrides == nil || len(providers) == 0 {
		return nil
	}
	overrides, err := s.profileOverrides.GetProfileOverrides(ctx, "default")
	if err != nil {
		return err
	}
	names := make(map[string]bool, len(providers))
	for _, provider := range providers {
		names[provider] = true
	}
	references := app.OverrideProviderReferences(overrides.OverridesJSON, names)
	if len(references) == 0 {
		return nil
	}
	return &config.OverlayWriteError{
		Code:    config.OverlayErrorProviderInUse,
		Message: "the provider is referenced by stored profile overrides",
		Details: references,
	}
}

// auditConfigOverlay records actor, paths and revisions after a write. Values
// never reach the audit log.
func (s *Server) auditConfigOverlay(r *http.Request, changes []config.OverlayChange, result *config.OverlayWriteResult) {
	message := "configuration overlay unchanged"
	if result.Changed {
		message = "configuration overlay changed"
	}
	s.logger.Info(message, "event", "platform_audit", "actor_id", currentUserID(r),
		"action", "update", "paths", configOverlayPaths(changes),
		"base_revision", result.BaseRevision,
		"previous_overlay_revision", result.PreviousOverlayRevision,
		"overlay_revision", result.OverlayRevision,
		"changed", result.Changed)
}

// auditConfigOverlayRejected records a rejected write with its stable code.
// Paths are only logged after request conversion, so arbitrary raw request
// strings never reach the log; values are never logged.
func (s *Server) auditConfigOverlayRejected(r *http.Request, changes []config.OverlayChange, code string, dryRun bool) {
	s.auditConfigOverlayRejectedPaths(r, configOverlayPaths(changes), code, dryRun)
}

func (s *Server) auditConfigOverlayRejectedPaths(r *http.Request, paths []string, code string, dryRun bool) {
	s.logger.Info("configuration overlay write rejected", "event", "platform_audit", "actor_id", currentUserID(r),
		"action", "update", "paths", paths, "code", code, "dry_run", dryRun)
}

func configOverlayPaths(changes []config.OverlayChange) []string {
	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		paths = append(paths, strings.Join(change.Path, "."))
	}
	return paths
}

// configOverlayRequestPaths returns only paths that are safe to log before
// validation: bounded, non-empty, and free of control characters.
func configOverlayRequestPaths(request configOverlayPatchRequest) []string {
	paths := make([]string, 0, len(request.Changes))
	for _, change := range request.Changes {
		if len(change.Path) == 0 || len(change.Path) > 8 {
			continue
		}
		valid := true
		for _, segment := range change.Path {
			if segment == "" || len(segment) > 128 || strings.ContainsAny(segment, "\x00\n\r\t") {
				valid = false
				break
			}
		}
		if valid {
			paths = append(paths, strings.Join(change.Path, "."))
		}
	}
	return paths
}

func configReviewDefaultsViewFor(result *config.OverlayWriteResult) configReviewDefaultsView {
	view := configReviewDefaultsView{
		Overlay:         result.OverlayReviewDefaults,
		EffectiveIgnore: result.EffectiveIgnore,
		BuiltinIgnore:   config.DefaultReviewIgnorePatterns(),
	}
	if view.EffectiveIgnore == nil {
		view.EffectiveIgnore = []string{}
	}
	if result.Candidate != nil {
		view.IgnoreOrigin = string(result.Candidate.Origins["review_defaults.ignore"])
	}
	return view
}
