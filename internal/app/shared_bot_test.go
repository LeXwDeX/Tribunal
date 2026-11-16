package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

// sharedBotAPI wraps the standard fake so a test can bind users and access
// levels to specific tokens and user IDs, and observe which credential each
// call used.
type sharedBotAPI struct {
	*fakeGitLab

	mu          sync.Mutex
	users       map[string]*gitlab.User
	userErrs    map[string]error
	access      map[string]map[int64]int
	infoByToken map[string]*gitlab.TokenInfo
	infoErrs    map[string]error
	tokenUses   []string
	counts      map[string]int
}

func newSharedBotAPI() *sharedBotAPI {
	return &sharedBotAPI{
		fakeGitLab:  defaultFakeGitLab(),
		users:       map[string]*gitlab.User{},
		userErrs:    map[string]error{},
		access:      map[string]map[int64]int{},
		infoByToken: map[string]*gitlab.TokenInfo{},
		infoErrs:    map[string]error{},
		tokenUses:   []string{},
		counts:      map[string]int{},
	}
}

func (f *sharedBotAPI) use(name, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenUses = append(f.tokenUses, name+" "+token)
	f.counts[name]++
}

func (f *sharedBotAPI) callCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[name]
}

func (f *sharedBotAPI) usedTokens(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, use := range f.tokenUses {
		if strings.HasPrefix(use, name+" ") {
			out = append(out, strings.TrimPrefix(use, name+" "))
		}
	}
	return out
}

func (f *sharedBotAPI) setAccess(token string, userID int64, level int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.access[token] == nil {
		f.access[token] = map[int64]int{}
	}
	f.access[token][userID] = level
}

func (f *sharedBotAPI) GetProject(ctx context.Context, token, ref string) (*gitlab.Project, error) {
	f.use("GetProject", token)
	return f.fakeGitLab.GetProject(ctx, token, ref)
}

func (f *sharedBotAPI) CurrentUser(ctx context.Context, token string) (*gitlab.User, error) {
	f.use("CurrentUser", token)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.userErrs[token]; err != nil {
		return nil, err
	}
	if user := f.users[token]; user != nil {
		return user, nil
	}
	return f.fakeGitLab.CurrentUser(ctx, token)
}

func (f *sharedBotAPI) GetMemberAccess(ctx context.Context, token string, projectID, userID int64) (int, error) {
	f.use("GetMemberAccess", token)
	f.mu.Lock()
	defer f.mu.Unlock()
	if levels := f.access[token]; levels != nil {
		if level, ok := levels[userID]; ok {
			return level, nil
		}
	}
	return 0, &gitlab.Error{Op: "member_access", Status: 404}
}

func (f *sharedBotAPI) GetTokenInfo(ctx context.Context, token string) (*gitlab.TokenInfo, error) {
	f.use("GetTokenInfo", token)
	f.mu.Lock()
	if err := f.infoErrs[token]; err != nil {
		f.mu.Unlock()
		return nil, err
	}
	if info := f.infoByToken[token]; info != nil {
		f.mu.Unlock()
		return info, nil
	}
	f.mu.Unlock()
	return f.fakeGitLab.GetTokenInfo(ctx, token)
}

func (f *sharedBotAPI) ListProjectHooks(ctx context.Context, token string, projectID int64) ([]gitlab.Hook, error) {
	f.use("ListProjectHooks", token)
	return f.fakeGitLab.ListProjectHooks(ctx, token, projectID)
}

func (f *sharedBotAPI) CreateProjectHook(ctx context.Context, token string, projectID int64, spec gitlab.HookSpec) (int64, error) {
	f.use("CreateProjectHook", token)
	return f.fakeGitLab.CreateProjectHook(ctx, token, projectID, spec)
}

func (f *sharedBotAPI) DeleteProjectHook(ctx context.Context, token string, projectID, hookID int64) error {
	f.use("DeleteProjectHook", token)
	return f.fakeGitLab.DeleteProjectHook(ctx, token, projectID, hookID)
}

