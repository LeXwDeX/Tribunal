package server

import (
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// #512: model create/update returns a field-level error for an anthropic
// thinking budget that is not below max_tokens.
func TestModelDetailsRejectThinkingBudgetNotBelowMaxTokens(t *testing.T) {
	model := store.Model{
		DisplayName: "claude", Protocol: "anthropic", BaseURL: "https://example.test", Model: "claude-x",
		Params: store.ModelParams{
			MaxTokens: 1024,
			Thinking:  &config.ThinkingConfig{BudgetTokens: 2048},
		},
	}
	details := modelDetails(model)
	if len(details) != 1 {
		t.Fatalf("details = %+v, want one thinking budget error", details)
	}
	if details[0].Field != "params.thinking.budget_tokens" {
		t.Fatalf("field = %q, want params.thinking.budget_tokens", details[0].Field)
	}
	model.Params.MaxTokens = 4096
	if details := modelDetails(model); len(details) != 0 {
		t.Fatalf("valid model rejected: %+v", details)
	}
}
