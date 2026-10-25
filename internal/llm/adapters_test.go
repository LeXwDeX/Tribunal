package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
)

func floatPtr(value float64) *float64 { return &value }

func mustClient(t *testing.T, name string, cfg config.ProviderConfig, opts ...RegistryOption) Client {
	t.Helper()
	registry, err := NewRegistry(map[string]config.ProviderConfig{name: cfg}, opts...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	client, err := registry.Client(name)
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	return client
}

func readFileTool() ToolDef {
	return ToolDef{
		Name:        "read_file",
		Description: "Read a file",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
	}
}

func requireKind(t *testing.T, err error, kind ErrKind) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error, got nil", kind)
	}
	var classified *Error
	if !errors.As(err, &classified) {
		t.Fatalf("expected *llm.Error, got %T: %v", err, err)
	}
	if classified.Kind != kind {
		t.Fatalf("expected kind %s, got %s (%v)", kind, classified.Kind, err)
	}
	return classified
}

func findPart(message Message, partType PartType) *Part {
	for i := range message.Parts {
		if message.Parts[i].Type == partType {
			return &message.Parts[i]
		}
	}
	return nil
}

func assertContains(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Fatalf("request body does not contain %q:\n%s", want, body)
	}
}

func assertNotContains(t *testing.T, body, unwanted string) {
	t.Helper()
	if strings.Contains(body, unwanted) {
		t.Fatalf("request body unexpectedly contains %q:\n%s", unwanted, body)
	}
}

func TestOpenAIChatToolRoundTrip(t *testing.T) {
	t.Setenv("TEST_CHAT_KEY", "test-chat-key")
	server, recorder := newReplayServer(t, "testdata/chat_rounds.json")
	client := mustClient(t, "chat", config.ProviderConfig{
		Protocol:       "openai-chat",
		BaseURL:        server.URL + "/v1",
		Model:          "test-chat",
		APIKeyEnv:      "TEST_CHAT_KEY",
		MaxConcurrency: 2,
		MaxTokens:      512,
		Temperature:    floatPtr(0.2),
	})

	info := client.Info()
	if info.Name != "chat" || info.Protocol != "openai-chat" || info.Model != "test-chat" {
		t.Fatalf("unexpected info: %+v", info)
	}
	first, err := client.Generate(context.Background(), &Request{
		System: []Block{{Text: "system prompt", Cache: true}},
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "inspect"}},
		}},
		Tools:      []ToolDef{readFileTool()},
		ToolChoice: "auto",
	})
	if err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	if first.StopReason != StopToolUse {
		t.Fatalf("expected tool_use, got %s", first.StopReason)
	}
	if text := findPart(first.Message, PartText); text == nil || text.Text != "checking" {
		t.Fatalf("missing assistant text: %+v", first.Message)
	}
	call := findPart(first.Message, PartToolCall)
	if call == nil || call.ToolCall.ID != "call_1" || call.ToolCall.Name != "read_file" {
		t.Fatalf("missing tool call: %+v", first.Message)
	}
	if string(call.ToolCall.Input) != `{"path":"README.md"}` {
		t.Fatalf("unexpected tool input: %s", call.ToolCall.Input)
	}
	if first.Usage.InputTokens != 10 || first.Usage.OutputTokens != 5 || first.Usage.CacheReadTokens != 2 {
		t.Fatalf("unexpected usage: %+v", first.Usage)
	}
	if first.RequestID != "chatcmpl-1" {
		t.Fatalf("unexpected request id: %q", first.RequestID)
	}

	second, err := client.Generate(context.Background(), &Request{
		System: []Block{{Text: "system prompt"}},
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "inspect"}}},
			first.Message,
			{Role: RoleUser, Parts: []Part{{
				Type:       PartToolResult,
				ToolResult: &ToolResult{CallID: "call_1", Content: "file body"},
			}}},
		},
		Tools:      []ToolDef{readFileTool()},
		ToolChoice: "auto",
	})
	if err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if text := findPart(second.Message, PartText); text == nil || text.Text != "done" {
		t.Fatalf("unexpected second response: %+v", second.Message)
	}
	if second.StopReason != StopEndTurn {
		t.Fatalf("expected end_turn, got %s", second.StopReason)
	}

	requests := recorder.requests()
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}
	if got := requests[0].Header.Get("Authorization"); got != "Bearer test-chat-key" {
		t.Fatalf("unexpected Authorization header: %q", got)
	}
	if got := requests[0].Header.Get("X-Session-Id"); got != "" {
		t.Fatalf("openai-chat must not send X-Session-Id, got %q", got)
	}
	firstBody := recorder.body(0)
	assertContains(t, firstBody, `"max_tokens":512`)
	assertContains(t, firstBody, `"temperature":0.2`)
	assertContains(t, firstBody, `"tool_choice":"auto"`)
	assertContains(t, firstBody, `"name":"read_file"`)
	secondBody := recorder.body(1)
	assertContains(t, secondBody, `"tool_calls"`)
	assertContains(t, secondBody, `"role":"tool"`)
	assertContains(t, secondBody, `"tool_call_id":"call_1"`)
	assertContains(t, secondBody, `"file body"`)
	assertNotContains(t, secondBody, `"reasoning"`)
}

