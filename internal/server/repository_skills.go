package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type repositorySkillAPI interface {
	DiscoverRepositorySkills(context.Context, int64, string) (*app.RepositorySkillDiscovery, error)
	RepositorySkillAuthorization(context.Context, int64, string, string) (*store.RepositorySkillAuthorization, error)
	ReplaceRepositorySkillAuthorization(context.Context, store.RepositorySkillAuthorization, string, string, string, int64) (*store.RepositorySkillAuthorization, error)
}

func (s *Server) repositorySkillContext(r *http.Request, projectID int64) context.Context {
	principal, ok := principalFrom(r.Context())
	return app.WithDispatchAuthorization(r.Context(), func(ctx context.Context, id int64) error {
		access, available := s.access.(interface {
			AuthorizeRepositorySkillManagement(context.Context, int64, int64) error
		})
		if !ok || !available || s.auth == nil || principal.user.ID <= 0 || id != projectID || s.roleFor(ctx, principal.user) != roleAdmin {
			return store.ErrNotFound
		}
		return access.AuthorizeRepositorySkillManagement(ctx, principal.user.ID, id)
	})
}

func (s *Server) repositorySkillRequest(w http.ResponseWriter, r *http.Request, needsBlock bool) (repositorySkillAPI, int64, string, string, bool) {
	if !s.requirePlatformAdmin(w, r) {
		return nil, 0, "", "", false
	}
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return nil, 0, "", "", false
	}
	if _, ok := s.requireProjectAccess(w, r, projectID, true); !ok {
		return nil, 0, "", "", false
	}
	query := r.URL.Query()
	if len(query["target_branch"]) != 1 || query.Get("target_branch") == "" || strings.TrimSpace(query.Get("target_branch")) != query.Get("target_branch") {
		validationFailed(w, r, "target_branch", "one target branch is required")
		return nil, 0, "", "", false
	}
	branch := query.Get("target_branch")
	if _, err := repo.NormalizeRef("refs/heads/" + branch); err != nil {
		validationFailed(w, r, "target_branch", "target branch is invalid")
		return nil, 0, "", "", false
	}
	block := ""
	if needsBlock {
		if len(query["block"]) != 1 || !blockNameRE.MatchString(query.Get("block")) {
			validationFailed(w, r, "block", "one valid block is required")
			return nil, 0, "", "", false
		}
		block = query.Get("block")
	}
	service, ok := s.workspace.(repositorySkillAPI)
	if !ok || service == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "repository skill service is not configured", nil)
		return nil, 0, "", "", false
	}
	return service, projectID, branch, block, true
}

func (s *Server) handleDiscoverRepositorySkills(w http.ResponseWriter, r *http.Request) {
	service, projectID, branch, _, ok := s.repositorySkillRequest(w, r, false)
	if !ok {
		return
	}
	result, err := service.DiscoverRepositorySkills(s.repositorySkillContext(r, projectID), projectID, branch)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleGetRepositorySkillAuthorization(w http.ResponseWriter, r *http.Request) {
	service, projectID, branch, block, ok := s.repositorySkillRequest(w, r, true)
	if !ok {
		return
	}
	result, err := service.RepositorySkillAuthorization(s.repositorySkillContext(r, projectID), projectID, branch, block)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type repositorySkillAuthorizationRequest struct {
	Skills           *[]string `json:"skills"`
	ExpectedRevision *string   `json:"expected_revision"`
	TargetSHA        string    `json:"target_sha"`
	SnapshotSHA256   string    `json:"snapshot_sha256"`
}

func (s *Server) handleReplaceRepositorySkillAuthorization(w http.ResponseWriter, r *http.Request) {
	service, projectID, branch, block, ok := s.repositorySkillRequest(w, r, true)
	if !ok {
		return
	}
	var request repositorySkillAuthorizationRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if request.ExpectedRevision == nil || !store.ValidBlockBindingRevision(*request.ExpectedRevision) {
		validationFailed(w, r, "expected_revision", "expected_revision is required")
		return
	}
	if request.Skills == nil || len(*request.Skills) > 256 {
		validationFailed(w, r, "skills", "skills must be an explicit array with at most 256 entries")
		return
	}
	for _, name := range *request.Skills {
		value, qualified := strings.CutPrefix(name, "repo:")
		if !qualified || !skillNameRE.MatchString(value) || strings.Contains(value, "__") {
			validationFailed(w, r, "skills", "skills must be source-qualified repository names")
			return
		}
	}
	if len(*request.Skills) > 0 && repo.ValidateSHA(request.TargetSHA) != nil {
		validationFailed(w, r, "target_sha", "target_sha must identify the reviewed commit")
		return
	}
	if len(*request.Skills) > 0 && !store.ValidRepositorySkillSnapshotSHA256(request.SnapshotSHA256) {
		validationFailed(w, r, "snapshot_sha256", "snapshot_sha256 must identify the reviewed filtered snapshot")
		return
	}
	grant := store.RepositorySkillAuthorization{ProjectID: projectID, TargetBranch: branch, Block: block, Skills: *request.Skills}
	result, err := service.ReplaceRepositorySkillAuthorization(s.repositorySkillContext(r, projectID), grant, *request.ExpectedRevision, request.TargetSHA, request.SnapshotSHA256, currentUserID(r))
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
