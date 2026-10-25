package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/app"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/secret"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/workspace"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

const wiringHead = "cccccccccccccccccccccccccccccccccccccccc"

type wiringGitLab struct {
	mu  sync.Mutex
	mr  *gitlab.MergeRequest
	err error
}

func (f *wiringGitLab) GetMergeRequest(context.Context, string, int64, int64) (*gitlab.MergeRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	copy := *f.mr
	return &copy, nil
}

type wiringEnv struct {
	t       *testing.T
	server  *httptest.Server
	oauth   *httptest.Server
	db      *sqlite.Store
	api     *wiringGitLab
	backend *AppExtensions
}

func seedWiringProject(t *testing.T, db *sqlite.Store, cipher *secret.Cipher) {
	t.Helper()
	token, err := cipher.Encrypt([]byte("glpat-wiring-token"))
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}
	hook, err := cipher.Encrypt([]byte("whsec"))
	if err != nil {
		t.Fatalf("encrypt hook secret: %v", err)
	}
	if _, err := db.RegisterProject(context.Background(), store.Project{
		ID:                42,
		PathWithNamespace: "group/repo",
		WebURL:            "https://gitlab.example.com/group/repo",
		DefaultBranch:     "main",
		TokenEnc:          token,
		TokenScopes:       []string{"api"},
		WebhookSecretEnc:  hook,
		Profile:           "default",
		Settings:          json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("RegisterProject: %v", err)
	}
}

