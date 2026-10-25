package review

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/agent"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

func coverageWorkflow(files ...string) dag.WorkflowView {
	return evidenceWorkflow(
		completedNode("rv.std.p1", KindReviewStd, reviewNodeParams{
			Package: "p1", Channel: KindReviewStd, Files: files, Focus: "review", Provider: "local",
		}, ReviewOut{
			Verdict: "PASS", EvidenceStatus: "sufficient", Findings: []FindingOut{},
			EvidenceGaps: []string{}, Summary: "clean",
		}),
		evidenceGateNode("rv.gate.p1", ReviewGateOut{
			Verdict: "PASS", Accepted: []Ref{}, Rebutted: []Rebut{}, Summary: "clean",
		}),
	)
}

func TestExecutorCoverageTruncatedCollectionCannotPass(t *testing.T) {
	dossier := validDossier(t)
	dossier.DiffSummary.Truncated = true
	wf := coverageWorkflow("a.go", "b.go")
	executor := evidenceExecutorWithDossier(t, wf, dossier)
	gathered := runEvidenceGather(t, executor, wf)
	signal, ok := unresolvedOfKind(gathered.Unresolved, UnresolvedEvidence)
	if !ok {
		t.Fatalf("gather unresolved = %+v", gathered.Unresolved)
	}
	if !strings.Contains(signal.Message, "truncated") ||
		!strings.Contains(signal.Message, "2") {
		t.Fatalf("coverage message does not state the scope: %q", signal.Message)
	}
	adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
	result := runEvidenceSynthesis(t, executor, wf, adjudicated)
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE", result.Verdict)
	}
	if !strings.Contains(result.Summary, "truncated") {
		t.Fatalf("summary does not state the coverage gap: %q", result.Summary)
	}
	if result.Output.Verdict != VerdictPass {
		t.Fatalf("raw settlement = %q", result.Output.Verdict)
	}
}

func TestExecutorCoveragePromptTruncation(t *testing.T) {
	longDiff := strings.Repeat("x", maxFileDiffRunes+10)
	cases := []struct {
		name   string
		mutate func(*Dossier)
		files  []string
	}{
		{
			name: "per-file truncation",
			mutate: func(d *Dossier) {
				d.Files[0].Diff = longDiff
			},
			files: []string{"a.go"},
		},
		{
			name: "task total truncation",
			mutate: func(d *Dossier) {
				d.Files[0].Diff = strings.Repeat("y", maxTaskFileRunes/2+1)
				d.Files[1].Diff = strings.Repeat("z", maxTaskFileRunes/2+1)
			},
			files: []string{"a.go", "b.go"},
		},
		{
			name:   "file absent from materials",
			mutate: func(*Dossier) {},
			files:  []string{"a.go", "missing.go"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dossier := validDossier(t)
			tc.mutate(dossier)
			wf := coverageWorkflow(tc.files...)
			executor := evidenceExecutorWithDossier(t, wf, dossier)
			gathered := runEvidenceGather(t, executor, wf)
			if _, ok := unresolvedOfKind(gathered.Unresolved, UnresolvedEvidence); !ok {
				t.Fatalf("gather unresolved = %+v", gathered.Unresolved)
			}
			adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
			result := runEvidenceSynthesis(t, executor, wf, adjudicated)
			if result.Verdict != VerdictIncomplete {
				t.Fatalf("verdict = %q, want INCOMPLETE", result.Verdict)
			}
		})
	}
}

func TestExecutorCoverageCompleteDiffHasNoGap(t *testing.T) {
	wf := coverageWorkflow("a.go", "b.go")
	executor := evidenceExecutor(t, wf)
	gathered := runEvidenceGather(t, executor, wf)
	if _, ok := unresolvedOfKind(gathered.Unresolved, UnresolvedEvidence); ok {
		t.Fatalf("complete diff produced a coverage gap: %+v", gathered.Unresolved)
	}
}

