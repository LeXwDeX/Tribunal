package review

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func TestBudgetAttemptSynthesisCountsUncommittedResult(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{true: "fallback", false: "success"}[fallback], func(t *testing.T) {
			profile := defaultProfile()
			profile.Config.Synthesize.Mode = "agent"
			profile.Config.Synthesize.Provider = "claude"
			dossier := validDossier(t)
			spec, err := BuildGraph(profile, dossier, GraphConfig{AllowedProviders: []string{"local", "claude"}})
			if err != nil {
				t.Fatal(err)
			}
			adjudicated, _ := json.Marshal(AdjudicateOut{Groups: []Group{}})
			wf := dag.WorkflowView{ID: "wf", Spec: spec, Usage: dag.Usage{InputTokens: 4}, Nodes: map[string]dag.NodeView{NodeAdjudicate: {ID: NodeAdjudicate, Spec: mustNode(t, spec, NodeAdjudicate), Status: dag.NodeCompleted, Output: adjudicated}, NodeSynth: {ID: NodeSynth, Spec: mustNode(t, spec, NodeSynth), Status: dag.NodeRunning}}, Order: []string{NodeAdjudicate, NodeSynth}}
			wf.Spec.Budget.MaxTokens = 10
			executor := newTestExecutor(t, ExecutorDeps{Dossier: dossier, Profile: profile, Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil }, AgentRun: func(_ context.Context, s agent.RunSpec) (*agent.RunResult, error) {
				if fallback {
					return &agent.RunResult{Usage: llm.Usage{InputTokens: 6}}, &llm.Error{Kind: llm.ErrTransient}
				}
				output, err := s.Submit.Decode(json.RawMessage(`{"verdict":"PASS","summary":"ok","decisions":[],"new_findings":[],"extend_requests":[]}`))
				return &agent.RunResult{Output: output, Usage: llm.Usage{InputTokens: 6}}, err
			}})
			out, err := executor.Execute(context.Background(), dag.NodeInput{WorkflowID: wf.ID, Node: mustNode(t, spec, NodeSynth)})
			if err != nil {
				t.Fatal(err)
			}
			result, err := decodeSynthesisResult(out.Data)
			if err != nil {
				t.Fatal(err)
			}
			if result.Verdict != VerdictIncomplete || out.Usage.InputTokens != 6 {
				t.Fatalf("verdict=%s usage=%+v", result.Verdict, out.Usage)
			}
		})
	}
}