func newWiringEnv(t *testing.T) *wiringEnv {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, t.TempDir()+"/wiring.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cipher, err := secret.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	seedWiringProject(t, db, cipher)

	api := &wiringGitLab{mr: &gitlab.MergeRequest{
		IID: 7, State: "opened", TargetProjectID: 42, SHA: wiringHead,
	}}
	dispatcher, err := app.NewDispatcher(app.DispatcherConfig{
		Store:    db,
		GitLab:   api,
		Cipher:   cipher,
		Profiles: map[string]config.ProfileConfig{"default": {}, "deep": {}},
		Logger:   newTestLogger(io.Discard),
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	contexts, err := app.NewContextService(app.ContextServiceConfig{
		Store:     db,
		Workspace: workspace.NewManager(t.TempDir(), &repo.Git{}),
	})
	if err != nil {
		t.Fatalf("NewContextService: %v", err)
	}

	t.Setenv("WIRING_PROVIDER_KEY", "wiring-provider-secret")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"wiring-model","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(provider.Close)
	providerConfig := config.ProviderConfig{
		Protocol:       "openai-chat",
		BaseURL:        provider.URL + "/v1",
		Model:          "wiring-model",
		APIKeyEnv:      "WIRING_PROVIDER_KEY",
		MaxConcurrency: 1,
	}
	registry, err := llm.NewRegistry(map[string]config.ProviderConfig{"fake": providerConfig})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	extensions, err := app.NewExtensionService(app.ExtensionServiceConfig{
		Store:     db,
		Clients:   registry,
		Workspace: t.TempDir(),
		Logger:    newTestLogger(io.Discard),
	})
	if err != nil {
		t.Fatalf("NewExtensionService: %v", err)
	}
	backend := NewAppExtensions(extensions)

	oauth := newFakeGitLab(t)
	worker := &app.Lifecycle{}
	worker.MarkStarted()
	apiServer, err := New(Config{
		AdminToken: testAdminToken,
		Projects:   stubAdmin{},
		Webhooks:   stubWebhookAcceptor{},
		Database:   db,
		Worker:     worker,
		Logger:     newTestLogger(io.Discard),
		Auth: AuthConfig{
			GitLabURL:       oauth.URL,
			ClientID:        testOAuthClientID,
			ClientSecret:    testOAuthClientSecret,
			RedirectURL:     testOAuthRedirectURL,
			SessionKey:      []byte("0123456789abcdef0123456789abcdef"),
			SessionTTL:      time.Hour,
			BootstrapAdmins: []string{"alice"},
			HTTPClient:      oauth.Client(),
			Now:             time.Now,
		},
		Store:      db,
		Reviews:    dispatcher,
		Workspace:  dispatcher,
		Contexts:   contexts,
		Extensions: backend,
		MCP:        backend,
		LLM:        backend,
		Tokens:     db,
		Access:     db,
		Platform: &config.Config{
			Providers: map[string]config.ProviderConfig{"fake": providerConfig},
			Profiles:  map[string]config.ProfileConfig{"default": {}, "deep": {}},
		},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	server := httptest.NewServer(apiServer.Handler())
	t.Cleanup(func() {
		server.Close()
		oauth.Close()
	})
	return &wiringEnv{t: t, server: server, oauth: oauth, db: db, api: api, backend: backend}
}

func (e *wiringEnv) bearer() *m6Client {
	return &m6Client{t: e.t, base: e.server.URL, client: &http.Client{}, bearer: testAdminToken}
}

// business returns the platform-admin session with an explicit manage grant on
// the wiring project.
func (e *wiringEnv) business() *m6Client {
	e.t.Helper()
	client := e.loginAs("code-admin")
	if err := e.db.SetProjectGrant(context.Background(), 42, 7, store.ProjectAccessManage); err != nil {
		e.t.Fatalf("SetProjectGrant: %v", err)
	}
	return client
}

func (e *wiringEnv) loginAs(code string) *m6Client {
	e.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		e.t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	c := &m6Client{t: e.t, base: e.server.URL, client: client}
	login := c.request(http.MethodGet, "/auth/login", nil, nil)
	if login.status != http.StatusFound {
		e.t.Fatalf("login status = %d, body=%s", login.status, login.body)
	}
	location, err := url.Parse(login.header.Get("Location"))
	if err != nil {
		e.t.Fatalf("parse login location: %v", err)
	}
	callback := c.request(http.MethodGet, "/auth/callback?"+url.Values{
		"code": {code}, "state": {location.Query().Get("state")},
	}.Encode(), nil, nil)
	if callback.status != http.StatusSeeOther {
		e.t.Fatalf("callback status = %d, body=%s", callback.status, callback.body)
	}
	me := c.request(http.MethodGet, "/api/v1/me", nil, nil)
	body := decodeMap(e.t, me.body)
	csrf, _ := body["csrf_token"].(string)
	if csrf == "" {
		e.t.Fatal("login did not return a CSRF token")
	}
	c.csrf = csrf
	return c
}

func (e *wiringEnv) seededWorkflow() string {
	e.t.Helper()
	ctx := context.Background()
	control, err := e.db.GetReviewControl(ctx, 42, 7)
	if err != nil {
		e.t.Fatalf("GetReviewControl: %v", err)
	}
	workflowID, _, err := e.db.CreateReviewWorkflow(ctx, sqlite.ReviewWorkflowRequest{
		JobID: "wiring-workflow-job",
		Spec:  dag.GraphSpec{Nodes: []dag.NodeSpec{{ID: "seed", Kind: "noop"}}},
		Meta: map[string]string{
			"trigger": "api", "base_sha": strings.Repeat("1", 40),
			"fingerprint": "fp", "config_hash": "cfg", "prompt_hash": "prompt",
		},
		ProjectID: 42, MRIID: 7, HeadSHA: control.HeadSHA,
		DossierVersion: 1, DossierData: json.RawMessage(`{"seed":true}`),
		ExpectedGeneration: control.Generation,
	})
	if err != nil {
		e.t.Fatalf("CreateReviewWorkflow: %v", err)
	}
	return workflowID
}

func TestWiringReviewTriggerEnqueuesRun(t *testing.T) {
	env := newWiringEnv(t)
	client := env.business()

	response := client.json(http.MethodPost, "/api/v1/projects/42/merge_requests/7/reviews", map[string]any{
		"profile": "deep", "full": true,
	})
	assertStatus(t, response, http.StatusAccepted)
	body := decodeMap(t, response.body)
	jobID, _ := body["job_id"].(string)
	if jobID == "" {
		t.Fatalf("response has no job_id: %s", response.body)
	}
	job, err := env.db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Kind != "review.run" || job.Key != "review:42:7" || job.State != store.JobQueued {
		t.Fatalf("job = %+v", job)
	}
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{
		"project_id": float64(42),
		"mr_iid":     float64(7),
		"head_sha":   wiringHead,
		"generation": float64(1),
		"profile":    "deep",
		"trigger":    "manual",
		"full":       true,
	}
	for key, value := range expected {
		if payload[key] != value {
			t.Fatalf("payload[%s] = %v, want %v (payload=%v)", key, payload[key], value, payload)
		}
	}
}

func TestWiringReviewTriggerRejectsClosedMR(t *testing.T) {
	env := newWiringEnv(t)
	env.api.mu.Lock()
	env.api.mr.State = "closed"
	env.api.mu.Unlock()

	response := env.business().json(http.MethodPost, "/api/v1/projects/42/merge_requests/7/reviews", nil)
	assertStatus(t, response, http.StatusConflict)
	body := decodeMap(t, response.body)
	errBody, _ := body["error"].(map[string]any)
	if errBody["code"] != "conflict" {
		t.Fatalf("error = %v", body)
	}
	if _, err := env.db.GetReviewControl(context.Background(), 42, 7); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("closed MR created a review control: %v", err)
	}
}

func TestWiringCancelAndRepublishEnqueueJobs(t *testing.T) {
	env := newWiringEnv(t)
	client := env.business()
	trigger := client.json(http.MethodPost, "/api/v1/projects/42/merge_requests/7/reviews", nil)
	assertStatus(t, trigger, http.StatusAccepted)
	workflowID := env.seededWorkflow()

	cancel := client.request(http.MethodPost, "/api/v1/reviews/"+workflowID+"/cancel", nil, nil)
	assertStatus(t, cancel, http.StatusAccepted)
	cancelBody := decodeMap(t, cancel.body)
	cancelJob, err := env.db.GetJob(context.Background(), cancelBody["job_id"].(string))
	if err != nil {
		t.Fatalf("cancel job: %v", err)
	}
	if cancelJob.Kind != "review.cancel" {
		t.Fatalf("cancel kind = %q", cancelJob.Kind)
	}

	republish := client.request(http.MethodPost, "/api/v1/reviews/"+workflowID+"/republish", nil, nil)
	assertStatus(t, republish, http.StatusAccepted)
	republishBody := decodeMap(t, republish.body)
	publishJob, err := env.db.GetJob(context.Background(), republishBody["job_id"].(string))
	if err != nil {
		t.Fatalf("publish job: %v", err)
	}
	if publishJob.Kind != "review.publish" || publishJob.Key != "review.publish:"+workflowID {
		t.Fatalf("publish job = %+v", publishJob)
	}
	var payload map[string]any
	if err := json.Unmarshal(publishJob.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["workflow_id"] != workflowID {
		t.Fatalf("publish payload = %v", payload)
	}
}

func TestWiringWorkspaceRefreshEnqueuesJob(t *testing.T) {
	env := newWiringEnv(t)
	response := env.business().json(http.MethodPost, "/api/v1/projects/42/workspace/refresh", map[string]any{
		"regenerate_context": true, "reindex": false,
	})
	assertStatus(t, response, http.StatusAccepted)
	body := decodeMap(t, response.body)
	job, err := env.db.GetJob(context.Background(), body["job_id"].(string))
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Kind != "workspace.refresh" || job.Key != "ws:42" {
		t.Fatalf("job = %+v", job)
	}
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["ref"] != "refs/heads/main" || payload["branch"] != "main" ||
		payload["regenerate_context"] != true || payload["reindex"] != false {
		t.Fatalf("payload = %v", payload)
	}
}

func TestWiringMCPCRUDRoundtrip(t *testing.T) {
	env := newWiringEnv(t)
	client := env.bearer()

	created := client.json(http.MethodPost, "/api/v1/mcp-servers", map[string]any{
		"scope": "platform",
		"name":  "github",
		"config": map[string]any{
			"command": "npx",
			"env":     map[string]any{"API_KEY": "super-secret"},
		},
	})
	assertStatus(t, created, http.StatusCreated)
	createdBody := decodeMap(t, created.body)
	id, _ := createdBody["id"].(string)
	if id == "" {
		t.Fatalf("created server has no id: %s", created.body)
	}
	rows, err := env.db.ListMCPServers(context.Background(), "platform", nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("stored servers = %+v, %v", rows, err)
	}
	if !strings.Contains(string(rows[0].Config), "super-secret") {
		t.Fatalf("store did not keep the secret: %s", rows[0].Config)
	}

	list := client.request(http.MethodGet, "/api/v1/mcp-servers?scope=platform", nil, nil)
	assertStatus(t, list, http.StatusOK)
	if strings.Contains(string(list.body), "super-secret") {
		t.Fatalf("list leaked a secret: %s", list.body)
	}

	updated := client.json(http.MethodPut, "/api/v1/mcp-servers/"+id, map[string]any{
		"enabled": false,
	})
	assertStatus(t, updated, http.StatusOK)
	if !strings.Contains(string(updated.body), `"enabled":false`) {
		t.Fatalf("update response = %s", updated.body)
	}

	deleted := client.request(http.MethodDelete, "/api/v1/mcp-servers/"+id, nil, nil)
	assertStatus(t, deleted, http.StatusNoContent)
	rows, err = env.db.ListMCPServers(context.Background(), "platform", nil)
	if err != nil || len(rows) != 0 {
		t.Fatalf("servers after delete = %+v, %v", rows, err)
	}
}

func TestWiringProviderTest(t *testing.T) {
	env := newWiringEnv(t)
	response := env.bearer().request(http.MethodPost, "/api/v1/providers/fake/test", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := decodeMap(t, response.body)
	if body["ok"] != true {
		t.Fatalf("provider test = %s", response.body)
	}
	if body["model"] != "wiring-model" {
		t.Fatalf("model = %v", body["model"])
	}
	if _, ok := body["latency_ms"].(float64); !ok {
		t.Fatalf("latency_ms missing: %s", response.body)
	}
	if strings.Contains(string(response.body), "wiring-provider-secret") {
		t.Fatalf("provider response leaked the API key: %s", response.body)
	}
}

func TestWiringAPITokensRequireSession(t *testing.T) {
	env := newWiringEnv(t)

	anonymous := &m6Client{t: t, base: env.server.URL, client: &http.Client{}}
	assertStatus(t, anonymous.request(http.MethodGet, "/api/v1/api-tokens", nil, nil), http.StatusUnauthorized)

	admin := env.loginAs("code-admin")
	created := admin.json(http.MethodPost, "/api/v1/api-tokens", map[string]any{"name": "ci"})
	assertStatus(t, created, http.StatusCreated)
	createdBody := decodeMap(t, created.body)
	plaintext, _ := createdBody["token"].(string)
	if !strings.HasPrefix(plaintext, "crc_") {
		t.Fatalf("token = %v", createdBody["token"])
	}
	id, _ := createdBody["id"].(string)
	if id == "" {
		t.Fatalf("created token has no id: %s", created.body)
	}

	list := admin.request(http.MethodGet, "/api/v1/api-tokens", nil, nil)
	assertStatus(t, list, http.StatusOK)
	if strings.Contains(string(list.body), plaintext) {
		t.Fatalf("list leaked the plaintext token: %s", list.body)
	}

	noCSRF := admin.withoutCSRF().json(http.MethodPost, "/api/v1/api-tokens", map[string]any{"name": "nope"})
	assertStatus(t, noCSRF, http.StatusForbidden)

	viewer := env.loginAs("code-viewer")
	assertStatus(t, viewer.json(http.MethodPost, "/api/v1/api-tokens", map[string]any{"name": "self-service"}), http.StatusCreated)

	deleted := admin.request(http.MethodDelete, "/api/v1/api-tokens/"+id, nil, nil)
	assertStatus(t, deleted, http.StatusNoContent)
	after := admin.request(http.MethodGet, "/api/v1/api-tokens", nil, nil)
	assertStatus(t, after, http.StatusOK)
	afterBody := decodeMap(t, after.body)
	if tokens, _ := afterBody["tokens"].([]any); len(tokens) != 0 {
		t.Fatalf("tokens after delete = %v", tokens)
	}
}

func TestWiringFindingAnnotationQualityLifecycle(t *testing.T) {
	env := newWiringEnv(t)
	ctx := context.Background()

	// Establish the review control used by the shared seededWorkflow fixture.
	trigger := env.business().json(http.MethodPost, "/api/v1/projects/42/merge_requests/7/reviews", nil)
	assertStatus(t, trigger, http.StatusAccepted)
	workflowID := env.seededWorkflow()

	wf, err := env.db.GetWorkflow(ctx, workflowID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	at := time.Now().UTC()
	queued, err := json.Marshal(dag.NodeQueuedPayload{NodeID: "seed", Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	started, err := json.Marshal(dag.NodeStartedPayload{
		NodeID: "seed", Attempt: 1, Provider: "challenger", Model: "m",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Append(ctx, workflowID, wf.GraphRev,
		dag.Event{Type: dag.EventNodeQueued, Payload: queued, At: at},
		dag.Event{Type: dag.EventNodeStarted, Payload: started, At: at},
	); err != nil {
		t.Fatalf("Append node events: %v", err)
	}

	finding := store.Finding{
		ID: "wiring-annotation-f1", OriginNode: "seed", OriginProvider: "proposer",
		Severity: "P1", Category: "logic", Title: "producer title", Body: "body",
		ChallengeNode: "seed", ChallengeStatus: "refuted",
	}
	if err := env.db.ReplaceFindings(ctx, workflowID, []store.Finding{finding}); err != nil {
		t.Fatalf("ReplaceFindings: %v", err)
	}

	findingsURL := "/api/v1/reviews/" + workflowID + "/findings"
	annotationURL := findingsURL + "/" + finding.ID + "/annotation"
	admin := env.loginAs("code-admin")
	getFinding := func() map[string]any {
		response := admin.request(http.MethodGet, findingsURL, nil, nil)
		assertStatus(t, response, http.StatusOK)
		for _, item := range decodeMap(t, response.body)["findings"].([]any) {
			entry := item.(map[string]any)
			if entry["id"] == finding.ID {
				return entry
			}
		}
		t.Fatalf("finding %s not returned", finding.ID)
		return nil
	}
	getQuality := func() map[string]map[string]any {
		response := admin.request(http.MethodGet, "/api/v1/stats?project_id=42", nil, nil)
		assertStatus(t, response, http.StatusOK)
		out := make(map[string]map[string]any)
		for _, item := range decodeMap(t, response.body)["quality_by_provider"].([]any) {
			entry := item.(map[string]any)
			out[entry["provider"].(string)] = entry
		}
		return out
	}

	set := admin.json(http.MethodPut, annotationURL, map[string]any{"annotation": "valid"})
	assertStatus(t, set, http.StatusOK)
	setBody := decodeMap(t, set.body)
	if setBody["annotation"] != "valid" || setBody["annotated_by"] != "alice" {
		t.Fatalf("set response = %v", setBody)
	}
	if _, err := time.Parse(time.RFC3339, setBody["annotated_at"].(string)); err != nil {
		t.Fatalf("annotated_at = %v: %v", setBody["annotated_at"], err)
	}

	listed := getFinding()
	if listed["annotation"] != "valid" || listed["annotated_by"] != "alice" {
		t.Fatalf("listed finding after set = %v", listed)
	}

	quality := getQuality()
	proposer, challenger := quality["proposer"], quality["challenger"]
	if proposer == nil || challenger == nil || len(quality) != 2 {
		t.Fatalf("quality_by_provider = %v", quality)
	}
	if proposer["reviewed"] != 1.0 || proposer["valid"] != 1.0 || proposer["accuracy"] != 1.0 ||
		proposer["overturned"] != 1.0 || proposer["overturned_ratio"] != 1.0 {
		t.Fatalf("proposer quality = %v", proposer)
	}
	if challenger["refuted"] != 1.0 || challenger["refuted_reviewed"] != 1.0 ||
		challenger["false_refuted"] != 1.0 || challenger["false_kill_rate"] != 1.0 {
		t.Fatalf("challenger quality = %v", challenger)
	}

	replacement := finding
	replacement.Title = "replacement title"
	replacement.Annotation = "invalid"
	replacement.AnnotatedBy = "producer"
	if err := env.db.ReplaceFindings(ctx, workflowID, []store.Finding{replacement}); err != nil {
		t.Fatalf("ReplaceFindings same ID: %v", err)
	}
	replaced := getFinding()
	if replaced["title"] != "replacement title" || replaced["annotation"] != "valid" ||
		replaced["annotated_by"] != "alice" {
		t.Fatalf("finding after same-ID replacement = %v", replaced)
	}

	clear := admin.json(http.MethodPut, annotationURL, map[string]any{"annotation": ""})
	assertStatus(t, clear, http.StatusOK)
	cleared := decodeMap(t, clear.body)
	if cleared["annotation"] != "" {
		t.Fatalf("cleared response = %v", cleared)
	}
	if _, ok := cleared["annotated_by"]; ok {
		t.Fatalf("cleared response kept annotated_by: %v", cleared)
	}
	if _, ok := cleared["annotated_at"]; ok {
		t.Fatalf("cleared response kept annotated_at: %v", cleared)
	}
	clearedFinding := getFinding()
	if clearedFinding["annotation"] != "" {
		t.Fatalf("finding after clear = %v", clearedFinding)
	}
	if _, ok := clearedFinding["annotated_by"]; ok {
		t.Fatalf("listed finding kept annotated_by: %v", clearedFinding)
	}
	if _, ok := clearedFinding["annotated_at"]; ok {
		t.Fatalf("listed finding kept annotated_at: %v", clearedFinding)
	}

	quality = getQuality()
	proposer, challenger = quality["proposer"], quality["challenger"]
	if proposer["proposed"] != 1.0 || proposer["reviewed"] != 0.0 || proposer["accuracy"] != nil {
		t.Fatalf("proposer denominator after clear = %v", proposer)
	}
	if challenger["refuted"] != 1.0 || challenger["refuted_reviewed"] != 0.0 || challenger["false_kill_rate"] != nil {
		t.Fatalf("challenger denominator after clear = %v", challenger)
	}
}
