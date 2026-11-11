package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestProviderCapabilityBounds covers the relaxed capability parameter
// semantics (#404): zero max_tokens means "send no limit" for the OpenAI
// protocols, anthropic still requires a positive value, and zero
// max_concurrency means "no local semaphore".
func TestProviderCapabilityBounds(t *testing.T) {
	base := func(protocol string) ProviderConfig {
		return ProviderConfig{Protocol: protocol, Model: "m", MaxTokens: 1, MaxConcurrency: 1}
	}
	for _, tc := range []struct {
		name    string
		cfg     ProviderConfig
		wantErr string
	}{
		{"chat zero max tokens", func() ProviderConfig { c := base("openai-chat"); c.MaxTokens = 0; return c }(), ""},
		{"responses zero max tokens", func() ProviderConfig { c := base("openai-responses"); c.MaxTokens = 0; return c }(), ""},
		{"anthropic zero max tokens", func() ProviderConfig { c := base("anthropic"); c.MaxTokens = 0; return c }(), "max_tokens must be positive for anthropic"},
		{"chat negative max tokens", func() ProviderConfig { c := base("openai-chat"); c.MaxTokens = -1; return c }(), "max_tokens must not be negative"},
		{"anthropic negative max tokens", func() ProviderConfig { c := base("anthropic"); c.MaxTokens = -1; return c }(), "max_tokens must not be negative"},
		{"zero max concurrency", func() ProviderConfig { c := base("openai-chat"); c.MaxConcurrency = 0; return c }(), ""},
		{"negative max concurrency", func() ProviderConfig { c := base("openai-chat"); c.MaxConcurrency = -1; return c }(), "max_concurrency must not be negative"},
		{"thinking enabled", func() ProviderConfig {
			c := base("openai-chat")
			c.Thinking = &ThinkingConfig{Type: "enabled"}
			return c
		}(), ""},
		{"thinking disabled", func() ProviderConfig {
			c := base("openai-chat")
			c.Thinking = &ThinkingConfig{Type: "disabled"}
			return c
		}(), ""},
		{"thinking invalid type", func() ProviderConfig {
			c := base("openai-chat")
			c.Thinking = &ThinkingConfig{Type: "always"}
			return c
		}(), "thinking.type"},
		{"responses params unsupported", func() ProviderConfig {
			c := base("openai-responses")
			c.ResponsesParams = "unsupported"
			return c
		}(), ""},
		{"responses params invalid", func() ProviderConfig {
			c := base("openai-responses")
			c.ResponsesParams = "never"
			return c
		}(), "responses_params"},
		{"effort none with capability", func() ProviderConfig {
			c := base("openai-chat")
			c.AllowReasoningEffortNone = true
			c.Reasoning = &ReasoningConfig{Effort: "none"}
			return c
		}(), ""},
		{"effort none without capability", func() ProviderConfig {
			c := base("openai-chat")
			c.Reasoning = &ReasoningConfig{Effort: "none"}
			return c
		}(), "allow_reasoning_effort_none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateProviderConfig("model", tc.cfg, ProviderValidationOptions{})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateProviderConfig() = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadReasoningEffortNoneRequiresCapability(t *testing.T) {
	path := writeConfig(t, "none.yaml", `
providers:
  p: {protocol: openai-chat, model: m, reasoning: {effort: none}}
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "allow_reasoning_effort_none") {
		t.Fatalf("effort none without capability load error = %v", err)
	}
	path = writeConfig(t, "none-capability.yaml", `
providers:
  p: {protocol: openai-chat, model: m, allow_reasoning_effort_none: true, reasoning: {effort: none}}
`)
	warned, err := Load(path)
	if err != nil {
		t.Fatalf("effort none with capability rejected: %v", err)
	}
	for _, warning := range warned.Warnings {
		if strings.Contains(warning, "reasoning.effort") {
			t.Fatalf("capability-declaring provider warned about effort: %v", warned.Warnings)
		}
	}
}

func TestThinkingConfigJSONIncludesTypeOnlyWhenSet(t *testing.T) {
	raw, err := json.Marshal(ProviderConfig{Thinking: &ThinkingConfig{BudgetTokens: 2048}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"type"`) {
		t.Fatalf("empty thinking type must be omitted: %s", raw)
	}
	raw, err = json.Marshal(ProviderConfig{Thinking: &ThinkingConfig{BudgetTokens: 2048, Type: "enabled"}})
	if err != nil || !strings.Contains(string(raw), `"type":"enabled"`) {
		t.Fatalf("configured thinking type must be part of the identity: %s err=%v", raw, err)
	}
}

func TestNodeParamsThinkingType(t *testing.T) {
	params := NodeParams{Thinking: &ThinkingConfig{Type: "enabled"}}
	if params.IsZero() {
		t.Fatal("a thinking type alone must not be zero")
	}
	overlaid := NodeParams{}.Overlay(params)
	if overlaid.Thinking == nil || overlaid.Thinking.Type != "enabled" {
		t.Fatalf("overlay lost thinking type: %+v", overlaid.Thinking)
	}
	own := NodeParams{Thinking: &ThinkingConfig{BudgetTokens: 1024}}
	merged := own.Overlay(NodeParams{Thinking: &ThinkingConfig{Type: "disabled"}})
	if merged.Thinking == nil || merged.Thinking.BudgetTokens != 1024 || merged.Thinking.Type != "" {
		t.Fatalf("node thinking must replace the default wholesale: %+v", merged.Thinking)
	}
	if err := ValidateNodeParams("params", NodeParams{Thinking: &ThinkingConfig{Type: "always"}}); err == nil ||
		!strings.Contains(err.Error(), "thinking.type") {
		t.Fatalf("invalid thinking type accepted: %v", err)
	}
	if err := ValidateNodeParams("params", NodeParams{Thinking: &ThinkingConfig{Type: "disabled"}}); err != nil {
		t.Fatalf("thinking type without a budget rejected: %v", err)
	}
}

func TestNodeParamsReasoningEffortNoneIsStructurallyAccepted(t *testing.T) {
	// The model capability is checked by the profile override validation,
	// which knows the effective model; this layer only fixes the vocabulary.
	if err := ValidateNodeParams("params", NodeParams{Reasoning: &ReasoningConfig{Effort: "none"}}); err != nil {
		t.Fatalf("effort none rejected structurally: %v", err)
	}
	if err := ValidateNodeParams("params", NodeParams{Reasoning: &ReasoningConfig{Effort: "extreme"}}); err == nil {
		t.Fatal("unknown effort accepted")
	}
}
