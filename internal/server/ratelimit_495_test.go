package server

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

func newRateLimitEnv(t *testing.T, limits config.LimitsConfig) *m6Env {
	t.Helper()
	workspace := t.TempDir()
	api := defaultStubGitLab()
	return newM6Env(t, m6Options{
		platform: &config.Config{Limits: limits},
		adminFactory: func(st *sqlite.Store) AdminService {
			return testProjectService(t, st, api, workspace)
		},
	})
}

func rateLimitedRegisterBody() map[string]any {
	return map[string]any{"project": "42", "token": "glpat-project-token", "auto_webhook": false}
}

func assertRateLimited(t *testing.T, response apiResponse) {
	t.Helper()
	if response.status != http.StatusTooManyRequests {
		t.Fatalf("status = %d %s, want 429", response.status, response.body)
	}
	raw := response.header.Get("Retry-After")
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 1 {
		t.Fatalf("Retry-After = %q, want a positive integer", raw)
	}
	body := decodeMap(t, response.body)
	envelope, _ := body["error"].(map[string]any)
	if code, _ := envelope["code"].(string); code != "rate_limited" {
		t.Fatalf("error code = %q, body = %s", code, response.body)
	}
}

func TestRegisterRateLimitPerUser(t *testing.T) {
	env := newRateLimitEnv(t, config.LimitsConfig{RegisterPerMinute: 2, ValidatePerMinute: 30})
	alice := env.loginAs("code-admin")

	for i := 0; i < 2; i++ {
		response := alice.json(http.MethodPost, "/api/v1/projects", rateLimitedRegisterBody())
		if response.status == http.StatusTooManyRequests {
			t.Fatalf("request %d was limited too early: %s", i+1, response.body)
		}
	}
	assertRateLimited(t, alice.json(http.MethodPost, "/api/v1/projects", rateLimitedRegisterBody()))

	// Another authenticated user has an independent bucket.
	bob := env.loginAs("code-viewer")
	if response := bob.json(http.MethodPost, "/api/v1/projects", rateLimitedRegisterBody()); response.status == http.StatusTooManyRequests {
		t.Fatalf("another user shared the bucket: %s", response.body)
	}
	// Authentication runs before the limiter: an unauthenticated request is
	// still a 401, never a 429.
	anonymous := env.anonymousClient().json(http.MethodPost, "/api/v1/projects", rateLimitedRegisterBody())
	if anonymous.status != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d %s, want 401", anonymous.status, anonymous.body)
	}
}

func TestRegisterRateLimitBootstrapBearerHasOwnBucket(t *testing.T) {
	env := newRateLimitEnv(t, config.LimitsConfig{RegisterPerMinute: 1, ValidatePerMinute: 30})
	alice := env.loginAs("code-admin")
	if response := alice.json(http.MethodPost, "/api/v1/projects", rateLimitedRegisterBody()); response.status == http.StatusTooManyRequests {
		t.Fatalf("first user request was limited: %s", response.body)
	}
	assertRateLimited(t, alice.json(http.MethodPost, "/api/v1/projects", rateLimitedRegisterBody()))

	bearer := env.bearerClient()
	response := bearer.json(http.MethodPost, "/api/v1/projects", rateLimitedRegisterBody())
	if response.status == http.StatusTooManyRequests {
		t.Fatalf("bootstrap bearer shared the user bucket: %s", response.body)
	}
}

func TestValidateRateLimit(t *testing.T) {
	env := newRateLimitEnv(t, config.LimitsConfig{RegisterPerMinute: 30, ValidatePerMinute: 1})
	client := env.loginAs("code-admin")
	body := map[string]any{"project": "42", "token": "glpat-project-token"}

	first := client.json(http.MethodPost, "/api/v1/projects/validate", body)
	if first.status != http.StatusOK {
		t.Fatalf("first validate = %d %s", first.status, first.body)
	}
	assertRateLimited(t, client.json(http.MethodPost, "/api/v1/projects/validate", body))

	// Registration has its own bucket and is unaffected.
	if response := client.json(http.MethodPost, "/api/v1/projects", rateLimitedRegisterBody()); response.status == http.StatusTooManyRequests {
		t.Fatalf("validate limit leaked into registration: %s", response.body)
	}
}
