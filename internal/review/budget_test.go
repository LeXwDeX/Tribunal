package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/agent"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

// runReviewWorkflow drives a full review graph with the deterministic review
// executor. The fake runner is the only model boundary, so call counts prove
// that budget exhaustion never reaches a provider.
func runReviewWorkflow(t *testing.T, profile Profile, maxNodes int, maxTokens int64, runner AgentRunner) dag.WorkflowView {
	t.Helper()
	profile.Config.Budget.MaxNodes = maxNodes
	profile.Config.Budget.MaxTokens = maxTokens
	dossier := validDossier(t)
	graphCfg := GraphConfig{AllowedProviders: []string{"local", "claude"}}
	spec, err := BuildGraph(profile, dossier, graphCfg)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	store := dag.NewMemoryEventStore()
	var engine *dag.Engine
	executor, err := NewExecutor(ExecutorDeps{
		Dossier:  dossier,
		Profile:  profile,
		Graph:    graphCfg,
		AgentRun: runner,
		Snapshot: func(ctx context.Context, id string) (dag.WorkflowView, error) {
			return engine.Snapshot(ctx, id)
		},
		Locate: func(string, int, Side) bool { return true },
	})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	engine = dag.NewEngine(store, executor, executor, dag.Options{})
	wfID, err := engine.Create(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	view, err := engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return view
}

func finding(path string, line int, severity Severity, title string) FindingOut {
	return FindingOut{
		Path:       path,
		Line:       line,
		Severity:   severity,
		Category:   "correctness",
		Title:      title,
		Body:       "body for " + title,
		Evidence:   "evidence for " + title,
		Confidence: ConfidenceHigh,
	}
}

func decodeNodeOutput[T any](t *testing.T, view dag.WorkflowView, nodeID string) T {
	t.Helper()
	node, ok := view.Nodes[nodeID]
	if !ok || node.Status != dag.NodeCompleted {
		t.Fatalf("node %s: %+v", nodeID, node)
	}
	var out T
	if err := json.Unmarshal(node.Output, &out); err != nil {
		t.Fatalf("decode %s: %v", nodeID, err)
	}
	return out
}

func countNodePrefix(view dag.WorkflowView, prefix string) int {
	count := 0
	for id := range view.Nodes {
		if strings.HasPrefix(id, prefix) {
			count++
		}
	}
	return count
}

func TestGatherBoundsVerifyFanOutByMaxNodes(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Plan.MaxPackages = 2
	profile.Config.Plan.PackageTokens = 1
	findings := []FindingOut{
		finding("a.go", 10, SeverityP1, "p1 first"),
		finding("a.go", 20, SeverityP1, "p1 second"),
		finding("a.go", 30, SeverityP1, "p1 third"),
		finding("a.go", 40, SeverityP1, "p1 fourth"),
		finding("a.go", 50, SeverityP0, "p0 first"),
		finding("a.go", 60, SeverityP0, "p0 second"),
	}
	var mu sync.Mutex
	calls := map[string]int{}
	runner := func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		nodeID := llm.CallMetaFromContext(ctx).NodeID
		mu.Lock()
		calls[nodeID]++
		mu.Unlock()
		switch {
		case strings.HasPrefix(nodeID, "cr."):
			out := CodeReviewOut{Summary: "review", Findings: []FindingOut{}}
			if nodeID == "cr.p1" {
				out.Findings = findings
			}
			raw, _ := json.Marshal(out)
			decoded, err := spec.Submit.Decode(raw)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
			}
			return &agent.RunResult{Output: decoded}, nil
		case strings.HasPrefix(nodeID, "verify."):
			raw, _ := json.Marshal(VerifyOut{Status: "UNCERTAIN", Reason: "evidence is inconclusive", Evidence: []CodeRef{}})
			decoded, err := spec.Submit.Decode(raw)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
			}
			return &agent.RunResult{Output: decoded}, nil
		default:
			return nil, fmt.Errorf("unexpected model call for %s", nodeID)
		}
	}

	view := runReviewWorkflow(t, profile, 8, 0, runner)
	if view.Status != dag.WorkflowCompleted {
		t.Fatalf("workflow = %s (%s)", view.Status, view.Error)
	}
	if got := countNodePrefix(view, "verify."); got != 2 {
		t.Fatalf("verify nodes = %d, want 2", got)
	}

	gathered := decodeNodeOutput[GatherOut](t, view, NodeGather)
	if len(gathered.Groups) != len(findings) {
		t.Fatalf("groups = %d, want %d", len(gathered.Groups), len(findings))
	}
	if len(gathered.ToChallenge) != 2 {
		t.Fatalf("challenges = %d, want 2", len(gathered.ToChallenge))
	}
	byID := make(map[string]Group, len(gathered.Groups))
	for _, group := range gathered.Groups {
		byID[group.ID] = group
	}
	for _, target := range gathered.ToChallenge {
		if byID[target.GroupID].Severity != SeverityP0 {
			t.Fatalf("target %s severity = %s, want P0 priority", target.GroupID, byID[target.GroupID].Severity)
		}
	}
	if !warningsContain(gathered.Warnings, "max_nodes") {
		t.Fatalf("gather warnings = %v", gathered.Warnings)
	}

	adjudicated := decodeNodeOutput[AdjudicateOut](t, view, NodeAdjudicate)
	if len(adjudicated.Groups) != len(findings) {
		t.Fatalf("adjudicated groups = %d", len(adjudicated.Groups))
	}
	for _, group := range adjudicated.Groups {
		if group.Challenge == nil || group.Challenge.Status != ChallengeUncertain {
			t.Fatalf("group %s challenge = %+v, want uncertain", group.ID, group.Challenge)
		}
		if group.Decision == nil || group.Decision.Action != ActionMark {
			t.Fatalf("group %s decision = %+v, want mark", group.ID, group.Decision)
		}
	}

	result := decodeNodeOutput[SynthesisResult](t, view, NodeSynth)
	if result.Verdict != VerdictBlocking {
		t.Fatalf("verdict = %s, want BLOCKING", result.Verdict)
	}
	if !strings.Contains(result.Summary, "未核验") {
		t.Fatalf("summary missing unverified caveat: %q", result.Summary)
	}
	if !warningsContain(result.Warnings, "max_nodes") {
		t.Fatalf("synthesis warnings = %v", result.Warnings)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["cr.p1"] != 1 || calls["cr.p2"] != 1 || len(calls) != 4 {
		t.Fatalf("model calls = %v", calls)
	}
}

