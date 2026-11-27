package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
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

func (f *sharedBotAPI) setHooks(hooks []gitlab.Hook) {
	f.fakeGitLab.mu.Lock()
	defer f.fakeGitLab.mu.Unlock()
	f.hooks = hooks
}

func (f *sharedBotAPI) ListProjectHooks(ctx context.Context, token string, projectID int64) ([]gitlab.Hook, error) {
	f.use("ListProjectHooks", token)
	return f.fakeGitLab.ListProjectHooks(ctx, token, projectID)
}

func (f *sharedBotAPI) CreateProjectHook(ctx context.Context, token string, projectID int64, spec gitlab.HookSpec) (int64, error) {
	f.use("CreateProjectHook", token)
	return f.fakeGitLab.CreateProjectHook(ctx, token, projectID, spec)
}

func (f *sharedBotAPI) UpdateProjectHook(ctx context.Context, token string, projectID, hookID int64, spec gitlab.HookSpec) error {
	f.use("UpdateProjectHook", token)
	return f.fakeGitLab.UpdateProjectHook(ctx, token, projectID, hookID, spec)
}

func (f *sharedBotAPI) DeleteProjectHook(ctx context.Context, token string, projectID, hookID int64) error {
	f.use("DeleteProjectHook", token)
	return f.fakeGitLab.DeleteProjectHook(ctx, token, projectID, hookID)
}

const (
	sharedBotToken    = "glpat-shared-bot"
	provisioningToken = "glpat-provisioning"
	projectAccessTok  = "glpat-project-token"
	botUserID         = int64(9)
	registrantUserID  = int64(7)
	botUsername       = "tribunal"
	registrantAccess  = reporterAccessLevel
	// Reporter is enough for the shared bot to read code/MRs and write review
	// notes; webhook lifecycle uses provisioningToken.
	botReporterRole = reporterAccessLevel
)

func newSharedBotProjectService(t *testing.T, st *sqlite.Store, api gitlab.API, shared string, now func() time.Time) *ProjectService {
	t.Helper()
	return newSharedBotProjectServiceWithProvisioning(t, st, api, shared, provisioningToken, now)
}

func newSharedBotProjectServiceWithProvisioning(t *testing.T, st *sqlite.Store, api gitlab.API, shared, provisioning string, now func() time.Time) *ProjectService {
	t.Helper()
	// Registration grants manage access to a real user row; the HTTP layer
	// relies on the logged-in user already existing, so seed the registrant.
	if _, err := st.CreateAPIToken(context.Background(), store.TokenUser{
		ID: registrantUserID, Username: "alice", Role: "viewer",
	}, store.APIToken{Name: "seed", TokenHash: make([]byte, 32)}); err != nil {
		t.Fatalf("seed registrant: %v", err)
	}
	service, err := NewProjectService(ProjectServiceConfig{
		Store:             st,
		GitLab:            api,
		Cipher:            newTestCipher(t),
		Profiles:          testProfiles(),
		PublicURL:         "https://review.example.com",
		SharedBotToken:    shared,
		ProvisioningToken: provisioning,
		Workspace:         workspace.NewManager(t.TempDir(), nil),
		GitLabURL:         "https://gitlab.example.com",
		ProbeRemote: func(context.Context, string, string) (string, error) {
			return sha40, nil
		},
		ValidationTimeout: 2 * time.Second,
		Now:               now,
	})
	if err != nil {
		t.Fatalf("NewProjectService: %v", err)
	}
	return service
}

