package config

import (
	"strings"
	"testing"
)

func xhighProvider() ProviderConfig {
	return ProviderConfig{
		Protocol:       "openai-chat",
		Model:          "m",
		MaxTokens:      1,
		MaxConcurrency: 1,
		Reasoning:      &ReasoningConfig{Effort: "xhigh"},
	}
}

func TestProviderEffortAcceptsXHigh(t *testing.T) {
	if err := ValidateProviderConfig("model", xhighProvider(), ProviderValidationOptions{}); err != nil {
		t.Fatalf("provider effort xhigh rejected: %v", err)
	}
	path := writeConfig(t, "xhigh.yaml", `
providers:
  p: {protocol: openai-chat, model: m, reasoning: {effort: xhigh}}
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("load provider effort xhigh: %v", err)
	}
}

func TestNodeParamsEffortAcceptsXHigh(t *testing.T) {
	if err := ValidateNodeParams("params", NodeParams{Reasoning: &ReasoningConfig{Effort: "xhigh"}}); err != nil {
		t.Fatalf("node effort xhigh rejected: %v", err)
	}
}

func TestEffortErrorsListXHigh(t *testing.T) {
	provider := xhighProvider()
	provider.Reasoning = &ReasoningConfig{Effort: "ultra"}
	if err := ValidateProviderConfig("model", provider, ProviderValidationOptions{}); err == nil ||
		!strings.Contains(err.Error(), "xhigh") {
		t.Fatalf("provider error = %v, want xhigh in the allowed list", err)
	}
	snapshotCfg := &Config{Providers: map[string]ProviderConfig{"p": provider}}
	if err := checkProviderEffort(snapshotCfg); err == nil || !strings.Contains(err.Error(), "xhigh") {
		t.Fatalf("snapshot error = %v, want xhigh in the allowed list", err)
	}
	if err := ValidateNodeParams("params", NodeParams{Reasoning: &ReasoningConfig{Effort: "ultra"}}); err == nil ||
		!strings.Contains(err.Error(), "xhigh") {
		t.Fatalf("node error = %v, want xhigh in the allowed list", err)
	}
	if err := ValidateNodeParams("params", NodeParams{Reasoning: &ReasoningConfig{}}); err == nil ||
		!strings.Contains(err.Error(), "xhigh") {
		t.Fatalf("empty node effort error = %v, want xhigh in the allowed list", err)
	}
}
