package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
)

// capabilityWireServer records request bodies and replays fixed responses.
type capabilityWireServer struct {
	mu        sync.Mutex
	bodies    []string
	responses []string
}

func newCapabilityWireServer(t *testing.T, responses ...string) (*httptest.Server, *capabilityWireServer) {
	t.Helper()
	capture := &capabilityWireServer{responses: responses}
	server := httptest.NewServer(http.HandlerFunc(capture.serveHTTP))
	t.Cleanup(server.Close)
	return server, capture
}

func (s *capabilityWireServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	index := len(s.bodies)
	s.bodies = append(s.bodies, string(body))
	response := ""
	if index < len(s.responses) {
		response = s.responses[index]
	} else if len(s.responses) > 0 {
		response = s.responses[len(s.responses)-1]
	}
	s.mu.Unlock()
	if response == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(response))
}

func (s *capabilityWireServer) body(t *testing.T, index int) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.bodies) {
		t.Fatalf("request %d missing; only %d seen", index, len(s.bodies))
	}
	return s.bodies[index]
}

func (s *capabilityWireServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

const chatReasoningToolCall = `{"id":"chatcmpl-reasoning","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","reasoning_content":"need the file","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

const chatReasoningToolCallWithoutContent = `{"id":"chatcmpl-reasoning","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

func toolLoopRequest(first Message) *Request {
	return &Request{
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "inspect"}}},
			first,
			{Role: RoleTool, Parts: []Part{{
				Type:       PartToolResult,
				ToolResult: &ToolResult{CallID: "call_1", Content: "file body"},
			}}},
		},
		Tools: []ToolDef{readFileTool()},
	}
}

