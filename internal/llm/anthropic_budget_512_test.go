package llm

import (
	"context"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
)

// #512: the adapter fails closed when the effective thinking budget is not
// below max_tokens. Configuration validation catches API-written models, but
// a registry can be built without it.
func TestAnthropicRejectsThinkingBudgetNotBelowMaxTokens(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY", "test-anthropic-key")
	cases := []struct {
		name      string
		maxTokens int
		budget    int
	}{
		{name: "budget above max_tokens", maxTokens: 1024, budget: 2048},
		{name: "budget equals max_tokens", maxTokens: 2048, budget: 2048},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
			server, capture := newCapabilityWireServer(t, response)
			client := mustClient(t, "anthropic", config.ProviderConfig{
				Protocol:  "anthropic",
				BaseURL:   server.URL,
				Model:     "claude-test",
				APIKeyEnv: "TEST_ANTHROPIC_KEY",
				MaxTokens: tc.maxTokens,
				Thinking:  &config.ThinkingConfig{BudgetTokens: tc.budget},
			})
			_, err := client.Generate(context.Background(), &Request{
				Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "inspect"}}}},
			})
			failure := requireKind(t, err, ErrBadRequest)
			if failure.Retryable() {
				t.Fatal("thinking budget mismatch must not be retryable")
			}
			if capture.count() != 0 {
				t.Fatalf("request must not be sent, saw %d", capture.count())
			}
		})
	}
}
