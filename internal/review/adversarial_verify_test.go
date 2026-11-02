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

func TestAdversarialPriorRefutation(t *testing.T) {
	for _, tc := range []struct {
		name, request, policy, overrideAction                         string
		missing, failed, override, firstOverride, judge, invalidPrior bool
	}{
		{name: "unrequested refutation cannot be overridden", request: "other", override: true},
		{name: "unrequested refutation cannot be marked", request: "other", override: true, overrideAction: "mark"},
		{name: "unrequested refutation cannot be downgraded", request: "other", override: true, overrideAction: "downgrade"},
		{name: "first model override does not erase prior rule refutation", request: "other", firstOverride: true},
		{name: "failed reverify retains refutation mark", request: "refuted", failed: true, policy: "mark"},
		{name: "failed reverify cannot republish prior refutation", request: "refuted", failed: true, policy: "mark", override: true},
		{name: "missing reverify retains refutation drop", request: "refuted", missing: true, policy: "drop"},
		{name: "failed reverify retains refutation judge", request: "refuted", failed: true, policy: "judge"},
		{name: "uncertain reverify remains judgeable", request: "refuted", judge: true, policy: "judge"},
		{name: "invalid prior evidence follows uncertain policy", request: "refuted", failed: true, invalidPrior: true, policy: "mark"},
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
			spec, err := BuildGraph(profile, dossier, cfg)
			if err != nil {
				t.Fatal(err)
			}
			expanders, err := BuildExpanders(profile, dossier, cfg)
			if err != nil {
				t.Fatal(err)
			}
			wf := dag.WorkflowView{ID: "wf-adversarial", Spec: spec, Nodes: map[string]dag.NodeView{}}
			for _, node := range spec.Nodes {
				wf.Order = append(wf.Order, node.ID)
				wf.Nodes[node.ID] = dag.NodeView{ID: node.ID, Spec: node, Status: dag.NodePending}
			}
			finish := func(node dag.NodeSpec, raw json.RawMessage) {
				view := wf.Nodes[node.ID]
				view.ID, view.Spec, view.Status, view.Attempt, view.Output = node.ID, node, dag.NodeCompleted, 1, raw
				wf.Nodes[node.ID] = view
			}
			apply := func(ext dag.Extension) {
				for _, patch := range ext.DepPatches {
					for i := range wf.Spec.Nodes {
						if wf.Spec.Nodes[i].ID == patch.Node {
							wf.Spec.Nodes[i].DependsOn = append(wf.Spec.Nodes[i].DependsOn, patch.Add...)
							view := wf.Nodes[patch.Node]
							view.Spec = wf.Spec.Nodes[i]
							wf.Nodes[patch.Node] = view
						}
					}
				}
				for _, node := range ext.Nodes {
					wf.Spec.Nodes = append(wf.Spec.Nodes, node)
					wf.Order = append(wf.Order, node.ID)
					wf.Nodes[node.ID] = dag.NodeView{ID: node.ID, Spec: node, Status: dag.NodePending}
				}
				if err := dag.ValidateGraph(wf.Spec); err != nil {
					t.Fatal(err)
				}
			}
			plan := PlanOut{Risk: "low", RiskReasons: []string{"small"}, Packages: []Package{{ID: "p1", Blocks: []string{BlockCodeReview}, Files: []string{"a.go", "b.go"}, Focus: "review"}}, ChallengeOnly: []Severity{SeverityP0, SeverityP1}}
			planRaw, _ := json.Marshal(plan)
			planNode := wf.Nodes[NodePlan].Spec
			finish(planNode, planRaw)
			ext, err := expanders[KindPlan].Expand(ctx, wf, planNode, planRaw)
			if err != nil {
				t.Fatal(err)
			}
			apply(ext)
			if len(ext.Nodes) != 1 {
				t.Fatalf("plan nodes: %+v", ext.Nodes)
			}
			proposal := CodeReviewOut{Summary: "two findings", Findings: []FindingOut{finding("a.go", 2, SeverityP1, "refuted"), finding("b.go", 1, SeverityP2, "other")}}
			proposalRaw, _ := json.Marshal(proposal)
			finish(ext.Nodes[0], proposalRaw)
			var modelRaw json.RawMessage
			var secondPrompt string
			executor := newTestExecutor(t, ExecutorDeps{Profile: profile, Dossier: dossier, Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil }, Locate: func(string, int, Side) bool { return true }, AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
				secondPrompt = spec.Prompt
				out, err := spec.Submit.Decode(modelRaw)
				if err != nil {
					return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
				}
				return &agent.RunResult{Output: out}, nil
			}})
			run := func(node dag.NodeSpec) json.RawMessage {
				out, err := executor.Execute(ctx, dag.NodeInput{WorkflowID: wf.ID, Node: node, Attempt: 1})
				if err != nil {
					t.Fatalf("execute %s: %v", node.ID, err)
				}
				return out.Data
			}
			gatherNode := wf.Nodes[NodeGather].Spec
			gatherRaw := run(gatherNode)
			finish(gatherNode, gatherRaw)
			gather, err := decodeGatherOut(gatherRaw)
			if err != nil || len(gather.Groups) != 2 || len(gather.ToChallenge) != 1 {
				t.Fatalf("gather: %+v, %v", gather, err)
			}
			var a, b string
			for _, group := range gather.Groups {
				if group.Title == "refuted" {
					a = group.ID
				}
				if group.Title == "other" {
					b = group.ID
				}
			}
			if a == "" || b == "" || gather.ToChallenge[0].GroupID != a {
				t.Fatalf("unexpected groups: %+v", gather.Groups)
			}
			ext, err = expanders[KindGather].Expand(ctx, wf, gatherNode, gatherRaw)
			if err != nil || len(ext.Nodes) != 1 {
				t.Fatalf("gather expansion: %+v %v", ext, err)
			}
			apply(ext)
			ref := CodeRef{Path: "a.go", StartLine: 2, EndLine: 2, Root: "head", Note: "earlier proof"}
			if tc.invalidPrior {
				ref.Path = "missing.go"
			}
			prior := VerifyOut{Status: "REFUTED", Reason: "earlier refutation", Evidence: []CodeRef{ref}}
			if tc.judge {
				prior.Status = "CONFIRMED"
			}
			modelRaw, _ = json.Marshal(prior)
			verifyRaw := run(ext.Nodes[0])
			finish(ext.Nodes[0], verifyRaw)
			adjudicateNode := wf.Nodes[NodeAdjudicate].Spec
			adjudicateRaw := run(adjudicateNode)
			finish(adjudicateNode, adjudicateRaw)
			adjudicated, err := decodeAdjudicateOut(adjudicateRaw)
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range adjudicated.Groups {
				if g.ID == a && !tc.judge && !tc.invalidPrior && (g.Decision.Action != ActionDrop || g.Challenge.Status != ChallengeRefuted) {
					t.Fatalf("prior refutation not adjudicated: %+v", g)
				}
			}
			target := a
			if tc.request == "other" {
				target = b
			}
			first := SynthesizeOut{Verdict: VerdictPass, Summary: "first", Decisions: []Decision{}, NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{{Kind: "verify", FindingID: target, Focus: "check"}}}
			if tc.judge || tc.invalidPrior || tc.firstOverride {
				first.Verdict = VerdictBlocking
			}
			if tc.firstOverride {
				first.Decisions = []Decision{{FindingID: a, Action: "publish", Reason: "first model overrides the refutation"}}
			}
			modelRaw, _ = json.Marshal(first)
			firstNode := wf.Nodes[NodeSynth].Spec
			firstRaw := run(firstNode)
			finish(firstNode, firstRaw)
			ext, err = expanders[KindSynthesize].Expand(ctx, wf, firstNode, firstRaw)
			if err != nil {
				t.Fatal(err)
			}
			apply(ext)
			wf.ExtendsUsed = 1
			var synth2 dag.NodeSpec
			for _, node := range ext.Nodes {
				if node.ID == NodeSynth2 {
					synth2 = node
					continue
				}
				if tc.failed {
					view := wf.Nodes[node.ID]
					view.Status = dag.NodeFailed
					view.Error = "secret failure"
					wf.Nodes[node.ID] = view
				} else if tc.missing {
					view := wf.Nodes[node.ID]
					view.Status = dag.NodeSkipped
					view.Reason = "not scheduled"
					wf.Nodes[node.ID] = view
				} else {
					status := "CONFIRMED"
					if tc.judge {
						status = "UNCERTAIN"
					}
					modelRaw, _ = json.Marshal(VerifyOut{Status: status, Reason: "other confirmed", Evidence: []CodeRef{}})
					finish(node, run(node))
				}
			}
			second := SynthesizeOut{Verdict: VerdictPass, Summary: "second", Decisions: []Decision{}, NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{}}
			if tc.invalidPrior {
				second.Verdict = VerdictBlocking
			}
			if tc.override || tc.judge {
				action := tc.overrideAction
				if action == "" {
					action = "publish"
				}
				decision := Decision{FindingID: a, Action: action, Reason: "ignore previous refutation"}
				if action == "downgrade" {
					decision.Severity = SeverityP2
				}
				second.Decisions = []Decision{decision}
				second.Verdict = VerdictBlocking
			}
			modelRaw, _ = json.Marshal(second)
			result, err := decodeSynthesisResult(run(synth2))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, group := range result.Groups {
				if group.ID == a {
					found = true
					if tc.invalidPrior {
						if result.Fallback || group.Decision.Action != ActionMark || group.Challenge == nil || group.Challenge.Status != ChallengeUncertain {
							t.Fatalf("invalid evidence was trusted: %+v", group)
						}
						continue
					}
					if tc.judge {
						if result.Fallback || group.Decision.Action != ActionPublish || group.Challenge == nil || group.Challenge.Status != ChallengeUncertain {
							t.Fatalf("judge policy was not honored: %+v", group)
						}
						continue
					}
					if group.Decision.Action != ActionDrop || group.Challenge == nil || group.Challenge.Status != ChallengeRefuted || group.Challenge.Node != "verify.1" || len(group.Challenge.Evidence) != 1 || group.Challenge.Evidence[0] != ref {
						t.Fatalf("prior refutation lost or published: %+v", group)
					}
					if !strings.Contains(secondPrompt, "earlier refutation") {
						t.Fatal("synthesis prompt lost prior refutation")
					}
				}
			}
			if !found {
				t.Fatal("original finding missing from synthesis")
			}
			if tc.override && !result.Fallback {
				t.Fatal("model override was accepted")
			}
			if (tc.failed || tc.missing) && (!strings.Contains(strings.Join(result.Warnings, " "), "verify failed or missing") || strings.Contains(strings.Join(result.Warnings, " "), "secret failure")) {
				t.Fatalf("missing or unsafe disclosure: %v", result.Warnings)
			}
		})
	}
}