const (
	sharedBotToken    = "glpat-shared-bot"
	projectAccessTok  = "glpat-project-token"
	botUserID         = int64(9)
	registrantUserID  = int64(7)
	botUsername       = "tribunal"
	registrantAccess  = reporterAccessLevel
	botMaintainerRole = maintainerAccessLevel
)

func newSharedBotProjectService(t *testing.T, st *sqlite.Store, api gitlab.API, shared string, now func() time.Time) *ProjectService {
	t.Helper()
	// Registration grants manage access to a real user row; the HTTP layer
	// relies on the logged-in user already existing, so seed the registrant.
	if _, err := st.CreateAPIToken(context.Background(), store.TokenUser{
		ID: registrantUserID, Username: "alice", Role: "viewer",
	}, store.APIToken{Name: "seed", TokenHash: make([]byte, 32)}); err != nil {
		t.Fatalf("seed registrant: %v", err)
	}
	service, err := NewProjectService(ProjectServiceConfig{
		Store:          st,
		GitLab:         api,
		Cipher:         newTestCipher(t),
		Profiles:       testProfiles(),
		PublicURL:      "https://review.example.com",
		SharedBotToken: shared,
		Workspace:      workspace.NewManager(t.TempDir(), nil),
		Now:            now,
	})
	if err != nil {
		t.Fatalf("NewProjectService: %v", err)
	}
	return service
}

func configureSharedBot(api *sharedBotAPI) {
	api.users[sharedBotToken] = &gitlab.User{ID: botUserID, Username: botUsername}
	api.setAccess(sharedBotToken, botUserID, botMaintainerRole)
	api.setAccess(sharedBotToken, registrantUserID, registrantAccess)
	api.infoByToken[sharedBotToken] = &gitlab.TokenInfo{
		Scopes: []string{"api", "read_repository"}, Active: true,
	}
}

func assertAppError(t *testing.T, err error, code, field string) {
	t.Helper()
	var appErr *Error
	if !errors.As(err, &appErr) {
		t.Fatalf("error = %v, want *app.Error", err)
	}
	if appErr.Code != code {
		t.Fatalf("error code = %q, want %q (%v)", appErr.Code, code, err)
	}
	if field == "" {
		return
	}
	for _, detail := range appErr.Details {
		if detail.Field == field {
			return
		}
	}
	t.Fatalf("error details = %+v, want field %q", appErr.Details, field)
}

func TestProjectTokenResolver(t *testing.T) {
	cipher := newTestCipher(t)
	projectEnc, err := cipher.Seal([]byte(projectAccessTok), secret.TokenAAD(42))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		resolver *projectTokenResolver
		project  *store.Project
		want     string
		wantErr  error
	}{
		{
			name:     "project token wins over shared",
			resolver: newProjectTokenResolver(cipher, sharedBotToken),
			project:  &store.Project{ID: 42, TokenEnc: projectEnc},
			want:     projectAccessTok,
		},
		{
			name:     "empty sentinel falls back to shared",
			resolver: newProjectTokenResolver(cipher, sharedBotToken),
			project:  &store.Project{ID: 42, TokenEnc: []byte{}},
			want:     sharedBotToken,
		},
		{
			name:     "nil sentinel falls back to shared",
			resolver: newProjectTokenResolver(cipher, sharedBotToken),
			project:  &store.Project{ID: 42},
			want:     sharedBotToken,
		},
		{
			name:     "empty sentinel without shared is a shared-credential error",
			resolver: newProjectTokenResolver(cipher, ""),
			project:  &store.Project{ID: 42, TokenEnc: []byte{}},
			wantErr:  ErrSharedBotCredentialUnavailable,
		},
		{
			name:     "corrupt project token reports a project-token error",
			resolver: newProjectTokenResolver(cipher, sharedBotToken),
			project:  &store.Project{ID: 42, TokenEnc: []byte("not-sealed")},
			wantErr:  ErrProjectTokenUnavailable,
		},
		{
			name:     "nil project is a project-token error",
			resolver: newProjectTokenResolver(cipher, sharedBotToken),
			wantErr:  ErrProjectTokenUnavailable,
		},
		{
			name:    "nil resolver with project token cannot open",
			project: &store.Project{ID: 42, TokenEnc: projectEnc},
			wantErr: ErrProjectTokenUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.resolver.Token(tt.project)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Token() error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("Token() = %q, want %q", got, tt.want)
			}
		})
	}
	if !strings.Contains(ErrProjectTokenUnavailable.Error(), "project") {
		t.Fatalf("project error text = %q", ErrProjectTokenUnavailable)
	}
	if !strings.Contains(ErrSharedBotCredentialUnavailable.Error(), "shared bot") {
		t.Fatalf("shared error text = %q", ErrSharedBotCredentialUnavailable)
	}
}