func TestProposalBudgetExhaustionSettlesWithoutLLM(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Plan.MaxPackages = 1
	var mu sync.Mutex
	calls := map[string]int{}
	runner := func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		nodeID := llm.CallMetaFromContext(ctx).NodeID
		mu.Lock()
		calls[nodeID]++
		mu.Unlock()
		if nodeID != "cr.p1" {
			return nil, fmt.Errorf("unexpected model call for %s", nodeID)
		}
		raw, _ := json.Marshal(CodeReviewOut{
			Summary:  "review",
			Findings: []FindingOut{finding("a.go", 10, SeverityP1, "budget proposal")},
		})
		decoded, err := spec.Submit.Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
		}
		return &agent.RunResult{Output: decoded, Usage: llm.Usage{InputTokens: 100, OutputTokens: 20}}, nil
	}

	view := runReviewWorkflow(t, profile, 30, 10, runner)
	if view.Status != dag.WorkflowCompleted {
		t.Fatalf("workflow = %s (%s)", view.Status, view.Error)
	}
	mu.Lock()
	if calls["cr.p1"] != 1 || len(calls) != 1 {
		mu.Unlock()
		t.Fatalf("model calls = %v, want only the proposal", calls)
	}
	mu.Unlock()

	gathered := decodeNodeOutput[GatherOut](t, view, NodeGather)
	if len(gathered.ToChallenge) != 0 {
		t.Fatalf("challenges after budget exhaustion = %+v", gathered.ToChallenge)
	}
	if len(gathered.Groups) != 1 || gathered.Groups[0].Challenge == nil || gathered.Groups[0].Challenge.Status != ChallengeSkipped {
		t.Fatalf("gather group = %+v", gathered.Groups)
	}
	result := decodeNodeOutput[SynthesisResult](t, view, NodeSynth)
	if result.Verdict != VerdictBlocking {
		t.Fatalf("verdict = %s, want BLOCKING", result.Verdict)
	}
	if !strings.Contains(result.Summary, "未核验") {
		t.Fatalf("summary missing unverified caveat: %q", result.Summary)
	}
	if !warningsContain(result.Warnings, "budget") {
		t.Fatalf("synthesis warnings = %v", result.Warnings)
	}
}

func TestVerifyBudgetExhaustionMarksUnverified(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Plan.MaxPackages = 1
	var mu sync.Mutex
	calls := map[string]int{}
	runner := func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		nodeID := llm.CallMetaFromContext(ctx).NodeID
		mu.Lock()
		calls[nodeID]++
		mu.Unlock()
		switch {
		case nodeID == "cr.p1":
			raw, _ := json.Marshal(CodeReviewOut{
				Summary:  "review",
				Findings: []FindingOut{finding("a.go", 10, SeverityP1, "verify budget")},
			})
			decoded, err := spec.Submit.Decode(raw)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
			}
			return &agent.RunResult{Output: decoded}, nil
		case strings.HasPrefix(nodeID, "verify."):
			raw, _ := json.Marshal(VerifyOut{Status: "UNCERTAIN", Reason: "not enough evidence", Evidence: []CodeRef{}})
			decoded, err := spec.Submit.Decode(raw)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
			}
			return &agent.RunResult{Output: decoded, Usage: llm.Usage{InputTokens: 100, OutputTokens: 20}}, nil
		default:
			return nil, fmt.Errorf("unexpected model call for %s", nodeID)
		}
	}

	view := runReviewWorkflow(t, profile, 30, 50, runner)
	if view.Status != dag.WorkflowCompleted {
		t.Fatalf("workflow = %s (%s)", view.Status, view.Error)
	}
	mu.Lock()
	if calls["cr.p1"] != 1 || len(calls) != 2 {
		mu.Unlock()
		t.Fatalf("model calls = %v, want proposal and one verify", calls)
	}
	mu.Unlock()

	adjudicated := decodeNodeOutput[AdjudicateOut](t, view, NodeAdjudicate)
	if len(adjudicated.Groups) != 1 {
		t.Fatalf("adjudicated groups = %d", len(adjudicated.Groups))
	}
	group := adjudicated.Groups[0]
	if group.Challenge == nil || group.Challenge.Status != ChallengeUncertain {
		t.Fatalf("challenge = %+v, want uncertain", group.Challenge)
	}
	if group.Decision == nil || group.Decision.Action != ActionMark {
		t.Fatalf("decision = %+v, want mark", group.Decision)
	}
	result := decodeNodeOutput[SynthesisResult](t, view, NodeSynth)
	if result.Verdict != VerdictBlocking {
		t.Fatalf("verdict = %s, want BLOCKING", result.Verdict)
	}
	if !strings.Contains(result.Summary, "未核验") {
		t.Fatalf("summary missing unverified caveat: %q", result.Summary)
	}
	if !warningsContain(result.Warnings, "budget") {
		t.Fatalf("synthesis warnings = %v", result.Warnings)
	}
}

