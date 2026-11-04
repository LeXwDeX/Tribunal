package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// projectLookupFailGitLab fails the project lookup that precedes every clone,
// as GitLab does for a revoked or under-privileged project token.
type projectLookupFailGitLab struct {
	*reviewGitLab
	err error
}

func (g *projectLookupFailGitLab) GetProject(context.Context, string, string) (*gitlab.Project, error) {
	return nil, g.err
}

// fetchFailGitOps fails the fetch of the project base ref.
type fetchFailGitOps struct {
	*fakeGitOps
	err error
}

func (g *fetchFailGitOps) Fetch(context.Context, string, repo.Auth, ...string) error { return g.err }

const workspaceFailureRaw = "https://gitlab.example/private?token=glpat-synthetic-secret"

func decodeWorkspaceFailureLog(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()
	var event map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var candidate map[string]any
		if err := json.Unmarshal([]byte(line), &candidate); err != nil {
			t.Fatalf("decode log %q: %v", line, err)
		}
		if candidate["msg"] == "workspace operation failed" {
			event = candidate
		}
	}
	if event == nil {
		t.Fatalf("workspace failure was not logged: %s", logs.String())
	}
	for _, secret := range []string{"glpat-", "gitlab.example", "/private"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("log leaked %q: %s", secret, logs.String())
		}
	}
	return event
}

func TestOnboardWithRejectedTokenRecordsActionableError(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		message string
		class   string
	}{
		{"revoked token", 401, store.WorkspaceGitLabTokenRejected, "gitlab_auth"},
		{"insufficient role", 403, store.WorkspaceGitLabForbidden, "gitlab_forbidden"},
		{"project gone", 404, store.WorkspaceGitLabNotFound, "gitlab_not_found"},
		{"rate limited", 429, store.WorkspaceGitLabRateLimited, "gitlab_rate_limited"},
		{"server error", 502, store.WorkspaceGitLabUnavailable, "gitlab_unavailable"},
		{"unreachable", 0, store.WorkspaceGitLabUnavailable, "gitlab_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			service := unrestrictedIndexService(t, f)
			service.gitlab = &projectLookupFailGitLab{reviewGitLab: f.gitlab, err: &gitlab.Error{Op: "get_project", Status: tc.status}}
			var logs bytes.Buffer
			service.logger = slog.New(slog.NewJSONHandler(&logs, nil))

			if err := service.Onboard(context.Background(), 42); err == nil {
				t.Fatal("onboarding with a rejected token succeeded")
			}
			state, err := f.db.GetWorkspace(context.Background(), 42)
			if err != nil {
				t.Fatalf("GetWorkspace: %v", err)
			}
			if state.State != "error" || state.LastError != tc.message {
				t.Fatalf("workspace = %+v, want error %q", state, tc.message)
			}
			event := decodeWorkspaceFailureLog(t, &logs)
			if event["error_class"] != tc.class {
				t.Errorf("error_class = %v, want %q", event["error_class"], tc.class)
			}
			status, ok := event["upstream_status"]
			if tc.status == 0 {
				if ok {
					t.Errorf("upstream_status = %v for a transport failure", status)
				}
			} else if status != float64(tc.status) {
				t.Errorf("upstream_status = %v, want %d", status, tc.status)
			}
		})
	}
}

func TestRefreshWithGitFailureRecordsActionableError(t *testing.T) {
	cases := []struct {
		reason  string
		message string
		class   string
	}{
		{repo.ReasonAuth, store.WorkspaceGitAuthFailed, "git_auth"},
		{repo.ReasonForbidden, store.WorkspaceGitForbidden, "git_forbidden"},
		{repo.ReasonRepositoryNotFound, store.WorkspaceGitRepositoryNotFound, "git_repository_not_found"},
		{repo.ReasonRefNotFound, store.WorkspaceGitBranchNotFound, "git_ref_not_found"},
		{repo.ReasonNetwork, store.WorkspaceGitNetwork, "git_network"},
		{"", store.WorkspaceGitFailed, "git_failed"},
	}
	for _, tc := range cases {
		t.Run("reason "+tc.reason, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			service := unrestrictedIndexService(t, f)
			service.git = &fetchFailGitOps{fakeGitOps: f.git, err: fmt.Errorf("wrap: %w", &repo.Error{Op: "fetch", ExitCode: 128, Reason: tc.reason})}
			var logs bytes.Buffer
			service.logger = slog.New(slog.NewJSONHandler(&logs, nil))

			if err := service.Refresh(context.Background(), 42, "master"); err == nil {
				t.Fatal("refresh with a failed fetch succeeded")
			}
			state, err := f.db.GetWorkspace(context.Background(), 42)
			if err != nil {
				t.Fatalf("GetWorkspace: %v", err)
			}
			if state.State != "error" || state.LastError != tc.message {
				t.Fatalf("workspace = %+v, want error %q", state, tc.message)
			}
			event := decodeWorkspaceFailureLog(t, &logs)
			if event["error_class"] != tc.class {
				t.Errorf("error_class = %v, want %q", event["error_class"], tc.class)
			}
			if _, ok := event["upstream_status"]; ok {
				t.Errorf("git failure logged an upstream_status: %v", event)
			}
		})
	}
}

func TestWorkspaceFailureMessagesAreFixedAndSanitized(t *testing.T) {
	cases := []struct {
		name    string
		cause   error
		message string
	}{
		{"model auth", &llm.Error{Kind: llm.ErrAuth, Status: 403, Err: errors.New(workspaceFailureRaw)}, store.WorkspaceModelAuthFailed},
		{"model rate limited", &llm.Error{Kind: llm.ErrRateLimited, Status: 429}, store.WorkspaceModelRateLimited},
		{"model network", &llm.Error{Kind: llm.ErrTransient}, store.WorkspaceModelUnavailable},
		{"model overloaded", &llm.Error{Kind: llm.ErrOverloaded, Status: 529}, store.WorkspaceModelUnavailable},
		{"model config", &llm.Error{Kind: llm.ErrBadRequest, Status: 400}, store.WorkspaceModelFailed},
		{"onboarding node auth", &onboardingFailure{class: "upstream_auth", status: 401}, store.WorkspaceModelAuthFailed},
		{"onboarding node timeout", &onboardingFailure{class: "timeout"}, store.WorkspaceModelTimedOut},
		{"onboarding node unsafe", &onboardingFailure{class: "unsafe_model_response"}, store.WorkspaceModelFailed},
		{"timeout", fmt.Errorf("%s: %w", workspaceFailureRaw, context.DeadlineExceeded), store.WorkspaceTimedOut},
		{"canceled", fmt.Errorf("%s: %w", workspaceFailureRaw, context.Canceled), store.WorkspaceCanceled},
		{"profile missing", PermanentJob(errOnboardingProfileMissing), store.WorkspaceOnboardingUnconfigured},
		{"identity mismatch", errProjectIdentityChanged, store.WorkspaceGitLabProjectChanged},
		{"raw", errors.New(workspaceFailureRaw), store.WorkspaceOperationFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyWorkspaceFailure(tc.cause)
			if got.message != tc.message {
				t.Fatalf("message = %q, want %q", got.message, tc.message)
			}
			if !store.IsWorkspaceFailureMessage(got.message) {
				t.Fatalf("message %q is outside the fixed set", got.message)
			}
		})
	}
}
