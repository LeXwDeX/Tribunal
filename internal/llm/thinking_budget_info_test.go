package llm

import (
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
)

func TestProviderInfoReportsAnthropicThinkingBudget(t *testing.T) {
	t.Setenv("TEST_KEY", "k")
	cfg := config.ProviderConfig{
		Protocol: "anthropic", BaseURL: "http://127.0.0.1:1", Model: "claude-sonnet",
		APIKeyEnv: "TEST_KEY", MaxConcurrency: 1, MaxTokens: 4096,
		Thinking: &config.ThinkingConfig{BudgetTokens: 2048},
	}
	if got := mustClient(t, "claude", cfg).Info().ThinkingBudget; got != 2048 {
		t.Fatalf("thinking budget = %d, want 2048", got)
	}

	cfg.Thinking = nil
	if got := mustClient(t, "claude", cfg).Info().ThinkingBudget; got != 0 {
		t.Fatalf("thinking budget without thinking = %d, want 0", got)
	}

	openai := chatConfig("http://127.0.0.1:1/v1", "TEST_KEY")
	openai.Thinking = &config.ThinkingConfig{BudgetTokens: 2048}
	if got := mustClient(t, "chat", openai).Info().ThinkingBudget; got != 0 {
		t.Fatalf("non-anthropic thinking budget = %d, want 0", got)
	}
}
