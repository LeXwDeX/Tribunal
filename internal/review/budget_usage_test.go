package review

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/agent"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

func TestRunAgentCarriesPartialUsageAcrossRetries(t *testing.T) {
	attempts := 0
	var seen []llm.Usage
	executor := newTestExecutor(t, ExecutorDeps{
		AgentRun: func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			attempts++
			if spec.BeforeTurn != nil {
				if err := spec.BeforeTurn(ctx, llm.Usage{}); err != nil {
					return nil, err
				}
			}
			if attempts == 1 {
				return &agent.RunResult{Usage: llm.Usage{InputTokens: 10}, Turns: 1},
					&llm.Error{Kind: llm.ErrTransient}
			}
			return &agent.RunResult{Usage: llm.Usage{OutputTokens: 5}, Turns: 1}, nil
		},
	})
	result, err := executor.runAgent(context.Background(), "local", agent.RunSpec{
		Submit: agent.SubmitSpec{
			Schema: json.RawMessage(`{"type":"object"}`),
			Decode: func(json.RawMessage) (any, error) { return nil, nil },
		},
		BeforeTurn: func(_ context.Context, usage llm.Usage) error {
			seen = append(seen, usage)
			return nil
		},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d", attempts)
	}
	if len(seen) != 2 || seen[0].InputTokens != 0 || seen[1].InputTokens != 10 {
		t.Fatalf("budget hook usage = %+v, want a carried first attempt", seen)
	}
	if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 5 {
		t.Fatalf("result usage = %+v, want 10+5", result.Usage)
	}
}

func TestExecutorFailureRetainsKnownUsage(t *testing.T) {
	profile := onboardingProfile()
	calls := 0
	executor := newTestExecutor(t, ExecutorDeps{
		Profile: profile,
		AgentRun: func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			calls++
			if spec.BeforeTurn != nil {
				_ = spec.BeforeTurn(ctx, llm.Usage{})
			}
			return &agent.RunResult{Usage: llm.Usage{InputTokens: 12, OutputTokens: 3}, Turns: 1},
				&llm.Error{Kind: llm.ErrAuth}
		},
	})
	params, _ := json.Marshal(onboardingParams{Provider: "claude"})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: "wf-onboarding-failure",
		Node:       dag.NodeSpec{ID: "onboarding", Kind: KindOnboarding, Params: params},
	})
	if err == nil {
		t.Fatal("expected the model failure")
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
	if out.Usage.InputTokens != 12 || out.Usage.OutputTokens != 3 {
		t.Fatalf("failed node usage = %+v, want the known partial usage", out.Usage)
	}
}

func TestExecutorOnboardingSharesWorkflowBudget(t *testing.T) {
	profile := onboardingProfile()
	wf := dag.WorkflowView{
		ID:    "wf-onboarding-budget",
		Spec:  dag.GraphSpec{Budget: dag.Budget{MaxTokens: 5}},
		Usage: dag.Usage{InputTokens: 5},
	}
	calls := 0
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:  profile,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		AgentRun: func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			calls++
			if spec.BeforeTurn == nil {
				return nil, errors.New("missing workflow budget hook")
			}
			if err := spec.BeforeTurn(ctx, llm.Usage{}); err != nil {
				return nil, err
			}
			return &agent.RunResult{Output: &OnboardingOut{Overview: "x"}}, nil
		},
	})
	params, _ := json.Marshal(onboardingParams{Provider: "claude"})
	_, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: wf.ID,
		Node:       dag.NodeSpec{ID: "onboarding", Kind: KindOnboarding, Params: params},
	})
	if err == nil || !errors.Is(err, errTokenBudgetExhausted) {
		t.Fatalf("error = %v, want token budget exhaustion", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want exactly one blocked attempt", calls)
	}
}

func TestExecutorExhaustedBudgetCannotReturnPass(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Synthesize.Mode = "code"
	dossier := validDossier(t)
	spec, err := BuildGraph(profile, dossier, GraphConfig{AllowedProviders: []string{"local", "claude"}})
	if err != nil {
		t.Fatal(err)
	}
	adjudicatedRaw, _ := json.Marshal(AdjudicateOut{Groups: []Group{}})
	wf := dag.WorkflowView{
		ID:    "wf-budget-pass",
		Spec:  dag.GraphSpec{Nodes: spec.Nodes, Budget: dag.Budget{MaxTokens: 5, MaxNodes: 30}},
		Usage: dag.Usage{InputTokens: 5},
		Nodes: map[string]dag.NodeView{
			NodeAdjudicate: {ID: NodeAdjudicate, Spec: mustNode(t, spec, NodeAdjudicate), Status: dag.NodeCompleted, Output: adjudicatedRaw},
			NodeSynth:      {ID: NodeSynth, Spec: mustNode(t, spec, NodeSynth), Status: dag.NodeRunning},
		},
		Order: []string{NodeAdjudicate, NodeSynth},
	}
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  dossier,
		Profile:  profile,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
	})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: wf.ID,
		Node:       mustNode(t, spec, NodeSynth),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := decodeSynthesisResult(out.Data)
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE instead of a false PASS", result.Verdict)
	}
	if len(result.Unresolved) == 0 || result.Unresolved[0].Kind != UnresolvedEvidence {
		t.Fatalf("unresolved = %+v", result.Unresolved)
	}
}

