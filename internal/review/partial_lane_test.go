package review

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func partialLaneOutputRaw(t *testing.T, out PartialLaneOut) []byte {
	t.Helper()
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal partial lane output: %v", err)
	}
	return raw
}

func decodePartialLaneOutput(t *testing.T, out dag.NodeOutput) PartialLaneOut {
	t.Helper()
	partial, ok := decodePartialLaneOut(out.Data)
	if !ok {
		t.Fatalf("output is not a partial lane envelope: %s", out.Data)
	}
	return *partial
}

// A decoded lane result that arrives together with a partial failure must be
// submitted with the lane instead of discarded.
func TestPartialReviewLaneKeepsDecodedOutput(t *testing.T) {
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:     lanesProfile(),
		Dossier:     validDossier(t),
		MaxAttempts: 1,
		AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			return &agent.RunResult{
				Output: &ReviewOut{
					Verdict:        "BLOCKING",
					EvidenceStatus: "sufficient",
					Findings:       []FindingOut{finding("a.go", 10, SeverityP1, "unlocked write")},
					EvidenceGaps:   []string{},
					Summary:        "one blocking finding",
				},
				Usage:   llm.Usage{InputTokens: 1950395, OutputTokens: 1200},
				Turns:   30,
				Partial: true,
			}, agent.ErrNoSubmit
		},
	})
	out, err := executor.Execute(context.Background(), laneNodeInput(t))
	if err != nil {
		t.Fatalf("Execute returned a hard failure instead of a partial lane: %v", err)
	}
	if out.Usage.InputTokens != 1950395 || out.Usage.OutputTokens != 1200 {
		t.Fatalf("usage = %+v", out.Usage)
	}
	partial := decodePartialLaneOutput(t, out)
	if partial.Partial.ErrorKind != noSubmitClass || partial.Partial.Turns != 30 {
		t.Fatalf("partial info = %+v", partial.Partial)
	}
	if partial.Partial.Usage.InputTokens != 1950395 {
		t.Fatalf("partial usage = %+v", partial.Partial.Usage)
	}
	if len(partial.Findings) != 1 || partial.Findings[0].Title != "unlocked write" {
		t.Fatalf("findings = %+v", partial.Findings)
	}
}

// A submit attempt that was rejected at the contract level still contains
// findings worth keeping. The salvage reads the last submit_result arguments
// from the transcript and validates each finding on its own.
func TestPartialValidateLaneSalvagesRejectedSubmitFindings(t *testing.T) {
	rejected := json.RawMessage(`{
		"status":"MAYBE",
		"checks":[],
		"findings":[
			{"path":"a.go","line":4,"severity":"P2","category":"correctness","title":"missing nil check","body":"b","evidence":"e","confidence":"high"},
			{"path":"","line":0,"severity":"P9","category":"","title":"","body":"","evidence":"","confidence":""}
		],
		"summary":"partial"
	}`)
	transcript := []llm.Message{{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{{
			Type:     llm.PartToolCall,
			ToolCall: &llm.ToolCall{ID: "call-1", Name: agent.SubmitToolName, Input: rejected},
		}},
	}}
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:     lanesProfile(),
		Dossier:     validDossier(t),
		MaxAttempts: 1,
		AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			return &agent.RunResult{
				Usage:      llm.Usage{InputTokens: 80, OutputTokens: 8},
				Turns:      3,
				Transcript: transcript,
				Partial:    true,
			}, agent.ErrInvalidSubmit
		},
	})
	params, err := json.Marshal(validateParams{Provider: "local"})
	if err != nil {
		t.Fatal(err)
	}
	in := dag.NodeInput{
		WorkflowID: "wf",
		Node:       dag.NodeSpec{ID: NodeValidate, Kind: KindValidate, Params: params},
	}
	out, err := executor.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	partial := decodePartialLaneOutput(t, out)
	if partial.Partial.ErrorKind != invalidSubmitClass || partial.Partial.Turns != 3 {
		t.Fatalf("partial info = %+v", partial.Partial)
	}
	if len(partial.Findings) != 1 || partial.Findings[0].Title != "missing nil check" {
		t.Fatalf("salvaged findings = %+v", partial.Findings)
	}
}

// A partial run without any decodable output keeps the existing hard failure;
// the downgrade must not invent an empty lane.
func TestPartialLaneWithoutRecoverableOutputKeepsFailure(t *testing.T) {
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:     lanesProfile(),
		Dossier:     validDossier(t),
		MaxAttempts: 1,
		AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			return &agent.RunResult{Usage: llm.Usage{InputTokens: 10}, Turns: 2, Partial: true}, agent.ErrNoSubmit
		},
	})
	out, err := executor.Execute(context.Background(), laneNodeInput(t))
	if err == nil {
		t.Fatal("Execute succeeded, want the run failure")
	}
	if len(out.Data) != 0 {
		t.Fatalf("output = %s, want empty", out.Data)
	}
	var partial *PartialRunError
	if !errors.As(err, &partial) {
		t.Fatalf("error = %v, want PartialRunError", err)
	}
}

// Cancellation is fail-closed: the caller owns that outcome and it must not be
// converted into a completed lane.
func TestPartialLaneCancellationIsNotDowngraded(t *testing.T) {
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:     lanesProfile(),
		Dossier:     validDossier(t),
		MaxAttempts: 1,
		AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			return &agent.RunResult{
				Output:  &ReviewOut{Verdict: "PASS", Findings: []FindingOut{}, EvidenceGaps: []string{}, Summary: "ok"},
				Usage:   llm.Usage{InputTokens: 10},
				Turns:   2,
				Partial: true,
			}, context.Canceled
		},
	})
	out, err := executor.Execute(context.Background(), laneNodeInput(t))
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
	if len(out.Data) != 0 {
		t.Fatalf("output = %s, want empty", out.Data)
	}
}

