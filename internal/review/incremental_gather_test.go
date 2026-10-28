package review

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func TestExecutorGatherPreservesOriginalFindingIdentity(t *testing.T) {
	stdA := Ref{Node: "rv.std.p1", Index: 0}
	stdB := Ref{Node: "rv.std.p1", Index: 1}
	intentC := Ref{Node: "rv.intent.p1", Index: 0}
	intentD := Ref{Node: "rv.intent.p1", Index: 1}
	titles := map[Ref]string{stdA: "A", stdB: "B", intentC: "C", intentD: "D"}
	tests := []struct {
		name        string
		incremental *IncrementalRange
		multi       bool
		rebutted    []Ref
		groups      [][]Ref
		dropped     []Ref
	}{
		{
			name: "incremental accepts B while A is rebutted", incremental: &IncrementalRange{Files: []string{"b.go"}},
			rebutted: []Ref{stdA}, groups: [][]Ref{{stdB}},
		},
		{
			name: "incremental rebuts B while A is accepted", incremental: &IncrementalRange{Files: []string{"b.go"}},
			rebutted: []Ref{stdB}, dropped: []Ref{stdB},
		},
		{
			name: "incremental accepts both", incremental: &IncrementalRange{Files: []string{"b.go"}},
			groups: [][]Ref{{stdB}},
		},
		{
			name: "full review with rebuttal", rebutted: []Ref{stdA},
			groups: [][]Ref{{stdB}}, dropped: []Ref{stdA},
		},
		{
			name: "full review with opposite rebuttal", rebutted: []Ref{stdB},
			groups: [][]Ref{{stdA}}, dropped: []Ref{stdB},
		},
		{
			name: "full review accepts both", groups: [][]Ref{{stdA}, {stdB}},
		},
		{
			name: "unspecified incremental files leave all findings", incremental: &IncrementalRange{},
			groups: [][]Ref{{stdA}, {stdB}},
		},
		{
			name: "explicit full review ignores file list", incremental: &IncrementalRange{Full: true, Files: []string{"b.go"}},
			groups: [][]Ref{{stdA}, {stdB}},
		},
		{
			name: "incremental merges accepted sources", incremental: &IncrementalRange{Files: []string{"b.go"}}, multi: true,
			rebutted: []Ref{stdA, intentC}, groups: [][]Ref{{stdB, intentD}},
		},
		{
			name: "incremental rebuts only first source", incremental: &IncrementalRange{Files: []string{"b.go"}}, multi: true,
			rebutted: []Ref{stdB, intentC}, groups: [][]Ref{{intentD}}, dropped: []Ref{stdB},
		},
		{
			name: "incremental rebuts only second source", incremental: &IncrementalRange{Files: []string{"b.go"}}, multi: true,
			rebutted: []Ref{stdA, intentD}, groups: [][]Ref{{stdB}}, dropped: []Ref{intentD},
		},
		{
			name: "empty incremental set", incremental: &IncrementalRange{Files: []string{}},
			rebutted: []Ref{stdB},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			std := completedNode(stdA.Node, KindReviewStd, reviewNodeParams{}, ReviewOut{
				Verdict: "PASS", EvidenceStatus: "sufficient", EvidenceGaps: []string{}, Summary: "reviewed",
				Findings: []FindingOut{finding("a.go", 10, SeverityP2, "A"), finding("b.go", 10, SeverityP2, "B")},
			})
			std.Attempt, std.Provider, std.Model = 2, "local", "std-model"
			nodes := []dag.NodeView{std}
			refs := []Ref{stdA, stdB}
			if tc.multi {
				intent := completedNode(intentC.Node, KindReviewInt, reviewNodeParams{}, ReviewOut{
					Verdict: "PASS", EvidenceStatus: "sufficient", EvidenceGaps: []string{}, Summary: "reviewed",
					Findings: []FindingOut{finding("c.go", 20, SeverityP2, "C"), finding("b.go", 11, SeverityP2, "D")},
				})
				intent.Attempt, intent.Provider, intent.Model = 3, "other", "intent-model"
				nodes = append(nodes, intent)
				refs = append(refs, intentC, intentD)
			}
			rebutted := make(map[Ref]bool, len(tc.rebutted))
			for _, ref := range tc.rebutted {
				rebutted[ref] = true
			}
			gate := ReviewGateOut{Verdict: "PASS", Accepted: []Ref{}, Rebutted: []Rebut{}, Summary: "reviewed"}
			for _, ref := range refs {
				if rebutted[ref] {
					gate.Rebutted = append(gate.Rebutted, Rebut{Ref: ref, Reason: "rebut " + ref.Node})
				} else {
					gate.Accepted = append(gate.Accepted, ref)
				}
			}
			if err := ValidateReviewGate(gate, refs); err != nil {
				t.Fatalf("invalid gate fixture: %v", err)
			}
			nodes = append(nodes, evidenceGateNode("rv.gate.p1", gate))
			wf := evidenceWorkflow(nodes...)
			dossier := validDossier(t)
			dossier.Incremental = tc.incremental
			gathered := runEvidenceGather(t, evidenceExecutorWithDossier(t, wf, dossier), wf)

			checkFinding := func(got Finding, ref Ref) {
				t.Helper()
				node := wf.Nodes[ref.Node]
				wantOrigin := Origin{Node: ref.Node, Index: ref.Index, Attempt: node.Attempt, Provider: node.Provider, Model: node.Model}
				if got.Origin != wantOrigin || got.WorkflowID != wf.ID || got.ID != StableFindingID(wf.ID, ref.Node, node.Attempt, ref.Index) || got.Title != titles[ref] {
					t.Errorf("finding identity = %+v / %q / %q / %q, want %+v / %q / %q / %q", got.Origin, got.WorkflowID, got.ID, got.Title, wantOrigin, wf.ID, StableFindingID(wf.ID, ref.Node, node.Attempt, ref.Index), titles[ref])
				}
			}
			if len(gathered.Groups) != len(tc.groups) {
				t.Fatalf("groups = %+v, want sources %+v; dropped = %+v", gathered.Groups, tc.groups, gathered.Dropped)
			}
			for i, refs := range tc.groups {
				group := gathered.Groups[i]
				first := refs[0]
				node := wf.Nodes[first.Node]
				if group.ID != StableGroupID(wf.ID, first.Node, node.Attempt, first.Index) || group.Origin != (Origin{Node: first.Node, Index: first.Index, Attempt: node.Attempt, Provider: node.Provider, Model: node.Model}) {
					t.Errorf("group identity = %q / %+v for first source %+v", group.ID, group.Origin, first)
				}
				if len(group.Sources) != len(refs) {
					t.Fatalf("group sources = %+v, want %+v", group.Sources, refs)
				}
				for j, ref := range refs {
					checkFinding(group.Sources[j], ref)
				}
			}
			if len(gathered.Dropped) != len(tc.dropped) {
				t.Fatalf("dropped = %+v, want %+v", gathered.Dropped, tc.dropped)
			}
			for i, ref := range tc.dropped {
				dropped := gathered.Dropped[i]
				checkFinding(dropped, ref)
				if dropped.Decision == nil || dropped.Decision.Action != ActionDrop || dropped.Decision.Source != "review.gate" || dropped.Decision.Reason != "rebut "+ref.Node {
					t.Errorf("dropped decision = %+v for %+v", dropped.Decision, ref)
				}
			}
		})
	}
}

