package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/store"
)

const projectRecentReviewLimit = 20

// projectDetailResponse keeps every stored project field at the top level and
// adds bounded workspace and review data. The embedded pointer preserves the
// existing flat response contract.
type projectDetailResponse struct {
	*store.Project
	Access        string                   `json:"access"`
	Workspace     *projectWorkspaceSummary `json:"workspace,omitempty"`
	RecentReviews []reviewSummary          `json:"recent_reviews"`
}

// projectListItem is the list equivalent: stored fields stay flat, while
// workspace and last_review carry the bounded admin summary.
type projectListItem struct {
	*store.Project
	Access     string                   `json:"access"`
	Workspace  *projectWorkspaceSummary `json:"workspace,omitempty"`
	LastReview *reviewSummary           `json:"last_review,omitempty"`
}

type registerProjectRequest struct {
	Project     string          `json:"project"`
	Token       string          `json:"token"`
	Profile     string          `json:"profile,omitempty"`
	AutoWebhook bool            `json:"auto_webhook,omitempty"`
	Settings    json.RawMessage `json:"settings,omitempty"`
}

type registerProjectResponse struct {
	Project       *store.Project `json:"project"`
	JobID         string         `json:"job_id"`
	WebhookSecret string         `json:"webhook_secret,omitempty"`
}

type validateProjectRequest struct {
	Project string `json:"project"`
	Token   string `json:"token"`
}

type validateProjectResponse struct {
	ProjectID         int64      `json:"project_id"`
	PathWithNamespace string     `json:"path_with_namespace"`
	WebURL            string     `json:"web_url"`
	DefaultBranch     string     `json:"default_branch"`
	Username          string     `json:"username"`
	AccessLevel       int        `json:"access_level"`
	TokenScopes       []string   `json:"token_scopes"`
	TokenExpiresAt    *time.Time `json:"token_expires_at,omitempty"`
	GitHead           string     `json:"git_head"`
}

type updateProjectRequest struct {
	Profile  *string          `json:"profile,omitempty"`
	Settings *json.RawMessage `json:"settings,omitempty"`
	Enabled  *bool            `json:"enabled,omitempty"`
}

type rotateProjectTokenRequest struct {
	Token string `json:"token"`
}

type projectWebhookResponse struct {
	Project   *store.Project `json:"project"`
	HookID    int64          `json:"hook_id"`
	State     string         `json:"state"`
	Events    []string       `json:"events"`
	LastError string         `json:"last_error"`
	ManagedBy string         `json:"managed_by,omitempty"`
	CreatedAt *time.Time     `json:"created_at,omitempty"`
	UpdatedAt *time.Time     `json:"updated_at,omitempty"`
}

func webhookResponse(project *store.Project, hookID int64) projectWebhookResponse {
	response := projectWebhookResponse{Project: project, HookID: hookID, Events: []string{}}
	if project == nil {
		return response
	}
	response.State = project.WebhookState
	response.Events = append([]string{}, project.WebhookEvents...)
	response.LastError = project.WebhookError
	response.ManagedBy = project.WebhookManagedBy
	response.CreatedAt = project.WebhookCreatedAt
	response.UpdatedAt = project.WebhookUpdatedAt
	return response
}

func (s *Server) handleRegisterProject(w http.ResponseWriter, r *http.Request) {
	var request registerProjectRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, r, http.StatusBadRequest, "bad_request", "request body is too large", nil)
			return
		}
		writeError(w, r, http.StatusBadRequest, "validation_failed", "invalid JSON request body", nil)
		return
	}
	registration, err := s.projects.Register(r.Context(), app.RegisterInput{
		Project:     request.Project,
		AccessToken: request.Token,
		Profile:     request.Profile,
		AutoWebhook: request.AutoWebhook,
		Settings:    request.Settings,
	})
	if err != nil {
		writeAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, registerProjectResponse{
		Project:       registration.Project,
		JobID:         registration.JobID,
		WebhookSecret: registration.WebhookSecret,
	})
}

