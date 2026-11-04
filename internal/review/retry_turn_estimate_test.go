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

func TestRetryInheritsSlowestTurnEstimate(t *testing.T) {
	var estimates []time.Duration
	attempts := 0
	executor := newTestExecutor(t, ExecutorDeps{
		Profile: onboardingProfile(),
		Sleep:   func(context.Context, time.Duration) error { return nil },
		AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			attempts++
			estimates = append(estimates, spec.TurnEstimate)
			switch attempts {
			case 1:
				return &agent.RunResult{LongestTurn: 40 * time.Second, Usage: llm.Usage{InputTokens: 1}}, &llm.Error{Kind: llm.ErrTransient}
			case 2:
				// A failure before any response keeps the earlier estimate.
				return nil, &llm.Error{Kind: llm.ErrTransient}
			}
			return &agent.RunResult{Output: &OnboardingOut{Overview: "ok"}}, nil
		},
	})
	_, err := executor.Execute(context.Background(), dag.NodeInput{WorkflowID: "wf", Node: dag.NodeSpec{
		ID: KindOnboarding, Kind: KindOnboarding, Params: json.RawMessage(`{"provider":"fake"}`),
	}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := []time.Duration{0, 40 * time.Second, 40 * time.Second}
	if len(estimates) != len(want) {
		t.Fatalf("attempts = %d, want %d", len(estimates), len(want))
	}
	for i := range want {
		if estimates[i] != want[i] {
			t.Fatalf("attempt %d turn estimate = %s, want %s", i+1, estimates[i], want[i])
		}
	}
}
