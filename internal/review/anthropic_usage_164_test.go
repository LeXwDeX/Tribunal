package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

const anthropicUsage164First = `{"id":"first","type":"message","role":"assistant","model":"fake","content":[{"type":"text","text":"draft"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":3,"cache_read_input_tokens":2,"output_tokens_details":{"thinking_tokens":4}}}`
const anthropicUsage164Second = `{"id":"second","type":"message","role":"assistant","model":"fake","content":[{"type":"tool_use","id":"call2","name":"submit_result","input":{"overview":"audit","architecture":"audit","modules":[],"conventions":[],"build_and_test":[],"risk_hotspots":[],"review_focus":[]}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`

type anthropicUsage164Transport struct{ calls atomic.Int64 }

func (tr *anthropicUsage164Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "usage164.invalid" {
		return nil, fmt.Errorf("unexpected host %q", req.URL.Host)
	}
	var body string
	switch tr.calls.Add(1) {
	case 1:
		body = anthropicUsage164First
	case 2:
		body = anthropicUsage164Second
	default:
		return nil, errors.New("unexpected extra request")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func anthropicUsage164Registry(t *testing.T) (*llm.Registry, *anthropicUsage164Transport) {
	t.Helper()
	t.Setenv("TEST_ANTHROPIC_USAGE_164_KEY", "fake-key")
	transport := &anthropicUsage164Transport{}
	registry, err := llm.NewRegistry(map[string]config.ProviderConfig{
		"fake": {Protocol: "anthropic", BaseURL: "https://usage164.invalid", Model: "fake", APIKeyEnv: "TEST_ANTHROPIC_USAGE_164_KEY", MaxTokens: 128},
	}, llm.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	return registry, transport
}

func anthropicUsage164Cost(_ string, usage llm.Usage) int64 {
	return usage.InputTokens*3 + usage.OutputTokens*15
}

func TestAnthropicUsage164ReviewExecutorBudget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		limit      int64
		committed  dag.Usage
		wantBlocks bool
	}{
		{name: "over limit", limit: 18, wantBlocks: true},
		{name: "at limit", limit: 20, wantBlocks: true},
		{name: "below limit", limit: 21},
		{name: "committed usage at limit", limit: 24, committed: dag.Usage{InputTokens: 3, OutputTokens: 1}, wantBlocks: true},
		{name: "committed usage below limit", limit: 25, committed: dag.Usage{InputTokens: 3, OutputTokens: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry, transport := anthropicUsage164Registry(t)
			wf := dag.WorkflowView{Spec: dag.GraphSpec{Budget: dag.Budget{MaxTokens: tc.limit}}, Usage: tc.committed}
			executor := newTestExecutor(t, ExecutorDeps{
				Profile:  onboardingProfile(),
				Clients:  registry,
				Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
				Cost:     anthropicUsage164Cost,
			})
			out, err := executor.Execute(context.Background(), dag.NodeInput{
				WorkflowID: "usage-164", Node: dag.NodeSpec{ID: "onboarding", Kind: KindOnboarding, Params: json.RawMessage(`{"provider":"fake"}`)},
			})
			want := dag.Usage{InputTokens: 15, OutputTokens: 5, CacheReadTokens: 2, CacheWriteTokens: 3, ReasoningTokens: 4, CostMicros: 120}
			wantCalls := int64(1)
			if !tc.wantBlocks {
				want.InputTokens++
				want.OutputTokens++
				want.CostMicros = 138
				wantCalls++
			}
			if tc.wantBlocks && !errors.Is(err, errTokenBudgetExhausted) || !tc.wantBlocks && err != nil {
				t.Errorf("error = %v, want budget exhausted = %t", err, tc.wantBlocks)
			}
			if out.Usage != want || transport.calls.Load() != wantCalls {
				t.Errorf("usage = %+v, calls = %d; want %+v, calls = %d", out.Usage, transport.calls.Load(), want, wantCalls)
			}
		})
	}
}

type anthropicUsage164DAGExecutor struct {
	client llm.Client
	review *Executor
}

func (e anthropicUsage164DAGExecutor) Execute(ctx context.Context, _ dag.NodeInput) (dag.NodeOutput, error) {
	resp, err := e.client.Generate(ctx, &llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "audit"}}}}})
	if err != nil {
		return dag.NodeOutput{}, err
	}
	return dag.NodeOutput{Data: json.RawMessage(`{}`), Usage: e.review.dagUsage("fake", resp.Usage)}, nil
}

func TestAnthropicUsage164DAGSchedulerBudget(t *testing.T) {
	registry, transport := anthropicUsage164Registry(t)
	client, err := registry.Client("fake")
	if err != nil {
		t.Fatal(err)
	}
	executor := newTestExecutor(t, ExecutorDeps{Cost: anthropicUsage164Cost})
	engine := dag.NewEngine(dag.NewMemoryEventStore(), anthropicUsage164DAGExecutor{client: client, review: executor}, nil, dag.Options{MaxConcurrentNodes: 1})
	id, err := engine.Create(context.Background(), dag.GraphSpec{
		Nodes: []dag.NodeSpec{
			{ID: "a", Kind: "audit", Required: true},
			{ID: "b", Kind: "audit", Required: true, DependsOn: []string{"a"}},
		}, Budget: dag.Budget{MaxTokens: 18},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	want := dag.Usage{InputTokens: 15, OutputTokens: 5, CacheReadTokens: 2, CacheWriteTokens: 3, ReasoningTokens: 4, CostMicros: 120}
	if wf.Usage != want || wf.Nodes["a"].Usage != want || wf.Nodes["b"].Status != dag.NodeFailed || wf.Nodes["b"].Reason != "budget_exhausted" || transport.calls.Load() != 1 {
		t.Fatalf("workflow usage = %+v, first = %+v, second = %+v, calls = %d; want usage %+v and second node blocked after one call", wf.Usage, wf.Nodes["a"], wf.Nodes["b"], transport.calls.Load(), want)
	}
}
