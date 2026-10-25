package review

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

func defaultProfile() Profile {
	return Profile{
		Name: "default",
		Config: config.ProfileConfig{
			Plan:        config.PlanConfig{Mode: "code", PackageTokens: 60000, MaxPackages: 2},
			Propose:     []config.ProposeConfig{{Block: "code-review", Provider: "local"}},
			Challenge:   config.ChallengeConfig{Provider: "claude", Only: []string{"P0", "P1"}},
			OnUncertain: "mark",
			Synthesize:  config.SynthesizeConfig{Mode: "code"},
			Budget:      config.BudgetConfig{MaxNodes: 30, MaxExtends: 0, Deadline: 20 * time.Minute},
		},
	}
}

func deepProfile() Profile {
	return Profile{
		Name: "deep",
		Config: config.ProfileConfig{
			Plan: config.PlanConfig{Mode: "agent", Provider: "claude", MaxPackages: 6},
			Propose: []config.ProposeConfig{
				{Block: "review", Provider: "claude"},
				{Block: "review", Provider: "gpt"},
				{Block: "code-review", Provider: "local"},
			},
			Reasoner:    &config.ReasonerConfig{Provider: "claude", When: "auto"},
			Consensus:   &config.ConsensusConfig{MinVotes: 2, SameModelWeight: 0.5},
			Challenge:   config.ChallengeConfig{Provider: "cross", Fallback: "gpt", Only: []string{"P0", "P1", "P2"}},
			OnUncertain: "judge",
			Synthesize:  config.SynthesizeConfig{Mode: "agent", Provider: "claude"},
			Budget:      config.BudgetConfig{MaxNodes: 30, MaxExtends: 1, Deadline: 40 * time.Minute},
		},
	}
}

func onboardingProfile() Profile {
	return Profile{
		Name: "onboarding",
		Config: config.ProfileConfig{
			Agent: &config.AgentConfig{Provider: "claude"},
		},
	}
}

func nodeByID(spec dag.GraphSpec, id string) (dag.NodeSpec, bool) {
	for _, node := range spec.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return dag.NodeSpec{}, false
}

func TestBuildGraphDefaultTopology(t *testing.T) {
	spec, err := BuildGraph(defaultProfile(), validDossier(t), GraphConfig{AllowedProviders: []string{"local", "claude"}})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	if len(spec.Nodes) != 4 {
		t.Fatalf("nodes = %d, want 4", len(spec.Nodes))
	}
	plan, ok := nodeByID(spec, NodePlan)
	if !ok || !plan.Expands || plan.Kind != KindPlan || !plan.Required {
		t.Fatalf("unexpected plan node: %+v", plan)
	}
	gather, ok := nodeByID(spec, NodeGather)
	if !ok || !gather.Expands || !gather.Required || strings.Join(gather.DependsOn, ",") != NodePlan {
		t.Fatalf("unexpected gather node: %+v", gather)
	}
	adjudicate, ok := nodeByID(spec, NodeAdjudicate)
	if !ok || len(adjudicate.InputMapping) != 0 {
		t.Fatalf("unexpected adjudicate node: %+v", adjudicate)
	}
	synthesize, ok := nodeByID(spec, NodeSynth)
	if !ok || synthesize.Expands {
		t.Fatalf("code synthesis must not expand: %+v", synthesize)
	}
	if spec.Budget.MaxNodes != 30 || spec.Budget.MaxExtends != 0 || spec.Budget.Deadline != 20*time.Minute {
		t.Fatalf("budget = %+v", spec.Budget)
	}
}

func TestBuildGraphDeepTopology(t *testing.T) {
	spec, err := BuildGraph(deepProfile(), validDossier(t), GraphConfig{
		AllowedProviders: []string{"claude", "gpt", "local"},
	})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	if len(spec.Nodes) != 5 {
		t.Fatalf("nodes = %d, want 5", len(spec.Nodes))
	}
	reasoner, ok := nodeByID(spec, NodeReasoner)
	if !ok {
		t.Fatalf("reasoner missing")
	}
	if reasoner.Condition == "" || reasoner.Required {
		t.Fatalf("unexpected reasoner: %+v", reasoner)
	}
	gather, _ := nodeByID(spec, NodeGather)
	if strings.Join(gather.DependsOn, ",") != "plan,reasoner" {
		t.Fatalf("gather deps = %v", gather.DependsOn)
	}
	synthesize, _ := nodeByID(spec, NodeSynth)
	if !synthesize.Expands {
		t.Fatalf("agent synthesis should expand when max_extends > 0")
	}
}

func TestBuildGraphOnboarding(t *testing.T) {
	spec, err := BuildGraph(onboardingProfile(), nil, GraphConfig{AllowedProviders: []string{"claude"}})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	if len(spec.Nodes) != 1 || spec.Nodes[0].ID != KindOnboarding || !spec.Nodes[0].Required {
		t.Fatalf("unexpected onboarding graph: %+v", spec.Nodes)
	}
}

