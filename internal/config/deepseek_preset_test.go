package config

import (
	"os"
	"strings"
	"testing"
)

// The documented DeepSeek provider values must be parseable without becoming
// a default deployment. The fixture mirrors the commented template blocks.
func TestDeepSeekPresetFixtureLoadsAndValidates(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DEEPSEEK_API_KEY", "test-deepseek-key")

	cfg := loadConfig(t, "testdata/deepseek-preset.yaml")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(fixture) = %v", err)
	}
	for name, concurrency := range map[string]int{"deepseek-v4-pro": 500, "deepseek-flash": 2500} {
		provider, ok := cfg.Providers[name]
		if !ok {
			t.Fatalf("providers.%s is missing", name)
		}
		if provider.Protocol != "openai-chat" || provider.BaseURL != "https://api.deepseek.com" ||
			provider.Model != name || provider.APIKeyEnv != "DEEPSEEK_API_KEY" {
			t.Errorf("providers.%s = %+v", name, provider)
		}
		if provider.MaxTokens != 393216 || provider.ContextWindow != 655360 || provider.MaxConcurrency != concurrency {
			t.Errorf("providers.%s limits = %+v", name, provider)
		}
		if provider.Reasoning == nil || provider.Reasoning.Effort != "max" {
			t.Errorf("providers.%s reasoning = %+v", name, provider.Reasoning)
		}
		if provider.ForcedToolChoice != "unsupported" || !provider.PreserveReasoningContent || provider.StrictRoleAlternation {
			t.Errorf("providers.%s capabilities = %+v", name, provider)
		}
	}
	if provider := cfg.Profiles["default"].Provider; provider != "deepseek-v4-pro" {
		t.Errorf("fixture profile provider = %q", provider)
	}
}

func TestDeepSeekPresetTemplatesDocumentVerifiedValues(t *testing.T) {
	for _, path := range []string{"../../config/server.example.yaml", dockerConfigPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := string(data)
		for _, want := range []string{
			"DeepSeek-only: do not",
			"these values to another provider",
			"https://api-docs.deepseek.com/quick_start/pricing",
			"verified 2026-11-11",
			`model: "deepseek-v4-pro"`,
			`model: "deepseek-flash"`,
			"max_concurrency: 500",
			"max_concurrency: 2500",
			"max_tokens: 393216",
			"context_window: 655360",
			"reasoning: { effort: max }",
			"forced_tool_choice: unsupported",
			"preserve_reasoning_content: true",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lost DeepSeek template fragment %q", path, want)
			}
		}
	}
}

func TestDeepSeekPresetIsNotAGenericDefault(t *testing.T) {
	setRequiredEnv(t)
	example := loadConfig(t, "../../config/server.example.yaml")
	for _, name := range []string{"deepseek-v4-pro", "deepseek-flash"} {
		if _, ok := example.Providers[name]; ok {
			t.Errorf("server.example.yaml enables providers.%s by default", name)
		}
	}
	if provider := example.Profiles["default"].Provider; provider != "" {
		t.Errorf("server.example.yaml default provider = %q, want unset", provider)
	}

	setDockerConfigEnv(t)
	docker := loadConfig(t, dockerConfigPath)
	for _, name := range []string{"deepseek-v4-pro", "deepseek-flash"} {
		if _, ok := docker.Providers[name]; ok {
			t.Errorf("server.docker.yaml enables providers.%s by default", name)
		}
	}
	if provider := docker.Profiles["default"].Provider; provider != "primary" {
		t.Errorf("server.docker.yaml default provider = %q, want primary", provider)
	}
}