// partialLaneExecutor uses the lanes flow so failed or degraded lanes produce
// evidence gaps, with a fixed dossier snapshot for the locator.
func partialLaneExecutor(t *testing.T, wf dag.WorkflowView) *Executor {
	t.Helper()
	head, base := fixedSnapshot(t)
	dossier := validDossier(t)
	dossier.HeadDir = head
	dossier.BaseDir = base
	return newTestExecutor(t, ExecutorDeps{
		Profile:  lanesProfile(),
		Dossier:  dossier,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		Locate:   func(string, int, Side) bool { return true },
	})
}

func partialValidateNode(t *testing.T, findings ...FindingOut) dag.NodeView {
	t.Helper()
	return completedNode(NodeValidate, KindValidate, validateParams{Provider: "local"}, PartialLaneOut{
		Partial: PartialLaneInfo{
			ErrorKind: noSubmitClass,
			Turns:     30,
			Usage:     dag.Usage{InputTokens: 1950395, OutputTokens: 1200},
		},
		Findings: findings,
	})
}

// The recovered findings enter the vote, while the evidence gap carries the
// fixed class, turns and usage. A P2-only result must not become PASS.
func TestGatherDowngradedLaneFeedsFindingsAndEvidenceGap(t *testing.T) {
	node := partialValidateNode(t,
		finding("a.go", 10, SeverityP2, "slow path"),
		finding("a.go", 20, SeverityInfo, "naming"),
	)
	wf := evidenceWorkflow(node)
	executor := partialLaneExecutor(t, wf)
	gathered := runEvidenceGather(t, executor, wf)
	if len(gathered.Groups) != 2 {
		t.Fatalf("groups = %+v, want the two recovered findings", gathered.Groups)
	}
	if len(gathered.Unresolved) != 1 {
		t.Fatalf("unresolved = %+v", gathered.Unresolved)
	}
	signal := gathered.Unresolved[0]
	for _, want := range []string{
		"error_kind=no_submit", "turns=30", "input_tokens=1950395",
		"output_tokens=1200", "recovered_findings=2",
	} {
		if !strings.Contains(signal.Message, want) {
			t.Fatalf("signal %q does not contain %q", signal.Message, want)
		}
	}
	adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
	result := runEvidenceSynthesis(t, executor, wf, adjudicated)
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE (never PASS)", result.Verdict)
	}
}

// An existing P0/P1 published finding keeps BLOCKING; the evidence gap moves
// into warnings instead of downgrading the verdict (#455).
func TestDowngradedLaneKeepsBlockingFindings(t *testing.T) {
	node := partialValidateNode(t, finding("a.go", 10, SeverityP1, "data race"))
	wf := evidenceWorkflow(node)
	executor := partialLaneExecutor(t, wf)
	gathered := runEvidenceGather(t, executor, wf)
	adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
	result := runEvidenceSynthesis(t, executor, wf, adjudicated)
	if result.Verdict != VerdictBlocking {
		t.Fatalf("verdict = %q, want BLOCKING", result.Verdict)
	}
	if len(result.Unresolved) != 0 {
		t.Fatalf("unresolved = %+v, want the gap in warnings for BLOCKING", result.Unresolved)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("missing evidence-gap warning")
	}
}

// A failed lane without recovered output still gets an attributable gap: the
// fixed class, the sanitized detail and the recorded usage replace the generic
// "did not complete" message.
func TestFailedLaneEvidenceGapCarriesAttribution(t *testing.T) {
	node := completedNode(NodeValidate, KindValidate, validateParams{Provider: "local"}, ValidateOut{})
	node.Status = dag.NodeFailed
	node.Reason = noSubmitClass
	node.Error = "submit_result missing after 30 turns"
	node.Usage = dag.Usage{InputTokens: 1950395, OutputTokens: 1200}
	wf := evidenceWorkflow(node)
	executor := partialLaneExecutor(t, wf)
	gathered := runEvidenceGather(t, executor, wf)
	if len(gathered.Unresolved) != 1 {
		t.Fatalf("unresolved = %+v", gathered.Unresolved)
	}
	for _, want := range []string{
		"error_kind=no_submit", "turns=30", "input_tokens=1950395", "output_tokens=1200",
	} {
		if !strings.Contains(gathered.Unresolved[0].Message, want) {
			t.Fatalf("signal %q does not contain %q", gathered.Unresolved[0].Message, want)
		}
	}
}

// The MR comment localizes the new gap instead of leaking the machine text.
func TestPartialLaneSignalIsLocalized(t *testing.T) {
	signal := partialLaneSignal("validate", KindValidate, PartialLaneInfo{
		ErrorKind: noSubmitClass, Turns: 30,
		Usage: dag.Usage{InputTokens: 1950395, OutputTokens: 1200},
	}, 2)
	text, ok := localizeMachineMessage(signal.Message)
	if !ok {
		t.Fatalf("message %q was not localized", signal.Message)
	}
	if !strings.Contains(text, "验证评审") || !strings.Contains(text, "30 轮") || !strings.Contains(text, "2 条问题") {
		t.Fatalf("localized text = %q", text)
	}
}
