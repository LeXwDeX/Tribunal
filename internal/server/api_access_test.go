package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
)

func isolationEnv(t *testing.T) (*m6Env, *fakeReviewStore) {
	t.Helper()
	fake := newFakeReviewStore()
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{
		store: fake,
		adminFactory: func(st *sqlite.Store) AdminService {
			return testProjectService(t, st, api, t.TempDir())
		},
	})
	registerProjectRow(t, env, 42, "group/alpha")
	registerProjectRow(t, env, 43, "group/beta")
	fake.accessByUser = map[int64]map[int64]bool{
		7: {42: true},
		8: {43: true},
	}
	return env, fake
}

func seedIsolationWorkflow(env *m6Env, fake *fakeReviewStore, id string, projectID int64) {
	fake.setWorkflow(store.Workflow{
		ID: id, Kind: "review", ProjectID: projectID, Trigger: "api", Status: "completed",
		CreatedAt: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC),
	})
	fake.setNodes(id, store.WorkflowNode{WorkflowID: id, ID: "plan", Kind: "plan", Status: "completed"})
	fake.setFindings(id)
	fake.setCalls(id, "plan", store.LLMCall{
		ID: "call-" + id, Turn: 1, Provider: "p", Model: "m",
		Request: json.RawMessage(`{"prompt":"secret"}`), OutputTokens: 1,
	})
}