func TestBudgetAttemptOnboardingHTTPFailureStopsAtCommittedLimit(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"failed","object":"response","created_at":1,"model":"test","status":"failed","error":{"code":"server_error","message":"secret"},"output":[],"usage":{"input_tokens":11,"output_tokens":7}}`))
	}))
	defer server.Close()
	t.Setenv("BUDGET_ATTEMPT_KEY", "key")
	var records []llm.CallRecord
	registry, err := llm.NewRegistry(map[string]config.ProviderConfig{"fake": {Protocol: "openai-responses", Model: "test", BaseURL: server.URL, APIKeyEnv: "BUDGET_ATTEMPT_KEY"}},
		llm.WithRetryBaseDelay(time.Millisecond), llm.WithRecorder(llm.RecorderFunc(func(_ context.Context, r llm.CallRecord) { records = append(records, r) })))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		limit    int64
		attempts int32
	}{{"provider-retry", 20, 1}, {"agent-retry", 92, 5}} {
		t.Run(tc.name, func(t *testing.T) {
			calls.Store(0)
			records = nil
			wf := dag.WorkflowView{ID: "wf", Usage: dag.Usage{InputTokens: 2}, Spec: dag.GraphSpec{Budget: dag.Budget{MaxTokens: tc.limit}}}
			e := newTestExecutor(t, ExecutorDeps{Profile: onboardingProfile(), Clients: registry, Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil }, Sleep: func(context.Context, time.Duration) error { return nil }})
			out, err := e.Execute(context.Background(), dag.NodeInput{WorkflowID: wf.ID, Node: dag.NodeSpec{ID: "onboarding", Kind: KindOnboarding, Params: json.RawMessage(`{"provider":"fake"}`)}})
			var failure *llm.Error
			if !errors.Is(err, errTokenBudgetExhausted) || !errors.As(err, &failure) || failure.Kind != llm.ErrTransient || calls.Load() != tc.attempts || len(records) != int(tc.attempts) || out.Usage.InputTokens != int64(tc.attempts)*11 || out.Usage.OutputTokens != int64(tc.attempts)*7 {
				t.Fatalf("err=%v calls=%d records=%d usage=%+v", err, calls.Load(), len(records), out.Usage)
			}
		})
	}
}

func TestBudgetAttemptResponsesFailedToolCannotBecomeSynthesisFallback(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"failed","object":"response","created_at":1,"model":"test","status":"failed","error":{"code":"server_error","message":"secret"},"output":[{"type":"function_call","id":"fc_1","call_id":"c1","name":"submit_result","arguments":"{}","status":"completed"}],"usage":{"input_tokens":11,"output_tokens":7}}`))
	}))
	defer server.Close()
	t.Setenv("BUDGET_ATTEMPT_SYNTH_KEY", "key")
	registry, err := llm.NewRegistry(map[string]config.ProviderConfig{"test": {Protocol: "openai-responses", Model: "test", BaseURL: server.URL, APIKeyEnv: "BUDGET_ATTEMPT_SYNTH_KEY"}}, llm.WithRetryBaseDelay(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	client, err := registry.Client("test")
	if err != nil {
		t.Fatal(err)
	}
	executor := evidenceAgentSynthesizer(t, func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		spec.Client = client
		return agent.Run(ctx, spec)
	})
	out, err := executor.Execute(context.Background(), safetySynthesisInput())
	var classified interface{ ErrorClass() string }
	var failure *llm.Error
	if !errors.Is(err, agent.ErrUnsafeResponse) || !errors.As(err, &failure) || failure.Kind != llm.ErrUnsafeResponse || !errors.As(err, &classified) || classified.ErrorClass() != "unsafe_model_response" || len(out.Data) != 0 || calls.Load() != 1 || out.Usage.InputTokens != 11 || out.Usage.OutputTokens != 7 {
		t.Fatalf("err=%v output=%s calls=%d usage=%+v", err, out.Data, calls.Load(), out.Usage)
	}
}

func TestBudgetAttemptOverflowAndNegativeSnapshot(t *testing.T) {
	for _, usage := range []dag.Usage{{InputTokens: math.MaxInt64, OutputTokens: 1}, {InputTokens: 20, OutputTokens: -20}, {InputTokens: -1}} {
		e := newTestExecutor(t, ExecutorDeps{Snapshot: func(context.Context, string) (dag.WorkflowView, error) {
			return dag.WorkflowView{Usage: usage, Spec: dag.GraphSpec{Budget: dag.Budget{MaxTokens: 10}}}, nil
		}})
		if err := e.budgetHook("wf")(context.Background(), llm.Usage{}); err == nil {
			t.Fatalf("snapshot bypassed budget: %+v", usage)
		}
	}
}

func TestBudgetAttemptRejectsNegativeGuardInput(t *testing.T) {
	e := newTestExecutor(t, ExecutorDeps{Snapshot: func(context.Context, string) (dag.WorkflowView, error) {
		return dag.WorkflowView{Spec: dag.GraphSpec{Budget: dag.Budget{MaxTokens: 10}}}, nil
	}})
	if err := e.budgetHook("wf")(context.Background(), llm.Usage{InputTokens: -1}); err == nil {
		t.Fatal("invalid usage bypassed the budget check")
	}
}