func TestBuildGraphDockerProfiles(t *testing.T) {
	for k, v := range map[string]string{
		"CRC_PUBLIC_URL":     "https://review.example.test",
		"GITLAB_URL":         "https://gitlab.example.test",
		"LLM_PROTOCOL":       "anthropic",
		"LLM_BASE_URL":       "https://llm.example.test",
		"LLM_MODEL":          "test-model",
		"LLM_API_KEY":        "test-key",
		"REVIEW_ADMIN_TOKEN": "0123456789abcdef0123456789abcdef",
		"REVIEW_MASTER_KEY":  "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	} {
		t.Setenv(k, v)
	}
	cfg, err := config.Load("../../config/server.docker.yaml")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Config.Validate: %v", err)
	}
	allowed := []string{"primary"}

	defaultSpec, err := BuildGraph(Profile{Name: "default", Config: cfg.Profiles["default"]}, validDossier(t), GraphConfig{AllowedProviders: allowed})
	if err != nil {
		t.Fatalf("default BuildGraph: %v", err)
	}
	for _, id := range []string{NodePlan, NodeGather, NodeAdjudicate, NodeSynth} {
		if _, ok := nodeByID(defaultSpec, id); !ok {
			t.Fatalf("default graph missing node %q", id)
		}
	}

	onboardingSpec, err := BuildGraph(Profile{Name: "onboarding", Config: cfg.Profiles["onboarding"]}, nil, GraphConfig{AllowedProviders: allowed})
	if err != nil {
		t.Fatalf("onboarding BuildGraph: %v", err)
	}
	if len(onboardingSpec.Nodes) != 1 || onboardingSpec.Nodes[0].ID != KindOnboarding {
		t.Fatalf("unexpected onboarding graph: %+v", onboardingSpec.Nodes)
	}
}

func TestPlanFanOutAndDepPatch(t *testing.T) {
	profile := defaultProfile()
	profile.Config.Plan.MaxPackages = 2
	cfg := GraphConfig{AllowedProviders: []string{"local", "claude"}}
	spec, err := BuildGraph(profile, validDossier(t), cfg)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	expanders, err := BuildExpanders(profile, validDossier(t), cfg)
	if err != nil {
		t.Fatalf("BuildExpanders: %v", err)
	}
	wf := dag.WorkflowView{
		ID:   "wf-1",
		Spec: spec,
		Nodes: map[string]dag.NodeView{
			NodePlan:   {ID: NodePlan, Spec: spec.Nodes[0], Status: dag.NodeCompleted},
			NodeGather: {ID: NodeGather, Spec: mustNode(t, spec, NodeGather), Status: dag.NodePending},
		},
	}
	planOut := PlanOut{
		Risk:        "medium",
		RiskReasons: []string{"two packages"},
		Packages: []Package{
			{ID: "one", Blocks: []string{"code-review"}, Files: []string{"a.go"}, Focus: "one", Provider: "local"},
			{ID: "two", Blocks: []string{"code-review"}, Files: []string{"b.go"}, Focus: "two"},
		},
		RunReasoner:   false,
		ChallengeOnly: []Severity{SeverityP0, SeverityP1},
	}
	out, _ := json.Marshal(planOut)
	planNode, _ := nodeByID(spec, NodePlan)
	ext, err := expanders[KindPlan].Expand(context.Background(), wf, planNode, out)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(ext.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(ext.Nodes))
	}
	if ext.Nodes[0].ID != "cr.one" || ext.Nodes[1].ID != "cr.two" {
		t.Fatalf("node ids = %q, %q", ext.Nodes[0].ID, ext.Nodes[1].ID)
	}
	if len(ext.DepPatches) != 1 || ext.DepPatches[0].Node != NodeGather {
		t.Fatalf("dep patches = %+v", ext.DepPatches)
	}
	if strings.Join(ext.DepPatches[0].Add, ",") != "cr.one,cr.two" {
		t.Fatalf("patch add = %v", ext.DepPatches[0].Add)
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
}

func mustNode(t *testing.T, spec dag.GraphSpec, id string) dag.NodeSpec {
	t.Helper()
	node, ok := nodeByID(spec, id)
	if !ok {
		t.Fatalf("node %q missing", id)
	}
	return node
}

func TestProfileValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Profile)
		allowed []string
		want    string
	}{
		{
			name: "plan mode",
			mutate: func(p *Profile) {
				p.Config.Plan.Mode = "magic"
			},
			want: "plan.mode",
		},
		{
			name: "propose block",
			mutate: func(p *Profile) {
				p.Config.Propose[0].Block = "verify"
			},
			want: "propose[0].block",
		},
		{
			name:    "unknown provider",
			mutate:  func(p *Profile) {},
			allowed: []string{"local"},
			want:    "challenge.provider",
		},
		{
			name: "judge requires agent synthesis",
			mutate: func(p *Profile) {
				p.Config.OnUncertain = "judge"
			},
			want: "on_uncertain",
		},
		{
			name: "cross without fallback",
			mutate: func(p *Profile) {
				p.Config.Challenge.Provider = "cross"
			},
			want: "challenge.fallback",
		},
		{
			name: "budget max nodes",
			mutate: func(p *Profile) {
				p.Config.Budget.MaxNodes = 101
			},
			want: "budget.max_nodes",
		},
		{
			name: "max fan out",
			mutate: func(p *Profile) {
				p.Config.Plan.MaxPackages = 40
			},
			allowed: []string{"local", "claude"},
			want:    "max_fan_out",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profile := defaultProfile()
			tc.mutate(&profile)
			_, err := BuildGraph(profile, validDossier(t), GraphConfig{AllowedProviders: tc.allowed, MaxFanOut: 24})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestPlanExpanderRejectsUnauthorizedFile(t *testing.T) {
	profile := defaultProfile()
	cfg := GraphConfig{AllowedProviders: []string{"local", "claude"}}
	spec, err := BuildGraph(profile, validDossier(t), cfg)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	expanders, err := BuildExpanders(profile, validDossier(t), cfg)
	if err != nil {
		t.Fatalf("BuildExpanders: %v", err)
	}
	wf := dag.WorkflowView{
		ID:   "wf-2",
		Spec: spec,
		Nodes: map[string]dag.NodeView{
			NodePlan:   {ID: NodePlan, Spec: mustNode(t, spec, NodePlan), Status: dag.NodeCompleted},
			NodeGather: {ID: NodeGather, Spec: mustNode(t, spec, NodeGather), Status: dag.NodePending},
		},
	}
	planOut := PlanOut{
		Risk:          "low",
		RiskReasons:   []string{"small"},
		Packages:      []Package{{ID: "one", Blocks: []string{"code-review"}, Files: []string{"outside.go"}, Focus: "one"}},
		ChallengeOnly: []Severity{SeverityP0},
	}
	out, _ := json.Marshal(planOut)
	planNode := mustNode(t, spec, NodePlan)
	_, err = expanders[KindPlan].Expand(context.Background(), wf, planNode, out)
	if err == nil || !errors.Is(err, ErrExtensionRejected) {
		t.Fatalf("expected extension rejection, got %v", err)
	}
}

func TestSynthesizeExpanderCreatesVerifyAndSecondSynthesis(t *testing.T) {
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
	result := SynthesisResult{
		Verdict: VerdictBlocking,
		Summary: "summary",
		Groups:  []Group{group},
		Output: SynthesizeOut{
			Verdict:        VerdictBlocking,
			Summary:        "summary",
			Decisions:      []Decision{},
			NewFindings:    []FindingOut{},
			ExtendRequests: []ExtendRequest{{Kind: "verify", FindingID: "g1", Focus: "double check"}},
		},
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("result: %v", err)
	}
	outputRaw, _ := json.Marshal(result)
	adjudicatedRaw, _ := json.Marshal(AdjudicateOut{Groups: []Group{group}})
	synthNode := mustNode(t, spec, NodeSynth)
	wf := dag.WorkflowView{
		ID:          "wf-extend",
		Spec:        spec,
		ExtendsUsed: 0,
		Nodes: map[string]dag.NodeView{
			NodeAdjudicate: {ID: NodeAdjudicate, Spec: mustNode(t, spec, NodeAdjudicate), Status: dag.NodeCompleted, Output: adjudicatedRaw},
			NodeSynth:      {ID: NodeSynth, Spec: synthNode, Status: dag.NodeCompleted, Attempt: 1, Output: outputRaw},
		},
		Order: []string{NodeAdjudicate, NodeSynth},
	}
	ext, err := expanders[KindSynthesize].Expand(context.Background(), wf, synthNode, outputRaw)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if ext.Source != dag.ExtensionSourceExtend {
		t.Fatalf("source = %q", ext.Source)
	}
	var verify, synth2 *dag.NodeSpec
	for i := range ext.Nodes {
		switch ext.Nodes[i].ID {
		case "verify.x1.1":
			verify = &ext.Nodes[i]
		case NodeSynth2:
			synth2 = &ext.Nodes[i]
		}
	}
	if verify == nil || synth2 == nil {
		t.Fatalf("missing extension nodes: %+v", ext.Nodes)
	}
	if strings.Join(synth2.DependsOn, ",") != "synthesize,adjudicate,verify.x1.1" {
		t.Fatalf("synthesize.2 deps = %v", synth2.DependsOn)
	}
	params, err := decodeParams[synthesizeParams](synth2.Params)
	if err != nil || params.PreviousNode != NodeSynth || params.MaxExtends != 1 {
		t.Fatalf("synthesize.2 params = %+v, err=%v", params, err)
	}
	candidate := spec
	candidate.Nodes = append(append([]dag.NodeSpec(nil), spec.Nodes...), ext.Nodes...)
	if err := dag.ValidateGraph(candidate); err != nil {
		t.Fatalf("extended graph: %v", err)
	}
}