func TestRequiredProposalFailureStillFailsWorkflow(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Plan.MaxPackages = 1
	runner := func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		return nil, errors.New("proposal provider failed")
	}
	view := runReviewWorkflow(t, profile, 30, 0, runner)
	if view.Status != dag.WorkflowFailed {
		t.Fatalf("workflow = %s, want failed", view.Status)
	}
	if view.Nodes[NodeGather].Status != dag.NodeFailed {
		t.Fatalf("gather = %s, want failed", view.Nodes[NodeGather].Status)
	}
	if _, ok := view.Nodes[NodeSynth]; ok {
		if view.Nodes[NodeSynth].Status == dag.NodeCompleted {
			t.Fatal("synthesis completed despite required proposal failure")
		}
	}
}

func TestSynthesizeExtensionRespectsMaxNodesForMainFinding(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Synthesize.Mode = "agent"
	profile.Config.Synthesize.Provider = "claude"
	profile.Config.Budget.MaxExtends = 1
	profile.Config.Budget.MaxNodes = 4
	cfg := GraphConfig{AllowedProviders: []string{"local", "claude"}}
	dossier := validDossier(t)
	spec, err := BuildGraph(profile, dossier, cfg)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	expanders, err := BuildExpanders(profile, dossier, cfg)
	if err != nil {
		t.Fatalf("BuildExpanders: %v", err)
	}
	adjudicatedRaw, _ := json.Marshal(AdjudicateOut{Groups: []Group{}})
	newFinding := finding("a.go", 5, SeverityP1, "main new finding")
	index := 0
	output := SynthesizeOut{
		Verdict:     VerdictBlocking,
		Summary:     "summary",
		Decisions:   []Decision{},
		NewFindings: []FindingOut{newFinding},
		ExtendRequests: []ExtendRequest{{
			Kind:            "verify",
			NewFindingIndex: &index,
			Focus:           "verify the main finding",
		}},
	}
	result := SynthesisResult{
		Verdict: VerdictBlocking,
		Summary: "summary",
		Groups:  []Group{},
		Output:  output,
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("result: %v", err)
	}
	resultRaw, _ := json.Marshal(result)
	synthNode := mustNode(t, spec, NodeSynth)
	wf := dag.WorkflowView{
		ID:   "wf-synth-maxnodes",
		Spec: spec,
		Nodes: map[string]dag.NodeView{
			NodeAdjudicate: {ID: NodeAdjudicate, Spec: mustNode(t, spec, NodeAdjudicate), Status: dag.NodeCompleted, Output: adjudicatedRaw},
			NodeSynth:      {ID: NodeSynth, Spec: synthNode, Status: dag.NodeCompleted, Attempt: 1, Output: resultRaw},
		},
		Order: []string{NodeAdjudicate, NodeSynth},
	}
	ext, err := expanders[KindSynthesize].Expand(context.Background(), wf, synthNode, resultRaw)
	if err != nil {
		t.Fatalf("Expand should not fail the graph when max_nodes is full: %v", err)
	}
	for _, node := range ext.Nodes {
		if node.Kind == KindVerify || node.ID == NodeSynth2 {
			t.Fatalf("extension created %s without node capacity: %+v", node.ID, ext.Nodes)
		}
	}
}

func warningsContain(warnings []string, fragment string) bool {
	for _, warning := range warnings {
		if strings.Contains(strings.ToLower(warning), strings.ToLower(fragment)) {
			return true
		}
	}
	return false
}

