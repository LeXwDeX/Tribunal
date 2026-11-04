package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
)

func TestSanitizeToolCallsRecordsInvalidCategory(t *testing.T) {
	cases := []struct {
		name  string
		calls []*ToolCall
		kind  string
		bytes int
	}{
		{"nil call", []*ToolCall{nil}, ToolCallMissing, 0},
		{"empty id", []*ToolCall{{Name: "t", Input: json.RawMessage(`{}`)}}, ToolCallEmptyID, 2},
		{"empty name", []*ToolCall{{ID: "a", Input: json.RawMessage(`{}`)}}, ToolCallEmptyName, 2},
		{"duplicate id", []*ToolCall{{ID: "a", Name: "t", Input: json.RawMessage(`{}`)}, {ID: "a", Name: "t", Input: json.RawMessage(`{"x":1}`)}}, ToolCallDuplicateID, 7},
		{"empty input", []*ToolCall{{ID: "a", Name: "t", Input: json.RawMessage(" ")}}, ToolCallEmptyInput, 1},
		{"not object", []*ToolCall{{ID: "a", Name: "t", Input: json.RawMessage(`"{}"`)}}, ToolCallNotObject, 4},
		{"invalid json", []*ToolCall{{ID: "a", Name: "t", Input: json.RawMessage(`{"a":"secret`)}}, ToolCallInvalidJSON, 12},
		{"trailing data", []*ToolCall{{ID: "a", Name: "t", Input: json.RawMessage(`{}{}`)}}, ToolCallInvalidJSON, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := Message{Role: RoleAssistant}
			for _, c := range tc.calls {
				msg.Parts = append(msg.Parts, Part{Type: PartToolCall, ToolCall: c})
			}
			out, invalid := SanitizeToolCalls(msg, true)
			if !invalid {
				t.Fatal("want invalid")
			}
			kind, n := ToolCallIssue(out)
			if kind != tc.kind || n != tc.bytes {
				t.Fatalf("issue = %q/%d, want %q/%d", kind, n, tc.kind, tc.bytes)
			}
			for _, part := range out.Parts {
				if part.ToolCall.ID != "" || part.ToolCall.Name != "" || len(part.ToolCall.Input) != 0 {
					t.Fatal("rejected calls must be cleared")
				}
			}
			// A second pass keeps the category instead of reporting empty_id.
			again, invalid := SanitizeToolCalls(out, true)
			if kind, _ := ToolCallIssue(again); !invalid || kind != tc.kind {
				t.Fatalf("second pass = %q/%v, want %q", kind, invalid, tc.kind)
			}
		})
	}
}

func TestSanitizeToolCallsRecordsDisallowedStop(t *testing.T) {
	msg := Message{Role: RoleAssistant, Parts: []Part{{Type: PartToolCall, ToolCall: &ToolCall{ID: "a", Name: "t", Input: json.RawMessage(`{}`)}}}}
	out, invalid := SanitizeToolCalls(msg, false)
	if invalid {
		t.Fatal("a valid call under a non-tool stop is not itself invalid")
	}
	if kind, _ := ToolCallIssue(out); kind != ToolCallUnexpectedStop {
		t.Fatalf("issue = %q, want %q", kind, ToolCallUnexpectedStop)
	}
	if _, invalid := SanitizeToolCalls(out, false); !invalid {
		t.Fatal("a cleared call stays invalid on a second pass")
	}
}

func TestToolCallIssueNotSerialized(t *testing.T) {
	data, err := json.Marshal(ToolCall{Invalid: ToolCallInvalidJSON, InputBytes: 9})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"id":"","name":"","input":null}` {
		t.Fatalf("ToolCall JSON = %s, want the issue fields omitted", data)
	}
}

func chatToolCallClient(t *testing.T, toolCalls string) Client {
	t.Helper()
	body := `{"id":"c","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":` +
		toolCalls + `}}],"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	t.Setenv("ISSUE_TEST_KEY", "key")
	return mustClient(t, "openai-chat", config.ProviderConfig{Protocol: "openai-chat", Model: "test", BaseURL: server.URL, APIKeyEnv: "ISSUE_TEST_KEY", MaxTokens: 512})
}

