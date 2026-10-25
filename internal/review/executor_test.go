package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/agent"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

func newTestExecutor(t *testing.T, deps ExecutorDeps) *Executor {
	t.Helper()
	if deps.Profile.Name == "" {
		deps.Profile = defaultProfile()
	}
	executor, err := NewExecutor(deps)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	return executor
}

func completedNode(id, kind string, params any, output any) dag.NodeView {
	paramsRaw, _ := json.Marshal(params)
	outputRaw, _ := json.Marshal(output)
	return dag.NodeView{
		ID:     id,
		Spec:   dag.NodeSpec{ID: id, Kind: kind, Params: paramsRaw},
		Status: dag.NodeCompleted,
		Output: outputRaw,
	}
}

func TestExecutorDispatchUnknownKind(t *testing.T) {
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  validDossier(t),
		AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) { return nil, nil },
	})
	_, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: "wf",
		Node:       dag.NodeSpec{ID: "x", Kind: "bogus"},
	})
	if !errors.Is(err, ErrUnknownNodeKind) {
		t.Fatalf("expected ErrUnknownNodeKind, got %v", err)
	}
}

func TestExecutorGatherDedupeAndGateDrop(t *testing.T) {
	dossier := validDossier(t)
	findings := []FindingOut{
		{
			Path: "a.go", Line: 10, Severity: SeverityP1, Category: "correctness",
			Title: "first", Body: "body", Evidence: "evidence", Confidence: ConfidenceHigh,
		},
		{
			Path: "a.go", Line: 11, Severity: SeverityP1, Category: "correctness",
			Title: "second", Body: "body", Evidence: "evidence", Confidence: ConfidenceHigh,
		},
	}
	cr := completedNode("cr.a", KindCodeReview, reviewNodeParams{Package: "a", Files: []string{"a.go"}, Focus: "f", Provider: "local"}, CodeReviewOut{
		Summary: "summary", Findings: findings,
	})
	gate := completedNode("rv.gate.a", KindReviewGate, gateParams{Package: "a", Files: []string{"a.go"}}, ReviewGateOut{
		Verdict: "PASS", Accepted: []Ref{}, Rebutted: []Rebut{{Ref: Ref{Node: "cr.a", Index: 0}, Reason: "not reproducible"}}, Summary: "gate",
	})
	wf := dag.WorkflowView{
		ID:    "wf-gather",
		Spec:  dag.GraphSpec{Budget: dag.Budget{MaxTokens: 10000}},
		Nodes: map[string]dag.NodeView{"cr.a": cr, "rv.gate.a": gate},
		Order: []string{"cr.a", "rv.gate.a"},
	}
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  dossier,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
	})
	params := gatherParams{
		ChallengeOnly:     []Severity{SeverityP0, SeverityP1},
		ChallengeProvider: "claude",
		AllowedProviders:  []string{"local", "claude"},
	}
	paramsRaw, _ := json.Marshal(params)
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: "wf-gather",
		Node:       dag.NodeSpec{ID: NodeGather, Kind: KindGather, Params: paramsRaw},
	})
	if err != nil {
		t.Fatalf("Execute gather: %v", err)
	}
	gathered, err := decodeGatherOut(out.Data)
	if err != nil {
		t.Fatalf("decode gather: %v", err)
	}
	if len(gathered.Dropped) != 1 || gathered.Dropped[0].Title != "first" {
		t.Fatalf("dropped = %+v", gathered.Dropped)
	}
	if len(gathered.Groups) != 1 || len(gathered.ToChallenge) != 1 {
		t.Fatalf("groups=%d, challenges=%d", len(gathered.Groups), len(gathered.ToChallenge))
	}
	if gathered.ToChallenge[0].Provider != "claude" {
		t.Fatalf("challenge provider = %q", gathered.ToChallenge[0].Provider)
	}
	if out.Usage.InputTokens != 0 || out.Usage.OutputTokens != 0 {
		t.Fatalf("code node usage = %+v", out.Usage)
	}
}