func configureSharedBot(api *sharedBotAPI) {
	api.users[sharedBotToken] = &gitlab.User{ID: botUserID, Username: botUsername}
	api.setAccess(sharedBotToken, botUserID, botReporterRole)
	api.setAccess(sharedBotToken, registrantUserID, registrantAccess)
	api.infoByToken[sharedBotToken] = &gitlab.TokenInfo{
		Scopes: []string{"api", "read_repository"}, Active: true,
	}
	// The provisioning credential only needs api for webhook lifecycle calls.
	api.infoByToken[provisioningToken] = &gitlab.TokenInfo{
		Scopes: []string{"api"}, Active: true,
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

func TestHookTokenUsesProvisioningForSharedOnly(t *testing.T) {
	cipher := newTestCipher(t)
	projectEnc, err := cipher.Seal([]byte(projectAccessTok), secret.TokenAAD(42))
	if err != nil {
		t.Fatal(err)
	}
	shared := newProjectTokenResolver(cipher, sharedBotToken).WithProvisioning(provisioningToken)
	if token, err := shared.HookToken(&store.Project{ID: 42, TokenEnc: []byte{}}); err != nil || token != provisioningToken {
		t.Fatalf("shared hook token = %q, %v", token, err)
	}
	if token, err := shared.HookToken(&store.Project{ID: 42, TokenEnc: projectEnc}); err != nil || token != projectAccessTok {
		t.Fatalf("project hook token = %q, %v", token, err)
	}
	withoutProvisioning := newProjectTokenResolver(cipher, sharedBotToken)
	if _, err := withoutProvisioning.HookToken(&store.Project{ID: 42, TokenEnc: []byte{}}); !errors.Is(err, ErrProvisioningCredentialUnavailable) {
		t.Fatalf("missing provisioning error = %v", err)
	}
	withoutShared := newProjectTokenResolver(cipher, "")
	if _, err := withoutShared.HookToken(&store.Project{ID: 42, TokenEnc: []byte{}}); !errors.Is(err, ErrSharedBotCredentialUnavailable) {
		t.Fatalf("missing shared error = %v", err)
	}
	// The review credential never switches to provisioning.
	if token, err := shared.Token(&store.Project{ID: 42, TokenEnc: []byte{}}); err != nil || token != sharedBotToken {
		t.Fatalf("shared review token = %q, %v", token, err)
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
		t.Fatalf("GetProject tokens = %v, want the review bot", got)
	}
	if got := api.usedTokens("CurrentUser"); len(got) != 1 || got[0] != sharedBotToken {
		t.Fatalf("CurrentUser tokens = %v, want the review bot", got)
	}
	if got := api.usedTokens("CreateProjectHook"); len(got) != 1 || got[0] != provisioningToken {
		t.Fatalf("CreateProjectHook tokens = %v, want the provisioning credential", got)
	}
	if got := api.usedTokens("GetTokenInfo"); len(got) != 2 || got[0] != sharedBotToken || got[1] != provisioningToken {
		t.Fatalf("GetTokenInfo calls = %v, want bot then provisioning", got)
	}
	hasGrant, err := st.HasProjectGrant(ctx, registrantUserID, 42)
	if err != nil || !hasGrant {
		t.Fatalf("owner grant = %v, %v; want manage grant", hasGrant, err)
	}
	grants, _, err := st.ListProjectGrants(ctx, 42, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].UserID != registrantUserID || grants[0].Role != store.ProjectAccessManage {
		t.Fatalf("owner grants = %+v, want one manage grant", grants)
	}
}

func TestRegisterSharedCredentialAutoWebhookRequiresProvisioning(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := newSharedBotProjectServiceWithProvisioning(t, st, api, sharedBotToken, "", nil)

	_, err := service.Register(ctx, RegisterInput{Project: "42", OwnerUserID: registrantUserID, AutoWebhook: true})
	assertAppError(t, err, "validation_failed", "provisioning_credential")
	assertRegisterResidueFree(t, ctx, st, api)
	if api.callCount("CreateProjectHook") != 0 {
		t.Fatalf("hook creation was attempted without provisioning: %v", api.tokenUses)
	}
}

func TestRegisterSharedCredentialManualWebhookNeedsNoProvisioning(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := newSharedBotProjectServiceWithProvisioning(t, st, api, sharedBotToken, "", nil)

	registration, err := service.Register(ctx, RegisterInput{
		Project: "42", OwnerUserID: registrantUserID, AutoWebhook: false,
	})
	if err != nil {
		t.Fatalf("Register without provisioning: %v", err)
	}
	if registration.WebhookSecret == "" {
		t.Fatal("manual registration did not return a one-time secret")
	}
	if api.callCount("CreateProjectHook") != 0 {
		t.Fatalf("manual registration created a hook: %v", api.tokenUses)
	}
}

func TestRegisterSharedCredentialRejectsBadProvisioningCredential(t *testing.T) {
	expired := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		info *gitlab.TokenInfo
	}{
		{name: "expired", info: &gitlab.TokenInfo{Scopes: []string{"api"}, Active: true, ExpiresAt: &expired}},
		{name: "inactive", info: &gitlab.TokenInfo{Scopes: []string{"api"}, Active: false}},
		{name: "missing api scope", info: &gitlab.TokenInfo{Scopes: []string{"read_repository"}, Active: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := openTestStore(t)
			api := newSharedBotAPI()
			configureSharedBot(api)
			api.infoByToken[provisioningToken] = tc.info
			now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
			service := newSharedBotProjectService(t, st, api, sharedBotToken, func() time.Time { return now })

			_, err := service.Register(ctx, RegisterInput{Project: "42", OwnerUserID: registrantUserID, AutoWebhook: true})
			assertAppError(t, err, "validation_failed", "provisioning_credential")
			assertRegisterResidueFree(t, ctx, st, api)
			if api.callCount("CreateProjectHook") != 0 {
				t.Fatalf("hook creation was attempted with an unusable provisioning credential")
			}
		})
	}
}

func TestRegisterSharedCredentialRejectedProvisioningNamesPlatformCredential(t *testing.T) {
	ctx := context.Background()
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.infoErrs[provisioningToken] = &gitlab.Error{Op: "token_info", Status: 401}
	service := newSharedBotProjectService(t, openTestStore(t), api, sharedBotToken, nil)

	_, err := service.Register(ctx, RegisterInput{Project: "42", OwnerUserID: registrantUserID, AutoWebhook: true})
	assertAppError(t, err, "validation_failed", "provisioning_credential")
	if strings.Contains(err.Error(), provisioningToken) {
		t.Fatalf("error leaked the provisioning token: %v", err)
	}
}

func TestRegisterPerProjectTokenStillOwnsItsWebhook(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)

	if _, err := service.Register(ctx, RegisterInput{
		Project: "42", AccessToken: projectAccessTok, AutoWebhook: true, TrustedPlatformAdmin: true,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := api.usedTokens("CreateProjectHook"); len(got) != 1 || got[0] != projectAccessTok {
		t.Fatalf("CreateProjectHook tokens = %v, want the project token", got)
	}
	for _, use := range api.tokenUses {
		if strings.HasPrefix(use, "CreateProjectHook "+provisioningToken) {
			t.Fatalf("project-token registration used provisioning: %v", api.tokenUses)
		}
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := cipher.Open(saved.TokenEnc, secret.TokenAAD(42)); err != nil || string(plain) != projectAccessTok {
		t.Fatalf("stored project token = %q, %v", plain, err)
	}
}

func TestSharedWebhookLifecycleUsesProvisioning(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service, saved := registerSharedWithHook(t, ctx, st, api)
	if saved.HookID == nil {
		t.Fatal("shared project has no hook")
	}
	api.setHooks([]gitlab.Hook{{ID: *saved.HookID}})

	if _, err := service.RebuildWebhook(ctx, 42); err != nil {
		t.Fatalf("RebuildWebhook: %v", err)
	}
	for _, call := range []string{"ListProjectHooks", "UpdateProjectHook"} {
		tokens := api.usedTokens(call)
		if len(tokens) != 1 || tokens[0] != provisioningToken {
			t.Fatalf("%s tokens = %v, want provisioning", call, tokens)
		}
	}
	if _, err := service.DisableWebhook(ctx, 42); err != nil {
		t.Fatalf("DisableWebhook: %v", err)
	}
	if tokens := api.usedTokens("DeleteProjectHook"); len(tokens) != 1 || tokens[0] != provisioningToken {
		t.Fatalf("DeleteProjectHook tokens = %v, want provisioning", tokens)
	}
}

func TestSharedCredentialManualEnableRequiresProvisioning(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := newSharedBotProjectServiceWithProvisioning(t, st, api, sharedBotToken, "", nil)
	if _, err := service.Register(ctx, RegisterInput{
		Project: "42", OwnerUserID: registrantUserID, AutoWebhook: false,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err := service.CreateWebhook(ctx, 42)
	assertAppError(t, err, "validation_failed", "provisioning_credential")
	if api.callCount("ListProjectHooks") != 0 {
		t.Fatalf("hook lifecycle ran without provisioning: %v", api.tokenUses)
	}
}

func TestRegisterSharedCredentialRequiresReporterBot(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.setAccess(sharedBotToken, botUserID, 10)
	service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)

	_, err := service.Register(ctx, RegisterInput{Project: "42", OwnerUserID: registrantUserID, AutoWebhook: true})
	assertAppError(t, err, "validation_failed", "project")
	assertRegisterResidueFree(t, ctx, st, api)
}

func TestRegisterSharedCredentialAcceptsReporterBot(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.setAccess(sharedBotToken, botUserID, reporterAccessLevel)
	service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)

	if _, err := service.Register(ctx, RegisterInput{
		Project: "42", OwnerUserID: registrantUserID, AutoWebhook: true,
	}); err != nil {
		t.Fatalf("Register with a Reporter bot: %v", err)
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if saved.HookID == nil {
		t.Fatal("Reporter bot registration did not create a webhook through provisioning")
	}
}

func TestRegisterSharedCredentialRejectsNonMemberRegistrant(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.setAccess(sharedBotToken, registrantUserID, 10)
	service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)

	_, err := service.Register(ctx, RegisterInput{Project: "42", OwnerUserID: registrantUserID, AutoWebhook: true})
	assertAppError(t, err, "validation_failed", "project")
	assertRegisterResidueFree(t, ctx, st, api)
}

// assertRegisterResidueFree proves a failed precheck left no project, owner
// grant, workspace or GitLab hook behind.
func assertRegisterResidueFree(t *testing.T, ctx context.Context, st *sqlite.Store, api *sharedBotAPI) {
	t.Helper()
	if _, err := st.GetProject(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project persisted after failed precheck: %v", err)
	}
	if granted, err := st.HasProjectGrant(ctx, registrantUserID, 42); err != nil || granted {
		t.Fatalf("owner grant after failed precheck = %v, %v", granted, err)
	}
	if projects, err := st.ListProjects(ctx); err != nil || len(projects) != 0 {
		t.Fatalf("projects after failed precheck = %v, %v", projects, err)
	}
	if len(api.hookSpecs) != 0 || api.callCount("CreateProjectHook") != 0 {
		t.Fatalf("hook created after failed precheck: %+v", api.hookSpecs)
	}
}

func TestRegisterSharedCredentialMissingScopeRejectedBeforePersistence(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.infoByToken[sharedBotToken] = &gitlab.TokenInfo{Scopes: []string{"api"}, Active: true}
	service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)

	_, err := service.Register(ctx, RegisterInput{Project: "42", OwnerUserID: registrantUserID, AutoWebhook: true})
	assertAppError(t, err, "validation_failed", "shared_bot_credential")
	assertRegisterResidueFree(t, ctx, st, api)
	// Membership checks must not run once the credential itself is unusable.
	if api.callCount("GetMemberAccess") != 0 {
		t.Fatalf("member checks ran for an unusable shared credential: %v", api.tokenUses)
	}
}

func TestRegisterSharedCredentialExpiredRejectedBeforePersistence(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	expired := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	api.infoByToken[sharedBotToken] = &gitlab.TokenInfo{
		Scopes: []string{"api", "read_repository"}, Active: true, ExpiresAt: &expired,
	}
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	service := newSharedBotProjectService(t, st, api, sharedBotToken, func() time.Time { return now })

	_, err := service.Register(ctx, RegisterInput{Project: "42", OwnerUserID: registrantUserID, AutoWebhook: true})
	assertAppError(t, err, "validation_failed", "shared_bot_credential")
	assertRegisterResidueFree(t, ctx, st, api)
}

func TestRegisterSharedCredentialNonTrustedPrechecksAreIndistinguishable(t *testing.T) {
	cases := []struct {
		name      string
		configure func(*sharedBotAPI)
		input     RegisterInput
	}{
		{
			name: "bot below reporter",
			configure: func(api *sharedBotAPI) {
				api.setAccess(sharedBotToken, botUserID, 10)
			},
			input: RegisterInput{Project: "42", OwnerUserID: registrantUserID, AutoWebhook: true},
		},
		{
			name: "registrant not reporter",
			configure: func(api *sharedBotAPI) {
				api.setAccess(sharedBotToken, registrantUserID, 10)
			},
			input: RegisterInput{Project: "42", OwnerUserID: registrantUserID, AutoWebhook: true},
		},
		{
			name:      "registrant identity missing",
			configure: func(*sharedBotAPI) {},
			input:     RegisterInput{Project: "42", AutoWebhook: true},
		},
	}
	var first *Error
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := openTestStore(t)
			api := newSharedBotAPI()
			configureSharedBot(api)
			tc.configure(api)
			service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)
			_, err := service.Register(ctx, tc.input)
			var appErr *Error
			if !errors.As(err, &appErr) {
				t.Fatalf("error = %v, want *app.Error", err)
			}
			if first == nil {
				first = appErr
				if appErr.Code != "validation_failed" || appErr.Message != "project registration is not permitted" {
					t.Fatalf("unexpected unified error: %#v", appErr)
				}
			}
			if appErr.Code != first.Code || appErr.Message != first.Message ||
				len(appErr.Details) != len(first.Details) || len(appErr.Details) != 1 ||
				appErr.Details[0] != first.Details[0] {
				t.Fatalf("error differs between precheck causes: %#v vs %#v", appErr, first)
			}
			assertRegisterResidueFree(t, ctx, st, api)
		})
	}
}

