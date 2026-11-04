package review

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func TestBudgetedAgentNodeReportsRemainingWorkflowBudget(t *testing.T) {
	wf := dag.WorkflowView{
		ID:    "wf-remaining",
		Spec:  dag.GraphSpec{Budget: dag.Budget{MaxTokens: 1000}},
		Usage: dag.Usage{InputTokens: 200},
	}
	attempts := 0
	var remaining []int64
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:  onboardingProfile(),
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		Sleep:    func(context.Context, time.Duration) error { return nil },
		AgentRun: func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			attempts++
			if spec.BudgetRemaining == nil {
				t.Fatal("budgeted node has no BudgetRemaining")
			}
			left, ok := spec.BudgetRemaining(ctx, llm.Usage{InputTokens: 100, OutputTokens: 50})
			if !ok {
				t.Fatal("budget not reported")
			}
			remaining = append(remaining, left)
			if attempts == 1 {
				return &agent.RunResult{Usage: llm.Usage{InputTokens: 30}}, &llm.Error{Kind: llm.ErrTransient}
			}
			return &agent.RunResult{Output: &OnboardingOut{Overview: "ok"}}, nil
		},
	})
	_, err := executor.Execute(context.Background(), dag.NodeInput{WorkflowID: wf.ID, Node: dag.NodeSpec{
		ID: KindOnboarding, Kind: KindOnboarding, Params: json.RawMessage(`{"provider":"fake"}`),
	}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// 1000 - 200 workflow - 150 this attempt; the retry also counts the
	// failed attempt's 30 tokens.
	if len(remaining) != 2 || remaining[0] != 650 || remaining[1] != 620 {
		t.Fatalf("remaining = %v, want [650 620]", remaining)
	}
}

func TestUnbudgetedWorkflowReportsNoBudget(t *testing.T) {
	wf := dag.WorkflowView{ID: "wf-unlimited"}
	executor := newTestExecutor(t, ExecutorDeps{Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil }})
	if _, ok := executor.budgetRemaining(wf.ID)(context.Background(), llm.Usage{InputTokens: 10}); ok {
		t.Fatal("a workflow without max_tokens must report no budget")
	}
}
