package config

import (
	"strings"
	"testing"
)

func anthropicBudgetProvider(maxTokens, budget int) ProviderConfig {
	return ProviderConfig{
		Protocol:  "anthropic",
		BaseURL:   "https://anthropic.example.com",
		Model:     "claude-test",
		APIKeyEnv: "TEST_ANTHROPIC_KEY",
		MaxTokens: maxTokens,
		Thinking:  &ThinkingConfig{BudgetTokens: budget},
	}
}

// #512: Anthropic requires thinking.budget_tokens < max_tokens. API-written
// models reject the mismatch instead of storing a model whose every request
// fails with a non-retryable 400.
func TestValidateProviderConfigRejectsThinkingBudgetNotBelowMaxTokens(t *testing.T) {
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
			err := ValidateProviderConfig("model", anthropicBudgetProvider(tc.maxTokens, tc.budget), ProviderValidationOptions{EnforceThinkingBudget: true})
			if err == nil || !strings.Contains(err.Error(), "thinking.budget_tokens must be less than max_tokens") {
				t.Fatalf("ValidateProviderConfig() = %v, want thinking budget error", err)
			}
		})
	}
}

func TestValidateProviderConfigThinkingBudgetScope(t *testing.T) {
	openAIConfig := ProviderConfig{
		Protocol:  "openai-chat",
		BaseURL:   "https://openai.example.com",
		Model:     "gpt-test",
		APIKeyEnv: "TEST_OPENAI_KEY",
		MaxTokens: 1024,
		Thinking:  &ThinkingConfig{BudgetTokens: 2048},
	}
	cases := []struct {
		name string
		cfg  ProviderConfig
		opts ProviderValidationOptions
	}{
		{
			name: "valid anthropic budget is accepted",
			cfg:  anthropicBudgetProvider(4096, 2048),
			opts: ProviderValidationOptions{EnforceThinkingBudget: true},
		},
		{
			name: "config-file validation only warns",
			cfg:  anthropicBudgetProvider(1024, 2048),
			opts: ProviderValidationOptions{},
		},
		{
			name: "openai thinking budget is not sent by the adapter",
			cfg:  openAIConfig,
			opts: ProviderValidationOptions{EnforceThinkingBudget: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateProviderConfig("model", tc.cfg, tc.opts); err != nil {
				t.Fatalf("ValidateProviderConfig() = %v, want nil", err)
			}
		})
	}
}

// Existing deployments with a mismatched provider must still start; the load
// path records a warning instead of rejecting the configuration.
func TestLoadWarnsWhenAnthropicThinkingBudgetIsNotBelowMaxTokens(t *testing.T) {
	base := writeConfig(t, "base.yaml", `
server:
  listen: ":9000"
gitlab:
  url: "https://gitlab.example.com"
providers:
  primary:
    protocol: anthropic
    model: claude
    api_key_env: PRIMARY_KEY
    max_tokens: 1024
    thinking:
      budget_tokens: 2048
`)
	cfg, err := Load(base)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "providers.primary.thinking.budget_tokens") ||
		!strings.Contains(cfg.Warnings[0], "max_tokens") {
		t.Fatalf("warnings = %q", cfg.Warnings)
	}
	if strings.Contains(cfg.Warnings[0], "2048") || strings.Contains(cfg.Warnings[0], "1024") {
		t.Fatalf("warning must not include values: %q", cfg.Warnings[0])
	}
}