func TestExecutorPlanCode(t *testing.T) {
	dossier := validDossier(t)
	spec, err := BuildGraph(defaultProfile(), dossier, GraphConfig{})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	planNode := mustNode(t, spec, NodePlan)
	executor := newTestExecutor(t, ExecutorDeps{Dossier: dossier})
	out, err := executor.Execute(context.Background(), dag.NodeInput{WorkflowID: "wf-plan", Node: planNode})
	if err != nil {
		t.Fatalf("Execute plan: %v", err)
	}
	plan, err := DecodeStrict[*PlanOut](out.Data)
	if err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if len(plan.Packages) == 0 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	switch plan.Risk {
	case "low", "medium", "high":
	default:
		t.Fatalf("invalid risk %q", plan.Risk)
	}
	if len(plan.Packages[0].Files) == 0 || plan.Packages[0].Files[0] != "a.go" {
		t.Fatalf("unexpected package files: %+v", plan.Packages[0])
	}
}

func TestExecutorVerifyNormalizesRefutation(t *testing.T) {
	finding := FindingOut{
		Path: "a.go", Line: 3, Severity: SeverityP1, Category: "correctness",
		Title: "bug", Body: "body", Evidence: "evidence", Confidence: ConfidenceHigh,
	}
	paramsRaw, _ := json.Marshal(verifyParams{GroupID: "g1", Finding: finding, Provider: "claude"})
	invalid := VerifyOut{Status: "REFUTED", Reason: "not a bug", Evidence: []CodeRef{}}
	invalidRaw, _ := json.Marshal(invalid)
	runnerCalls := 0
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier: validDossier(t),
		AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			runnerCalls++
			decoded, err := spec.Submit.Decode(invalidRaw)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
			}
			return &agent.RunResult{Output: decoded, Usage: llm.Usage{InputTokens: 3, OutputTokens: 2}}, nil
		},
	})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: "wf-verify",
		Node:       dag.NodeSpec{ID: "verify.1", Kind: KindVerify, Params: paramsRaw},
	})
	if err != nil {
		t.Fatalf("Execute verify: %v", err)
	}
	if runnerCalls != 1 {
		t.Fatalf("runner calls = %d", runnerCalls)
	}
	envelope, err := decodeVerifyResult(out.Data)
	if err != nil {
		t.Fatalf("decode verify: %v", err)
	}
	if envelope.Result.Status != "UNCERTAIN" {
		t.Fatalf("status = %q, want UNCERTAIN", envelope.Result.Status)
	}
	if out.Usage.InputTokens != 3 || out.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

func TestExecutorReviewGateValidatesCoverage(t *testing.T) {
	dossier := validDossier(t)
	review := ReviewOut{
		Verdict: "PASS", EvidenceStatus: "sufficient",
		Findings: []FindingOut{
			{Path: "a.go", Line: 1, Severity: SeverityP2, Category: "correctness", Title: "one", Body: "b", Evidence: "e", Confidence: ConfidenceHigh},
		},
		EvidenceGaps: []string{}, Summary: "review",
	}
	channel := completedNode("rv.std.a", KindReviewStd, reviewNodeParams{Package: "a", Channel: KindReviewStd, Files: []string{"a.go"}, Focus: "f", Provider: "claude"}, review)
	wf := dag.WorkflowView{
		ID:    "wf-gate",
		Spec:  dag.GraphSpec{Budget: dag.Budget{MaxNodes: 30}},
		Nodes: map[string]dag.NodeView{"rv.std.a": channel},
		Order: []string{"rv.std.a"},
	}
	gateParamsRaw, _ := json.Marshal(gateParams{Package: "a", Files: []string{"a.go"}, Channels: []channelRef{{Node: "rv.std.a", Kind: KindReviewStd}}})
	runnerCalls := 0
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  dossier,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			runnerCalls++
			gate := ReviewGateOut{
				Verdict:  "PASS",
				Accepted: []Ref{{Node: "rv.std.a", Index: 0}},
				Rebutted: []Rebut{},
				Summary:  "ok",
			}
			raw, _ := json.Marshal(gate)
			decoded, err := spec.Submit.Decode(raw)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
			}
			return &agent.RunResult{Output: decoded}, nil
		},
	})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: "wf-gate",
		Node:       dag.NodeSpec{ID: "rv.gate.a", Kind: KindReviewGate, Params: gateParamsRaw},
	})
	if err != nil {
		t.Fatalf("Execute gate: %v", err)
	}
	if runnerCalls != 1 {
		t.Fatalf("runner calls = %d", runnerCalls)
	}
	gate, err := DecodeStrict[*ReviewGateOut](out.Data)
	if err != nil {
		t.Fatalf("decode gate: %v", err)
	}
	if len(gate.Accepted) != 1 {
		t.Fatalf("accepted = %+v", gate.Accepted)
	}
}

