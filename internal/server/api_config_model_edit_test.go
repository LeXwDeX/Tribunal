package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
)

// configProviderView finds one provider in a decoded GET /api/v1/config body.
func configProviderView(t *testing.T, body map[string]any, name string) map[string]any {
	t.Helper()
	configView, ok := body["config"].(map[string]any)
	if !ok {
		t.Fatalf("config view is missing: %v", body)
	}
	providers, ok := configView["providers"].([]any)
	if !ok {
		t.Fatalf("providers are missing: %v", configView)
	}
	for _, item := range providers {
		provider, ok := item.(map[string]any)
		if ok && provider["name"] == name {
			return provider
		}
	}
	t.Fatalf("provider %q not found in %v", name, providers)
	return nil
}

// TestConfigModelOverlayEditRoundTrip edits every non-secret provider field
// through the console write API and verifies the file-effective view and its
// origins after the watcher would reload the snapshot (#412).
func TestConfigModelOverlayEditRoundTrip(t *testing.T) {
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlayAPIDocument)
	admin := harness.env.loginAs("code-admin")
	response := harness.patch(admin, harness.requestBody(
		setChange([]string{"providers", "primary", "protocol"}, "openai-chat"),
		setChange([]string{"providers", "primary", "base_url"}, "https://api.example.test/v1"),
		setChange([]string{"providers", "primary", "model"}, "claude-opus"),
		setChange([]string{"providers", "primary", "max_tokens"}, 8192),
		setChange([]string{"providers", "primary", "context_window"}, 200000),
		setChange([]string{"providers", "primary", "temperature"}, 0.4),
		setChange([]string{"providers", "primary", "max_concurrency"}, 8),
		setChange([]string{"providers", "primary", "rpm"}, 120),
		setChange([]string{"providers", "primary", "forced_tool_choice"}, "supported"),
		setChange([]string{"providers", "primary", "strict_role_alternation"}, true),
		setChange([]string{"providers", "primary", "reasoning", "effort"}, "high"),
		setChange([]string{"providers", "primary", "thinking", "type"}, "enabled"),
		setChange([]string{"providers", "primary", "thinking", "budget_tokens"}, 2048),
		setChange([]string{"providers", "primary", "preserve_reasoning_content"}, true),
		setChange([]string{"providers", "primary", "allow_reasoning_effort_none"}, true),
		setChange([]string{"providers", "primary", "output_config_effort"}, false),
		setChange([]string{"providers", "primary", "responses_params"}, "supported"),
	))
	assertStatus(t, response, http.StatusOK)
	write := harness.env.decode(t, response)
	if write["applied"] != true || write["changed"] != true || write["restart_required"] != true {
		t.Fatalf("applied/changed/restart_required = %v/%v/%v", write["applied"], write["changed"], write["restart_required"])
	}
	if strings.Contains(string(response.body), "PRIMARY_KEY") || strings.Contains(string(response.body), "primary-key-value") {
		t.Fatalf("write response leaked the credential environment name or value: %s", response.body)
	}
	// The watcher would publish the reloaded snapshot; apply it directly so the
	// read API reports the file-effective values without waiting for a poll.
	snapshot, err := config.LoadSnapshot([]string{harness.base}, harness.overlay)
	if err != nil {
		t.Fatalf("LoadSnapshot() error: %v", err)
	}
	harness.env.api.SetConfigState(&config.State{Snapshot: snapshot, UpdatedAt: time.Now()})

	get := admin.request(http.MethodGet, "/api/v1/config", nil, nil)
	assertStatus(t, get, http.StatusOK)
	read := harness.env.decode(t, get)
	primary := configProviderView(t, read, "primary")
	checks := map[string]any{
		"protocol":                    "openai-chat",
		"base_url":                    "https://api.example.test/v1",
		"model":                       "claude-opus",
		"max_tokens":                  float64(8192),
		"context_window":              float64(200000),
		"temperature":                 0.4,
		"max_concurrency":             float64(8),
		"rpm":                         float64(120),
		"forced_tool_choice":          "supported",
		"strict_role_alternation":     true,
		"preserve_reasoning_content":  true,
		"allow_reasoning_effort_none": true,
		"responses_params":            "supported",
	}
	for field, want := range checks {
		if got := primary[field]; got != want {
			t.Errorf("providers.primary.%s = %v, want %v", field, got, want)
		}
	}
	reasoning, _ := primary["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Errorf("reasoning.effort = %v, want high", reasoning)
	}
	thinking, _ := primary["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(2048) {
		t.Errorf("thinking = %v, want enabled/2048", thinking)
	}

	origins, _ := read["origins"].(map[string]any)
	for _, path := range []string{
		"providers.primary.protocol", "providers.primary.base_url", "providers.primary.model",
		"providers.primary.max_tokens", "providers.primary.context_window", "providers.primary.temperature",
		"providers.primary.max_concurrency", "providers.primary.rpm", "providers.primary.forced_tool_choice",
		"providers.primary.strict_role_alternation", "providers.primary.reasoning.effort",
		"providers.primary.thinking.type", "providers.primary.thinking.budget_tokens",
		"providers.primary.preserve_reasoning_content", "providers.primary.allow_reasoning_effort_none",
		"providers.primary.output_config_effort", "providers.primary.responses_params",
	} {
		if origin := origins[path]; origin != string(config.OriginOverlay) {
			t.Errorf("origin[%s] = %v, want overlay", path, origin)
		}
	}
	if _, ok := origins["providers.primary.api_key_env"]; ok {
		t.Fatalf("origins exposed api_key_env: %v", origins)
	}
	if strings.Contains(string(get.body), "PRIMARY_KEY") || strings.Contains(string(get.body), "primary-key-value") {
		t.Fatalf("config response leaked the credential environment name or value: %s", get.body)
	}
}

