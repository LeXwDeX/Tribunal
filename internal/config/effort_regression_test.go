package config

import (
	"strings"
	"testing"
)

func TestProviderEffortValidationAndWarnings(t *testing.T) {
	path := writeConfig(t, "effort.yaml", `
providers:
  bad: {protocol: openai-chat, reasoning: {effort: invalid}}
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "providers.bad.reasoning.effort") {
		t.Fatalf("invalid effort load error = %v", err)
	}
	cfg := &Config{Providers: map[string]ProviderConfig{"bad": {Reasoning: &ReasoningConfig{Effort: "invalid"}}}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "providers.bad.reasoning.effort") {
		t.Fatalf("invalid effort validation error = %v", err)
	}
	path = writeConfig(t, "warnings.yaml", `
providers:
  a: {protocol: anthropic, reasoning: {effort: high}}
  b: {protocol: openai-chat, thinking: {budget_tokens: 2048}}
  c: {protocol: openai-responses, thinking: {budget_tokens: 2048}}
`)
	warned, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"providers.a.reasoning.effort", "providers.b.thinking.budget_tokens", "providers.c.thinking.budget_tokens"} {
		requireWarning(t, warned.Warnings, field)
	}
}