func TestChatToolCallIssueCategories(t *testing.T) {
	cases := []struct {
		name, calls, kind string
		bytes             int
	}{
		{"truncated arguments", `[{"id":"a","type":"function","function":{"name":"submit_result","arguments":"{\"summary\":\"PROVIDER-TEXT"}}]`, ToolCallInvalidJSON, 25},
		{"object arguments", `[{"id":"a","type":"function","function":{"name":"submit_result","arguments":{"a":1}}}]`, ToolCallNonString, 0},
		{"unknown type", `[{"id":"a","type":"custom","function":{"name":"submit_result","arguments":"{}"}}]`, ToolCallUnsupported, 0},
		{"malformed list", `{"id":"a"}`, ToolCallMalformedList, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := chatToolCallClient(t, tc.calls).Generate(context.Background(), simpleRequest())
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if kind, n := ToolCallIssue(resp.Message); kind != tc.kind || n != tc.bytes {
				t.Fatalf("issue = %q/%d, want %q/%d", kind, n, tc.kind, tc.bytes)
			}
		})
	}
}

func TestAuditRecordsInvalidToolCallWithoutProviderText(t *testing.T) {
	var records []CallRecord
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"submit_result","arguments":"{\"summary\":\"PROVIDER-TEXT"}}]}}],"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}`))
	}))
	defer server.Close()
	t.Setenv("ISSUE_AUDIT_KEY", "key")
	registry, err := NewRegistry(map[string]config.ProviderConfig{"p": {Protocol: "openai-chat", Model: "test", BaseURL: server.URL, APIKeyEnv: "ISSUE_AUDIT_KEY", MaxTokens: 512}},
		WithRecorder(RecorderFunc(func(_ context.Context, rec CallRecord) { records = append(records, rec) })))
	if err != nil {
		t.Fatal(err)
	}
	client, err := registry.Client("p")
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithCallMeta(context.Background(), CallMeta{Turn: 3})
	if _, err := client.Generate(ctx, simpleRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(records) != 1 || records[0].Audit == nil || records[0].Audit.Response == nil {
		t.Fatalf("records = %+v", records)
	}
	resp := records[0].Audit.Response
	if resp.InvalidToolCall != ToolCallInvalidJSON || resp.InvalidToolInputBytes != 25 {
		t.Fatalf("audit = %q/%d, want %q/25", resp.InvalidToolCall, resp.InvalidToolInputBytes, ToolCallInvalidJSON)
	}
	data, _ := json.Marshal(records[0].Audit)
	if strings.Contains(string(data), "PROVIDER-TEXT") || strings.Contains(string(data), "submit_result") {
		t.Fatalf("audit leaks provider text: %s", data)
	}
	if !strings.Contains(string(data), `"invalid_tool_call":"invalid_json"`) || !strings.Contains(string(data), `"invalid_tool_input_bytes":25`) {
		t.Fatalf("audit JSON = %s", data)
	}
}

func TestSanitizeToolCallsMarksValidCallsOfRejectedBatch(t *testing.T) {
	msg := Message{Role: RoleAssistant, Parts: []Part{
		{Type: PartToolCall, ToolCall: &ToolCall{ID: "a", Name: "t", Input: json.RawMessage(`{}`)}},
		{Type: PartToolCall, ToolCall: &ToolCall{ID: "b", Name: "t", Input: json.RawMessage(`{`)}},
	}}
	out, invalid := SanitizeToolCalls(msg, true)
	if !invalid || out.Parts[0].ToolCall.Invalid != ToolCallRejectedBatch || out.Parts[1].ToolCall.Invalid != ToolCallInvalidJSON {
		t.Fatalf("categories = %q, %q", out.Parts[0].ToolCall.Invalid, out.Parts[1].ToolCall.Invalid)
	}
	if kind, n := ToolCallIssue(out); kind != ToolCallInvalidJSON || n != 1 {
		t.Fatalf("issue = %q/%d", kind, n)
	}
}
