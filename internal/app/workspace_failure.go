package app

import (
	"context"
	"errors"
	"net/http"

	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

var (
	// errOnboardingProfileMissing reports that no onboarding profile exists.
	errOnboardingProfileMissing = errors.New("app: onboarding profile is not configured")
	// errProjectIdentityChanged reports that GitLab no longer describes the
	// registered project at its registered location.
	errProjectIdentityChanged = errors.New("app: project identity changed")
)

// workspaceFailure is the sanitized view of a workspace operation failure:
// a fixed stored message, a fixed log class and an optional HTTP status.
type workspaceFailure struct {
	message string
	class   string
	status  int
}

// classifyWorkspaceFailure maps an error to fixed values only. It never reads
// error text, so tokens, URLs, paths and upstream bodies cannot leak.
func classifyWorkspaceFailure(err error) workspaceFailure {
	var node *onboardingFailure
	if errors.As(err, &node) {
		return workspaceFailure{message: modelFailureMessage(node.class), class: node.class, status: node.status}
	}
	var upstream *llm.Error
	if errors.As(err, &upstream) && upstream.Kind != llm.ErrCanceled {
		class := workspaceErrorClass(err)
		return workspaceFailure{message: modelFailureMessage(class), class: class, status: upstream.ErrorStatus()}
	}
	var gl *gitlab.Error
	if errors.As(err, &gl) {
		return gitLabWorkspaceFailure(gl.Status)
	}
	var ge *repo.Error
	if errors.As(err, &ge) {
		return gitWorkspaceFailure(ge.Reason)
	}
	switch {
	case errors.Is(err, errOnboardingProfileMissing):
		return workspaceFailure{message: store.WorkspaceOnboardingUnconfigured, class: "config"}
	case errors.Is(err, errProjectIdentityChanged):
		return workspaceFailure{message: store.WorkspaceGitLabProjectChanged, class: "gitlab_project_changed"}
	}
	class := workspaceErrorClass(err)
	switch {
	case class == errclass.Timeout || errors.Is(err, context.DeadlineExceeded):
		return workspaceFailure{message: store.WorkspaceTimedOut, class: errclass.Timeout}
	case class == errclass.Canceled || errors.Is(err, workspace.ErrOutboundInvalidated):
		return workspaceFailure{message: store.WorkspaceCanceled, class: errclass.Canceled}
	case class == "unsafe_model_response":
		return workspaceFailure{message: store.WorkspaceModelFailed, class: class}
	}
	return workspaceFailure{message: store.WorkspaceOperationFailed, class: class}
}

func gitLabWorkspaceFailure(status int) workspaceFailure {
	out := workspaceFailure{status: status}
	switch {
	case status == http.StatusUnauthorized:
		out.message, out.class = store.WorkspaceGitLabTokenRejected, "gitlab_auth"
	case status == http.StatusForbidden:
		out.message, out.class = store.WorkspaceGitLabForbidden, "gitlab_forbidden"
	case status == http.StatusNotFound:
		out.message, out.class = store.WorkspaceGitLabNotFound, "gitlab_not_found"
	case status == http.StatusTooManyRequests:
		out.message, out.class = store.WorkspaceGitLabRateLimited, "gitlab_rate_limited"
	case status == 0 || status >= 500:
		out.message, out.class = store.WorkspaceGitLabUnavailable, "gitlab_unavailable"
	default:
		out.message, out.class = store.WorkspaceGitLabRejected, "gitlab_rejected"
	}
	return out
}

func gitWorkspaceFailure(reason string) workspaceFailure {
	switch reason {
	case repo.ReasonAuth:
		return workspaceFailure{message: store.WorkspaceGitAuthFailed, class: "git_auth"}
	case repo.ReasonForbidden:
		return workspaceFailure{message: store.WorkspaceGitForbidden, class: "git_forbidden"}
	case repo.ReasonRepositoryNotFound:
		return workspaceFailure{message: store.WorkspaceGitRepositoryNotFound, class: "git_repository_not_found"}
	case repo.ReasonRefNotFound:
		return workspaceFailure{message: store.WorkspaceGitBranchNotFound, class: "git_ref_not_found"}
	case repo.ReasonNetwork:
		return workspaceFailure{message: store.WorkspaceGitNetwork, class: "git_network"}
	default:
		return workspaceFailure{message: store.WorkspaceGitFailed, class: "git_failed"}
	}
}

// modelFailureMessage maps an onboarding model failure class to a message.
func modelFailureMessage(class string) string {
	switch class {
	case "upstream_auth":
		return store.WorkspaceModelAuthFailed
	case "rate_limited":
		return store.WorkspaceModelRateLimited
	case "network", "overloaded":
		return store.WorkspaceModelUnavailable
	case errclass.Timeout:
		return store.WorkspaceModelTimedOut
	case errclass.Canceled:
		return store.WorkspaceCanceled
	default:
		return store.WorkspaceModelFailed
	}
}