func TestAdjudicateBudgetSkippedVerifyIsUncertain(t *testing.T) {
	group := Group{
		ID:       "g1",
		Severity: SeverityP1,
		Category: "correctness",
		Path:     "a.go",
		Line:     10,
		Side:     SideNew,
		Title:    "budget skipped verify",
		Body:     "body",
		Evidence: "evidence",
		Sources: []Finding{{
			ID: "f1", Origin: Origin{Node: "cr.p1", Index: 0, Provider: "local"},
			Severity: SeverityP1, Category: "correctness", Path: "a.go", Line: 10, Side: SideNew,
			Title: "budget skipped verify", Body: "body", Evidence: "evidence",
		}},
		Challenge: &Challenge{Status: ChallengePending},
	}
	gathered := GatherOut{
		Groups:      []Group{group},
		ToChallenge: []ChallengeTarget{{GroupID: "g1", Sequence: 1, Provider: "claude"}},
	}
	verifyParamsRaw, _ := json.Marshal(verifyParams{GroupID: "g1", Provider: "claude", Sequence: 1})
	wf := dag.WorkflowView{
		ID:   "wf-skipped-verify",
		Spec: dag.GraphSpec{Budget: dag.Budget{MaxNodes: 30, MaxTokens: 10}},
		Nodes: map[string]dag.NodeView{
			NodeGather: completedNode(NodeGather, KindGather, gatherParams{}, gathered),
			"verify.1": {
				ID: "verify.1", Spec: dag.NodeSpec{ID: "verify.1", Kind: KindVerify, Params: verifyParamsRaw},
				Status: dag.NodeSkipped, Reason: "budget_exhausted",
			},
		},
		Order: []string{NodeGather, "verify.1"},
		Usage: dag.Usage{InputTokens: 10},
	}
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  validDossier(t),
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
	})
	paramsRaw, _ := json.Marshal(adjudicateParams{ChallengeOnly: []Severity{SeverityP0, SeverityP1}, OnUncertain: OnUncertainMark})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: wf.ID,
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
	if got.Challenge == nil || got.Challenge.Status != ChallengeUncertain {
		t.Fatalf("challenge = %+v, want uncertain", got.Challenge)
	}
	if got.Decision == nil || got.Decision.Action != ActionMark {
		t.Fatalf("decision = %+v, want mark", got.Decision)
	}
	if !warningsContain(adjudicated.Warnings, "budget_exhausted") {
		t.Fatalf("warnings = %v", adjudicated.Warnings)
	}

	result, err := buildCodeSynthesis(adjudicated.Groups, synthesizeParams{OnUncertain: string(OnUncertainMark)}, adjudicated.Warnings)
	if err != nil {
		t.Fatalf("buildCodeSynthesis: %v", err)
	}
	if result.Verdict != VerdictBlocking {
		t.Fatalf("verdict = %s, want BLOCKING", result.Verdict)
	}
	if !strings.Contains(result.Summary, "未核验") {
		t.Fatalf("summary missing caveat: %q", result.Summary)
	}
}

func TestMainFindingMarkRequiresVerification(t *testing.T) {
	group := Group{
		ID:       "main",
		Origin:   Origin{Node: NodeSynth, Main: true},
		Severity: SeverityP1,
		Decision: &FindingDecision{Action: ActionMark, Reason: "uncertain"},
	}
	out := SynthesizeOut{Verdict: VerdictBlocking, Summary: "summary"}
	if err := validateMainChallenges(&out, []Group{group}); err == nil {
		t.Fatal("main finding without verification was allowed to be marked")
	}
	group.Challenge = &Challenge{Status: ChallengeConfirmed}
	if err := validateMainChallenges(&out, []Group{group}); err != nil {
		t.Fatalf("confirmed main finding rejected: %v", err)
	}
}

func TestSynthExpanderMainFindingWithCapacityCreatesVerification(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Synthesize.Mode = "agent"
	profile.Config.Synthesize.Provider = "claude"
	profile.Config.Budget.MaxExtends = 1
	cfg := GraphConfig{AllowedProviders: []string{"local", "claude"}}
	dossier := validDossier(t)
	spec, err := BuildGraph(profile, dossier, cfg)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	expanders, err := BuildExpanders(profile, dossier, cfg)
	if err != nil {
		t.Fatalf("BuildExpanders: %v", err)
	}
	adjudicatedRaw, _ := json.Marshal(AdjudicateOut{Groups: []Group{}})
	index := 0
	output := SynthesizeOut{
		Verdict:     VerdictBlocking,
		Summary:     "summary",
		Decisions:   []Decision{},
		NewFindings: []FindingOut{finding("a.go", 5, SeverityP1, "main new finding")},
		ExtendRequests: []ExtendRequest{{
			Kind: "verify", NewFindingIndex: &index, Focus: "verify the main finding",
		}},
	}
	result := SynthesisResult{Verdict: VerdictBlocking, Summary: "summary", Groups: []Group{}, Output: output}
	if err := result.Validate(); err != nil {
		t.Fatalf("result: %v", err)
	}
	resultRaw, _ := json.Marshal(result)
	synthNode := mustNode(t, spec, NodeSynth)
	wf := dag.WorkflowView{
		ID:   "wf-synth-capacity",
		Spec: spec,
		Nodes: map[string]dag.NodeView{
			NodeAdjudicate: {ID: NodeAdjudicate, Spec: mustNode(t, spec, NodeAdjudicate), Status: dag.NodeCompleted, Output: adjudicatedRaw},
			NodeSynth:      {ID: NodeSynth, Spec: synthNode, Status: dag.NodeCompleted, Attempt: 1, Output: resultRaw},
		},
		Order: []string{NodeAdjudicate, NodeSynth},
	}
	ext, err := expanders[KindSynthesize].Expand(context.Background(), wf, synthNode, resultRaw)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	var verify, synth2 bool
	for _, node := range ext.Nodes {
		switch {
		case node.Kind == KindVerify:
			verify = true
		case node.ID == NodeSynth2:
			synth2 = true
		}
	}
	if !verify || !synth2 {
		t.Fatalf("extension = %+v, want verify and synthesize.2", ext.Nodes)
	}
}

