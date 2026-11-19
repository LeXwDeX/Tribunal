package review

import (
	"context"
	"strings"
	"testing"
)

// divergentIndexSHA is a valid commit id that differs from testStartSHA, the
// MR target commit of validDossier.
const divergentIndexSHA = "abcdef0123456789abcdef0123456789abcdef01"

var boundGraphTools = []string{
	"mcp__codebase-memory__search_graph",
	"mcp__codebase-memory__trace_path",
	"mcp__codebase-memory__get_code_snippet",
}

// indexBlockedCodeReview runs one code-review node whose workspace index
// identity cannot be proven for the bound tools and returns its decoded output.
// change adjusts the dossier before the run. The node must run without
// connecting to any MCP server.
func indexBlockedCodeReview(t *testing.T, bound []string, change func(*Dossier)) *CodeReviewOut {
	t.Helper()
	d := validDossier(t)
	d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{}}
	change(d)
	provider := &ignoreMetadataProvider{names: bound}
	var seen []string
	e := newTestExecutor(t, ExecutorDeps{Dossier: d, Blocks: graphBlocks, Tools: provider, AgentRun: submitCodeReview(`{"summary":"ok","findings":[]}`, &seen)})
	out, err := e.Execute(context.Background(), codeReviewInput(t))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if provider.calls != 0 {
		t.Fatalf("MCP provider connected %d time(s) although the index identity is unproven", provider.calls)
	}
	for _, name := range seen {
		if strings.HasPrefix(name, "mcp__") {
			t.Fatalf("agent received MCP tool %q although the index identity is unproven", name)
		}
	}
	result, err := DecodeStrict[*CodeReviewOut](out.Data)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	return result
}

func indexDiverged(d *Dossier) {
	d.WorkspaceProvenance.MCPEligible = false
	d.WorkspaceProvenance.IndexSHA = divergentIndexSHA
}

// A node whose bound tools are all withheld for the same reason records one
// gap, not one per tool: a review with several agent nodes would otherwise
// bury the real evidence gaps under near-identical lines.
func TestIndexIdentityBlockedMCPRecordsOneGapForAllBoundTools(t *testing.T) {
	result := indexBlockedCodeReview(t, boundGraphTools, indexDiverged)
	if len(result.ToolGaps) != 1 {
		t.Fatalf("tool gaps = %q, want exactly one gap for the node", result.ToolGaps)
	}
	gap := result.ToolGaps[0]
	for _, want := range []string{
		"block code-review",
		"MCP tools unavailable",
		"3 bound tool(s) blocked by workspace index identity",
		"(index abcdef01, target 33333333)",
	} {
		if !strings.Contains(gap, want) {
			t.Fatalf("gap %q does not contain %q", gap, want)
		}
	}
	if strings.Contains(gap, divergentIndexSHA) || strings.Contains(gap, testStartSHA) {
		t.Fatalf("gap %q shows a full commit id, want the 8 character short form", gap)
	}
}

// The gap still keeps a review from settling a clean PASS, and synthesis sees
// one evidence signal for the node.
func TestIndexIdentityGapStillPreventsCleanPass(t *testing.T) {
	result := indexBlockedCodeReview(t, boundGraphTools, indexDiverged)
	node := completedNode("cr.a", KindCodeReview,
		reviewNodeParams{Package: "a", Files: []string{"a.go"}, Focus: "f", Provider: "local"}, *result)
	wf := evidenceWorkflow(node)
	synth := runEvidenceSynthesis(t, evidenceExecutor(t, wf), wf, AdjudicateOut{Groups: []Group{}})
	if synth.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %s, want INCOMPLETE when the code graph was withheld", synth.Verdict)
	}
	signals := 0
	for _, signal := range synth.Unresolved {
		if signal.Node == "cr.a" && signal.Kind == UnresolvedEvidence && strings.Contains(signal.Message, "blocked by workspace index identity") {
			signals++
		}
	}
	if signals != 1 {
		t.Fatalf("unresolved = %+v, want one index identity signal for the node", synth.Unresolved)
	}
	if !containsText(synth.Warnings, "blocked by workspace index identity") {
		t.Fatalf("warnings = %q, want the index identity gap", synth.Warnings)
	}
}

// Missing or malformed identity evidence is named, never guessed, and never
// panics.
func TestIndexIdentityGapNamesMissingEvidenceAsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Dossier)
		want   string
	}{
		{"no provenance", func(d *Dossier) { d.WorkspaceProvenance = nil }, "(index unknown, target unknown)"},
		{"no index recorded", func(d *Dossier) {
			d.WorkspaceProvenance.MCPEligible = false
			d.WorkspaceProvenance.IndexSHA = ""
		}, "(index unknown, target 33333333)"},
		{"index not a full commit id", func(d *Dossier) { d.WorkspaceProvenance.IndexSHA = "bad" }, "(index bad, target 33333333)"},
		{"index at another commit", func(d *Dossier) { d.WorkspaceProvenance.IndexSHA = testBaseSHA }, "(index 22222222, target 33333333)"},
		{"index ready but not eligible", func(d *Dossier) { d.WorkspaceProvenance.MCPEligible = false }, "(index 33333333, target 33333333)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := indexBlockedCodeReview(t, boundGraphTools, tc.change)
			if len(result.ToolGaps) != 1 || !strings.Contains(result.ToolGaps[0], "3 bound tool(s) blocked by workspace index identity "+tc.want) {
				t.Fatalf("tool gaps = %q, want one gap ending with %q", result.ToolGaps, tc.want)
			}
		})
	}
}

// The gap text is built from stored evidence, so it tolerates a missing dossier
// or provenance and commit ids of any length.
func TestIndexIdentityGapToleratesMissingEvidenceAndOddCommitIDs(t *testing.T) {
	provenance := func(index, target string) *Dossier {
		return &Dossier{WorkspaceProvenance: &WorkspaceProvenance{IndexSHA: index, TargetSHA: target}}
	}
	for _, tc := range []struct {
		name string
		d    *Dossier
		want string
	}{
		{"nil dossier", nil, "(index unknown, target unknown)"},
		{"nil provenance", &Dossier{}, "(index unknown, target unknown)"},
		{"blank values", provenance(" ", ""), "(index unknown, target unknown)"},
		{"short values are kept whole", provenance("abc", "12345678"), "(index abc, target 12345678)"},
		{"long values keep 8 characters", provenance(divergentIndexSHA, testStartSHA), "(index abcdef01, target 33333333)"},
		{"multibyte values are cut on a character boundary", provenance("提交提交提交提交提交", "x"), "(index 提交提交提交提交, target x)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := "2 bound tool(s) blocked by workspace index identity " + tc.want
			if got := indexIdentityGap(tc.d, 2).Error(); got != want {
				t.Fatalf("gap = %q, want %q", got, want)
			}
		})
	}
}

// No effective binding means nothing was withheld, so nothing is recorded.
func TestIndexIdentityBlockedWithoutEffectiveBindingsRecordsNoGap(t *testing.T) {
	result := indexBlockedCodeReview(t, nil, indexDiverged)
	if len(result.ToolGaps) != 0 {
		t.Fatalf("tool gaps = %q, want none without effective MCP bindings", result.ToolGaps)
	}
}