func TestOpenAIResponsesReasoningRoundTrip(t *testing.T) {
	t.Setenv("TEST_RESPONSES_KEY", "test-responses-key")
	server, recorder := newReplayServer(t, "testdata/responses_rounds.json")
	client := mustClient(t, "responses", config.ProviderConfig{
		Protocol:       "openai-responses",
		BaseURL:        server.URL + "/v1",
		Model:          "test-responses",
		APIKeyEnv:      "TEST_RESPONSES_KEY",
		MaxConcurrency: 2,
		MaxTokens:      512,
		Temperature:    floatPtr(0.1),
		Reasoning:      &config.ReasoningConfig{Effort: "low"},
	})

	first, err := client.Generate(context.Background(), &Request{
		System: []Block{{Text: "system prompt"}},
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "inspect"}},
		}},
		Tools:      []ToolDef{readFileTool()},
		ToolChoice: "auto",
		Reasoning:  &ReasoningOpts{Effort: "medium"},
	})
	if err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	if first.StopReason != StopToolUse {
		t.Fatalf("expected tool_use, got %s", first.StopReason)
	}
	reasoning := findPart(first.Message, PartReasoning)
	if reasoning == nil || reasoning.Reasoning.Protocol != "openai-responses" {
		t.Fatalf("missing reasoning part: %+v", first.Message)
	}
	if reasoning.Reasoning.Summary != "thought" {
		t.Fatalf("unexpected reasoning summary: %q", reasoning.Reasoning.Summary)
	}
	reasoningRaw := map[string]any{}
	if err := json.Unmarshal(reasoning.Reasoning.Raw, &reasoningRaw); err != nil {
		t.Fatalf("reasoning item is not JSON: %v", err)
	}
	if reasoningRaw["encrypted_content"] != "enc_abc" {
		t.Fatalf("reasoning item lost encrypted content: %s", reasoning.Reasoning.Raw)
	}
	call := findPart(first.Message, PartToolCall)
	if call == nil || call.ToolCall.ID != "call_1" {
		t.Fatalf("missing function call: %+v", first.Message)
	}
	if first.Usage.ReasoningTokens != 4 || first.Usage.CacheWriteTokens != 1 {
		t.Fatalf("unexpected usage: %+v", first.Usage)
	}

	second, err := client.Generate(context.Background(), &Request{
		System: []Block{{Text: "system prompt"}},
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "inspect"}}},
			first.Message,
			{Role: RoleTool, Parts: []Part{{
				Type:       PartToolResult,
				ToolResult: &ToolResult{CallID: "call_1", Content: "file body"},
			}}},
		},
		Tools:      []ToolDef{readFileTool()},
		ToolChoice: "auto",
		Reasoning:  &ReasoningOpts{Effort: "medium"},
	})
	if err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if text := findPart(second.Message, PartText); text == nil || text.Text != "done" {
		t.Fatalf("unexpected second response: %+v", second.Message)
	}

	firstBody := recorder.body(0)
	assertContains(t, firstBody, `"store":false`)
	assertContains(t, firstBody, `"include":["reasoning.encrypted_content"]`)
	assertContains(t, firstBody, `"max_output_tokens":512`)
	assertContains(t, firstBody, `"temperature":0.1`)
	assertContains(t, firstBody, `"reasoning":{"effort":"medium"}`)
	assertContains(t, firstBody, `"instructions":"system prompt"`)
	secondBody := recorder.body(1)
	assertContains(t, secondBody, `"type":"reasoning"`)
	assertContains(t, secondBody, `"encrypted_content":"enc_abc"`)
	assertContains(t, secondBody, `"type":"function_call"`)
	assertContains(t, secondBody, `"call_id":"call_1"`)
	assertContains(t, secondBody, `"type":"function_call_output"`)
	assertContains(t, secondBody, `"output":"file body"`)
	assertNotContains(t, secondBody, `"previous_response_id"`)
}

