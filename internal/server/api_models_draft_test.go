package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

const draftChatOK = `{"id":"x","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

type draftUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	auths []string
	fail  bool
}

func newDraftUpstream(t *testing.T) *draftUpstream {
	t.Helper()
	u := &draftUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.auths = append(u.auths, r.Header.Get("Authorization")+"|"+r.Header.Get("x-api-key"))
		fail := u.fail
		u.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"upstream-secret-body"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(draftChatOK))
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *draftUpstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.auths...)
}

// storedKeyModelStore serves a stored credential for one saved model.
type storedKeyModelStore struct {
	fakeModelStore
	id, key string
	baseURL string
	deleted bool
}

func (f *storedKeyModelStore) GetModel(_ context.Context, id string) (*store.Model, error) {
	if id != f.id {
		return nil, store.ErrNotFound
	}
	m := &store.Model{ID: id, Protocol: "openai-chat", BaseURL: f.baseURL}
	if f.deleted {
		now := time.Now()
		m.DeletedAt = &now
	}
	return m, nil
}

func (f *storedKeyModelStore) OpenModelCredential(_ context.Context, id string) ([]byte, error) {
	if id != f.id {
		return nil, store.ErrNotFound
	}
	return []byte(f.key), nil
}

func draftBody(baseURL string, extra string) string {
	return `{"display_name":"draft","protocol":"openai-chat","base_url":"` + baseURL + `","model":"m","params":{"max_tokens":16,"max_concurrency":1}` + extra + `}`
}

func postDraft(server *Server, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/models:test", strings.NewReader(body))
	response := httptest.NewRecorder()
	server.handleTestModelDraft(response, request)
	return response
}

func TestDraftModelTestReturnsResultWithoutPersisting(t *testing.T) {
	upstream := newDraftUpstream(t)
	ms := &fakeModelStore{}
	catalog, err := llm.NewCatalog(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, logs := newModelTestServer(t, catalog, ms)
	refreshed := false
	server.refreshModels = func(context.Context) error { refreshed = true; return nil }
	response := postDraft(server, draftBody(upstream.URL, `,"api_key":"draft-key-canary"`))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", response.Code, response.Body.String())
	}
	var result struct {
		OK        bool   `json:"ok"`
		Model     string `json:"model"`
		LatencyMS *int64 `json:"latency_ms"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || !result.OK || result.Model != "upstream-model" || result.LatencyMS == nil {
		t.Fatalf("result = %s (%v), want model upstream-model and latency_ms", response.Body.String(), err)
	}
	if ms.createdCredential != "" || refreshed || len(catalog.List()) != 0 {
		t.Fatal("draft test persisted or refreshed state")
	}
	if seen := upstream.seen(); len(seen) != 1 || !strings.Contains(seen[0], "draft-key-canary") {
		t.Fatalf("upstream requests = %v, want one carrying the draft key", seen)
	}
	if strings.Contains(response.Body.String(), "draft-key-canary") || strings.Contains(logs.String(), "draft-key-canary") {
		t.Fatal("draft key leaked into response or logs")
	}
	if !strings.Contains(logs.String(), `"action":"test_draft"`) || !strings.Contains(logs.String(), "platform_audit") {
		t.Fatalf("audit log missing: %s", logs.String())
	}
}