// handleValidateProject checks connectivity and permissions without changing
// any state. A missing probe or trusted root is reported as unavailable.
func (s *Server) handleValidateProject(w http.ResponseWriter, r *http.Request) {
	var request validateProjectRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	result, err := s.projects.Validate(r.Context(), app.ValidateProjectInput{
		Project:     request.Project,
		AccessToken: request.Token,
	})
	if err != nil {
		if errors.Is(err, app.ErrProjectValidationUnavailable) {
			writeError(w, r, http.StatusServiceUnavailable, "unavailable", "project validation is not configured", nil)
			return
		}
		writeAppError(w, r, err)
		return
	}
	scopes := result.TokenScopes
	if scopes == nil {
		scopes = []string{}
	}
	writeJSON(w, http.StatusOK, validateProjectResponse{
		ProjectID:         result.ProjectID,
		PathWithNamespace: result.PathWithNamespace,
		WebURL:            result.WebURL,
		DefaultBranch:     result.DefaultBranch,
		Username:          result.Username,
		AccessLevel:       result.AccessLevel,
		TokenScopes:       scopes,
		TokenExpiresAt:    result.TokenExpiresAt,
		GitHead:           result.GitHead,
	})
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) > 200 {
		validationFailed(w, r, "q", "q must be at most 200 bytes")
		return
	}
	var enabled *bool
	switch raw := strings.TrimSpace(r.URL.Query().Get("enabled")); raw {
	case "":
	case "true", "1":
		value := true
		enabled = &value
	case "false", "0":
		value := false
		enabled = &value
	default:
		validationFailed(w, r, "enabled", "enabled must be true or false")
		return
	}
	if !s.requireAccess(w, r) {
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		validationFailed(w, r, "limit", err.Error())
		return
	}
	cursor := r.URL.Query().Get("cursor")
	userID := currentUserID(r)
	filter := app.ProjectListFilter{
		Query:        query,
		Enabled:      enabled,
		AccessUserID: accessUserIDPtr(userID),
	}
	projects, next, err := s.projects.ListFilteredPage(r.Context(), filter, cursor, limit)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if projects == nil {
		projects = []store.Project{}
	}
	if s.store == nil {
		items := make([]projectListItem, 0, len(projects))
		for i := range projects {
			access, ok, err := s.projectGrant(r, projects[i].ID)
			if err != nil {
				s.writeStoreError(w, r, err)
				return
			}
			if !ok {
				continue
			}
			items = append(items, projectListItem{Project: &projects[i], Access: access})
		}
		writeJSON(w, http.StatusOK, map[string]any{"projects": items, "next_cursor": next})
		return
	}
	// Bounded N+1: one project page plus at most two store reads per project,
	// with pageLimit capped at 200.
	items := make([]projectListItem, 0, len(projects))
	for i := range projects {
		project := &projects[i]
		access, ok, err := s.projectGrant(r, project.ID)
		if err != nil {
			s.writeStoreError(w, r, err)
			return
		}
		if !ok {
			// A grant revoked between the list query and this read must not
			// produce a visible row.
			continue
		}
		workspace, err := s.projectWorkspaceSummary(r.Context(), project.ID)
		if err != nil {
			s.writeStoreError(w, r, err)
			return
		}
		reviews, err := s.projectRecentReviews(r.Context(), project.ID, 1)
		if err != nil {
			s.writeStoreError(w, r, err)
			return
		}
		var lastReview *reviewSummary
		if len(reviews) > 0 {
			lastReview = &reviews[0]
		}
		items = append(items, projectListItem{
			Project: project, Access: access, Workspace: workspace, LastReview: lastReview,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": items, "next_cursor": next})
}

// projectGrant re-reads one project grant for response decoration. A row that
// lost its grant is skipped instead of being shown with stale access.
func (s *Server) projectGrant(r *http.Request, projectID int64) (string, bool, error) {
	userID := currentUserID(r)
	if userID <= 0 || s.access == nil {
		return "", false, nil
	}
	role, err := s.access.GetProjectAccess(r.Context(), userID, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	return role, true, nil
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	project, err := s.projects.Get(r.Context(), id)
	if err != nil {
		writeAppError(w, r, err)
		return
	}
	access := grantedAccess(r.Context())
	if s.store == nil {
		writeJSON(w, http.StatusOK, projectDetailResponse{
			Project: project, Access: access, RecentReviews: []reviewSummary{},
		})
		return
	}
	workspace, err := s.projectWorkspaceSummary(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	reviews, err := s.projectRecentReviews(r.Context(), id, projectRecentReviewLimit)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, projectDetailResponse{
		Project:       project,
		Access:        access,
		Workspace:     workspace,
		RecentReviews: reviews,
	})
}

func (s *Server) projectWorkspaceSummary(ctx context.Context, projectID int64) (*projectWorkspaceSummary, error) {
	workspace, err := s.store.GetWorkspace(ctx, projectID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return workspaceSummary(workspace), nil
}

// projectRecentReviews returns at most limit reviews, newest first. The store
// applies the project and kind filters before limiting.
func (s *Server) projectRecentReviews(ctx context.Context, projectID int64, limit int) ([]reviewSummary, error) {
	project := projectID
	workflows, _, err := s.store.ListWorkflows(ctx, store.WorkflowFilter{
		ProjectID: &project,
		Kind:      "review",
	}, "", limit)
	if err != nil {
		return nil, err
	}
	reviews := make([]reviewSummary, 0, len(workflows))
	for _, workflow := range workflows {
		reviews = append(reviews, reviewSummary{Workflow: workflow, DurationMS: workflowDuration(workflow)})
	}
	return reviews, nil
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	var request updateProjectRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if request.Profile == nil && request.Settings == nil && request.Enabled == nil {
		validationFailed(w, r, "body", "at least one of profile, settings or enabled is required")
		return
	}
	project, err := s.projects.Update(r.Context(), id, app.UpdateProjectInput{
		Profile:  request.Profile,
		Settings: request.Settings,
		Enabled:  request.Enabled,
	})
	if err != nil {
		writeAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) handleRotateProjectToken(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	var request rotateProjectTokenRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	project, err := s.projects.RotateToken(r.Context(), id, request.Token)
	if err != nil {
		writeAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) handleCreateProjectWebhook(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	registration, err := s.projects.CreateWebhook(r.Context(), id)
	if err != nil {
		writeAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, webhookResponse(registration.Project, registration.HookID))
}

func (s *Server) handleRebuildProjectWebhook(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	registration, err := s.projects.RebuildWebhook(r.Context(), id)
	if err != nil {
		writeAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, webhookResponse(registration.Project, registration.HookID))
}

func (s *Server) handleDisableProjectWebhook(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	project, err := s.projects.DisableWebhook(r.Context(), id)
	if err != nil {
		writeAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, webhookResponse(project, 0))
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	if err := s.projects.Delete(r.Context(), id); err != nil {
		writeAppError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
