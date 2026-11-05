package config

import (
	"strings"
	"testing"
)

func TestModelProviderValidation(t *testing.T) {
	temp := 2.1
	cfg := ProviderConfig{Protocol: "openai-chat", Model: "model", MaxConcurrency: 1, MaxTokens: 1}
	if err := ValidateProviderConfig("model", cfg, ProviderValidationOptions{}); err != nil {
		t.Fatalf("valid model: %v", err)
	}
	cfg.Temperature = &temp
	cfg.MaxTokens = 0
	cfg.Thinking = &ThinkingConfig{BudgetTokens: -1}
	cfg.ContextWindow = -1
	err := ValidateProviderConfig("model", cfg, ProviderValidationOptions{})
	for _, field := range []string{"temperature", "max_tokens", "budget_tokens", "context_window"} {
		if err == nil || !strings.Contains(err.Error(), "model."+field) && !strings.Contains(err.Error(), "model.thinking."+field) {
			t.Errorf("missing %s error: %v", field, err)
		}
	}
}

func TestProviderNameRejectsColon(t *testing.T) {
	cfg := &Config{Providers: map[string]ProviderConfig{"user:collision": {}}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "providers.user:collision") {
		t.Fatalf("provider name error missing: %v", err)
	}
}

func TestModelProviderURLAndBounds(t *testing.T) {
	cfg := ProviderConfig{Protocol: "openai-chat", Model: "m", MaxConcurrency: 1, MaxTokens: 1}
	for _, url := range []string{"https://user:password@example.com", "https://example.com/?secret=value", "https://example.com/#fragment"} {
		cfg.BaseURL = url
		if err := ValidateProviderConfig("model", cfg, ProviderValidationOptions{}); err == nil || strings.Contains(err.Error(), url) {
			t.Fatal("invalid URL accepted or included in error")
		}
	}
	cfg.BaseURL = "https://example.com/v1"
	for _, temperature := range []float64{0, 2} {
		cfg.Temperature = &temperature
		if err := ValidateProviderConfig("model", cfg, ProviderValidationOptions{}); err != nil {
			t.Fatalf("valid boundary rejected: %v", err)
		}
	}
	if err := ValidateProviderConfig("model", cfg, ProviderValidationOptions{RequireAPIKeyEnv: true}); err == nil || !strings.Contains(err.Error(), "model.api_key_env") {
		t.Fatal("missing environment key accepted")
	}
}
