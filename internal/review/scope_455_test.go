package review

import (
	"fmt"
	"strings"
	"testing"
)

func TestApplyUnresolvedPreservesBlockingVerdict(t *testing.T) {
	output := SynthesizeOut{
		Verdict: VerdictBlocking, Summary: "P0 found", Decisions: []Decision{},
		NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{},
	}
	result := &SynthesisResult{
		Verdict: VerdictBlocking, Summary: "P0 found", Groups: []Group{}, Output: output,
	}
	applyUnresolved(result, []UnresolvedSignal{{
		Node: "coverage", Kind: UnresolvedEvidence, Message: "one file was not reviewed",
	}})
	if result.Verdict != VerdictBlocking {
		t.Fatalf("verdict = %q, want BLOCKING preserved", result.Verdict)
	}
	// The contract only allows unresolved records with INCOMPLETE.
	if len(result.Unresolved) != 0 {
		t.Fatalf("unresolved = %+v, want none for BLOCKING", result.Unresolved)
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "one file was not reviewed") {
		t.Fatalf("warnings = %v, want the unresolved gap disclosed", result.Warnings)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("blocking result with gaps must stay valid: %v", err)
	}
}

func TestApplyUnresolvedEscalatesPassToIncomplete(t *testing.T) {
	output := SynthesizeOut{
		Verdict: VerdictPass, Summary: "clean", Decisions: []Decision{},
		NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{},
	}
	result := &SynthesisResult{Verdict: VerdictPass, Summary: "clean", Groups: []Group{}, Output: output}
	applyUnresolved(result, []UnresolvedSignal{{
		Node: "coverage", Kind: UnresolvedEvidence, Message: "diff collection omitted a file",
	}})
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE", result.Verdict)
	}
	if len(result.Unresolved) != 1 || !strings.Contains(result.Summary, "diff collection omitted a file") {
		t.Fatalf("unresolved/summary = %+v / %q", result.Unresolved, result.Summary)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("incomplete result must stay valid: %v", err)
	}
}

func TestExecutorCoverageWithheldFileIsNotIncompleteAlone(t *testing.T) {
	dossier := validDossier(t)
	dossier.DiffSummary = DiffSummary{
		Files: 2, Total: 3, Truncated: true,
		Omitted: []OmittedFile{{Path: "css/big.css", Reason: "too_large"}},
	}
	wf := coverageWorkflow("a.go", "b.go")
	executor := evidenceExecutorWithDossier(t, wf, dossier)
	gathered := runEvidenceGather(t, executor, wf)
	if signal, ok := unresolvedOfKind(gathered.Unresolved, UnresolvedEvidence); ok {
		t.Fatalf("a single withheld file must not force unresolved: %+v", signal)
	}
	adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
	result := runEvidenceSynthesis(t, executor, wf, adjudicated)
	if result.Verdict != VerdictPass {
		t.Fatalf("verdict = %q, want PASS; summary = %q", result.Verdict, result.Summary)
	}
}

func TestExecutorCoverageHardOmissionStillIncomplete(t *testing.T) {
	dossier := validDossier(t)
	dossier.DiffSummary = DiffSummary{
		Files: 2, Total: 3, Truncated: true,
		Omitted: []OmittedFile{{Path: "late.go", Reason: "max_files"}},
	}
	wf := coverageWorkflow("a.go", "b.go")
	executor := evidenceExecutorWithDossier(t, wf, dossier)
	gathered := runEvidenceGather(t, executor, wf)
	signal, ok := unresolvedOfKind(gathered.Unresolved, UnresolvedEvidence)
	if !ok || !strings.Contains(signal.Message, "max_files") || !strings.Contains(signal.Message, "late.go") {
		t.Fatalf("hard omission signal = %+v / %v", signal, gathered.Unresolved)
	}
	adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
	result := runEvidenceSynthesis(t, executor, wf, adjudicated)
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE", result.Verdict)
	}
}

