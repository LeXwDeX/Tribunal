package review

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

type ignoreMetadataProvider struct {
	calls int
	names []string
}

func TestIgnoredOutputRetainsAgentUsage(t *testing.T) {
	d := validDossier(t)
	d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{"private.txt"}}
	calls := 0
	e := newTestExecutor(t, ExecutorDeps{Dossier: d, AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		calls++
		return &agent.RunResult{Output: &CodeReviewOut{Summary: "bad", Findings: []FindingOut{{Path: "private.txt"}}}, Usage: llm.Usage{InputTokens: 42, OutputTokens: 9, CacheReadTokens: 6, CacheWriteTokens: 3, ReasoningTokens: 2}}, nil
	}, Cost: func(string, llm.Usage) int64 { return 17 }})
	out, err := e.Execute(context.Background(), codeReviewInput(t))
	want := dag.Usage{InputTokens: 42, OutputTokens: 9, CacheReadTokens: 6, CacheWriteTokens: 3, ReasoningTokens: 2, CostMicros: 17}
	if err == nil || knownUsage(err) != want || out.Usage != want || len(out.Data) != 0 || calls != 1 {
		t.Fatalf("error=%v error usage=%+v node output=%+v calls=%d", err, knownUsage(err), out, calls)
	}
}

func TestIgnoredAgentOutputChecksAllFindingContracts(t *testing.T) {
	d := validDossier(t)
	d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{"private/"}}
	d.Ignored = []string{"renamed-public.go"}
	d.HeadDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(d.HeadDir, "allowed.go"), []byte("allowed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e := newTestExecutor(t, ExecutorDeps{Dossier: d})
	for _, path := range []string{"private/a.go", "renamed-public.go", "allowed.go", ""} {
		for name, output := range map[string]any{
			"code-review": &CodeReviewOut{Findings: []FindingOut{{Path: path}}},
			"review":      &ReviewOut{Findings: []FindingOut{{Path: path}}},
			"reasoner":    &ReasonerOut{Findings: []ReasonerFinding{{Path: path}}},
			"verify":      &VerifyOut{Evidence: []CodeRef{{Path: path, StartLine: 1, EndLine: 1, Note: "ok"}}},
			"synthesize":  &SynthesizeOut{NewFindings: []FindingOut{{Path: path}}},
			"extend":      &SynthesizeOut{ExtendRequests: []ExtendRequest{{Files: []string{path}}}},
		} {
			t.Run(name+"/"+path, func(t *testing.T) {
				want := path == "private/a.go" || path == "renamed-public.go" || (name == "verify" && path == "")
				if e.ignoredAgentOutput(output) != want {
					t.Fatalf("ignored output=%v, want %v", output, want)
				}
			})
		}
	}
}

func (p *ignoreMetadataProvider) Tools(context.Context, string) ([]agent.Tool, error) {
	p.calls++
	return nil, nil
}
func (p *ignoreMetadataProvider) BoundToolNames(context.Context, string) ([]string, error) {
	return p.names, nil
}

func TestIgnoreUsesEffectiveMCPBindingsWithoutConnecting(t *testing.T) {
	for _, names := range [][]string{nil, {"mcp__graph__search"}} {
		d := validDossier(t)
		d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{"private"}}
		provider := &ignoreMetadataProvider{names: names}
		var seen []string
		e := newTestExecutor(t, ExecutorDeps{Dossier: d, Blocks: graphBlocks, Tools: provider, AgentRun: submitCodeReview(`{"summary":"ok","findings":[]}`, &seen)})
		out, err := e.Execute(context.Background(), codeReviewInput(t))
		if err != nil {
			t.Fatal(err)
		}
		result, err := DecodeStrict[*CodeReviewOut](out.Data)
		if err != nil || len(result.ToolGaps) != len(names) || provider.calls != 0 {
			t.Fatalf("names=%v gaps=%v calls=%d err=%v", names, result.ToolGaps, provider.calls, err)
		}
	}
}
