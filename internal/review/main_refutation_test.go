package review

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func TestSecondSynthesisMainRefutationCannotBePublished(t *testing.T) {
	for _, tc := range []struct {
		name, status, action, policy string
		invalid                      bool
		wantFallback                 bool
	}{
		{name: "valid publish", status: "REFUTED", action: "publish", wantFallback: true},
		{name: "valid mark", status: "REFUTED", action: "mark", wantFallback: true},
		{name: "valid downgrade", status: "REFUTED", action: "downgrade", wantFallback: true},
		{name: "invalid publish", status: "REFUTED", action: "publish", invalid: true},
		{name: "invalid mark", status: "REFUTED", action: "mark", invalid: true},
		{name: "invalid downgrade", status: "REFUTED", action: "downgrade", invalid: true},
		{name: "uncertain judge", status: "UNCERTAIN", action: "publish", policy: "judge"},
		{name: "uncertain drop policy", status: "UNCERTAIN", action: "mark", policy: "drop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			profile := defaultProfile()
			profile.Config.Synthesize.Mode, profile.Config.Synthesize.Provider = "agent", "claude"
			profile.Config.Budget.MaxExtends = 1
			if tc.policy != "" {
				profile.Config.OnUncertain = tc.policy
			}
			dossier := validDossier(t)
			dossier.HeadDir, dossier.BaseDir = fixedSnapshot(t)
			cfg := GraphConfig{AllowedProviders: []string{"local", "claude"}}
			graph, err := BuildGraph(profile, dossier, cfg)
			if err != nil {
				t.Fatal(err)
			}
			expanders, err := BuildExpanders(profile, dossier, cfg)
			if err != nil {
				t.Fatal(err)
			}
			wf := dag.WorkflowView{ID: "wf-main-refutation", Spec: graph, Nodes: map[string]dag.NodeView{}}
			for _, node := range graph.Nodes {
				wf.Order = append(wf.Order, node.ID)
				wf.Nodes[node.ID] = dag.NodeView{ID: node.ID, Spec: node, Status: dag.NodePending}
			}
			finish := func(node dag.NodeSpec, raw json.RawMessage) {
				view := wf.Nodes[node.ID]
				view.ID, view.Spec, view.Status, view.Attempt, view.Output = node.ID, node, dag.NodeCompleted, 1, raw
				wf.Nodes[node.ID] = view
			}
			adjudicateRaw, _ := json.Marshal(AdjudicateOut{Groups: []Group{}})
			finish(wf.Nodes[NodeAdjudicate].Spec, adjudicateRaw)
			var modelRaw json.RawMessage
			executor := newTestExecutor(t, ExecutorDeps{
				Profile: profile, Dossier: dossier,
				Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
				AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
					out, err := spec.Submit.Decode(modelRaw)
					if err != nil {
						return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
					}
					return &agent.RunResult{Output: out}, nil
				},
			})
			run := func(node dag.NodeSpec) json.RawMessage {
				out, err := executor.Execute(ctx, dag.NodeInput{WorkflowID: wf.ID, Node: node, Attempt: 1})
				if err != nil {
					t.Fatalf("execute %s: %v", node.ID, err)
				}
				return out.Data
			}
			index := 0
			first := SynthesizeOut{Verdict: VerdictPass, Summary: "first", Decisions: []Decision{}, NewFindings: []FindingOut{finding("a.go", 2, SeverityP1, "new main finding")}, ExtendRequests: []ExtendRequest{{Kind: "verify", NewFindingIndex: &index, Focus: "verify"}}}
			modelRaw, _ = json.Marshal(first)
			synth := wf.Nodes[NodeSynth].Spec
			firstRaw := run(synth)
			firstResult, err := decodeSynthesisResult(firstRaw)
			if err != nil || firstResult.Fallback || len(firstResult.Groups) != 1 || !firstResult.Groups[0].Origin.Main {
				t.Fatalf("first synthesis: %+v, %v", firstResult, err)
			}
			finish(synth, firstRaw)
			ext, err := expanders[KindSynthesize].Expand(ctx, wf, synth, firstRaw)
			if err != nil {
				t.Fatal(err)
			}
			var verify, synth2 dag.NodeSpec
			for _, node := range ext.Nodes {
				wf.Spec.Nodes = append(wf.Spec.Nodes, node)
				wf.Order = append(wf.Order, node.ID)
				wf.Nodes[node.ID] = dag.NodeView{ID: node.ID, Spec: node, Status: dag.NodePending}
				if node.Kind == KindVerify {
					verify = node
				} else if node.ID == NodeSynth2 {
					synth2 = node
				}
			}
			if verify.ID == "" || synth2.ID == "" {
				t.Fatalf("missing extended nodes: %+v", ext.Nodes)
			}
			if err := dag.ValidateGraph(wf.Spec); err != nil {
				t.Fatal(err)
			}
			wf.ExtendsUsed = 1
			ref := CodeRef{Path: "a.go", StartLine: 2, EndLine: 2, Root: "head", Note: "proof"}
			if tc.invalid {
				ref.Path = "missing.go"
			}
			modelRaw, _ = json.Marshal(VerifyOut{Status: tc.status, Reason: "verification", Evidence: []CodeRef{ref}})
			finish(verify, run(verify))
			decision := Decision{FindingID: firstResult.Groups[0].ID, Action: tc.action, Reason: "model decision"}
			if tc.action == "downgrade" {
				decision.Severity = SeverityP2
			}
			second := SynthesizeOut{Verdict: VerdictBlocking, Summary: "second", Decisions: []Decision{decision}, NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{}}
			modelRaw, _ = json.Marshal(second)
			result, err := decodeSynthesisResult(run(synth2))
			if err != nil || len(result.Groups) != 1 {
				t.Fatalf("second synthesis: %+v, %v", result, err)
			}
			group := result.Groups[0]
			if !group.Origin.Main || group.Challenge == nil || group.Challenge.Node != verify.ID {
				t.Fatalf("lost main verification: %+v", group)
			}
			if tc.wantFallback {
				if !result.Fallback || group.Decision.Action != ActionDrop || group.Challenge.Status != ChallengeRefuted || len(group.Challenge.Evidence) != 1 || group.Challenge.Evidence[0] != ref {
					t.Fatalf("valid refutation was overridden: fallback=%v decision=%+v challenge=%+v", result.Fallback, group.Decision, group.Challenge)
				}
			} else if result.Fallback || group.Decision.Action != Action(tc.action) || group.Challenge.Status != ChallengeUncertain {
				t.Fatalf("uncertain policy was blocked: fallback=%v decision=%+v challenge=%+v", result.Fallback, group.Decision, group.Challenge)
			}
		})
	}
}