func TestSynthesizeI5EnforcementAndRulesFallback(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Synthesize.Mode = "agent"
	profile.Config.Synthesize.Provider = "claude"
	profile.Config.Budget.MaxExtends = 1
	dossier := validDossier(t)
	spec, err := BuildGraph(profile, dossier, GraphConfig{})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	group := Group{
		ID:       "g1",
		Origin:   Origin{Node: "cr.a", Index: 0, Provider: "local"},
		Severity: SeverityP1,
		Category: "correctness",
		Path:     "a.go",
		Line:     10,
		Side:     SideNew,
		Title:    "published finding",
		Body:     "body",
		Evidence: "evidence",
		Sources: []Finding{{
			ID:       "f1",
			Origin:   Origin{Node: "cr.a", Index: 0, Provider: "local"},
			Severity: SeverityP1,
			Category: "correctness",
			Path:     "a.go",
			Line:     10,
			Side:     SideNew,
			Title:    "published finding",
			Body:     "body",
			Evidence: "evidence",
		}},
		Decision: &FindingDecision{Action: ActionPublish, Reason: "rule publish", Source: "rules"},
		Challenge: &Challenge{
			Status:   ChallengeConfirmed,
			Evidence: []CodeRef{{Path: "a.go", StartLine: 10, EndLine: 10, Root: "head", Note: "confirmed"}},
		},
	}
	adjudicateRaw, _ := json.Marshal(AdjudicateOut{Groups: []Group{group}})
	adjudicateNode := dag.NodeView{
		ID:     NodeAdjudicate,
		Spec:   dag.NodeSpec{ID: NodeAdjudicate, Kind: KindAdjudicate},
		Status: dag.NodeCompleted,
		Output: adjudicateRaw,
	}
	wf := dag.WorkflowView{
		ID:          "wf-synth",
		Spec:        spec,
		ExtendsUsed: 0,
		Nodes:       map[string]dag.NodeView{NodeAdjudicate: adjudicateNode},
		Order:       []string{NodeAdjudicate},
	}
	newFinding := FindingOut{
		Path: "b.go", Line: 4, Severity: SeverityP2, Category: "logic",
		Title: "new finding", Body: "body", Evidence: "evidence", Confidence: ConfidenceMedium,
	}
	output := SynthesizeOut{
		Verdict:        "BLOCKING",
		Summary:        "model summary",
		Decisions:      []Decision{},
		NewFindings:    []FindingOut{newFinding},
		ExtendRequests: []ExtendRequest{},
	}
	outputRaw, _ := json.Marshal(output)
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  dossier,
		Profile:  profile,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			_, err := spec.Submit.Decode(outputRaw)
			if err == nil {
				t.Fatalf("unverified new finding was accepted")
			}
			return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
		},
	})
	synthNode := mustNode(t, spec, NodeSynth)
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: "wf-synth",
		Node:       synthNode,
	})
	if err != nil {
		t.Fatalf("Execute synthesize: %v", err)
	}
	result, err := decodeSynthesisResult(out.Data)
	if err != nil {
		t.Fatalf("decode synthesis result: %v", err)
	}
	if !result.Fallback {
		t.Fatalf("expected rules fallback")
	}
	if len(result.Output.NewFindings) != 0 {
		t.Fatalf("fallback published unverified findings: %+v", result.Output.NewFindings)
	}
	if len(result.Warnings) == 0 {
		t.Fatalf("expected fallback warnings")
	}
	for _, group := range result.Groups {
		if group.Origin.Main {
			t.Fatalf("unverified finding leaked into fallback groups")
		}
	}
	if result.Verdict != VerdictBlocking {
		t.Fatalf("verdict = %q, want BLOCKING", result.Verdict)
	}
}

