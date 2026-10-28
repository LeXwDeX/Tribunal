package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
)

type countIgnoreTools struct{ calls int }

func (c *countIgnoreTools) Tools(context.Context, string) ([]agent.Tool, error) {
	c.calls++
	return nil, nil
}

func TestIgnorePolicySnapshotCompatibility(t *testing.T) {
	legacy := validDossier(t)
	before, err := MarshalDossier(legacy)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := DossierHash(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Dossier
	if err := json.Unmarshal(before, &decoded); err != nil {
		t.Fatal(err)
	}
	after, err := MarshalDossier(&decoded)
	if err != nil || string(before) != string(after) {
		t.Fatalf("legacy changed: %s %s %v", before, after, err)
	}
	again, err := DossierHash(&decoded)
	if err != nil || again != hash {
		t.Fatalf("legacy hash changed: %s %s %v", hash, again, err)
	}
	decoded.IgnorePolicy = &IgnorePolicy{Version: 1, Patterns: []string{}}
	current, err := MarshalDossier(&decoded)
	if err != nil || !strings.Contains(string(current), `"patterns":[]`) {
		t.Fatalf("empty policy = %s, %v", current, err)
	}
	currentHash, err := DossierHash(&decoded)
	if err != nil || currentHash == hash {
		t.Fatalf("policy hash = %s %v", currentHash, err)
	}
}

func TestIgnorePolicyAgentToolsAndMCP(t *testing.T) {
	head := t.TempDir()
	if err := os.WriteFile(filepath.Join(head, "private.txt"), []byte("CANARY_PRIVATE\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(head, "allowed.txt"), []byte("ALLOWED\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		patterns  []string
		bound     bool
		wantGap   bool
		wantCalls int
		denied    bool
	}{
		{"nonempty-bound", []string{"private.txt"}, true, false, 0, true},
		{"nonempty-unbound", []string{"private.txt"}, false, false, 0, true},
		{"empty-bound", []string{}, true, false, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dossier := validDossier(t)
			dossier.HeadDir = head
			dossier.IgnorePolicy = &IgnorePolicy{Version: 1, Patterns: tc.patterns}
			provider := &countIgnoreTools{}
			blocks := map[string]config.BlockBinding{}
			if tc.bound {
				blocks[BlockCodeReview] = config.BlockBinding{MCPTools: []string{"mcp__graph__search"}}
			}
			runner := func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
				for _, tool := range spec.Tools {
					if tool.Def().Name == "read_file" {
						result, err := tool.Call(ctx, json.RawMessage(`{"path":"private.txt"}`))
						if (err != nil) != tc.denied {
							t.Errorf("read = %q, %v", result.Content, err)
						}
						result, err = tool.Call(ctx, json.RawMessage(`{"path":"allowed.txt"}`))
						if err != nil || !strings.Contains(result.Content, "ALLOWED") {
							t.Errorf("allowed = %q, %v", result.Content, err)
						}
					}
					if tool.Def().Name == "grep" {
						result, err := tool.Call(ctx, json.RawMessage(`{"pattern":"CANARY"}`))
						if err != nil || strings.Contains(result.Content, "CANARY_PRIVATE") == tc.denied {
							t.Errorf("grep = %q, %v", result.Content, err)
						}
					}
				}
				decoded, err := spec.Submit.Decode(json.RawMessage(`{"summary":"ok","findings":[]}`))
				return &agent.RunResult{Output: decoded}, err
			}
			executor := newTestExecutor(t, ExecutorDeps{Dossier: dossier, Blocks: blocks, Tools: provider, AgentRun: runner})
			out, err := executor.Execute(context.Background(), codeReviewInput(t))
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeStrict[*CodeReviewOut](out.Data)
			if err != nil {
				t.Fatal(err)
			}
			if (len(decoded.ToolGaps) > 0) != tc.wantGap || provider.calls != tc.wantCalls {
				t.Fatalf("gaps=%v provider calls=%d", decoded.ToolGaps, provider.calls)
			}
		})
	}
}

func TestIgnorePolicyLegacyExecutorRejectsBeforeAgentAndMCP(t *testing.T) {
	for _, policy := range []*IgnorePolicy{nil, {Version: 2, Patterns: []string{}}, {Version: 1}} {
		d := validDossier(t)
		provider := &countIgnoreTools{}
		calls := 0
		e := newTestExecutor(t, ExecutorDeps{Dossier: d, Blocks: graphBlocks, Tools: provider,
			AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) { calls++; return nil, nil }})
		e.deps.Dossier.IgnorePolicy = policy
		if _, err := e.Execute(context.Background(), codeReviewInput(t)); err == nil {
			t.Fatalf("invalid policy executed: %+v", policy)
		}
		if calls != 0 || provider.calls != 0 {
			t.Fatalf("agent=%d MCP=%d", calls, provider.calls)
		}
	}
}