func TestRegisterSharedCredentialURLOnlyCreatesWebhook(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)

	registration, err := service.Register(ctx, RegisterInput{
		Project:              "42",
		AccessToken:          "",
		AutoWebhook:          true,
		OwnerUserID:          registrantUserID,
		TrustedPlatformAdmin: false,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if registration.JobID == "" || registration.WebhookSecret != "" {
		t.Fatalf("registration = %+v", registration)
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if len(saved.TokenEnc) != 0 || saved.TokenExpiresAt != nil || len(saved.TokenScopes) != 0 {
		t.Fatalf("shared project credential fields = %+v", saved)
	}
	if saved.HookID == nil || *saved.HookID != 99 || saved.WebhookState != store.WebhookStateEnabled {
		t.Fatalf("webhook = %+v", saved)
	}
	if got := api.usedTokens("GetProject"); len(got) != 1 || got[0] != sharedBotToken {
		t.Fatalf("GetProject tokens = %v", got)
	}
	if got := api.usedTokens("CreateProjectHook"); len(got) != 1 || got[0] != sharedBotToken {
		t.Fatalf("CreateProjectHook tokens = %v", got)
	}
	if calls := api.callCount("GetTokenInfo"); calls != 0 {
		t.Fatalf("GetTokenInfo calls = %d, want shared mode to skip project token info", calls)
	}
}

func TestRegisterSharedCredentialRequiresMaintainerBot(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.setAccess(sharedBotToken, botUserID, 30)
	service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)

	_, err := service.Register(ctx, RegisterInput{Project: "42", OwnerUserID: registrantUserID})
	assertAppError(t, err, "validation_failed", "shared_bot_access")
	if _, err := st.GetProject(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project persisted after bot check: %v", err)
	}
	if len(api.hookSpecs) != 0 {
		t.Fatalf("hook created after bot check: %+v", api.hookSpecs)
	}
}

func TestRegisterSharedCredentialRejectsNonMemberRegistrant(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.setAccess(sharedBotToken, registrantUserID, 10)
	service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)

	_, err := service.Register(ctx, RegisterInput{Project: "42", OwnerUserID: registrantUserID})
	assertAppError(t, err, "validation_failed", "access_level")
	if _, err := st.GetProject(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project persisted after registrant denied: %v", err)
	}
}

func TestRegisterSharedCredentialTrustedAdminSkipsRegistrantCheck(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.mu.Lock()
	delete(api.access[sharedBotToken], registrantUserID)
	api.mu.Unlock()
	service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)

	if _, err := service.Register(ctx, RegisterInput{
		Project: "42", OwnerUserID: registrantUserID, TrustedPlatformAdmin: true,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := st.GetProject(ctx, 42); err != nil {
		t.Fatalf("GetProject: %v", err)
	}
}

func TestRegisterSharedCredentialRequiresRegistrantIdentity(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)

	_, err := service.Register(ctx, RegisterInput{Project: "42"})
	assertAppError(t, err, "validation_failed", "token")
	if _, err := st.GetProject(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project persisted without registrant identity: %v", err)
	}
}

