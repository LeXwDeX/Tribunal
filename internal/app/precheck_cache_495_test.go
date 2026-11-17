package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

// newCachedSharedProjectService builds the shared-mode service with the
// precheck cache enabled. The TTL is explicit so tests can advance time.
func newCachedSharedProjectService(t *testing.T, st *sqlite.Store, api gitlab.API, now func() time.Time, ttl time.Duration) *ProjectService {
	t.Helper()
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
		SharedBotToken:    sharedBotToken,
		ProvisioningToken: provisioningToken,
		Workspace:         workspace.NewManager(t.TempDir(), nil),
		GitLabURL:         "https://gitlab.example.com",
		ProbeRemote: func(context.Context, string, string) (string, error) {
			return sha40, nil
		},
		ValidationTimeout: 2 * time.Second,
		PrecheckCacheTTL:  ttl,
		Now:               now,
	})
	if err != nil {
		t.Fatalf("NewProjectService: %v", err)
	}
	return service
}

func sharedCachedFixture(t *testing.T) (*sharedBotAPI, *ProjectService, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := newCachedSharedProjectService(t, openTestStore(t), api, func() time.Time { return now }, 45*time.Second)
	return api, service, &now
}

func TestValidateSharedCredentialCacheHitSkipsGitLab(t *testing.T) {
	ctx := context.Background()
	api, service, _ := sharedCachedFixture(t)
	input := ValidateProjectInput{Project: "42", OwnerUserID: registrantUserID}

	first, err := service.Validate(ctx, input)
	if err != nil {
		t.Fatalf("first Validate: %v", err)
	}
	calls := len(api.callList())
	if calls == 0 {
		t.Fatal("first validation made no GitLab calls")
	}

	second, err := service.Validate(ctx, input)
	if err != nil {
		t.Fatalf("second Validate: %v", err)
	}
	if got := len(api.callList()) - calls; got != 0 {
		t.Fatalf("cached validation made %d new GitLab calls: %v", got, api.callList()[calls:])
	}
	if second.ProjectID != first.ProjectID || second.GitHead != first.GitHead ||
		second.Username != first.Username || second.AccessLevel != first.AccessLevel ||
		!second.SharedCredential {
		t.Fatalf("cached validation differs: %+v vs %+v", second, first)
	}
}

func TestValidateSharedCredentialCacheExpiresAfterTTL(t *testing.T) {
	ctx := context.Background()
	api, service, now := sharedCachedFixture(t)
	input := ValidateProjectInput{Project: "42", OwnerUserID: registrantUserID}

	if _, err := service.Validate(ctx, input); err != nil {
		t.Fatalf("first Validate: %v", err)
	}
	*now = now.Add(46 * time.Second)
	if _, err := service.Validate(ctx, input); err != nil {
		t.Fatalf("Validate after TTL: %v", err)
	}
	if calls := api.callCount("GetProject"); calls != 2 {
		t.Fatalf("GetProject calls after TTL = %d, want 2", calls)
	}
}

func TestValidateSharedCredentialPermissionRevokedAfterTTL(t *testing.T) {
	ctx := context.Background()
	api, service, now := sharedCachedFixture(t)
	input := ValidateProjectInput{Project: "42", OwnerUserID: registrantUserID}

	if _, err := service.Validate(ctx, input); err != nil {
		t.Fatalf("first Validate: %v", err)
	}
	api.setAccess(sharedBotToken, botUserID, 10)

	if _, err := service.Validate(ctx, input); err != nil {
		t.Fatalf("cached Validate after revocation: %v", err)
	}
	*now = now.Add(46 * time.Second)
	_, err := service.Validate(ctx, input)
	assertAppError(t, err, "validation_failed", "project")
}

func TestValidateSharedCredentialCacheIsPerUser(t *testing.T) {
	ctx := context.Background()
	api, service, _ := sharedCachedFixture(t)

	if _, err := service.Validate(ctx, ValidateProjectInput{Project: "42", OwnerUserID: registrantUserID}); err != nil {
		t.Fatalf("first Validate: %v", err)
	}
	botUserCalls := api.callCount("CurrentUser")
	botAccessCalls := api.callCount("GetMemberAccess")

	otherUserID := registrantUserID + 100
	api.setAccess(sharedBotToken, otherUserID, registrantAccess)
	if _, err := service.Validate(ctx, ValidateProjectInput{Project: "42", OwnerUserID: otherUserID}); err != nil {
		t.Fatalf("Validate for another user: %v", err)
	}
	if got := api.callCount("CurrentUser"); got != botUserCalls {
		t.Fatalf("bot identity was re-probed for another user: %d -> %d", botUserCalls, got)
	}
	if got := api.callCount("GetMemberAccess"); got != botAccessCalls+1 {
		t.Fatalf("registrant access calls = %d, want %d", got, botAccessCalls+1)
	}
}

