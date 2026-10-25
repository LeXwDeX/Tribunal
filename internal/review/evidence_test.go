package review

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/agent"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

func evidenceReviewNode(id string, out ReviewOut) dag.NodeView {
	return completedNode(id, KindReviewStd, reviewNodeParams{
		Package: "p1", Channel: KindReviewStd, Files: []string{"a.go"}, Focus: "review", Provider: "local",
	}, out)
}

func evidenceGateNode(id string, out ReviewGateOut) dag.NodeView {
	return completedNode(id, KindReviewGate, gateParams{Package: "p1", Files: []string{"a.go"}}, out)
}

func evidenceWorkflow(nodes ...dag.NodeView) dag.WorkflowView {
	wf := dag.WorkflowView{
		ID:    "wf-evidence",
		Spec:  dag.GraphSpec{Budget: dag.Budget{MaxNodes: 30}},
		Nodes: make(map[string]dag.NodeView, len(nodes)),
		Order: make([]string, 0, len(nodes)),
	}
	for _, node := range nodes {
		wf.Nodes[node.ID] = node
		wf.Order = append(wf.Order, node.ID)
	}
	return wf
}

func evidenceExecutor(t *testing.T, wf dag.WorkflowView) *Executor {
	t.Helper()
	head, base := fixedSnapshot(t)
	dossier := validDossier(t)
	dossier.HeadDir = head
	dossier.BaseDir = base
	return evidenceExecutorWithDossier(t, wf, dossier)
}

func evidenceExecutorWithDossier(t *testing.T, wf dag.WorkflowView, dossier *Dossier) *Executor {
	t.Helper()
	return newTestExecutor(t, ExecutorDeps{
		Dossier:  dossier,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		Locate:   func(string, int, Side) bool { return true },
	})
}

func runEvidenceGather(t *testing.T, executor *Executor, wf dag.WorkflowView) GatherOut {
	t.Helper()
	params, _ := json.Marshal(gatherParams{
		ChallengeOnly:     []Severity{SeverityP0, SeverityP1},
		ChallengeProvider: "claude",
		AllowedProviders:  []string{"local", "claude"},
	})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: wf.ID,
		Node:       dag.NodeSpec{ID: NodeGather, Kind: KindGather, Params: params},
	})
	if err != nil {
		t.Fatalf("execute gather: %v", err)
	}
	gathered, err := decodeGatherOut(out.Data)
	if err != nil {
		t.Fatalf("decode gather: %v", err)
	}
	return gathered
}

func runEvidenceAdjudicate(t *testing.T, executor *Executor, wf dag.WorkflowView, gathered GatherOut) AdjudicateOut {
	t.Helper()
	gatheredRaw, _ := json.Marshal(gathered)
	params, _ := json.Marshal(adjudicateParams{ChallengeOnly: []Severity{SeverityP0, SeverityP1}, OnUncertain: OnUncertainMark})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: wf.ID,
		Node:       dag.NodeSpec{ID: NodeAdjudicate, Kind: KindAdjudicate, Params: params},
		Inputs:     map[string]json.RawMessage{"gather": gatheredRaw},
	})
	if err != nil {
		t.Fatalf("execute adjudicate: %v", err)
	}
	adjudicated, err := decodeAdjudicateOut(out.Data)
	if err != nil {
		t.Fatalf("decode adjudicate: %v", err)
	}
	return adjudicated
}

func runEvidenceSynthesis(t *testing.T, executor *Executor, wf dag.WorkflowView, adjudicated AdjudicateOut) SynthesisResult {
	t.Helper()
	adjudicatedRaw, _ := json.Marshal(adjudicated)
	params, _ := json.Marshal(synthesizeParams{Mode: "code", OnUncertain: string(OnUncertainMark)})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: wf.ID,
		Node:       dag.NodeSpec{ID: NodeSynth, Kind: KindSynthesize, Params: params},
		Inputs:     map[string]json.RawMessage{"adjudicate": adjudicatedRaw},
	})
	if err != nil {
		t.Fatalf("execute synthesize: %v", err)
	}
	result, err := decodeSynthesisResult(out.Data)
	if err != nil {
		t.Fatalf("decode synthesis: %v", err)
	}
	return result
}