func TestExecutorAgentSynthesisCannotClearCoverageGap(t *testing.T) {
	executor := evidenceAgentSynthesizer(t, func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		raw, _ := json.Marshal(SynthesizeOut{
			Verdict: VerdictPass, Summary: "all good", Decisions: []Decision{},
			NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{},
		})
		decoded, err := spec.Submit.Decode(raw)
		if err != nil {
			return nil, err
		}
		return &agent.RunResult{Output: decoded}, nil
	})
	adjudicated := AdjudicateOut{
		Groups: []Group{},
		Unresolved: []UnresolvedSignal{{
			Node: "coverage", Kind: UnresolvedEvidence,
			Message: "diff collection truncated: 2 files included; at least one changed file or line was omitted",
		}},
	}
	adjudicatedRaw, _ := json.Marshal(adjudicated)
	params, _ := json.Marshal(synthesizeParams{Mode: "agent", Provider: "claude", OnUncertain: string(OnUncertainMark)})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: "wf-coverage-agent",
		Node:       dag.NodeSpec{ID: NodeSynth, Kind: KindSynthesize, Params: params},
		Attempt:    1,
		Inputs:     map[string]json.RawMessage{"adjudicate": adjudicatedRaw},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := decodeSynthesisResult(out.Data)
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE", result.Verdict)
	}
	if strings.Contains(result.Summary, "all good") {
		t.Fatalf("agent summary masked the coverage gap: %q", result.Summary)
	}
}

func TestExecutorCoverageIgnoredFilesAreAllowed(t *testing.T) {
	dossier := validDossier(t)
	dossier.Ignored = []string{"ignored.go"}
	wf := coverageWorkflow("a.go", "b.go")
	executor := evidenceExecutorWithDossier(t, wf, dossier)
	gathered := runEvidenceGather(t, executor, wf)
	if _, ok := unresolvedOfKind(gathered.Unresolved, UnresolvedEvidence); ok {
		t.Fatalf("ignore rules produced a coverage gap: %+v", gathered.Unresolved)
	}
}

func TestExecutorCoverageTaskLimitWithoutPerFileTruncation(t *testing.T) {
	dossier := validDossier(t)
	diff := strings.Repeat("x", maxFileDiffRunes)
	dossier.Files = nil
	files := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("f%d.go", i)
		files = append(files, name)
		dossier.Files = append(dossier.Files, FileChange{
			Path: name, Status: "modified", Diff: diff, Additions: 1,
		})
	}
	dossier.DiffSummary = DiffSummary{Files: len(files)}
	wf := coverageWorkflow(files...)
	executor := evidenceExecutorWithDossier(t, wf, dossier)
	gathered := runEvidenceGather(t, executor, wf)
	coverageSignals := 0
	var taskLimit string
	for _, signal := range gathered.Unresolved {
		if signal.Kind == UnresolvedEvidence && strings.Contains(signal.Message, "prompt limit") {
			coverageSignals++
			taskLimit = signal.Message
		}
	}
	if coverageSignals != 1 {
		t.Fatalf("task-limit coverage signals = %d, want exactly one: %+v", coverageSignals, gathered.Unresolved)
	}
	if !strings.Contains(taskLimit, "f7.go") || !strings.Contains(taskLimit, "1 file(s) were omitted") {
		t.Fatalf("task-limit message %q does not list the omitted path", taskLimit)
	}
	if strings.Contains(taskLimit, "later files") {
		t.Fatalf("task-limit message does not list every omitted path: %q", taskLimit)
	}
	adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
	result := runEvidenceSynthesis(t, executor, wf, adjudicated)
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE", result.Verdict)
	}
}