func TestExecutorCoverageGitLabTruncationCannotPass(t *testing.T) {
	dossier := validDossier(t)
	dossier.DiffSummary = DiffSummary{Files: 2, Total: 9, Truncated: true}
	wf := coverageWorkflow("a.go", "b.go")
	executor := evidenceExecutorWithDossier(t, wf, dossier)
	gathered := runEvidenceGather(t, executor, wf)
	signal, ok := unresolvedOfKind(gathered.Unresolved, UnresolvedEvidence)
	if !ok {
		t.Fatalf("GitLab-side truncation must produce an evidence gap: %+v", gathered.Unresolved)
	}
	if !strings.Contains(signal.Message, "not delivered") || !strings.Contains(signal.Message, "9") {
		t.Fatalf("truncation message does not state the missing scope: %q", signal.Message)
	}
	adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
	result := runEvidenceSynthesis(t, executor, wf, adjudicated)
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE", result.Verdict)
	}
}

func TestExecutorCoverageGitLabStateAloneCannotPass(t *testing.T) {
	// The reported count matches the included files, but the GitLab version
	// state says the diff collection is not collected.
	dossier := validDossier(t)
	dossier.DiffSummary = DiffSummary{Files: 2, Total: 2, Truncated: true}
	wf := coverageWorkflow("a.go", "b.go")
	executor := evidenceExecutorWithDossier(t, wf, dossier)
	gathered := runEvidenceGather(t, executor, wf)
	signal, ok := unresolvedOfKind(gathered.Unresolved, UnresolvedEvidence)
	if !ok || !strings.Contains(signal.Message, "not collected") {
		t.Fatalf("state-only truncation signal = %+v / %v", signal, gathered.Unresolved)
	}
	adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
	result := runEvidenceSynthesis(t, executor, wf, adjudicated)
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE", result.Verdict)
	}
}

func TestIncrementalGatherKeepsUnlocatedFinding(t *testing.T) {
	node := completedNode("rv.std.p1", KindReviewStd, reviewNodeParams{
		Package: "p1", Channel: KindReviewStd, Files: []string{"a.go", "b.go"}, Focus: "review", Provider: "local",
	}, ReviewOut{
		Verdict: "PASS", EvidenceStatus: "sufficient", EvidenceGaps: []string{}, Summary: "reviewed",
		Findings: []FindingOut{
			finding("", 0, SeverityP1, "Unlocated risk"),
			finding("a.go", 5, SeverityP2, "Already reviewed file"),
		},
	})
	gate := evidenceGateNode("rv.gate.p1", ReviewGateOut{
		Verdict: "PASS", Accepted: []Ref{}, Rebutted: []Rebut{}, Summary: "reviewed",
	})
	wf := evidenceWorkflow(node, gate)
	dossier := validDossier(t)
	dossier.Incremental = &IncrementalRange{Head: testHeadSHA, Files: []string{"b.go"}, Reason: "test"}
	gathered := runEvidenceGather(t, evidenceExecutorWithDossier(t, wf, dossier), wf)
	if len(gathered.Groups) != 1 {
		t.Fatalf("groups = %+v, want only the unlocated finding", gathered.Groups)
	}
	group := gathered.Groups[0]
	if group.Path != "" || group.Severity != SeverityP1 || group.Title != "Unlocated risk" {
		t.Fatalf("unlocated finding was not preserved: %+v", group)
	}
}

func TestIncrementalGatherFlagsSevereFindingOutsideScope(t *testing.T) {
	node := completedNode("rv.std.p1", KindReviewStd, reviewNodeParams{
		Package: "p1", Channel: KindReviewStd, Files: []string{"a.go", "b.go"}, Focus: "review", Provider: "local",
	}, ReviewOut{
		Verdict: "PASS", EvidenceStatus: "sufficient", EvidenceGaps: []string{}, Summary: "reviewed",
		Findings: []FindingOut{
			finding("a.go", 5, SeverityP1, "Severe out of scope"),
			finding("b.go", 5, SeverityP2, "In scope"),
		},
	})
	gate := evidenceGateNode("rv.gate.p1", ReviewGateOut{
		Verdict: "PASS", Accepted: []Ref{}, Rebutted: []Rebut{}, Summary: "reviewed",
	})
	wf := evidenceWorkflow(node, gate)
	dossier := validDossier(t)
	dossier.Incremental = &IncrementalRange{Head: testHeadSHA, Files: []string{"b.go"}, Reason: "test"}
	executor := evidenceExecutorWithDossier(t, wf, dossier)
	gathered := runEvidenceGather(t, executor, wf)
	signal, ok := unresolvedOfKind(gathered.Unresolved, UnresolvedEvidence)
	if !ok {
		t.Fatalf("out-of-scope P1 must produce an evidence gap: %+v", gathered.Unresolved)
	}
	if !strings.Contains(signal.Message, "a.go") || !strings.Contains(signal.Message, "P1") {
		t.Fatalf("signal does not identify the dropped finding: %q", signal.Message)
	}
	if len(gathered.Groups) != 1 || gathered.Groups[0].Path != "b.go" {
		t.Fatalf("groups = %+v, want only the in-scope group", gathered.Groups)
	}
	adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
	result := runEvidenceSynthesis(t, executor, wf, adjudicated)
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE", result.Verdict)
	}
}

