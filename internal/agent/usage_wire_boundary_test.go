package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
)

func TestUsageWireBoundaryAgentRetainsKnownUsage(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":-0.5,"total_tokens":5}}`))
	}))
	defer server.Close()
	t.Setenv("AGENT_USAGE_WIRE_KEY", "key")
	registry, err := llm.NewRegistry(map[string]config.ProviderConfig{
		"chat": {
			Protocol:  "openai-chat",
			BaseURL:   server.URL,
			Model:     "test",
			APIKeyEnv: "AGENT_USAGE_WIRE_KEY",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := registry.Client("chat")
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), testSpec(client))
	if !errors.Is(err, llm.ErrUnsafeProviderResponse) || result == nil || result.Usage != (llm.Usage{InputTokens: 5, Incomplete: true}) || calls.Load() != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls.Load())
	}
}
