package review

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func renameChainDossier(t *testing.T) *Dossier {
	t.Helper()
	d := validDossier(t)
	d.Files = []FileChange{
		{Path: "c.go", OldPath: "b.go", Status: "renamed", Diff: "@@ -1 +1 @@\n-old\n+new\n"},
		{Path: "b.go", OldPath: "a.go", Status: "renamed", Diff: "@@ -1 +1 @@\n-old\n+new\n"},
	}
	return d
}

func TestRenameFollowupTaskShowsBothSidePathsWithoutDiffHeaders(t *testing.T) {
	d := renameChainDossier(t)
	task := reviewTask(d, reviewNodeParams{Files: []string{"c.go", "b.go"}})
	for _, want := range []string{"old: b.go", "new: c.go", "old: a.go", "new: b.go"} {
		if !strings.Contains(task, want) {
			t.Errorf("task lacks side provenance %q:\n%s", want, task)
		}
	}
}

func TestRenameFollowupTaskDoesNotInventMissingOldRenamePath(t *testing.T) {
	d := validDossier(t)
	d.Files = []FileChange{{Path: "new.go", Status: "renamed", Diff: "@@ -1 +1 @@\n-old\n+new\n"}}
	task := reviewTask(d, reviewNodeParams{Files: []string{"new.go"}})
	if !strings.Contains(task, "old: 未知") || !strings.Contains(task, "new: new.go") {
		t.Fatalf("missing old rename path was not disclosed:\n%s", task)
	}
}

func TestRenameFollowupTaskDisclosesAbsentAddedAndDeletedSides(t *testing.T) {
	d := validDossier(t)
	d.Files = []FileChange{
		{Path: "new.go", Status: "added", Diff: "+new\n"},
		{Path: "old.go", Status: "deleted", Diff: "-old\n"},
	}
	task := reviewTask(d, reviewNodeParams{Files: []string{"new.go", "old.go"}})
	for _, want := range []string{"old: 不存在", "new: new.go", "old: old.go", "new: 不存在"} {
		if !strings.Contains(task, want) {
			t.Errorf("task lacks %q:\n%s", want, task)
		}
	}
}

func TestRenameFollowupDedupeUsesActualChangedFile(t *testing.T) {
	d := renameChainDossier(t)
	old := finding("b.go", 1, SeverityP2, "same issue")
	old.Side = "old"
	newOther := finding("b.go", 1, SeverityP2, "same issue")
	newSame := finding("c.go", 1, SeverityP2, "same issue")
	nodes := []dag.NodeView{
		completedNode("old", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{old}}),
		completedNode("new-other", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{newOther}}),
		completedNode("new-same", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{newSame}}),
	}
	for i := range nodes {
		nodes[i].Provider = nodes[i].ID
	}
	wf := evidenceWorkflow(nodes...)
	got := runEvidenceGather(t, evidenceExecutorWithDossier(t, wf, d), wf)
	if len(got.Groups) != 3 {
		t.Fatalf("groups = %+v, want all ambiguous sources preserved", got.Groups)
	}
	keys := map[string]bool{}
	for _, group := range got.Groups {
		if len(group.Sources) != 1 || group.Votes != 1 || keys[group.PublishKey] || group.Sources[0].PublishKey != group.PublishKey {
			t.Fatalf("ambiguous source merged or shared a key: %+v", got.Groups)
		}
		keys[group.PublishKey] = true
	}
}

func TestRenameFollowupUnambiguousRenameStillDedupesAcrossSides(t *testing.T) {
	d := renameChainDossier(t)
	d.Files = d.Files[:1]
	old := finding("b.go", 1, SeverityP2, "same issue")
	old.Side = "old"
	newSide := finding("c.go", 1, SeverityP2, "same issue")
	a := completedNode("a", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{old}})
	b := completedNode("b", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{newSide}})
	a.Provider, b.Provider = "one", "two"
	wf := evidenceWorkflow(a, b)
	got := runEvidenceGather(t, evidenceExecutorWithDossier(t, wf, d), wf)
	if len(got.Groups) != 1 || len(got.Groups[0].Sources) != 2 || got.Groups[0].Votes != 2 {
		t.Fatalf("same unambiguous rename did not merge across sides: %+v", got.Groups)
	}
}