func TestSynthesizeSecondPassAcceptsInheritedBudget(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Synthesize.Mode = "agent"
	profile.Config.Synthesize.Provider = "claude"
	profile.Config.Budget.MaxExtends = 1
	graphCfg := GraphConfig{AllowedProviders: []string{"local", "claude"}}
	dossier := validDossier(t)
	spec, err := BuildGraph(profile, dossier, graphCfg)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	expanders, err := BuildExpanders(profile, dossier, graphCfg)
	if err != nil {
		t.Fatalf("BuildExpanders: %v", err)
	}
	group := Group{
		ID:       "g1",
		Severity: SeverityP1,
		Category: "correctness",
		Path:     "a.go",
		Line:     10,
		Side:     SideNew,
		Title:    "bug",
		Body:     "body",
		Evidence: "evidence",
		Sources: []Finding{{
			ID: "f1", Origin: Origin{Node: "cr.a", Index: 0, Provider: "local"},
			Severity: SeverityP1, Category: "correctness", Path: "a.go", Line: 10, Side: SideNew,
			Title: "bug", Body: "body", Evidence: "evidence",
		}},
		Decision:  &FindingDecision{Action: ActionPublish, Reason: "publish", Source: "rules"},
		Challenge: &Challenge{Status: ChallengeConfirmed},
	}
	first := SynthesisResult{
		Verdict: VerdictBlocking,
		Summary: "first summary",
		Groups:  []Group{group},
		Output: SynthesizeOut{
			Verdict:        VerdictBlocking,
			Summary:        "first summary",
			Decisions:      []Decision{},
			NewFindings:    []FindingOut{},
			ExtendRequests: []ExtendRequest{{Kind: "verify", FindingID: "g1", Focus: "double check"}},
		},
	}
	firstRaw, _ := json.Marshal(first)
	adjudicatedRaw, _ := json.Marshal(AdjudicateOut{Groups: []Group{group}})
	synthNode := mustNode(t, spec, NodeSynth)
	wf := dag.WorkflowView{
		ID:          "wf-synth2-budget",
		Spec:        spec,
		ExtendsUsed: 0,
		Nodes: map[string]dag.NodeView{
			NodeAdjudicate: {ID: NodeAdjudicate, Spec: mustNode(t, spec, NodeAdjudicate), Status: dag.NodeCompleted, Output: adjudicatedRaw},
			NodeSynth:      {ID: NodeSynth, Spec: synthNode, Status: dag.NodeCompleted, Attempt: 1, Output: firstRaw},
		},
		Order: []string{NodeAdjudicate, NodeSynth},
	}
	ext, err := expanders[KindSynthesize].Expand(context.Background(), wf, synthNode, firstRaw)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	var synth2 *dag.NodeSpec
	for i := range ext.Nodes {
		if ext.Nodes[i].ID == NodeSynth2 {
			synth2 = &ext.Nodes[i]
			break
		}
	}
	if synth2 == nil {
		t.Fatalf("missing synthesize.2 node: %+v", ext.Nodes)
	}
	params, err := decodeParams[synthesizeParams](synth2.Params)
	if err != nil || params.MaxExtends != 1 {
		t.Fatalf("synthesize.2 params = %+v, err=%v", params, err)
	}

	// The second pass runs after the extension was applied.
	wf.ExtendsUsed = 1
	modelOut := SynthesizeOut{
		Verdict:        VerdictBlocking,
		Summary:        "model second summary",
		Decisions:      []Decision{},
		NewFindings:    []FindingOut{},
		ExtendRequests: []ExtendRequest{},
	}
	modelRaw, _ := json.Marshal(modelOut)
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  dossier,
		Profile:  profile,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			decoded, err := spec.Submit.Decode(modelRaw)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
			}
			return &agent.RunResult{Output: decoded}, nil
		},
	})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: wf.ID,
		Node:       *synth2,
	})
	if err != nil {
		t.Fatalf("Execute synthesize.2: %v", err)
	}
	result, err := decodeSynthesisResult(out.Data)
	if err != nil {
		t.Fatalf("decode synthesis result: %v", err)
	}
	if result.Fallback {
		t.Fatalf("synthesize.2 fell back to rules despite a valid model output")
	}
	if result.Summary != modelOut.Summary {
		t.Fatalf("summary = %q, want model output", result.Summary)
	}
}

