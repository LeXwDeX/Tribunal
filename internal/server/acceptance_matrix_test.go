package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// Regression tests for the "真实验收矩阵" rows in docs/delivery-readiness.md
// that can be automated against the in-process server.

// A project manage grant alone does not allow host execution configuration:
// creating project MCP servers, Skills or block bindings needs a platform
// admin in addition to manage.
func TestAcceptanceManageGrantCannotConfigureHostExtensions(t *testing.T) {
	env, _ := isolationEnv(t)
	env.grant(8, 43, store.ProjectAccessManage)
	bob := env.loginAs("code-viewer")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(testSkillManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		method string
		path   string
		body   map[string]any
	}{
		"project mcp server": {http.MethodPost, "/api/v1/mcp-servers", map[string]any{
			"scope": "project", "project_id": 43, "name": "server",
			"config": map[string]any{"command": "npx"},
		}},
		"project skill dir": {http.MethodPost, "/api/v1/skills", map[string]any{
			"scope": "project", "project_id": 43, "dir": dir,
		}},
		"project block binding": {http.MethodPut, "/api/v1/block-bindings/review", map[string]any{
			"scope": "project", "project_id": 43, "skills": []string{"go-review"},
			"expected_revision": store.BlockBindingAbsentRevision,
		}},
	} {
		t.Run(name, func(t *testing.T) {
			assertStatus(t, bob.json(tc.method, tc.path, tc.body), http.StatusForbidden)
		})
	}
	if len(env.extensions.servers) != 0 || len(env.extensions.dirs) != 0 || len(env.extensions.bindings) != 0 {
		t.Fatalf("manage-only user changed extensions: servers=%d dirs=%d bindings=%d",
			len(env.extensions.servers), len(env.extensions.dirs), len(env.extensions.bindings))
	}

	// The manage grant still allows reading the project's own extensions.
	assertStatus(t, bob.request(http.MethodGet, "/api/v1/mcp-servers?scope=project&project_id=43", nil, nil), http.StatusOK)
}

// The same user gets the same authorization result through the browser
// session and through a personal API token.
func TestAcceptanceSessionAndAPITokenAuthorizeAlike(t *testing.T) {
	env, fake := isolationEnv(t)
	seedIsolationWorkflow(env, fake, "wf-42", 42)
	seedIsolationWorkflow(env, fake, "wf-43", 43)
	env.grant(8, 43, store.ProjectAccessRead)
	session := env.loginAs("code-viewer")
	created := session.json(http.MethodPost, "/api/v1/api-tokens", map[string]any{"name": "parity"})
	assertStatus(t, created, http.StatusCreated)
	plain, _ := env.decode(t, created)["token"].(string)
	if plain == "" {
		t.Fatal("missing plaintext token")
	}
	token := &m6Client{t: t, base: env.server.URL, client: &http.Client{}, bearer: plain}

	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/api/v1/projects/43", http.StatusOK},
		{http.MethodGet, "/api/v1/projects/42", http.StatusNotFound},
		{http.MethodGet, "/api/v1/reviews/wf-43", http.StatusOK},
		{http.MethodGet, "/api/v1/reviews/wf-42", http.StatusNotFound},
		{http.MethodGet, "/api/v1/reviews/wf-43/nodes/plan/calls", http.StatusOK},
		{http.MethodGet, "/api/v1/reviews/wf-43/nodes/plan/calls?include_payload=true", http.StatusForbidden},
		{http.MethodGet, "/api/v1/stats?project_id=43", http.StatusOK},
		{http.MethodGet, "/api/v1/stats?project_id=42", http.StatusNotFound},
		{http.MethodGet, "/api/v1/mcp-servers?scope=project&project_id=43", http.StatusOK},
		{http.MethodGet, "/api/v1/mcp-servers?scope=platform", http.StatusForbidden},
		{http.MethodGet, "/api/v1/admin/users", http.StatusForbidden},
		{http.MethodPost, "/api/v1/projects/43/merge_requests/7/reviews", http.StatusForbidden},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var viaSession, viaToken apiResponse
			if tc.method == http.MethodGet {
				viaSession = session.request(tc.method, tc.path, nil, nil)
				viaToken = token.request(tc.method, tc.path, nil, nil)
			} else {
				body := map[string]any{}
				viaSession = session.json(tc.method, tc.path, body)
				viaToken = token.json(tc.method, tc.path, body)
			}
			if viaSession.status != tc.want || viaToken.status != tc.want {
				t.Fatalf("session = %d, token = %d, want both %d", viaSession.status, viaToken.status, tc.want)
			}
		})
	}
	sessionList := session.request(http.MethodGet, "/api/v1/reviews", nil, nil)
	tokenList := token.request(http.MethodGet, "/api/v1/reviews", nil, nil)
	assertStatus(t, sessionList, http.StatusOK)
	assertStatus(t, tokenList, http.StatusOK)
	assertReviewIDs(t, env.decode(t, sessionList), []string{"wf-43"})
	assertReviewIDs(t, env.decode(t, tokenList), []string{"wf-43"})
}