func TestProposalFromReviewCarriesEvidenceState(t *testing.T) {
	out := ReviewOut{
		Verdict:        "BLOCKING",
		EvidenceStatus: "insufficient",
		Findings:       []FindingOut{},
		EvidenceGaps:   []string{"pipeline log unavailable"},
		Summary:        "cannot verify the build",
	}
	proposal := ProposalFromReview("rv.std.p1", KindReviewStd, "local", "m", 1, out)
	if proposal.Verdict != "BLOCKING" || proposal.EvidenceStatus != "insufficient" {
		t.Fatalf("proposal = %+v", proposal)
	}
	if len(proposal.EvidenceGaps) != 1 || proposal.EvidenceGaps[0] != "pipeline log unavailable" {
		t.Fatalf("gaps = %v", proposal.EvidenceGaps)
	}
}

func TestGatherPreservesInsufficientEvidence(t *testing.T) {
	proposal := ProposalFromReview("rv.std.p1", KindReviewStd, "local", "m", 1, ReviewOut{
		Verdict:        "BLOCKING",
		EvidenceStatus: "insufficient",
		Findings:       []FindingOut{},
		EvidenceGaps:   []string{"pipeline log unavailable"},
		Summary:        "cannot verify the build",
	})
	result, err := Gather(GatherInput{WorkflowID: "wf", Proposals: []Proposal{proposal}}, GatherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Groups) != 0 {
		t.Fatalf("groups = %+v", result.Groups)
	}
	signal, ok := unresolvedOfKind(result.Unresolved, UnresolvedEvidence)
	if !ok {
		t.Fatalf("missing evidence signal: %+v", result.Unresolved)
	}
	if signal.Node != "rv.std.p1" || signal.Message != "pipeline log unavailable" {
		t.Fatalf("signal = %+v", signal)
	}
	if _, ok := unresolvedOfKind(result.Unresolved, UnresolvedBlocking); !ok {
		t.Fatalf("BLOCKING without findings must stay unresolved: %+v", result.Unresolved)
	}
}

func unresolvedOfKind(values []UnresolvedSignal, kind string) (UnresolvedSignal, bool) {
	for _, value := range values {
		if value.Kind == kind {
			return value, true
		}
	}
	return UnresolvedSignal{}, false
}

func TestGatherPreservesReviewBlockingWithoutFindings(t *testing.T) {
	proposal := ProposalFromReview("rv.intent.p1", KindReviewInt, "local", "m", 1, ReviewOut{
		Verdict:        "BLOCKING",
		EvidenceStatus: "sufficient",
		Findings:       []FindingOut{},
		EvidenceGaps:   []string{},
		Summary:        "the change contradicts the MR intent",
	})
	result, err := Gather(GatherInput{WorkflowID: "wf", Proposals: []Proposal{proposal}}, GatherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Unresolved) != 1 || result.Unresolved[0].Kind != UnresolvedBlocking {
		t.Fatalf("unresolved = %+v", result.Unresolved)
	}
}