func TestRenameFollowupDuplicateOldPathsCannotShareVotesOrKeys(t *testing.T) {
	d := renameChainDossier(t)
	d.Files[1].OldPath = "b.go"
	old := finding("b.go", 1, SeverityP2, "same issue")
	old.Side = "old"
	a := completedNode("a", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{old}})
	b := completedNode("b", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{old}})
	a.Provider, b.Provider = "one", "two"
	wf := evidenceWorkflow(a, b)
	got := runEvidenceGather(t, evidenceExecutorWithDossier(t, wf, d), wf)
	if len(got.Groups) != 2 || got.Groups[0].Votes != 1 || got.Groups[1].Votes != 1 {
		t.Fatalf("ambiguous old paths shared votes: %+v", got.Groups)
	}
	if got.Groups[0].PublishKey == got.Groups[1].PublishKey {
		t.Fatalf("ambiguous old paths shared key: %+v", got.Groups)
	}
}

func TestRenameFollowupUnchangedFileStillDedupesAcrossSides(t *testing.T) {
	d := validDossier(t)
	d.Files = []FileChange{{Path: "a.go", OldPath: "a.go", Status: "modified"}}
	old := finding("a.go", 5, SeverityP2, "same issue")
	old.Side = "old"
	newSide := old
	newSide.Side = "new"
	a := completedNode("a", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{old}})
	b := completedNode("b", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{newSide}})
	a.Provider, b.Provider = "one", "two"
	wf := evidenceWorkflow(a, b)
	got := runEvidenceGather(t, evidenceExecutorWithDossier(t, wf, d), wf)
	if len(got.Groups) != 1 || len(got.Groups[0].Sources) != 2 || got.Groups[0].Votes != 2 {
		t.Fatalf("same modified file did not merge across sides: %+v", got.Groups)
	}
	if got.Groups[0].PublishKey != PublishKeyFor("a.go", old.Title, old.Evidence) {
		t.Fatalf("unchanged file lost its existing publication key: %q", got.Groups[0].PublishKey)
	}
}

func TestRenameFollowupIgnoredUnknownOldPathDoesNotCreateEvidenceGap(t *testing.T) {
	d := validDossier(t)
	d.Files = []FileChange{{Path: "a.go", OldPath: "a.go", Status: "modified"}}
	d.Ignored = []string{"excluded.go"}
	old := finding("a.go", 5, SeverityP2, "same issue")
	old.Side = "old"
	newSide := old
	newSide.Side = "new"
	a := completedNode("a", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{old}})
	b := completedNode("b", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{newSide}})
	wf := evidenceWorkflow(a, b)
	got := runEvidenceGather(t, evidenceExecutorWithDossier(t, wf, d), wf)
	if len(got.Groups) != 2 || got.Groups[0].PublishKey == got.Groups[1].PublishKey {
		t.Fatalf("unknown ignored side was treated as proven provenance: %+v", got.Groups)
	}
	for _, signal := range got.Unresolved {
		if signal.Node == "provenance" {
			t.Fatalf("ignore alone produced an evidence gap: %+v", got.Unresolved)
		}
	}
}

func TestRenameFollowupAbsentFileSideCannotDeduplicate(t *testing.T) {
	for _, status := range []string{"added", "deleted"} {
		t.Run(status, func(t *testing.T) {
			d := validDossier(t)
			d.Files = []FileChange{{Path: "a.go", Status: status}}
			old := finding("a.go", 5, SeverityP2, "same issue")
			old.Side = "old"
			newSide := old
			newSide.Side = "new"
			a := completedNode("a", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{old}})
			b := completedNode("b", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{newSide}})
			wf := evidenceWorkflow(a, b)
			got := runEvidenceGather(t, evidenceExecutorWithDossier(t, wf, d), wf)
			if len(got.Groups) != 2 || got.Groups[0].PublishKey == got.Groups[1].PublishKey {
				t.Fatalf("absent file side was merged: %+v", got.Groups)
			}
		})
	}
}

