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

// ignoredPartialDossier uses the default-style generated-file pattern of the
// reported failure: a recovered finding on such a path must never reach the
// publisher, which rejects the whole review (#513).
func ignoredPartialDossier(t *testing.T) *Dossier {
	t.Helper()
	d := validDossier(t)
	d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{"**/*.pb.go"}}
	return d
}

func hasIgnoredGap(gaps []string) bool {
	for _, gap := range gaps {
		if strings.Contains(gap, "ignored") {
			return true
		}
	}
	return false
}

// A review lane that stopped before submit_result keeps its decodable result,
// but findings on ignored paths are dropped and disclosed as an evidence gap.
func TestPartialReviewLaneDropsIgnoredFindings(t *testing.T) {
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:     lanesProfile(),
		Dossier:     ignoredPartialDossier(t),
		MaxAttempts: 1,
		AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			return &agent.RunResult{
				Output: &ReviewOut{
					Verdict:        "BLOCKING",
					EvidenceStatus: "sufficient",
					Findings: []FindingOut{
						finding("api/x.pb.go", 10, SeverityP1, "generated code finding"),
						finding("a.go", 20, SeverityP2, "kept finding"),
					},
					EvidenceGaps: []string{"runtime evidence missing"},
					Summary:      "stored submit rejected",
				},
				Usage:   llm.Usage{InputTokens: 100, OutputTokens: 10},
				Turns:   9,
				Partial: true,
			}, agent.ErrNoSubmit
		},
	})
	out, err := executor.Execute(context.Background(), laneNodeInput(t))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	partial := decodePartialLaneOutput(t, out)
	if len(partial.Findings) != 1 || partial.Findings[0].Path != "a.go" {
		t.Fatalf("findings = %+v, want only the non-ignored finding", partial.Findings)
	}
	if len(partial.EvidenceGaps) != 2 || partial.EvidenceGaps[0] != "runtime evidence missing" || !hasIgnoredGap(partial.EvidenceGaps) {
		t.Fatalf("evidence gaps = %+v, want the stored gap and the dropped-finding note", partial.EvidenceGaps)
	}
	wf := evidenceWorkflow(completedNode("rv.p1", KindReview,
		reviewNodeParams{Package: "p1", Files: []string{"a.go"}, Focus: "f", Provider: "local"}, partial))
	gathered := runEvidenceGather(t, partialLaneExecutor(t, wf), wf)
	if len(gathered.Groups) != 1 || gathered.Groups[0].Path != "a.go" {
		t.Fatalf("gathered groups = %+v, want only the kept finding", gathered.Groups)
	}
	if len(gathered.Unresolved) != 1 {
		t.Fatalf("unresolved = %+v, want the partial-lane evidence gap", gathered.Unresolved)
	}
}