func TestSynthesizeSecondPassRejectsExtendRequest(t *testing.T) {
	out := SynthesizeOut{
		Verdict:        VerdictPass,
		Summary:        "second",
		Decisions:      []Decision{},
		NewFindings:    []FindingOut{},
		ExtendRequests: []ExtendRequest{{Kind: "verify", FindingID: "g1", Focus: "again"}},
	}
	ctx := SynthesisContext{
		WorkflowID:  "wf-synth2-reject",
		NodeID:      NodeSynth2,
		Groups:      []Group{},
		MaxExtends:  2,
		ExtendsUsed: 1,
	}
	err := ValidateSynthesis(out, ctx)
	if err == nil || !strings.Contains(err.Error(), "synthesize.2 cannot extend again") {
		t.Fatalf("extend request error = %v", err)
	}
}

func TestOnboardingOutputValidation(t *testing.T) {
	valid := OnboardingOut{
		Overview:     "overview",
		Architecture: "architecture",
		Modules:      []Module{{Path: "internal", Purpose: "core"}},
		Conventions:  []string{"testing"},
		BuildAndTest: []string{"go test ./..."},
		RiskHotspots: []string{"none"},
		ReviewFocus:  []string{"contracts"},
	}
	validRaw, _ := json.Marshal(valid)
	runnerCalls := 0
	executor := newTestExecutor(t, ExecutorDeps{
		Profile: onboardingProfile(),
		AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			runnerCalls++
			decoded, err := spec.Submit.Decode(validRaw)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
			}
			return &agent.RunResult{Output: decoded, Usage: llm.Usage{InputTokens: 4, OutputTokens: 5}}, nil
		},
	})
	paramsRaw, _ := json.Marshal(onboardingParams{Provider: "claude"})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: "wf-onboarding",
		Node:       dag.NodeSpec{ID: KindOnboarding, Kind: KindOnboarding, Params: paramsRaw},
	})
	if err != nil {
		t.Fatalf("Execute onboarding: %v", err)
	}
	if runnerCalls != 1 {
		t.Fatalf("runner calls = %d", runnerCalls)
	}
	decoded, err := DecodeStrict[*OnboardingOut](out.Data)
	if err != nil {
		t.Fatalf("decode onboarding: %v", err)
	}
	if decoded.Overview != "overview" {
		t.Fatalf("overview = %q", decoded.Overview)
	}
	if out.Usage.InputTokens != 4 || out.Usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

func TestOnboardingOutputValidationRejectsInvalidSubmit(t *testing.T) {
	executor := newTestExecutor(t, ExecutorDeps{
		Profile: onboardingProfile(),
		AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			_, err := spec.Submit.Decode([]byte(`{}`))
			if err == nil {
				t.Fatalf("invalid onboarding output was accepted")
			}
			return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
		},
	})
	paramsRaw, _ := json.Marshal(onboardingParams{Provider: "claude"})
	_, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: "wf-onboarding",
		Node:       dag.NodeSpec{ID: KindOnboarding, Kind: KindOnboarding, Params: paramsRaw},
	})
	if err == nil || !errors.Is(err, agent.ErrInvalidSubmit) {
		t.Fatalf("expected ErrInvalidSubmit, got %v", err)
	}
}