func TestProjectAccessIsolationTwoUsersTwoProjects(t *testing.T) {
	env, fake := isolationEnv(t)
	seedIsolationWorkflow(env, fake, "wf-42", 42)
	seedIsolationWorkflow(env, fake, "wf-43", 43)
	env.grant(7, 42, store.ProjectAccessManage)
	env.grant(8, 43, store.ProjectAccessRead)

	alice := env.loginAs("code-admin")
	bob := env.loginAs("code-viewer")

	for name, tc := range map[string]struct {
		client *m6Client
		path   string
		want   int
	}{
		"alice own project":       {alice, "/api/v1/projects/42", http.StatusOK},
		"alice foreign project":   {alice, "/api/v1/projects/43", http.StatusNotFound},
		"bob foreign project":     {bob, "/api/v1/projects/42", http.StatusNotFound},
		"bob granted project":     {bob, "/api/v1/projects/43", http.StatusOK},
		"alice foreign review":    {alice, "/api/v1/reviews/wf-43", http.StatusNotFound},
		"bob foreign review":      {bob, "/api/v1/reviews/wf-42", http.StatusNotFound},
		"bob granted review":      {bob, "/api/v1/reviews/wf-43", http.StatusOK},
		"bob foreign node":        {bob, "/api/v1/reviews/wf-42/nodes/plan", http.StatusNotFound},
		"bob foreign findings":    {bob, "/api/v1/reviews/wf-42/findings", http.StatusNotFound},
		"bob foreign calls":       {bob, "/api/v1/reviews/wf-42/nodes/plan/calls", http.StatusNotFound},
		"bob foreign stream":      {bob, "/api/v1/reviews/wf-42/stream", http.StatusNotFound},
		"bob foreign workspace":   {bob, "/api/v1/projects/42/workspace", http.StatusNotFound},
		"bob foreign context":     {bob, "/api/v1/projects/42/context-docs", http.StatusNotFound},
		"bob foreign stats":       {bob, "/api/v1/stats?project_id=42", http.StatusNotFound},
		"bob foreign annotations": {bob, "/api/v1/reviews/wf-42/findings/f1/annotation", http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			var response apiResponse
			if strings.HasSuffix(tc.path, "/annotation") {
				response = tc.client.json(http.MethodPut, tc.path, map[string]any{"annotation": "valid"})
			} else {
				response = tc.client.request(http.MethodGet, tc.path, nil, nil)
			}
			assertStatus(t, response, tc.want)
		})
	}

	aliceList := alice.request(http.MethodGet, "/api/v1/reviews", nil, nil)
	assertStatus(t, aliceList, http.StatusOK)
	assertReviewIDs(t, env.decode(t, aliceList), []string{"wf-42"})
	bobList := bob.request(http.MethodGet, "/api/v1/reviews", nil, nil)
	assertStatus(t, bobList, http.StatusOK)
	assertReviewIDs(t, env.decode(t, bobList), []string{"wf-43"})

	aliceProjects := alice.request(http.MethodGet, "/api/v1/projects", nil, nil)
	assertStatus(t, aliceProjects, http.StatusOK)
	assertProjectAccess(t, env.decode(t, aliceProjects), map[int64]string{42: store.ProjectAccessManage})
	bobProjects := bob.request(http.MethodGet, "/api/v1/projects", nil, nil)
	assertStatus(t, bobProjects, http.StatusOK)
	assertProjectAccess(t, env.decode(t, bobProjects), map[int64]string{43: store.ProjectAccessRead})

	// A cursor is bound to its access scope and cannot be replayed by another
	// user, even when the target project itself is granted.
	env.grant(7, 43, store.ProjectAccessRead)
	env.grant(8, 42, store.ProjectAccessRead)
	head := alice.request(http.MethodGet, "/api/v1/projects?limit=1", nil, nil)
	assertStatus(t, head, http.StatusOK)
	cursor, _ := env.decode(t, head)["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("missing cross-scope cursor")
	}
	cross := bob.request(http.MethodGet, "/api/v1/projects?limit=1&cursor="+cursor, nil, nil)
	assertStatus(t, cross, http.StatusBadRequest)

	// LLM payload requires manage even when the caller can read the project.
	bobReadPayload := bob.request(http.MethodGet,
		"/api/v1/reviews/wf-43/nodes/plan/calls?include_payload=true", nil, nil)
	assertStatus(t, bobReadPayload, http.StatusForbidden)
	bobRead := bob.request(http.MethodGet, "/api/v1/reviews/wf-43/nodes/plan/calls", nil, nil)
	assertStatus(t, bobRead, http.StatusOK)
	alicePayload := alice.request(http.MethodGet,
		"/api/v1/reviews/wf-42/nodes/plan/calls?include_payload=true", nil, nil)
	assertStatus(t, alicePayload, http.StatusOK)

	// The annotation is a write; read access is a 403, absent access a 404.
	bobAnnotate := bob.json(http.MethodPut, "/api/v1/reviews/wf-43/findings/f1/annotation",
		map[string]any{"annotation": "valid"})
	assertStatus(t, bobAnnotate, http.StatusForbidden)

	// Stats always carry the caller's access scope before aggregation.
	bobStats := bob.request(http.MethodGet, "/api/v1/stats", nil, nil)
	assertStatus(t, bobStats, http.StatusOK)
	if fake.lastStats.AccessUserID == nil || *fake.lastStats.AccessUserID != 8 {
		t.Fatalf("stats access user = %v", fake.lastStats.AccessUserID)
	}
	aliceStats := alice.request(http.MethodGet, "/api/v1/stats?project_id=42", nil, nil)
	assertStatus(t, aliceStats, http.StatusOK)
	if fake.lastStats.AccessUserID == nil || *fake.lastStats.AccessUserID != 7 {
		t.Fatalf("stats access user = %v", fake.lastStats.AccessUserID)
	}
	if fake.lastStats.ProjectID == nil || *fake.lastStats.ProjectID != 42 {
		t.Fatalf("stats project filter = %v", fake.lastStats.ProjectID)
	}
}

func assertReviewIDs(t *testing.T, body map[string]any, want []string) {
	t.Helper()
	reviews := body["reviews"].([]any)
	got := make([]string, 0, len(reviews))
	for _, item := range reviews {
		got = append(got, item.(map[string]any)["id"].(string))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("review ids = %v, want %v", got, want)
	}
}

func assertProjectAccess(t *testing.T, body map[string]any, want map[int64]string) {
	t.Helper()
	projects := body["projects"].([]any)
	if len(projects) != len(want) {
		t.Fatalf("projects = %v, want %v", projects, want)
	}
	for _, item := range projects {
		project := item.(map[string]any)
		id := int64(project["id"].(float64))
		if project["access"] != want[id] {
			t.Fatalf("project %d access = %v, want %s", id, project["access"], want[id])
		}
	}
}

