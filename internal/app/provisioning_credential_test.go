package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
)

func TestProvisioningCredentialHealthStates(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-48 * time.Hour)
	future := now.Add(30 * 24 * time.Hour)
	tests := []struct {
		name         string
		configure    func(*sharedBotAPI)
		provisioning string
		wantState    string
		wantExpires  *time.Time
	}{
		{name: "not configured", provisioning: "", wantState: ProvisioningStateNotConfigured},
		{
			name: "ok", provisioning: provisioningToken,
			configure: configureSharedBot,
			wantState: ProvisioningStateOK,
		},
		{
			name: "ok with expiry", provisioning: provisioningToken,
			configure: func(api *sharedBotAPI) {
				configureSharedBot(api)
				api.infoByToken[provisioningToken] = &gitlab.TokenInfo{
					Scopes: []string{"api"}, Active: true, ExpiresAt: &future,
				}
			},
			wantState: ProvisioningStateOK, wantExpires: &future,
		},
		{
			name: "inactive", provisioning: provisioningToken,
			configure: func(api *sharedBotAPI) {
				configureSharedBot(api)
				api.infoByToken[provisioningToken] = &gitlab.TokenInfo{Scopes: []string{"api"}, Active: false}
			},
			wantState: ProvisioningStateExpired,
		},
		{
			name: "expired", provisioning: provisioningToken,
			configure: func(api *sharedBotAPI) {
				configureSharedBot(api)
				api.infoByToken[provisioningToken] = &gitlab.TokenInfo{
					Scopes: []string{"api"}, Active: true, ExpiresAt: &expired,
				}
			},
			wantState: ProvisioningStateExpired, wantExpires: &expired,
		},
		{
			name: "missing api scope", provisioning: provisioningToken,
			configure: func(api *sharedBotAPI) {
				configureSharedBot(api)
				api.infoByToken[provisioningToken] = &gitlab.TokenInfo{
					Scopes: []string{"read_repository"}, Active: true,
				}
			},
			wantState: ProvisioningStateInsufficient,
		},
		{
			name: "rejected token", provisioning: provisioningToken,
			configure: func(api *sharedBotAPI) {
				configureSharedBot(api)
				api.infoErrs = map[string]error{provisioningToken: &gitlab.Error{Op: "token_info", Status: 401}}
			},
			wantState: ProvisioningStateExpired,
		},
		{
			name: "forbidden token", provisioning: provisioningToken,
			configure: func(api *sharedBotAPI) {
				configureSharedBot(api)
				api.infoErrs = map[string]error{provisioningToken: &gitlab.Error{Op: "token_info", Status: 403}}
			},
			wantState: ProvisioningStateInsufficient,
		},
		{
			name: "unreachable", provisioning: provisioningToken,
			configure: func(api *sharedBotAPI) {
				configureSharedBot(api)
				api.infoErrs = map[string]error{provisioningToken: &gitlab.Error{Op: "token_info", Status: 500}}
			},
			wantState: ProvisioningStateUnreachable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newSharedBotAPI()
			if tt.configure != nil {
				tt.configure(api)
			}
			service := newSharedBotProjectServiceWithProvisioning(t, openTestStore(t), api, "", tt.provisioning, func() time.Time { return now })
			health := service.ProvisioningCredentialHealth(context.Background())
			if health.State != tt.wantState {
				t.Fatalf("state = %q, want %q (%+v)", health.State, tt.wantState, health)
			}
			if health.Configured != (tt.provisioning != "") {
				t.Fatalf("configured = %v", health.Configured)
			}
			if tt.wantExpires == nil {
				if health.ExpiresAt != nil {
					t.Fatalf("expires = %v, want nil", health.ExpiresAt)
				}
			} else if health.ExpiresAt == nil || !health.ExpiresAt.Equal(*tt.wantExpires) {
				t.Fatalf("expires = %v, want %v", health.ExpiresAt, tt.wantExpires)
			}
			if tt.provisioning == "" && api.callCount("GetTokenInfo") != 0 {
				t.Fatalf("not-configured health called GitLab %d times", api.callCount("GetTokenInfo"))
			}
			if tt.provisioning != "" {
				encoded, err := json.Marshal(health)
				if err != nil {
					t.Fatalf("marshal health: %v", err)
				}
				if strings.Contains(string(encoded), tt.provisioning) {
					t.Fatalf("health leaks the credential: %s", encoded)
				}
			}
		})
	}
}