func TestVerificationCaveatKeepsFirstParagraph(t *testing.T) {
	summary := withVerificationCaveat("first paragraph\n\nsecond paragraph", []string{"budget exhausted: some findings were not verified"})
	if !strings.HasPrefix(summary, "first paragraph ") || !strings.Contains(summary, "未核验") || !strings.HasSuffix(summary, "\n\nsecond paragraph") {
		t.Fatalf("caveat summary = %q", summary)
	}
	long := strings.Repeat("x", MaxSummaryRunes)
	capped := withVerificationCaveat(long, []string{"max_nodes limit reached"})
	if len([]rune(capped)) > MaxSummaryRunes {
		t.Fatalf("capped runes = %d", len([]rune(capped)))
	}
	if !strings.Contains(capped, "未核验") {
		t.Fatalf("caveat was truncated away: %q", capped)
	}
}

func TestSynthesizeReportsUnscheduledExtensionsAtMaxNodes(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Synthesize.Mode = "agent"
	profile.Config.Synthesize.Provider = "claude"
	profile.Config.Budget.MaxExtends = 1
	profile.Config.Budget.MaxNodes = 4
	cfg := GraphConfig{AllowedProviders: []string{"local", "claude"}}
	dossier := validDossier(t)
	spec, err := BuildGraph(profile, dossier, cfg)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	adjudicatedRaw, _ := json.Marshal(AdjudicateOut{Groups: []Group{}})
	wf := dag.WorkflowView{
		ID:   "wf-synth-report",
		Spec: spec,
		Nodes: map[string]dag.NodeView{
			NodeAdjudicate: {ID: NodeAdjudicate, Spec: mustNode(t, spec, NodeAdjudicate), Status: dag.NodeCompleted, Output: adjudicatedRaw},
			NodeSynth:      {ID: NodeSynth, Spec: mustNode(t, spec, NodeSynth), Status: dag.NodeRunning},
		},
		Order: []string{NodeAdjudicate, NodeSynth},
	}
	index := 0
	output := SynthesizeOut{
		Verdict:     VerdictBlocking,
		Summary:     "model summary",
		Decisions:   []Decision{},
		NewFindings: []FindingOut{finding("a.go", 5, SeverityP1, "main new finding")},
		ExtendRequests: []ExtendRequest{{
			Kind: "verify", NewFindingIndex: &index, Focus: "verify the main finding",
		}},
	}
	outputRaw, _ := json.Marshal(output)
	calls := 0
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  dossier,
		Profile:  profile,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			calls++
			decoded, err := spec.Submit.Decode(outputRaw)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
			}
			return &agent.RunResult{Output: decoded}, nil
		},
	})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: wf.ID,
		Node:       mustNode(t, spec, NodeSynth),
	})
	if err != nil {
		t.Fatalf("Execute synthesize: %v", err)
	}
	if calls != 1 {
		t.Fatalf("model calls = %d", calls)
	}
	result, err := decodeSynthesisResult(out.Data)
	if err != nil {
		t.Fatalf("decode synthesis: %v", err)
	}
	if !warningsContain(result.Warnings, "max_nodes") {
		t.Fatalf("warnings = %v", result.Warnings)
	}
	if !strings.Contains(result.Summary, "未核验") {
		t.Fatalf("summary missing caveat: %q", result.Summary)
	}
	for _, group := range result.Groups {
		if group.Origin.Main && group.Published() {
			t.Fatalf("unverified main finding was published: %+v", group)
		}
	}
}

func TestAgentSynthesisSkipsModelWhenTokenBudgetExhausted(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Synthesize.Mode = "agent"
	profile.Config.Synthesize.Provider = "claude"
	profile.Config.Budget.MaxTokens = 10
	profile.Config.Budget.MaxNodes = 30
	dossier := validDossier(t)
	spec, err := BuildGraph(profile, dossier, GraphConfig{AllowedProviders: []string{"local", "claude"}})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	group := Group{
		ID:        "g1",
		Severity:  SeverityP1,
		Category:  "correctness",
		Path:      "a.go",
		Line:      10,
		Side:      SideNew,
		Title:     "budget skipped",
		Body:      "body",
		Challenge: &Challenge{Status: ChallengeSkipped, Reason: "budget exhausted"},
		Decision:  &FindingDecision{Action: ActionMark, Reason: "challenge skipped; on_uncertain=mark", Source: "rules"},
		Sources: []Finding{{
			ID: "f1", Origin: Origin{Node: "cr.p1", Index: 0, Provider: "local"},
			Severity: SeverityP1, Category: "correctness", Path: "a.go", Line: 10, Side: SideNew,
			Title: "budget skipped", Body: "body", Evidence: "evidence",
		}},
	}
	adjudicatedRaw, _ := json.Marshal(AdjudicateOut{Groups: []Group{group}, Warnings: []string{"budget exhausted: some findings were not verified"}})
	wf := dag.WorkflowView{
		ID:    "wf-agent-budget",
		Spec:  spec,
		Usage: dag.Usage{InputTokens: 10},
		Nodes: map[string]dag.NodeView{
			NodeAdjudicate: {ID: NodeAdjudicate, Spec: mustNode(t, spec, NodeAdjudicate), Status: dag.NodeCompleted, Output: adjudicatedRaw},
			NodeSynth:      {ID: NodeSynth, Spec: mustNode(t, spec, NodeSynth), Status: dag.NodeRunning},
		},
		Order: []string{NodeAdjudicate, NodeSynth},
	}
	calls := 0
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  dossier,
		Profile:  profile,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			calls++
			return nil, errors.New("model must not be called")
		},
	})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: wf.ID,
		Node:       mustNode(t, spec, NodeSynth),
	})
	if err != nil {
		t.Fatalf("Execute synthesize: %v", err)
	}
	if calls != 0 {
		t.Fatalf("model calls = %d, want 0", calls)
	}
	result, err := decodeSynthesisResult(out.Data)
	if err != nil {
		t.Fatalf("decode synthesis: %v", err)
	}
	if result.Verdict != VerdictBlocking {
		t.Fatalf("verdict = %s, want BLOCKING", result.Verdict)
	}
	if !strings.Contains(result.Summary, "未核验") {
		t.Fatalf("summary missing caveat: %q", result.Summary)
	}
	if !warningsContain(result.Warnings, "budget") {
		t.Fatalf("warnings = %v", result.Warnings)
	}
}