func TestExtensionScopeAuthorization(t *testing.T) {
	env, _ := isolationEnv(t)
	env.grant(8, 43, store.ProjectAccessRead)
	alice := env.loginAs("code-admin")
	env.grant(7, 42, store.ProjectAccessManage)
	bob := env.loginAs("code-viewer")

	for name, tc := range map[string]struct {
		client *m6Client
		path   string
		want   int
	}{
		"bob global view":        {bob, "/api/v1/mcp-servers", http.StatusForbidden},
		"bob foreign project":    {bob, "/api/v1/mcp-servers?scope=project&project_id=42", http.StatusNotFound},
		"bob granted project":    {bob, "/api/v1/mcp-servers?scope=project&project_id=43", http.StatusOK},
		"bob platform view":      {bob, "/api/v1/mcp-servers?scope=platform", http.StatusForbidden},
		"bob global bindings":    {bob, "/api/v1/block-bindings", http.StatusForbidden},
		"bob platform bindings":  {bob, "/api/v1/block-bindings?scope=platform", http.StatusForbidden},
		"bob granted skill list": {bob, "/api/v1/skills?scope=project&project_id=43", http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			response := tc.client.request(http.MethodGet, tc.path, nil, nil)
			assertStatus(t, response, tc.want)
		})
	}

	// Host execution configuration is admin-only and additionally needs
	// manage on the project. Read grants cannot create project extensions.
	bobCreate := bob.json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{
		"scope": "project", "project_id": 43, "name": "server",
		"config": map[string]any{"command": "npx"},
	})
	assertStatus(t, bobCreate, http.StatusForbidden)
	bobPlatform := bob.json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{
		"scope": "platform", "name": "server", "config": map[string]any{"command": "npx"},
	})
	assertStatus(t, bobPlatform, http.StatusForbidden)

	// Alice is a platform admin but still needs the manage grant.
	aliceCreate := alice.json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{
		"scope": "project", "project_id": 42, "name": "server",
		"config": map[string]any{"command": "npx"},
	})
	assertStatus(t, aliceCreate, http.StatusCreated)
	aliceOther := alice.json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{
		"scope": "project", "project_id": 43, "name": "other",
		"config": map[string]any{"command": "npx"},
	})
	assertStatus(t, aliceOther, http.StatusNotFound)
}

func TestPublicationStateFilterAndStreamTerminal(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake, streamPoll: 10 * time.Millisecond})
	env.grant(7, 42, store.ProjectAccessManage)
	started := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	fake.setWorkflow(store.Workflow{
		ID: "wf-pending", Kind: "review", ProjectID: 42, Trigger: "api", Status: "completed",
		PublicationState: store.WorkflowPublicationPending, CreatedAt: started,
	})
	fake.setNodes("wf-pending")
	fake.setWorkflow(store.Workflow{
		ID: "wf-published", Kind: "review", ProjectID: 42, Trigger: "api", Status: "completed",
		PublicationState: store.WorkflowPublicationPublished, CreatedAt: started.Add(time.Minute),
	})
	fake.setNodes("wf-published")
	client := env.businessClient()

	detail := client.request(http.MethodGet, "/api/v1/reviews/wf-pending", nil, nil)
	assertStatus(t, detail, http.StatusOK)
	body := env.decode(t, detail)
	if body["publication_state"] != store.WorkflowPublicationPending {
		t.Fatalf("detail publication_state = %v", body["publication_state"])
	}
	if body["access"] != store.ProjectAccessManage {
		t.Fatalf("detail access = %v", body["access"])
	}

	filtered := client.request(http.MethodGet, "/api/v1/reviews?publication_state=pending", nil, nil)
	assertStatus(t, filtered, http.StatusOK)
	assertReviewIDs(t, env.decode(t, filtered), []string{"wf-pending"})
	invalid := client.request(http.MethodGet, "/api/v1/reviews?publication_state=unknown", nil, nil)
	assertStatus(t, invalid, http.StatusBadRequest)

	// A completed workflow with a pending publication stays open until the
	// publication reaches a terminal state.
	_, events := streamEventsAs(t, env, client, "wf-pending")
	time.Sleep(20 * time.Millisecond)
	fake.setWorkflow(store.Workflow{
		ID: "wf-pending", Kind: "review", ProjectID: 42, Trigger: "api", Status: "completed",
		PublicationState: store.WorkflowPublicationFailed,
		PublicationError: "merge request is not available", CreatedAt: started,
	})
	received := drainEvents(t, events)
	if len(received) == 0 {
		t.Fatal("publication event missing")
	}
	last := received[len(received)-1]
	if last.name != "workflow" || !strings.Contains(last.data, `"publication_state":"failed"`) ||
		!strings.Contains(last.data, `"publication_error":"merge request is not available"`) {
		t.Fatalf("terminal publication event = %+v (all=%v)", last, received)
	}

	// A terminal publication closes immediately.
	_, done := streamEventsAs(t, env, client, "wf-published")
	doneEvents := drainEvents(t, done)
	if len(doneEvents) != 1 || !strings.Contains(doneEvents[0].data, `"publication_state":"published"`) {
		t.Fatalf("terminal stream events = %v", doneEvents)
	}
}