func TestAnthropicThinkingRoundTrip(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY", "test-anthropic-key")
	server, recorder := newReplayServer(t, "testdata/anthropic_rounds.json")
	client := mustClient(t, "anthropic", config.ProviderConfig{
		Protocol:       "anthropic",
		BaseURL:        server.URL,
		Model:          "claude-test",
		APIKeyEnv:      "TEST_ANTHROPIC_KEY",
		MaxConcurrency: 2,
		MaxTokens:      512,
		Temperature:    floatPtr(0.3),
		Thinking:       &config.ThinkingConfig{BudgetTokens: 2048},
	})

	first, err := client.Generate(context.Background(), &Request{
		System: []Block{
			{Text: "stable prefix"},
			{Text: "cacheable suffix", Cache: true},
		},
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "inspect"}},
		}},
		Tools:       []ToolDef{readFileTool()},
		ToolChoice:  "auto",
		Temperature: floatPtr(0.7),
		Reasoning:   &ReasoningOpts{BudgetTokens: 4096},
	})
	if err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	if first.StopReason != StopToolUse {
		t.Fatalf("expected tool_use, got %s", first.StopReason)
	}
	var reasoningParts []Part
	for _, part := range first.Message.Parts {
		if part.Type == PartReasoning {
			reasoningParts = append(reasoningParts, part)
		}
	}
	if len(reasoningParts) != 2 {
		t.Fatalf("expected thinking and redacted_thinking parts, got %+v", first.Message)
	}
	thinkingRaw := map[string]any{}
	if err := json.Unmarshal(reasoningParts[0].Reasoning.Raw, &thinkingRaw); err != nil {
		t.Fatalf("thinking item is not JSON: %v", err)
	}
	if reasoningParts[0].Reasoning.Protocol != "anthropic" || thinkingRaw["signature"] != "sig_abc" {
		t.Fatalf("thinking signature lost: %+v", reasoningParts[0].Reasoning)
	}
	redactedRaw := map[string]any{}
	if err := json.Unmarshal(reasoningParts[1].Reasoning.Raw, &redactedRaw); err != nil {
		t.Fatalf("redacted thinking item is not JSON: %v", err)
	}
	if redactedRaw["data"] != "redacted_data" || reasoningParts[1].Reasoning.Summary != "[redacted]" {
		t.Fatalf("redacted thinking lost: %+v", reasoningParts[1].Reasoning)
	}
	call := findPart(first.Message, PartToolCall)
	if call == nil || call.ToolCall.ID != "toolu_1" || call.ToolCall.Name != "read_file" {
		t.Fatalf("missing tool use: %+v", first.Message)
	}
	if first.Usage.CacheReadTokens != 2 || first.Usage.CacheWriteTokens != 3 || first.Usage.ReasoningTokens != 4 {
		t.Fatalf("unexpected usage: %+v", first.Usage)
	}

	second, err := client.Generate(context.Background(), &Request{
		System: []Block{{Text: "stable prefix"}},
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "inspect"}}},
			first.Message,
			{Role: RoleUser, Parts: []Part{{
				Type:       PartToolResult,
				ToolResult: &ToolResult{CallID: "toolu_1", Content: "file body"},
			}}},
		},
		Tools:      []ToolDef{readFileTool()},
		ToolChoice: "auto",
	})
	if err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if text := findPart(second.Message, PartText); text == nil || text.Text != "done" {
		t.Fatalf("unexpected second response: %+v", second.Message)
	}

	requests := recorder.requests()
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}
	if got := requests[0].Header.Get("x-api-key"); got != "test-anthropic-key" {
		t.Fatalf("unexpected x-api-key header: %q", got)
	}
	if got := requests[0].Header.Get("X-Session-Id"); got != "" {
		t.Fatalf("anthropic must not send X-Session-Id, got %q", got)
	}
	firstBody := recorder.body(0)
	assertContains(t, firstBody, `"cache_control":{"type":"ephemeral"}`)
	assertContains(t, firstBody, `"max_tokens":512`)
	assertContains(t, firstBody, `"thinking":{"budget_tokens":4096,"type":"enabled"}`)
	assertContains(t, firstBody, `"tool_choice":{"type":"auto"}`)
	assertNotContains(t, firstBody, `"temperature"`)
	secondBody := recorder.body(1)
	assertContains(t, secondBody, `"type":"thinking"`)
	assertContains(t, secondBody, `"signature":"sig_abc"`)
	assertContains(t, secondBody, `"type":"redacted_thinking"`)
	assertContains(t, secondBody, `"data":"redacted_data"`)
	assertContains(t, secondBody, `"type":"tool_result"`)
	assertContains(t, secondBody, `"tool_use_id":"toolu_1"`)
	assertContains(t, secondBody, `"file body"`)
}

