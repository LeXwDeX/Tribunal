package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/agent"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/review"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/prompts"
)

// staticProviderClients resolves fixed provider metadata without calling a
// model. These tests use a scripted AgentRunner; only Info is consulted.
type staticProviderClients map[string]llm.Client

func (p staticProviderClients) Client(name string) (llm.Client, error) {
	client, ok := p[name]
	if !ok {
		return nil, fmt.Errorf("provider %q is not configured", name)
	}
	return client, nil
}

type attributionClient struct {
	name  string
	model string
}

func (c attributionClient) Generate(context.Context, *llm.Request) (*llm.Response, error) {
	return nil, errors.New("unexpected model call in attribution test")
}

func (c attributionClient) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: c.name, Model: c.model}
}

func assertNodeAttribution(t *testing.T, nodes []store.WorkflowNode, id, provider, model string) {
	t.Helper()
	for _, node := range nodes {
		if node.ID != id {
			continue
		}
		if node.Provider != provider || node.Model != model {
			t.Fatalf("%s attribution = (%q,%q), want (%q,%q) status=%s",
				id, node.Provider, node.Model, provider, model, node.Status)
		}
		return
	}
	t.Fatalf("node %s not found in %v", id, nodes)
}

// TestProviderAttributionPersistsThroughEngineAndStats runs the real
// scheduler, review executor, and SQLite projections. Model-backed nodes must
// carry the selected provider and configured model; code nodes stay blank.
func TestProviderAttributionPersistsThroughEngineAndStats(t *testing.T) {
	f, _, workflow, snapshot := runDeepReview(t, "job-attribution", func(f *reviewFixture, _ *scriptedAgentRunner) {
		f.service.clients = staticProviderClients{"fake": attributionClient{name: "fake", model: "fake-model"}}
	})

	modelNodes := []string{"plan", "reasoner", "rv.std.p1", "rv.intent.p1", "rv.gate.p1", "verify.1", "synthesize"}
	for _, id := range modelNodes {
		node := snapshot.Nodes[id]
		if node.Provider != "fake" || node.Model != "fake-model" {
			t.Fatalf("%s snapshot attribution = (%q,%q), want (fake,fake-model)", id, node.Provider, node.Model)
		}
	}
	for _, id := range []string{"gather", "adjudicate"} {
		node := snapshot.Nodes[id]
		if node.Provider != "" || node.Model != "" {
			t.Fatalf("%s code node attribution = (%q,%q), want empty", id, node.Provider, node.Model)
		}
	}

	nodes, err := f.db.GetWorkflowNodes(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflowNodes: %v", err)
	}
	for _, id := range modelNodes {
		assertNodeAttribution(t, nodes, id, "fake", "fake-model")
	}
	for _, id := range []string{"gather", "adjudicate"} {
		assertNodeAttribution(t, nodes, id, "", "")
	}

	projectID := int64(42)
	stats, err := f.db.Stats(context.Background(), store.StatsFilter{ProjectID: &projectID})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	var found *store.ProviderStats
	for i := range stats.ByProvider {
		if stats.ByProvider[i].Provider == "fake" {
			found = &stats.ByProvider[i]
		}
	}
	if found == nil {
		t.Fatalf("stats.by_provider = %+v, want fake", stats.ByProvider)
	}
	if found.Runs != int64(len(modelNodes)) {
		t.Fatalf("fake provider runs = %d, want %d", found.Runs, len(modelNodes))
	}
}

// TestProviderAttributionPersistsForFailedModelNode proves attribution is
// committed before Execute, so a failed model node still records its model.
func TestProviderAttributionPersistsForFailedModelNode(t *testing.T) {
	f, _, workflow, snapshot := runDeepReview(t, "job-attribution-failure", func(f *reviewFixture, runner *scriptedAgentRunner) {
		runner.fail[prompts.Verify] = errors.New("verify failed")
		f.service.clients = staticProviderClients{"fake": attributionClient{name: "fake", model: "fake-model"}}
	})
	verify := snapshot.Nodes["verify.1"]
	if verify.Status != "failed" {
		t.Fatalf("verify.1 status = %s, want failed", verify.Status)
	}
	if verify.Provider != "fake" || verify.Model != "fake-model" {
		t.Fatalf("verify.1 snapshot attribution = (%q,%q), want (fake,fake-model)", verify.Provider, verify.Model)
	}
	nodes, err := f.db.GetWorkflowNodes(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflowNodes: %v", err)
	}
	assertNodeAttribution(t, nodes, "verify.1", "fake", "fake-model")
}

// TestOnboardingProviderAttributionPersists covers the onboarding graph that
// real runs hit before review. A scripted runner replaces the model call.
func TestOnboardingProviderAttributionPersists(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	contexts := newContextService(t, f)
	seedWorkspaceContext(t, f, contexts, false)
	runner := func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		out := review.OnboardingOut{
			Overview:     "overview",
			Architecture: "architecture",
			Modules:      []review.Module{{Path: "internal", Purpose: "core"}},
			Conventions:  []string{"testing"},
			BuildAndTest: []string{"go test ./..."},
			RiskHotspots: []string{"none"},
			ReviewFocus:  []string{"contracts"},
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return nil, err
		}
		decoded, err := spec.Submit.Decode(raw)
		if err != nil {
			return nil, err
		}
		return &agent.RunResult{Output: decoded, Usage: llm.Usage{InputTokens: 3, OutputTokens: 4}}, nil
	}
	svc := newOnboardingContextService(t, f, runner)
	svc.clients = staticProviderClients{"fake": attributionClient{name: "fake", model: "fake-model"}}

	if err := svc.RefreshWithOptions(context.Background(), 42, "main", WorkspaceRefreshOptions{
		RegenerateContext: contextBoolPtr(true),
	}); err != nil {
		t.Fatalf("RefreshWithOptions: %v", err)
	}
	workflows, _, err := f.db.ListWorkflows(context.Background(), store.WorkflowFilter{Kind: "onboarding"}, "", 10)
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(workflows) != 1 {
		t.Fatalf("onboarding workflows = %d, want 1", len(workflows))
	}
	nodes, err := f.db.GetWorkflowNodes(context.Background(), workflows[0].ID)
	if err != nil {
		t.Fatalf("GetWorkflowNodes: %v", err)
	}
	assertNodeAttribution(t, nodes, "onboarding", "fake", "fake-model")
}

// TestProviderAttributionPersistsForCodeReviewNode covers the exact reported
// gap: cr.p1 saves tokens but previously had no provider/model. The default
// profile keeps plan and synthesis in code mode, so only cr.p1 is attributed.
func TestProviderAttributionPersistsForCodeReviewNode(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.clients = staticProviderClients{"fake": attributionClient{name: "fake", model: "fake-model"}}

	job := f.seedReview(t, "job-attribution-code-review")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	nodes, err := f.db.GetWorkflowNodes(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflowNodes: %v", err)
	}
	assertNodeAttribution(t, nodes, "cr.p1", "fake", "fake-model")
	for _, node := range nodes {
		switch node.ID {
		case "cr.p1":
			if node.TokensIn == 0 || node.TokensOut == 0 {
				t.Fatalf("cr.p1 tokens = (%d,%d), want saved usage", node.TokensIn, node.TokensOut)
			}
		case "plan", "gather", "adjudicate", "synthesize":
			if node.Provider != "" || node.Model != "" {
				t.Fatalf("code node %s attribution = (%q,%q), want empty", node.ID, node.Provider, node.Model)
			}
		}
	}
}
