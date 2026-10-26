package review

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

type failingTools struct{ err error }

func (f failingTools) Tools(context.Context, string) ([]agent.Tool, error) { return nil, f.err }

type emptyTools struct{}

func (emptyTools) Tools(context.Context, string) ([]agent.Tool, error) { return nil, nil }

var graphBlocks = map[string]config.BlockBinding{
	BlockCodeReview: {MCPTools: []string{"mcp__codebase-memory__search_graph"}},
}

// submitCodeReview submits raw through the node's own decoder and records
// the tool names the agent received.
func submitCodeReview(raw string, seen *[]string) AgentRunner {
	return func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		for _, tool := range spec.Tools {
			*seen = append(*seen, tool.Def().Name)
		}
		decoded, err := spec.Submit.Decode(json.RawMessage(raw))
		if err != nil {
			return nil, err
		}
		return &agent.RunResult{Output: decoded}, nil
	}
}

func codeReviewInput(t *testing.T) dag.NodeInput {
	t.Helper()
	params, err := json.Marshal(reviewNodeParams{Package: "a", Files: []string{"a.go"}, Focus: "f", Provider: "local"})
	if err != nil {
		t.Fatal(err)
	}
	return dag.NodeInput{WorkflowID: "wf-tools", Node: dag.NodeSpec{ID: "cr.a", Kind: KindCodeReview, Params: params}}
}

func TestCodeReviewContinuesWithoutUnavailableMCPTools(t *testing.T) {
	var seen []string
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  validDossier(t),
		Blocks:   graphBlocks,
		Tools:    failingTools{err: errors.New(`mcp: server "codebase-memory": connect failed`)},
		AgentRun: submitCodeReview(`{"summary":"no issues","findings":[]}`, &seen),
	})
	out, err := executor.Execute(context.Background(), codeReviewInput(t))
	if err != nil {
		t.Fatalf("code-review failed instead of degrading: %v", err)
	}
	for _, name := range seen {
		if strings.HasPrefix(name, "mcp__") {
			t.Fatalf("agent received MCP tool %q after binding failed", name)
		}
	}
	decoded, err := DecodeStrict[*CodeReviewOut](out.Data)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(decoded.ToolGaps) != 1 ||
		!strings.Contains(decoded.ToolGaps[0], "codebase-memory") ||
		!strings.Contains(decoded.ToolGaps[0], "connect failed") {
		t.Fatalf("tool gaps = %q, want the diagnosable binding failure", decoded.ToolGaps)
	}
}

func TestCodeReviewToolGapsAreCodeOwned(t *testing.T) {
	var seen []string
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  validDossier(t),
		Blocks:   graphBlocks,
		Tools:    emptyTools{},
		AgentRun: submitCodeReview(`{"summary":"ok","findings":[],"tool_gaps":["model claim"]}`, &seen),
	})
	out, err := executor.Execute(context.Background(), codeReviewInput(t))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	decoded, err := DecodeStrict[*CodeReviewOut](out.Data)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(decoded.ToolGaps) != 0 {
		t.Fatalf("tool gaps = %q, want model-supplied values discarded", decoded.ToolGaps)
	}
	schema, err := ContractSchema(ContractCodeReview)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(schema), "tool_gaps") {
		t.Fatalf("submit schema exposes the code-owned tool_gaps field: %s", schema)
	}
}

func TestCodeReviewFailsWhenContextEndsDuringToolBinding(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var seen []string
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  validDossier(t),
		Blocks:   graphBlocks,
		Tools:    failingTools{err: context.Canceled},
		AgentRun: submitCodeReview(`{"summary":"ok","findings":[]}`, &seen),
	})
	if _, err := executor.Execute(ctx, codeReviewInput(t)); err == nil {
		t.Fatal("Execute succeeded after cancellation; cancellation must not degrade to a review")
	}
}

func TestSynthesisReportsMCPToolGapsInsteadOfPass(t *testing.T) {
	node := completedNode("cr.a", KindCodeReview,
		reviewNodeParams{Package: "a", Files: []string{"a.go"}, Focus: "f", Provider: "local"},
		CodeReviewOut{
			Summary:  "no issues",
			Findings: []FindingOut{},
			ToolGaps: []string{`block code-review: mcp: server "codebase-memory": connect failed`},
		})
	wf := evidenceWorkflow(node)
	executor := evidenceExecutor(t, wf)
	result := runEvidenceSynthesis(t, executor, wf, AdjudicateOut{Groups: []Group{}})
	if result.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %s, want INCOMPLETE when code graph tools were unavailable", result.Verdict)
	}
	if !containsText(result.Warnings, "codebase-memory") {
		t.Fatalf("warnings = %q, want the MCP tool gap", result.Warnings)
	}
	found := false
	for _, signal := range result.Unresolved {
		if signal.Node == "cr.a" && signal.Kind == UnresolvedEvidence && strings.Contains(signal.Message, "connect failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("unresolved = %+v, want the node's tool gap", result.Unresolved)
	}
}

func TestSynthesisWithoutToolGapsStaysPass(t *testing.T) {
	node := completedNode("cr.a", KindCodeReview,
		reviewNodeParams{Package: "a", Files: []string{"a.go"}, Focus: "f", Provider: "local"},
		CodeReviewOut{Summary: "no issues", Findings: []FindingOut{}})
	wf := evidenceWorkflow(node)
	result := runEvidenceSynthesis(t, evidenceExecutor(t, wf), wf, AdjudicateOut{Groups: []Group{}})
	if result.Verdict != VerdictPass {
		t.Fatalf("verdict = %s, want PASS", result.Verdict)
	}
}

func containsText(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}