func TestExecutorRetriesTransportErrorsOnly(t *testing.T) {
	valid := OnboardingOut{
		Overview: "o", Architecture: "a", Modules: []Module{}, Conventions: []string{},
		BuildAndTest: []string{}, RiskHotspots: []string{}, ReviewFocus: []string{},
	}
	validRaw, _ := json.Marshal(valid)
	calls := 0
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:     onboardingProfile(),
		MaxAttempts: 3,
		Backoff:     time.Microsecond,
		Sleep:       func(context.Context, time.Duration) error { return nil },
		AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			calls++
			if calls == 1 {
				return nil, &llm.Error{Kind: llm.ErrTransient}
			}
			decoded, err := spec.Submit.Decode(validRaw)
			if err != nil {
				return nil, err
			}
			return &agent.RunResult{Output: decoded}, nil
		},
	})
	paramsRaw, _ := json.Marshal(onboardingParams{Provider: "claude"})
	if _, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: "wf-retry",
		Node:       dag.NodeSpec{ID: KindOnboarding, Kind: KindOnboarding, Params: paramsRaw},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestAdjudicateNodeAppliesVerifyResult(t *testing.T) {
	group := Group{
		ID:       "g1",
		Severity: SeverityP1,
		Category: "correctness",
		Path:     "a.go",
		Line:     10,
		Side:     SideNew,
		Title:    "bug",
		Body:     "body",
		Evidence: "evidence",
		Sources: []Finding{{
			ID: "f1", Origin: Origin{Node: "cr.a", Index: 0, Provider: "local"},
			Severity: SeverityP1, Category: "correctness", Path: "a.go", Line: 10, Side: SideNew,
			Title: "bug", Body: "body", Evidence: "evidence",
		}},
		Challenge: &Challenge{Status: ChallengePending},
	}
	gathered := GatherOut{
		Groups:      []Group{group},
		ToChallenge: []ChallengeTarget{{GroupID: "g1", Sequence: 1, Provider: "claude"}},
	}
	verify := VerifyResult{
		GroupID:  "g1",
		Provider: "claude",
		Result:   VerifyOut{Status: "CONFIRMED", Reason: "reproduced", Evidence: []CodeRef{}},
	}
	wf := dag.WorkflowView{
		ID:   "wf-adjudicate",
		Spec: dag.GraphSpec{Budget: dag.Budget{MaxNodes: 30}},
		Nodes: map[string]dag.NodeView{
			NodeGather: completedNode(NodeGather, KindGather, gatherParams{}, gathered),
			"verify.1": completedNode("verify.1", KindVerify, verifyParams{GroupID: "g1", Provider: "claude"}, verify),
		},
		Order: []string{NodeGather, "verify.1"},
	}
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  validDossier(t),
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		Locate:   func(string, int, Side) bool { return true },
	})
	paramsRaw, _ := json.Marshal(adjudicateParams{ChallengeOnly: []Severity{SeverityP0, SeverityP1}, OnUncertain: OnUncertainMark})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: "wf-adjudicate",
		Node:       dag.NodeSpec{ID: NodeAdjudicate, Kind: KindAdjudicate, Params: paramsRaw},
	})
	if err != nil {
		t.Fatalf("Execute adjudicate: %v", err)
	}
	adjudicated, err := decodeAdjudicateOut(out.Data)
	if err != nil {
		t.Fatalf("decode adjudicate: %v", err)
	}
	if len(adjudicated.Groups) != 1 {
		t.Fatalf("groups = %d", len(adjudicated.Groups))
	}
	got := adjudicated.Groups[0]
	if got.Challenge == nil || got.Challenge.Status != ChallengeConfirmed {
		t.Fatalf("challenge = %+v", got.Challenge)
	}
	if got.Decision == nil || got.Decision.Action != ActionPublish {
		t.Fatalf("decision = %+v", got.Decision)
	}
	if !got.PositionOK {
		t.Fatalf("expected a positioned finding")
	}
}

// metadataClients resolves fixed provider metadata for attribution tests.
type metadataClients map[string]llm.Client

func (c metadataClients) Client(name string) (llm.Client, error) {
	client, ok := c[name]
	if !ok {
		return nil, fmt.Errorf("provider %q is not configured", name)
	}
	return client, nil
}

type metadataClient struct{ model string }

func (c metadataClient) Generate(context.Context, *llm.Request) (*llm.Response, error) {
	return nil, errors.New("unexpected model call")
}