func TestProvisioningCredentialHealthBareServiceDoesNotPanic(t *testing.T) {
	if health := (&ProjectService{}).ProvisioningCredentialHealth(context.Background()); health.State != ProvisioningStateNotConfigured {
		t.Fatalf("bare service state = %q", health.State)
	}
	bare := &ProjectService{tokens: newProjectTokenResolver(nil, "").WithProvisioning(provisioningToken)}
	if health := bare.ProvisioningCredentialHealth(context.Background()); health.State != ProvisioningStateUnreachable {
		t.Fatalf("bare provisioning service state = %q", health.State)
	}
}

func TestProvisioningCredentialHealthUsesFiveMinuteCache(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := newSharedBotProjectServiceWithProvisioning(t, openTestStore(t), api, "", provisioningToken, func() time.Time { return now })

	first := service.ProvisioningCredentialHealth(context.Background())
	if first.State != ProvisioningStateOK {
		t.Fatalf("first state = %q", first.State)
	}
	if calls := api.callCount("GetTokenInfo"); calls != 1 {
		t.Fatalf("GetTokenInfo calls = %d, want 1", calls)
	}
	now = now.Add(4 * time.Minute)
	second := service.ProvisioningCredentialHealth(context.Background())
	if second.CheckedAt != first.CheckedAt {
		t.Fatalf("cached health was reprobed: %+v vs %+v", first, second)
	}
	if calls := api.callCount("GetTokenInfo"); calls != 1 {
		t.Fatalf("GetTokenInfo calls after cache hit = %d", calls)
	}
	now = now.Add(2 * time.Minute)
	service.ProvisioningCredentialHealth(context.Background())
	if calls := api.callCount("GetTokenInfo"); calls != 2 {
		t.Fatalf("GetTokenInfo calls after TTL = %d, want 2", calls)
	}
}

func TestProvisioningCredentialHealthCachesFailureForTTL(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.infoErrs = map[string]error{provisioningToken: &gitlab.Error{Op: "token_info", Status: 500}}
	service := newSharedBotProjectServiceWithProvisioning(t, openTestStore(t), api, "", provisioningToken, func() time.Time { return now })

	if health := service.ProvisioningCredentialHealth(context.Background()); health.State != ProvisioningStateUnreachable {
		t.Fatalf("state = %q", health.State)
	}
	now = now.Add(4 * time.Minute)
	service.ProvisioningCredentialHealth(context.Background())
	if calls := api.callCount("GetTokenInfo"); calls != 1 {
		t.Fatalf("failed probe was not cached: %d calls", calls)
	}
	now = now.Add(2 * time.Minute)
	service.ProvisioningCredentialHealth(context.Background())
	if calls := api.callCount("GetTokenInfo"); calls != 2 {
		t.Fatalf("failed probe was not retried after TTL: %d calls", calls)
	}
}

func TestProvisioningCredentialHealthSharesInflightProbe(t *testing.T) {
	api := newSharedBotAPI()
	configureSharedBot(api)
	gated := &gatedHealthAPI{sharedBotAPI: api, entered: make(chan struct{}), release: make(chan struct{})}
	service := newSharedBotProjectServiceWithProvisioning(t, openTestStore(t), gated, "", provisioningToken, nil)

	results := make(chan ProvisioningCredentialHealth, 2)
	go func() { results <- service.ProvisioningCredentialHealth(context.Background()) }()
	<-gated.entered
	go func() { results <- service.ProvisioningCredentialHealth(context.Background()) }()
	close(gated.release)
	for i := 0; i < 2; i++ {
		if health := <-results; health.State != ProvisioningStateOK {
			t.Fatalf("state = %q", health.State)
		}
	}
	if entries := gated.entryCount(); entries != 1 {
		t.Fatalf("concurrent cache miss probed %d times, want 1", entries)
	}
}

func TestProvisioningCredentialHealthCancellationIsNotCached(t *testing.T) {
	api := newSharedBotAPI()
	configureSharedBot(api)
	cancelAware := &cancelAwareHealthAPI{sharedBotAPI: api, entered: make(chan struct{})}
	service := newSharedBotProjectServiceWithProvisioning(t, openTestStore(t), cancelAware, "", provisioningToken, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-cancelAware.entered
		cancel()
	}()
	if health := service.ProvisioningCredentialHealth(ctx); health.State != ProvisioningStateUnreachable {
		t.Fatalf("cancelled state = %q", health.State)
	}
	if health := service.ProvisioningCredentialHealth(context.Background()); health.State != ProvisioningStateOK {
		t.Fatalf("state after cancelled probe = %q, want a fresh probe", health.State)
	}
	if calls := cancelAware.callCount(); calls != 2 {
		t.Fatalf("probe calls = %d, want the cancelled result not cached", calls)
	}
}
