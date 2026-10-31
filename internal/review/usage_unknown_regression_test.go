package review

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// unknownUsageClient returns a safe first response with caller-supplied usage.
// The second response is a valid submit; tests assert that the budget guard
// blocks before it is requested.
type unknownUsageClient struct {
	mu       sync.Mutex
	calls    int
	firstUse llm.Usage
}

func (c *unknownUsageClient) Generate(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()
	if call == 1 {
		return &llm.Response{StopReason: llm.StopEndTurn, Usage: c.firstUse}, nil
	}
	message := llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{
		{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "submit", Name: agent.SubmitToolName, Input: json.RawMessage(`{"overview":"ok"}`)}},
	}}
	return &llm.Response{Message: message, StopReason: llm.StopToolUse, Usage: llm.Usage{InputTokens: 1}}, nil
}

func (c *unknownUsageClient) Info() llm.ProviderInfo { return llm.ProviderInfo{Name: "fake"} }

func (c *unknownUsageClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type staticClients struct{ client llm.Client }

func (s staticClients) Client(string) (llm.Client, error) { return s.client, nil }

func requireUnsafeModelFailure(t *testing.T, err error) {
	t.Helper()
	var classified interface{ ErrorClass() string }
	if err == nil || !errors.As(err, &classified) || classified.ErrorClass() != "unsafe_model_response" {
		t.Fatalf("error=%v want unsafe_model_response", err)
	}
}

func TestBudgetAttemptIncompleteCustomClientStopsBeforeNextRequest(t *testing.T) {
	for _, usage := range []llm.Usage{
		{Incomplete: true},
		{InputTokens: 5, Incomplete: true},
	} {
		client := &unknownUsageClient{firstUse: usage}
		executor := newTestExecutor(t, ExecutorDeps{Profile: onboardingProfile(), Clients: staticClients{client}})
		out, err := executor.Execute(context.Background(), dag.NodeInput{
			WorkflowID: "wf", Node: dag.NodeSpec{ID: "onboarding", Kind: KindOnboarding, Params: json.RawMessage(`{"provider":"fake"}`)},
		})
		if client.count() != 1 || !out.UsageIncomplete || out.Usage.InputTokens != usage.InputTokens || out.Usage.OutputTokens != usage.OutputTokens {
			t.Fatalf("usage=%+v calls=%d out=%+v err=%v", usage, client.count(), out, err)
		}
		requireUnsafeModelFailure(t, err)
	}
}

func TestBudgetAttemptIncompleteProviderUsageStopsBeforeNextRequest(t *testing.T) {
	chatNoUsage := `{"id":"x","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`
	responsesMissingOutput := `{"id":"x","object":"response","created_at":1,"model":"test","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5}}`
	anthropicMissingInput := `{"id":"msg","type":"message","role":"assistant","model":"test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"output_tokens":7}}`
	cases := []struct {
		name     string
		protocol string
		body     string
		in, out  int64
	}{
		{"chat missing usage", "openai-chat", chatNoUsage, 0, 0},
		{"responses missing output", "openai-responses", responsesMissingOutput, 5, 0},
		{"anthropic missing input", "anthropic", anthropicMissingInput, 0, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			var mu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			t.Setenv("UNKNOWN_USAGE_KEY", "key")
			registry, err := llm.NewRegistry(map[string]config.ProviderConfig{"fake": {
				Protocol: tc.protocol, Model: "test", BaseURL: server.URL, APIKeyEnv: "UNKNOWN_USAGE_KEY", MaxTokens: 512,
			}}, llm.WithRetryBaseDelay(time.Millisecond))
			if err != nil {
				t.Fatal(err)
			}
			executor := newTestExecutor(t, ExecutorDeps{Profile: onboardingProfile(), Clients: registry})
			out, err := executor.Execute(context.Background(), dag.NodeInput{
				WorkflowID: "wf", Node: dag.NodeSpec{ID: "onboarding", Kind: KindOnboarding, Params: json.RawMessage(`{"provider":"fake"}`)},
			})
			mu.Lock()
			requestCount := calls
			mu.Unlock()
			if requestCount != 1 || !out.UsageIncomplete || out.Usage.InputTokens != tc.in || out.Usage.OutputTokens != tc.out {
				t.Fatalf("calls=%d out=%+v err=%v", requestCount, out, err)
			}
			requireUnsafeModelFailure(t, err)
		})
	}
}

func TestSynthesisBeforeTurnRejectsIncompleteUsage(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Synthesize.Mode = "agent"
	profile.Config.Synthesize.Provider = "claude"
	dossier := validDossier(t)
	spec, err := BuildGraph(profile, dossier, GraphConfig{AllowedProviders: []string{"local", "claude"}})
	if err != nil {
		t.Fatal(err)
	}
	adjudicated, _ := json.Marshal(AdjudicateOut{Groups: []Group{}})
	wf := dag.WorkflowView{ID: "wf", Spec: spec, Nodes: map[string]dag.NodeView{
		NodeAdjudicate: {ID: NodeAdjudicate, Spec: mustNode(t, spec, NodeAdjudicate), Status: dag.NodeCompleted, Output: adjudicated},
		NodeSynth:      {ID: NodeSynth, Spec: mustNode(t, spec, NodeSynth), Status: dag.NodeRunning},
	}, Order: []string{NodeAdjudicate, NodeSynth}}
	client := &unknownUsageClient{firstUse: llm.Usage{InputTokens: 5, Incomplete: true}}
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier: dossier, Profile: profile,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		Clients:  staticClients{client},
	})
	out, err := executor.Execute(context.Background(), dag.NodeInput{WorkflowID: wf.ID, Node: mustNode(t, spec, NodeSynth)})
	if client.count() != 1 || !out.UsageIncomplete || out.Usage.InputTokens != 5 || len(out.Data) != 0 {
		t.Fatalf("calls=%d out=%+v err=%v", client.count(), out, err)
	}
	requireUnsafeModelFailure(t, err)
}