func TestExecutorOnboardingStopsSecondTurnAtSharedLimit(t *testing.T) {
	profile := onboardingProfile()
	wf := dag.WorkflowView{
		ID:    "wf-onboarding-multiturn",
		Spec:  dag.GraphSpec{Budget: dag.Budget{MaxTokens: 5}},
		Usage: dag.Usage{},
	}
	turns := 0
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:  profile,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		AgentRun: func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			turns++
			// First turn at zero usage is allowed, the second request is not.
			if err := spec.BeforeTurn(ctx, llm.Usage{}); err != nil {
				return nil, err
			}
			if err := spec.BeforeTurn(ctx, llm.Usage{InputTokens: 5}); err != nil {
				return nil, err
			}
			return &agent.RunResult{Usage: llm.Usage{InputTokens: 5}}, nil
		},
	})
	params, _ := json.Marshal(onboardingParams{Provider: "claude"})
	_, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: wf.ID,
		Node:       dag.NodeSpec{ID: "onboarding", Kind: KindOnboarding, Params: params},
	})
	if err == nil || !errors.Is(err, errTokenBudgetExhausted) {
		t.Fatalf("error = %v, want the second turn blocked", err)
	}
	if turns != 1 {
		t.Fatalf("turns = %d", turns)
	}
}

func TestExecutorMidCallBudgetCannotReturnPass(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Synthesize.Mode = "agent"
	profile.Config.Synthesize.Provider = "claude"
	profile.Config.Budget.MaxTokens = 10
	profile.Config.Budget.MaxNodes = 30
	dossier := validDossier(t)
	spec, err := BuildGraph(profile, dossier, GraphConfig{AllowedProviders: []string{"local", "claude"}})
	if err != nil {
		t.Fatal(err)
	}
	group := Group{
		ID: "g1", Severity: SeverityP2, Category: "correctness",
		Path: "a.go", Line: 10, Side: SideNew, Title: "small issue",
		Body: "body", Evidence: "evidence",
		Challenge: &Challenge{Status: ChallengeUncertain, Reason: "verify uncertain"},
		Decision:  &FindingDecision{Action: ActionMark, Reason: "on_uncertain=mark", Source: "rules"},
		Sources: []Finding{{
			ID: "f1", Origin: Origin{Node: "cr.p1", Index: 0, Provider: "local"},
			Severity: SeverityP2, Category: "correctness", Path: "a.go", Line: 10, Side: SideNew,
			Title: "small issue", Body: "body", Evidence: "evidence",
		}},
	}
	adjudicatedRaw, _ := json.Marshal(AdjudicateOut{Groups: []Group{group}})
	usage := int64(0)
	wf := dag.WorkflowView{
		ID:   "wf-mid-budget-pass",
		Spec: spec,
		Nodes: map[string]dag.NodeView{
			NodeAdjudicate: {ID: NodeAdjudicate, Spec: mustNode(t, spec, NodeAdjudicate), Status: dag.NodeCompleted, Output: adjudicatedRaw},
			NodeSynth:      {ID: NodeSynth, Spec: mustNode(t, spec, NodeSynth), Status: dag.NodeRunning},
		},
		Order: []string{NodeAdjudicate, NodeSynth},
	}
	output := SynthesizeOut{
		Verdict: VerdictPass, Summary: "looks good",
		Decisions:   []Decision{{FindingID: group.ID, Action: "mark", Reason: "small issue"}},
		NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{},
	}
	outputRaw, _ := json.Marshal(output)
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier: dossier,
		Profile: profile,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) {
			wf.Usage = dag.Usage{InputTokens: usage}
			return wf, nil
		},
		AgentRun: func(ctx context.Context, runSpec agent.RunSpec) (*agent.RunResult, error) {
			if err := runSpec.BeforeTurn(ctx, llm.Usage{}); err != nil {
				return nil, err
			}
			usage = 10
			if err := runSpec.BeforeTurn(ctx, llm.Usage{InputTokens: 10}); err != nil {
				return nil, err
			}
			decoded, err := runSpec.Submit.Decode(outputRaw)
			if err != nil {
				return nil, err
			}
			return &agent.RunResult{Output: decoded}, nil
		},
	})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: wf.ID,
		Node:       mustNode(t, spec, NodeSynth),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := decodeSynthesisResult(out.Data)
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE after mid-call budget exhaustion", result.Verdict)
	}
	if len(result.Unresolved) == 0 {
		t.Fatal("budget exhaustion left no unresolved gap")
	}
}

func TestExecutorFailureRetainsCacheAndReasoningUsage(t *testing.T) {
	profile := onboardingProfile()
	executor := newTestExecutor(t, ExecutorDeps{
		Profile: profile,
		AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			return &agent.RunResult{Usage: llm.Usage{
				CacheReadTokens: 4, CacheWriteTokens: 2, ReasoningTokens: 3,
			}, Turns: 1}, &llm.Error{Kind: llm.ErrAuth}
		},
	})
	params, _ := json.Marshal(onboardingParams{Provider: "claude"})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: "wf-onboarding-cache-usage",
		Node:       dag.NodeSpec{ID: "onboarding", Kind: KindOnboarding, Params: params},
	})
	if err == nil {
		t.Fatal("expected the model failure")
	}
	if out.Usage.CacheReadTokens != 4 || out.Usage.CacheWriteTokens != 2 ||
		out.Usage.ReasoningTokens != 3 {
		t.Fatalf("failed node usage = %+v, want cache and reasoning tokens", out.Usage)
	}
}