// The salvage path of a validate lane reads the rejected submit_result from the
// transcript; findings on ignored paths are dropped there as well.
func TestPartialValidateLaneDropsIgnoredSalvagedFindings(t *testing.T) {
	rejected := json.RawMessage(`{
		"status":"MAYBE",
		"checks":[],
		"findings":[
			{"path":"api/x.pb.go","line":4,"severity":"P2","category":"correctness","title":"generated finding","body":"b","evidence":"e","confidence":"high"},
			{"path":"a.go","line":8,"severity":"P2","category":"correctness","title":"kept finding","body":"b","evidence":"e","confidence":"high"}
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
		Dossier:     ignoredPartialDossier(t),
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
	if len(partial.Findings) != 1 || partial.Findings[0].Path != "a.go" {
		t.Fatalf("salvaged findings = %+v, want only the non-ignored finding", partial.Findings)
	}
	if !hasIgnoredGap(partial.EvidenceGaps) {
		t.Fatalf("evidence gaps = %+v, want the dropped-finding note", partial.EvidenceGaps)
	}
}

// When every recovered finding sits on an ignored path, nothing is left to
// carry forward and the lane keeps its original hard failure.
func TestPartialLaneWithOnlyIgnoredFindingsKeepsFailure(t *testing.T) {
	rejected := json.RawMessage(`{
		"status":"MAYBE",
		"checks":[],
		"findings":[
			{"path":"api/x.pb.go","line":4,"severity":"P2","category":"correctness","title":"generated finding","body":"b","evidence":"e","confidence":"high"}
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
		Dossier:     ignoredPartialDossier(t),
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
	var partial *PartialRunError
	if !errors.As(err, &partial) {
		t.Fatalf("error = %v, want the original PartialRunError", err)
	}
	if len(out.Data) != 0 {
		t.Fatalf("output = %s, want empty", out.Data)
	}
}

// A validate lane whose normal submit points at an ignored path keeps the
// existing hard-failure semantics: the lane fails before anything is published.
func TestValidateLaneNormalSubmitWithIgnoredFindingFails(t *testing.T) {
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:     lanesProfile(),
		Dossier:     ignoredPartialDossier(t),
		MaxAttempts: 1,
		AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			return &agent.RunResult{
				Output: &ValidateOut{
					Status:   "PASS",
					Checks:   []ValidateCheck{},
					Findings: []FindingOut{finding("api/x.pb.go", 10, SeverityP2, "generated")},
					Summary:  "ok",
				},
				Usage: llm.Usage{InputTokens: 30, OutputTokens: 3},
			}, nil
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
	if !errors.Is(err, agent.ErrIgnoredPath) {
		t.Fatalf("error = %v, want ErrIgnoredPath", err)
	}
	if len(out.Data) != 0 {
		t.Fatalf("output = %s, want empty", out.Data)
	}
}

// In the lanes flow a code-review lane that stopped before submit_result is
// downgraded like the other lanes: the recovered findings survive (minus the
// ignored ones) and gather keeps an evidence gap (#527).
func TestPartialCodeReviewLaneDowngradesInLanesFlow(t *testing.T) {
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:     lanesProfile(),
		Dossier:     ignoredPartialDossier(t),
		MaxAttempts: 1,
		AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			return &agent.RunResult{
				Output: &CodeReviewOut{
					Summary: "stopped before submit",
					Findings: []FindingOut{
						finding("api/x.pb.go", 3, SeverityP2, "generated"),
						finding("a.go", 7, SeverityP1, "kept"),
					},
				},
				Usage:   llm.Usage{InputTokens: 500, OutputTokens: 40},
				Turns:   12,
				Partial: true,
			}, agent.ErrNoSubmit
		},
	})
	out, err := executor.Execute(context.Background(), codeReviewInput(t))
	if err != nil {
		t.Fatalf("lanes code-review lane failed instead of downgrading: %v", err)
	}
	partial := decodePartialLaneOutput(t, out)
	if partial.Partial.ErrorKind != noSubmitClass || partial.Partial.Turns != 12 {
		t.Fatalf("partial info = %+v", partial.Partial)
	}
	if len(partial.Findings) != 1 || partial.Findings[0].Path != "a.go" {
		t.Fatalf("recovered findings = %+v, want only the non-ignored finding", partial.Findings)
	}
	wf := evidenceWorkflow(completedNode("cr.a", KindCodeReview,
		reviewNodeParams{Package: "a", Files: []string{"a.go"}, Focus: "f", Provider: "local"}, partial))
	gathered := runEvidenceGather(t, partialLaneExecutor(t, wf), wf)
	if len(gathered.Groups) != 1 || gathered.Groups[0].Path != "a.go" {
		t.Fatalf("gathered groups = %+v, want only the kept finding", gathered.Groups)
	}
	if len(gathered.Unresolved) != 1 || !strings.Contains(gathered.Unresolved[0].Message, "recovered_findings=1") {
		t.Fatalf("unresolved = %+v, want the partial-lane evidence gap", gathered.Unresolved)
	}
}

// The legacy flow requires the code-review node; downgrading a partial run
// there would turn a missing required lane into a soft completion, so the
// hard failure must stay.
func TestPartialCodeReviewLaneStaysFailedInLegacyFlow(t *testing.T) {
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:     defaultProfile(),
		Dossier:     validDossier(t),
		MaxAttempts: 1,
		AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			return &agent.RunResult{
				Output: &CodeReviewOut{
					Summary:  "stopped before submit",
					Findings: []FindingOut{finding("a.go", 1, SeverityP1, "kept")},
				},
				Usage:   llm.Usage{InputTokens: 10},
				Turns:   4,
				Partial: true,
			}, agent.ErrNoSubmit
		},
	})
	out, err := executor.Execute(context.Background(), codeReviewInput(t))
	if err == nil {
		t.Fatal("legacy code-review lane was downgraded, want the hard failure")
	}
	var partial *PartialRunError
	if !errors.As(err, &partial) {
		t.Fatalf("error = %v, want PartialRunError", err)
	}
	if len(out.Data) != 0 {
		t.Fatalf("output = %s, want empty", out.Data)
	}
}
