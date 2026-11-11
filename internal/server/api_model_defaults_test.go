package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func wireModelDefaults(t *testing.T, env *m6Env, catalog *llm.Catalog) {
	t.Helper()
	env.api.modelDefaults = env.sqlite
	env.api.modelsStore = env.sqlite
	if catalog != nil {
		env.api.models = catalog
		env.api.refreshModels = catalog.Refresh
	}
}

func TestModelDefaultsEndpointsRequireAdminAndCSRF(t *testing.T) {
	env := newM6Env(t, m6Options{})
	wireModelDefaults(t, env, nil)
	viewer := env.loginAs("code-viewer")
	admin := env.loginAs("code-admin")
	adminWithoutCSRF := admin.withoutCSRF()
	body := []byte(`{"params":{"max_tokens":4096},"revision":0}`)
	for _, endpoint := range []struct{ name, method, path string }{
		{"get", http.MethodGet, "/api/v1/model-defaults"},
		{"put", http.MethodPut, "/api/v1/model-defaults"},
		{"delete", http.MethodDelete, "/api/v1/model-defaults?revision=1"},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			if response := viewer.request(endpoint.method, endpoint.path, body, nil); response.status != http.StatusForbidden {
				t.Errorf("non-admin status = %d, want 403", response.status)
			}
			if !safeMethod(endpoint.method) {
				if response := adminWithoutCSRF.request(endpoint.method, endpoint.path, body, nil); response.status != http.StatusForbidden {
					t.Errorf("session without CSRF status = %d, want 403", response.status)
				}
			}
		})
	}
	response := admin.request(http.MethodGet, "/api/v1/model-defaults", nil, nil)
	assertStatus(t, response, http.StatusOK)
	var view struct {
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(response.body, &view); err != nil || view.Revision != 0 {
		t.Fatalf("initial view = %s (%v)", response.body, err)
	}
}