func TestRegisterSharedCredentialTrustedAdminKeepsDetailedPrecheck(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.setAccess(sharedBotToken, botUserID, 10)
	service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)

	_, err := service.Register(ctx, RegisterInput{
		Project: "42", OwnerUserID: registrantUserID, AutoWebhook: true, TrustedPlatformAdmin: true,
	})
	assertAppError(t, err, "validation_failed", "shared_bot_access")
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

	_, err := service.Register(ctx, RegisterInput{Project: "42", AutoWebhook: true})
	assertAppError(t, err, "validation_failed", "project")
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

func TestValidateSharedCredentialWithoutProjectToken(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)

	result, err := service.Validate(ctx, ValidateProjectInput{Project: "42", OwnerUserID: registrantUserID})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !result.SharedCredential || result.Username != botUsername || result.AccessLevel != botReporterRole {
		t.Fatalf("validation = %+v", result)
	}
	if strings.Join(result.TokenScopes, ",") != "api,read_repository" || result.GitHead != sha40 {
		t.Fatalf("scopes/head = %v/%s", result.TokenScopes, result.GitHead)
	}
	if _, err := st.GetProject(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("validation persisted a project: %v", err)
	}
	if len(api.hookSpecs) != 0 {
		t.Fatalf("validation created a hook: %+v", api.hookSpecs)
	}
	for _, token := range api.usedTokens("GetProject") {
		if token != sharedBotToken {
			t.Fatalf("validation used %q instead of the shared bot", token)
		}
	}
}

