package server

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func detailFields(body map[string]any) []string {
	envelope, _ := body["error"].(map[string]any)
	raw, _ := envelope["details"].([]any)
	fields := make([]string, 0, len(raw))
	for _, item := range raw {
		if detail, ok := item.(map[string]any); ok {
			if field, ok := detail["field"].(string); ok {
				fields = append(fields, field)
			}
		}
	}
	return fields
}

func containsField(fields []string, want string) bool {
	for _, field := range fields {
		if field == want {
			return true
		}
	}
	return false
}

// TestModelDefaultsRevalidateStoredProfileOverrides keeps a default reset from
// leaving a stored effort:none override that fails at review time.
func TestModelDefaultsRevalidateStoredProfileOverrides(t *testing.T) {
	t.Setenv("OVERRIDE_DEFAULTS_KEY", "key")
	platform := &config.Config{}
	platform.Storage.LLMCallRetention = 720 * time.Hour
	env := newM6Env(t, m6Options{profileOverrides: true, platform: platform})
	catalog, err := llm.NewCatalog(context.Background(),
		map[string]config.ProviderConfig{
			"base": {Protocol: "openai-chat", BaseURL: "https://93.184.216.34/v1", Model: "m",
				APIKeyEnv: "OVERRIDE_DEFAULTS_KEY", MaxTokens: 4096, MaxConcurrency: 1},
		}, env.sqlite)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	wireModelDefaults(t, env, catalog)
	ctx := context.Background()
	model, err := env.sqlite.CreateModel(ctx, store.Model{
		DisplayName: "reviewer", Protocol: "openai-chat", BaseURL: "https://93.184.216.34/v1",
		Model: "reviewer-model", Params: store.ModelParams{MaxTokens: 4096, MaxConcurrency: 1}, CreatedBy: 7,
	}, "")
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if err := catalog.Refresh(ctx); err != nil {
		t.Fatalf("catalog refresh: %v", err)
	}
	admin := env.loginAs("code-admin")

	// The defaults grant the capability the override needs, so the override
	// saves.
	response := admin.json(http.MethodPut, "/api/v1/model-defaults", map[string]any{
		"revision": 0,
		"params": map[string]any{
			"allow_reasoning_effort_none": true,
			"max_tokens":                  4096,
		},
	})
	assertStatus(t, response, http.StatusOK)
	revision := int64(env.decode(t, response)["revision"].(float64))

	overrides := map[string]any{
		"overrides": map[string]any{
			"lanes": map[string]any{
				"review": map[string]any{
					"model":  model.ID,
					"params": map[string]any{"reasoning": map[string]any{"effort": "none"}},
				},
			},
		},
		"revision": 0,
	}
	response = admin.json(http.MethodPut, "/api/v1/profiles/default/overrides", overrides)
	assertStatus(t, response, http.StatusOK)

	// Removing the capability must fail before writing and name the node.
	response = admin.json(http.MethodPut, "/api/v1/model-defaults", map[string]any{
		"revision": revision,
		"params":   map[string]any{"max_tokens": 4096},
	})
	assertStatus(t, response, http.StatusBadRequest)
	body := env.decode(t, response)
	if !containsField(detailFields(body), "overrides.lanes.review.params.reasoning.effort") {
		t.Fatalf("details = %v, want the affected override node", detailFields(body))
	}

	// The reset path reports the same referrers and requires the explicit force.
	response = admin.json(http.MethodDelete,
		"/api/v1/model-defaults?revision="+strconv.FormatInt(revision, 10)+"&force=false", nil)
	assertStatus(t, response, http.StatusConflict)
	body = env.decode(t, response)
	if body["error"].(map[string]any)["code"] != "overrides_in_use" {
		t.Fatalf("reset error = %v", body["error"])
	}
	if !containsField(detailFields(body), "overrides.lanes.review.params.reasoning.effort") {
		t.Fatalf("reset details = %v", detailFields(body))
	}
	response = admin.json(http.MethodDelete,
		"/api/v1/model-defaults?revision="+strconv.FormatInt(revision, 10)+"&force=true", nil)
	assertStatus(t, response, http.StatusOK)
}
