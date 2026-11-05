package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type profileOverrideView struct {
	Profile   string                `json:"profile"`
	Overrides json.RawMessage       `json:"overrides"`
	Revision  int64                 `json:"revision"`
	Effective config.ProfileConfig  `json:"effective"`
	Nodes     []app.ProfileNodeView `json:"nodes"`
}

func (s *Server) defaultProfile() (config.ProfileConfig, bool) {
	if s.platform == nil {
		return config.ProfileConfig{}, false
	}
	profile, ok := s.platform.Profiles["default"]
	return profile, ok
}

func (s *Server) handleGetDefaultProfile(w http.ResponseWriter, r *http.Request) {
	profile, ok := s.defaultProfile()
	if !ok {
		writeError(w, r, http.StatusNotFound, "not_found", "profile not found", nil)
		return
	}
	body := []byte(`{}`)
	var revision int64
	if s.profileOverrides != nil {
		overrides, err := s.profileOverrides.GetProfileOverrides(r.Context(), "default")
		if err == nil && overrides != nil {
			body = []byte(overrides.OverridesJSON)
			revision = overrides.Revision
		}
	}
	writeJSON(w, http.StatusOK, profileOverrideView{
		Profile: "default", Overrides: body, Revision: revision,
		Effective: app.ApplyProfileOverrides(profile, string(body)),
		Nodes:     app.ResolveProfileNodes(profile, string(body)),
	})
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
	overrides, err := s.profileOverrides.PutProfileOverrides(r.Context(), "default", string(request.Overrides), request.Revision, currentUserID(r))
	if err != nil {
		s.writeProfileOverrideError(w, r, err)
		return
	}
	s.auditProfileOverrides(r, "update")
	profile, _ := s.defaultProfile()
	writeJSON(w, http.StatusOK, profileOverrideView{
		Profile: "default", Overrides: []byte(overrides.OverridesJSON), Revision: overrides.Revision,
		Effective: app.ApplyProfileOverrides(profile, overrides.OverridesJSON),
		Nodes:     app.ResolveProfileNodes(profile, overrides.OverridesJSON),
	})
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
	if err := s.profileOverrides.DeleteProfileOverrides(r.Context(), "default", revision); err != nil {
		s.writeProfileOverrideError(w, r, err)
		return
	}
	s.auditProfileOverrides(r, "delete")
	profile, _ := s.defaultProfile()
	writeJSON(w, http.StatusOK, profileOverrideView{
		Profile: "default", Overrides: []byte(`{}`), Revision: revision + 1,
		Effective: profile,
	})
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