func TestGatherExpanderNoSlotsReturnsEmptyExtension(t *testing.T) {
	profile := defaultProfile()
	cfg := GraphConfig{AllowedProviders: []string{"local", "claude"}}
	dossier := validDossier(t)
	spec, err := BuildGraph(profile, dossier, cfg)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	expanders, err := BuildExpanders(profile, dossier, cfg)
	if err != nil {
		t.Fatalf("BuildExpanders: %v", err)
	}
	spec.Budget.MaxNodes = len(spec.Nodes)
	group := Group{
		ID:        "g1",
		Severity:  SeverityP1,
		Category:  "correctness",
		Path:      "a.go",
		Line:      10,
		Side:      SideNew,
		Title:     "unverified",
		Body:      "body",
		Evidence:  "evidence",
		Challenge: &Challenge{Status: ChallengePending},
	}
	out := GatherOut{
		Groups:      []Group{group},
		ToChallenge: []ChallengeTarget{{GroupID: "g1", Sequence: 1, Provider: "claude"}},
	}
	raw, _ := json.Marshal(out)
	ext, err := expanders[KindGather].Expand(context.Background(), dag.WorkflowView{
		ID: "wf-gather-no-slots", Spec: spec,
	}, mustNode(t, spec, NodeGather), raw)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(ext.Nodes) != 0 || len(ext.DepPatches) != 0 {
		t.Fatalf("extension = %+v, want empty", ext)
	}
}

func mainFindingGroup(id string) Group {
	return Group{
		ID:       id,
		Origin:   Origin{Node: NodeSynth, Main: true},
		Severity: SeverityP1,
		Category: "correctness",
		Path:     "a.go",
		Line:     10,
		Side:     SideNew,
		Title:    "main finding",
		Body:     "body",
		Evidence: "evidence",
		Sources: []Finding{{
			ID: "f1", Origin: Origin{Node: NodeSynth, Index: 0, Attempt: 1, Main: true},
			Severity: SeverityP1, Category: "correctness", Path: "a.go", Line: 10, Side: SideNew,
			Title: "main finding", Body: "body", Evidence: "evidence",
		}},
	}
}

func TestValidateMainChallengesRejectsEveryPublishingAction(t *testing.T) {
	for _, action := range []Action{ActionPublish, ActionMark, ActionDowngrade} {
		for _, status := range []ChallengeStatus{"", ChallengePending, ChallengeFailed, ChallengeSkipped} {
			t.Run(string(action)+"/"+string(status), func(t *testing.T) {
				group := mainFindingGroup("main")
				if status != "" {
					group.Challenge = &Challenge{Status: status}
				}
				group.Decision = &FindingDecision{Action: action}
				out := SynthesizeOut{
					Verdict: VerdictBlocking,
					Summary: "summary",
					Decisions: []Decision{{
						FindingID: "main", Action: string(action), Severity: SeverityP2, Reason: strings.Repeat("r", 30),
					}},
				}
				if err := validateMainChallenges(&out, []Group{group}); err == nil {
					t.Fatalf("action %s with challenge %q was accepted", action, status)
				}
			})
		}
	}
	group := mainFindingGroup("main")
	group.Challenge = &Challenge{Status: ChallengeConfirmed}
	group.Decision = &FindingDecision{Action: ActionDowngrade, Severity: SeverityP2}
	out := SynthesizeOut{Verdict: VerdictPass, Summary: "summary", Decisions: []Decision{{
		FindingID: "main", Action: "downgrade", Severity: SeverityP2, Reason: strings.Repeat("r", 30),
	}}}
	if err := validateMainChallenges(&out, []Group{group}); err != nil {
		t.Fatalf("confirmed main finding downgrade rejected: %v", err)
	}
}

