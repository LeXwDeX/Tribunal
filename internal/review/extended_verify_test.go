package review

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func TestExtendedVerifyExistingGroup(t *testing.T) {
	for _, tc := range []struct {
		name, status, policy                                                              string
		invalid, fail, missing, override, oldConfirmed, oldRefuted, gateDrop, judge, code bool
		want                                                                              Action
		challenge                                                                         ChallengeStatus
	}{
		{name: "refuted", status: "REFUTED", want: ActionDrop, challenge: ChallengeRefuted},
		{name: "refuted overrides old confirmation", status: "REFUTED", oldConfirmed: true, want: ActionDrop, challenge: ChallengeRefuted},
		{name: "confirmation overrides old refutation", status: "CONFIRMED", oldRefuted: true, want: ActionPublish, challenge: ChallengeConfirmed},
		{name: "review gate drop remains", status: "CONFIRMED", gateDrop: true, want: ActionDrop, challenge: ChallengeConfirmed},
		{name: "model cannot republish refutation", status: "REFUTED", override: true, want: ActionDrop, challenge: ChallengeRefuted},
		{name: "invalid refutation mark", status: "REFUTED", invalid: true, policy: "mark", want: ActionMark, challenge: ChallengeUncertain},
		{name: "uncertain drop", status: "UNCERTAIN", policy: "drop", want: ActionDrop, challenge: ChallengeUncertain},
		{name: "failed mark", fail: true, policy: "mark", want: ActionMark, challenge: ChallengeUncertain},
		{name: "missing mark", missing: true, policy: "mark", want: ActionMark, challenge: ChallengeUncertain},
		{name: "uncertain judge", status: "UNCERTAIN", policy: "judge", judge: true, want: ActionPublish, challenge: ChallengeUncertain},
		{name: "code settlement", status: "REFUTED", code: true, want: ActionDrop, challenge: ChallengeRefuted},
		{name: "confirmed", status: "CONFIRMED", want: ActionPublish, challenge: ChallengeConfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := defaultProfile()
			profile.Config.Synthesize.Mode = "agent"
			profile.Config.Synthesize.Provider = "claude"
			profile.Config.Budget.MaxExtends = 1
			dossier := validDossier(t)
			dossier.HeadDir, dossier.BaseDir = fixedSnapshot(t)
			group := Group{ID: "g1", Severity: SeverityP2, Category: "correctness", Path: "a.go", Line: 10, Side: SideNew,
				Title: "bug", Body: "body", Evidence: "original evidence", Sources: []Finding{{ID: "f1", Origin: Origin{Node: "cr.a", Provider: "local"}, Severity: SeverityP2, Category: "correctness", Path: "a.go", Line: 10, Side: SideNew, Title: "bug", Body: "body", Evidence: "original evidence"}},
				Decision: &FindingDecision{Action: ActionPublish, Reason: "prior", Source: "rules"}}
			if tc.oldConfirmed {
				group.Challenge = &Challenge{Status: ChallengeConfirmed, Node: "verify.1", Provider: "local", Reason: "old"}
			}
			if tc.oldRefuted {
				group.Challenge = &Challenge{Status: ChallengeRefuted, Node: "verify.1", Provider: "local", Reason: "old", Evidence: []CodeRef{{Path: "a.go", StartLine: 2, EndLine: 2, Root: "head", Note: "old"}}}
				group.Decision = &FindingDecision{Action: ActionDrop, Reason: "old", Source: "rules"}
			}
			if tc.gateDrop {
				group.Decision = &FindingDecision{Action: ActionDrop, Reason: "gate", Source: "review.gate"}
			}
			other := cloneGroups([]Group{group})[0]
			other.ID = "g2"
			other.Decision = &FindingDecision{Action: ActionPublish, Reason: "unchanged", Source: "synthesize"}
			other.Challenge = nil
			first := SynthesisResult{Verdict: VerdictPass, Summary: "first", Groups: []Group{group, other}, Warnings: []string{"first warning"}, Output: SynthesizeOut{Verdict: VerdictPass, Summary: "first", Decisions: []Decision{}, NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{{Kind: "verify", FindingID: group.ID, Focus: "check"}}}}
			params := synthesizeParams{Mode: "agent", Provider: "claude", MaxExtends: 1, OnUncertain: tc.policy}
			adjudicate := completedNode(NodeAdjudicate, KindAdjudicate, adjudicateParams{}, AdjudicateOut{Groups: []Group{group, other}, Warnings: []string{"adjudicate warning"}})
			synth := completedNode(NodeSynth, KindSynthesize, params, first)
			wf := evidenceWorkflow(adjudicate, synth)
			wf.Spec.Nodes = []dag.NodeSpec{adjudicate.Spec, synth.Spec}
			wf.Spec.Budget.MaxExtends = 1
			ext, err := (SynthesizeExpander{Profile: profile, Dossier: dossier, Config: GraphConfig{AllowedProviders: []string{"local", "claude"}}}).Expand(context.Background(), wf, synth.Spec, synth.Output)
			if err != nil {
				t.Fatal(err)
			}
			var second dag.NodeSpec
			for _, node := range ext.Nodes {
				wf.Order = append(wf.Order, node.ID)
				if node.ID == NodeSynth2 {
					second = node
					continue
				}
				ref := CodeRef{Path: "a.go", StartLine: 2, EndLine: 2, Root: "head", Note: "refutation"}
				if tc.invalid {
					ref.Path = "missing.go"
				}
				out := VerifyResult{GroupID: group.ID, Provider: "claude", Model: "model", SameModel: true, Result: VerifyOut{Status: tc.status, Reason: "extended reason", Evidence: []CodeRef{ref}}}
				view := completedNode(node.ID, node.Kind, json.RawMessage(node.Params), out)
				view.Spec = node
				if tc.fail {
					view.Status = dag.NodeFailed
					view.Error = "sensitive error"
				}
				if !tc.missing {
					wf.Nodes[node.ID] = view
				}
			}
			if tc.oldConfirmed || tc.oldRefuted {
				oldStatus := "CONFIRMED"
				if tc.oldRefuted {
					oldStatus = "REFUTED"
				}
				old := completedNode("verify.1", KindVerify, verifyParams{GroupID: group.ID}, VerifyResult{GroupID: group.ID, Provider: "local", Result: VerifyOut{Status: oldStatus, Reason: "old", Evidence: []CodeRef{{Path: "a.go", StartLine: 2, EndLine: 2, Root: "head", Note: "old"}}}})
				wf.Nodes[old.ID] = old
				wf.Order = append([]string{old.ID}, wf.Order...)
			}
			wf.ExtendsUsed = 1
			if tc.code {
				p, _ := decodeParams[synthesizeParams](second.Params)
				p.Mode = "code"
				second.Params, _ = json.Marshal(p)
			}
			var prompt string
			var chargedProvider string
			executor := newTestExecutor(t, ExecutorDeps{Dossier: dossier, Profile: profile, Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil }, Cost: func(provider string, _ llm.Usage) int64 { chargedProvider = provider; return 42 }, AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
				prompt = spec.Prompt
				out := SynthesizeOut{Verdict: VerdictPass, Summary: "second", Decisions: []Decision{}, NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{}}
				if tc.override || tc.judge {
					out.Decisions = []Decision{{FindingID: group.ID, Action: "publish", Reason: "ignore refutation"}}
				}
				raw, _ := json.Marshal(out)
				decoded, err := spec.Submit.Decode(raw)
				if err != nil {
					return &agent.RunResult{Usage: llm.Usage{InputTokens: 4, OutputTokens: 5}}, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
				}
				return &agent.RunResult{Output: decoded, Usage: llm.Usage{InputTokens: 4, OutputTokens: 5}}, nil
			}})
			resultRaw, err := executor.Execute(context.Background(), dag.NodeInput{WorkflowID: wf.ID, Node: second, Attempt: 1})
			if err != nil {
				t.Fatal(err)
			}
			if !tc.code && (chargedProvider != "claude" || resultRaw.Usage.InputTokens != 4 || resultRaw.Usage.OutputTokens != 5 || resultRaw.Usage.CostMicros != 42) {
				t.Fatalf("lost provider usage: provider=%q usage=%+v", chargedProvider, resultRaw.Usage)
			}
			result, err := decodeSynthesisResult(resultRaw.Data)
			if err != nil {
				t.Fatal(err)
			}
			got := result.Groups[0]
			if got.Decision.Action != tc.want || got.Challenge == nil || got.Challenge.Status != tc.challenge {
				t.Fatalf("got %+v", got)
			}
			if got.Evidence != group.Evidence || len(got.Sources) != 1 {
				t.Fatalf("lost original finding: %+v", got)
			}
			if len(result.Groups) != 2 || result.Groups[1].ID != "g2" || result.Groups[1].Decision.Reason != "unchanged" || result.Groups[1].Challenge != nil {
				t.Fatalf("unrequested group changed: %+v", result.Groups)
			}
			if !tc.fail && !tc.missing && (got.Challenge.Node != "verify.x1.1" || got.Challenge.Provider != "claude" || got.Challenge.Model != "model" || !got.Challenge.SameModel || (!tc.code && !tc.invalid && !strings.Contains(prompt, "extended reason"))) {
				t.Fatalf("lost challenge: %+v", got.Challenge)
			}
			if !strings.Contains(strings.Join(result.Warnings, " "), "adjudicate warning") || !strings.Contains(strings.Join(result.Warnings, " "), "first warning") {
				t.Fatalf("lost warnings: %v", result.Warnings)
			}
			if tc.fail && strings.Contains(strings.Join(result.Warnings, " "), "sensitive error") {
				t.Fatalf("exposed node error: %v", result.Warnings)
			}
			if tc.override && !result.Fallback {
				t.Fatal("model override did not trigger rules fallback")
			}
		})
	}
}

func TestExtendedVerifySameRoundPriority(t *testing.T) {
	makeVerify := func(id, status string) dag.NodeView {
		return completedNode(id, KindVerify, verifyParams{GroupID: "g1"}, VerifyResult{GroupID: "g1", Provider: "claude", Result: VerifyOut{Status: status, Reason: "reason", Evidence: []CodeRef{{Path: "a.go", StartLine: 2, EndLine: 2, Root: "head", Note: "evidence"}}}})
	}
	first := makeVerify("verify.x1.1", "REFUTED")
	confirmed := makeVerify("verify.x1.2", "CONFIRMED")
	tie := makeVerify("verify.x1.3", "CONFIRMED")
	wf := evidenceWorkflow(first, confirmed, tie)
	results, _, err := collectVerifyResults(wf)
	if err != nil || results["g1"].Node != confirmed.ID {
		t.Fatalf("priority/tie selection: %+v, %v", results, err)
	}
}
