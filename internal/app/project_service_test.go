package app

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/secret"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func newProjectService(t *testing.T, st ProjectStore, api gitlab.API, cipher *secret.Cipher, publicURL string) *ProjectService {
	t.Helper()
	service, err := NewProjectService(ProjectServiceConfig{
		Store:     st,
		GitLab:    api,
		Cipher:    cipher,
		Profiles:  testProfiles(),
		PublicURL: publicURL,
	})
	if err != nil {
		t.Fatalf("NewProjectService: %v", err)
	}
	return service
}

func TestRegisterWithoutWebhook(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, cipher, "")

	registration, err := service.Register(ctx, RegisterInput{
		Project:     "group/repo",
		AccessToken: "glpat-project-token",
		Settings:    json.RawMessage(`{"skip_draft": false}`),
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if registration.JobID == "" {
		t.Fatal("job id is empty")
	}
	if registration.Project.ID != 42 || registration.Project.Profile != "default" {
		t.Errorf("project = %+v", registration.Project)
	}
	if registration.WebhookSecret == "" {
		t.Fatal("manual webhook secret must be returned once")
	}
	if len(registration.WebhookSecret) != 43 {
		t.Errorf("webhook secret length = %d, want 43", len(registration.WebhookSecret))
	}
	if len(api.hookSpecs) != 0 {
		t.Errorf("hook was created without auto_webhook: %+v", api.hookSpecs)
	}

	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	token, err := cipher.Decrypt(saved.TokenEnc)
	if err != nil || string(token) != "glpat-project-token" {
		t.Fatalf("decrypt token = %q, %v", token, err)
	}
	secretPlain, err := cipher.Decrypt(saved.WebhookSecretEnc)
	if err != nil || string(secretPlain) != registration.WebhookSecret {
		t.Fatalf("decrypt webhook secret = %q, %v", secretPlain, err)
	}
	if saved.HookID != nil {
		t.Errorf("hook id = %v, want nil", *saved.HookID)
	}
	if saved.WebhookState != store.WebhookStateDisabled || len(saved.WebhookEvents) != 0 {
		t.Errorf("webhook state = %q, events = %v", saved.WebhookState, saved.WebhookEvents)
	}
	if string(saved.Settings) != `{"skip_draft":false}` {
		t.Errorf("settings = %s", saved.Settings)
	}
	if strings.Contains(string(saved.TokenEnc), "glpat") {
		t.Fatal("token was stored in plaintext")
	}

	job, err := st.GetJob(ctx, registration.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Kind != "workspace.onboard" || job.Key != "workspace.onboard:42" || job.State != store.JobQueued {
		t.Errorf("job = %+v", job)
	}
	var payload map[string]int64
	if err := json.Unmarshal(job.Payload, &payload); err != nil || payload["project_id"] != 42 {
		t.Errorf("job payload = %s (%v)", job.Payload, err)
	}

	encoded, err := json.Marshal(registration.Project)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"token_enc", "webhook_secret_enc", "glpat-project-token", registration.WebhookSecret} {
		if strings.Contains(text, forbidden) {
			t.Errorf("serialized project leaked %q: %s", forbidden, text)
		}
	}

	list, err := service.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	got, err := service.Get(ctx, 42)
	if err != nil || got.ID != 42 {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := service.Get(ctx, 999); err == nil {
		t.Fatal("Get unknown project succeeded")
	}
}

func TestRegisterAutoWebhook(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, cipher, "https://review.example.com/")

	registration, err := service.Register(ctx, RegisterInput{
		Project:     "42",
		AccessToken: "glpat-project-token",
		AutoWebhook: true,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if registration.WebhookSecret != "" {
		t.Fatalf("auto webhook must not return the secret: %q", registration.WebhookSecret)
	}
	if len(api.hookSpecs) != 1 {
		t.Fatalf("hook specs = %+v", api.hookSpecs)
	}
	spec := api.hookSpecs[0]
	if spec.URL != "https://review.example.com/webhooks/gitlab" {
		t.Errorf("hook URL = %q", spec.URL)
	}
	if !spec.PushEvents || !spec.MergeRequestsEvents || !spec.NoteEvents || !spec.EnableSSLVerification {
		t.Errorf("hook events = %+v", spec)
	}
	if len(spec.Secret) != 43 {
		t.Errorf("hook secret length = %d", len(spec.Secret))
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if saved.HookID == nil || *saved.HookID != 99 {
		t.Errorf("hook id = %v", saved.HookID)
	}
	if saved.WebhookState != store.WebhookStateEnabled ||
		strings.Join(saved.WebhookEvents, ",") != "push_events,merge_requests_events,note_events" {
		t.Errorf("webhook state = %q, events = %v", saved.WebhookState, saved.WebhookEvents)
	}
	decrypted, err := cipher.Decrypt(saved.WebhookSecretEnc)
	if err != nil || string(decrypted) != spec.Secret {
		t.Fatalf("stored webhook secret does not match created hook (%v)", err)
	}
}

func TestAutoWebhookURLPreservesPublicPath(t *testing.T) {
	tests := []struct {
		publicURL string
		want      string
	}{
		{publicURL: "https://review.example", want: "https://review.example/webhooks/gitlab"},
		{publicURL: "https://review.example/internal/review", want: "https://review.example/internal/review/webhooks/gitlab"},
		{publicURL: "https://review.example/internal/review/", want: "https://review.example/internal/review/webhooks/gitlab"},
		{publicURL: "https://review.example/internal%20review", want: "https://review.example/internal%20review/webhooks/gitlab"},
	}
	for _, tt := range tests {
		t.Run(tt.publicURL, func(t *testing.T) {
			st := openTestStore(t)
			api := defaultFakeGitLab()
			service := newProjectService(t, st, api, newTestCipher(t), tt.publicURL)
			if _, err := service.Register(context.Background(), RegisterInput{
				Project:     "42",
				AccessToken: "glpat-project-token",
				AutoWebhook: true,
			}); err != nil {
				t.Fatalf("Register: %v", err)
			}
			if len(api.hookSpecs) != 1 {
				t.Fatalf("hook specs = %+v", api.hookSpecs)
			}
			if got := api.hookSpecs[0].URL; got != tt.want {
				t.Fatalf("hook URL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRegisterValidation(t *testing.T) {
	expired := time.Now().Add(-48 * time.Hour)
	validInfo := &gitlab.TokenInfo{Scopes: []string{"api", "read_repository"}, Active: true}
	tests := []struct {
		name     string
		input    RegisterInput
		info     *gitlab.TokenInfo
		public   string
		profiles map[string]config.ProfileConfig
		wantCode string
	}{
		{name: "empty token", input: RegisterInput{Project: "42"}, info: validInfo, wantCode: "validation_failed"},
		{name: "invalid project", input: RegisterInput{Project: "https://gitlab.com/group/repo", AccessToken: "t"}, info: validInfo, wantCode: "validation_failed"},
		{name: "unknown profile", input: RegisterInput{Project: "42", AccessToken: "t", Profile: "missing"}, info: validInfo, wantCode: "validation_failed"},
		{name: "unknown settings", input: RegisterInput{Project: "42", AccessToken: "t", Settings: json.RawMessage(`{"bogus":true}`)}, info: validInfo, wantCode: "validation_failed"},
		{name: "settings not object", input: RegisterInput{Project: "42", AccessToken: "t", Settings: json.RawMessage(`[]`)}, info: validInfo, wantCode: "validation_failed"},
		{name: "bad glob", input: RegisterInput{Project: "42", AccessToken: "t", Settings: json.RawMessage(`{"target_branches":["["]}`)}, info: validInfo, wantCode: "validation_failed"},
		{name: "bad label profile", input: RegisterInput{Project: "42", AccessToken: "t", Settings: json.RawMessage(`{"profile_by_label":{"deep-review":"missing"}}`)}, info: validInfo, wantCode: "validation_failed"},
		{name: "auto without public url", input: RegisterInput{Project: "42", AccessToken: "t", AutoWebhook: true}, info: validInfo, wantCode: "validation_failed"},
		{name: "expired", input: RegisterInput{Project: "42", AccessToken: "t"}, info: &gitlab.TokenInfo{Scopes: []string{"api", "read_repository"}, Active: true, ExpiresAt: &expired}, wantCode: "validation_failed"},
		{name: "inactive", input: RegisterInput{Project: "42", AccessToken: "t"}, info: &gitlab.TokenInfo{Scopes: []string{"api", "read_repository"}, Active: false}, wantCode: "validation_failed"},
		{name: "revoked", input: RegisterInput{Project: "42", AccessToken: "t"}, info: &gitlab.TokenInfo{Scopes: []string{"api", "read_repository"}, Active: true, Revoked: true}, wantCode: "validation_failed"},
		{name: "missing api", input: RegisterInput{Project: "42", AccessToken: "t"}, info: &gitlab.TokenInfo{Scopes: []string{"read_repository"}, Active: true}, wantCode: "validation_failed"},
		{name: "missing read_repository", input: RegisterInput{Project: "42", AccessToken: "t"}, info: &gitlab.TokenInfo{Scopes: []string{"api"}, Active: true}, wantCode: "validation_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openTestStore(t)
			api := defaultFakeGitLab()
			if tt.info != nil {
				api.tokenInfo = tt.info
			}
			service := newProjectService(t, st, api, newTestCipher(t), tt.public)
			_, err := service.Register(context.Background(), tt.input)
			if err == nil {
				t.Fatal("expected error")
			}
			var appErr *Error
			if !errors.As(err, &appErr) || appErr.Code != tt.wantCode {
				t.Fatalf("error = %#v, want code %q", err, tt.wantCode)
			}
			if len(api.hookSpecs) != 0 {
				t.Errorf("hook created despite validation failure")
			}
			if _, getErr := st.GetProject(context.Background(), 42); !errors.Is(getErr, store.ErrNotFound) {
				t.Errorf("project was persisted on validation failure: %v", getErr)
			}
		})
	}
}

func TestRegisterUpstreamFailureIsSanitized(t *testing.T) {
	st := openTestStore(t)
	api := defaultFakeGitLab()
	api.projectErr = &gitlab.Error{Op: "get_project", Status: 403}
	service := newProjectService(t, st, api, newTestCipher(t), "")
	_, err := service.Register(context.Background(), RegisterInput{Project: "42", AccessToken: "glpat-secret"})
	if err == nil {
		t.Fatal("expected error")
	}
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "gitlab_rejected" || appErr.Upstream != 403 {
		t.Fatalf("error = %#v", err)
	}
	for _, forbidden := range []string{"glpat-secret", "https://", "upstream"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("error leaked %q: %s", forbidden, err)
		}
	}
}

func TestRegisterCompensation(t *testing.T) {
	tests := []struct {
		name           string
		deleteErr      error
		wantCompensate bool
	}{
		{name: "delete succeeds", wantCompensate: false},
		{name: "delete fails", deleteErr: errors.New("remote down"), wantCompensate: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := openTestStore(t)
			api := defaultFakeGitLab()
			api.deleteErr = tt.deleteErr
			store := &failingProjectStore{ProjectStore: base, err: errors.New("disk full")}
			service := newProjectService(t, store, api, newTestCipher(t), "https://review.example.com")
			_, err := service.Register(context.Background(), RegisterInput{
				Project:     "42",
				AccessToken: "glpat-project",
				AutoWebhook: true,
			})
			var appErr *Error
			if !errors.As(err, &appErr) || appErr.Code != "internal" {
				t.Fatalf("error = %#v", err)
			}
			if appErr.Compensate != tt.wantCompensate {
				t.Errorf("compensate = %v, want %v", appErr.Compensate, tt.wantCompensate)
			}
			if tt.wantCompensate && !strings.Contains(appErr.Message, "cleanup") {
				t.Errorf("compensation failure was not reported: %q", appErr.Message)
			}
			if len(api.deletedHooks) != 1 || api.deletedHooks[0] != 99 {
				t.Errorf("deleted hooks = %v", api.deletedHooks)
			}
		})
	}
}

func TestRegisterConcurrentCreatesSingleHook(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")

	const workers = 4
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := service.Register(ctx, RegisterInput{
				Project:     "42",
				AccessToken: "glpat-project",
				AutoWebhook: true,
			})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
			continue
		}
		var appErr *Error
		if errors.As(err, &appErr) && appErr.Code == "conflict" {
			conflicts++
			continue
		}
		t.Fatalf("unexpected error: %v", err)
	}
	if success != 1 || conflicts != workers-1 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
	if api.callCount("CreateProjectHook") != 1 || len(api.hookSpecs) != 1 {
		t.Fatalf("hook calls = %d", api.callCount("CreateProjectHook"))
	}
}

func TestValidateTokenInfoExpiryDate(t *testing.T) {
	now := time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC)
	today := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	if err := validateTokenInfo(&gitlab.TokenInfo{
		Scopes: []string{"api", "read_repository"}, Active: true, ExpiresAt: &today,
	}, now); err != nil {
		t.Fatalf("token expiring today rejected: %v", err)
	}
	yesterday := today.Add(-24 * time.Hour)
	if err := validateTokenInfo(&gitlab.TokenInfo{
		Scopes: []string{"api", "read_repository"}, Active: true, ExpiresAt: &yesterday,
	}, now); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestRegisterSettingsOverrideKeepsDefaults(t *testing.T) {
	st := openTestStore(t)
	api := defaultFakeGitLab()
	defaults := testDefaults()
	service, err := NewProjectService(ProjectServiceConfig{
		Store:    st,
		GitLab:   api,
		Cipher:   newTestCipher(t),
		Profiles: testProfiles(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Register(context.Background(), RegisterInput{
		Project:     "42",
		AccessToken: "token-long-enough",
		Settings:    json.RawMessage(`{"ignore":["custom/**"],"profile_by_label":{"deep-review":"deep"}}`),
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !reflect.DeepEqual(defaults, testDefaults()) {
		t.Fatalf("review defaults were mutated: %+v", defaults)
	}
	saved, err := st.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveSettings(saved.Settings, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Ignore) != 2 || resolved.Ignore[0] != "**/*.lock" || resolved.Ignore[1] != "custom/**" {
		t.Errorf("resolved ignore = %v", resolved.Ignore)
	}
	if resolved.ProfileByLabel["deep-review"] != "deep" {
		t.Errorf("profile_by_label = %v", resolved.ProfileByLabel)
	}
	if !resolved.SkipDraft || resolved.MaxFiles != 200 {
		t.Errorf("defaults were not preserved: %+v", resolved)
	}
}

func TestRegisterDuplicateNonConcurrent(t *testing.T) {
	st := openTestStore(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, newTestCipher(t), "")
	input := RegisterInput{Project: "42", AccessToken: "token"}
	if _, err := service.Register(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	_, err := service.Register(context.Background(), input)
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Fatalf("error = %#v", err)
	}
}

func TestProjectServiceRequiresDependencies(t *testing.T) {
	if _, err := NewProjectService(ProjectServiceConfig{}); err == nil {
		t.Fatal("expected error")
	}
}

func newWorkspaceProjectService(t *testing.T, st ProjectStore, api gitlab.API, cipher *secret.Cipher, root string) *ProjectService {
	t.Helper()
	service, err := NewProjectService(ProjectServiceConfig{
		Store:     st,
		GitLab:    api,
		Cipher:    cipher,
		Profiles:  testProfiles(),
		Workspace: root,
	})
	if err != nil {
		t.Fatalf("NewProjectService: %v", err)
	}
	return service
}

func TestProjectDeleteRemovesWorkspaceAndManagedSkills(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	root := t.TempDir()
	seedProject(t, st, cipher, 42, "webhook-secret", "", "default")
	service := newWorkspaceProjectService(t, st, defaultFakeGitLab(), cipher, root)

	paths := []string{
		filepath.Join(root, "42"),
		filepath.Join(root, "skills", "project-42"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "project-data"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.Delete(ctx, 42); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s still exists: %v", path, err)
		}
	}
	if _, err := st.GetProject(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("project still registered: %v", err)
	}
}

func TestProjectDeleteRefusesSymlinkedWorkspaceRoot(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	seedProject(t, st, cipher, 42, "webhook-secret", "", "default")

	outside := t.TempDir()
	outsidePaths := []string{
		filepath.Join(outside, "42"),
		filepath.Join(outside, "skills", "project-42"),
	}
	for _, path := range outsidePaths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	service := newWorkspaceProjectService(t, st, defaultFakeGitLab(), cipher, link)

	if err := service.Delete(ctx, 42); err == nil {
		t.Fatal("Delete followed a symlinked workspace root")
	}
	for _, path := range outsidePaths {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("outside path %s was removed: %v", path, err)
		}
	}
}

func TestProjectDeleteRefusesSymlinkedSkillsDir(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	seedProject(t, st, cipher, 42, "webhook-secret", "", "default")

	root := t.TempDir()
	outside := t.TempDir()
	outsideSkill := filepath.Join(outside, "project-42")
	if err := os.MkdirAll(outsideSkill, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "42"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "skills")); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	service := newWorkspaceProjectService(t, st, defaultFakeGitLab(), cipher, root)

	if err := service.Delete(ctx, 42); err == nil {
		t.Fatal("Delete followed a symlinked skills directory")
	}
	if _, err := os.Stat(outsideSkill); err != nil {
		t.Errorf("outside skill %s was removed: %v", outsideSkill, err)
	}
}

func TestProjectDeleteRemovesSymlinkedProjectDirLinkOnly(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	seedProject(t, st, cipher, 42, "webhook-secret", "", "default")

	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "keep")
	if err := os.WriteFile(outsideFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	projectLink := filepath.Join(root, "42")
	if err := os.Symlink(outside, projectLink); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	service := newWorkspaceProjectService(t, st, defaultFakeGitLab(), cipher, root)

	if err := service.Delete(ctx, 42); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Lstat(projectLink); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("project symlink still exists: %v", err)
	}
	if _, err := os.Stat(outsideFile); err != nil {
		t.Errorf("outside content was removed: %v", err)
	}
}
