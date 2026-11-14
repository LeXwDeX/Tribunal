package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
)

type probeRequest struct {
	MaxTokens  int    `json:"max_tokens"`
	ToolChoice string `json:"tool_choice"`
	Tools      []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func newProbeRegistry(t *testing.T, body string) (llm.Client, *[]probeRequest) {
	t.Helper()
	t.Setenv("PROBE_TOOL_KEY", "probe-secret")
	requests := &[]probeRequest{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var request probeRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		*requests = append(*requests, request)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)
	registry, err := llm.NewRegistry(map[string]config.ProviderConfig{
		"fake": {
			Protocol: "openai-chat", BaseURL: upstream.URL + "/v1", Model: "fake-model",
			APIKeyEnv: "PROBE_TOOL_KEY", MaxConcurrency: 1, ForcedToolChoice: "supported",
		},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	client, err := registry.Client("fake")
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	return client, requests
}

const probeToolCallBody = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"fake-model","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"submit_result","arguments":"{\"summary\":\"ok\"}"}}]}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`

const probeTextBody = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"fake-model","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func TestProbeProviderToolsRequiresARealToolCall(t *testing.T) {
	client, requests := newProbeRegistry(t, probeToolCallBody)
	result, err := ProbeProviderTools(context.Background(), client)
	if err != nil {
		t.Fatalf("ProbeProviderTools: %v", err)
	}
	if !result.ToolCall || result.Probe != ProbeToolCall || result.Model != "fake-model" {
		t.Fatalf("result = %+v", result)
	}
	if len(*requests) != 1 {
		t.Fatalf("requests = %d", len(*requests))
	}
	request := (*requests)[0]
	if request.ToolChoice != "required" {
		t.Fatalf("tool_choice = %q, want required for a provider that supports it", request.ToolChoice)
	}
	if request.MaxTokens < 64 {
		t.Fatalf("max_tokens = %d, want a usable budget instead of one token", request.MaxTokens)
	}
	if len(request.Tools) != 1 || request.Tools[0].Function.Name != "submit_result" {
		t.Fatalf("tools = %+v", request.Tools)
	}
}

func TestProbeProviderToolsRejectsATextOnlyAnswer(t *testing.T) {
	client, _ := newProbeRegistry(t, probeTextBody)
	_, err := ProbeProviderTools(context.Background(), client)
	if !errors.Is(err, ErrToolCallUnsupported) {
		t.Fatalf("err = %v, want ErrToolCallUnsupported", err)
	}
}

func TestProbeProviderConnectivityUsesAUsableTokenBudget(t *testing.T) {
	client, requests := newProbeRegistry(t, probeTextBody)
	result, err := ProbeProvider(context.Background(), client)
	if err != nil {
		t.Fatalf("ProbeProvider: %v", err)
	}
	if result.Probe != ProbeConnectivity {
		t.Fatalf("probe = %q", result.Probe)
	}
	if len(*requests) != 1 || (*requests)[0].MaxTokens < 8 {
		t.Fatalf("connectivity max_tokens = %+v", *requests)
	}
	if len((*requests)[0].Tools) != 0 {
		t.Fatal("connectivity probe must not send tools")
	}
}