func TestReviewStreamClosesOnGrantRevocationAndDisabledUser(t *testing.T) {
	env := newM6Env(t, m6Options{store: newFakeReviewStore(), streamPoll: 10 * time.Millisecond})
	fake := env.store.(*fakeReviewStore)
	env.grant(8, 42, store.ProjectAccessRead)
	fake.setWorkflow(store.Workflow{ID: "wf-live", Kind: "review", ProjectID: 42, Trigger: "api", Status: "running"})
	fake.setNodes("wf-live")
	bob := env.loginAs("code-viewer")

	_, events := streamEventsAs(t, env, bob, "wf-live")
	time.Sleep(20 * time.Millisecond)
	if err := env.sqlite.DeleteProjectGrant(context.Background(), 42, 8); err != nil {
		t.Fatalf("DeleteProjectGrant: %v", err)
	}
	drainEvents(t, events)

	env.grant(8, 42, store.ProjectAccessRead)
	_, disabledEvents := streamEventsAs(t, env, bob, "wf-live")
	time.Sleep(20 * time.Millisecond)
	if err := env.sqlite.SetUserDisabled(context.Background(), 8, true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	drainEvents(t, disabledEvents)
}

func TestBootstrapBearerIsManagementPlaneOnly(t *testing.T) {
	env, fake := isolationEnv(t)
	seedIsolationWorkflow(env, fake, "wf-42", 42)
	env.grant(7, 42, store.ProjectAccessManage)
	bearer := env.bearerClient()

	list := bearer.request(http.MethodGet, "/api/v1/projects", nil, nil)
	assertStatus(t, list, http.StatusOK)
	if projects := env.decode(t, list)["projects"].([]any); len(projects) != 0 {
		t.Fatalf("bootstrap bearer project list = %v", projects)
	}
	for _, path := range []string{
		"/api/v1/projects/42",
		"/api/v1/reviews/wf-42",
		"/api/v1/reviews/wf-42/findings",
		"/api/v1/stats?project_id=42",
		"/api/v1/mcp-servers?scope=project&project_id=42",
	} {
		response := bearer.request(http.MethodGet, path, nil, nil)
		assertStatus(t, response, http.StatusNotFound)
	}
	stats := bearer.request(http.MethodGet, "/api/v1/stats", nil, nil)
	assertStatus(t, stats, http.StatusOK)
	if fake.lastStats.AccessUserID == nil || *fake.lastStats.AccessUserID != 0 {
		t.Fatalf("bootstrap stats access user = %v", fake.lastStats.AccessUserID)
	}
	providers := bearer.request(http.MethodGet, "/api/v1/providers", nil, nil)
	assertStatus(t, providers, http.StatusOK)
	if fake.lastStats.AccessUserID == nil || *fake.lastStats.AccessUserID != 0 {
		t.Fatalf("provider stats access user = %v", fake.lastStats.AccessUserID)
	}
	// Management operations still work without a project grant.
	registered := bearer.json(http.MethodPost, "/api/v1/projects", map[string]any{
		"project": "42", "token": "x",
	})
	if registered.status == http.StatusUnauthorized || registered.status == http.StatusForbidden {
		t.Fatalf("bootstrap registration = %d %s", registered.status, registered.body)
	}
}

func TestProjectRoutesFailClosedWithoutAccessStore(t *testing.T) {
	apiServer, err := New(Config{
		AdminToken: testAdminToken,
		Projects:   stubAdmin{},
		Webhooks:   stubWebhookAcceptor{},
		Logger:     newTestLogger(io.Discard),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	apiServer.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status without access store = %d, want 503", recorder.Code)
	}
}

func TestAdminAccessEndpoints(t *testing.T) {
	env, _ := isolationEnv(t)
	bob := env.loginAs("code-viewer")
	env.grant(7, 42, store.ProjectAccessManage)
	admin := env.bearerClient()

	users := admin.request(http.MethodGet, "/api/v1/admin/users", nil, nil)
	assertStatus(t, users, http.StatusOK)
	if len(env.decode(t, users)["users"].([]any)) < 2 {
		t.Fatalf("users = %s", users.body)
	}

	disabled := admin.json(http.MethodPatch, "/api/v1/admin/users/8", map[string]any{"disabled": true})
	assertStatus(t, disabled, http.StatusOK)
	if env.decode(t, disabled)["disabled"] != true {
		t.Fatalf("disabled response = %s", disabled.body)
	}
	// Disabling revokes bob's session.
	assertStatus(t, bob.request(http.MethodGet, "/api/v1/me", nil, nil), http.StatusUnauthorized)

	enabled := admin.json(http.MethodPatch, "/admin/users/8", map[string]any{"disabled": false})
	assertStatus(t, enabled, http.StatusOK)
	restored := env.loginAs("code-viewer")
	assertStatus(t, restored.request(http.MethodGet, "/api/v1/me", nil, nil), http.StatusOK)

	projects := admin.request(http.MethodGet, "/admin/projects", nil, nil)
	assertStatus(t, projects, http.StatusOK)
	projectItems := env.decode(t, projects)["projects"].([]any)
	if len(projectItems) == 0 {
		t.Fatalf("admin projects = %s", projects.body)
	}
	first := projectItems[0].(map[string]any)
	for _, forbidden := range []string{"settings", "web_url", "profile", "recent_reviews"} {
		if _, present := first[forbidden]; present {
			t.Fatalf("admin project list leaked %q: %v", forbidden, first)
		}
	}

	set := admin.json(http.MethodPut, "/api/v1/admin/projects/42/grants/8", map[string]any{"role": "read"})
	assertStatus(t, set, http.StatusOK)
	grants := admin.request(http.MethodGet, "/api/v1/admin/projects/42/grants", nil, nil)
	assertStatus(t, grants, http.StatusOK)
	grantItems := env.decode(t, grants)["grants"].([]any)
	foundRead := false
	for _, item := range grantItems {
		grant := item.(map[string]any)
		if grant["user_id"] == float64(8) && grant["role"] == "read" {
			foundRead = true
		}
	}
	if !foundRead {
		t.Fatalf("grants = %s", grants.body)
	}

	badRole := admin.json(http.MethodPut, "/api/v1/admin/projects/42/grants/8", map[string]any{"role": "owner"})
	assertStatus(t, badRole, http.StatusBadRequest)
	missingUser := admin.json(http.MethodPut, "/api/v1/admin/projects/42/grants/999", map[string]any{"role": "read"})
	assertStatus(t, missingUser, http.StatusNotFound)

	deleted := admin.request(http.MethodDelete, "/api/v1/admin/projects/42/grants/8", nil, nil)
	assertStatus(t, deleted, http.StatusNoContent)
	revoked := admin.request(http.MethodDelete, "/api/v1/admin/users/8/credentials", nil, nil)
	assertStatus(t, revoked, http.StatusNoContent)
}

func TestDisabledUserCannotLoginOrReuseCredentials(t *testing.T) {
	env := newAuthEnv(t)
	bob := env.loginAs(env.client(), "code-viewer")

	plain := "crc_disabled-user-token"
	sum := sha256.Sum256([]byte(plain))
	if _, err := env.db.CreateAPIToken(context.Background(), store.TokenUser{
		ID: 8, Username: "bob", Name: "Bob Viewer", Role: roleViewer,
	}, store.APIToken{Name: "old", TokenHash: sum[:]}); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	tokenClient := &m6Client{t: t, base: env.server.URL, client: &http.Client{}, bearer: plain}
	assertStatus(t, tokenClient.request(http.MethodGet, "/api/v1/me", nil, nil), http.StatusOK)

	if err := env.db.SetUserDisabled(context.Background(), 8, true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	assertStatus(t, env.request(env.clientWithCookie(bob), http.MethodGet, "/api/v1/me", nil, nil), http.StatusUnauthorized)
	assertStatus(t, tokenClient.request(http.MethodGet, "/api/v1/me", nil, nil), http.StatusUnauthorized)

	// Re-enabling restores login but never revives old credentials.
	if err := env.db.SetUserDisabled(context.Background(), 8, false); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	assertStatus(t, env.request(env.clientWithCookie(bob), http.MethodGet, "/api/v1/me", nil, nil), http.StatusUnauthorized)
	assertStatus(t, tokenClient.request(http.MethodGet, "/api/v1/me", nil, nil), http.StatusUnauthorized)
	_ = env.loginAs(env.client(), "code-viewer")

	if err := env.db.SetUserDisabled(context.Background(), 8, true); err != nil {
		t.Fatalf("disable again: %v", err)
	}
	deniedClient := env.client()
	state, _ := env.startLogin(deniedClient)
	denied := env.callback(deniedClient, "code-viewer", state)
	assertStatus(t, denied, http.StatusForbidden)
}

func TestAPITokenDefaultAndMaximumExpiry(t *testing.T) {
	env := newM6Env(t, m6Options{})
	client := env.loginAs("code-viewer")

	created := client.json(http.MethodPost, "/api/v1/api-tokens", map[string]any{"name": "default"})
	assertStatus(t, created, http.StatusCreated)
	body := env.decode(t, created)
	expires, err := time.Parse(time.RFC3339Nano, body["expires_at"].(string))
	if err != nil {
		t.Fatalf("expires_at: %v", err)
	}
	days := time.Until(expires).Hours() / 24
	if days < 89 || days > 91 {
		t.Fatalf("default expiry = %.2f days", days)
	}
	for _, bad := range []int{0, -1, 366} {
		response := client.json(http.MethodPost, "/api/v1/api-tokens", map[string]any{
			"name": "bad", "expires_in_days": bad,
		})
		assertStatus(t, response, http.StatusBadRequest)
	}
	max := client.json(http.MethodPost, "/api/v1/api-tokens", map[string]any{
		"name": "max", "expires_in_days": 365,
	})
	assertStatus(t, max, http.StatusCreated)
}

func TestAdminRevocationInvalidatesOldCredentials(t *testing.T) {
	env := newAuthEnv(t)
	bobClient := env.client()
	bobSession := env.loginAs(bobClient, "code-viewer")
	me := env.request(bobClient, http.MethodGet, "/api/v1/me", nil, nil)
	csrf := decodeMap(t, me.body)["csrf_token"].(string)
	created := env.request(bobClient, http.MethodPost, "/api/v1/api-tokens",
		[]byte(`{"name":"old"}`), map[string]string{
			"Content-Type": "application/json", csrfHeaderName: csrf,
		})
	if created.status != http.StatusCreated {
		t.Fatalf("create token = %d %s", created.status, created.body)
	}
	plain, _ := decodeMap(t, created.body)["token"].(string)
	if plain == "" {
		t.Fatal("missing plaintext token")
	}

	revoked := env.request(env.client(), http.MethodDelete,
		"/api/v1/admin/users/8/credentials", nil, map[string]string{
			"Authorization": "Bearer " + testAdminToken,
		})
	if revoked.status != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", revoked.status, revoked.body)
	}
	assertStatus(t, env.request(env.clientWithCookie(bobSession), http.MethodGet, "/api/v1/me", nil, nil), http.StatusUnauthorized)
	tokenClient := &http.Client{}
	req, _ := http.NewRequest(http.MethodGet, env.server.URL+"/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	resp, err := tokenClient.Do(req)
	if err != nil {
		t.Fatalf("old token request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token status = %d", resp.StatusCode)
	}
}