func TestSynth2MainFindingDowngradeRequiresVerification(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Synthesize.Mode = "agent"
	profile.Config.Synthesize.Provider = "claude"
	profile.Config.Budget.MaxExtends = 1
	cfg := GraphConfig{AllowedProviders: []string{"local", "claude"}}
	dossier := validDossier(t)
	spec, err := BuildGraph(profile, dossier, cfg)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	main := mainFindingGroup("main")
	previous := SynthesisResult{
		Verdict: VerdictBlocking,
		Summary: "previous",
		Groups:  []Group{main},
		Output: SynthesizeOut{
			Verdict: VerdictBlocking, Summary: "previous",
			Decisions: []Decision{}, NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{},
		},
	}
	if err := previous.Validate(); err != nil {
		t.Fatalf("previous result: %v", err)
	}
	previousRaw, _ := json.Marshal(previous)
	adjudicatedRaw, _ := json.Marshal(AdjudicateOut{Groups: []Group{}})
	verifyParam, _ := json.Marshal(verifyParams{GroupID: main.ID, Provider: "claude", Sequence: 1})

	for _, tc := range []struct {
		name     string
		verify   *dag.NodeView
		wantWarn string
	}{
		{name: "failed", verify: &dag.NodeView{ID: "verify.1", Spec: dag.NodeSpec{ID: "verify.1", Kind: KindVerify, Params: verifyParam}, Status: dag.NodeFailed, Error: "provider failure"}, wantWarn: "verification"},
		{name: "skipped", verify: &dag.NodeView{ID: "verify.1", Spec: dag.NodeSpec{ID: "verify.1", Kind: KindVerify, Params: verifyParam}, Status: dag.NodeSkipped, Reason: "budget_exhausted"}, wantWarn: "verification"},
		{name: "missing", verify: nil, wantWarn: "verification"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes := map[string]dag.NodeView{
				NodeAdjudicate: {ID: NodeAdjudicate, Spec: mustNode(t, spec, NodeAdjudicate), Status: dag.NodeCompleted, Output: adjudicatedRaw},
				NodeSynth:      {ID: NodeSynth, Spec: mustNode(t, spec, NodeSynth), Status: dag.NodeCompleted, Attempt: 1, Output: previousRaw},
			}
			order := []string{NodeAdjudicate, NodeSynth}
			if tc.verify != nil {
				nodes[tc.verify.ID] = *tc.verify
				order = append(order, tc.verify.ID)
			}
			wf := dag.WorkflowView{ID: "wf-main-" + tc.name, Spec: spec, Nodes: nodes, Order: order}
			output := SynthesizeOut{
				Verdict: VerdictPass,
				Summary: "model downgraded an unverified main finding",
				Decisions: []Decision{{
					FindingID: main.ID,
					Action:    "downgrade",
					Severity:  SeverityP2,
					Reason:    strings.Repeat("downgrade reason ", 3),
				}},
				NewFindings:    []FindingOut{},
				ExtendRequests: []ExtendRequest{},
			}
			outputRaw, _ := json.Marshal(output)
			params, _ := json.Marshal(synthesizeParams{
				Mode: "agent", Provider: "claude", MaxExtends: 1, OnUncertain: string(OnUncertainMark),
				PreviousNode: NodeSynth, PreviousAttempt: 1,
			})
			runnerCalls := 0
			executor := newTestExecutor(t, ExecutorDeps{
				Dossier:  dossier,
				Profile:  profile,
				Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
				AgentRun: func(_ context.Context, runSpec agent.RunSpec) (*agent.RunResult, error) {
					runnerCalls++
					if _, err := runSpec.Submit.Decode(outputRaw); err == nil {
						t.Fatalf("unverified main finding downgrade was accepted")
					}
					return nil, fmt.Errorf("%w: invalid decision", agent.ErrInvalidSubmit)
				},
			})
			out, err := executor.Execute(context.Background(), dag.NodeInput{
				WorkflowID: wf.ID,
				Node: dag.NodeSpec{
					ID: NodeSynth2, Kind: KindSynthesize, Params: params,
				},
			})
			if err != nil {
				t.Fatalf("Execute synthesize.2: %v", err)
			}
			if runnerCalls != 1 {
				t.Fatalf("runner calls = %d", runnerCalls)
			}
			result, err := decodeSynthesisResult(out.Data)
			if err != nil {
				t.Fatalf("decode synthesis: %v", err)
			}
			if !result.Fallback {
				t.Fatal("expected rules fallback")
			}
			if result.Verdict != VerdictPass {
				t.Fatalf("verdict = %s, want PASS", result.Verdict)
			}
			if !warningsContain(result.Warnings, tc.wantWarn) {
				t.Fatalf("warnings = %v", result.Warnings)
			}
			for _, group := range result.Groups {
				if group.Origin.Main && group.Published() {
					t.Fatalf("unverified main finding was published: %+v", group.Decision)
				}
			}
		})
	}
}