func TestRenameFollowupIncrementalKeepsOldSideInMixedAndSingleProposals(t *testing.T) {
	d := renameChainDossier(t)
	d.Files = append(d.Files, FileChange{Path: "d.go", Status: "modified"})
	d.Incremental = &IncrementalRange{Files: []string{"c.go"}}
	old := finding("b.go", 1, SeverityP2, "renamed old side")
	old.Side = "old"
	other := finding("d.go", 1, SeverityP2, "other new side")
	mixed := completedNode("mixed", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{other, old}})
	single := completedNode("single", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{old}})
	mixed.Provider, single.Provider = "one", "two"
	wf := evidenceWorkflow(mixed, single)
	got := runEvidenceGather(t, evidenceExecutorWithDossier(t, wf, d), wf)
	if len(got.Groups) != 2 || len(got.Groups[0].Sources) != 1 || len(got.Groups[1].Sources) != 1 {
		t.Fatalf("incremental groups = %+v, want both old-side sources", got.Groups)
	}
	if got.Groups[0].Sources[0].Origin.Node != "mixed" || got.Groups[0].Sources[0].Origin.Index != 1 || got.Groups[1].Sources[0].Origin.Node != "single" {
		t.Fatalf("incremental source identities = %+v", got.Groups)
	}
}

func TestRenameFollowupIncrementalPreservesGateRefs(t *testing.T) {
	d := renameChainDossier(t)
	d.Files = append(d.Files, FileChange{Path: "d.go", Status: "modified"})
	d.Incremental = &IncrementalRange{Files: []string{"c.go"}}
	old := finding("b.go", 1, SeverityP2, "renamed old side")
	old.Side = "old"
	other := finding("d.go", 1, SeverityP2, "other new side")
	proposal := evidenceReviewNode("std", ReviewOut{Verdict: "PASS", EvidenceStatus: "sufficient", EvidenceGaps: []string{}, Summary: "reviewed", Findings: []FindingOut{other, old}})
	gate := evidenceGateNode("gate", ReviewGateOut{Verdict: "PASS", Summary: "reviewed", Accepted: []Ref{{Node: "std", Index: 0}}, Rebutted: []Rebut{{Ref: Ref{Node: "std", Index: 1}, Reason: "rebutted"}}})
	wf := evidenceWorkflow(proposal, gate)
	got := runEvidenceGather(t, evidenceExecutorWithDossier(t, wf, d), wf)
	if len(got.Groups) != 0 || len(got.Dropped) != 1 || got.Dropped[0].Origin.Node != "std" || got.Dropped[0].Origin.Index != 1 {
		t.Fatalf("gate refs lost during incremental filtering: groups=%+v dropped=%+v", got.Groups, got.Dropped)
	}
}

func TestRenameFollowupIncrementalRetainsUncertainOldProvenance(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		d := renameChainDossier(t)
		if duplicate {
			d.Files[1].OldPath = "b.go"
		} else {
			d.Files[0].OldPath = ""
			d.Files = d.Files[:1]
		}
		d.Incremental = &IncrementalRange{Files: []string{"c.go"}}
		old := finding("b.go", 1, SeverityP2, "uncertain provenance")
		old.Side = "old"
		node := completedNode("old", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{old}})
		wf := evidenceWorkflow(node)
		got := runEvidenceGather(t, evidenceExecutorWithDossier(t, wf, d), wf)
		if len(got.Groups) != 1 || got.Groups[0].Path != "b.go" {
			t.Fatalf("duplicate=%v: uncertain old finding was filtered: %+v", duplicate, got.Groups)
		}
	}
}

func TestRenameFollowupAmbiguousPathCannotShareConsensusOrPublicationKey(t *testing.T) {
	d := renameChainDossier(t)
	old := finding("b.go", 1, SeverityP2, "same issue")
	old.Side = "old"
	newSame := finding("c.go", 1, SeverityP2, "same issue")
	a := completedNode("a", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{old}})
	b := completedNode("b", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{newSame}})
	a.Provider, b.Provider = "one", "two"
	wf := evidenceWorkflow(a, b)
	got := runEvidenceGather(t, evidenceExecutorWithDossier(t, wf, d), wf)
	if len(got.Groups) != 2 || got.Groups[0].Votes != 1 || got.Groups[1].Votes != 1 || got.Groups[0].PublishKey == got.Groups[1].PublishKey {
		t.Fatalf("ambiguous model path was treated as proven provenance: %+v", got.Groups)
	}
}