// TestConfigModelOverlayEditRejectsCredentialWrites keeps credentials and
// MCP env/headers out of the writable allowlist (#412).
func TestConfigModelOverlayEditRejectsCredentialWrites(t *testing.T) {
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlayAPIDocument)
	before := harness.readOverlay()
	admin := harness.env.loginAs("code-admin")
	cases := []struct {
		name   string
		change map[string]any
	}{
		{"api_key_env value", setChange([]string{"providers", "primary", "api_key_env"}, "sk-live-credential")},
		{"api_key value", setChange([]string{"providers", "primary", "api_key"}, "sk-live-credential")},
		{"mcp config", setChange([]string{"mcp", "config"}, map[string]any{"env": map[string]any{"TOKEN": "sk-live-credential"}})},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response := harness.patch(admin, harness.requestBody(test.change))
			assertStatus(t, response, http.StatusBadRequest)
			if code := errorCode(t, harness.env.decode(t, response)); code != config.OverlayErrorInvalidChange {
				t.Fatalf("error code = %q, want %q", code, config.OverlayErrorInvalidChange)
			}
			if strings.Contains(string(response.body), "sk-live-credential") {
				t.Fatalf("error response echoed the credential value: %s", response.body)
			}
			if harness.readOverlay() != before {
				t.Fatal("rejected credential write modified the overlay")
			}
		})
	}
}

// TestConfigModelOverlayEditListsConfigProfileReferences returns 409 with the
// configuration profile that still names the provider (#412).
func TestConfigModelOverlayEditListsConfigProfileReferences(t *testing.T) {
	const overlay = `
providers:
  referenced-only:
    protocol: openai-chat
    model: gpt-test
    api_key_env: SECONDARY_KEY
profiles:
  default:
    lanes:
      review:
        provider: referenced-only
`
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlay)
	before := harness.readOverlay()
	admin := harness.env.loginAs("code-admin")
	response := harness.patch(admin, harness.requestBody(
		deleteChange([]string{"providers", "referenced-only"}),
	))
	assertStatus(t, response, http.StatusConflict)
	body := harness.env.decode(t, response)
	if code := errorCode(t, body); code != config.OverlayErrorProviderInUse {
		t.Fatalf("error code = %q, want %q", code, config.OverlayErrorProviderInUse)
	}
	details, _ := body["error"].(map[string]any)["details"].([]any)
	joined := ""
	for _, detail := range details {
		joined += detail.(map[string]any)["message"].(string) + "\n"
	}
	if !strings.Contains(joined, "profiles.default.lanes.review.provider refers to providers.referenced-only") {
		t.Fatalf("details do not list the configuration profile referrer: %v", details)
	}
	if harness.readOverlay() != before {
		t.Fatal("referenced provider delete modified the overlay")
	}
}

// TestConfigModelOverlayEditAggregatesConfigAndStoredReferences returns both
// referrer kinds in one 409, so a reset does not need a second attempt (#441).
func TestConfigModelOverlayEditAggregatesConfigAndStoredReferences(t *testing.T) {
	const overlay = `
providers:
  referenced-only:
    protocol: openai-chat
    model: gpt-test
    api_key_env: SECONDARY_KEY
profiles:
  default:
    lanes:
      review:
        provider: referenced-only
`
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlay)
	ctx := context.Background()
	const overrides = `{"lanes":{"review":{"model":"config:referenced-only"}}}`
	if _, err := harness.env.sqlite.PutProfileOverrides(ctx, "default", overrides, 0, 7); err != nil {
		t.Fatalf("PutProfileOverrides: %v", err)
	}
	before := harness.readOverlay()
	admin := harness.env.loginAs("code-admin")
	response := harness.patch(admin, harness.requestBody(
		deleteChange([]string{"providers", "referenced-only"}),
	))
	assertStatus(t, response, http.StatusConflict)
	body := harness.env.decode(t, response)
	if code := errorCode(t, body); code != config.OverlayErrorProviderInUse {
		t.Fatalf("error code = %q, want %q", code, config.OverlayErrorProviderInUse)
	}
	details, _ := body["error"].(map[string]any)["details"].([]any)
	joined := ""
	for _, detail := range details {
		joined += detail.(map[string]any)["message"].(string) + "\n"
	}
	for _, want := range []string{
		"profiles.default.lanes.review.provider refers to providers.referenced-only",
		"overrides.lanes.review.model refers to providers.referenced-only",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("details lack %q:\n%s", want, joined)
		}
	}
	if harness.readOverlay() != before {
		t.Fatal("referenced provider delete modified the overlay")
	}
	stored, err := harness.env.sqlite.GetProfileOverrides(ctx, "default")
	if err != nil || stored.OverridesJSON != overrides {
		t.Fatalf("stored overrides changed: %+v, %v", stored, err)
	}
}
