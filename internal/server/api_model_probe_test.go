package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
)

func newToolProbeEnv(t *testing.T, body string) (*m6Env, *[]map[string]any) {
	t.Helper()
	t.Setenv("PROBE_ENDPOINT_KEY", "endpoint-secret")
	requests := &[]map[string]any{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		*requests = append(*requests, request)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)
	catalog, err := llm.NewCatalog(context.Background(), map[string]config.ProviderConfig{
		"primary": {
			Protocol: "openai-chat", BaseURL: upstream.URL + "/v1", Model: "fake-model",
			APIKeyEnv: "PROBE_ENDPOINT_KEY", MaxTokens: 2048, MaxConcurrency: 1,
			ForcedToolChoice: "supported",
		},
	}, nil)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	env := newM6Env(t, m6Options{models: catalog})
	return env, requests
}

const endpointToolCallBody = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"fake-model","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"submit_result","arguments":"{\"summary\":\"ok\"}"}}]}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`

const endpointTextBody = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"fake-model","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func TestModelToolProbeEndpoint(t *testing.T) {
	env, requests := newToolProbeEnv(t, endpointToolCallBody)
	admin := env.loginAs("code-admin")

	response := admin.request(http.MethodPost, "/api/v1/models/config:primary/probe", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)
	if body["ok"] != true || body["tool_call"] != true || body["probe"] != "tool_call" {
		t.Fatalf("probe response = %v", body)
	}
	if len(*requests) != 1 {
		t.Fatalf("upstream requests = %d", len(*requests))
	}
	upstream := (*requests)[0]
	if upstream["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v", upstream["tool_choice"])
	}
	if tokens, ok := upstream["max_tokens"].(float64); !ok || tokens < 64 {
		t.Fatalf("max_tokens = %v", upstream["max_tokens"])
	}

	viewer := env.loginAs("code-viewer")
	assertStatus(t, viewer.request(http.MethodPost, "/api/v1/models/config:primary/probe", nil, nil), http.StatusForbidden)
}

func TestModelToolProbeEndpointRejectsTextOnlyModels(t *testing.T) {
	env, _ := newToolProbeEnv(t, endpointTextBody)
	admin := env.loginAs("code-admin")
	response := admin.request(http.MethodPost, "/api/v1/models/config:primary/probe", nil, nil)
	assertStatus(t, response, http.StatusUnprocessableEntity)
	body := env.decode(t, response)
	if body["error"].(map[string]any)["code"] != "tool_call_unsupported" {
		t.Fatalf("error = %v", body["error"])
	}

	missing := admin.request(http.MethodPost, "/api/v1/models/config:missing/probe", nil, nil)
	assertStatus(t, missing, http.StatusNotFound)
}
