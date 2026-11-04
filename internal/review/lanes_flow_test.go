package review

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func lanesProfile() Profile {
	return Profile{
		Name: "default",
		Config: config.ProfileConfig{
			Flow:        FlowLanes,
			Plan:        config.PlanConfig{Mode: "code", PackageTokens: 60000, MaxPackages: 2},
			Propose:     []config.ProposeConfig{{Block: "code-review", Provider: "local"}},
			Challenge:   config.ChallengeConfig{Provider: "claude", Only: []string{"P0", "P1"}},
			OnUncertain: "mark",
			Synthesize:  config.SynthesizeConfig{Mode: "code"},
			Budget:      config.BudgetConfig{MaxNodes: 30, MaxExtends: 0, Deadline: 30 * time.Minute},
		},
	}
}

func validExploreOut() ExploreOut {
	return ExploreOut{
		Summary: "The change adds a retry loop to the client.",
		Facts: []ExploreFact{
			{Statement: "Retry count is unbounded", Path: "a.go", Line: 3, Evidence: "for { call() }"},
		},
		Inferences: []string{"Callers may hang on persistent failure"},
		Unknowns:   []string{"Whether callers set a deadline"},
	}
}

func TestBuildGraphLanesTopology(t *testing.T) {
	spec, err := BuildGraph(lanesProfile(), validDossier(t), GraphConfig{AllowedProviders: []string{"local", "claude"}})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	ids := make([]string, 0, len(spec.Nodes))
	for _, node := range spec.Nodes {
		ids = append(ids, node.ID)
	}
	if got := strings.Join(ids, ","); got != "explore,plan,gather,adjudicate,synthesize" {
		t.Fatalf("initial nodes = %s", got)
	}
	explore := mustNode(t, spec, NodeExplore)
	if explore.Kind != KindExplore || explore.Required || explore.TokenFree || len(explore.DependsOn) != 0 || explore.Timeout != DefaultExploreTimeout {
		t.Fatalf("explore node = %+v", explore)
	}
	params, err := decodeParams[exploreParams](explore.Params)
	if err != nil || params.Provider != "local" {
		t.Fatalf("explore params = %+v, %v; want provider defaulted from code-review", params, err)
	}
	plan := mustNode(t, spec, NodePlan)
	if !plan.Expands || !plan.Required || strings.Join(plan.DependsOn, ",") != NodeExplore || !plan.TokenFree {
		t.Fatalf("plan node = %+v", plan)
	}
	gather := mustNode(t, spec, NodeGather)
	if strings.Join(gather.DependsOn, ",") != NodePlan {
		t.Fatalf("gather deps = %v; lanes are patched in by the plan expansion", gather.DependsOn)
	}
	if _, ok := nodeByID(spec, NodeReasoner); ok {
		t.Fatal("lanes flow must not add the legacy static reasoner")
	}
}

