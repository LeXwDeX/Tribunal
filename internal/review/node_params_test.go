package review

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// namedClient is a model boundary that only knows its provider name.
type namedClient struct{ name string }

func (c namedClient) Generate(context.Context, *llm.Request) (*llm.Response, error) {
	return nil, nil
}
func (c namedClient) Info() llm.ProviderInfo { return llm.ProviderInfo{Name: c.name} }

type namedClients struct{}

func (namedClients) Client(name string) (llm.Client, error) { return namedClient{name: name}, nil }

type nodeCall struct {
	client      string
	maxTokens   int
	temperature *float64
	reasoning   *llm.ReasoningOpts
	meta        llm.CallMeta
}

func intp(v int) *int           { return &v }
func floatp(v float64) *float64 { return &v }

// Each node runs on the model its params name and sends the parameter
// overrides of its own node as request values (#334). The agent runner is the
// only model boundary: the client it receives is the one the node selected.
func TestNodesCallTheirModelWithTheirParams(t *testing.T) {
	profile := lanesProfile()
	profile.Config.NodeParams = map[string]config.NodeParams{
		config.NodeExplore:    {MaxTokens: intp(111), Source: "override"},
		config.NodeCodeReview: {Temperature: floatp(0.25), Source: "override_default"},
		config.NodeReview:     {Reasoning: &config.ReasoningConfig{Effort: "max"}, Source: "override"},
		config.NodeValidate:   {Thinking: &config.ThinkingConfig{BudgetTokens: 2048}, Source: "override"},
	}
	outputs := map[string]any{
		NodeExplore:  validExploreOut(),
		"cr.p1":      CodeReviewOut{Summary: "cr", Findings: []FindingOut{}},
		"rv.p1":      ReviewOut{Verdict: "PASS", EvidenceStatus: "sufficient", Findings: []FindingOut{}, EvidenceGaps: []string{}, Summary: "ok"},
		NodeReasoner: ReasonerOut{ResultType: "CLEAN", Findings: []ReasonerFinding{}},
		NodeValidate: passingValidate(),
	}
	calls := map[string]nodeCall{}
	executor := newTestExecutor(t, ExecutorDeps{
		Profile: profile,
		Dossier: validDossier(t),
		Clients: namedClients{},
		AgentRun: func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			meta := llm.CallMetaFromContext(ctx)
			call := nodeCall{maxTokens: spec.MaxTokens, temperature: spec.Temperature, reasoning: spec.Reasoning, meta: meta}
			call.client = spec.Client.Info().Name
			calls[meta.NodeID] = call
			raw, _ := json.Marshal(outputs[meta.NodeID])
			decoded, err := spec.Submit.Decode(raw)
			if err != nil {
				return nil, err
			}
			return &agent.RunResult{Output: decoded, Usage: llm.Usage{InputTokens: 1, OutputTokens: 1}}, nil
		},
	})
	encode := func(v any) json.RawMessage { raw, _ := json.Marshal(v); return raw }
	nodes := []dag.NodeSpec{
		{ID: NodeExplore, Kind: KindExplore, Params: encode(exploreParams{Provider: "user:explore"})},
		{ID: "cr.p1", Kind: KindCodeReview, Params: encode(reviewNodeParams{Package: "p1", Files: []string{"a.go"}, Focus: "f", Provider: "user:code"})},
		{ID: "rv.p1", Kind: KindReview, Params: encode(reviewNodeParams{Package: "p1", Files: []string{"a.go"}, Focus: "f", Provider: "user:review"})},
		{ID: NodeReasoner, Kind: KindReasoner, Params: encode(reasonerParams{Provider: "user:reasoner", When: "always", Tools: true})},
		{ID: NodeValidate, Kind: KindValidate, Params: encode(validateParams{Provider: "user:validate"})},
	}
	for _, node := range nodes {
		if _, err := executor.Execute(context.Background(), dag.NodeInput{WorkflowID: "wf", Node: node}); err != nil {
			t.Fatalf("Execute %s: %v", node.ID, err)
		}
	}
	wantClient := map[string]string{NodeExplore: "user:explore", "cr.p1": "user:code", "rv.p1": "user:review", NodeReasoner: "user:reasoner", NodeValidate: "user:validate"}
	for id, want := range wantClient {
		if got := calls[id].client; got != want {
			t.Errorf("%s called model %q, want %q", id, got, want)
		}
		if got := calls[id].meta.ModelID; got != want {
			t.Errorf("%s audit model_id = %q, want %q", id, got, want)
		}
	}
	if c := calls[NodeExplore]; c.maxTokens != 111 || c.temperature != nil || c.reasoning != nil || c.meta.ParamSource != "override" {
		t.Errorf("explore request = %+v", c)
	}
	if c := calls["cr.p1"]; c.temperature == nil || *c.temperature != 0.25 || c.maxTokens != 0 || c.meta.ParamSource != "override_default" {
		t.Errorf("code-review request = %+v", c)
	}
	if c := calls["rv.p1"]; c.reasoning == nil || c.reasoning.Effort != "max" || c.reasoning.BudgetTokens != 0 {
		t.Errorf("review request = %+v", c)
	}
	if c := calls[NodeValidate]; c.reasoning == nil || c.reasoning.BudgetTokens != 2048 || c.reasoning.Effort != "" {
		t.Errorf("validate request = %+v", c)
	}
	if c := calls[NodeReasoner]; c.maxTokens != 0 || c.temperature != nil || c.reasoning != nil || c.meta.ParamSource != "" {
		t.Errorf("reasoner has no params but request = %+v", c)
	}
}

func TestChallengeParamsApplyToVerifyNodes(t *testing.T) {
	profile := lanesProfile()
	profile.Config.NodeParams = map[string]config.NodeParams{
		config.NodeChallenge: {MaxTokens: intp(900), Source: "override"},
	}
	executor := newTestExecutor(t, ExecutorDeps{Profile: profile})
	params, ok := executor.nodeParams(KindVerify)
	if !ok || params.MaxTokens == nil || *params.MaxTokens != 900 {
		t.Fatalf("verify params = %+v, %v", params, ok)
	}
	if _, ok := executor.nodeParams(KindOnboarding); ok {
		t.Fatal("a node outside the seven configurable ones must not receive params")
	}
	var spec agent.RunSpec
	applyNodeParams(&spec, params)
	if spec.MaxTokens != 900 {
		t.Fatalf("spec.MaxTokens = %d", spec.MaxTokens)
	}
}