func TestOpenAIChatZeroMaxTokensOmitsTheField(t *testing.T) {
	t.Setenv("CAP_KEY", "k")
	server, capture := newCapabilityWireServer(t, chatOK)
	cfg := chatConfig(server.URL+"/v1", "CAP_KEY")
	cfg.MaxTokens = 0
	client := mustClient(t, "chat", cfg)
	if _, err := client.Generate(context.Background(), simpleRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	body := capture.body(t, 0)
	assertNotContains(t, body, `"max_tokens"`)
	assertNotContains(t, body, `"max_output_tokens"`)
}

func TestOpenAIChatRejectsNegativeMaxTokensBeforeHTTP(t *testing.T) {
	t.Setenv("CAP_KEY", "k")
	server, capture := newCapabilityWireServer(t, chatOK)
	cfg := chatConfig(server.URL+"/v1", "CAP_KEY")
	cfg.MaxTokens = -1
	client := mustClient(t, "chat", cfg)
	_, err := client.Generate(context.Background(), simpleRequest())
	requireKind(t, err, ErrBadRequest)
	if capture.count() != 0 {
		t.Fatalf("negative max_tokens reached the provider: %d requests", capture.count())
	}
}

func TestOpenAIChatThinkingTypeReachesWireAndAudit(t *testing.T) {
	for _, thinkingType := range []string{"enabled", "disabled"} {
		t.Run(thinkingType, func(t *testing.T) {
			t.Setenv("CAP_KEY", "k")
			server, capture := newCapabilityWireServer(t, chatOK)
			var records []CallRecord
			cfg := chatConfig(server.URL+"/v1", "CAP_KEY")
			cfg.Thinking = &config.ThinkingConfig{Type: thinkingType}
			cfg.Reasoning = &config.ReasoningConfig{Effort: "max"}
			client := mustClient(t, "chat", cfg,
				WithRecorder(RecorderFunc(func(_ context.Context, call CallRecord) { records = append(records, call) })))
			if _, err := client.Generate(context.Background(), simpleRequest()); err != nil {
				t.Fatalf("Generate: %v", err)
			}
			body := capture.body(t, 0)
			assertContains(t, body, `"thinking":{"type":"`+thinkingType+`"}`)
			assertContains(t, body, `"reasoning_effort":"max"`)
			if len(records) != 1 || records[0].Audit == nil || records[0].Audit.Request.Wire == nil {
				t.Fatalf("missing audit wire: %+v", records)
			}
			wire := records[0].Audit.Request.Wire
			if wire.ThinkingType != thinkingType || wire.ThinkingEnabled != (thinkingType == "enabled") {
				t.Fatalf("wire thinking = %+v", wire)
			}
		})
	}
}

func TestOpenAIChatReasoningEffortNoneRequiresCapability(t *testing.T) {
	t.Setenv("CAP_KEY", "k")
	server, capture := newCapabilityWireServer(t, chatOK)
	cfg := chatConfig(server.URL+"/v1", "CAP_KEY")
	cfg.Reasoning = &config.ReasoningConfig{Effort: "none"}
	client := mustClient(t, "chat", cfg)
	_, err := client.Generate(context.Background(), simpleRequest())
	requireKind(t, err, ErrBadRequest)
	if capture.count() != 0 {
		t.Fatalf("effort none without capability reached the provider")
	}

	cfg.AllowReasoningEffortNone = true
	client = mustClient(t, "chat", cfg)
	if _, err := client.Generate(context.Background(), simpleRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	body := capture.body(t, 0)
	assertContains(t, body, `"reasoning_effort":"none"`)
}

func TestOpenAIChatPreservesReasoningContentAcrossToolTurns(t *testing.T) {
	t.Setenv("CAP_KEY", "k")
	server, capture := newCapabilityWireServer(t, chatReasoningToolCall, chatOK)
	var records []CallRecord
	cfg := chatConfig(server.URL+"/v1", "CAP_KEY")
	cfg.PreserveReasoningContent = true
	client := mustClient(t, "chat", cfg,
		WithRecorder(RecorderFunc(func(_ context.Context, call CallRecord) { records = append(records, call) })))
	first, err := client.Generate(context.Background(), &Request{
		Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "inspect"}}}},
		Tools:    []ToolDef{readFileTool()},
	})
	if err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	reasoning := findPart(first.Message, PartReasoning)
	if reasoning == nil || reasoning.Reasoning.Protocol != "openai-chat" {
		t.Fatalf("missing openai-chat reasoning part: %+v", first.Message)
	}
	var payload struct {
		ReasoningContent string `json:"reasoning_content"`
	}
	if err := json.Unmarshal(reasoning.Reasoning.Raw, &payload); err != nil || payload.ReasoningContent != "need the file" {
		t.Fatalf("reasoning payload = %s err=%v", reasoning.Reasoning.Raw, err)
	}
	if _, err := client.Generate(context.Background(), toolLoopRequest(first.Message)); err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	secondBody := capture.body(t, 1)
	assertContains(t, secondBody, `"reasoning_content":"need the file"`)
	assertContains(t, secondBody, `"tool_calls"`)
	if len(records) != 2 {
		t.Fatalf("records = %d", len(records))
	}
	if records[0].Audit.Request.Wire.ReasoningContentReplayed {
		t.Fatal("first request replayed reasoning content")
	}
	if !records[1].Audit.Request.Wire.ReasoningContentReplayed {
		t.Fatal("second request did not report replayed reasoning content")
	}
	if raw, _ := json.Marshal(records[1].Audit.Request); strings.Contains(string(raw), "need the file") {
		t.Fatalf("audit stored reasoning content: %s", raw)
	}
}

