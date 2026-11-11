package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// Provider configs are serialized into review execution identities
// (internal/app). A field added later must be omitted when unset, or every
// stored workflow's identity changes on upgrade and in-flight reviews are
// cancelled.
func TestProviderConfigJSONOmitsUnsetContextWindow(t *testing.T) {
	raw, err := json.Marshal(ProviderConfig{Protocol: "openai-chat", Model: "m", MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"protocol":"openai-chat","base_url":"","model":"m","api_key_env":"","max_concurrency":1,"rpm":0,"max_tokens":0,"temperature":null,"thinking":null,"reasoning":null,"price":null}`
	if string(raw) != want {
		t.Fatalf("provider identity JSON changed:\n got %s\nwant %s", raw, want)
	}
	raw, err = json.Marshal(ProviderConfig{ForcedToolChoice: "supported"})
	if err != nil || !strings.Contains(string(raw), `"forced_tool_choice":"supported"`) {
		t.Fatalf("configured forced tool choice must be part of the identity: %s err=%v", raw, err)
	}
	raw, err = json.Marshal(ProviderConfig{ContextWindow: 128000})
	if err != nil || !strings.Contains(string(raw), `"context_window":128000`) {
		t.Fatalf("configured context window must be part of the identity: %s err=%v", raw, err)
	}
}

// Legacy YAML files still carry providers.*.price. v1.4.0 keeps parsing the
// field so those files load and the execution identity stays byte-identical;
// it is no longer used for any calculation.
func TestLoadKeepsLegacyProviderPrice(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("TEST_LEGACY_PRICE_KEY", "key")
	path := writeConfig(t, "legacy-price.yaml", validGitLab+`
providers:
  legacy:
    protocol: openai-chat
    base_url: "https://example.test/v1"
    model: "m"
    api_key_env: TEST_LEGACY_PRICE_KEY
    price: { input_per_mtok: 1.5, output_per_mtok: 2.5 }
`)
	cfg := loadConfig(t, path)
	provider, ok := cfg.Providers["legacy"]
	if !ok || provider.Price == nil || provider.Price.InputPerMTok != 1.5 || provider.Price.OutputPerMTok != 2.5 {
		t.Fatalf("legacy price was not parsed: %+v", provider.Price)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("legacy price config must still start: %v", err)
	}
	raw, err := json.Marshal(provider)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"price":{"input_per_mtok":1.5,"output_per_mtok":2.5}`) {
		t.Fatalf("legacy price must stay in the execution identity JSON: %s", raw)
	}
}