func TestRenameFollowupOmittedPathCannotShareConsensusOrPublicationKey(t *testing.T) {
	d := renameChainDossier(t)
	d.Files = d.Files[:1]
	d.DiffSummary = DiffSummary{Truncated: true, Omitted: []OmittedFile{{Path: "b.go", Reason: "max_files"}}}
	old := finding("b.go", 1, SeverityP2, "same issue")
	old.Side = "old"
	newSame := finding("c.go", 1, SeverityP2, "same issue")
	a := completedNode("a", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{old}})
	b := completedNode("b", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{newSame}})
	a.Provider, b.Provider = "one", "two"
	wf := evidenceWorkflow(a, b)
	got := runEvidenceGather(t, evidenceExecutorWithDossier(t, wf, d), wf)
	if len(got.Groups) != 2 || got.Groups[0].Votes != 1 || got.Groups[1].Votes != 1 || got.Groups[0].PublishKey == got.Groups[1].PublishKey {
		t.Fatalf("omitted-path ambiguity shared votes or keys: %+v", got.Groups)
	}
}

func TestRenameFollowupIncrementalDoesNotDiscardAmbiguousOtherSide(t *testing.T) {
	d := renameChainDossier(t)
	d.Incremental = &IncrementalRange{Files: []string{"c.go"}}
	newSide := finding("b.go", 1, SeverityP2, "ambiguous side")
	newSide.Side = "new"
	node := completedNode("node", KindCodeReview, reviewNodeParams{}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{newSide}})
	wf := evidenceWorkflow(node)
	got := runEvidenceGather(t, evidenceExecutorWithDossier(t, wf, d), wf)
	if len(got.Groups) != 1 || got.Groups[0].Path != "b.go" {
		t.Fatalf("ambiguous finding was silently discarded: %+v", got.Groups)
	}
}

func TestRenameFollowupGateRebuttalCannotHideAmbiguousProvenance(t *testing.T) {
	d := renameChainDossier(t)
	old := finding("b.go", 1, SeverityP2, "uncertain provenance")
	old.Side = "old"
	proposal := evidenceReviewNode("std", ReviewOut{Verdict: "PASS", EvidenceStatus: "sufficient", EvidenceGaps: []string{}, Summary: "reviewed", Findings: []FindingOut{old}})
	gate := evidenceGateNode("gate", ReviewGateOut{Verdict: "PASS", Summary: "reviewed", Accepted: []Ref{}, Rebutted: []Rebut{{Ref: Ref{Node: "std", Index: 0}, Reason: "rebutted"}}})
	wf := evidenceWorkflow(proposal, gate)
	e := evidenceExecutorWithDossier(t, wf, d)
	gathered := runEvidenceGather(t, e, wf)
	if len(gathered.Groups) != 0 || len(gathered.Dropped) != 1 {
		t.Fatalf("gate result = %+v", gathered)
	}
	var provenance bool
	for _, signal := range gathered.Unresolved {
		provenance = provenance || signal.Node == "provenance"
	}
	if !provenance {
		t.Fatalf("gate hid ambiguous provenance: %+v", gathered.Unresolved)
	}
	result := runEvidenceSynthesis(t, e, wf, runEvidenceAdjudicate(t, e, wf, gathered))
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE", result.Verdict)
	}
}

func TestRenameFollowupExtensionAmbiguityCannotProducePass(t *testing.T) {
	d := renameChainDossier(t)
	old := finding("b.go", 1, SeverityP2, "extension issue")
	old.Side = "old"
	ext := completedNode("rv.ext.1", KindCodeReview, reviewNodeParams{Files: []string{"c.go", "b.go"}}, CodeReviewOut{Summary: "reviewed", Findings: []FindingOut{old}})
	wf := evidenceWorkflow(ext)
	e := evidenceExecutorWithDossier(t, wf, d)
	previous := SynthesisResult{Verdict: VerdictPass, Summary: "reviewed", Groups: []Group{}, Output: SynthesizeOut{
		Verdict: VerdictPass, Summary: "reviewed", Decisions: []Decision{}, NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{},
	}}
	previousRaw, _ := json.Marshal(previous)
	adjudicatedRaw, _ := json.Marshal(AdjudicateOut{Groups: []Group{}})
	params, _ := json.Marshal(synthesizeParams{Mode: "code", PreviousNode: NodeSynth, OnUncertain: string(OnUncertainMark)})
	out, err := e.Execute(context.Background(), dag.NodeInput{
		WorkflowID: wf.ID,
		Node:       dag.NodeSpec{ID: NodeSynth2, Kind: KindSynthesize, Params: params},
		Inputs:     map[string]json.RawMessage{"adjudicate": adjudicatedRaw, "previous": previousRaw},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := decodeSynthesisResult(out.Data)
	if err != nil || result.Verdict != VerdictIncomplete || len(result.Groups) != 1 {
		t.Fatalf("extension result=%+v err=%v", result, err)
	}
}
