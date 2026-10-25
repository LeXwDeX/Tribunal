package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

type probeCall struct {
	url   string
	token string
}

func newValidationService(t *testing.T, st ProjectStore, api gitlab.API, root string,
	probe func(context.Context, string, string) (string, error)) *ProjectService {
	t.Helper()
	service, err := NewProjectService(ProjectServiceConfig{
		Store:             st,
		GitLab:            api,
		Cipher:            newTestCipher(t),
		Profiles:          testProfiles(),
		GitLabURL:         root,
		ProbeRemote:       probe,
		ValidationTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewProjectService: %v", err)
	}
	return service
}

func TestValidateProjectReturnsMetadataWithoutPersisting(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	expires := time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)
	api.tokenInfo = &gitlab.TokenInfo{
		Scopes: []string{"api", "read_repository"}, Active: true, ExpiresAt: &expires,
	}
	var calls []probeCall
	probe := func(_ context.Context, remoteURL, token string) (string, error) {
		calls = append(calls, probeCall{url: remoteURL, token: token})
		return sha40, nil
	}
	service := newValidationService(t, st, api, "https://gitlab.example.com", probe)

	result, err := service.Validate(ctx, ValidateProjectInput{
		Project:     "https://gitlab.example.com/group/repo.git/",
		AccessToken: "glpat-project-token",
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if result.ProjectID != 42 || result.PathWithNamespace != "group/repo" ||
		result.WebURL != "https://gitlab.example.com/group/repo" ||
		result.DefaultBranch != "main" || result.Username != "review-bot" ||
		result.AccessLevel != 30 || result.GitHead != sha40 {
		t.Fatalf("validation = %+v", result)
	}
	if strings.Join(result.TokenScopes, ",") != "api,read_repository" {
		t.Fatalf("scopes = %v", result.TokenScopes)
	}
	if result.TokenExpiresAt == nil || !result.TokenExpiresAt.Equal(expires) {
		t.Fatalf("expires = %v", result.TokenExpiresAt)
	}
	if len(calls) != 1 || calls[0].url != "https://gitlab.example.com/group/repo.git" ||
		calls[0].token != "glpat-project-token" {
		t.Fatalf("probe calls = %+v", calls)
	}
	if api.callCount("CreateProjectHook") != 0 || len(api.hookSpecs) != 0 {
		t.Fatal("validation created a webhook")
	}
	for _, call := range []string{"ListProjectHooks", "UpdateProjectHook", "DeleteProjectHook"} {
		if api.callCount(call) != 0 {
			t.Fatalf("validation called %s", call)
		}
	}
	if _, err := st.GetProject(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("validation persisted a project: %v", err)
	}
	if projects, err := st.ListProjects(ctx); err != nil || len(projects) != 0 {
		t.Fatalf("projects after validation = %v, %v", projects, err)
	}
}

func TestRegisterAcceptsFullURL(t *testing.T) {
	tests := []struct {
		name     string
		root     string
		input    string
		wantRef  string
		wantCode string
	}{
		{name: "url", root: "https://gitlab.example.com", input: "https://gitlab.example.com/group/repo", wantRef: "group/repo"},
		{name: "url dot git", root: "https://gitlab.example.com", input: "https://gitlab.example.com/group/repo.git", wantRef: "group/repo"},
		{name: "url trailing slash", root: "https://gitlab.example.com", input: "https://gitlab.example.com/group/repo/", wantRef: "group/repo"},
		{name: "base path", root: "https://gitlab.example.com/gitlab", input: "https://gitlab.example.com/gitlab/group/repo.git", wantRef: "group/repo"},
		{name: "numeric id", root: "https://gitlab.example.com", input: "42", wantRef: "42"},
		{name: "namespace path", root: "https://gitlab.example.com", input: "group/repo", wantRef: "group/repo"},
		{name: "foreign origin", root: "https://gitlab.example.com", input: "https://evil.example.com/group/repo", wantCode: "validation_failed"},
		{name: "scheme mismatch", root: "https://gitlab.example.com", input: "http://gitlab.example.com/group/repo", wantCode: "validation_failed"},
		{name: "credentials", root: "https://gitlab.example.com", input: "https://user:secret@gitlab.example.com/group/repo", wantCode: "validation_failed"},
		{name: "query", root: "https://gitlab.example.com", input: "https://gitlab.example.com/group/repo?x=1", wantCode: "validation_failed"},
		{name: "fragment", root: "https://gitlab.example.com", input: "https://gitlab.example.com/group/repo#readme", wantCode: "validation_failed"},
		{name: "trailing fragment", root: "https://gitlab.example.com", input: "https://gitlab.example.com/group/repo#", wantCode: "validation_failed"},
		{name: "control byte", root: "https://gitlab.example.com", input: "https://gitlab.example.com/group/repo\x01", wantCode: "validation_failed"},
		{name: "encoded traversal", root: "https://gitlab.example.com", input: "https://gitlab.example.com/group/%2e%2e/secret.git", wantCode: "validation_failed"},
		{name: "path traversal", root: "https://gitlab.example.com", input: "https://gitlab.example.com/group/../secret.git", wantCode: "validation_failed"},
		{name: "double slash", root: "https://gitlab.example.com", input: "https://gitlab.example.com/group//repo", wantCode: "validation_failed"},
		{name: "outside base path", root: "https://gitlab.example.com/gitlab", input: "https://gitlab.example.com/group/repo", wantCode: "validation_failed"},
		{name: "base path without project", root: "https://gitlab.example.com/gitlab", input: "https://gitlab.example.com/gitlab", wantCode: "validation_failed"},
		{name: "dot git is not a project", root: "https://gitlab.example.com", input: "https://gitlab.example.com/group/.git", wantCode: "validation_failed"},
		{name: "no project", root: "https://gitlab.example.com", input: "https://gitlab.example.com/", wantCode: "validation_failed"},
		{name: "ssh scheme", root: "https://gitlab.example.com", input: "ssh://git@gitlab.example.com/group/repo.git", wantCode: "validation_failed"},
		{name: "control character", root: "https://gitlab.example.com", input: "https://gitlab.example.com/group/repo\n", wantCode: "validation_failed"},
		{name: "url when root unset", root: "", input: "https://gitlab.example.com/group/repo", wantCode: "validation_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openTestStore(t)
			api := defaultFakeGitLab()
			if strings.HasSuffix(tt.root, "/gitlab") {
				project := *api.project
				project.WebURL = "https://gitlab.example.com/gitlab/group/repo"
				project.HTTPURLToRepo = "https://gitlab.example.com/gitlab/group/repo.git"
				api.project = &project
			}
			service := newValidationService(t, st, api, tt.root, func(context.Context, string, string) (string, error) {
				return sha40, nil
			})
			_, err := service.Register(context.Background(), RegisterInput{
				Project:     tt.input,
				AccessToken: "glpat-project-token",
			})
			if tt.wantCode != "" {
				var appErr *Error
				if !errors.As(err, &appErr) || appErr.Code != tt.wantCode {
					t.Fatalf("error = %#v, want %s", err, tt.wantCode)
				}
				if api.callCount("GetProject") != 0 {
					t.Fatalf("invalid project reached GitLab: %v", api.projectRefSnapshot())
				}
				return
			}
			if err != nil {
				t.Fatalf("Register: %v", err)
			}
			refs := api.projectRefSnapshot()
			if len(refs) != 1 || refs[0] != tt.wantRef {
				t.Fatalf("GitLab project refs = %v, want %q", refs, tt.wantRef)
			}
		})
	}
}

func TestRegisterRejectsUntrustedReturnedURL(t *testing.T) {
	api := defaultFakeGitLab()
	project := *api.project
	project.WebURL = "https://evil.example.com/group/repo"
	api.project = &project
	st := openTestStore(t)
	service := newValidationService(t, st, api, "https://gitlab.example.com",
		func(context.Context, string, string) (string, error) {
			return sha40, nil
		})
	_, err := service.Register(context.Background(), RegisterInput{
		Project:     "group/repo",
		AccessToken: "glpat-project-token",
	})
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "gitlab_rejected" {
		t.Fatalf("error = %#v, want gitlab_rejected", err)
	}
	if _, getErr := st.GetProject(context.Background(), 42); !errors.Is(getErr, store.ErrNotFound) {
		t.Fatalf("untrusted project was stored: %v", getErr)
	}
}

func TestValidateRejectsUntrustedGitLabURLs(t *testing.T) {
	tests := []struct {
		name     string
		webURL   string
		cloneURL string
		branch   string
	}{
		{name: "foreign web url", webURL: "https://evil.example.com/group/repo"},
		{name: "foreign clone url", cloneURL: "https://evil.example.com/group/repo.git"},
		{name: "wrong path", cloneURL: "https://gitlab.example.com/group/other.git"},
		{name: "missing clone url", cloneURL: "-"},
		{name: "missing web url", webURL: "-"},
		{name: "missing branch", branch: "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := defaultFakeGitLab()
			project := *api.project
			if tt.webURL == "-" {
				project.WebURL = ""
			} else if tt.webURL != "" {
				project.WebURL = tt.webURL
			}
			if tt.cloneURL == "-" {
				project.HTTPURLToRepo = ""
			} else if tt.cloneURL != "" {
				project.HTTPURLToRepo = tt.cloneURL
			}
			if tt.branch == "-" {
				project.DefaultBranch = ""
			}
			api.project = &project
			var probes int
			service := newValidationService(t, openTestStore(t), api, "https://gitlab.example.com",
				func(context.Context, string, string) (string, error) {
					probes++
					return sha40, nil
				})
			_, err := service.Validate(context.Background(), ValidateProjectInput{
				Project: "group/repo", AccessToken: "glpat-secret-token",
			})
			var appErr *Error
			if !errors.As(err, &appErr) || appErr.Code != "gitlab_rejected" {
				t.Fatalf("error = %#v, want gitlab_rejected", err)
			}
			if probes != 0 {
				t.Fatalf("git probe ran %d times for untrusted metadata", probes)
			}
			for _, forbidden := range []string{"glpat-secret-token", "evil.example.com"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("error leaked %q: %v", forbidden, err)
				}
			}
		})
	}
}