func TestValidateSharedCredentialRejectsMissingScope(t *testing.T) {
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.infoByToken[sharedBotToken] = &gitlab.TokenInfo{Scopes: []string{"api"}, Active: true}
	service := newSharedBotProjectService(t, openTestStore(t), api, sharedBotToken, nil)

	_, err := service.Validate(context.Background(), ValidateProjectInput{Project: "42", OwnerUserID: registrantUserID})
	assertAppError(t, err, "validation_failed", "shared_bot_credential")
}

func TestValidateSharedCredentialPrechecksAreIndistinguishable(t *testing.T) {
	cases := []struct {
		name      string
		configure func(*sharedBotAPI)
		input     ValidateProjectInput
	}{
		{
			name:      "bot below reporter",
			configure: func(api *sharedBotAPI) { api.setAccess(sharedBotToken, botUserID, 10) },
			input:     ValidateProjectInput{Project: "42", OwnerUserID: registrantUserID},
		},
		{
			name:      "registrant not reporter",
			configure: func(api *sharedBotAPI) { api.setAccess(sharedBotToken, registrantUserID, 10) },
			input:     ValidateProjectInput{Project: "42", OwnerUserID: registrantUserID},
		},
		{
			name:      "registrant identity missing",
			configure: func(*sharedBotAPI) {},
			input:     ValidateProjectInput{Project: "42"},
		},
	}
	var first *Error
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newSharedBotAPI()
			configureSharedBot(api)
			tc.configure(api)
			service := newSharedBotProjectService(t, openTestStore(t), api, sharedBotToken, nil)
			_, err := service.Validate(context.Background(), tc.input)
			var appErr *Error
			if !errors.As(err, &appErr) {
				t.Fatalf("error = %v, want *app.Error", err)
			}
			if first == nil {
				first = appErr
			}
			if appErr.Code != first.Code || appErr.Message != first.Message ||
				len(appErr.Details) != 1 || first.Details[0] != appErr.Details[0] {
				t.Fatalf("error differs between precheck causes: %#v vs %#v", appErr, first)
			}
		})
	}
}

