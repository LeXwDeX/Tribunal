package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

// AccessStore is the project authorization and account-administration surface.
// It is optional only so tests of unrelated endpoints can omit it; every route
// that needs project access fails closed with 503 when it is absent.
type AccessStore interface {
	GetProjectAccess(ctx context.Context, userID, projectID int64) (string, error)
	ListUsers(ctx context.Context, cursor string, limit int) ([]store.User, string, error)
	SetUserDisabled(ctx context.Context, userID int64, disabled bool) error
	RevokeUserCredentials(ctx context.Context, userID int64) error
	ListProjectGrants(ctx context.Context, projectID int64, cursor string, limit int) ([]store.ProjectGrant, string, error)
	SetProjectGrant(ctx context.Context, projectID, userID int64, role string) error
	DeleteProjectGrant(ctx context.Context, projectID, userID int64) error
}

// accessPolicy is the per-route authorization decision. Unknown zero values
// deny, so adding a route without a policy cannot accidentally expose it.
type accessPolicy int

const (
	policyDeny accessPolicy = iota
	policyAuthenticated
	policyPlatformAdmin
	policyProjectRead
	policyProjectManage
	policyReviewRead
	policyReviewManage
)

type accessContextKey struct{}
type workflowContextKey struct{}

// grantedAccess returns the role checked by the route guard.
func grantedAccess(ctx context.Context) string {
	value, _ := ctx.Value(accessContextKey{}).(string)
	return value
}

// guardedWorkflow returns the workflow loaded by a review guard.
func guardedWorkflow(ctx context.Context) (*store.Workflow, bool) {
	value, ok := ctx.Value(workflowContextKey{}).(*store.Workflow)
	return value, ok
}

func withGrantedAccess(ctx context.Context, access string) context.Context {
	return context.WithValue(ctx, accessContextKey{}, access)
}

func withGuardedWorkflow(ctx context.Context, workflow *store.Workflow) context.Context {
	return context.WithValue(ctx, workflowContextKey{}, workflow)
}

// guard applies one explicit policy after authentication and CSRF checks.
func (s *Server) guard(policy accessPolicy, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := principalFrom(r.Context()); !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "authentication required", nil)
			return
		}
		ctx := r.Context()
		switch policy {
		case policyAuthenticated:
		case policyPlatformAdmin:
			if !s.requirePlatformAdmin(w, r) {
				return
			}
		case policyProjectRead, policyProjectManage:
			projectID, err := parseID(r.PathValue("id"))
			if err != nil {
				validationFailed(w, r, "id", "id must be a positive integer")
				return
			}
			access, ok := s.requireProjectAccess(w, r, projectID, policy == policyProjectManage)
			if !ok {
				return
			}
			ctx = withGrantedAccess(ctx, access)
		case policyReviewRead, policyReviewManage:
			if !s.requireStore(w, r) {
				return
			}
			workflow, err := s.store.GetWorkflow(ctx, r.PathValue("wf_id"))
			if err != nil {
				s.writeStoreError(w, r, err)
				return
			}
			access, ok := s.requireProjectAccess(w, r, workflow.ProjectID, policy == policyReviewManage)
			if !ok {
				return
			}
			ctx = withGrantedAccess(ctx, access)
			ctx = withGuardedWorkflow(ctx, workflow)
		case policyDeny:
			fallthrough
		default:
			writeError(w, r, http.StatusForbidden, "forbidden", "route is not authorized", nil)
			return
		}
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) requirePlatformAdmin(w http.ResponseWriter, r *http.Request) bool {
	value, ok := principalFrom(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "unauthenticated", "authentication required", nil)
		return false
	}
	if value.user.Role != roleAdmin {
		writeError(w, r, http.StatusForbidden, "forbidden", "platform admin role required", nil)
		return false
	}
	return true
}

func (s *Server) requireAccess(w http.ResponseWriter, r *http.Request) bool {
	if s.access == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "access store is not configured", nil)
		return false
	}
	return true
}

// requireProjectAccess checks the stored grant for the authenticated user.
// Platform administrators get no implicit grant: missing grants are 404 and a
// read grant cannot perform a manage operation.
func (s *Server) requireProjectAccess(w http.ResponseWriter, r *http.Request, projectID int64, manage bool) (string, bool) {
	if !s.requireAccess(w, r) {
		return "", false
	}
	value, ok := principalFrom(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "unauthenticated", "authentication required", nil)
		return "", false
	}
	// The static bootstrap bearer is ID 0 and never has a project grant.
	if value.user.ID <= 0 || projectID <= 0 {
		writeError(w, r, http.StatusNotFound, "not_found", "not found", nil)
		return "", false
	}
	role, err := s.access.GetProjectAccess(r.Context(), value.user.ID, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "not found", nil)
			return "", false
		}
		s.logger.Error("project access lookup failed",
			"request_id", requestID(r.Context()), "project_id", projectID)
		writeError(w, r, http.StatusInternalServerError, "internal", "internal error", nil)
		return "", false
	}
	switch role {
	case store.ProjectAccessManage:
		return role, true
	case store.ProjectAccessRead:
		if manage {
			writeError(w, r, http.StatusForbidden, "forbidden", "project manage role required", nil)
			return "", false
		}
		return role, true
	default:
		// Unknown stored roles are denied rather than interpreted.
		writeError(w, r, http.StatusForbidden, "forbidden", "project role is not recognized", nil)
		return "", false
	}
}

// requireManageGranted is used by handlers that already hold a read-level
// route guard but need a stronger capability for one operation.
func (s *Server) requireManageGranted(w http.ResponseWriter, r *http.Request, projectID int64) bool {
	if grantedAccess(r.Context()) == store.ProjectAccessManage {
		return true
	}
	_, ok := s.requireProjectAccess(w, r, projectID, true)
	return ok
}

// currentUserID returns the authenticated user ID. The static bearer is 0.
func currentUserID(r *http.Request) int64 {
	value, ok := principalFrom(r.Context())
	if !ok {
		return 0
	}
	return value.user.ID
}

// accessUserIDPtr always returns a non-nil pointer for HTTP list and stats
// handlers. The bootstrap bearer (ID 0) therefore matches no rows instead of
// falling back to the trusted unrestricted internal path.
func accessUserIDPtr(userID int64) *int64 {
	value := userID
	if value < 0 {
		value = 0
	}
	return &value
}