func TestModelDefaultsPutValidationRevisionAndReset(t *testing.T) {
	env := newM6Env(t, m6Options{})
	wireModelDefaults(t, env, nil)
	admin := env.loginAs("code-admin")

	for _, invalid := range []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"unknown parameter", map[string]any{"params": map[string]any{"foo": 1}, "revision": 0}, "params.foo"},
		{"price is removed", map[string]any{"params": map[string]any{"price": map[string]any{"input_per_mtok": 1}}, "revision": 0}, "params.price"},
		{"negative max_tokens", map[string]any{"params": map[string]any{"max_tokens": -1}, "revision": 0}, "params.max_tokens"},
		{"temperature out of range", map[string]any{"params": map[string]any{"temperature": 3}, "revision": 0}, "params.temperature"},
		{"invalid effort", map[string]any{"params": map[string]any{"reasoning": map[string]any{"effort": "bogus"}}, "revision": 0}, "params.reasoning.effort"},
		{"unknown top-level field", map[string]any{"params": map[string]any{}, "revision": 0, "extra": true}, "extra"},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			response := admin.json(http.MethodPut, "/api/v1/model-defaults", invalid.body)
			assertStatus(t, response, http.StatusBadRequest)
			if !strings.Contains(string(response.body), invalid.field) {
				t.Fatalf("400 lacks field %q: %s", invalid.field, response.body)
			}
		})
	}
	if response := admin.json(http.MethodPut, "/api/v1/model-defaults", map[string]any{"revision": 0}); response.status != http.StatusBadRequest ||
		!strings.Contains(string(response.body), `"field":"params"`) {
		t.Fatalf("missing params = %d %s", response.status, response.body)
	}
	// A draft may not submit null for a non-nullable parameter.
	if response := admin.json(http.MethodPut, "/api/v1/model-defaults", map[string]any{"params": map[string]any{"max_tokens": nil}, "revision": 0}); response.status != http.StatusBadRequest {
		t.Fatalf("null max_tokens = %d %s", response.status, response.body)
	}

	response := admin.json(http.MethodPut, "/api/v1/model-defaults", map[string]any{
		"params": map[string]any{
			"max_tokens": 4096, "context_window": 128000, "temperature": 0.3,
			"reasoning": map[string]any{"effort": "high"}, "strict_role_alternation": true, "rpm": 60,
		},
		"revision": 0,
	})
	assertStatus(t, response, http.StatusOK)
	var saved modelDefaultsView
	if err := json.Unmarshal(response.body, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 || saved.Params.MaxTokens != 4096 || saved.Params.ContextWindow != 128000 ||
		saved.Params.Temperature == nil || *saved.Params.Temperature != 0.3 || !saved.Params.StrictRoleAlternation ||
		saved.Params.Reasoning == nil || saved.Params.Reasoning.Effort != "high" || saved.Params.RPM != 60 {
		t.Fatalf("saved defaults = %+v", saved)
	}
	if strings.Contains(string(response.body), "price") || strings.Contains(string(response.body), "cost") {
		t.Fatalf("defaults response leaks removed cost fields: %s", response.body)
	}
	// A stale writer must not overwrite another administrator's document.
	stale := admin.json(http.MethodPut, "/api/v1/model-defaults", map[string]any{"params": map[string]any{"max_tokens": 1}, "revision": 0})
	assertStatus(t, stale, http.StatusConflict)
	if !strings.Contains(string(stale.body), "stale_revision") {
		t.Fatalf("stale put = %d %s", stale.status, stale.body)
	}

	// Revision 1 replaces the whole document.
	updated := admin.json(http.MethodPut, "/api/v1/model-defaults", map[string]any{"params": map[string]any{"rpm": 30}, "revision": 1})
	assertStatus(t, updated, http.StatusOK)
	var replaced modelDefaultsView
	if err := json.Unmarshal(updated.body, &replaced); err != nil {
		t.Fatal(err)
	}
	if replaced.Revision != 2 || replaced.Params.RPM != 30 || replaced.Params.MaxTokens != 0 || replaced.Params.Temperature != nil {
		t.Fatalf("replaced defaults = %+v", replaced)
	}

	get := admin.request(http.MethodGet, "/api/v1/model-defaults", nil, nil)
	assertStatus(t, get, http.StatusOK)
	var read modelDefaultsView
	if err := json.Unmarshal(get.body, &read); err != nil || read.Revision != 2 || read.Params.RPM != 30 {
		t.Fatalf("get defaults = %s (%v)", get.body, err)
	}

	if response := admin.request(http.MethodDelete, "/api/v1/model-defaults?revision=1", nil, nil); response.status != http.StatusConflict {
		t.Fatalf("stale delete = %d %s", response.status, response.body)
	}
	reset := admin.request(http.MethodDelete, "/api/v1/model-defaults?revision=2", nil, nil)
	assertStatus(t, reset, http.StatusOK)
	var cleared modelDefaultsView
	if err := json.Unmarshal(reset.body, &cleared); err != nil {
		t.Fatal(err)
	}
	if cleared.Revision != 3 || cleared.Params != (store.ModelParams{}) {
		t.Fatalf("reset defaults = %+v", cleared)
	}
	if response := admin.request(http.MethodDelete, "/api/v1/model-defaults", nil, nil); response.status != http.StatusBadRequest {
		t.Fatalf("delete without revision = %d %s", response.status, response.body)
	}
}

