package review

import (
	"context"
	"testing"
)

// An ignore policy withholds MCP tools by design; it is a safety downgrade,
// not a coverage gap, so a clean review must still settle PASS.
func TestIgnorePolicyBlockedMCPDoesNotPreventPass(t *testing.T) {
	d := validDossier(t)
	d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{"**/*.lock"}}
	provider := &ignoreMetadataProvider{names: []string{"mcp__codebase-memory__search_graph"}}
	var seen []string
	e := newTestExecutor(t, ExecutorDeps{Dossier: d, Blocks: graphBlocks, Tools: provider, AgentRun: submitCodeReview(`{"summary":"ok","findings":[]}`, &seen)})
	out, err := e.Execute(context.Background(), codeReviewInput(t))
	if err != nil {
		t.Fatal(err)
	}
	result, err := DecodeStrict[*CodeReviewOut](out.Data)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolGaps) != 0 || provider.calls != 0 {
		t.Fatalf("gaps=%v provider calls=%d", result.ToolGaps, provider.calls)
	}
	for _, name := range seen {
		if name == "mcp__codebase-memory__search_graph" {
			t.Fatalf("agent received MCP tool under ignore policy: %v", seen)
		}
	}
	node := completedNode("cr.a", KindCodeReview,
		reviewNodeParams{Package: "a", Files: []string{"a.go"}, Focus: "f", Provider: "local"}, *result)
	wf := evidenceWorkflow(node)
	synth := runEvidenceSynthesis(t, evidenceExecutor(t, wf), wf, AdjudicateOut{Groups: []Group{}})
	if synth.Verdict != VerdictPass {
		t.Fatalf("verdict = %s, want PASS", synth.Verdict)
	}
}

// Without an ignore policy, an unprovable index identity still records a gap.
func TestIndexIdentityBlockedMCPStillRecordsGap(t *testing.T) {
	d := validDossier(t)
	d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{}}
	d.WorkspaceProvenance.MCPEligible = false
	provider := &ignoreMetadataProvider{names: []string{"mcp__codebase-memory__search_graph"}}
	var seen []string
	e := newTestExecutor(t, ExecutorDeps{Dossier: d, Blocks: graphBlocks, Tools: provider, AgentRun: submitCodeReview(`{"summary":"ok","findings":[]}`, &seen)})
	out, err := e.Execute(context.Background(), codeReviewInput(t))
	if err != nil {
		t.Fatal(err)
	}
	result, err := DecodeStrict[*CodeReviewOut](out.Data)
	if err != nil || len(result.ToolGaps) != 1 || provider.calls != 0 {
		t.Fatalf("gaps=%v calls=%d err=%v", result.ToolGaps, provider.calls, err)
	}
}
