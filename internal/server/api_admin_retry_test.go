package server

import (
	"net/http"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestAdminRetryOnboarding(t *testing.T) {
	env := newM6Env(t, m6Options{})
	client := env.bearerClient()

	resp := client.json(http.MethodPost, "/api/v1/admin/projects/42/onboarding/retry", nil)
	if resp.status != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", resp.status, resp.body)
	}
	if got := env.decode(t, resp)["job_id"]; got != "job-onboard-retry" {
		t.Fatalf("job_id = %v", got)
	}
	if len(env.workspace.retryCalls) != 1 || env.workspace.retryCalls[0] != 42 {
		t.Fatalf("retry calls = %v", env.workspace.retryCalls)
	}

	resp = client.json(http.MethodPost, "/api/v1/admin/projects/abc/onboarding/retry", nil)
	if resp.status != http.StatusBadRequest {
		t.Fatalf("invalid id status = %d, body=%s", resp.status, resp.body)
	}

	env.workspace.err = store.ErrNotFound
	resp = client.json(http.MethodPost, "/api/v1/admin/projects/99/onboarding/retry", nil)
	if resp.status != http.StatusNotFound {
		t.Fatalf("missing project status = %d, body=%s", resp.status, resp.body)
	}

	resp = env.anonymousClient().json(http.MethodPost, "/api/v1/admin/projects/42/onboarding/retry", nil)
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, body=%s", resp.status, resp.body)
	}
}
