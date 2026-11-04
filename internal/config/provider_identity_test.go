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
	raw, err = json.Marshal(ProviderConfig{ContextWindow: 128000})
	if err != nil || !strings.Contains(string(raw), `"context_window":128000`) {
		t.Fatalf("configured context window must be part of the identity: %s err=%v", raw, err)
	}
}