func TestExecutorCoverageCollectionGapNamesCountsAndReasons(t *testing.T) {
	dossier := validDossier(t)
	dossier.DiffSummary = DiffSummary{
		Total: 5, Files: 2, Truncated: true,
		Omitted: []OmittedFile{
			{Path: "big.bin", Reason: "too_large"},
			{Path: "c.go", Reason: "max_files"},
			{Path: "d.go", Reason: "max_diff_lines"},
		},
	}
	wf := coverageWorkflow("a.go", "b.go")
	executor := evidenceExecutorWithDossier(t, wf, dossier)
	gathered := runEvidenceGather(t, executor, wf)
	signal, ok := unresolvedOfKind(gathered.Unresolved, UnresolvedEvidence)
	if !ok {
		t.Fatalf("gather unresolved = %+v", gathered.Unresolved)
	}
	for _, want := range []string{"5", "2", "3", "too_large", "max_files", "max_diff_lines", "big.bin", "c.go", "d.go"} {
		if !strings.Contains(signal.Message, want) {
			t.Fatalf("coverage message %q does not contain %q", signal.Message, want)
		}
	}
	if strings.Contains(signal.Message, "actually reviewed") {
		t.Fatalf("coverage message treats included files as reviewed: %q", signal.Message)
	}
}

func TestExecutorCoverageLegacyTruncatedFailsClosed(t *testing.T) {
	dossier := validDossier(t)
	dossier.DiffSummary.Truncated = true
	wf := coverageWorkflow("a.go", "b.go")
	executor := evidenceExecutorWithDossier(t, wf, dossier)
	gathered := runEvidenceGather(t, executor, wf)
	signal, ok := unresolvedOfKind(gathered.Unresolved, UnresolvedEvidence)
	if !ok {
		t.Fatalf("gather unresolved = %+v", gathered.Unresolved)
	}
	if !strings.Contains(signal.Message, "not recorded") ||
		!strings.Contains(signal.Message, "not fully known") {
		t.Fatalf("legacy truncation did not fail closed: %q", signal.Message)
	}
}

func TestExecutorCoverageUnassignedIncludedFiles(t *testing.T) {
	dossier := validDossier(t)
	wf := coverageWorkflow("a.go")
	plan := PlanOut{
		Risk: "low", RiskReasons: []string{"small"},
		Packages: []Package{{
			ID: "p1", Blocks: []string{"review"}, Files: []string{"a.go"}, Focus: "review",
		}},
		ChallengeOnly: []Severity{SeverityP1},
	}
	wf.Nodes[NodePlan] = completedNode(NodePlan, KindPlan, planParams{}, plan)
	wf.Order = append([]string{NodePlan}, wf.Order...)
	executor := evidenceExecutorWithDossier(t, wf, dossier)
	gathered := runEvidenceGather(t, executor, wf)
	var found bool
	for _, signal := range gathered.Unresolved {
		if signal.Kind == UnresolvedEvidence && strings.Contains(signal.Message, "b.go") &&
			strings.Contains(signal.Message, "never assigned") {
			found = true
		}
	}
	if !found {
		t.Fatalf("unassigned included file was not disclosed: %+v", gathered.Unresolved)
	}
}

func TestExecutorFallbackCannotClearCoverageGap(t *testing.T) {
	executor := evidenceAgentSynthesizer(t, func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, agent.ErrNoSubmit
	})
	adjudicated := AdjudicateOut{
		Groups: []Group{},
		Unresolved: []UnresolvedSignal{{
			Node: "coverage", Kind: UnresolvedEvidence,
			Message: "diff collection omitted 2 of 5 changed file(s): 2 included in the materials snapshot",
		}},
	}
	adjudicatedRaw, _ := json.Marshal(adjudicated)
	params, _ := json.Marshal(synthesizeParams{Mode: "agent", Provider: "claude", OnUncertain: string(OnUncertainMark)})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: "wf-coverage-fallback",
		Node:       dag.NodeSpec{ID: NodeSynth, Kind: KindSynthesize, Params: params},
		Attempt:    1,
		Inputs:     map[string]json.RawMessage{"adjudicate": adjudicatedRaw},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := decodeSynthesisResult(out.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Fallback {
		t.Fatalf("expected the deterministic fallback, got %+v", result)
	}
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("fallback verdict = %q, want INCOMPLETE", result.Verdict)
	}
	if !strings.Contains(result.Summary, "omitted 2 of 5") {
		t.Fatalf("fallback summary masked the coverage gap: %q", result.Summary)
	}
}
