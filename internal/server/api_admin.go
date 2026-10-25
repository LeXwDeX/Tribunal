package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/app"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

// adminProjectView is the minimal management-plane project list. It
// deliberately omits settings, summaries and repository code.
type adminProjectView struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
}

func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccess(w, r) {
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		validationFailed(w, r, "limit", err.Error())
		return
	}
	users, next, err := s.access.ListUsers(r.Context(), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.writeAccessError(w, r, err)
		return
	}
	if users == nil {
		users = []store.User{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "next_cursor": next})
}

func (s *Server) handleAdminSetUserDisabled(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccess(w, r) {
		return
	}
	userID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	var request struct {
		Disabled *bool `json:"disabled"`
	}
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if request.Disabled == nil {
		validationFailed(w, r, "disabled", "disabled is required")
		return
	}
	if err := s.access.SetUserDisabled(r.Context(), userID, *request.Disabled); err != nil {
		s.writeAccessError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": userID, "disabled": *request.Disabled})
}

func (s *Server) handleAdminRevokeCredentials(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccess(w, r) {
		return
	}
	userID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	if err := s.access.RevokeUserCredentials(r.Context(), userID); err != nil {
		s.writeAccessError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminListProjects returns only the registered project identity. It is
// the management view used to pick a project before editing grants.
func (s *Server) handleAdminListProjects(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccess(w, r) {
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		validationFailed(w, r, "limit", err.Error())
		return
	}
	projects, next, err := s.projects.ListFilteredPage(r.Context(),
		app.ProjectListFilter{}, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	items := make([]adminProjectView, 0, len(projects))
	for _, project := range projects {
		items = append(items, adminProjectView{ID: project.ID, PathWithNamespace: project.PathWithNamespace})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": items, "next_cursor": next})
}

func (s *Server) handleAdminListGrants(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccess(w, r) {
		return
	}
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		validationFailed(w, r, "limit", err.Error())
		return
	}
	grants, next, err := s.access.ListProjectGrants(r.Context(), projectID, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.writeAccessError(w, r, err)
		return
	}
	if grants == nil {
		grants = []store.ProjectGrant{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": grants, "next_cursor": next})
}

func (s *Server) handleAdminSetGrant(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccess(w, r) {
		return
	}
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	userID, err := parseID(r.PathValue("user_id"))
	if err != nil {
		validationFailed(w, r, "user_id", "user_id must be a positive integer")
		return
	}
	var request struct {
		Role string `json:"role"`
	}
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	role := strings.TrimSpace(request.Role)
	if role != store.ProjectAccessRead && role != store.ProjectAccessManage {
		validationFailed(w, r, "role", "role must be read or manage")
		return
	}
	if err := s.access.SetProjectGrant(r.Context(), projectID, userID, role); err != nil {
		s.writeAccessError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, store.ProjectGrant{ProjectID: projectID, UserID: userID, Role: role})
}

func (s *Server) handleAdminDeleteGrant(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccess(w, r) {
		return
	}
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	userID, err := parseID(r.PathValue("user_id"))
	if err != nil {
		validationFailed(w, r, "user_id", "user_id must be a positive integer")
		return
	}
	if err := s.access.DeleteProjectGrant(r.Context(), projectID, userID); err != nil {
		s.writeAccessError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeAccessError maps access-store sentinels without logging row content.
func (s *Server) writeAccessError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "not_found", "not found", nil)
		return
	}
	if errors.Is(err, store.ErrInvalidCursor) {
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid cursor", nil)
		return
	}
	s.logger.Error("access administration request failed",
		"request_id", requestID(r.Context()), "path", r.URL.Path)
	writeError(w, r, http.StatusInternalServerError, "internal", "internal error", nil)
}