func TestValidateSharedCredentialTrustedAdminSkipsRegistrantCheck(t *testing.T) {
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.mu.Lock()
	delete(api.access[sharedBotToken], registrantUserID)
	api.mu.Unlock()
	service := newSharedBotProjectService(t, openTestStore(t), api, sharedBotToken, nil)

	result, err := service.Validate(context.Background(), ValidateProjectInput{
		Project: "42", TrustedPlatformAdmin: true,
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !result.SharedCredential || result.AccessLevel != botReporterRole {
		t.Fatalf("validation = %+v", result)
	}
}

func TestValidatePerProjectTokenIsNotShared(t *testing.T) {
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := newSharedBotProjectService(t, openTestStore(t), api, sharedBotToken, nil)

	result, err := service.Validate(context.Background(), ValidateProjectInput{
		Project: "42", AccessToken: projectAccessTok, TrustedPlatformAdmin: true,
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if result.SharedCredential {
		t.Fatalf("project token validation reported shared credential: %+v", result)
	}
	for _, token := range api.usedTokens("GetProject") {
		if token == sharedBotToken {
			t.Fatalf("project token validation used the shared bot: %v", api.tokenUses)
		}
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
			name: "forbidden token", shared: sharedBotToken,
			configure: func(api *sharedBotAPI) {
				configureSharedBot(api)
				api.infoErrs = map[string]error{sharedBotToken: &gitlab.Error{Op: "token_info", Status: 403}}
			},
			wantState: SharedBotStateInsufficient,
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
			if tt.shared == "" && api.callCount("GetTokenInfo") != 0 {
				t.Fatalf("not-configured health called GitLab %d times", api.callCount("GetTokenInfo"))
			}
		})
	}
}

func TestSharedCredentialHealthBareServiceDoesNotPanic(t *testing.T) {
	if health := (&ProjectService{}).SharedBotCredentialHealth(context.Background()); health.State != SharedBotStateNotConfigured {
		t.Fatalf("bare service state = %q", health.State)
	}
	bare := &ProjectService{tokens: newProjectTokenResolver(nil, sharedBotToken)}
	if health := bare.SharedBotCredentialHealth(context.Background()); health.State != SharedBotStateUnreachable {
		t.Fatalf("bare shared service state = %q", health.State)
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

func TestSharedCredentialHealthCachesFailureForTTL(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.infoErrs = map[string]error{sharedBotToken: &gitlab.Error{Op: "token_info", Status: 500}}
	service := newSharedBotProjectService(t, openTestStore(t), api, sharedBotToken, func() time.Time { return now })

	if health := service.SharedBotCredentialHealth(context.Background()); health.State != SharedBotStateUnreachable {
		t.Fatalf("state = %q", health.State)
	}
	now = now.Add(4 * time.Minute)
	service.SharedBotCredentialHealth(context.Background())
	if calls := api.callCount("GetTokenInfo"); calls != 1 {
		t.Fatalf("failed probe was not cached: %d calls", calls)
	}
	now = now.Add(2 * time.Minute)
	service.SharedBotCredentialHealth(context.Background())
	if calls := api.callCount("GetTokenInfo"); calls != 2 {
		t.Fatalf("failed probe was not retried after TTL: %d calls", calls)
	}
}

// gatedHealthAPI blocks the first probe so a second caller must join the
// in-flight call instead of starting another one.
type gatedHealthAPI struct {
	*sharedBotAPI
	entered chan struct{}
	release chan struct{}
	once    sync.Once

	mu      sync.Mutex
	entries int
}

func (f *gatedHealthAPI) GetTokenInfo(ctx context.Context, token string) (*gitlab.TokenInfo, error) {
	f.mu.Lock()
	f.entries++
	f.mu.Unlock()
	f.once.Do(func() { close(f.entered) })
	select {
	case <-f.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return f.sharedBotAPI.GetTokenInfo(ctx, token)
}

func (f *gatedHealthAPI) entryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entries
}

func TestSharedCredentialHealthSharesInflightProbe(t *testing.T) {
	api := newSharedBotAPI()
	configureSharedBot(api)
	gated := &gatedHealthAPI{sharedBotAPI: api, entered: make(chan struct{}), release: make(chan struct{})}
	service := newSharedBotProjectService(t, openTestStore(t), gated, sharedBotToken, nil)

	results := make(chan SharedBotCredentialHealth, 2)
	go func() { results <- service.SharedBotCredentialHealth(context.Background()) }()
	<-gated.entered
	go func() { results <- service.SharedBotCredentialHealth(context.Background()) }()
	close(gated.release)
	for i := 0; i < 2; i++ {
		if health := <-results; health.State != SharedBotStateOK {
			t.Fatalf("state = %q", health.State)
		}
	}
	if entries := gated.entryCount(); entries != 1 {
		t.Fatalf("concurrent cache miss probed %d times, want 1", entries)
	}
}

// cancelAwareHealthAPI makes the first probe wait for its context, so the test
// can cancel it and then prove the result was not cached.
type cancelAwareHealthAPI struct {
	*sharedBotAPI
	entered chan struct{}
	once    sync.Once

	mu    sync.Mutex
	calls int
}

func (f *cancelAwareHealthAPI) GetTokenInfo(ctx context.Context, token string) (*gitlab.TokenInfo, error) {
	f.mu.Lock()
	f.calls++
	first := f.calls == 1
	f.mu.Unlock()
	if first {
		f.once.Do(func() { close(f.entered) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.sharedBotAPI.GetTokenInfo(ctx, token)
}

func (f *cancelAwareHealthAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestSharedCredentialHealthCancellationIsNotCached(t *testing.T) {
	api := newSharedBotAPI()
	configureSharedBot(api)
	cancelAware := &cancelAwareHealthAPI{sharedBotAPI: api, entered: make(chan struct{})}
	service := newSharedBotProjectService(t, openTestStore(t), cancelAware, sharedBotToken, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-cancelAware.entered
		cancel()
	}()
	if health := service.SharedBotCredentialHealth(ctx); health.State != SharedBotStateUnreachable {
		t.Fatalf("cancelled state = %q", health.State)
	}
	if health := service.SharedBotCredentialHealth(context.Background()); health.State != SharedBotStateOK {
		t.Fatalf("state after cancelled probe = %q, want a fresh probe", health.State)
	}
	if calls := cancelAware.callCount(); calls != 2 {
		t.Fatalf("probe calls = %d, want the cancelled result not cached", calls)
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

func TestDeniedEnvNamesIncludeSharedBotAndProvisioningTokens(t *testing.T) {
	cfg := &config.Config{}
	cfg.Secrets.MasterKeyEnv = "REVIEW_MASTER_KEY"
	cfg.GitLab.BotTokenEnv = "TRIBUNAL_BOT_TOKEN"
	cfg.GitLab.ProvisioningTokenEnv = "TRIBUNAL_PROVISIONING_TOKEN"
	names := deniedEnvNames(cfg)
	want := map[string]bool{
		"REVIEW_MASTER_KEY": false, "TRIBUNAL_BOT_TOKEN": false, "TRIBUNAL_PROVISIONING_TOKEN": false,
	}
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

// A private project the shared bot is not a member of answers 404. A trusted
// administrator must learn that the bot's membership is the likely cause
// instead of a bare upstream status.
func TestRegisterSharedCredentialTrustedAdminProjectNotFoundNamesSharedBot(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.fakeGitLab.mu.Lock()
	api.projectErr = &gitlab.Error{Op: "get_project", Status: http.StatusNotFound}
	api.fakeGitLab.mu.Unlock()
	service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)

	_, err := service.Register(ctx, RegisterInput{
		Project: "42", OwnerUserID: registrantUserID, AutoWebhook: true, TrustedPlatformAdmin: true,
	})
	assertAppError(t, err, "gitlab_rejected", "shared_bot_access")
	var appErr *Error
	if errors.As(err, &appErr) && appErr.Upstream != http.StatusNotFound {
		t.Fatalf("upstream = %d, want 404", appErr.Upstream)
	}
	assertRegisterResidueFree(t, ctx, st, api)
}

func TestValidateSharedCredentialTrustedAdminProjectNotFoundNamesSharedBot(t *testing.T) {
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.fakeGitLab.mu.Lock()
	api.projectErr = &gitlab.Error{Op: "get_project", Status: http.StatusNotFound}
	api.fakeGitLab.mu.Unlock()
	service := newSharedBotProjectService(t, openTestStore(t), api, sharedBotToken, nil)

	_, err := service.Validate(context.Background(), ValidateProjectInput{Project: "42", TrustedPlatformAdmin: true})
	assertAppError(t, err, "gitlab_rejected", "shared_bot_access")
}

// The non-trusted answer for an invisible project stays the unified denial so
// a signed-in user cannot probe which projects the shared bot can see.
func TestRegisterSharedCredentialNonTrustedProjectNotFoundStaysUnified(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.fakeGitLab.mu.Lock()
	api.projectErr = &gitlab.Error{Op: "get_project", Status: http.StatusNotFound}
	api.fakeGitLab.mu.Unlock()
	service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)

	_, err := service.Register(ctx, RegisterInput{Project: "42", OwnerUserID: registrantUserID, AutoWebhook: true})
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "validation_failed" || appErr.Message != "project registration is not permitted" {
		t.Fatalf("error = %#v, want unified precheck denial", err)
	}
	for _, detail := range appErr.Details {
		if detail.Field == "shared_bot_access" {
			t.Fatalf("non-trusted denial leaked shared bot detail: %+v", appErr.Details)
		}
	}
}
