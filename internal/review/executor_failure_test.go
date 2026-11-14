package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func laneNodeInput(t *testing.T) dag.NodeInput {
	t.Helper()
	params, err := json.Marshal(reviewNodeParams{
		Package: "p1", Files: []string{"a.go"}, Focus: "f", Provider: "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	return dag.NodeInput{
		WorkflowID: "wf",
		Node:       dag.NodeSpec{ID: "rv.p1", Kind: KindReview, Params: params},
	}
}

func passingReviewOut(spec agent.RunSpec) (*agent.RunResult, error) {
	decoded, err := spec.Submit.Decode(json.RawMessage(
		`{"verdict":"PASS","evidence_status":"sufficient","findings":[],"evidence_gaps":[],"summary":"ok"}`))
	return &agent.RunResult{Output: decoded}, err
}

// The executor must expose the workflow deadline to the agent loop; otherwise
// nearDeadline never fires and the workflow timer hard-cancels the node.
func TestExecutorScopesNodeToWorkflowDeadline(t *testing.T) {
	deadline := time.Now().Add(45 * time.Minute)
	var observed time.Time
	executor := newTestExecutor(t, ExecutorDeps{
		Profile: lanesProfile(),
		Dossier: validDossier(t),
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) {
			return dag.WorkflowView{Deadline: deadline}, nil
		},
		AgentRun: func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			observed, _ = ctx.Deadline()
			return passingReviewOut(spec)
		},
	})
	in := laneNodeInput(t)
	// Review lanes carry no node-level deadline (#451), which is the shape that
	// made the missing workflow deadline observable in production.
	in.Node.Timeout = DefaultReviewTimeout
	if _, err := executor.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !observed.Equal(deadline) {
		t.Fatalf("agent context deadline = %v, want the workflow deadline %v", observed, deadline)
	}
}

// A node-level timeout that expires before the workflow deadline still wins, so
// the deadline propagation cannot extend configured node bounds.
func TestExecutorKeepsEarlierNodeTimeout(t *testing.T) {
	workflowDeadline := time.Now().Add(45 * time.Minute)
	nodeTimeout := 5 * time.Minute
	var observed time.Time
	executor := newTestExecutor(t, ExecutorDeps{
		Profile: lanesProfile(),
		Dossier: validDossier(t),
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) {
			return dag.WorkflowView{Deadline: workflowDeadline}, nil
		},
		AgentRun: func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			observed, _ = ctx.Deadline()
			return passingReviewOut(spec)
		},
	})
	in := laneNodeInput(t)
	in.Node.Timeout = nodeTimeout
	start := time.Now()
	if _, err := executor.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if observed.Before(start.Add(nodeTimeout-time.Second)) || observed.After(start.Add(nodeTimeout+time.Second)) {
		t.Fatalf("agent context deadline = %v, want the node timeout", observed)
	}
}

// Every failed node produces exactly one WARN carrying identifiers, the fixed
// class, turns and usage, and never the upstream error text.
func TestExecutorWarnsOnFailedNodeWithoutErrorText(t *testing.T) {
	const canary = "upstream-private-canary"
	var logs bytes.Buffer
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:     lanesProfile(),
		Dossier:     validDossier(t),
		MaxAttempts: 1,
		Logger:      slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			return &agent.RunResult{
					Usage:   llm.Usage{InputTokens: 1950000, OutputTokens: 1200},
					Turns:   30,
					Partial: true,
				},
				fmt.Errorf("%w: provider said %s", agent.ErrNoSubmit, canary)
		},
	})
	out, err := executor.Execute(context.Background(), laneNodeInput(t))
	if err == nil {
		t.Fatal("Execute succeeded, want the run failure")
	}
	if out.Usage.InputTokens != 1950000 || out.Usage.OutputTokens != 1200 {
		t.Fatalf("out.Usage = %+v, want the failed run usage", out.Usage)
	}
	var detailed interface{ ErrorDetail() string }
	if !errors.As(err, &detailed) || detailed.ErrorDetail() != "submit_result missing after 30 turns" {
		t.Fatalf("error detail = %v", err)
	}
	logged := logs.String()
	for _, want := range []string{
		"level=WARN", "workflow_id=wf", "node_id=rv.p1", "node_kind=review",
		"error_kind=no_submit", "turns=30", "input_tokens=1950000", "output_tokens=1200",
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log %q does not contain %q", logged, want)
		}
	}
	if strings.Contains(logged, canary) {
		t.Fatalf("log leaked the upstream error text: %q", logged)
	}
	if strings.Count(logged, "level=WARN") != 1 {
		t.Fatalf("want one WARN for the failed node, got %d: %q", strings.Count(logged, "level=WARN"), logged)
	}
}