func TestOpenAIChatDropsReasoningContentWithoutCapability(t *testing.T) {
	t.Setenv("CAP_KEY", "k")
	firstServer, firstCapture := newCapabilityWireServer(t, chatReasoningToolCall, chatOK)
	client := mustClient(t, "chat", chatConfig(firstServer.URL+"/v1", "CAP_KEY"))
	first, err := client.Generate(context.Background(), &Request{
		Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "inspect"}}}},
		Tools:    []ToolDef{readFileTool()},
	})
	if err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	if findPart(first.Message, PartReasoning) != nil {
		t.Fatalf("reasoning part kept without the capability: %+v", first.Message)
	}
	if _, err := client.Generate(context.Background(), toolLoopRequest(first.Message)); err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	assertNotContains(t, firstCapture.body(t, 1), `"reasoning_content"`)

	// The dropped extension must not change the request body: the same loop
	// with a response that never carried reasoning_content is byte-identical.
	plainServer, plainCapture := newCapabilityWireServer(t, chatReasoningToolCallWithoutContent, chatOK)
	plain := mustClient(t, "chat", chatConfig(plainServer.URL+"/v1", "CAP_KEY"))
	plainFirst, err := plain.Generate(context.Background(), &Request{
		Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "inspect"}}}},
		Tools:    []ToolDef{readFileTool()},
	})
	if err != nil {
		t.Fatalf("plain first Generate: %v", err)
	}
	if _, err := plain.Generate(context.Background(), toolLoopRequest(plainFirst.Message)); err != nil {
		t.Fatalf("plain second Generate: %v", err)
	}
	if firstCapture.body(t, 1) != plainCapture.body(t, 1) {
		t.Fatalf("dropped reasoning changed the request body:\nwith extension: %s\nwithout:        %s",
			firstCapture.body(t, 1), plainCapture.body(t, 1))
	}
}