func TestDraftModelTestValidationReturnsFieldDetails(t *testing.T) {
	catalog, err := llm.NewCatalog(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, _ := newModelTestServer(t, catalog, &fakeModelStore{})
	response := postDraft(server, `{"display_name":"","protocol":"nope","base_url":"https://example.com","model":"","params":{"max_tokens":-1,"max_concurrency":1},"api_key":"k"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Details []struct {
				Field string `json:"field"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	fields := map[string]bool{}
	for _, d := range body.Error.Details {
		fields[d.Field] = true
	}
	for _, want := range []string{"display_name", "protocol", "model", "params.max_tokens"} {
		if !fields[want] {
			t.Errorf("details %v missing field %q (body %s)", fields, want, response.Body.String())
		}
	}
}

func TestDraftModelTestStoredKeyWhenAPIKeyOmitted(t *testing.T) {
	upstream := newDraftUpstream(t)
	ms := &storedKeyModelStore{id: "user:saved", key: "stored-key-canary", baseURL: upstream.URL}
	catalog, err := llm.NewCatalog(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, logs := newModelTestServer(t, catalog, ms)
	response := postDraft(server, draftBody(upstream.URL, `,"id":"user:saved"`))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", response.Code, response.Body.String())
	}
	if seen := upstream.seen(); len(seen) != 1 || !strings.Contains(seen[0], "stored-key-canary") {
		t.Fatalf("upstream requests = %v, want one carrying the stored key", seen)
	}
	if strings.Contains(response.Body.String(), "stored-key-canary") || strings.Contains(logs.String(), "stored-key-canary") {
		t.Fatal("stored key leaked into response or logs")
	}
}

func TestDraftModelTestExplicitKeyOverridesStoredKey(t *testing.T) {
	upstream := newDraftUpstream(t)
	ms := &storedKeyModelStore{id: "user:saved", key: "stored-key-canary", baseURL: upstream.URL}
	catalog, _ := llm.NewCatalog(context.Background(), nil, nil)
	server, _ := newModelTestServer(t, catalog, ms)
	response := postDraft(server, draftBody(upstream.URL, `,"id":"user:saved","api_key":"new-key"`))
	if seen := upstream.seen(); response.Code != http.StatusOK || len(seen) != 1 || !strings.Contains(seen[0], "new-key") || strings.Contains(seen[0], "stored-key-canary") {
		t.Fatalf("status=%d upstream=%v, want the submitted key only", response.Code, seen)
	}
}

func TestDraftModelTestUnknownIDReturnsNotFound(t *testing.T) {
	upstream := newDraftUpstream(t)
	catalog, _ := llm.NewCatalog(context.Background(), nil, nil)
	server, _ := newModelTestServer(t, catalog, &storedKeyModelStore{id: "user:saved", key: "k", baseURL: upstream.URL})
	response := postDraft(server, draftBody(upstream.URL, `,"id":"user:missing"`))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if len(upstream.seen()) != 0 {
		t.Fatal("upstream was called for an unknown model id")
	}
}

func TestDraftModelTestRejectsConfiguredModelID(t *testing.T) {
	upstream := newDraftUpstream(t)
	catalog, _ := llm.NewCatalog(context.Background(), nil, nil)
	server, _ := newModelTestServer(t, catalog, &fakeModelStore{})
	response := postDraft(server, draftBody(upstream.URL, `,"id":"config:primary"`))
	if response.Code != http.StatusBadRequest || len(upstream.seen()) != 0 {
		t.Fatalf("status = %d upstream=%v, want 400 without upstream call", response.Code, upstream.seen())
	}
}

func TestDraftModelTestEgressDeniedReturns422(t *testing.T) {
	upstream := newDraftUpstream(t)
	server := newDraftServerWithEgress(t, config.EgressConfig{DenyCIDRs: []string{"127.0.0.0/8"}})
	response := postDraft(server, draftBody(upstream.URL, `,"api_key":"k"`))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), `"egress_denied"`) {
		t.Fatalf("status = %d body=%s, want 422 egress_denied", response.Code, response.Body.String())
	}
	if len(upstream.seen()) != 0 {
		t.Fatal("denied destination was dialed")
	}
}

func newDraftServerWithEgress(t *testing.T, egress config.EgressConfig) *Server {
	t.Helper()
	catalog, err := llm.NewCatalog(context.Background(), nil, nil, llm.WithEgress(egress))
	if err != nil {
		t.Fatal(err)
	}
	server, _ := newModelTestServer(t, catalog, &fakeModelStore{})
	return server
}

func TestDraftModelTestUpstreamFailureHidesUpstreamBody(t *testing.T) {
	upstream := newDraftUpstream(t)
	upstream.fail = true
	catalog, _ := llm.NewCatalog(context.Background(), nil, nil)
	server, logs := newModelTestServer(t, catalog, &fakeModelStore{})
	response := postDraft(server, draftBody(upstream.URL, `,"api_key":"fail-key-canary"`))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), `"provider_test_failed"`) {
		t.Fatalf("status = %d body=%s, want 422 provider_test_failed", response.Code, response.Body.String())
	}
	for name, text := range map[string]string{"response": response.Body.String(), "logs": logs.String()} {
		if strings.Contains(text, "upstream-secret-body") || strings.Contains(text, "fail-key-canary") {
			t.Fatalf("%s leaked upstream body or key: %s", name, text)
		}
	}
}

// Create and update report the same per-field details as the draft test, and
// an invalid body never reaches the store.
func TestModelWritesReturnFieldDetails(t *testing.T) {
	catalog, err := llm.NewCatalog(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	invalid := `{"display_name":"","protocol":"nope","base_url":"https://example.com","model":"","params":{"max_tokens":-1,"max_concurrency":1},"revision":1}`
	for _, tc := range []struct {
		name string
		call func(*Server, http.ResponseWriter, *http.Request)
	}{
		{"create", (*Server).handleCreateModel},
		{"update", (*Server).handleUpdateModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := &fakeModelStore{}
			server, _ := newModelTestServer(t, catalog, ms)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/models", strings.NewReader(invalid))
			request.SetPathValue("id", "user:one")
			response := httptest.NewRecorder()
			tc.call(server, response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
			}
			for _, want := range []string{`"field":"display_name"`, `"field":"protocol"`, `"field":"params.max_tokens"`} {
				if !strings.Contains(response.Body.String(), want) {
					t.Errorf("body %s missing %s", response.Body.String(), want)
				}
			}
		})
	}
}