func TestValidateRejectsInsufficientIdentityOrAccess(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*fakeGitLab)
		wantCode string
	}{
		{name: "missing api scope", mutate: func(f *fakeGitLab) {
			f.tokenInfo = &gitlab.TokenInfo{Scopes: []string{"read_repository"}, Active: true}
		}, wantCode: "validation_failed"},
		{name: "missing read repository scope", mutate: func(f *fakeGitLab) {
			f.tokenInfo = &gitlab.TokenInfo{Scopes: []string{"api"}, Active: true}
		}, wantCode: "validation_failed"},
		{name: "inactive token", mutate: func(f *fakeGitLab) {
			f.tokenInfo = &gitlab.TokenInfo{Scopes: []string{"api", "read_repository"}}
		}, wantCode: "validation_failed"},
		{name: "guest access", mutate: func(f *fakeGitLab) { f.access = 10 }, wantCode: "validation_failed"},
		{name: "no member", mutate: func(f *fakeGitLab) {
			f.accessErr = &gitlab.Error{Op: "get_member", Status: 404}
		}, wantCode: "gitlab_rejected"},
		{name: "user lookup failed", mutate: func(f *fakeGitLab) {
			f.userErr = &gitlab.Error{Op: "current_user", Status: 401}
		}, wantCode: "gitlab_rejected"},
		{name: "project lookup failed", mutate: func(f *fakeGitLab) {
			f.projectErr = &gitlab.Error{Op: "get_project", Status: 403}
		}, wantCode: "gitlab_rejected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := defaultFakeGitLab()
			tt.mutate(api)
			var probes int
			service := newValidationService(t, openTestStore(t), api, "https://gitlab.example.com",
				func(context.Context, string, string) (string, error) {
					probes++
					return sha40, nil
				})
			_, err := service.Validate(context.Background(), ValidateProjectInput{
				Project: "group/repo", AccessToken: "glpat-secret-token",
			})
			var appErr *Error
			if !errors.As(err, &appErr) || appErr.Code != tt.wantCode {
				t.Fatalf("error = %#v, want %s", err, tt.wantCode)
			}
			if strings.Contains(err.Error(), "glpat-secret-token") || strings.Contains(err.Error(), "https://") {
				t.Fatalf("error leaked details: %v", err)
			}
			if probes != 0 {
				t.Fatalf("git probe ran despite failed identity or access")
			}
		})
	}
}

