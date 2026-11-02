package review

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func TestReviewProbeInvalidProviderUsageFlag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fake","object":"response","created_at":1,"model":"test","status":"completed","output":[{"type":"function_call","id":"fc","call_id":"c","name":"submit_result","arguments":"{}","status":"completed"}],"usage":{"input_tokens":5,"output_tokens":-2}}`))
	}))
	defer server.Close()
	t.Setenv("PROBE_PROVIDER_KEY", "fake")
	registry, err := llm.NewRegistry(map[string]config.ProviderConfig{"fake": {
		Protocol: "openai-responses", Model: "test", BaseURL: server.URL, APIKeyEnv: "PROBE_PROVIDER_KEY",
	}}, llm.WithRetryBaseDelay(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	executor := newTestExecutor(t, ExecutorDeps{Profile: onboardingProfile(), Clients: registry})
	engine := dag.NewEngine(dag.NewMemoryEventStore(), executor, nil, dag.Options{})
	id, err := engine.Create(context.Background(), dag.GraphSpec{Nodes: []dag.NodeSpec{{
		ID: "onboarding", Kind: KindOnboarding, Required: true, Params: json.RawMessage(`{"provider":"fake"}`),
	}}}, nil)
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
	node := wf.Nodes["onboarding"]
	if node.Reason != "unsafe_model_response" || node.Usage.InputTokens != 5 || node.Usage.OutputTokens != 0 {
		t.Fatalf("unexpected node: status=%s reason=%s usage=%+v", node.Status, node.Reason, node.Usage)
	}
	if !node.UsageIncomplete || !wf.UsageIncomplete {
		t.Fatalf("invalid provider usage was not marked incomplete: node=%t workflow=%t usage=%+v", node.UsageIncomplete, wf.UsageIncomplete, node.Usage)
	}
}

func TestReviewRealProviderReturnsIncompleteNodeOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fake","object":"response","created_at":1,"model":"test","status":"completed","output":[{"type":"function_call","id":"fc","call_id":"c","name":"submit_result","arguments":"{}","status":"completed"}],"usage":{"input_tokens":5,"output_tokens":-2}}`))
	}))
	defer server.Close()
	t.Setenv("NODE_USAGE_PROBE_KEY", "fake")
	registry, err := llm.NewRegistry(map[string]config.ProviderConfig{"fake": {Protocol: "openai-responses", Model: "test", BaseURL: server.URL, APIKeyEnv: "NODE_USAGE_PROBE_KEY"}}, llm.WithRetryBaseDelay(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	e := newTestExecutor(t, ExecutorDeps{Profile: onboardingProfile(), Clients: registry})
	out, err := e.Execute(context.Background(), dag.NodeInput{WorkflowID: "wf", Node: dag.NodeSpec{ID: "onboarding", Kind: KindOnboarding, Params: json.RawMessage(`{"provider":"fake"}`)}})
	if !errors.Is(err, agent.ErrUnsafeResponse) || out.Usage.InputTokens != 5 || out.Usage.OutputTokens != 0 || !out.UsageIncomplete {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

// API follow-up: expose this same SQLite workflow through an authenticated
// server and verify list, detail, graph and node endpoints before and after
// reopening the store. Assert 5/0, both flags, and project access isolation.
func TestReviewInvalidProviderUsagePersistsInSQLite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")
	open := func() *sqlite.Store {
		st, err := sqlite.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	st := open()
	defer func() { _ = st.Close() }()
	_, err := st.RegisterProject(ctx, store.Project{ID: 42, PathWithNamespace: "group/project", WebURL: "https://gitlab.invalid/group/project", DefaultBranch: "main", TokenEnc: []byte("fake"), WebhookSecretEnc: []byte("fake"), TokenScopes: []string{"api"}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fake","object":"response","created_at":1,"model":"test","status":"completed","output":[{"type":"function_call","id":"fc","call_id":"c","name":"submit_result","arguments":"{}","status":"completed"}],"usage":{"input_tokens":5,"output_tokens":-2}}`))
	}))
	defer server.Close()
	t.Setenv("SQLITE_USAGE_PROBE_KEY", "fake")
	registry, err := llm.NewRegistry(map[string]config.ProviderConfig{"fake": {Protocol: "openai-responses", Model: "test", BaseURL: server.URL, APIKeyEnv: "SQLITE_USAGE_PROBE_KEY"}}, llm.WithRetryBaseDelay(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	e := newTestExecutor(t, ExecutorDeps{Profile: onboardingProfile(), Clients: registry})
	engine := dag.NewEngine(st, e, nil, dag.Options{})
	id, err := engine.Create(ctx, dag.GraphSpec{Nodes: []dag.NodeSpec{{ID: "onboarding", Kind: KindOnboarding, Required: true, Params: json.RawMessage(`{"provider":"fake"}`)}}}, map[string]string{"project_id": "42", "kind": "review", "mr_iid": "7", "trigger": "api"})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(ctx, id); err != nil {
		t.Fatal(err)
	}
	check := func(label string) {
		t.Helper()
		wf, err := st.GetWorkflow(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		nodes, err := st.GetWorkflowNodes(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !wf.UsageIncomplete || wf.TokensIn != 5 || wf.TokensOut != 0 || len(nodes) != 1 || !nodes[0].UsageIncomplete || nodes[0].TokensIn != 5 || nodes[0].TokensOut != 0 || nodes[0].Status != string(dag.NodeFailed) {
			t.Fatalf("%s workflow=%+v nodes=%+v", label, wf, nodes)
		}
	}
	check("before restart")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = open()
	check("after restart")
}

func TestReviewNodeOutputUsageIncomplete(t *testing.T) {
	for _, mode := range []string{"success", "failure", "cancel", "zero", "invalid-output"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e := newTestExecutor(t, ExecutorDeps{Profile: onboardingProfile(), AgentRun: func(_ context.Context, _ agent.RunSpec) (*agent.RunResult, error) {
				usage := llm.Usage{InputTokens: 5, Incomplete: true}
				if mode == "zero" {
					usage.InputTokens = 0
				}
				if mode == "cancel" {
					cancel()
				}
				if mode == "invalid-output" {
					return &agent.RunResult{Output: "unexpected", Usage: usage}, nil
				}
				if mode != "success" {
					return &agent.RunResult{Usage: usage}, &llm.Error{Kind: llm.ErrUnsafeResponse}
				}
				return &agent.RunResult{Output: &OnboardingOut{Overview: "ok"}, Usage: usage}, nil
			}})
			out, err := e.Execute(ctx, dag.NodeInput{WorkflowID: "wf", Node: dag.NodeSpec{ID: "onboarding", Kind: KindOnboarding, Params: json.RawMessage(`{"provider":"fake"}`)}})
			want := int64(5)
			if mode == "zero" {
				want = 0
			}
			if (err == nil) != (mode == "success") || mode == "cancel" && !errors.Is(err, context.Canceled) || out.Usage.InputTokens != want || !out.UsageIncomplete {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}
}

func TestReviewSynthesisFallbackPreservesIncompleteUsage(t *testing.T) {
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
	e := newTestExecutor(t, ExecutorDeps{Dossier: dossier, Profile: profile, Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil }, AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return &agent.RunResult{Usage: llm.Usage{InputTokens: 5, Incomplete: true}}, &llm.Error{Kind: llm.ErrTransient}
	}})
	out, err := e.Execute(context.Background(), dag.NodeInput{WorkflowID: wf.ID, Node: mustNode(t, spec, NodeSynth)})
	if err != nil || out.Usage.InputTokens != 5 || !out.UsageIncomplete {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	result, err := decodeSynthesisResult(out.Data)
	if err != nil || !result.Fallback {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestReviewRetryOverflowMarksNodeUsageIncomplete(t *testing.T) {
	attempts := 0
	e := newTestExecutor(t, ExecutorDeps{Profile: onboardingProfile(), AgentRun: func(_ context.Context, _ agent.RunSpec) (*agent.RunResult, error) {
		attempts++
		if attempts == 1 {
			return &agent.RunResult{Usage: llm.Usage{InputTokens: math.MaxInt64 - 1}}, &llm.Error{Kind: llm.ErrTransient}
		}
		return &agent.RunResult{Output: &OnboardingOut{Overview: "ok"}, Usage: llm.Usage{InputTokens: 2}}, nil
	}})
	out, err := e.Execute(context.Background(), dag.NodeInput{WorkflowID: "wf", Node: dag.NodeSpec{ID: "onboarding", Kind: KindOnboarding, Params: json.RawMessage(`{"provider":"fake"}`)}})
	if err != nil || attempts != 2 || out.Usage.InputTokens != math.MaxInt64 || !out.UsageIncomplete {
		t.Fatalf("out=%+v err=%v attempts=%d", out, err, attempts)
	}
}

func TestReviewRunnerNilResultRetainsInvalidErrorUsage(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "nil", true: "partial"}[partial], func(t *testing.T) {
			attempts := 0
			e := newTestExecutor(t, ExecutorDeps{Profile: onboardingProfile(), AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
				attempts++
				var result *agent.RunResult
				if partial {
					result = &agent.RunResult{Usage: llm.Usage{InputTokens: 3, OutputTokens: 1}}
				}
				return result, &llm.Error{Kind: llm.ErrTransient, Usage: llm.Usage{InputTokens: 5, OutputTokens: -2}}
			}})
			out, err := e.Execute(context.Background(), dag.NodeInput{WorkflowID: "wf", Node: dag.NodeSpec{ID: "onboarding", Kind: KindOnboarding, Params: json.RawMessage(`{"provider":"fake"}`)}})
			wantOut := int64(0)
			if partial {
				wantOut = 1
			}
			if !llm.IsUnsafeResponse(err) || out.Usage.InputTokens != 5 || out.Usage.OutputTokens != wantOut || !out.UsageIncomplete || attempts != 1 {
				t.Fatalf("out=%+v err=%v attempts=%d", out, err, attempts)
			}
		})
	}
}
