package sqlite

import (
	"context"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// #512: model writes reject an anthropic thinking budget that is not below
// max_tokens, because the provider rejects every request with a 400.
func TestModelValidationRejectsThinkingBudgetNotBelowMaxTokens(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	if err := s.ConfigureModelSecrets(ctx, sealTestCipher(t, 1)); err != nil {
		t.Fatal(err)
	}
	input := store.Model{
		DisplayName: "claude", Protocol: "anthropic", BaseURL: "https://example.test", Model: "claude-x",
		Params: store.ModelParams{
			MaxTokens: 1024,
			Thinking:  &config.ThinkingConfig{BudgetTokens: 2048},
		},
		CreatedBy: 1,
	}
	if _, err := s.CreateModel(ctx, input, ""); err == nil {
		t.Fatal("thinking budget above max_tokens must be rejected")
	}
	input.Params.MaxTokens = 2048
	if _, err := s.CreateModel(ctx, input, ""); err == nil {
		t.Fatal("thinking budget equal to max_tokens must be rejected")
	}
	input.Params.MaxTokens = 4096
	if _, err := s.CreateModel(ctx, input, ""); err != nil {
		t.Fatalf("valid thinking budget rejected: %v", err)
	}
}
