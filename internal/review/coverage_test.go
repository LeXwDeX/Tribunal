package review

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/pkg/dag"
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
	cases := []struct {
		name   string
		mutate func(*Dossier)
		files  []string
	}{
		{
			name: "oversized file",
			mutate: func(d *Dossier) {
				d.Files[0].Diff = "@@ -1 +1 @@\n+" + strings.Repeat("x", MaxTaskRunes+1000)
			},
			files: []string{"a.go"},
		},
		{
			name: "inline budget omits the tail",
			mutate: func(d *Dossier) {
				files := make([]FileChange, 0, 20)
				for i := 0; i < 20; i++ {
					files = append(files, FileChange{
						Path: fmt.Sprintf("f%02d.go", i), Status: "modified",
						Diff: "@@ -1 +1 @@\n+" + strings.Repeat("y", maxFileDiffRunes), Additions: 1,
					})
				}
				d.Files = files
				d.DiffSummary = DiffSummary{Files: len(files)}
			},
			files: func() []string {
				files := make([]string, 0, 20)
				for i := 0; i < 20; i++ {
					files = append(files, fmt.Sprintf("f%02d.go", i))
				}
				return files
			}(),
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
	diff := "@@ -1 +1 @@\n+" + strings.Repeat("x", MaxTaskRunes+1000)
	dossier.Files = nil
	files := []string{"f0.go"}
	dossier.Files = append(dossier.Files, FileChange{
		Path: "f0.go", Status: "modified", Diff: diff, Additions: 1,
	})
	dossier.DiffSummary = DiffSummary{Files: len(files)}
	wf := coverageWorkflow(files...)
	executor := evidenceExecutorWithDossier(t, wf, dossier)
	gathered := runEvidenceGather(t, executor, wf)
	coverageSignals := 0
	var taskLimit string
	for _, signal := range gathered.Unresolved {
		if signal.Kind == UnresolvedEvidence && strings.Contains(signal.Message, "f0.go") {
			coverageSignals++
			taskLimit = signal.Message
		}
	}
	if coverageSignals != 1 {
		t.Fatalf("oversized-file coverage signals = %d, want exactly one: %+v", coverageSignals, gathered.Unresolved)
	}
	if !strings.Contains(taskLimit, "f0.go") {
		t.Fatalf("coverage message %q does not list the omitted path", taskLimit)
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

func contentFreeDossier(t *testing.T, status string) *Dossier {
	t.Helper()
	d := validDossier(t)
	d.Files = append(d.Files, FileChange{Path: "billing/__init__.py", OldPath: "billing/old.py", Status: status})
	d.DiffSummary.Files = len(d.Files)
	return d
}

func TestExecutorCoverageContentFreeFileIsNotAGap(t *testing.T) {
	for _, status := range []string{"added", "deleted", "renamed"} {
		t.Run(status, func(t *testing.T) {
			dossier := contentFreeDossier(t, status)
			wf := coverageWorkflow("a.go", "b.go", "billing/__init__.py")
			executor := evidenceExecutorWithDossier(t, wf, dossier)
			gathered := runEvidenceGather(t, executor, wf)
			if signal, ok := unresolvedOfKind(gathered.Unresolved, UnresolvedEvidence); ok {
				t.Fatalf("content-free %s file produced a coverage gap: %+v", status, signal)
			}
			found := false
			for _, w := range gathered.Warnings {
				if strings.Contains(w, "no textual changes to review") && strings.Contains(w, "billing/__init__.py") {
					found = true
				}
			}
			if !found {
				t.Fatalf("warnings do not list the content-free path: %v", gathered.Warnings)
			}
			adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
			result := runEvidenceSynthesis(t, executor, wf, adjudicated)
			if result.Verdict != VerdictPass {
				t.Fatalf("verdict = %q, want PASS", result.Verdict)
			}
		})
	}
}

func TestExecutorCoverageModifiedEmptyDiffStaysGap(t *testing.T) {
	dossier := contentFreeDossier(t, "modified")
	wf := coverageWorkflow("a.go", "billing/__init__.py")
	executor := evidenceExecutorWithDossier(t, wf, dossier)
	gathered := runEvidenceGather(t, executor, wf)
	signal, ok := unresolvedOfKind(gathered.Unresolved, UnresolvedEvidence)
	if !ok || !strings.Contains(signal.Message, "billing/__init__.py") {
		t.Fatalf("modified empty diff must stay a gap: %+v", gathered.Unresolved)
	}
	for _, w := range gathered.Warnings {
		if strings.Contains(w, "no textual changes to review") {
			t.Fatalf("modified empty diff must not be listed as content-free: %v", gathered.Warnings)
		}
	}
	result := runEvidenceSynthesis(t, executor, wf, runEvidenceAdjudicate(t, executor, wf, gathered))
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE", result.Verdict)
	}
}

func TestPromptDiffGapsContentFree(t *testing.T) {
	d := contentFreeDossier(t, "added")
	gaps, gapped := promptDiffGaps(d, []string{"billing/__init__.py"})
	if len(gaps) != 0 || len(gapped) != 0 {
		t.Fatalf("gaps = %v, gapped = %v", gaps, gapped)
	}
	d.Files[2].Status = "modified"
	if gaps, _ := promptDiffGaps(d, []string{"billing/__init__.py"}); len(gaps) != 1 {
		t.Fatalf("modified empty diff gaps = %v", gaps)
	}
}

func TestContentFreeIndexLine(t *testing.T) {
	d := contentFreeDossier(t, "added")
	d.Files = append(d.Files, FileChange{Path: "m.py", Status: "modified"})
	index, _, _, _ := renderDiffsWithSpans(d, []string{"billing/__init__.py", "m.py"}, 10000)
	if !strings.Contains(index, "billing/__init__.py（added，+0/-0，无文本变更，无需评审）") {
		t.Fatalf("index does not mark the content-free file: %s", index)
	}
	if !strings.Contains(index, "m.py（modified，+0/-0，无 diff 内容）") {
		t.Fatalf("modified empty diff lost its marker: %s", index)
	}
}