func TestAnthropicForcedToolChoiceDisablesThinking(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY", "test-anthropic-key")
	server, recorder := newReplayServer(t, "testdata/anthropic_rounds.json")
	client := mustClient(t, "anthropic", config.ProviderConfig{
		Protocol:       "anthropic",
		BaseURL:        server.URL,
		Model:          "claude-test",
		APIKeyEnv:      "TEST_ANTHROPIC_KEY",
		MaxConcurrency: 2,
		MaxTokens:      512,
		Thinking:       &config.ThinkingConfig{BudgetTokens: 2048},
	})

	cases := []struct {
		choice string
		kind   string
		name   string
	}{
		{choice: "read_file", kind: "tool", name: "read_file"},
		{choice: "required", kind: "any"},
	}
	for i, tc := range cases {
		if _, err := client.Generate(context.Background(), &Request{
			Messages: []Message{{
				Role:  RoleUser,
				Parts: []Part{{Type: PartText, Text: "inspect"}},
			}},
			Tools:      []ToolDef{readFileTool()},
			ToolChoice: tc.choice,
		}); err != nil {
			t.Fatalf("Generate %q: %v", tc.choice, err)
		}
		var body struct {
			Thinking   json.RawMessage `json:"thinking"`
			ToolChoice struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"tool_choice"`
		}
		if err := json.Unmarshal([]byte(recorder.body(i)), &body); err != nil {
			t.Fatalf("decode request %d: %v", i, err)
		}
		if len(body.Thinking) != 0 {
			t.Fatalf("request %q kept thinking enabled: %s", tc.choice, body.Thinking)
		}
		if body.ToolChoice.Type != tc.kind || body.ToolChoice.Name != tc.name {
			t.Fatalf("request %q tool_choice = %+v", tc.choice, body.ToolChoice)
		}
	}
}

const (
	responsesSubmitJSON  = `{"id":"resp_submit","object":"response","created_at":1,"status":"completed","model":"test-responses","output":[{"type":"function_call","id":"fc_submit","call_id":"call_submit","name":"submit_result","arguments":"{\"answer\":\"ok\"}","status":"completed"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	responsesMessageJSON = `{"id":"resp_message","object":"response","created_at":2,"status":"completed","model":"test-responses","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
)

func responsesSessionRequest() *Request {
	return &Request{
		System:   []Block{{Text: "system prompt"}},
		Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "inspect"}}}},
	}
}

func TestOpenAIResponsesSessionHeaderPerRequest(t *testing.T) {
	t.Setenv("TEST_RESPONSES_KEY", "test-responses-key")
	cases := []struct {
		name string
		meta *CallMeta
	}{
		{name: "absent-metadata"},
		{name: "reused-metadata", meta: &CallMeta{WorkflowID: "wf-secret", NodeID: "node-secret", Turn: 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, recorder := newReplayServer(t, "testdata/responses_rounds.json")
			client := mustClient(t, "responses", config.ProviderConfig{
				Protocol:       "openai-responses",
				BaseURL:        server.URL + "/v1",
				Model:          "test-responses",
				APIKeyEnv:      "TEST_RESPONSES_KEY",
				MaxConcurrency: 2,
			})
			for i := 0; i < 2; i++ {
				ctx := context.Background()
				if tc.meta != nil {
					ctx = WithCallMeta(ctx, *tc.meta)
				}
				if _, err := client.Generate(ctx, responsesSessionRequest()); err != nil {
					t.Fatalf("Generate %d: %v", i, err)
				}
			}
			requests := recorder.requests()
			if len(requests) != 2 {
				t.Fatalf("expected 2 requests, got %d", len(requests))
			}
			first := requests[0].Header.Get("X-Session-Id")
			second := requests[1].Header.Get("X-Session-Id")
			if first == "" || second == "" {
				t.Fatalf("X-Session-Id must be present, got %q and %q", first, second)
			}
			if first == second {
				t.Fatalf("X-Session-Id must be fresh per request, got %q twice", first)
			}
			if tc.meta != nil && (strings.Contains(first, tc.meta.WorkflowID) || strings.Contains(second, tc.meta.WorkflowID) ||
				strings.Contains(first, tc.meta.NodeID) || strings.Contains(second, tc.meta.NodeID)) {
				t.Fatalf("X-Session-Id leaked CallMeta: %q, %q", first, second)
			}
			if recorder.body(0) != recorder.body(1) {
				t.Fatalf("identical requests must have identical bodies:\n%s\n%s", recorder.body(0), recorder.body(1))
			}
			body := recorder.body(0)
			assertContains(t, body, `"store":false`)
			assertNotContains(t, body, `"previous_response_id"`)
		})
	}
}

func TestOpenAIResponsesSessionHeaderConcurrent(t *testing.T) {
	t.Setenv("TEST_RESPONSES_KEY", "test-responses-key")
	cases := []struct {
		name string
		meta *CallMeta
	}{
		{name: "absent-metadata"},
		{name: "reused-metadata", meta: &CallMeta{WorkflowID: "same-workflow", NodeID: "same-node", Turn: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, recorder := newReplayServer(t, "testdata/responses_rounds.json")
			client := mustClient(t, "responses", config.ProviderConfig{
				Protocol:       "openai-responses",
				BaseURL:        server.URL + "/v1",
				Model:          "test-responses",
				APIKeyEnv:      "TEST_RESPONSES_KEY",
				MaxConcurrency: 2,
			})
			ctx := context.Background()
			if tc.meta != nil {
				ctx = WithCallMeta(ctx, *tc.meta)
			}
			var wg sync.WaitGroup
			errs := make(chan error, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := client.Generate(ctx, responsesSessionRequest())
					errs <- err
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("Generate: %v", err)
				}
			}
			requests := recorder.requests()
			if len(requests) != 2 {
				t.Fatalf("expected 2 requests, got %d", len(requests))
			}
			first := requests[0].Header.Get("X-Session-Id")
			second := requests[1].Header.Get("X-Session-Id")
			if first == "" || second == "" {
				t.Fatalf("X-Session-Id must be present, got %q and %q", first, second)
			}
			if first == second {
				t.Fatalf("concurrent requests must get distinct X-Session-Id values, got %q twice", first)
			}
			if recorder.body(0) != recorder.body(1) {
				t.Fatalf("identical concurrent requests must have identical bodies:\n%s\n%s", recorder.body(0), recorder.body(1))
			}
			body := recorder.body(0)
			assertContains(t, body, `"store":false`)
			assertNotContains(t, body, `"previous_response_id"`)
		})
	}
}

func TestOpenAIResponsesSessionHeaderFreshTurnAfterSubmit(t *testing.T) {
	t.Setenv("TEST_RESPONSES_KEY", "test-responses-key")
	var mu sync.Mutex
	var seen []replayRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		mu.Lock()
		index := len(seen)
		seen = append(seen, replayRequest{
			Method: req.Method,
			Path:   req.URL.Path,
			Header: req.Header.Clone(),
			Body:   body,
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if index == 0 {
			_, _ = w.Write([]byte(responsesSubmitJSON))
			return
		}
		_, _ = w.Write([]byte(responsesMessageJSON))
	}))
	t.Cleanup(server.Close)
	client := mustClient(t, "responses", config.ProviderConfig{
		Protocol:       "openai-responses",
		BaseURL:        server.URL + "/v1",
		Model:          "test-responses",
		APIKeyEnv:      "TEST_RESPONSES_KEY",
		MaxConcurrency: 2,
	})

	first, err := client.Generate(context.Background(), &Request{
		Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "inspect"}}}},
		Tools: []ToolDef{{
			Name:        "submit_result",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`),
		}},
	})
	if err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	if first.StopReason != StopToolUse {
		t.Fatalf("expected tool_use, got %s", first.StopReason)
	}
	call := findPart(first.Message, PartToolCall)
	if call == nil || call.ToolCall.Name != "submit_result" {
		t.Fatalf("missing terminal submit_result call: %+v", first.Message)
	}

	second, err := client.Generate(context.Background(), &Request{
		Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "new independent request"}}}},
	})
	if err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if text := findPart(second.Message, PartText); text == nil || text.Text != "ok" {
		t.Fatalf("unexpected second response: %+v", second.Message)
	}

	mu.Lock()
	requests := append([]replayRequest(nil), seen...)
	mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}
	firstSession := requests[0].Header.Get("X-Session-Id")
	secondSession := requests[1].Header.Get("X-Session-Id")
	if firstSession == "" || secondSession == "" {
		t.Fatalf("X-Session-Id must be present, got %q and %q", firstSession, secondSession)
	}
	if firstSession == secondSession {
		t.Fatalf("fresh turn must get a new X-Session-Id, got %q twice", firstSession)
	}
	secondBody := string(requests[1].Body)
	assertNotContains(t, secondBody, "resp_submit")
	assertNotContains(t, secondBody, "call_submit")
	assertNotContains(t, secondBody, `"function_call"`)
	assertNotContains(t, secondBody, `"previous_response_id"`)
	var payload struct {
		Store              bool              `json:"store"`
		PreviousResponseID string            `json:"previous_response_id"`
		Input              []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(requests[1].Body, &payload); err != nil {
		t.Fatalf("decode fresh request: %v", err)
	}
	if payload.Store {
		t.Fatalf("fresh turn must keep store:false: %s", secondBody)
	}
	if payload.PreviousResponseID != "" {
		t.Fatalf("fresh turn must not set previous_response_id: %q", payload.PreviousResponseID)
	}
	if len(payload.Input) != 1 {
		t.Fatalf("fresh user-only turn must send exactly one input item, got %d: %s", len(payload.Input), secondBody)
	}
	assertContains(t, string(payload.Input[0]), "new independent request")
}