// The model view reports explicit and effective parameters with their source,
// and config models are never affected by the defaults.
func TestModelDefaultsAppearInModelViews(t *testing.T) {
	t.Setenv("MODEL_DEFAULTS_CONFIG_KEY", "key")
	env := newM6Env(t, m6Options{})
	catalog, err := llm.NewCatalog(context.Background(), map[string]config.ProviderConfig{
		"base": {Protocol: "openai-chat", BaseURL: "https://93.184.216.34/v1", Model: "m", APIKeyEnv: "MODEL_DEFAULTS_CONFIG_KEY", MaxTokens: 777, MaxConcurrency: 1},
	}, env.sqlite)
	if err != nil {
		t.Fatal(err)
	}
	wireModelDefaults(t, env, catalog)
	admin := env.loginAs("code-admin")

	assertStatus(t, admin.json(http.MethodPut, "/api/v1/model-defaults", map[string]any{
		"params": map[string]any{"max_tokens": 4096, "temperature": 0.3, "reasoning": map[string]any{"effort": "high"}}, "revision": 0,
	}), http.StatusOK)

	ctx := context.Background()
	inheriting, err := env.sqlite.CreateModel(ctx, store.Model{
		DisplayName: "inheriting", Protocol: "anthropic", BaseURL: "https://93.184.216.34/v1", Model: "claude-x",
		Params: store.ModelParams{MaxConcurrency: 1}, CreatedBy: 7,
	}, "")
	if err != nil {
		t.Fatalf("create model relying on defaults: %v", err)
	}
	temperature := 0.1
	explicit, err := env.sqlite.CreateModel(ctx, store.Model{
		DisplayName: "explicit", Protocol: "openai-chat", BaseURL: "https://93.184.216.34/v1", Model: "m",
		Params: store.ModelParams{MaxTokens: 1000, Temperature: &temperature, MaxConcurrency: 1}, CreatedBy: 7,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	response := admin.request(http.MethodGet, "/api/v1/models", nil, nil)
	assertStatus(t, response, http.StatusOK)
	var list struct {
		Models []struct {
			ID              string            `json:"id"`
			Source          string            `json:"source"`
			Params          store.ModelParams `json:"params"`
			EffectiveParams store.ModelParams `json:"effective_params"`
			ParamSources    map[string]string `json:"param_sources"`
		} `json:"models"`
	}
	if err := json.Unmarshal(response.body, &list); err != nil {
		t.Fatal(err)
	}
	byID := map[string]struct {
		Source          string
		Params          store.ModelParams
		EffectiveParams store.ModelParams
		ParamSources    map[string]string
	}{}
	for _, model := range list.Models {
		byID[model.ID] = struct {
			Source          string
			Params          store.ModelParams
			EffectiveParams store.ModelParams
			ParamSources    map[string]string
		}{model.Source, model.Params, model.EffectiveParams, model.ParamSources}
	}
	inherited, ok := byID[inheriting.ID]
	if !ok {
		t.Fatalf("inheriting model missing from %s", response.body)
	}
	if inherited.Params.MaxTokens != 0 {
		t.Fatalf("stored params = %+v, want explicit only", inherited.Params)
	}
	if inherited.EffectiveParams.MaxTokens != 4096 || inherited.EffectiveParams.Temperature == nil ||
		*inherited.EffectiveParams.Temperature != 0.3 || inherited.EffectiveParams.Reasoning == nil ||
		inherited.EffectiveParams.Reasoning.Effort != "high" {
		t.Fatalf("effective params = %+v", inherited.EffectiveParams)
	}
	if inherited.ParamSources["max_tokens"] != store.ParamSourceDefault ||
		inherited.ParamSources["temperature"] != store.ParamSourceDefault ||
		inherited.ParamSources["reasoning"] != store.ParamSourceDefault {
		t.Fatalf("param sources = %+v", inherited.ParamSources)
	}
	explicitView, ok := byID[explicit.ID]
	if !ok {
		t.Fatal("explicit model missing")
	}
	if explicitView.EffectiveParams.MaxTokens != 1000 || explicitView.ParamSources["max_tokens"] != store.ParamSourceModel ||
		explicitView.EffectiveParams.Temperature == nil || *explicitView.EffectiveParams.Temperature != 0.1 ||
		explicitView.ParamSources["temperature"] != store.ParamSourceModel {
		t.Fatalf("explicit model view = %+v (%v)", explicitView.EffectiveParams, explicitView.ParamSources)
	}
	configured, ok := byID["config:base"]
	if !ok {
		t.Fatal("config model missing")
	}
	if configured.Source != "config" || configured.EffectiveParams.MaxTokens != 777 || configured.EffectiveParams.RPM != 0 ||
		len(configured.ParamSources) != 0 {
		t.Fatalf("config model view = %+v (%v)", configured.EffectiveParams, configured.ParamSources)
	}

	// Updating the defaults refreshes the catalog, so the next read reports
	// the new effective values without touching the model revision.
	assertStatus(t, admin.json(http.MethodPut, "/api/v1/model-defaults", map[string]any{
		"params": map[string]any{"max_tokens": 8192}, "revision": 1,
	}), http.StatusOK)
	single := admin.request(http.MethodGet, "/api/v1/models/"+inheriting.ID, nil, nil)
	assertStatus(t, single, http.StatusOK)
	var detail struct {
		Params          store.ModelParams `json:"params"`
		EffectiveParams store.ModelParams `json:"effective_params"`
	}
	if err := json.Unmarshal(single.body, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Params.MaxTokens != 0 || detail.EffectiveParams.MaxTokens != 8192 {
		t.Fatalf("detail after defaults update = %+v", detail)
	}
}

// Create and draft-test validation use the merged parameters, so a model can
// rely on the platform defaults for a protocol-required value.
func TestModelCreateValidationUsesDefaults(t *testing.T) {
	t.Setenv("MODEL_DEFAULTS_CONFIG_KEY", "key")
	env := newM6Env(t, m6Options{})
	catalog, err := llm.NewCatalog(context.Background(), nil, env.sqlite)
	if err != nil {
		t.Fatal(err)
	}
	wireModelDefaults(t, env, catalog)
	admin := env.loginAs("code-admin")

	body := map[string]any{
		"display_name": "claude", "protocol": "anthropic", "base_url": "https://93.184.216.34/v1", "model": "claude-x",
		"params": map[string]any{"max_concurrency": 1},
	}
	withoutDefaults := admin.json(http.MethodPost, "/api/v1/models", body)
	assertStatus(t, withoutDefaults, http.StatusBadRequest)
	if !strings.Contains(string(withoutDefaults.body), "params.max_tokens") {
		t.Fatalf("400 lacks params.max_tokens: %s", withoutDefaults.body)
	}
	assertStatus(t, admin.json(http.MethodPut, "/api/v1/model-defaults", map[string]any{
		"params": map[string]any{"max_tokens": 4096}, "revision": 0,
	}), http.StatusOK)
	assertStatus(t, admin.json(http.MethodPost, "/api/v1/models", body), http.StatusOK)

	// The draft test merges the defaults before validation too: an anthropic
	// draft without its own max_tokens passes validation and reaches the
	// egress check, which rejects the link-local probe address.
	draft := admin.json(http.MethodPost, "/api/v1/models:test", map[string]any{
		"display_name": "claude", "protocol": "anthropic", "base_url": "http://169.254.169.254", "model": "claude-x",
		"params": map[string]any{"max_concurrency": 1},
	})
	assertStatus(t, draft, http.StatusUnprocessableEntity)
	if !strings.Contains(string(draft.body), "egress_denied") {
		t.Fatalf("draft did not pass merged validation before egress: %s", draft.body)
	}
}

// Profile override validation must use the model's effective parameters, so a
// capability or max_tokens inherited from the platform defaults constrains
// node overrides exactly like a value stored on the model.
func TestProfileOverridesUseEffectiveModelParams(t *testing.T) {
	env := newM6Env(t, m6Options{profileOverrides: true})
	catalog, err := llm.NewCatalog(context.Background(), nil, env.sqlite)
	if err != nil {
		t.Fatal(err)
	}
	wireModelDefaults(t, env, catalog)
	admin := env.loginAs("code-admin")

	assertStatus(t, admin.json(http.MethodPut, "/api/v1/model-defaults", map[string]any{
		"params": map[string]any{"max_tokens": 1024, "allow_reasoning_effort_none": true}, "revision": 0,
	}), http.StatusOK)
	ctx := context.Background()
	model, err := env.sqlite.CreateModel(ctx, store.Model{
		DisplayName: "claude", Protocol: "anthropic", BaseURL: "https://93.184.216.34/v1", Model: "claude-x",
		Params: store.ModelParams{MaxConcurrency: 1}, CreatedBy: 7,
	}, "")
	if err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := catalog.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	profile := config.ProfileConfig{Provider: model.ID}
	env.api.platform = &config.Config{
		Profiles:    map[string]config.ProfileConfig{"default": profile},
		RawProfiles: map[string]config.ProfileConfig{"default": profile},
	}

	// allow_reasoning_effort_none is inherited, so effort none is valid.
	assertStatus(t, admin.json(http.MethodPut, "/api/v1/profiles/default/overrides", map[string]any{
		"overrides": map[string]any{"default": map[string]any{"params": map[string]any{"reasoning": map[string]any{"effort": "none"}}}},
		"revision":  0,
	}), http.StatusOK)

	// The inherited max_tokens bounds the thinking budget. Without effective
	// params the lookup would see 0 and accept the override.
	response := admin.json(http.MethodPut, "/api/v1/profiles/default/overrides", map[string]any{
		"overrides": map[string]any{"default": map[string]any{"params": map[string]any{"thinking": map[string]any{"budget_tokens": 1500}}}},
		"revision":  1,
	})
	assertStatus(t, response, http.StatusBadRequest)
	if !strings.Contains(string(response.body), "thinking.budget_tokens") {
		t.Fatalf("400 lacks the budget field: %s", response.body)
	}
}

// Defaults are shared across protocols. A value that passes the neutral
// validation can still make an existing model unavailable, so the save is
// rejected with the affected model and field.
func TestModelDefaultsRejectCrossProtocolBreakage(t *testing.T) {
	env := newM6Env(t, m6Options{})
	wireModelDefaults(t, env, nil)
	admin := env.loginAs("code-admin")

	assertStatus(t, admin.json(http.MethodPut, "/api/v1/model-defaults", map[string]any{
		"params": map[string]any{"max_tokens": 4096}, "revision": 0,
	}), http.StatusOK)
	ctx := context.Background()
	model, err := env.sqlite.CreateModel(ctx, store.Model{
		DisplayName: "claude", Protocol: "anthropic", BaseURL: "https://93.184.216.34/v1", Model: "claude-x",
		Params: store.ModelParams{MaxConcurrency: 1}, CreatedBy: 7,
	}, "")
	if err != nil {
		t.Fatalf("create anthropic model: %v", err)
	}

	for _, invalid := range []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"anthropic budget below 1024", map[string]any{
			"params": map[string]any{"max_tokens": 4096, "thinking": map[string]any{"budget_tokens": 512}}, "revision": 1,
		}, "thinking.budget_tokens"},
		{"anthropic effort none", map[string]any{
			"params": map[string]any{"max_tokens": 4096, "reasoning": map[string]any{"effort": "none"}, "allow_reasoning_effort_none": true}, "revision": 1,
		}, "reasoning.effort"},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			response := admin.json(http.MethodPut, "/api/v1/model-defaults", invalid.body)
			assertStatus(t, response, http.StatusBadRequest)
			body := string(response.body)
			if !strings.Contains(body, model.ID) || !strings.Contains(body, invalid.field) {
				t.Fatalf("400 lacks the affected model or field: %s", body)
			}
		})
	}

	// A rejected save does not change the stored defaults or revision.
	get := admin.request(http.MethodGet, "/api/v1/model-defaults", nil, nil)
	assertStatus(t, get, http.StatusOK)
	var stored modelDefaultsView
	if err := json.Unmarshal(get.body, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Revision != 1 || stored.Params.MaxTokens != 4096 || stored.Params.Thinking != nil {
		t.Fatalf("rejected save changed defaults: %+v", stored)
	}
	// A compatible replacement still succeeds.
	assertStatus(t, admin.json(http.MethodPut, "/api/v1/model-defaults", map[string]any{
		"params": map[string]any{"max_tokens": 8192}, "revision": 1,
	}), http.StatusOK)
}