func TestGatherKeepsBlockingExplainedByFindingsUnresolved(t *testing.T) {
	proposal := ProposalFromReview("rv.std.p1", KindReviewStd, "local", "m", 1, ReviewOut{
		Verdict:        "BLOCKING",
		EvidenceStatus: "sufficient",
		Findings:       []FindingOut{finding("a.go", 10, SeverityP1, "real defect")},
		EvidenceGaps:   []string{},
		Summary:        "one blocking finding",
	})
	result, err := Gather(GatherInput{
		WorkflowID: "wf",
		Proposals:  []Proposal{proposal},
		Unresolved: []UnresolvedSignal{},
	}, GatherOptions{ChallengeProvider: "claude", AllowedProviders: []string{"local", "claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Groups) != 1 {
		t.Fatalf("groups = %d", len(result.Groups))
	}
	if len(result.Unresolved) != 0 {
		t.Fatalf("findings must explain the channel verdict: %+v", result.Unresolved)
	}
}

func TestExecutorGatherKeepsGateBlockingWithoutAcceptedFindings(t *testing.T) {
	wf := evidenceWorkflow(evidenceGateNode("rv.gate.p1", ReviewGateOut{
		Verdict:  "BLOCKING",
		Accepted: []Ref{},
		Rebutted: []Rebut{},
		Summary:  "nothing can be accepted",
	}))
	executor := evidenceExecutor(t, wf)
	gathered := runEvidenceGather(t, executor, wf)
	if len(gathered.Unresolved) != 1 {
		t.Fatalf("gather unresolved = %+v", gathered.Unresolved)
	}
	if gathered.Unresolved[0].Kind != UnresolvedBlocking || gathered.Unresolved[0].Node != "rv.gate.p1" {
		t.Fatalf("gather signal = %+v", gathered.Unresolved[0])
	}
	adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
	if len(adjudicated.Unresolved) != 1 {
		t.Fatalf("adjudicate unresolved = %+v", adjudicated.Unresolved)
	}
}

func TestExecutorCodeSynthesisRetainsInsufficientEvidence(t *testing.T) {
	wf := evidenceWorkflow(
		evidenceReviewNode("rv.std.p1", ReviewOut{
			Verdict:        "BLOCKING",
			EvidenceStatus: "insufficient",
			Findings:       []FindingOut{},
			EvidenceGaps:   []string{"pipeline log unavailable"},
			Summary:        "cannot verify the build",
		}),
		evidenceGateNode("rv.gate.p1", ReviewGateOut{
			Verdict:  "PASS",
			Accepted: []Ref{},
			Rebutted: []Rebut{},
			Summary:  "no findings",
		}),
	)
	executor := evidenceExecutor(t, wf)
	gathered := runEvidenceGather(t, executor, wf)
	if _, ok := unresolvedOfKind(gathered.Unresolved, UnresolvedEvidence); !ok {
		t.Fatalf("gather unresolved = %+v", gathered.Unresolved)
	}
	adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
	if len(adjudicated.Unresolved) != len(gathered.Unresolved) {
		t.Fatalf("adjudicate unresolved = %+v", adjudicated.Unresolved)
	}
	result := runEvidenceSynthesis(t, executor, wf, adjudicated)
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE", result.Verdict)
	}
	if !strings.Contains(result.Summary, "pipeline log unavailable") {
		t.Fatalf("summary does not state the gap: %q", result.Summary)
	}
	if result.Output.Verdict != VerdictPass {
		t.Fatalf("raw settlement verdict = %q", result.Output.Verdict)
	}
	if _, ok := unresolvedOfKind(result.Unresolved, UnresolvedEvidence); !ok {
		t.Fatalf("result unresolved = %+v", result.Unresolved)
	}
}

func TestExecutorAgentSynthesisCannotOverrideInsufficientEvidence(t *testing.T) {
	executor := evidenceAgentSynthesizer(t, func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		raw, _ := json.Marshal(SynthesizeOut{
			Verdict:        VerdictPass,
			Summary:        "all good",
			Decisions:      []Decision{},
			NewFindings:    []FindingOut{},
			ExtendRequests: []ExtendRequest{},
		})
		decoded, err := spec.Submit.Decode(raw)
		if err != nil {
			return nil, err
		}
		return &agent.RunResult{Output: decoded}, nil
	})
	result := runAgentEvidenceSynthesis(t, executor, "wf-agent")
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE", result.Verdict)
	}
	if !strings.Contains(result.Summary, "pipeline log unavailable") {
		t.Fatalf("summary does not state the gap: %q", result.Summary)
	}
	if strings.Contains(result.Summary, "all good") {
		t.Fatalf("agent summary overrode the gap: %q", result.Summary)
	}
}

func TestExecutorAgentFallbackRetainsInsufficientEvidence(t *testing.T) {
	executor := evidenceAgentSynthesizer(t, func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, agent.ErrNoSubmit
	})
	result := runAgentEvidenceSynthesis(t, executor, "wf-agent-fallback")
	if !result.Fallback {
		t.Fatal("expected the deterministic fallback")
	}
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE", result.Verdict)
	}
	if !strings.Contains(result.Summary, "pipeline log unavailable") {
		t.Fatalf("summary does not state the gap: %q", result.Summary)
	}
}

func evidenceAgentSynthesizer(t *testing.T, runner func(context.Context, agent.RunSpec) (*agent.RunResult, error)) *Executor {
	t.Helper()
	profile := defaultProfile()
	profile.Config.Synthesize.Mode = "agent"
	profile.Config.Synthesize.Provider = "claude"
	wf := evidenceWorkflow()
	return newTestExecutor(t, ExecutorDeps{
		Profile:  profile,
		Dossier:  validDossier(t),
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		Locate:   func(string, int, Side) bool { return true },
		AgentRun: runner,
	})
}