func TestOpenAIResponsesSessionHeaderAttempts(t *testing.T) {
	t.Setenv("TEST_RESPONSES_KEY", "test-responses-key")
	t.Run("retry", func(t *testing.T) {
		var mu sync.Mutex
		var seen []replayRequest
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Errorf("read request body: %v", err)
			}
			mu.Lock()
			index := len(seen)
			seen = append(seen, replayRequest{
				Method: req.Method,
				Path:   req.URL.Path,
				Header: req.Header.Clone(),
				Body:   body,
			})
			mu.Unlock()
			if index == 0 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("upstream unavailable"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(responsesMessageJSON))
		}))
		t.Cleanup(server.Close)
		client := mustClient(t, "responses", config.ProviderConfig{
			Protocol:       "openai-responses",
			BaseURL:        server.URL + "/v1",
			Model:          "test-responses",
			APIKeyEnv:      "TEST_RESPONSES_KEY",
			MaxConcurrency: 1,
		}, WithRetryBaseDelay(time.Millisecond))
		if _, err := client.Generate(context.Background(), responsesSessionRequest()); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		mu.Lock()
		requests := append([]replayRequest(nil), seen...)
		mu.Unlock()
		if len(requests) != 2 {
			t.Fatalf("expected 2 attempts, got %d", len(requests))
		}
		first := requests[0].Header.Get("X-Session-Id")
		second := requests[1].Header.Get("X-Session-Id")
		if first == "" || second == "" {
			t.Fatalf("X-Session-Id must be present, got %q and %q", first, second)
		}
		if first == second {
			t.Fatalf("retry must get a new X-Session-Id, got %q twice", first)
		}
	})
	t.Run("bad-request-no-retry", func(t *testing.T) {
		var mu sync.Mutex
		var seen []replayRequest
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Errorf("read request body: %v", err)
			}
			mu.Lock()
			seen = append(seen, replayRequest{
				Method: req.Method,
				Path:   req.URL.Path,
				Header: req.Header.Clone(),
				Body:   body,
			})
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"No tool output found for function call","type":"invalid_request_error"}}`))
		}))
		t.Cleanup(server.Close)
		client := mustClient(t, "responses", config.ProviderConfig{
			Protocol:       "openai-responses",
			BaseURL:        server.URL + "/v1",
			Model:          "test-responses",
			APIKeyEnv:      "TEST_RESPONSES_KEY",
			MaxConcurrency: 1,
		}, WithRetryBaseDelay(time.Millisecond))
		_, err := client.Generate(context.Background(), responsesSessionRequest())
		requireKind(t, err, ErrBadRequest)
		mu.Lock()
		requests := append([]replayRequest(nil), seen...)
		mu.Unlock()
		if len(requests) != 1 {
			t.Fatalf("bad request must not retry, attempts=%d", len(requests))
		}
		if got := requests[0].Header.Get("X-Session-Id"); got == "" {
			t.Fatal("X-Session-Id must be present on a bad-request attempt")
		}
	})
}
