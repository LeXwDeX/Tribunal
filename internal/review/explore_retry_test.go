package review

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func exploreNodeInput(t *testing.T) dag.NodeInput {
	t.Helper()
	params, err := json.Marshal(exploreParams{Provider: "local"})
	if err != nil {
		t.Fatal(err)
	}
	return dag.NodeInput{
		WorkflowID: "wf",
		Node:       dag.NodeSpec{ID: NodeExplore, Kind: KindExplore, Params: params},
	}
}

func exploreOutJSON(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(validExploreOut())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A context overflow is the one explore failure worth a second attempt: the
// turn budget is halved and the retry runs exactly once.
func TestExploreRetriesContextTooLongWithHalvedTurns(t *testing.T) {
	profile := lanesProfile()
	profile.Config.Budget.MaxTurns = 90

	var mu sync.Mutex
	var turnBudgets []int
	calls := 0
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:     profile,
		Dossier:     validDossier(t),
		MaxAttempts: 1,
		AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			turnBudgets = append(turnBudgets, spec.MaxTurns)
			if calls == 1 {
				return &agent.RunResult{Usage: llm.Usage{InputTokens: 100}, Turns: 20, Partial: true},
					&llm.Error{Kind: llm.ErrContextTooLong}
			}
			decoded, err := spec.Submit.Decode(exploreOutJSON(t))
			if err != nil {
				return nil, err
			}
			return &agent.RunResult{Output: decoded, Usage: llm.Usage{InputTokens: 5, OutputTokens: 1}}, nil
		},
	})

	out, err := executor.Execute(context.Background(), exploreNodeInput(t))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if calls != 2 {
		t.Fatalf("agent calls = %d, want 2 (one retry)", calls)
	}
	if turnBudgets[0] != 90 {
		t.Fatalf("first attempt MaxTurns = %d, want 90 (the profile budget)", turnBudgets[0])
	}
	if turnBudgets[1] != 45 {
		t.Fatalf("retry MaxTurns = %d, want 45 (90/2)", turnBudgets[1])
	}
	if !strings.Contains(string(out.Data), "Retry count is unbounded") {
		t.Fatalf("retry output = %s, want the explore summary", out.Data)
	}
}

// A non context_too_long class is a real failure: it is not retried and the
// partial lane is still downgraded from the single attempt.
func TestExploreDoesNotRetryNonContextTooLong(t *testing.T) {
	profile := lanesProfile()
	profile.Config.Budget.MaxTurns = 90

	var mu sync.Mutex
	calls := 0
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:     profile,
		Dossier:     validDossier(t),
		MaxAttempts: 1,
		AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			decoded, err := spec.Submit.Decode(exploreOutJSON(t))
			if err != nil {
				return nil, err
			}
			return &agent.RunResult{Output: decoded, Usage: llm.Usage{InputTokens: 10}, Turns: 3, Partial: true}, agent.ErrNoSubmit
		},
	})

	out, err := executor.Execute(context.Background(), exploreNodeInput(t))
	if err != nil {
		t.Fatalf("Execute returned a hard failure instead of a partial lane: %v", err)
	}
	if calls != 1 {
		t.Fatalf("agent calls = %d, want 1 (no retry for a non context_too_long class)", calls)
	}
	partial := decodePartialLaneOutput(t, out)
	if partial.Partial.ErrorKind != noSubmitClass {
		t.Fatalf("partial error kind = %q, want %q", partial.Partial.ErrorKind, noSubmitClass)
	}
}

// With no profile MaxTurns the retry inherits the agent default (zero) and
// floors at the lower bound of one turn.
func TestExploreRetryMaxTurnsFloorsAtOne(t *testing.T) {
	if got := exploreRetryMaxTurns(90); got != 45 {
		t.Fatalf("exploreRetryMaxTurns(90) = %d, want 45", got)
	}
	if got := exploreRetryMaxTurns(1); got != 1 {
		t.Fatalf("exploreRetryMaxTurns(1) = %d, want 1", got)
	}
	if got := exploreRetryMaxTurns(0); got != 1 {
		t.Fatalf("exploreRetryMaxTurns(0) = %d, want 1", got)
	}
	if got := exploreRetryMaxTurns(-5); got != 1 {
		t.Fatalf("exploreRetryMaxTurns(-5) = %d, want 1", got)
	}
}