func runAgentEvidenceSynthesis(t *testing.T, executor *Executor, workflowID string) SynthesisResult {
	t.Helper()
	adjudicated := AdjudicateOut{
		Groups: []Group{},
		Unresolved: []UnresolvedSignal{{
			Node: "rv.std.p1", Kind: UnresolvedEvidence, Message: "pipeline log unavailable",
		}},
	}
	adjudicatedRaw, _ := json.Marshal(adjudicated)
	params, _ := json.Marshal(synthesizeParams{Mode: "agent", Provider: "claude", OnUncertain: string(OnUncertainMark)})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: workflowID,
		Node:       dag.NodeSpec{ID: NodeSynth, Kind: KindSynthesize, Params: params},
		Attempt:    1,
		Inputs:     map[string]json.RawMessage{"adjudicate": adjudicatedRaw},
	})
	if err != nil {
		t.Fatalf("execute synthesize: %v", err)
	}
	result, err := decodeSynthesisResult(out.Data)
	if err != nil {
		t.Fatalf("decode synthesis: %v", err)
	}
	return result
}

func TestRefutedFindingClearsBlockingWithoutPermanentIncomplete(t *testing.T) {
	review := ReviewOut{
		Verdict:        "BLOCKING",
		EvidenceStatus: "sufficient",
		Findings:       []FindingOut{finding("a.go", 10, SeverityP1, "apparent defect")},
		EvidenceGaps:   []string{},
		Summary:        "one blocking finding",
	}
	wf := evidenceWorkflow(
		evidenceReviewNode("rv.std.p1", review),
		evidenceGateNode("rv.gate.p1", ReviewGateOut{
			Verdict:  "BLOCKING",
			Accepted: []Ref{{Node: "rv.std.p1", Index: 0}},
			Rebutted: []Rebut{},
			Summary:  "accepted the finding",
		}),
	)
	gatherExecutor := evidenceExecutor(t, wf)
	gathered := runEvidenceGather(t, gatherExecutor, wf)
	if len(gathered.Unresolved) != 0 {
		t.Fatalf("explained blocking leaked into unresolved: %+v", gathered.Unresolved)
	}
	if len(gathered.Groups) != 1 || gathered.Groups[0].Challenge == nil {
		t.Fatalf("groups = %+v", gathered.Groups)
	}
	groupID := gathered.Groups[0].ID
	verify := completedNode("verify.1", KindVerify, verifyParams{GroupID: groupID, Provider: "claude"}, VerifyResult{
		GroupID:  groupID,
		Provider: "claude",
		Result: VerifyOut{
			Status: "REFUTED",
			Reason: "the caller already guards the value",
			Evidence: []CodeRef{{
				Path: "a.go", StartLine: 9, EndLine: 10, Root: "head", Note: "nil check before use",
			}},
		},
	})
	wf.Nodes[NodeGather] = completedNode(NodeGather, KindGather, gatherParams{}, gathered)
	wf.Nodes[verify.ID] = verify
	wf.Order = append(wf.Order, NodeGather, verify.ID)
	executor := evidenceExecutor(t, wf)

	adjudicated := runEvidenceAdjudicate(t, executor, wf, gathered)
	if len(adjudicated.Unresolved) != 0 {
		t.Fatalf("refutation leaked into unresolved: %+v", adjudicated.Unresolved)
	}
	if len(adjudicated.Groups) != 1 || adjudicated.Groups[0].Decision == nil || adjudicated.Groups[0].Decision.Action != ActionDrop {
		t.Fatalf("adjudicated groups = %+v", adjudicated.Groups)
	}
	result := runEvidenceSynthesis(t, executor, wf, adjudicated)
	if result.Verdict != VerdictPass {
		t.Fatalf("verdict = %q, want PASS after a valid refutation", result.Verdict)
	}
	if len(result.Unresolved) != 0 {
		t.Fatalf("result unresolved = %+v", result.Unresolved)
	}
}