func TestOpenAIResponsesZeroMaxTokensOmitsTheField(t *testing.T) {
	t.Setenv("CAP_KEY", "k")
	server, capture := newCapabilityWireServer(t, `{"id":"resp","object":"response","status":"completed","model":"test","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	cfg := config.ProviderConfig{Protocol: "openai-responses", BaseURL: server.URL + "/v1", Model: "test", APIKeyEnv: "CAP_KEY", MaxConcurrency: 1, MaxTokens: 0}
	client := mustClient(t, "responses", cfg)
	if _, err := client.Generate(context.Background(), simpleRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	body := capture.body(t, 0)
	assertNotContains(t, body, `"max_output_tokens"`)
	assertContains(t, body, `"store":false`)
}

func TestOpenAIResponsesParamsCapability(t *testing.T) {
	t.Setenv("CAP_KEY", "k")
	response := `{"id":"resp","object":"response","status":"completed","model":"test","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
	server, capture := newCapabilityWireServer(t, response)
	cfg := config.ProviderConfig{Protocol: "openai-responses", BaseURL: server.URL + "/v1", Model: "test", APIKeyEnv: "CAP_KEY", MaxConcurrency: 1, MaxTokens: 0}
	cfg.ResponsesParams = "unsupported"
	client := mustClient(t, "responses", cfg)
	if _, err := client.Generate(context.Background(), simpleRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	body := capture.body(t, 0)
	assertNotContains(t, body, `"store"`)
	assertNotContains(t, body, `"include"`)
	assertNotContains(t, body, "encrypted_content")
}

func TestOpenAIResponsesReasoningEffortNoneRequiresCapability(t *testing.T) {
	t.Setenv("CAP_KEY", "k")
	response := `{"id":"resp","object":"response","status":"completed","model":"test","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
	server, capture := newCapabilityWireServer(t, response)
	cfg := config.ProviderConfig{Protocol: "openai-responses", BaseURL: server.URL + "/v1", Model: "test", APIKeyEnv: "CAP_KEY", MaxConcurrency: 1, MaxTokens: 0, Reasoning: &config.ReasoningConfig{Effort: "none"}}
	client := mustClient(t, "responses", cfg)
	_, err := client.Generate(context.Background(), simpleRequest())
	requireKind(t, err, ErrBadRequest)
	if capture.count() != 0 {
		t.Fatal("effort none without capability reached the provider")
	}

	cfg.AllowReasoningEffortNone = true
	var records []CallRecord
	client = mustClient(t, "responses", cfg,
		WithRecorder(RecorderFunc(func(_ context.Context, call CallRecord) { records = append(records, call) })))
	if _, err := client.Generate(context.Background(), simpleRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	assertContains(t, capture.body(t, 0), `"reasoning":{"effort":"none"}`)
	if len(records) != 1 || records[0].Audit.Request.Wire.ThinkingEnabled {
		t.Fatalf("effort none must not report thinking enabled: %+v", records)
	}
}

func TestAnthropicMaxTokensRequirement(t *testing.T) {
	t.Setenv("CAP_KEY", "k")
	server, capture := newCapabilityWireServer(t, chatOK)
	for _, maxTokens := range []int{0, -1} {
		cfg := config.ProviderConfig{Protocol: "anthropic", BaseURL: server.URL, Model: "test", APIKeyEnv: "CAP_KEY", MaxConcurrency: 1, MaxTokens: maxTokens}
		client := mustClient(t, "anthropic", cfg)
		_, err := client.Generate(context.Background(), simpleRequest())
		requireKind(t, err, ErrBadRequest)
	}
	if capture.count() != 0 {
		t.Fatalf("invalid anthropic requests reached the provider: %d", capture.count())
	}
}

func TestAnthropicOutputConfigEffort(t *testing.T) {
	t.Setenv("CAP_KEY", "k")
	response := `{"id":"msg","type":"message","role":"assistant","model":"test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	server, capture := newCapabilityWireServer(t, response)
	cfg := config.ProviderConfig{Protocol: "anthropic", BaseURL: server.URL, Model: "test", APIKeyEnv: "CAP_KEY", MaxConcurrency: 1, MaxTokens: 64}
	cfg.OutputConfigEffort = true
	cfg.Reasoning = &config.ReasoningConfig{Effort: "max"}
	var records []CallRecord
	client := mustClient(t, "anthropic", cfg,
		WithRecorder(RecorderFunc(func(_ context.Context, call CallRecord) { records = append(records, call) })))
	if _, err := client.Generate(context.Background(), simpleRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	assertContains(t, capture.body(t, 0), `"output_config":{"effort":"max"}`)
	if len(records) != 1 || records[0].Audit.Request.Wire.Reasoning == nil ||
		records[0].Audit.Request.Wire.Reasoning.Effort != "max" {
		t.Fatalf("wire did not record output_config effort: %+v", records)
	}

	cfg.OutputConfigEffort = false
	client = mustClient(t, "anthropic", cfg)
	if _, err := client.Generate(context.Background(), simpleRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	assertNotContains(t, capture.body(t, 1), `"output_config"`)
}

func TestAnthropicOutputConfigRejectsEffortNone(t *testing.T) {
	t.Setenv("CAP_KEY", "k")
	server, capture := newCapabilityWireServer(t, `{"id":"msg","type":"message","role":"assistant","model":"test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	cfg := config.ProviderConfig{Protocol: "anthropic", BaseURL: server.URL, Model: "test", APIKeyEnv: "CAP_KEY", MaxConcurrency: 1, MaxTokens: 64,
		OutputConfigEffort: true, AllowReasoningEffortNone: true, Reasoning: &config.ReasoningConfig{Effort: "none"}}
	client := mustClient(t, "anthropic", cfg)
	_, err := client.Generate(context.Background(), simpleRequest())
	requireKind(t, err, ErrBadRequest)
	if capture.count() != 0 {
		t.Fatal("anthropic effort none reached the provider")
	}
}

// Existing configurations must keep their audited wire shape: the new fields
// only appear when a capability actually sends them.
func TestWireParamsOmitsUnsetCapabilityFields(t *testing.T) {
	raw, err := json.Marshal(WireParams{
		MaxTokens:   4096,
		Temperature: floatPtr(0.2),
		Reasoning:   &WireReasoning{Effort: "high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "thinking_type") || strings.Contains(string(raw), "reasoning_content_replayed") {
		t.Fatalf("unset capability fields were serialized: %s", raw)
	}
}

func TestChatUsageReadsDeepSeekPromptCacheCounters(t *testing.T) {
	raw := `{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_cache_hit_tokens":7,"prompt_cache_miss_tokens":3}`
	usage, complete := wireUsage(raw, chatUsageFields)
	if !complete {
		t.Fatal("usage marked incomplete")
	}
	want := Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 7, CacheMissTokens: 3}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
	if added := AddUsage(usage, Usage{CacheMissTokens: 1}); added.CacheMissTokens != 4 {
		t.Fatalf("cache miss tokens not accumulated: %+v", added)
	}
}
