package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

// workspaceResponse adds the derived index state without changing the stored
// workspace record.
type workspaceResponse struct {
	store.Workspace
	IndexStatus string `json:"index_status"`
}

// projectWorkspaceSummary is the bounded workspace view used by project
// detail and project list responses.
type projectWorkspaceSummary struct {
	State         string     `json:"state"`
	BaseSHA       string     `json:"base_sha,omitempty"`
	IndexStatus   string     `json:"index_status"`
	LastRefreshAt *time.Time `json:"last_refresh_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

// Platform MCP settings do not prove whether a project-level index is enabled.
// Report only the stored index checkpoint, not current index health.
func workspaceIndexStatus(workspace *store.Workspace) string {
	if workspace == nil {
		return "unknown"
	}
	base := strings.TrimSpace(workspace.BaseSHA)
	index := strings.TrimSpace(workspace.IndexSHA)
	if index == "" || base == "" {
		return "unknown"
	}
	// Older rows can contain index_sha values written before commands were
	// counted. Without a migration they are indistinguishable from a real
	// index, so SHA equality remains the only available evidence.
	if strings.EqualFold(index, base) {
		return "ready"
	}
	return "stale"
}

func workspaceSummary(workspace *store.Workspace) *projectWorkspaceSummary {
	if workspace == nil {
		return nil
	}
	return &projectWorkspaceSummary{
		State:         workspace.State,
		BaseSHA:       workspace.BaseSHA,
		IndexStatus:   workspaceIndexStatus(workspace),
		LastRefreshAt: workspace.LastRefreshAt,
		LastError:     workspace.LastError,
	}
}

func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	workspace, err := s.store.GetWorkspace(r.Context(), projectID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, workspaceResponse{
		Workspace:   *workspace,
		IndexStatus: workspaceIndexStatus(workspace),
	})
}

func (s *Server) handleRefreshWorkspace(w http.ResponseWriter, r *http.Request) {
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	if s.workspace == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "workspace service is not configured", nil)
		return
	}
	var request struct {
		RegenerateContext bool `json:"regenerate_context,omitempty"`
		Reindex           bool `json:"reindex,omitempty"`
	}
	if err := decodeOptionalJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	jobID, err := s.workspace.RefreshWorkspace(r.Context(), projectID, request.RegenerateContext, request.Reindex)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": jobID})
}

func (s *Server) handleListContextDocs(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
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
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind != "" && kind != "agents" && kind != "claude" {
		validationFailed(w, r, "kind", "kind must be agents or claude")
		return
	}
	documents, next, err := s.store.ListContextDocs(r.Context(), store.ContextDocFilter{
		ProjectID: projectID,
		Kind:      kind,
		Cursor:    r.URL.Query().Get("cursor"),
		Limit:     limit,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if documents == nil {
		documents = []store.ContextDoc{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": documents, "next_cursor": next})
}

func (s *Server) handleGetCurrentContextDoc(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	kind, err := contextKind(r)
	if err != nil {
		validationFailed(w, r, "kind", err.Error())
		return
	}
	doc, err := s.store.GetCurrentContextDoc(r.Context(), projectID, kind)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

type contextDocSaveRequest struct {
	Kind    string `json:"kind,omitempty"`
	Content string `json:"content"`
	Locked  *bool  `json:"locked,omitempty"`
}

func (s *Server) handleSaveCurrentContextDoc(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	kind, err := contextKind(r)
	if err != nil {
		validationFailed(w, r, "kind", err.Error())
		return
	}
	var request contextDocSaveRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if request.Kind != "" && request.Kind != kind {
		validationFailed(w, r, "kind", "body kind must match the kind query parameter")
		return
	}
	if strings.TrimSpace(request.Content) == "" {
		validationFailed(w, r, "content", "content must not be empty")
		return
	}
	if s.contexts == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "context service is not configured", nil)
		return
	}
	var userID *int64
	if principal, ok := principalFrom(r.Context()); ok && principal.user.ID > 0 {
		value := principal.user.ID
		userID = &value
	}
	saved, err := s.contexts.SaveContext(r.Context(), projectID, kind, request.Content, request.Locked, userID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) handleLockContextDoc(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	kind, err := contextKind(r)
	if err != nil {
		validationFailed(w, r, "kind", err.Error())
		return
	}
	var request struct {
		Locked *bool `json:"locked"`
	}
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if request.Locked == nil {
		validationFailed(w, r, "locked", "locked is required")
		return
	}
	if s.contexts == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "context service is not configured", nil)
		return
	}
	var userID *int64
	if principal, ok := principalFrom(r.Context()); ok && principal.user.ID > 0 {
		value := principal.user.ID
		userID = &value
	}
	if err := s.contexts.LockContext(r.Context(), projectID, kind, *request.Locked, userID); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
