package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func TestAPIAuditSafetyThreeProtocolsAndHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.db")
	st, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_, err = st.RegisterProject(ctx, store.Project{ID: 42, PathWithNamespace: "group/a", WebURL: "https://gitlab.example/group/a", DefaultBranch: "main", TokenEnc: []byte{1}, WebhookSecretEnc: []byte{2}, TokenScopes: []string{"api"}})
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(rev int64, typ string, value any) int64 {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		next, err := st.Append(ctx, "wf-audit", rev, dag.Event{Type: typ, Payload: raw, At: time.Now().UTC()})
		if err != nil {
			t.Fatalf("append %s: %v", typ, err)
		}
		return next
	}
	rev := appendEvent(0, dag.EventWorkflowCreated, dag.WorkflowCreatedPayload{
		WorkflowID: "wf-audit", Spec: dag.GraphSpec{Nodes: []dag.NodeSpec{{ID: "plan", Kind: "plan", Required: true}}, Budget: dag.Budget{MaxNodes: 5, MaxExtends: 1, MaxTokens: 1000}},
		Meta: map[string]string{"project_id": "42", "kind": "review", "mr_iid": "7", "head_sha": "head", "base_sha": "base", "fingerprint": "fp", "profile": "default", "trigger": "mr_open"},
	})
	rev = appendEvent(rev, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{})
	rev = appendEvent(rev, dag.EventNodeQueued, dag.NodeQueuedPayload{NodeID: "plan", Attempt: 1})
	appendEvent(rev, dag.EventNodeStarted, dag.NodeStartedPayload{NodeID: "plan", Attempt: 1})
	const secret = "audit-sensitive-canary"
	const oldID = "historic-upstream-canary"
	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	compress := func(s string) []byte {
		t.Helper()
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	legacyRequest := `{"messages":[{"role":"user","parts":[{"type":"text","text":"legacy prompt"}]}]}`
	legacyResponse := `{"message":{"role":"assistant","parts":[]},"request_id":"` + oldID + `"}`
	_, err = rawDB.ExecContext(ctx, `INSERT INTO llm_calls (id, workflow_id, node_id, turn, provider, model, request, response, latency_ms, request_id, created_at) VALUES ('legacy','wf-audit','plan',1,'p','test',?,?,0,?,'2020-01-01T00:00:00Z')`, compress(legacyRequest), compress(legacyResponse), oldID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rawDB.ExecContext(ctx, `INSERT INTO llm_calls (id, workflow_id, node_id, turn, provider, model, request, response, latency_ms, created_at) VALUES ('old-metadata','wf-audit','plan',1,'p','test',?,?,0,'2020-01-01T00:00:01Z')`, compress(`{"messages":2,"text_parts":1}`), compress(`{"usage":{"input_tokens":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIT_TEST_KEY", secret)
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic"} {
		for _, mode := range []string{"invalid", "refusal", "valid"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				var body string
				switch protocol {
				case "openai-chat":
					message, stop := `{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"submit_result","arguments":"{}"}}]}`, "tool_calls"
					if mode == "invalid" {
						message = strings.Replace(message, `"arguments":"{}"`, `"arguments":"[]"`, 1)
					}
					if mode == "refusal" {
						message, stop = `{"role":"assistant","refusal":"`+secret+`"}`, "stop"
					}
					body = fmt.Sprintf(`{"id":%q,"object":"chat.completion","model":"test","choices":[{"index":0,"finish_reason":%q,"message":%s}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`, secret, stop, message)
				case "openai-responses":
					output := `[{"type":"function_call","id":"fc","call_id":"call","name":"submit_result","status":"completed","arguments":"{}"}]`
					if mode == "invalid" {
						output = strings.Replace(output, `"arguments":"{}"`, `"arguments":"[]"`, 1)
					}
					if mode == "refusal" {
						output = `[{"type":"message","id":"msg","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"` + secret + `"}]}]`
					}
					body = fmt.Sprintf(`{"id":%q,"object":"response","created_at":1,"model":"test","status":"completed","output":%s,"usage":{"input_tokens":11,"output_tokens":7}}`, secret, output)
				case "anthropic":
					content, stop := `[{"type":"tool_use","id":"call","name":"submit_result","input":{}}]`, "tool_use"
					if mode == "invalid" {
						content = strings.Replace(content, `"input":{}`, `"input":[]`, 1)
					}
					if mode == "refusal" {
						content, stop = `[{"type":"text","text":"`+secret+`"}]`, "refusal"
					}
					body = fmt.Sprintf(`{"id":%q,"type":"message","role":"assistant","model":"test","content":%s,"stop_reason":%q,"usage":{"input_tokens":11,"output_tokens":7}}`, secret, content, stop)
				}
				var requests atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(body))
				}))
				t.Cleanup(upstream.Close)
				var records []llm.CallRecord
				registry, err := llm.NewRegistry(map[string]config.ProviderConfig{"p": {Protocol: protocol, Model: "test", BaseURL: upstream.URL, APIKeyEnv: "AUDIT_TEST_KEY", MaxTokens: 512}}, llm.WithRecorder(llm.RecorderFunc(func(ctx context.Context, r llm.CallRecord) {
					records = append(records, r)
					if err := st.RecordLLMCall(ctx, r); err != nil {
						t.Errorf("record: %v", err)
					}
				})), llm.WithRetryBaseDelay(time.Millisecond))
				if err != nil {
					t.Fatal(err)
				}
				client, err := registry.Client("p")
				if err != nil {
					t.Fatal(err)
				}
				submits := 0
				result, err := agent.Run(llm.WithCallMeta(ctx, llm.CallMeta{WorkflowID: "wf-audit", NodeID: "plan"}), agent.RunSpec{
					Client: client, Prompt: secret, MaxTurns: 1, Submit: agent.SubmitSpec{Decode: func(raw json.RawMessage) (any, error) { submits++; return string(raw), nil }},
				})
				if requests.Load() != 1 || len(records) != 1 || result == nil || result.Usage.InputTokens != 11 || result.Usage.OutputTokens != 7 {
					t.Fatalf("requests=%d records=%d result=%+v err=%v", requests.Load(), len(records), result, err)
				}
				unsafe := mode != "valid"
				if errors.Is(err, agent.ErrUnsafeResponse) != unsafe || llm.IsUnsafeResponse(records[0].Err) != unsafe || (submits == 1) == unsafe {
					t.Fatalf("mode=%s err=%v record=%v submits=%d", mode, err, records[0].Err, submits)
				}
				raw, _ := json.Marshal(records[0])
				if strings.Contains(string(raw), secret) {
					t.Fatal("recorder leaked canary")
				}
			})
		}
	}
	var newIDs int
	if err := rawDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM llm_calls WHERE id NOT IN ('legacy','old-metadata') AND request_id IS NOT NULL`).Scan(&newIDs); err != nil || newIDs != 0 {
		t.Fatalf("new IDs stored: %d, %v", newIDs, err)
	}
	var storedOld string
	if err := rawDB.QueryRowContext(ctx, `SELECT request_id FROM llm_calls WHERE id = 'legacy'`).Scan(&storedOld); err != nil || storedOld != oldID {
		t.Fatalf("history altered: %q %v", storedOld, err)
	}
	stats, err := st.Stats(ctx, store.StatsFilter{})
	if err != nil || len(stats.ByModel) != 1 || stats.ByModel[0].Calls != 11 || stats.ByModel[0].Errors != 6 {
		t.Fatalf("model statistics = %+v, %v", stats.ByModel, err)
	}
	env := newM6Env(t, m6Options{store: st})
	admin := env.businessClient()
	env.grant(8, 42, store.ProjectAccessRead)
	viewer := env.loginAs("code-viewer")
	pathAPI := "/api/v1/reviews/wf-audit/nodes/plan/calls"
	for _, client := range []*m6Client{admin, viewer} {
		response := client.request(http.MethodGet, pathAPI, nil, nil)
		assertStatus(t, response, http.StatusOK)
		if strings.Contains(string(response.body), secret) || strings.Contains(string(response.body), oldID) || strings.Contains(string(response.body), "legacy prompt") {
			t.Fatal("unexpanded API leaked audit data")
		}
	}
	assertStatus(t, viewer.request(http.MethodGet, pathAPI+"?include_payload=true", nil, nil), http.StatusForbidden)
	response := admin.request(http.MethodGet, pathAPI+"?include_payload=true", nil, nil)
	assertStatus(t, response, http.StatusOK)
	items := env.decode(t, response)["calls"].([]any)
	if len(items) != 11 {
		t.Fatalf("calls=%d", len(items))
	}
	unsafeCalls := 0
	for _, item := range items {
		call := item.(map[string]any)
		if _, ok := call["request_id"]; ok {
			t.Fatal("API returned upstream request_id")
		}
		if call["id"] == "legacy" {
			request := call["request"].(map[string]any)
			if _, ok := request["messages"].([]any); !ok || request["_format"] != nil {
				t.Fatalf("legacy request = %v", request)
			}
			if call["response"].(map[string]any)["request_id"] != oldID {
				t.Fatal("legacy payload changed")
			}
			continue
		}
		if call["id"] == "old-metadata" {
			request := call["request"].(map[string]any)
			if request["messages"] != float64(2) || request["_format"] != nil {
				t.Fatalf("unversioned metadata = %v", request)
			}
			continue
		}
		encoded, _ := json.Marshal(call)
		if strings.Contains(string(encoded), secret) {
			t.Fatal("new API audit leaked canary")
		}
		if kind, ok := call["error_kind"]; ok {
			if kind != "unsafe_model_response" {
				t.Fatalf("unexpected error category: %v", kind)
			}
			unsafeCalls++
		}
		for _, key := range []string{"request", "response"} {
			if call[key].(map[string]any)["_format"] != "audit_metadata_v1" {
				t.Fatalf("%s metadata = %v", key, call[key])
			}
		}
	}
	if unsafeCalls != 6 {
		t.Fatalf("API unsafe calls = %d, want 6", unsafeCalls)
	}
}