func TestIncrementalGatherKeepsManualAnnotation(t *testing.T) {
	ctx := context.Background()
	const node = "rv.std.p1"
	proposal := evidenceReviewNode(node, ReviewOut{
		Verdict: "PASS", EvidenceStatus: "sufficient", EvidenceGaps: []string{}, Summary: "reviewed",
		Findings: []FindingOut{finding("a.go", 10, SeverityP2, "A"), finding("b.go", 10, SeverityP2, "B")},
	})
	proposal.Attempt, proposal.Provider, proposal.Model = 2, "local", "model"
	gate := evidenceGateNode("rv.gate.p1", ReviewGateOut{
		Verdict: "PASS", Summary: "reviewed", Accepted: []Ref{{Node: node, Index: 1}},
		Rebutted: []Rebut{{Ref: Ref{Node: node, Index: 0}, Reason: "outside scope"}},
	})
	wf := evidenceWorkflow(proposal, gate)
	run := func(incremental *IncrementalRange) Group {
		t.Helper()
		dossier := validDossier(t)
		dossier.Incremental = incremental
		executor := newTestExecutor(t, ExecutorDeps{
			Dossier:  dossier,
			Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
			AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
				t.Fatal("gather must not call a model")
				return nil, nil
			},
		})
		gathered := runEvidenceGather(t, executor, wf)
		if len(gathered.Groups) != 1 || gathered.Groups[0].Title != "B" {
			t.Fatalf("gathered = %+v", gathered)
		}
		return gathered.Groups[0]
	}
	full := run(nil)

	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "annotation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.RegisterProject(ctx, store.Project{
		ID: 42, PathWithNamespace: "group/test", WebURL: "https://example.invalid/group/test",
		DefaultBranch: "main", TokenEnc: []byte{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(dag.WorkflowCreatedPayload{
		WorkflowID: wf.ID,
		Spec:       dag.GraphSpec{Nodes: []dag.NodeSpec{{ID: node, Kind: KindReviewStd, Required: true}}, Budget: dag.Budget{MaxNodes: 1}},
		Meta:       map[string]string{"project_id": "42", "kind": "review", "mr_iid": "7", "trigger": "mr_open"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Append(ctx, wf.ID, 0, dag.Event{Type: dag.EventWorkflowCreated, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	storeFinding := func(group Group) store.Finding {
		return store.Finding{
			ID: group.ID, WorkflowID: wf.ID, OriginNode: group.Origin.Node, OriginIndex: group.Origin.Index,
			Severity: string(group.Severity), Category: group.Category, Title: group.Title, Body: group.Body,
		}
	}
	if err := db.ReplaceFindings(ctx, wf.ID, []store.Finding{storeFinding(full)}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetFindingAnnotation(ctx, wf.ID, full.ID, store.FindingAnnotationValid, "reviewer"); err != nil {
		t.Fatal(err)
	}
	incremental := run(&IncrementalRange{Files: []string{"b.go"}})
	if err := db.ReplaceFindings(ctx, wf.ID, []store.Finding{storeFinding(incremental)}); err != nil {
		t.Fatal(err)
	}
	stored, err := db.ListFindings(ctx, store.FindingFilter{WorkflowID: wf.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].ID != full.ID || stored[0].OriginIndex != 1 || stored[0].Annotation != store.FindingAnnotationValid || stored[0].AnnotatedBy != "reviewer" || stored[0].AnnotatedAt == nil {
		t.Fatalf("annotated finding after incremental gather = %+v", stored)
	}
}