func TestBuildGraphLegacyFlowUnchanged(t *testing.T) {
	for _, flow := range []string{"", FlowLegacy} {
		profile := defaultProfile()
		profile.Config.Flow = flow
		spec, err := BuildGraph(profile, validDossier(t), GraphConfig{AllowedProviders: []string{"local", "claude"}})
		if err != nil {
			t.Fatalf("flow %q: BuildGraph: %v", flow, err)
		}
		if _, ok := nodeByID(spec, NodeExplore); ok || len(spec.Nodes) != 4 {
			t.Fatalf("flow %q: legacy graph changed: %+v", flow, spec.Nodes)
		}
		plan := mustNode(t, spec, NodePlan)
		if len(plan.DependsOn) != 0 {
			t.Fatalf("flow %q: legacy plan deps = %v", flow, plan.DependsOn)
		}
		var raw map[string]any
		if err := json.Unmarshal(plan.Params, &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["flow"]; ok {
			t.Fatalf("flow %q: legacy plan params gained a flow field: %s", flow, plan.Params)
		}
	}
}

// lanesExpansion runs the plan expander for a lanes profile with the given
// explore node state and returns the extension plus the base graph.
func lanesExpansion(t *testing.T, profile Profile, explore *dag.NodeView, packages []Package) (dag.GraphSpec, dag.Extension) {
	t.Helper()
	cfg := GraphConfig{AllowedProviders: []string{"local", "claude", "gpt"}}
	spec, err := BuildGraph(profile, validDossier(t), cfg)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	expanders, err := BuildExpanders(profile, validDossier(t), cfg)
	if err != nil {
		t.Fatalf("BuildExpanders: %v", err)
	}
	wf := dag.WorkflowView{
		ID:   "wf-lanes",
		Spec: spec,
		Nodes: map[string]dag.NodeView{
			NodePlan:   {ID: NodePlan, Spec: mustNode(t, spec, NodePlan), Status: dag.NodeCompleted},
			NodeGather: {ID: NodeGather, Spec: mustNode(t, spec, NodeGather), Status: dag.NodePending},
		},
	}
	if explore != nil {
		view := *explore
		view.Spec = mustNode(t, spec, NodeExplore)
		wf.Nodes[NodeExplore] = view
	}
	out, _ := json.Marshal(PlanOut{
		Risk: "medium", RiskReasons: []string{"code plan"}, Packages: packages,
		ChallengeOnly: []Severity{SeverityP0, SeverityP1},
	})
	ext, err := expanders[KindPlan].Expand(context.Background(), wf, mustNode(t, spec, NodePlan), out)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	candidate := spec
	candidate.Nodes = append(append([]dag.NodeSpec(nil), spec.Nodes...), ext.Nodes...)
	for i := range candidate.Nodes {
		for _, patch := range ext.DepPatches {
			if candidate.Nodes[i].ID == patch.Node {
				candidate.Nodes[i].DependsOn = append(candidate.Nodes[i].DependsOn, patch.Add...)
			}
		}
	}
	if err := dag.ValidateGraph(candidate); err != nil {
		t.Fatalf("extended graph: %v", err)
	}
	return candidate, ext
}

func completedExplore(t *testing.T) *dag.NodeView {
	t.Helper()
	raw, _ := json.Marshal(validExploreOut())
	return &dag.NodeView{ID: NodeExplore, Status: dag.NodeCompleted, Output: raw}
}

func TestLanesPlanExpansionOnePackageIsFourParallelLanes(t *testing.T) {
	_, ext := lanesExpansion(t, lanesProfile(), completedExplore(t), []Package{
		{ID: "p1", Blocks: []string{"code-review"}, Files: []string{"a.go", "b.go"}, Focus: "all"},
	})
	kinds := map[string]string{}
	for _, node := range ext.Nodes {
		kinds[node.ID] = node.Kind
		if strings.Join(node.DependsOn, ",") != NodePlan {
			t.Fatalf("lane %s deps = %v, want plan only", node.ID, node.DependsOn)
		}
		if node.Required {
			t.Fatalf("lane %s must be optional so one failed lane cannot abort the others", node.ID)
		}
		if node.Timeout != DefaultReviewTimeout {
			t.Fatalf("lane %s timeout = %v", node.ID, node.Timeout)
		}
	}
	want := map[string]string{"cr.p1": KindCodeReview, "rv.p1": KindReview, NodeReasoner: KindReasoner, NodeValidate: KindValidate}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("lanes = %v, want %v", kinds, want)
	}
	if len(ext.DepPatches) != 1 || ext.DepPatches[0].Node != NodeGather {
		t.Fatalf("dep patches = %+v", ext.DepPatches)
	}
	added := append([]string(nil), ext.DepPatches[0].Add...)
	sort.Strings(added)
	if strings.Join(added, ",") != "cr.p1,reasoner,rv.p1,validate" {
		t.Fatalf("gather patch = %v", added)
	}
}

func TestLanesPlanExpansionCarriesExplorationAndProviders(t *testing.T) {
	profile := lanesProfile()
	profile.Config.Lanes = &config.LanesConfig{
		Review:   config.LaneConfig{Provider: "claude"},
		Reasoner: config.LaneConfig{Provider: "gpt"},
	}
	_, ext := lanesExpansion(t, profile, completedExplore(t), []Package{
		{ID: "p1", Blocks: []string{"code-review"}, Files: []string{"a.go"}, Focus: "a"},
	})
	for _, node := range ext.Nodes {
		var exploration, provider string
		switch node.Kind {
		case KindCodeReview, KindReview:
			params, err := decodeParams[reviewNodeParams](node.Params)
			if err != nil {
				t.Fatal(err)
			}
			exploration, provider = params.Exploration, params.Provider
		case KindReasoner:
			params, err := decodeParams[reasonerParams](node.Params)
			if err != nil {
				t.Fatal(err)
			}
			if !params.Tools || params.When != "always" {
				t.Fatalf("reasoner lane params = %+v", params)
			}
			exploration, provider = params.Exploration, params.Provider
		case KindValidate:
			params, err := decodeParams[validateParams](node.Params)
			if err != nil {
				t.Fatal(err)
			}
			exploration, provider = params.Exploration, params.Provider
		}
		if !strings.Contains(exploration, "Retry count is unbounded") || !strings.Contains(exploration, "Whether callers set a deadline") {
			t.Fatalf("%s exploration = %q", node.ID, exploration)
		}
		wantProvider := map[string]string{KindCodeReview: "local", KindReview: "claude", KindReasoner: "gpt", KindValidate: "local"}[node.Kind]
		if provider != wantProvider {
			t.Fatalf("%s provider = %q, want %q", node.ID, provider, wantProvider)
		}
	}
}

