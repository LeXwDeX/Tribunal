package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
)

func TestWireAuditMatchesProtocolRequest(t *testing.T) {
	t.Setenv("WIRE_TEST_KEY", "test")
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				w.WriteHeader(http.StatusBadRequest)
			}))
			defer server.Close()
			var record CallRecord
			cfg := config.ProviderConfig{Protocol: protocol, Model: "m", BaseURL: server.URL, APIKeyEnv: "WIRE_TEST_KEY", MaxTokens: 4096, Temperature: floatPtr(0.4), Reasoning: &config.ReasoningConfig{Effort: "high"}, Thinking: &config.ThinkingConfig{BudgetTokens: 2048}}
			client := mustClient(t, "p", cfg, WithRecorder(RecorderFunc(func(_ context.Context, r CallRecord) { record = r })))
			_, _ = client.Generate(context.Background(), simpleRequest())
			if record.Audit == nil || record.Audit.Request == nil {
				t.Fatal("missing audit")
			}
			raw, _ := json.Marshal(record.Audit.Request)
			var audit map[string]any
			if err := json.Unmarshal(raw, &audit); err != nil {
				t.Fatal(err)
			}
			wire, ok := audit["wire"].(map[string]any)
			if !ok {
				t.Fatalf("missing wire: %s", raw)
			}
			maxKey := "max_tokens"
			if protocol == "openai-responses" {
				maxKey = "max_output_tokens"
			}
			if wire["max_tokens"] != body[maxKey] {
				t.Fatalf("wire max=%v body=%v", wire["max_tokens"], body[maxKey])
			}
			if wire["temperature"] != body["temperature"] {
				t.Fatalf("wire temperature=%v body=%v", wire["temperature"], body["temperature"])
			}
			if protocol == "anthropic" {
				if wire["reasoning"] != nil || wire["thinking_enabled"] != true || wire["temperature"] != nil {
					t.Fatalf("anthropic wire=%v", wire)
				}
				if wire["thinking"].(map[string]any)["budget_tokens"] != body["thinking"].(map[string]any)["budget_tokens"] {
					t.Fatalf("thinking wire=%v body=%v", wire, body)
				}
			} else {
				effort := body["reasoning_effort"]
				if protocol == "openai-responses" {
					effort = body["reasoning"].(map[string]any)["effort"]
				}
				if wire["reasoning"].(map[string]any)["effort"] != effort || effort != "high" || wire["thinking"] != nil {
					t.Fatalf("wire=%v body=%v", wire, body)
				}
			}
		})
	}
}

func TestWireAuditOpenAIEffortsAndAnthropicForcedChoice(t *testing.T) {
	t.Setenv("WIRE_TEST_KEY", "test")
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic"} {
		for _, value := range []string{"low", "medium", "high", "max"} {
			t.Run(protocol+"/"+value, func(t *testing.T) {
				var body map[string]any
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					w.WriteHeader(http.StatusBadRequest)
				}))
				defer server.Close()
				var record CallRecord
				cfg := config.ProviderConfig{Protocol: protocol, Model: "m", BaseURL: server.URL, APIKeyEnv: "WIRE_TEST_KEY", MaxTokens: 4096, Temperature: floatPtr(0.2), Thinking: &config.ThinkingConfig{BudgetTokens: 2048}}
				client := mustClient(t, "p", cfg, WithRecorder(RecorderFunc(func(_ context.Context, r CallRecord) { record = r })))
				req := simpleRequest()
				req.Reasoning = &ReasoningOpts{Effort: value}
				if protocol == "anthropic" {
					req.ToolChoice = "required"
					req.Tools = []ToolDef{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}}
				}
				_, _ = client.Generate(context.Background(), req)
				if record.Audit == nil || record.Audit.Request.Wire == nil {
					t.Fatal("missing wire")
				}
				wire := record.Audit.Request.Wire
				if record.Audit.Request.Reasoning.Effort != value {
					t.Fatal("request effort not retained")
				}
				if protocol == "anthropic" {
					if wire.Reasoning != nil || wire.ThinkingEnabled || wire.Thinking != nil || wire.Temperature == nil || *wire.Temperature != body["temperature"] {
						t.Fatalf("wire=%+v body=%v", wire, body)
					}
					if body["tool_choice"] == nil || body["thinking"] != nil {
						t.Fatalf("forced tool choice body=%v", body)
					}
				} else {
					sent := body["reasoning_effort"]
					if protocol == "openai-responses" {
						sent = body["reasoning"].(map[string]any)["effort"]
					}
					if wire.Reasoning == nil || wire.Reasoning.Effort != value || sent != value {
						t.Fatalf("wire=%+v body=%v", wire, body)
					}
				}
			})
		}
	}
}