func TestSufficientReviewStaysBlocking(t *testing.T) {
	proposal := ProposalFromReview("rv.std.p1", KindReviewStd, "local", "m", 1, ReviewOut{
		Verdict:        "BLOCKING",
		EvidenceStatus: "sufficient",
		Findings:       []FindingOut{finding("a.go", 10, SeverityP1, "real defect")},
		EvidenceGaps:   []string{},
		Summary:        "one blocking finding",
	})
	gathered, err := Gather(GatherInput{WorkflowID: "wf", Proposals: []Proposal{proposal}}, GatherOptions{
		ChallengeProvider: "claude", AllowedProviders: []string{"local", "claude"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gathered.Unresolved) != 0 {
		t.Fatalf("unresolved = %+v", gathered.Unresolved)
	}
	gathered.Groups[0].Challenge = &Challenge{Status: ChallengeConfirmed, Reason: "reproduced"}
	adjudicated, err := Adjudicate(gathered.Groups, AdjudicateInput{
		ChallengeOnly: []Severity{SeverityP0, SeverityP1},
		OnUncertain:   OnUncertainMark,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := buildCodeSynthesis(adjudicated.Groups, synthesizeParams{OnUncertain: string(OnUncertainMark)}, adjudicated.Warnings)
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != VerdictBlocking {
		t.Fatalf("verdict = %q, want BLOCKING", result.Verdict)
	}
}

func TestUnresolvedSignalValidation(t *testing.T) {
	cases := []struct {
		name   string
		signal UnresolvedSignal
	}{
		{"empty node", UnresolvedSignal{Kind: UnresolvedEvidence, Message: "gap"}},
		{"unknown kind", UnresolvedSignal{Node: "rv.std.p1", Kind: "warning", Message: "gap"}},
		{"empty message", UnresolvedSignal{Node: "rv.std.p1", Kind: UnresolvedEvidence}},
		{"multiline node", UnresolvedSignal{Node: "rv.std.p1\nrv.intent.p1", Kind: UnresolvedEvidence, Message: "gap"}},
		{"multiline message", UnresolvedSignal{Node: "rv.std.p1", Kind: UnresolvedEvidence, Message: "gap\nmore"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.signal.Validate(); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestSynthesisResultUnresolvedInvariants(t *testing.T) {
	passOutput := SynthesizeOut{
		Verdict: VerdictPass, Summary: "no findings", Decisions: []Decision{},
		NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{},
	}
	signal := UnresolvedSignal{Node: "rv.std.p1", Kind: UnresolvedEvidence, Message: "pipeline log unavailable"}
	cases := []struct {
		name   string
		result SynthesisResult
		valid  bool
	}{
		{
			name: "pass with explicit empty unresolved",
			result: SynthesisResult{
				Verdict: VerdictPass, Summary: "no findings", Groups: []Group{},
				Unresolved: []UnresolvedSignal{}, Output: passOutput,
			},
			valid: true,
		},
		{
			name: "pass with a signal",
			result: SynthesisResult{
				Verdict: VerdictPass, Summary: "no findings", Groups: []Group{},
				Unresolved: []UnresolvedSignal{signal}, Output: passOutput,
			},
		},
		{
			name: "incomplete without a signal",
			result: SynthesisResult{
				Verdict: VerdictIncomplete, Summary: "review incomplete", Groups: []Group{},
				Output: passOutput,
			},
		},
		{
			name: "incomplete with a signal",
			result: SynthesisResult{
				Verdict: VerdictIncomplete, Summary: "review incomplete", Groups: []Group{},
				Unresolved: []UnresolvedSignal{signal}, Output: passOutput,
			},
			valid: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.result.Validate()
			if tc.valid && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.valid && err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestEvidenceOutputsAcceptEmptyAndOmittedUnresolved(t *testing.T) {
	empty := []UnresolvedSignal{}
	gather := GatherOut{Groups: []Group{}, ToChallenge: []ChallengeTarget{}, Unresolved: empty}
	if err := gather.Validate(); err != nil {
		t.Fatalf("gather: %v", err)
	}
	adjudicate := AdjudicateOut{Groups: []Group{}, Unresolved: empty}
	if err := adjudicate.Validate(); err != nil {
		t.Fatalf("adjudicate: %v", err)
	}
	oldGather := []byte(`{"groups":[],"to_challenge":[]}`)
	decodedGather, err := decodeGatherOut(oldGather)
	if err != nil {
		t.Fatalf("old gather snapshot: %v", err)
	}
	if decodedGather.Unresolved != nil {
		t.Fatalf("old gather unresolved = %+v", decodedGather.Unresolved)
	}
	oldAdjudicate := []byte(`{"groups":[]}`)
	decodedAdjudicate, err := decodeAdjudicateOut(oldAdjudicate)
	if err != nil {
		t.Fatalf("old adjudicate snapshot: %v", err)
	}
	if decodedAdjudicate.Unresolved != nil {
		t.Fatalf("old adjudicate unresolved = %+v", decodedAdjudicate.Unresolved)
	}
}

func TestReviewOutEmptyArraysAreConcrete(t *testing.T) {
	raw := []byte(`{"verdict":"BLOCKING","evidence_status":"insufficient","findings":[],"evidence_gaps":[],"summary":"cannot verify"}`)
	out, err := DecodeStrict[*ReviewOut](raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Findings == nil || out.EvidenceGaps == nil {
		t.Fatalf("empty arrays were not preserved: findings=%v gaps=%v", out.Findings, out.EvidenceGaps)
	}
	if signal, ok := firstUnresolved(out); !ok || signal.Kind != UnresolvedEvidence {
		t.Fatalf("missing evidence signal for insufficient status: %+v", signal)
	}
}

func firstUnresolved(out *ReviewOut) (UnresolvedSignal, bool) {
	signals := proposalUnresolved(ProposalFromReview("rv.std.p1", KindReviewStd, "local", "m", 1, *out))
	if len(signals) == 0 {
		return UnresolvedSignal{}, false
	}
	return signals[0], true
}

func TestUnresolvedSignalUnmarshalErrorsAreReported(t *testing.T) {
	raw := []byte(`{"groups":[],"unresolved":[{"node":"","kind":"evidence_gap","message":"gap"}]}`)
	if _, err := decodeAdjudicateOut(raw); err == nil {
		t.Fatal("empty node must fail")
	}
}

func TestGatherExplicitUnresolvedIsValidatedAndDeduplicated(t *testing.T) {
	signal := UnresolvedSignal{Node: "rv.gate.p1", Kind: UnresolvedBlocking, Message: "review gate reported BLOCKING with no accepted findings"}
	result, err := Gather(GatherInput{
		WorkflowID: "wf",
		Unresolved: []UnresolvedSignal{signal, signal},
	}, GatherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Unresolved) != 1 || result.Unresolved[0] != signal {
		t.Fatalf("unresolved = %+v", result.Unresolved)
	}
	if _, err := Gather(GatherInput{
		WorkflowID: "wf",
		Unresolved: []UnresolvedSignal{{Node: "rv.gate.p1", Kind: "unknown", Message: "x"}},
	}, GatherOptions{}); err == nil {
		t.Fatal("unknown kind must fail")
	}
}

func TestSecondSynthesisKeepsPriorUnresolvedEvidence(t *testing.T) {
	passOutput := SynthesizeOut{
		Verdict: VerdictPass, Summary: "settled by rules", Decisions: []Decision{},
		NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{},
	}
	previous := SynthesisResult{
		Verdict: VerdictIncomplete, Summary: "review incomplete", Groups: []Group{},
		Unresolved: []UnresolvedSignal{{Node: "rv.std.p1", Kind: UnresolvedEvidence, Message: "pipeline log unavailable"}},
		Output:     passOutput,
	}
	previousRaw, _ := json.Marshal(previous)
	adjudicatedRaw, _ := json.Marshal(AdjudicateOut{Groups: []Group{}})
	wf := dag.WorkflowView{
		ID:    "wf-second",
		Spec:  dag.GraphSpec{Budget: dag.Budget{MaxNodes: 30}},
		Nodes: map[string]dag.NodeView{},
	}
	executor := evidenceExecutor(t, wf)
	params, _ := json.Marshal(synthesizeParams{Mode: "code", PreviousNode: NodeSynth, OnUncertain: string(OnUncertainMark)})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: wf.ID,
		Node:       dag.NodeSpec{ID: NodeSynth2, Kind: KindSynthesize, Params: params},
		Attempt:    1,
		Inputs: map[string]json.RawMessage{
			"adjudicate": adjudicatedRaw,
			"previous":   previousRaw,
		},
	})
	if err != nil {
		t.Fatalf("execute synthesize.2: %v", err)
	}
	result, err := decodeSynthesisResult(out.Data)
	if err != nil {
		t.Fatalf("decode synthesis.2: %v", err)
	}
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %q, want INCOMPLETE", result.Verdict)
	}
	if _, ok := unresolvedOfKind(result.Unresolved, UnresolvedEvidence); !ok || !strings.Contains(result.Summary, "pipeline log unavailable") {
		t.Fatalf("prior evidence was lost: %+v / %q", result.Unresolved, result.Summary)
	}
}

func TestRenderIncompleteSummaryStatesGaps(t *testing.T) {
	result := SynthesisResult{
		Verdict: VerdictIncomplete,
		Summary: incompleteSummary([]UnresolvedSignal{{Node: "rv.std.p1", Kind: UnresolvedEvidence, Message: "pipeline log unavailable"}}),
		Groups:  []Group{},
		Unresolved: []UnresolvedSignal{{
			Node: "rv.std.p1", Kind: UnresolvedEvidence, Message: "pipeline log unavailable",
		}},
		Output: SynthesizeOut{
			Verdict: VerdictPass, Summary: "no findings", Decisions: []Decision{},
			NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{},
		},
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("result: %v", err)
	}
	body, err := RenderSummary(SummaryInput{
		Verdict: result.Verdict, Summary: result.Summary, Groups: result.Groups, Fingerprint: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"⚪ 评审未完成", "pipeline log unavailable"} {
		if !strings.Contains(body, want) {
			t.Fatalf("summary is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "🟢 PASS") || strings.Contains(body, "🔴 BLOCKING") {
		t.Fatalf("incomplete summary rendered a review verdict:\n%s", body)
	}
}

func TestLegacySynthesisSnapshotWithoutUnresolvedIsAccepted(t *testing.T) {
	raw := []byte(`{"verdict":"PASS","summary":"no findings","groups":[],"output":{"verdict":"PASS","summary":"no findings","decisions":[],"new_findings":[],"extend_requests":[]}}`)
	result, err := decodeSynthesisResult(raw)
	if err != nil {
		t.Fatalf("legacy snapshot: %v", err)
	}
	if result.Unresolved != nil {
		t.Fatalf("legacy unresolved = %+v", result.Unresolved)
	}
}

func TestIncompleteSummaryBoundsGaps(t *testing.T) {
	values := make([]UnresolvedSignal, 0, 10)
	for i := 0; i < 10; i++ {
		values = append(values, UnresolvedSignal{
			Node: "rv.std.p1", Kind: UnresolvedEvidence, Message: strings.Repeat("x", 400),
		})
	}
	summary := incompleteSummary(values)
	if len([]rune(summary)) > MaxSummaryRunes {
		t.Fatalf("summary length = %d", len([]rune(summary)))
	}
	if !strings.Contains(summary, "另有 2 项") {
		t.Fatalf("summary omitted the overflow count: %q", summary)
	}
	if !strings.Contains(summary, "rv.std.p1") {
		t.Fatalf("summary omitted the node: %q", summary)
	}
}

func TestExecutorGatherKeepsEvidenceGapForFilteredIncrementalProposal(t *testing.T) {
	dossier := validDossier(t)
	dossier.Incremental = &IncrementalRange{Files: []string{"b.go"}}
	wf := evidenceWorkflow(evidenceReviewNode("rv.std.p1", ReviewOut{
		Verdict:        "BLOCKING",
		EvidenceStatus: "insufficient",
		Findings:       []FindingOut{finding("a.go", 10, SeverityP1, "outside the incremental scope")},
		EvidenceGaps:   []string{"pipeline log unavailable"},
		Summary:        "cannot verify the build",
	}))
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  dossier,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		Locate:   func(string, int, Side) bool { return true },
	})
	gathered := runEvidenceGather(t, executor, wf)
	if len(gathered.Groups) != 0 {
		t.Fatalf("filtered finding became a group: %+v", gathered.Groups)
	}
	if _, ok := unresolvedOfKind(gathered.Unresolved, UnresolvedEvidence); !ok {
		t.Fatalf("filtered finding hid the evidence gap: %+v", gathered.Unresolved)
	}
}