func TestRegisterSharedCredentialReusesCachedPrechecks(t *testing.T) {
	ctx := context.Background()
	api, service, _ := sharedCachedFixture(t)

	if _, err := service.Validate(ctx, ValidateProjectInput{Project: "42", OwnerUserID: registrantUserID}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	currentUserCalls := api.callCount("CurrentUser")
	memberCalls := api.callCount("GetMemberAccess")
	tokenInfoCalls := api.callCount("GetTokenInfo")

	if _, err := service.Register(ctx, RegisterInput{Project: "42", OwnerUserID: registrantUserID}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := api.callCount("CurrentUser"); got != currentUserCalls {
		t.Fatalf("registration re-probed shared bot identity: %d -> %d", currentUserCalls, got)
	}
	if got := api.callCount("GetMemberAccess"); got != memberCalls {
		t.Fatalf("registration re-probed membership: %d -> %d", memberCalls, got)
	}
	if got := api.callCount("GetTokenInfo"); got != tokenInfoCalls {
		t.Fatalf("registration re-probed shared token info: %d -> %d", tokenInfoCalls, got)
	}
}

func TestRegisterSharedCredentialCachesProvisioningValidity(t *testing.T) {
	ctx := context.Background()
	api, service, _ := sharedCachedFixture(t)
	input := RegisterInput{Project: "42", OwnerUserID: registrantUserID, AutoWebhook: true}

	if _, err := service.Register(ctx, input); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	provisioningCalls := 0
	for _, token := range api.usedTokens("GetTokenInfo") {
		if token == provisioningToken {
			provisioningCalls++
		}
	}
	if provisioningCalls != 1 {
		t.Fatalf("provisioning GetTokenInfo calls = %d, want 1", provisioningCalls)
	}

	_, err := service.Register(ctx, input)
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Fatalf("second Register error = %v, want conflict", err)
	}
	provisioningCalls = 0
	for _, token := range api.usedTokens("GetTokenInfo") {
		if token == provisioningToken {
			provisioningCalls++
		}
	}
	if provisioningCalls != 1 {
		t.Fatalf("provisioning validity was re-probed: %d calls, want 1", provisioningCalls)
	}
}

func TestProjectWritesInvalidateSharedPrecheckCache(t *testing.T) {
	cases := []struct {
		name string
		// setup registers the project when the write needs an existing row.
		setup func(t *testing.T, service *ProjectService)
		// write performs the state change after the cache is warm.
		write func(t *testing.T, service *ProjectService)
	}{
		{
			name:  "register",
			setup: func(*testing.T, *ProjectService) {},
			write: func(t *testing.T, service *ProjectService) {
				t.Helper()
				if _, err := service.Register(context.Background(), RegisterInput{Project: "42", OwnerUserID: registrantUserID}); err != nil {
					t.Fatalf("Register: %v", err)
				}
			},
		},
		{
			name: "switch to shared credential",
			setup: func(t *testing.T, service *ProjectService) {
				t.Helper()
				if _, err := service.Register(context.Background(), RegisterInput{Project: "42", OwnerUserID: registrantUserID}); err != nil {
					t.Fatalf("Register: %v", err)
				}
			},
			write: func(t *testing.T, service *ProjectService) {
				t.Helper()
				if _, err := service.RotateToken(context.Background(), 42, ""); err != nil {
					t.Fatalf("RotateToken: %v", err)
				}
			},
		},
		{
			name: "disable",
			setup: func(t *testing.T, service *ProjectService) {
				t.Helper()
				if _, err := service.Register(context.Background(), RegisterInput{Project: "42", OwnerUserID: registrantUserID}); err != nil {
					t.Fatalf("Register: %v", err)
				}
			},
			write: func(t *testing.T, service *ProjectService) {
				t.Helper()
				disabled := false
				if _, err := service.Update(context.Background(), 42, UpdateProjectInput{Enabled: &disabled}); err != nil {
					t.Fatalf("Update: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			api, service, _ := sharedCachedFixture(t)
			tc.setup(t, service)
			if _, err := service.Validate(ctx, ValidateProjectInput{Project: "42", OwnerUserID: registrantUserID}); err != nil {
				t.Fatalf("warm Validate: %v", err)
			}
			tc.write(t, service)
			before := api.callCount("GetProject")
			if _, err := service.Validate(ctx, ValidateProjectInput{Project: "42", OwnerUserID: registrantUserID}); err != nil {
				t.Fatalf("Validate after write: %v", err)
			}
			if got := api.callCount("GetProject"); got != before+1 {
				t.Fatalf("write did not invalidate the validation cache: GetProject %d -> %d", before, got)
			}
		})
	}
}