func TestSynthesisScopeDisclosesIncrementalRange(t *testing.T) {
	dossier := validDossier(t)
	dossier.Incremental = &IncrementalRange{
		LastHead: testLastSHA, Head: testHeadSHA, Files: []string{"b.go"},
		Reason: "changed files since the last reviewed head",
	}
	wf := coverageWorkflow("a.go", "b.go")
	executor := evidenceExecutorWithDossier(t, wf, dossier)
	gathered := runEvidenceGather(t, executor, wf)
	adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
	result := runEvidenceSynthesis(t, executor, wf, adjudicated)
	if result.Scope == nil {
		t.Fatal("synthesis result does not disclose the review scope")
	}
	scope := result.Scope
	if scope.Full || scope.LastHead != testLastSHA || scope.Head != testHeadSHA ||
		len(scope.Files) != 1 || scope.Files[0] != "b.go" {
		t.Fatalf("scope = %+v", scope)
	}
}

func TestSynthesisScopeCountsUnlocatedFindings(t *testing.T) {
	node := completedNode("rv.std.p1", KindReviewStd, reviewNodeParams{
		Package: "p1", Channel: KindReviewStd, Files: []string{"a.go"}, Focus: "review", Provider: "local",
	}, ReviewOut{
		Verdict: "BLOCKING", EvidenceStatus: "sufficient", EvidenceGaps: []string{}, Summary: "found",
		Findings: []FindingOut{finding("", 0, SeverityP1, "Unlocated risk")},
	})
	gate := evidenceGateNode("rv.gate.p1", ReviewGateOut{
		Verdict: "BLOCKING", Accepted: []Ref{{Node: "rv.std.p1", Index: 0}}, Rebutted: []Rebut{}, Summary: "found",
	})
	wf := evidenceWorkflow(node, gate)
	executor := evidenceExecutorWithDossier(t, wf, validDossier(t))
	gathered := runEvidenceGather(t, executor, wf)
	adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
	result := runEvidenceSynthesis(t, executor, wf, adjudicated)
	if result.Scope == nil || result.Scope.Unlocated != 1 {
		t.Fatalf("scope = %+v, want one unlocated finding", result.Scope)
	}
	if !result.Scope.Full || result.Scope.Head != testHeadSHA {
		t.Fatalf("full review scope = %+v", result.Scope)
	}
}

func TestSynthesisScopeCapsLargeIncrementalFileList(t *testing.T) {
	dossier := validDossier(t)
	files := make([]string, 0, maxScopeFiles+50)
	for i := 0; i < maxScopeFiles+50; i++ {
		files = append(files, fmt.Sprintf("pkg/f%03d.go", i))
	}
	dossier.Incremental = &IncrementalRange{Head: testHeadSHA, Files: files, Reason: "large push"}
	wf := coverageWorkflow("a.go", "b.go")
	executor := evidenceExecutorWithDossier(t, wf, dossier)
	gathered := runEvidenceGather(t, executor, wf)
	adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
	result := runEvidenceSynthesis(t, executor, wf, adjudicated)
	if result.Scope == nil {
		t.Fatal("scope missing")
	}
	if result.Scope.FileCount != len(files) || len(result.Scope.Files) != maxScopeFiles {
		t.Fatalf("scope = %d files listed / %d total, want %d/%d",
			len(result.Scope.Files), result.Scope.FileCount, maxScopeFiles, len(files))
	}
}