func (c metadataClient) Info() llm.ProviderInfo { return llm.ProviderInfo{Model: c.model} }

func TestExecutorNodeMetadata(t *testing.T) {
	executor := newTestExecutor(t, ExecutorDeps{
		Profile: deepProfile(),
		Clients: metadataClients{
			"claude": metadataClient{model: "claude-model"},
			"local":  metadataClient{model: "local-model"},
		},
	})
	params := func(value any) json.RawMessage {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	tests := []struct {
		name     string
		kind     string
		params   json.RawMessage
		provider string
		model    string
	}{
		{name: "plan code", kind: KindPlan, params: params(planParams{Mode: "code", Provider: "claude"})},
		{name: "plan agent", kind: KindPlan, params: params(planParams{Mode: "agent", Provider: "claude"}), provider: "claude", model: "claude-model"},
		{name: "code review", kind: KindCodeReview, params: params(reviewNodeParams{Provider: "local"}), provider: "local", model: "local-model"},
		{name: "review std", kind: KindReviewStd, params: params(reviewNodeParams{Provider: "claude"}), provider: "claude", model: "claude-model"},
		{name: "review intent", kind: KindReviewInt, params: params(reviewNodeParams{Provider: "claude"}), provider: "claude", model: "claude-model"},
		{name: "review gate", kind: KindReviewGate, params: params(gateParams{}), provider: "claude", model: "claude-model"},
		{name: "reasoner explicit", kind: KindReasoner, params: params(reasonerParams{Provider: "local", When: "always"}), provider: "local", model: "local-model"},
		{name: "reasoner profile fallback", kind: KindReasoner, params: params(reasonerParams{When: "always"}), provider: "claude", model: "claude-model"},
		{name: "verify", kind: KindVerify, params: params(verifyParams{Provider: "claude"}), provider: "claude", model: "claude-model"},
		{name: "synthesize code", kind: KindSynthesize, params: params(synthesizeParams{Mode: "code", Provider: "claude"})},
		{name: "synthesize agent", kind: KindSynthesize, params: params(synthesizeParams{Mode: "agent", Provider: "local"}), provider: "local", model: "local-model"},
		{name: "synthesize profile fallback", kind: KindSynthesize, params: params(synthesizeParams{Mode: "agent"}), provider: "claude", model: "claude-model"},
		{name: "onboarding", kind: KindOnboarding, params: params(onboardingParams{Provider: "claude"}), provider: "claude", model: "claude-model"},
		{name: "onboarding profile fallback", kind: KindOnboarding, params: params(onboardingParams{})},
		{name: "gather", kind: KindGather, params: params(gatherParams{ChallengeProvider: "claude"})},
		{name: "adjudicate", kind: KindAdjudicate, params: params(adjudicateParams{})},
		{name: "unknown kind", kind: "bogus"},
		{name: "invalid params", kind: KindVerify, params: json.RawMessage(`{`)},
		{name: "trailing params", kind: KindCodeReview, params: json.RawMessage(`{"provider":"local"}{"x":1}`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := executor.NodeMetadata(dag.NodeInput{Node: dag.NodeSpec{ID: "n", Kind: tc.kind, Params: tc.params}})
			if got.Provider != tc.provider || got.Model != tc.model {
				t.Fatalf("metadata = (%q,%q), want (%q,%q)", got.Provider, got.Model, tc.provider, tc.model)
			}
		})
	}
}

func TestExecutorNodeMetadataOnboardingProfileFallback(t *testing.T) {
	executor := newTestExecutor(t, ExecutorDeps{
		Profile: onboardingProfile(),
		Clients: metadataClients{"claude": metadataClient{model: "claude-model"}},
	})
	raw, err := json.Marshal(onboardingParams{})
	if err != nil {
		t.Fatal(err)
	}
	got := executor.NodeMetadata(dag.NodeInput{Node: dag.NodeSpec{ID: "onboarding", Kind: KindOnboarding, Params: raw}})
	if got.Provider != "claude" || got.Model != "claude-model" {
		t.Fatalf("metadata = (%q,%q), want (claude,claude-model)", got.Provider, got.Model)
	}
}
