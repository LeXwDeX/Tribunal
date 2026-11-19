package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/store"
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
	access, ok := s.access.(platformUserStore)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "platform user store is not configured", nil)
		return
	}
	ids, names := s.platformAdmins()
	users, next, err := access.ListPlatformUsers(r.Context(), r.URL.Query().Get("cursor"), limit, ids, names)
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
		Disabled   *bool   `json:"disabled"`
		Role       *string `json:"role"`
		Revision   *int64  `json:"revision"`
		GlobalRead *bool   `json:"global_read"`
	}
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if request.Disabled == nil && request.Role == nil && request.GlobalRead == nil {
		validationFailed(w, r, "role", "role, disabled or global_read is required")
		return
	}
	if request.Role != nil && *request.Role != roleAdmin && *request.Role != roleViewer {
		validationFailed(w, r, "role", "role must be admin or viewer")
		return
	}
	if request.Revision == nil || *request.Revision <= 0 {
		validationFailed(w, r, "revision", "revision is required")
		return
	}
	access, ok := s.access.(platformUserStore)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "platform user store is not configured", nil)
		return
	}
	ids, names := s.platformAdmins()
	oldRole, newRole, grantChanged, err := access.UpdatePlatformUser(r.Context(), userID, store.PlatformUserUpdate{
		Role: request.Role, Disabled: request.Disabled, GlobalRead: request.GlobalRead, Revision: request.Revision,
		ActorID: currentUserID(r), AdminIDs: ids, AdminUsernames: names,
	})
	if err != nil {
		s.logger.Info("platform user change rejected", "event", "platform_audit", "actor_id", currentUserID(r),
			"action", "update_user", "target_id", userID, "code", accessWriteCode(err), "result", "rejected")
		s.writeAccessError(w, r, err)
		return
	}
	// One audit line per accepted write, including disabled-only changes that
	// leave the role and grants untouched.
	attrs := []any{
		"event", "platform_audit", "actor_id", currentUserID(r),
		"action", "update_user", "target_id", userID,
		"old_role", oldRole, "new_role", newRole, "grant_changed", grantChanged,
		"result", "ok",
	}
	if request.Disabled != nil {
		attrs = append(attrs, "disabled", *request.Disabled)
	}
	if request.GlobalRead != nil {
		attrs = append(attrs, "global_read", *request.GlobalRead)
	}
	s.logger.Info("platform user changed", attrs...)
	s.invalidateStreamAuth()
	response := map[string]any{"id": userID, "role": newRole}
	if request.Disabled != nil {
		response["disabled"] = *request.Disabled
	}
	if request.GlobalRead != nil {
		response["global_read"] = *request.GlobalRead
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleAdminListUserGrants(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccess(w, r) {
		return
	}
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		validationFailed(w, r, "limit", err.Error())
		return
	}
	access, ok := s.access.(platformUserStore)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "platform user store is not configured", nil)
		return
	}
	grants, next, err := access.ListUserGrants(r.Context(), id, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.writeAccessError(w, r, err)
		return
	}
	if grants == nil {
		grants = []store.ProjectGrant{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": grants, "next_cursor": next})
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
		s.logger.Info("user credential revocation rejected", "event", "platform_audit", "actor_id", currentUserID(r),
			"action", "revoke_credentials", "target_id", userID, "code", accessWriteCode(err), "result", "rejected")
		s.writeAccessError(w, r, err)
		return
	}
	s.logger.Info("user credentials revoked", "event", "platform_audit", "actor_id", currentUserID(r),
		"action", "revoke_credentials", "target_id", userID, "result", "ok")
	s.invalidateStreamAuth()
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

// handleAdminRetryOnboarding enqueues a fresh onboarding job for a project.
// It is a platform-admin operation: project manage grants are not involved.
func (s *Server) handleAdminRetryOnboarding(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccess(w, r) {
		return
	}
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	if s.workspace == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "workspace service is not configured", nil)
		return
	}
	jobID, err := s.workspace.RetryOnboarding(r.Context(), projectID)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": jobID})
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
		s.logger.Info("project grant change rejected", "event", "platform_audit", "actor_id", currentUserID(r),
			"action", "grant", "target_id", userID, "project_id", projectID, "role", role,
			"code", accessWriteCode(err), "result", "rejected")
		s.writeAccessError(w, r, err)
		return
	}
	s.logger.Info("project grant changed", "event", "platform_audit", "actor_id", currentUserID(r),
		"action", "grant", "target_id", userID, "project_id", projectID, "role", role, "result", "ok")
	s.invalidateStreamAuth()
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
	// Read the grant before revoking so the audit line records the removed
	// role. A missing grant is reported by both calls with the same code.
	role, err := s.access.ProjectGrantRole(r.Context(), projectID, userID)
	if err != nil {
		s.logger.Info("project grant revocation rejected", "event", "platform_audit", "actor_id", currentUserID(r),
			"action", "revoke_grant", "target_id", userID, "project_id", projectID,
			"code", accessWriteCode(err), "result", "rejected")
		s.writeAccessError(w, r, err)
		return
	}
	if err := s.access.DeleteProjectGrant(r.Context(), projectID, userID); err != nil {
		s.logger.Info("project grant revocation rejected", "event", "platform_audit", "actor_id", currentUserID(r),
			"action", "revoke_grant", "target_id", userID, "project_id", projectID, "role", role,
			"code", accessWriteCode(err), "result", "rejected")
		s.writeAccessError(w, r, err)
		return
	}
	s.logger.Info("project grant revoked", "event", "platform_audit", "actor_id", currentUserID(r),
		"action", "revoke_grant", "target_id", userID, "project_id", projectID, "role", role, "result", "ok")
	s.invalidateStreamAuth()
	w.WriteHeader(http.StatusNoContent)
}