func TestBuildCodePlanSplitsByHardMaxFiles(t *testing.T) {
	dossier := validDossier(t)
	dossier.Files = nil
	total := HardMaxFiles + 1
	for i := 0; i < total; i++ {
		dossier.Files = append(dossier.Files, FileChange{
			Path: fmt.Sprintf("f%03d.go", i), Status: "modified",
			Diff: "@@ -1 +1 @@\n-a\n+b\n", Additions: 1, Deletions: 1,
		})
	}
	plan, err := buildCodePlan(planParams{
		MaxPackages:   DefaultMaxPackages,
		PackageTokens: 1 << 30,
		Propose:       []proposeRef{{Block: "code-review", Provider: "local"}},
	}, dossier)
	if err != nil {
		t.Fatalf("buildCodePlan: %v", err)
	}
	planned := 0
	if len(plan.Packages) < 2 {
		t.Fatalf("packages = %d, want the file list split across packages", len(plan.Packages))
	}
	for _, pkg := range plan.Packages {
		if len(pkg.Files) > HardMaxFiles {
			t.Fatalf("package %s has %d files, want at most %d", pkg.ID, len(pkg.Files), HardMaxFiles)
		}
		planned += len(pkg.Files)
	}
	if planned != total {
		t.Fatalf("planned files = %d, want %d", planned, total)
	}
}

func TestSynthesizePreservesBlockingWithEvidenceGap(t *testing.T) {
	node := completedNode("rv.std.p1", KindReviewStd, reviewNodeParams{
		Package: "p1", Channel: KindReviewStd, Files: []string{"a.go"}, Focus: "review", Provider: "local",
	}, ReviewOut{
		Verdict: "BLOCKING", EvidenceStatus: "insufficient",
		Findings:     []FindingOut{finding("a.go", 5, SeverityP0, "P0 defect")},
		EvidenceGaps: []string{"pipeline log unavailable"}, Summary: "defect found",
	})
	gate := evidenceGateNode("rv.gate.p1", ReviewGateOut{
		Verdict: "BLOCKING", Accepted: []Ref{{Node: "rv.std.p1", Index: 0}}, Rebutted: []Rebut{}, Summary: "defect found",
	})
	wf := evidenceWorkflow(node, gate)
	executor := evidenceExecutor(t, wf)
	gathered := runEvidenceGather(t, executor, wf)
	adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
	result := runEvidenceSynthesis(t, executor, wf, adjudicated)
	if result.Verdict != VerdictBlocking {
		t.Fatalf("verdict = %q, want BLOCKING; summary = %q", result.Verdict, result.Summary)
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "pipeline log unavailable") {
		t.Fatalf("warnings = %v, want the evidence gap disclosed", result.Warnings)
	}
}

func TestSynthesisScopeValidation(t *testing.T) {
	scope := &ReviewScope{
		LastHead: testLastSHA, Head: testHeadSHA, Files: []string{"b.go"},
	}
	output := SynthesizeOut{
		Verdict: VerdictPass, Summary: "clean", Decisions: []Decision{},
		NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{},
	}
	result := SynthesisResult{Verdict: VerdictPass, Summary: "clean", Groups: []Group{}, Output: output, Scope: scope}
	if err := result.Validate(); err != nil {
		t.Fatalf("valid scope rejected: %v", err)
	}
	bad := scope
	bad.Head = "not-a-sha"
	result.Scope = bad
	if err := result.Validate(); err == nil {
		t.Fatal("invalid head sha accepted")
	}
}

func TestUnlocatedFindingIncrementalGatherDoesNotNeedDossierFile(t *testing.T) {
	// A pathless finding cannot be attributed to a file; the incremental
	// filter must keep it even when the dossier has no matching entry.
	dossier := validDossier(t)
	dossier.Incremental = &IncrementalRange{Head: testHeadSHA, Files: []string{"b.go"}, Reason: "test"}
	scope := scopeWithUnlocated(incrementalFileSet(dossier))
	if scope == nil || !findingInScope("", Side(""), scope, dossier) {
		t.Fatalf("pathless finding was filtered out by scope %v", scope)
	}
	if findingInScope("a.go", SideNew, scope, dossier) {
		t.Fatal("out-of-scope file was accepted")
	}
}