func TestLanesPlanExpansionMultiPackageAndProviders(t *testing.T) {
	profile := lanesProfile()
	profile.Config.Propose = append(profile.Config.Propose, config.ProposeConfig{Block: "code-review", Provider: "gpt"})
	_, ext := lanesExpansion(t, profile, completedExplore(t), []Package{
		{ID: "one", Blocks: []string{"code-review"}, Files: []string{"a.go"}, Focus: "one"},
		{ID: "two", Blocks: []string{"code-review"}, Files: []string{"b.go"}, Focus: "two"},
	})
	ids := make([]string, 0, len(ext.Nodes))
	for _, node := range ext.Nodes {
		ids = append(ids, node.ID)
	}
	sort.Strings(ids)
	want := "cr.one.gpt.1,cr.one.local,cr.two.gpt.1,cr.two.local,reasoner,rv.one,rv.two,validate"
	if got := strings.Join(ids, ","); got != want {
		t.Fatalf("lane nodes = %s, want %s", got, want)
	}
}

func TestLanesPlanExpansionWithoutExploration(t *testing.T) {
	for name, explore := range map[string]*dag.NodeView{
		"failed":  {ID: NodeExplore, Status: dag.NodeFailed},
		"missing": nil,
		"garbage": {ID: NodeExplore, Status: dag.NodeCompleted, Output: json.RawMessage(`{"summary":""}`)},
	} {
		t.Run(name, func(t *testing.T) {
			_, ext := lanesExpansion(t, lanesProfile(), explore, []Package{
				{ID: "p1", Blocks: []string{"code-review"}, Files: []string{"a.go"}, Focus: "a"},
			})
			if len(ext.Nodes) != 4 {
				t.Fatalf("lanes = %d, want 4 even without exploration", len(ext.Nodes))
			}
			for _, node := range ext.Nodes {
				if strings.Contains(string(node.Params), "exploration") {
					t.Fatalf("%s carries exploration without a completed explore node: %s", node.ID, node.Params)
				}
			}
		})
	}
}

func TestRenderExplorationIsBounded(t *testing.T) {
	out := validExploreOut()
	out.Summary = strings.Repeat("s", 2000)
	for i := 0; i < 30; i++ {
		out.Facts = append(out.Facts, ExploreFact{Statement: strings.Repeat("f", 500), Path: "a.go", Evidence: strings.Repeat("e", 1000)})
	}
	rendered := renderExploration(out)
	if n := len([]rune(rendered)); n > MaxExplorationRunes {
		t.Fatalf("exploration runes = %d, max %d", n, MaxExplorationRunes)
	}
	if !strings.Contains(rendered, "已截断") {
		t.Fatal("a bounded exploration must say it was truncated")
	}
}

func TestLanesProfileValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Profile)
		want   string
	}{
		{name: "unknown flow", mutate: func(p *Profile) { p.Config.Flow = "fast" }, want: "flow"},
		{name: "review proposer", mutate: func(p *Profile) {
			p.Config.Propose = append(p.Config.Propose, config.ProposeConfig{Block: "review", Provider: "local"})
		}, want: "propose[1].block"},
		{name: "legacy reasoner", mutate: func(p *Profile) {
			p.Config.Reasoner = &config.ReasonerConfig{Provider: "local", When: "always"}
		}, want: "reasoner"},
		{name: "explore without lanes", mutate: func(p *Profile) {
			p.Config.Flow = FlowLegacy
			p.Config.Explore = &config.ExploreConfig{Provider: "local"}
		}, want: "explore"},
		{name: "unknown lane provider", mutate: func(p *Profile) {
			p.Config.Lanes = &config.LanesConfig{Validate: config.LaneConfig{Provider: "nope"}}
		}, want: "lanes.validate.provider"},
		{name: "no provider", mutate: func(p *Profile) {
			p.Config.Propose[0].Provider = ""
		}, want: "provider: is required"},
		{name: "unknown explore provider", mutate: func(p *Profile) {
			p.Config.Explore = &config.ExploreConfig{Provider: "nope"}
		}, want: "explore.provider"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profile := lanesProfile()
			tc.mutate(&profile)
			_, err := BuildGraph(profile, validDossier(t), GraphConfig{AllowedProviders: []string{"local", "claude"}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
	// "never" keeps a legacy reasoner block disabled and is accepted.
	profile := lanesProfile()
	profile.Config.Reasoner = &config.ReasonerConfig{When: "never"}
	if _, err := BuildGraph(profile, validDossier(t), GraphConfig{AllowedProviders: []string{"local", "claude"}}); err != nil {
		t.Fatalf("reasoner never: %v", err)
	}
}

func TestLanesFanOutBound(t *testing.T) {
	profile := lanesProfile()
	profile.Config.Plan.MaxPackages = 17 // 17 × (1 code-review + 1 review) + 2 = 36
	if _, err := BuildGraph(profile, validDossier(t), GraphConfig{AllowedProviders: []string{"local", "claude"}, MaxFanOut: 36}); err != nil {
		t.Fatalf("17 packages fit 36: %v", err)
	}
	profile.Config.Plan.MaxPackages = 18
	_, err := BuildGraph(profile, validDossier(t), GraphConfig{AllowedProviders: []string{"local", "claude"}, MaxFanOut: 36})
	if err == nil || !strings.Contains(err.Error(), "max_fan_out") {
		t.Fatalf("error = %v, want max_fan_out", err)
	}
}

func TestValidateOutContract(t *testing.T) {
	check := func(result string) ValidateCheck {
		return ValidateCheck{Name: "pipeline", Source: "pipeline", Result: result, Evidence: "pipeline 1 " + result}
	}
	cases := []struct {
		name string
		out  ValidateOut
		ok   bool
	}{
		{name: "pass", out: ValidateOut{Status: "PASS", Checks: []ValidateCheck{check("PASS")}, Findings: []FindingOut{}, Summary: "ok"}, ok: true},
		{name: "pass with failing check", out: ValidateOut{Status: "PASS", Checks: []ValidateCheck{check("FAIL")}, Findings: []FindingOut{}, Summary: "ok"}},
		{name: "pass with P1", out: ValidateOut{Status: "PASS", Checks: []ValidateCheck{check("PASS")}, Findings: []FindingOut{finding("a.go", 1, SeverityP1, "x")}, Summary: "ok"}},
		{name: "fail without evidence", out: ValidateOut{Status: "FAIL", Checks: []ValidateCheck{check("PASS")}, Findings: []FindingOut{}, Summary: "bad"}},
		{name: "fail with check", out: ValidateOut{Status: "FAIL", Checks: []ValidateCheck{check("FAIL")}, Findings: []FindingOut{}, Summary: "bad"}, ok: true},
		{name: "blocked without reason", out: ValidateOut{Status: "BLOCKED", Checks: []ValidateCheck{}, Findings: []FindingOut{}, Summary: "no ci"}},
		{name: "blocked", out: ValidateOut{Status: "BLOCKED", Checks: []ValidateCheck{check("BLOCKED")}, Findings: []FindingOut{}, BlockReason: "no pipeline", Summary: "no ci"}, ok: true},
		{name: "bad source", out: ValidateOut{Status: "PASS", Checks: []ValidateCheck{{Name: "x", Source: "shell", Result: "PASS", Evidence: "e"}}, Findings: []FindingOut{}, Summary: "ok"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.out)
			_, err := DecodeContract(ContractValidate, raw)
			if (err == nil) != tc.ok {
				t.Fatalf("DecodeContract err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
	for _, kind := range []string{ContractExplore, ContractValidate} {
		if _, err := ContractSchema(kind); err != nil {
			t.Fatalf("schema %s: %v", kind, err)
		}
	}
	raw, _ := json.Marshal(ExploreOut{Summary: "", Facts: []ExploreFact{}, Inferences: []string{}, Unknowns: []string{}})
	if _, err := DecodeContract(ContractExplore, raw); err == nil {
		t.Fatal("explore summary is required")
	}
}

// lanesGatherWorkflow returns completed lane nodes for one package.
func lanesGatherWorkflow(t *testing.T, validate ValidateOut, overrides ...dag.NodeView) dag.WorkflowView {
	t.Helper()
	nodes := map[string]dag.NodeView{
		"cr.p1": completedNode("cr.p1", KindCodeReview, reviewNodeParams{Package: "p1", Files: []string{"a.go"}, Focus: "f", Provider: "local"}, CodeReviewOut{
			Summary: "cr", Findings: []FindingOut{finding("a.go", 3, SeverityP2, "code-review finding")},
		}),
		"rv.p1": completedNode("rv.p1", KindReview, reviewNodeParams{Package: "p1", Files: []string{"a.go"}, Focus: "f", Provider: "local"}, ReviewOut{
			Verdict: "BLOCKING", EvidenceStatus: "sufficient", EvidenceGaps: []string{}, Summary: "rv",
			Findings: []FindingOut{finding("a.go", 3, SeverityP1, "review finding")},
		}),
		NodeReasoner: completedNode(NodeReasoner, KindReasoner, reasonerParams{Provider: "local", When: "always", Tools: true}, ReasonerOut{
			ResultType: "FINDINGS", Findings: []ReasonerFinding{{
				Severity: "notable", Dimension: "boundary", Finding: "reasoner finding", Where: "design", Why: "missing case",
				Assumptions: []string{}, Confidence: ConfidenceMedium,
			}},
		}),
		NodeValidate: completedNode(NodeValidate, KindValidate, validateParams{Provider: "local"}, validate),
	}
	for _, node := range overrides {
		nodes[node.ID] = node
	}
	order := []string{NodeExplore, NodePlan, "cr.p1", "rv.p1", NodeReasoner, NodeValidate}
	if _, ok := nodes[NodeExplore]; !ok {
		raw, _ := json.Marshal(validExploreOut())
		nodes[NodeExplore] = dag.NodeView{ID: NodeExplore, Spec: dag.NodeSpec{ID: NodeExplore, Kind: KindExplore}, Status: dag.NodeCompleted, Output: raw}
	}
	nodes[NodePlan] = dag.NodeView{ID: NodePlan, Spec: dag.NodeSpec{ID: NodePlan, Kind: KindPlan}, Status: dag.NodeCompleted}
	return dag.WorkflowView{ID: "wf-lanes-gather", Spec: dag.GraphSpec{Budget: dag.Budget{MaxNodes: 30}}, Nodes: nodes, Order: order}
}

func runLanesGather(t *testing.T, wf dag.WorkflowView) GatherOut {
	t.Helper()
	executor := newTestExecutor(t, ExecutorDeps{
		Profile:  lanesProfile(),
		Dossier:  validDossier(t),
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		Locate:   func(string, int, Side) bool { return true },
	})
	return runEvidenceGather(t, executor, wf)
}

func passingValidate() ValidateOut {
	return ValidateOut{
		Status: "PASS", Findings: []FindingOut{}, Summary: "pipeline passed",
		Checks: []ValidateCheck{{Name: "pipeline", Source: "pipeline", Result: "PASS", Evidence: "pipeline 7 success"}},
	}
}

func TestLanesGatherNormalizesFourLaneOutputs(t *testing.T) {
	validate := passingValidate()
	validate.Status = "FAIL"
	validate.Checks = []ValidateCheck{{Name: "unit", Source: "pipeline", Result: "FAIL", Evidence: "job unit failed at a.go:9"}}
	validate.Findings = []FindingOut{finding("a.go", 9, SeverityP1, "validate finding")}
	gathered := runLanesGather(t, lanesGatherWorkflow(t, validate))

	severities := map[string]Severity{}
	for _, group := range gathered.Groups {
		for _, source := range group.Sources {
			severities[source.Origin.Node] = group.Severity
		}
	}
	want := map[string]Severity{"cr.p1": SeverityP1, "rv.p1": SeverityP1, NodeReasoner: SeverityP2, NodeValidate: SeverityP1}
	for node, severity := range want {
		if got, ok := severities[node]; !ok || got != severity {
			t.Fatalf("lane %s severity = %q (present %v), want %q; groups=%+v", node, got, ok, severity, gathered.Groups)
		}
	}
	if len(gathered.Unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %+v", gathered.Unresolved)
	}
}

func TestLanesGatherValidateStatusSignals(t *testing.T) {
	t.Run("blocked is a warning, not an unresolved gap", func(t *testing.T) {
		validate := passingValidate()
		validate.Status = "BLOCKED"
		validate.BlockReason = "no pipeline for this head"
		validate.Checks = []ValidateCheck{{Name: "pipeline", Source: "pipeline", Result: "BLOCKED", Evidence: "no pipeline"}}
		gathered := runLanesGather(t, lanesGatherWorkflow(t, validate))
		if len(gathered.Unresolved) != 0 {
			t.Fatalf("BLOCKED validation must not block PASS: %+v", gathered.Unresolved)
		}
		if !containsSubstring(gathered.Warnings, "no pipeline for this head") {
			t.Fatalf("warnings = %v", gathered.Warnings)
		}
	})
	t.Run("fail without findings is review blocking", func(t *testing.T) {
		validate := passingValidate()
		validate.Status = "FAIL"
		validate.Checks = []ValidateCheck{{Name: "unit", Source: "pipeline", Result: "FAIL", Evidence: "job unit failed"}}
		gathered := runLanesGather(t, lanesGatherWorkflow(t, validate))
		if !hasUnresolved(gathered.Unresolved, NodeValidate, UnresolvedBlocking) {
			t.Fatalf("unresolved = %+v", gathered.Unresolved)
		}
	})
	t.Run("pass is silent", func(t *testing.T) {
		gathered := runLanesGather(t, lanesGatherWorkflow(t, passingValidate()))
		if len(gathered.Unresolved) != 0 || containsSubstring(gathered.Warnings, "validate") {
			t.Fatalf("unresolved=%+v warnings=%v", gathered.Unresolved, gathered.Warnings)
		}
	})
}

func TestLanesGatherFailedLaneIsIncomplete(t *testing.T) {
	for _, status := range []dag.NodeStatus{dag.NodeFailed, dag.NodeAborted, dag.NodeSkipped} {
		for _, lane := range []struct{ id, kind string }{
			{"cr.p1", KindCodeReview}, {"rv.p1", KindReview}, {NodeReasoner, KindReasoner}, {NodeValidate, KindValidate},
		} {
			t.Run(string(status)+"/"+lane.id, func(t *testing.T) {
				failed := dag.NodeView{ID: lane.id, Spec: dag.NodeSpec{ID: lane.id, Kind: lane.kind}, Status: status}
				gathered := runLanesGather(t, lanesGatherWorkflow(t, passingValidate(), failed))
				if !hasUnresolved(gathered.Unresolved, lane.id, UnresolvedEvidence) {
					t.Fatalf("unresolved = %+v, want evidence gap for %s", gathered.Unresolved, lane.id)
				}
				if len(gathered.Groups) == 0 {
					t.Fatal("the other lanes' findings must still be gathered")
				}
			})
		}
	}
}

func TestLanesGatherFailedExploreIsWarning(t *testing.T) {
	failed := dag.NodeView{ID: NodeExplore, Spec: dag.NodeSpec{ID: NodeExplore, Kind: KindExplore}, Status: dag.NodeFailed}
	gathered := runLanesGather(t, lanesGatherWorkflow(t, passingValidate(), failed))
	if len(gathered.Unresolved) != 0 {
		t.Fatalf("a failed explore must not block the review: %+v", gathered.Unresolved)
	}
	if !containsSubstring(gathered.Warnings, NodeExplore) {
		t.Fatalf("warnings = %v", gathered.Warnings)
	}
}

func TestLegacyGatherFailedReasonerStaysWarning(t *testing.T) {
	failed := dag.NodeView{ID: NodeReasoner, Spec: dag.NodeSpec{ID: NodeReasoner, Kind: KindReasoner}, Status: dag.NodeFailed}
	wf := lanesGatherWorkflow(t, passingValidate(), failed)
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  validDossier(t),
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		Locate:   func(string, int, Side) bool { return true },
	})
	gathered := runEvidenceGather(t, executor, wf)
	if hasUnresolved(gathered.Unresolved, NodeReasoner, UnresolvedEvidence) {
		t.Fatalf("legacy flow semantics changed: %+v", gathered.Unresolved)
	}
}

func TestLanesAgentNodesUseOwnBlocksAndTasks(t *testing.T) {
	type call struct{ task string }
	var calls []call
	exploreRaw, _ := json.Marshal(validExploreOut())
	validateRaw, _ := json.Marshal(passingValidate())
	reviewRaw, _ := json.Marshal(ReviewOut{Verdict: "PASS", EvidenceStatus: "sufficient", Findings: []FindingOut{}, EvidenceGaps: []string{}, Summary: "ok"})
	reasonerRaw, _ := json.Marshal(ReasonerOut{ResultType: "CLEAN", Findings: []ReasonerFinding{}})
	outputs := map[string]json.RawMessage{KindExplore: exploreRaw, KindValidate: validateRaw, KindReview: reviewRaw, KindReasoner: reasonerRaw}
	var current string
	executor := newTestExecutor(t, ExecutorDeps{
		Profile: lanesProfile(),
		Dossier: validDossier(t),
		AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			calls = append(calls, call{task: spec.Prompt})
			decoded, err := spec.Submit.Decode(outputs[current])
			if err != nil {
				return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
			}
			return &agent.RunResult{Output: decoded, Usage: llm.Usage{InputTokens: 5, OutputTokens: 1}}, nil
		},
	})
	params := func(value any) json.RawMessage {
		raw, _ := json.Marshal(value)
		return raw
	}
	exploration := renderExploration(validExploreOut())
	nodes := []dag.NodeSpec{
		{ID: NodeExplore, Kind: KindExplore, Params: params(exploreParams{Provider: "local"})},
		{ID: "rv.p1", Kind: KindReview, Params: params(reviewNodeParams{Package: "p1", Files: []string{"a.go"}, Focus: "f", Provider: "local", Exploration: exploration})},
		{ID: NodeReasoner, Kind: KindReasoner, Params: params(reasonerParams{Provider: "local", When: "always", Tools: true, Exploration: exploration})},
		{ID: NodeValidate, Kind: KindValidate, Params: params(validateParams{Provider: "local", Exploration: exploration})},
	}
	for _, node := range nodes {
		current = node.Kind
		out, err := executor.Execute(context.Background(), dag.NodeInput{WorkflowID: "wf", Node: node})
		if err != nil {
			t.Fatalf("Execute %s: %v", node.ID, err)
		}
		if out.Usage.InputTokens != 5 {
			t.Fatalf("%s usage = %+v", node.ID, out.Usage)
		}
	}
	if len(calls) != 4 {
		t.Fatalf("agent calls = %d", len(calls))
	}
	for i, node := range nodes[1:] {
		if !strings.Contains(calls[i+1].task, "Retry count is unbounded") {
			t.Fatalf("%s task lacks the exploration digest:\n%s", node.ID, calls[i+1].task)
		}
	}
	if strings.Contains(calls[0].task, "探索结论") {
		t.Fatal("explore must not receive its own digest")
	}
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

func hasUnresolved(signals []UnresolvedSignal, node, kind string) bool {
	for _, signal := range signals {
		if signal.Node == node && signal.Kind == kind {
			return true
		}
	}
	return false
}

func TestLanesValidateFailNeedsABlockingFindingInScope(t *testing.T) {
	failing := func(findings ...FindingOut) ValidateOut {
		return ValidateOut{
			Status: "FAIL", Findings: append([]FindingOut{}, findings...), Summary: "unit job failed",
			Checks: []ValidateCheck{{Name: "unit", Source: "pipeline", Result: "FAIL", Evidence: "job unit failed"}},
		}
	}
	cases := []struct {
		name        string
		validate    ValidateOut
		incremental *IncrementalRange
		wantSignal  bool
	}{
		{name: "P1 finding in scope blocks through the finding", validate: failing(finding("a.go", 9, SeverityP1, "broken"))},
		{name: "only a P2 finding", validate: failing(finding("a.go", 9, SeverityP2, "minor")), wantSignal: true},
		{name: "P1 finding filtered out by the incremental scope", validate: failing(finding("b.go", 9, SeverityP1, "elsewhere")),
			incremental: &IncrementalRange{Files: []string{"a.go"}}, wantSignal: true},
		{name: "P1 finding without a path in an incremental review", validate: failing(finding("", 0, SeverityP1, "pipeline")),
			incremental: &IncrementalRange{Files: []string{"a.go"}}, wantSignal: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := lanesGatherWorkflow(t, tc.validate)
			dossier := validDossier(t)
			dossier.Incremental = tc.incremental
			executor := newTestExecutor(t, ExecutorDeps{
				Profile: lanesProfile(), Dossier: dossier,
				Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
				Locate:   func(string, int, Side) bool { return true },
			})
			gathered := runEvidenceGather(t, executor, wf)
			if got := hasUnresolved(gathered.Unresolved, NodeValidate, UnresolvedBlocking); got != tc.wantSignal {
				t.Fatalf("validate blocking signal = %v, want %v; unresolved=%+v", got, tc.wantSignal, gathered.Unresolved)
			}
		})
	}
}

func TestValidateBlockedMustNotHideAFailingCheck(t *testing.T) {
	raw, _ := json.Marshal(ValidateOut{
		Status: "BLOCKED", BlockReason: "logs truncated", Findings: []FindingOut{}, Summary: "s",
		Checks: []ValidateCheck{{Name: "unit", Source: "pipeline", Result: "FAIL", Evidence: "job unit failed"}},
	})
	if _, err := DecodeContract(ContractValidate, raw); err == nil {
		t.Fatal("a FAIL check must make the status FAIL, not BLOCKED")
	}
}

func TestLanesAgentNodesRecordToolGaps(t *testing.T) {
	exploreRaw, _ := json.Marshal(validExploreOut())
	validateRaw, _ := json.Marshal(passingValidate())
	reasonerRaw, _ := json.Marshal(ReasonerOut{ResultType: "CLEAN", Findings: []ReasonerFinding{}})
	outputs := map[string]json.RawMessage{KindExplore: exploreRaw, KindValidate: validateRaw, KindReasoner: reasonerRaw}
	var current string
	executor := newTestExecutor(t, ExecutorDeps{
		Profile: lanesProfile(),
		Dossier: validDossier(t),
		Blocks: map[string]config.BlockBinding{
			BlockExplore: {MCPTools: []string{"mcp__graph__search"}}, BlockValidate: {MCPTools: []string{"mcp__graph__search"}},
			BlockReasoner: {MCPTools: []string{"mcp__graph__search"}},
		},
		Tools: failingTools{err: fmt.Errorf("graph server unavailable")},
		AgentRun: func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			decoded, err := spec.Submit.Decode(outputs[current])
			if err != nil {
				return nil, err
			}
			return &agent.RunResult{Output: decoded}, nil
		},
	})
	raw := func(value any) json.RawMessage { out, _ := json.Marshal(value); return out }
	for _, node := range []dag.NodeSpec{
		{ID: NodeExplore, Kind: KindExplore, Params: raw(exploreParams{Provider: "local"})},
		{ID: NodeValidate, Kind: KindValidate, Params: raw(validateParams{Provider: "local"})},
		{ID: NodeReasoner, Kind: KindReasoner, Params: raw(reasonerParams{Provider: "local", When: "always", Tools: true})},
	} {
		current = node.Kind
		out, err := executor.Execute(context.Background(), dag.NodeInput{WorkflowID: "wf", Node: node})
		if err != nil {
			t.Fatalf("Execute %s: %v", node.ID, err)
		}
		var gaps struct {
			ToolGaps []string `json:"tool_gaps"`
		}
		if err := json.Unmarshal(out.Data, &gaps); err != nil || len(gaps.ToolGaps) == 0 {
			t.Fatalf("%s output drops the MCP binding gap: %s", node.ID, out.Data)
		}
	}
}

// lanesRunner answers each lane by its role prompt; failRole makes that role
// fail permanently.
func lanesRunner(t *testing.T, failRole string, calls *sync.Map) AgentRunner {
	t.Helper()
	outputs := map[string]any{
		"# Explore role":     validExploreOut(),
		"# Code review role": CodeReviewOut{Summary: "cr", Findings: []FindingOut{}},
		"# Review lane role": ReviewOut{Verdict: "PASS", EvidenceStatus: "sufficient", Findings: []FindingOut{}, EvidenceGaps: []string{}, Summary: "ok"},
		"# Reasoner role":    ReasonerOut{ResultType: "CLEAN", Findings: []ReasonerFinding{}},
		"# Validate lane role": ValidateOut{Status: "PASS", Findings: []FindingOut{}, Summary: "ok",
			Checks: []ValidateCheck{{Name: "pipeline", Source: "pipeline", Result: "PASS", Evidence: "pipeline success"}}},
	}
	return func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		var system strings.Builder
		for _, block := range spec.System {
			system.WriteString(block.Text)
		}
		for role, output := range outputs {
			if !strings.Contains(system.String(), role) {
				continue
			}
			calls.Store(role, true)
			if role == failRole {
				return nil, fmt.Errorf("%w: lane failed", agent.ErrUnsafeResponse)
			}
			raw, _ := json.Marshal(output)
			decoded, err := spec.Submit.Decode(raw)
			if err != nil {
				return nil, err
			}
			return &agent.RunResult{Output: decoded, Usage: llm.Usage{InputTokens: 10, OutputTokens: 2}}, nil
		}
		t.Errorf("unexpected agent role in system prompt")
		return nil, fmt.Errorf("unexpected role")
	}
}

func TestLanesEngineExploreFailureStillRunsTheFourLanes(t *testing.T) {
	var calls sync.Map
	view := runReviewWorkflow(t, lanesProfile(), 30, 0, lanesRunner(t, "# Explore role", &calls))
	if view.Nodes[NodeExplore].Status != dag.NodeFailed {
		t.Fatalf("explore = %s, want failed", view.Nodes[NodeExplore].Status)
	}
	for _, id := range []string{NodePlan, "cr.p1", "rv.p1", NodeReasoner, NodeValidate, NodeGather, NodeAdjudicate, NodeSynth} {
		if view.Nodes[id].Status != dag.NodeCompleted {
			t.Fatalf("%s = %s, want completed after a failed explore", id, view.Nodes[id].Status)
		}
	}
	result := decodeNodeOutput[SynthesisResult](t, view, NodeSynth)
	if result.Verdict != VerdictPass || !containsSubstring(result.Warnings, NodeExplore) {
		t.Fatalf("verdict = %s warnings = %v, want PASS with an explore warning", result.Verdict, result.Warnings)
	}
}

func TestLanesEngineFailedLaneMakesTheReviewIncomplete(t *testing.T) {
	var calls sync.Map
	view := runReviewWorkflow(t, lanesProfile(), 30, 0, lanesRunner(t, "# Validate lane role", &calls))
	if view.Nodes[NodeValidate].Status != dag.NodeFailed {
		t.Fatalf("validate = %s, want failed", view.Nodes[NodeValidate].Status)
	}
	for _, role := range []string{"# Code review role", "# Review lane role", "# Reasoner role"} {
		if _, ok := calls.Load(role); !ok {
			t.Fatalf("lane %q did not run beside the failed lane", role)
		}
	}
	result := decodeNodeOutput[SynthesisResult](t, view, NodeSynth)
	if result.Verdict != VerdictIncomplete || !hasUnresolved(result.Unresolved, NodeValidate, UnresolvedEvidence) {
		t.Fatalf("verdict = %s unresolved = %+v, want INCOMPLETE naming validate", result.Verdict, result.Unresolved)
	}
}