func TestSynthesizeMidCallBudgetFallbackStopsModel(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Synthesize.Mode = "agent"
	profile.Config.Synthesize.Provider = "claude"
	profile.Config.Budget.MaxTokens = 10
	profile.Config.Budget.MaxNodes = 30
	dossier := validDossier(t)
	spec, err := BuildGraph(profile, dossier, GraphConfig{AllowedProviders: []string{"local", "claude"}})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	group := Group{
		ID:        "g1",
		Severity:  SeverityP1,
		Category:  "correctness",
		Path:      "a.go",
		Line:      10,
		Side:      SideNew,
		Title:     "already ruled",
		Body:      "body",
		Evidence:  "evidence",
		Challenge: &Challenge{Status: ChallengeUncertain, Reason: "verify uncertain"},
		Decision:  &FindingDecision{Action: ActionMark, Reason: "on_uncertain=mark", Source: "rules"},
		Sources: []Finding{{
			ID: "f1", Origin: Origin{Node: "cr.p1", Index: 0, Provider: "local"},
			Severity: SeverityP1, Category: "correctness", Path: "a.go", Line: 10, Side: SideNew,
			Title: "already ruled", Body: "body", Evidence: "evidence",
		}},
	}
	adjudicatedRaw, _ := json.Marshal(AdjudicateOut{Groups: []Group{group}})
	usage := int64(0)
	wf := dag.WorkflowView{
		ID:   "wf-mid-budget",
		Spec: spec,
		Nodes: map[string]dag.NodeView{
			NodeAdjudicate: {ID: NodeAdjudicate, Spec: mustNode(t, spec, NodeAdjudicate), Status: dag.NodeCompleted, Output: adjudicatedRaw},
			NodeSynth:      {ID: NodeSynth, Spec: mustNode(t, spec, NodeSynth), Status: dag.NodeRunning},
		},
		Order: []string{NodeAdjudicate, NodeSynth},
	}
	output := SynthesizeOut{
		Verdict: VerdictBlocking, Summary: "model summary",
		Decisions:   []Decision{{FindingID: group.ID, Action: "publish", Reason: "model publish"}},
		NewFindings: []FindingOut{}, ExtendRequests: []ExtendRequest{},
	}
	outputRaw, _ := json.Marshal(output)
	calls := 0
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier: dossier,
		Profile: profile,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) {
			wf.Usage = dag.Usage{InputTokens: usage}
			return wf, nil
		},
		AgentRun: func(ctx context.Context, runSpec agent.RunSpec) (*agent.RunResult, error) {
			calls++
			if runSpec.BeforeTurn != nil {
				if err := runSpec.BeforeTurn(ctx, llm.Usage{}); err != nil {
					return nil, err
				}
				usage = 10
				if err := runSpec.BeforeTurn(ctx, llm.Usage{InputTokens: 10}); err != nil {
					return nil, err
				}
			}
			decoded, err := runSpec.Submit.Decode(outputRaw)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
			}
			return &agent.RunResult{Output: decoded}, nil
		},
	})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: wf.ID,
		Node:       mustNode(t, spec, NodeSynth),
	})
	if err != nil {
		t.Fatalf("Execute synthesize: %v", err)
	}
	if calls != 1 {
		t.Fatalf("model calls = %d, want 1", calls)
	}
	result, err := decodeSynthesisResult(out.Data)
	if err != nil {
		t.Fatalf("decode synthesis: %v", err)
	}
	if !result.Fallback {
		t.Fatal("mid-call budget exhaustion did not fall back to rules")
	}
	if !warningsContain(result.Warnings, "during model synthesis") {
		t.Fatalf("warnings = %v", result.Warnings)
	}
	if result.Output.Summary == "model summary" {
		t.Fatal("model output was accepted after the budget was exhausted")
	}
}

func TestSynthesizeTransportFailureRetainsUsageAndDoesNotRetry(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Synthesize.Mode = "agent"
	profile.Config.Synthesize.Provider = "claude"
	profile.Config.Budget.MaxTokens = 100
	profile.Config.Budget.MaxNodes = 30
	dossier := validDossier(t)
	spec, err := BuildGraph(profile, dossier, GraphConfig{AllowedProviders: []string{"local", "claude"}})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	group := Group{
		ID:        "g1",
		Severity:  SeverityP2,
		Category:  "correctness",
		Path:      "a.go",
		Line:      10,
		Side:      SideNew,
		Title:     "already ruled",
		Body:      "body",
		Evidence:  "evidence",
		Challenge: &Challenge{Status: ChallengeUncertain, Reason: "verify uncertain"},
		Decision:  &FindingDecision{Action: ActionMark, Reason: "on_uncertain=mark", Source: "rules"},
		Sources: []Finding{{
			ID: "f1", Origin: Origin{Node: "cr.p1", Index: 0, Provider: "local"},
			Severity: SeverityP2, Category: "correctness", Path: "a.go", Line: 10, Side: SideNew,
			Title: "already ruled", Body: "body", Evidence: "evidence",
		}},
	}
	adjudicatedRaw, _ := json.Marshal(AdjudicateOut{Groups: []Group{group}})
	wf := dag.WorkflowView{
		ID:   "wf-synth-transport",
		Spec: spec,
		Nodes: map[string]dag.NodeView{
			NodeAdjudicate: {ID: NodeAdjudicate, Spec: mustNode(t, spec, NodeAdjudicate), Status: dag.NodeCompleted, Output: adjudicatedRaw},
			NodeSynth:      {ID: NodeSynth, Spec: mustNode(t, spec, NodeSynth), Status: dag.NodeRunning},
		},
		Order: []string{NodeAdjudicate, NodeSynth},
	}
	calls := 0
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  dossier,
		Profile:  profile,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			calls++
			return &agent.RunResult{Usage: llm.Usage{InputTokens: 30, OutputTokens: 5}, Turns: 1}, &llm.Error{Kind: llm.ErrTransient}
		},
	})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: wf.ID,
		Node:       mustNode(t, spec, NodeSynth),
	})
	if err != nil {
		t.Fatalf("Execute synthesize: %v", err)
	}
	if calls != 1 {
		t.Fatalf("model attempts = %d, want 1 (no retry for synth with BeforeTurn)", calls)
	}
	result, err := decodeSynthesisResult(out.Data)
	if err != nil {
		t.Fatalf("decode synthesis: %v", err)
	}
	if !result.Fallback {
		t.Fatal("transport failure did not fall back to rules")
	}
	if out.Usage.InputTokens != 30 || out.Usage.OutputTokens != 5 {
		t.Fatalf("fallback usage = %+v, want 30+5", out.Usage)
	}
}