func TestDraftModelTestStoredKeyAuditsHostOnly(t *testing.T) {
	upstream := newDraftUpstream(t)
	ms := &storedKeyModelStore{id: "user:saved", key: "stored-key-canary", baseURL: upstream.URL}
	catalog, _ := llm.NewCatalog(context.Background(), nil, nil)
	server, logs := newModelTestServer(t, catalog, ms)
	postDraft(server, draftBody(upstream.URL+"/v1", `,"id":"user:saved"`))
	host := strings.TrimPrefix(upstream.URL, "http://")
	if !strings.Contains(logs.String(), `"stored_credential":true`) || !strings.Contains(logs.String(), `"host":"`+host+`"`) {
		t.Fatalf("audit lacks stored_credential/host: %s", logs.String())
	}
	if strings.Contains(logs.String(), "/v1") {
		t.Fatalf("audit leaked path: %s", logs.String())
	}
}

func TestDraftModelTestStoredKeyRequiresSameEndpoint(t *testing.T) {
	upstream := newDraftUpstream(t)
	other := newDraftUpstream(t)
	ms := &storedKeyModelStore{id: "user:saved", key: "stored-key-canary", baseURL: upstream.URL}
	catalog, _ := llm.NewCatalog(context.Background(), nil, nil)
	server, _ := newModelTestServer(t, catalog, ms)
	for name, body := range map[string]string{
		"different host": draftBody(other.URL, `,"id":"user:saved"`),
		"different protocol": strings.Replace(draftBody(upstream.URL, `,"id":"user:saved"`),
			`"openai-chat"`, `"anthropic"`, 1),
	} {
		response := postDraft(server, body)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"api_key"`) {
			t.Errorf("%s: status = %d body=%s, want 400 on api_key", name, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "stored-key-canary") {
			t.Errorf("%s: response leaked the stored key", name)
		}
	}
	if len(upstream.seen())+len(other.seen()) != 0 {
		t.Fatal("stored key was sent to a changed endpoint")
	}
	// A different path on the same scheme+host+port still reuses the key.
	if response := postDraft(server, draftBody(upstream.URL+"/v2", `,"id":"user:saved"`)); response.Code != http.StatusOK {
		t.Fatalf("same endpoint with other path: status = %d body=%s, want 200", response.Code, response.Body.String())
	}
}

func TestDraftModelTestValidatesIDEvenWithExplicitKey(t *testing.T) {
	upstream := newDraftUpstream(t)
	ms := &storedKeyModelStore{id: "user:saved", key: "k", baseURL: upstream.URL, deleted: true}
	catalog, _ := llm.NewCatalog(context.Background(), nil, nil)
	server, _ := newModelTestServer(t, catalog, ms)
	for id, want := range map[string]int{"config:primary": http.StatusBadRequest, "user:missing": http.StatusNotFound, "user:saved": http.StatusNotFound} {
		response := postDraft(server, draftBody(upstream.URL, `,"api_key":"k","id":"`+id+`"`))
		if response.Code != want {
			t.Errorf("id %q: status = %d, want %d", id, response.Code, want)
		}
	}
	if len(upstream.seen()) != 0 {
		t.Fatal("upstream called despite invalid id")
	}
}

type countingRecorder struct {
	mu    sync.Mutex
	calls int
}

func (c *countingRecorder) RecordCall(context.Context, llm.CallRecord) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
}

func TestDraftModelTestRecordsNoLLMCalls(t *testing.T) {
	upstream := newDraftUpstream(t)
	recorder := &countingRecorder{}
	catalog, err := llm.NewCatalog(context.Background(), nil, nil, llm.WithRecorder(recorder))
	if err != nil {
		t.Fatal(err)
	}
	server, _ := newModelTestServer(t, catalog, &fakeModelStore{})
	if response := postDraft(server, draftBody(upstream.URL, `,"api_key":"k"`)); response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", response.Code, response.Body.String())
	}
	if recorder.calls != 0 {
		t.Fatalf("recorded %d calls, want 0", recorder.calls)
	}
}

func TestSavedModelTestResponseCarriesOK(t *testing.T) {
	catalog, _ := llm.NewCatalog(context.Background(), nil, nil)
	server, _ := newModelTestServer(t, catalog, &fakeModelStore{})
	server.llm = &fakeLLM{result: ProviderTestResult{Model: "m", LatencyMS: 7}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/models/user:x/test", nil)
	request.SetPathValue("id", "user:x")
	response := httptest.NewRecorder()
	server.handleTestModel(response, request)
	var body struct {
		OK        bool   `json:"ok"`
		Model     string `json:"model"`
		LatencyMS int64  `json:"latency_ms"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || !body.OK || body.Model != "m" || body.LatencyMS != 7 {
		t.Fatalf("body = %s (%v), want ok:true model:m latency_ms:7", response.Body.String(), err)
	}
}