func TestValidateRejectsMismatchedProjectIdentity(t *testing.T) {
	api := defaultFakeGitLab()
	api.project = &gitlab.Project{
		ID:                99,
		PathWithNamespace: "group/repo",
		WebURL:            "https://gitlab.example.com/group/repo",
		HTTPURLToRepo:     "https://gitlab.example.com/group/repo.git",
		DefaultBranch:     "main",
	}
	var probes int
	service := newValidationService(t, openTestStore(t), api, "https://gitlab.example.com",
		func(context.Context, string, string) (string, error) {
			probes++
			return sha40, nil
		})
	_, err := service.Validate(context.Background(), ValidateProjectInput{
		Project: "42", AccessToken: "glpat-secret-token",
	})
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "gitlab_rejected" {
		t.Fatalf("error = %#v, want gitlab_rejected", err)
	}
	if probes != 0 {
		t.Fatal("mismatched project reached the Git probe")
	}
}

func TestValidateSanitizesGitProbeFailureAndTimesOut(t *testing.T) {
	t.Run("probe failure", func(t *testing.T) {
		service := newValidationService(t, openTestStore(t), defaultFakeGitLab(),
			"https://gitlab.example.com",
			func(context.Context, string, string) (string, error) {
				return "", errors.New("fatal: https://oauth2:glpat-secret@gitlab.example.com/group/repo.git")
			})
		_, err := service.Validate(context.Background(), ValidateProjectInput{
			Project: "group/repo", AccessToken: "glpat-secret",
		})
		var appErr *Error
		if !errors.As(err, &appErr) || appErr.Code != "gitlab_rejected" {
			t.Fatalf("error = %#v, want gitlab_rejected", err)
		}
		for _, forbidden := range []string{"glpat-secret", "oauth2", "https://"} {
			if strings.Contains(err.Error(), forbidden) {
				t.Fatalf("error leaked %q: %v", forbidden, err)
			}
		}
	})

	t.Run("timeout", func(t *testing.T) {
		service, err := NewProjectService(ProjectServiceConfig{
			Store:             openTestStore(t),
			GitLab:            defaultFakeGitLab(),
			Cipher:            newTestCipher(t),
			Profiles:          testProfiles(),
			GitLabURL:         "https://gitlab.example.com",
			ValidationTimeout: 25 * time.Millisecond,
			ProbeRemote: func(ctx context.Context, _, _ string) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err = service.Validate(context.Background(), ValidateProjectInput{
			Project: "group/repo", AccessToken: "glpat-secret",
		})
		var appErr *Error
		if !errors.As(err, &appErr) || appErr.Code != "gitlab_rejected" {
			t.Fatalf("error = %#v, want gitlab_rejected", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("validation took %v, want bounded timeout", elapsed)
		}
	})
}

func TestValidateWithoutProbeConfigurationIsUnavailable(t *testing.T) {
	service, err := NewProjectService(ProjectServiceConfig{
		Store:     openTestStore(t),
		GitLab:    defaultFakeGitLab(),
		Cipher:    newTestCipher(t),
		Profiles:  testProfiles(),
		GitLabURL: "https://gitlab.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Validate(context.Background(), ValidateProjectInput{
		Project: "group/repo", AccessToken: "glpat-secret",
	})
	if !errors.Is(err, ErrProjectValidationUnavailable) {
		t.Fatalf("error = %v, want ErrProjectValidationUnavailable", err)
	}
}

func TestProjectServiceRejectsInvalidGitLabRoot(t *testing.T) {
	_, err := NewProjectService(ProjectServiceConfig{
		Store:     openTestStore(t),
		GitLab:    defaultFakeGitLab(),
		Cipher:    newTestCipher(t),
		Profiles:  testProfiles(),
		GitLabURL: "https://user:secret@gitlab.example.com",
	})
	if err == nil {
		t.Fatal("NewProjectService accepted a credential-bearing GitLab root")
	}
}
