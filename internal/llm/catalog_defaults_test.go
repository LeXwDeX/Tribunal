package llm

import (
	"context"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestCatalogAppliesDefaultsToUserModelsOnly(t *testing.T) {
	t.Setenv("CATALOG_DEFAULTS_KEY", "key")
	explicitTemperature := 0.1
	defaultTemperature := 0.7
	s := &catalogStore{
		models: []store.Model{
			{ID: "user:inherits", DisplayName: "inherits", Protocol: "openai-chat", BaseURL: "https://93.184.216.34/v1", Model: "m", Revision: 1, Params: store.ModelParams{MaxConcurrency: 2}},
			{ID: "user:explicit", DisplayName: "explicit", Protocol: "openai-chat", BaseURL: "https://93.184.216.34/v1", Model: "m", Revision: 1, Params: store.ModelParams{MaxTokens: 512, Temperature: &explicitTemperature, MaxConcurrency: 1}},
		},
		defaults:         store.ModelParams{MaxTokens: 4096, ContextWindow: 128000, Temperature: &defaultTemperature, RPM: 60},
		defaultsRevision: 1,
	}
	c, err := NewCatalog(context.Background(), map[string]config.ProviderConfig{
		"config-model": {Protocol: "openai-chat", BaseURL: "https://93.184.216.34/v1", Model: "m", APIKeyEnv: "CATALOG_DEFAULTS_KEY", MaxTokens: 1000, MaxConcurrency: 1},
	}, s)
	if err != nil {
		t.Fatal(err)
	}

	inherits, ok := c.Resolve("user:inherits")
	if !ok {
		t.Fatal("user model missing")
	}
	// The stored params stay explicit; the effective params inherit defaults.
	if inherits.Params.MaxTokens != 0 {
		t.Fatalf("stored max_tokens = %d, want unset", inherits.Params.MaxTokens)
	}
	if inherits.EffectiveParams.MaxTokens != 4096 || inherits.EffectiveParams.ContextWindow != 128000 || inherits.EffectiveParams.RPM != 60 {
		t.Fatalf("effective params = %+v", inherits.EffectiveParams)
	}
	if inherits.EffectiveParams.Temperature == nil || *inherits.EffectiveParams.Temperature != 0.7 {
		t.Fatalf("effective temperature = %v", inherits.EffectiveParams.Temperature)
	}
	for _, name := range []string{"max_tokens", "context_window", "temperature", "rpm"} {
		if inherits.ParamSources[name] != store.ParamSourceDefault {
			t.Fatalf("%s source = %q, want default", name, inherits.ParamSources[name])
		}
	}
	if got := inherits.ProviderConfig(); got.MaxTokens != 4096 || got.RPM != 60 || got.Temperature == nil || *got.Temperature != 0.7 {
		t.Fatalf("provider config = %+v", got)
	}

	explicit, _ := c.Resolve("user:explicit")
	if explicit.EffectiveParams.MaxTokens != 512 || explicit.ParamSources["max_tokens"] != store.ParamSourceModel {
		t.Fatalf("explicit max_tokens = %d (%s)", explicit.EffectiveParams.MaxTokens, explicit.ParamSources["max_tokens"])
	}
	if explicit.EffectiveParams.Temperature == nil || *explicit.EffectiveParams.Temperature != 0.1 ||
		explicit.ParamSources["temperature"] != store.ParamSourceModel {
		t.Fatalf("explicit temperature = %v (%s)", explicit.EffectiveParams.Temperature, explicit.ParamSources["temperature"])
	}

	// Config models keep their configured values and never read the defaults.
	configured, _ := c.Resolve("config:config-model")
	if configured.EffectiveParams.MaxTokens != 1000 || configured.EffectiveParams.RPM != 0 ||
		configured.EffectiveParams.ContextWindow != 0 {
		t.Fatalf("config model effective params = %+v", configured.EffectiveParams)
	}
	if len(configured.ParamSources) != 0 {
		t.Fatalf("config model param sources = %v, want none", configured.ParamSources)
	}

	// A defaults change has no model revision to invalidate the cache, so the
	// catalog must rebuild user clients when the defaults revision changes.
	oldClient, err := c.Client("user:inherits")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.defaults.MaxTokens = 8192
	s.defaultsRevision = 2
	s.mu.Unlock()
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	newClient, err := c.Client("user:inherits")
	if err != nil {
		t.Fatal(err)
	}
	if newClient == oldClient {
		t.Fatal("defaults change did not rebuild the user-model client")
	}
	updated, _ := c.Resolve("user:inherits")
	if updated.EffectiveParams.MaxTokens != 8192 {
		t.Fatalf("effective max_tokens after refresh = %d, want 8192", updated.EffectiveParams.MaxTokens)
	}
}

func TestCatalogValidationMergesDefaults(t *testing.T) {
	s := &catalogStore{models: []store.Model{
		{ID: "user:claude", DisplayName: "claude", Protocol: "anthropic", BaseURL: "https://93.184.216.34/v1", Model: "claude-x", Revision: 1, Params: store.ModelParams{MaxConcurrency: 1}},
	}}
	c, err := NewCatalog(context.Background(), nil, s)
	if err != nil {
		t.Fatal(err)
	}
	model, _ := c.Resolve("user:claude")
	if !model.Unavailable || model.UnavailableReason != "validation" {
		t.Fatalf("anthropic model without max_tokens = %+v, want validation failure", model)
	}
	s.mu.Lock()
	s.defaults = store.ModelParams{MaxTokens: 4096}
	s.defaultsRevision = 1
	s.mu.Unlock()
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	model, _ = c.Resolve("user:claude")
	if model.Unavailable {
		t.Fatalf("model relying on the default is unavailable: %+v", model)
	}
	if model.EffectiveParams.MaxTokens != 4096 || model.Params.MaxTokens != 0 {
		t.Fatalf("effective/stored max_tokens = %d/%d", model.EffectiveParams.MaxTokens, model.Params.MaxTokens)
	}
	if _, err := c.Client("user:claude"); err != nil {
		t.Fatalf("client built from defaults: %v", err)
	}
}