// A node cancelled with its workflow is not recorded as failed, so it must not
// add a WARN either.
func TestExecutorDoesNotWarnWhenWorkflowCancels(t *testing.T) {
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:     lanesProfile(),
		Dossier:     validDossier(t),
		MaxAttempts: 1,
		Logger:      slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		AgentRun: func(ctx context.Context, _ agent.RunSpec) (*agent.RunResult, error) {
			return nil, ctx.Err()
		},
	})
	if _, err := executor.Execute(ctx, laneNodeInput(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("cancelled node logged a warning: %q", logs.String())
	}
}

// A run that stopped before submit_result is carried as a typed partial error
// so a node layer can turn the collected lane state into an evidence gap.
func TestExecutorCarriesPartialLaneResult(t *testing.T) {
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:     lanesProfile(),
		Dossier:     validDossier(t),
		MaxAttempts: 1,
		AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			return &agent.RunResult{
				Usage:   llm.Usage{InputTokens: 40, OutputTokens: 4, Incomplete: true},
				Turns:   7,
				Partial: true,
			}, agent.ErrNoSubmit
		},
	})
	_, err := executor.Execute(context.Background(), laneNodeInput(t))
	var partial *PartialRunError
	if !errors.As(err, &partial) {
		t.Fatalf("error = %v, want a PartialRunError", err)
	}
	if partial.Class != noSubmitClass || partial.Turns != 7 || !partial.Incomplete {
		t.Fatalf("partial = %+v", partial)
	}
	if partial.Usage.InputTokens != 40 || partial.Usage.OutputTokens != 4 {
		t.Fatalf("partial usage = %+v", partial.Usage)
	}
	if partial.Result == nil || !partial.Result.Partial {
		t.Fatalf("partial result = %+v", partial.Result)
	}
	if got := errorDetail(t, err); got != "submit_result missing after 7 turns" {
		t.Fatalf("detail = %q", got)
	}
}

// deadlineSubmitClient keeps calling an unknown tool until the deadline prompt
// asks for submit_result. Its fake clock advances one turn per request, so the
// test proves the workflow deadline reaches the loop without wall-clock waits.
type deadlineSubmitClient struct {
	clock   time.Time
	step    time.Duration
	prompts int
}

func (c *deadlineSubmitClient) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: "deadline-fake"}
}

func (c *deadlineSubmitClient) now() time.Time { return c.clock }

func (c *deadlineSubmitClient) Generate(_ context.Context, req *llm.Request) (*llm.Response, error) {
	c.clock = c.clock.Add(c.step)
	last := req.Messages[len(req.Messages)-1]
	text := ""
	if last.Role == llm.RoleUser && len(last.Parts) == 1 {
		text = last.Parts[0].Text
	}
	turnEnding := func(call *llm.ToolCall) *llm.Response {
		return &llm.Response{
			Message: llm.Message{Role: llm.RoleAssistant,
				Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: call}}},
			StopReason: llm.StopToolUse,
		}
	}
	if strings.Contains(text, "nearly out of turns") {
		c.prompts++
		payload, err := json.Marshal(ReviewOut{
			Verdict: "PASS", EvidenceStatus: "sufficient",
			Findings: []FindingOut{}, EvidenceGaps: []string{}, Summary: "ok",
		})
		if err != nil {
			return nil, err
		}
		return turnEnding(&llm.ToolCall{ID: "submit", Name: agent.SubmitToolName, Input: payload}), nil
	}
	return turnEnding(&llm.ToolCall{ID: "early", Name: "read_file", Input: json.RawMessage(`{}`)}), nil
}

type fixedClients struct{ client llm.Client }

func (p fixedClients) Client(string) (llm.Client, error) { return p.client, nil }

// Acceptance shape: no request-count limit, a workflow deadline, and a model
// that keeps calling tools. The deadline must reach the agent loop so the run
// submits before the engine hard-cancels the node.
func TestExecutorSubmitsBeforeWorkflowDeadline(t *testing.T) {
	start := time.Now()
	client := &deadlineSubmitClient{clock: start, step: time.Minute}
	executor := newTestExecutor(t, ExecutorDeps{
		Profile: lanesProfile(),
		Dossier: validDossier(t),
		Clients: fixedClients{client: client},
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) {
			return dag.WorkflowView{Deadline: start.Add(10 * time.Minute)}, nil
		},
		Now: client.now,
	})
	in := laneNodeInput(t)
	in.Node.Timeout = DefaultReviewTimeout
	out, err := executor.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if client.prompts == 0 {
		t.Fatal("the run was never asked for submit_result before the deadline")
	}
	var review ReviewOut
	if err := json.Unmarshal(out.Data, &review); err != nil || review.Verdict != "PASS" {
		t.Fatalf("output = %s err=%v, want the submitted review", out.Data, err)
	}
}