// accessWriteCode maps access-store sentinels to the stable codes that
// writeAccessError returns. Audit lines use it instead of the raw error text.
func accessWriteCode(err error) string {
	switch {
	case errors.Is(err, store.ErrPlatformUserProtected):
		return "protected_user"
	case errors.Is(err, store.ErrStaleRevision):
		return "stale_revision"
	case errors.Is(err, store.ErrNotFound):
		return "not_found"
	case errors.Is(err, store.ErrInvalidCursor):
		return "bad_request"
	default:
		return "internal"
	}
}

// writeAccessError maps access-store sentinels without logging row content.
func (s *Server) writeAccessError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrPlatformUserProtected) {
		writeError(w, r, http.StatusForbidden, "protected_user", "platform administrator cannot be changed", nil)
		return
	}
	if errors.Is(err, store.ErrStaleRevision) {
		writeError(w, r, http.StatusConflict, "stale_revision", "user revision is stale", nil)
		return
	}
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

// handleAdminListProjectResidues is read-only. It reports paths below the
// configured workspace root that may belong to unregistered projects.
func (s *Server) handleAdminListProjectResidues(w http.ResponseWriter, r *http.Request) {
	if s.residues == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "project residue service is not configured", nil)
		return
	}
	residues, err := s.residues.ListProjectResidues(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if residues == nil {
		residues = []app.ProjectResidue{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"residues": residues})
}

// handleAdminCleanupProjectResidues requires an explicit confirm flag in
// addition to the CLI's own flag. Unknown ownership is kept and reported.
func (s *Server) handleAdminCleanupProjectResidues(w http.ResponseWriter, r *http.Request) {
	if s.residues == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "project residue service is not configured", nil)
		return
	}
	var request struct {
		Confirm bool `json:"confirm"`
	}
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if !request.Confirm {
		validationFailed(w, r, "confirm", "confirm must be true")
		return
	}
	result, err := s.residues.CleanupProjectResidues(r.Context())
	if err != nil {
		s.logger.Info("project residue cleanup rejected", "event", "platform_audit", "actor_id", currentUserID(r),
			"action", "cleanup_residues", "code", "cleanup_failed", "result", "rejected")
		s.writeServiceError(w, r, err)
		return
	}
	if result == nil {
		result = &app.ProjectResidueCleanup{
			Removed: []app.ProjectResidue{},
			Kept:    []app.ProjectResidue{},
			Failed:  []app.ProjectResidueFailure{},
		}
	}
	// Counts only: residue paths are host paths and stay out of the log.
	s.logger.Info("project residue cleanup completed", "event", "platform_audit", "actor_id", currentUserID(r),
		"action", "cleanup_residues", "removed", len(result.Removed), "kept", len(result.Kept),
		"failed", len(result.Failed), "result", "ok")
	writeJSON(w, http.StatusOK, result)
}