func TestRegisterEmptyTokenWithoutSharedCredentialStillRejected(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	service := newSharedBotProjectService(t, st, api, "", nil)

	_, err := service.Register(ctx, RegisterInput{Project: "42"})
	assertAppError(t, err, "validation_failed", "token")
	var appErr *Error
	_ = errors.As(err, &appErr)
	if appErr.Message != "access token is required" {
		t.Fatalf("error message = %q", appErr.Message)
	}
	if len(api.callList()) != 0 {
		t.Fatalf("GitLab calls without a credential: %v", api.callList())
	}
}

func TestRegisterPerProjectTokenOutranksSharedCredential(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service, err := NewProjectService(ProjectServiceConfig{
		Store: st, GitLab: api, Cipher: cipher, Profiles: testProfiles(),
		PublicURL: "https://review.example.com", SharedBotToken: sharedBotToken,
		Workspace: workspace.NewManager(t.TempDir(), nil),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.Register(ctx, RegisterInput{
		Project: "42", AccessToken: projectAccessTok, TrustedPlatformAdmin: true,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := api.usedTokens("GetProject"); len(got) != 1 || got[0] != projectAccessTok {
		t.Fatalf("GetProject tokens = %v", got)
	}
	for _, token := range append(api.usedTokens("CurrentUser"), api.usedTokens("GetTokenInfo")...) {
		if token == sharedBotToken {
			t.Fatalf("shared credential used for a project-token registration: %v", api.tokenUses)
		}
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := cipher.Open(saved.TokenEnc, secret.TokenAAD(42))
	if err != nil || string(plain) != projectAccessTok {
		t.Fatalf("stored token = %q, %v", plain, err)
	}
}

func TestSharedCredentialHealthStatesAndCache(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-48 * time.Hour)
	tests := []struct {
		name      string
		configure func(*sharedBotAPI)
		shared    string
		wantState string
		wantUser  string
	}{
		{name: "not configured", shared: "", wantState: SharedBotStateNotConfigured},
		{
			name: "ok", shared: sharedBotToken,
			configure: func(api *sharedBotAPI) { configureSharedBot(api) },
			wantState: SharedBotStateOK, wantUser: botUsername,
		},
		{
			name: "inactive", shared: sharedBotToken,
			configure: func(api *sharedBotAPI) {
				configureSharedBot(api)
				api.infoByToken[sharedBotToken] = &gitlab.TokenInfo{Scopes: []string{"api", "read_repository"}, Active: false}
			},
			wantState: SharedBotStateExpired,
		},
		{
			name: "expired", shared: sharedBotToken,
			configure: func(api *sharedBotAPI) {
				configureSharedBot(api)
				api.infoByToken[sharedBotToken] = &gitlab.TokenInfo{Scopes: []string{"api", "read_repository"}, Active: true, ExpiresAt: &expired}
			},
			wantState: SharedBotStateExpired,
		},
		{
			name: "missing scope", shared: sharedBotToken,
			configure: func(api *sharedBotAPI) {
				configureSharedBot(api)
				api.infoByToken[sharedBotToken] = &gitlab.TokenInfo{Scopes: []string{"api"}, Active: true}
			},
			wantState: SharedBotStateInsufficient,
		},
		{
			name: "rejected token", shared: sharedBotToken,
			configure: func(api *sharedBotAPI) {
				configureSharedBot(api)
				api.infoErrs = map[string]error{sharedBotToken: &gitlab.Error{Op: "token_info", Status: 401}}
			},
			wantState: SharedBotStateExpired,
		},
		{
			name: "unreachable", shared: sharedBotToken,
			configure: func(api *sharedBotAPI) {
				configureSharedBot(api)
				api.userErrs[sharedBotToken] = &gitlab.Error{Op: "current_user", Status: 500}
			},
			wantState: SharedBotStateUnreachable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newSharedBotAPI()
			if tt.configure != nil {
				tt.configure(api)
			}
			service := newSharedBotProjectService(t, openTestStore(t), api, tt.shared, func() time.Time { return now })
			health := service.SharedBotCredentialHealth(context.Background())
			if health.State != tt.wantState {
				t.Fatalf("state = %q, want %q (%+v)", health.State, tt.wantState, health)
			}
			if health.Configured != (tt.shared != "") {
				t.Fatalf("configured = %v", health.Configured)
			}
			if health.Username != tt.wantUser {
				t.Fatalf("username = %q, want %q", health.Username, tt.wantUser)
			}
			if strings.Contains(health.Username, sharedBotToken) {
				t.Fatal("health leaks the token")
			}
		})
	}
}

func TestSharedCredentialHealthUsesFiveMinuteCache(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := newSharedBotProjectService(t, openTestStore(t), api, sharedBotToken, func() time.Time { return now })

	first := service.SharedBotCredentialHealth(context.Background())
	if first.State != SharedBotStateOK {
		t.Fatalf("first state = %q", first.State)
	}
	if calls := api.callCount("GetTokenInfo"); calls != 1 {
		t.Fatalf("GetTokenInfo calls = %d, want 1", calls)
	}
	now = now.Add(4 * time.Minute)
	second := service.SharedBotCredentialHealth(context.Background())
	if second.CheckedAt != first.CheckedAt {
		t.Fatalf("cached health was reprobed: %+v vs %+v", first, second)
	}
	if calls := api.callCount("GetTokenInfo"); calls != 1 {
		t.Fatalf("GetTokenInfo calls after cache hit = %d", calls)
	}
	now = now.Add(2 * time.Minute)
	service.SharedBotCredentialHealth(context.Background())
	if calls := api.callCount("GetTokenInfo"); calls != 2 {
		t.Fatalf("GetTokenInfo calls after TTL = %d, want 2", calls)
	}
}

func TestMergeSharedBotUsernameFeedsSelfTriggerFilter(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := newSharedBotAPI()
	configureSharedBot(api)

	got := mergeSharedBotUsername(context.Background(), api, sharedBotToken, []string{"ci-bot"}, logger)
	if strings.Join(got, ",") != "ci-bot,"+botUsername {
		t.Fatalf("merged usernames = %v", got)
	}
	if again := mergeSharedBotUsername(context.Background(), api, sharedBotToken, got, logger); len(again) != 2 {
		t.Fatalf("duplicate shared username = %v", again)
	}
	if unchanged := mergeSharedBotUsername(context.Background(), api, "", []string{"ci-bot"}, logger); len(unchanged) != 1 || unchanged[0] != "ci-bot" {
		t.Fatalf("empty token changed usernames: %v", unchanged)
	}
	api.userErrs[sharedBotToken] = &gitlab.Error{Op: "current_user", Status: 500}
	if failed := mergeSharedBotUsername(context.Background(), api, sharedBotToken, []string{"ci-bot"}, logger); len(failed) != 1 {
		t.Fatalf("failed lookup changed usernames: %v", failed)
	}
}

func TestDeniedEnvNamesIncludeSharedBotToken(t *testing.T) {
	cfg := &config.Config{}
	cfg.Secrets.MasterKeyEnv = "REVIEW_MASTER_KEY"
	cfg.GitLab.BotTokenEnv = "TRIBUNAL_BOT_TOKEN"
	names := deniedEnvNames(cfg)
	want := map[string]bool{"REVIEW_MASTER_KEY": false, "TRIBUNAL_BOT_TOKEN": false}
	for _, name := range names {
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("%s missing from denied environment: %v", name, names)
		}
	}
}