func TestGateRebuttalIsNotSynthesisGroup(t *testing.T) {
	ctx := context.Background()
	profile := defaultProfile()
	profile.Config.Propose[0].Block = "review"
	profile.Config.Synthesize.Mode, profile.Config.Synthesize.Provider = "agent", "claude"
	dossier := validDossier(t)
	cfg := GraphConfig{AllowedProviders: []string{"local", "claude"}}
	spec, err := BuildGraph(profile, dossier, cfg)
	if err != nil {
		t.Fatal(err)
	}
	expanders, err := BuildExpanders(profile, dossier, cfg)
	if err != nil {
		t.Fatal(err)
	}
	wf := dag.WorkflowView{ID: "wf-gate", Spec: spec, Nodes: map[string]dag.NodeView{}}
	for _, node := range spec.Nodes {
		wf.Order = append(wf.Order, node.ID)
		wf.Nodes[node.ID] = dag.NodeView{ID: node.ID, Spec: node, Status: dag.NodePending}
	}
	finish := func(node dag.NodeSpec, raw json.RawMessage) {
		view := wf.Nodes[node.ID]
		view.ID, view.Spec, view.Status, view.Attempt, view.Output = node.ID, node, dag.NodeCompleted, 1, raw
		wf.Nodes[node.ID] = view
	}
	plan := PlanOut{Risk: "low", RiskReasons: []string{"small"}, Packages: []Package{{ID: "p1", Blocks: []string{"review"}, Files: []string{"a.go", "b.go"}, Focus: "review"}}, ChallengeOnly: []Severity{SeverityP0, SeverityP1}}
	planRaw, _ := json.Marshal(plan)
	planNode := wf.Nodes[NodePlan].Spec
	finish(planNode, planRaw)
	ext, err := expanders[KindPlan].Expand(ctx, wf, planNode, planRaw)
	if err != nil {
		t.Fatal(err)
	}
	for _, patch := range ext.DepPatches {
		for i := range wf.Spec.Nodes {
			if wf.Spec.Nodes[i].ID == patch.Node {
				wf.Spec.Nodes[i].DependsOn = append(wf.Spec.Nodes[i].DependsOn, patch.Add...)
				view := wf.Nodes[patch.Node]
				view.Spec = wf.Spec.Nodes[i]
				wf.Nodes[patch.Node] = view
			}
		}
	}
	var std, gate dag.NodeSpec
	for _, node := range ext.Nodes {
		wf.Spec.Nodes = append(wf.Spec.Nodes, node)
		wf.Order = append(wf.Order, node.ID)
		wf.Nodes[node.ID] = dag.NodeView{ID: node.ID, Spec: node, Status: dag.NodePending}
		if node.Kind == KindReviewStd {
			std = node
		}
		if node.Kind == KindReviewGate {
			gate = node
		}
	}
	if std.ID == "" || gate.ID == "" {
		t.Fatalf("missing review nodes: %+v", ext.Nodes)
	}
	if err := dag.ValidateGraph(wf.Spec); err != nil {
		t.Fatal(err)
	}
	for _, node := range ext.Nodes {
		if node.Kind != KindReviewStd && node.Kind != KindReviewInt {
			continue
		}
		out := ReviewOut{Verdict: "PASS", EvidenceStatus: "sufficient", Findings: []FindingOut{}, EvidenceGaps: []string{}, Summary: "review"}
		if node.ID == std.ID {
			out.Findings = []FindingOut{finding("a.go", 2, SeverityP2, "rebutted")}
		}
		raw, _ := json.Marshal(out)
		finish(node, raw)
	}
	var modelRaw json.RawMessage
	executor := newTestExecutor(t, ExecutorDeps{Profile: profile, Dossier: dossier, Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil }, AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		out, err := spec.Submit.Decode(modelRaw)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
		}
		return &agent.RunResult{Output: out}, nil
	}})
	run := func(node dag.NodeSpec) json.RawMessage {
		out, err := executor.Execute(ctx, dag.NodeInput{WorkflowID: wf.ID, Node: node, Attempt: 1})
		if err != nil {
			t.Fatalf("execute %s: %v", node.ID, err)
		}
		return out.Data
	}
	gateOut := ReviewGateOut{Verdict: "PASS", Accepted: []Ref{}, Rebutted: []Rebut{{Ref: Ref{Node: std.ID, Index: 0}, Reason: "not reproducible"}}, Summary: "gate"}
	modelRaw, _ = json.Marshal(gateOut)
	finish(gate, run(gate))
	gatherNode := wf.Nodes[NodeGather].Spec
	gatherRaw := run(gatherNode)
	finish(gatherNode, gatherRaw)
	gather, err := decodeGatherOut(gatherRaw)
	if err != nil || len(gather.Groups) != 0 || len(gather.Dropped) != 1 || gather.Dropped[0].Decision.Source != "review.gate" {
		t.Fatalf("gate gather: %+v, %v", gather, err)
	}
	adjudicate := wf.Nodes[NodeAdjudicate].Spec
	finish(adjudicate, run(adjudicate))
	groupID := "g_" + strings.TrimPrefix(gather.Dropped[0].ID, "f_")
	modelRaw, _ = json.Marshal(SynthesizeOut{Verdict: VerdictPass, Summary: "second", Decisions: []Decision{{FindingID: groupID, Action: "publish", Reason: "revive"}}, NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{}})
	result, err := decodeSynthesisResult(run(wf.Nodes[NodeSynth].Spec))
	if err != nil || !result.Fallback || len(result.Groups) != 0 {
		t.Fatalf("gate rebuttal entered synthesis: %+v, %v", result, err)
	}
}