// Revoking or disabling a user invalidates the old session and token. A
// later OAuth login works again, never revives the old credentials, and the
// project grant survives.
func TestAcceptanceReloginKeepsGrantWithoutOldCredentials(t *testing.T) {
	for _, tc := range []struct {
		name       string
		invalidate func(*testing.T, *m6Env)
		restore    func(*testing.T, *m6Env)
	}{
		{
			name: "revoke",
			invalidate: func(t *testing.T, env *m6Env) {
				revoked := env.bearerClient().request(http.MethodDelete, "/api/v1/admin/users/8/credentials", nil, nil)
				assertStatus(t, revoked, http.StatusNoContent)
			},
			restore: func(*testing.T, *m6Env) {},
		},
		{
			name: "disable then enable",
			invalidate: func(t *testing.T, env *m6Env) {
				disabled := env.bearerClient().json(http.MethodPatch, "/api/v1/admin/users/8", map[string]any{"disabled": true})
				assertStatus(t, disabled, http.StatusOK)
			},
			restore: func(t *testing.T, env *m6Env) {
				enabled := env.bearerClient().json(http.MethodPatch, "/api/v1/admin/users/8", map[string]any{"disabled": false})
				assertStatus(t, enabled, http.StatusOK)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, _ := isolationEnv(t)
			env.grant(8, 43, store.ProjectAccessRead)
			old := env.loginAs("code-viewer")
			created := old.json(http.MethodPost, "/api/v1/api-tokens", map[string]any{"name": "old"})
			assertStatus(t, created, http.StatusCreated)
			plain, _ := env.decode(t, created)["token"].(string)
			oldToken := &m6Client{t: t, base: env.server.URL, client: &http.Client{}, bearer: plain}
			assertStatus(t, oldToken.request(http.MethodGet, "/api/v1/projects/43", nil, nil), http.StatusOK)

			tc.invalidate(t, env)
			assertStatus(t, old.request(http.MethodGet, "/api/v1/projects/43", nil, nil), http.StatusUnauthorized)
			assertStatus(t, oldToken.request(http.MethodGet, "/api/v1/projects/43", nil, nil), http.StatusUnauthorized)

			tc.restore(t, env)
			assertStatus(t, old.request(http.MethodGet, "/api/v1/projects/43", nil, nil), http.StatusUnauthorized)
			assertStatus(t, oldToken.request(http.MethodGet, "/api/v1/projects/43", nil, nil), http.StatusUnauthorized)

			fresh := env.loginAs("code-viewer")
			assertStatus(t, fresh.request(http.MethodGet, "/api/v1/projects/43", nil, nil), http.StatusOK)
			assertStatus(t, fresh.request(http.MethodGet, "/api/v1/projects/42", nil, nil), http.StatusNotFound)
			assertStatus(t, old.request(http.MethodGet, "/api/v1/projects/43", nil, nil), http.StatusUnauthorized)
			assertStatus(t, oldToken.request(http.MethodGet, "/api/v1/projects/43", nil, nil), http.StatusUnauthorized)
		})
	}
}

// An INCOMPLETE review is shown as INCOMPLETE with a failed commit status;
// the API never maps it to PASS or success.
func TestAcceptanceIncompleteReviewIsNeverShownAsPass(t *testing.T) {
	fake := newFakeReviewStore()
	wf := reviewWorkflow("wf-incomplete", 42, time.Now().UTC())
	wf.Verdict = "INCOMPLETE"
	seedAdminReview(t, fake, wf)
	fake.setPublications(wf.ID, store.Publication{
		ID: "pub-status", Kind: "commit_status",
		PublishKey: "status:" + wf.HeadSHA + ":failed",
		GitlabID:   "ai-review", WorkflowID: wf.ID, UpdatedAt: time.Now().UTC(),
	})
	env := newM6Env(t, m6Options{store: &adminFieldsStore{fakeReviewStore: fake}})
	client := env.businessClient()

	detail := client.request(http.MethodGet, "/api/v1/reviews/"+wf.ID, nil, nil)
	assertStatus(t, detail, http.StatusOK)
	body := env.decode(t, detail)
	if body["verdict"] != "INCOMPLETE" {
		t.Fatalf("detail verdict = %v, want INCOMPLETE", body["verdict"])
	}
	if publications := body["publications"].(map[string]any); publications["commit_status"] != "failed" {
		t.Fatalf("commit_status = %v, want failed", publications["commit_status"])
	}
	if strings.Contains(string(detail.body), `"PASS"`) || strings.Contains(string(detail.body), `"success"`) {
		t.Fatalf("INCOMPLETE detail reports success: %s", detail.body)
	}

	list := client.request(http.MethodGet, "/api/v1/reviews", nil, nil)
	assertStatus(t, list, http.StatusOK)
	reviews := env.decode(t, list)["reviews"].([]any)
	if len(reviews) != 1 || reviews[0].(map[string]any)["verdict"] != "INCOMPLETE" {
		t.Fatalf("list verdicts = %v, want one INCOMPLETE", reviews)
	}
}