func TestBudgetAttemptSynthesisFallbackRechecksCommittedOnZeroUsage(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Synthesize.Mode = "agent"
	profile.Config.Synthesize.Provider = "claude"
	dossier := validDossier(t)
	spec, err := BuildGraph(profile, dossier, GraphConfig{AllowedProviders: []string{"local", "claude"}})
	if err != nil {
		t.Fatal(err)
	}
	adjudicated, _ := json.Marshal(AdjudicateOut{Groups: []Group{}})
	wf := dag.WorkflowView{ID: "wf", Spec: spec, Usage: dag.Usage{InputTokens: 4}, Nodes: map[string]dag.NodeView{
		NodeAdjudicate: {ID: NodeAdjudicate, Spec: mustNode(t, spec, NodeAdjudicate), Status: dag.NodeCompleted, Output: adjudicated},
		NodeSynth:      {ID: NodeSynth, Spec: mustNode(t, spec, NodeSynth), Status: dag.NodeRunning}}, Order: []string{NodeAdjudicate, NodeSynth}}
	wf.Spec.Budget.MaxTokens = 10
	executor := newTestExecutor(t, ExecutorDeps{Dossier: dossier, Profile: profile, Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil }, AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		wf.Usage.InputTokens = 10
		return nil, &llm.Error{Kind: llm.ErrTransient}
	}})
	out, err := executor.Execute(context.Background(), dag.NodeInput{WorkflowID: wf.ID, Node: mustNode(t, spec, NodeSynth)})
	if err != nil {
		t.Fatal(err)
	}
	result, err := decodeSynthesisResult(out.Data)
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict=%s", result.Verdict)
	}
}

func TestBudgetAttemptRunnerRetainsUnsafeErrorWithInvalidUsage(t *testing.T) {
	unsafe := &llm.Error{Kind: llm.ErrUnsafeResponse}
	executor := newTestExecutor(t, ExecutorDeps{AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return &agent.RunResult{Usage: llm.Usage{InputTokens: 5, OutputTokens: -2}}, errors.Join(agent.ErrUnsafeResponse, unsafe)
	}})
	result, err := executor.runAgent(context.Background(), "local", agent.RunSpec{Submit: agent.SubmitSpec{
		Schema: json.RawMessage(`{}`), Decode: func(json.RawMessage) (any, error) { return nil, nil },
	}}, true)
	if !errors.Is(err, agent.ErrUnsafeResponse) || !errors.Is(err, unsafe) || result == nil || result.Usage.InputTokens != 5 || result.Usage.OutputTokens != 0 {
		t.Fatalf("error=%v result=%+v", err, result)
	}
}

func TestBudgetAttemptSafetyOrCancellationCannotTriggerAgentRetry(t *testing.T) {
	for _, blocker := range []error{agent.ErrUnsafeResponse, context.Canceled, context.DeadlineExceeded} {
		calls := 0
		executor := newTestExecutor(t, ExecutorDeps{AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			calls++
			return &agent.RunResult{Usage: llm.Usage{InputTokens: 5}}, errors.Join(blocker, &llm.Error{Kind: llm.ErrTransient})
		}, Sleep: func(context.Context, time.Duration) error { return nil }})
		result, err := executor.runAgent(context.Background(), "local", agent.RunSpec{Submit: agent.SubmitSpec{Schema: json.RawMessage(`{}`), Decode: func(json.RawMessage) (any, error) { return nil, nil }}}, true)
		if !errors.Is(err, blocker) || calls != 1 || result == nil || result.Usage.InputTokens != 5 {
			t.Fatalf("blocker=%v err=%v calls=%d result=%+v", blocker, err, calls, result)
		}
	}
}

func TestBudgetAttemptSafetyOrCancellationCannotBecomeSynthesisFallback(t *testing.T) {
	for _, blocker := range []error{agent.ErrUnsafeResponse, context.Canceled, context.DeadlineExceeded, &llm.Error{Kind: llm.ErrUnsafeResponse}} {
		executor := evidenceAgentSynthesizer(t, func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			return &agent.RunResult{Usage: llm.Usage{InputTokens: 5}}, errors.Join(&llm.Error{Kind: llm.ErrTransient}, agent.ErrInvalidSubmit, blocker)
		})
		out, err := executor.Execute(context.Background(), safetySynthesisInput())
		if !errors.Is(err, blocker) || len(out.Data) != 0 || out.Usage.InputTokens != 5 {
			t.Fatalf("blocker=%v err=%v output=%s usage=%+v", blocker, err, out.Data, out.Usage)
		}
		if errors.Is(blocker, agent.ErrUnsafeResponse) || errors.Is(blocker, llm.ErrUnsafeProviderResponse) {
			var classified interface{ ErrorClass() string }
			if !errors.As(err, &classified) || classified.ErrorClass() != "unsafe_model_response" {
				t.Fatalf("unsafe failure lost its class: %v", err)
			}
		}
	}
}
