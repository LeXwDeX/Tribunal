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
	"github.com/LeXwDeX/tribunal/internal/skill"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

type disabledRepositorySkillProvider struct {
	binding SkillBinding
	calls   int
}

func (p *disabledRepositorySkillProvider) BindSkills(context.Context, []string) (SkillBinding, error) {
	p.calls++
	return p.binding, nil
}

func TestRepositorySkillsRemainDisabledBeforeAgentOrMCP(t *testing.T) {
	for _, tc := range []struct {
		name, block, requested string
		repository             bool
		wantError              bool
		wantBindCalls          int
	}{
		{"review-qualified", BlockCodeReview, "repo:demo", true, true, 0},
		{"review-qualified-whitespace", BlockCodeReview, " repo:demo ", true, true, 0},
		{"review-provider-alias", BlockCodeReview, "legacy", true, true, 1},
		{"onboarding-qualified", BlockOnboarding, "repo:demo", true, true, 0},
		{"onboarding-provider-alias", BlockOnboarding, "legacy", true, true, 1},
		{"legacy-skill", BlockCodeReview, "demo", false, false, 1},
	} {
		for _, strict := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/no-ignore", true: "/ignore"}[strict], func(t *testing.T) {
				container := t.TempDir()
				dir := filepath.Join(container, "demo")
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: demo\ndescription: CANARY_DESCRIPTION\n---\nCANARY_BODY\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				opts := skill.Options{PlatformDirs: []string{container}}
				name := "demo"
				if tc.repository {
					opts = skill.Options{RepoSkillsDir: container}
					name = "repo:demo"
				}
				loader, err := skill.New(opts)
				if err != nil {
					t.Fatal(err)
				}
				binding, err := loader.Bind([]string{name})
				if err != nil {
					t.Fatal(err)
				}
				provider := &disabledRepositorySkillProvider{binding: SkillBinding{
					Skills: []Skill{{Name: name, Description: "CANARY_DESCRIPTION"}}, Roots: binding.Roots(), Tool: binding.Tool(),
				}}
				patterns := []string{}
				if strict {
					patterns = []string{"private/**"}
				}
				dossier := &Dossier{IgnorePolicy: &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: patterns}}
				if !strict {
					dossier.DiffVersion.StartSHA = testStartSHA
					dossier.WorkspaceProvenance = &WorkspaceProvenance{Version: WorkspaceProvenanceVersion, TargetSHA: testStartSHA, IndexSHA: testStartSHA, MCPEligible: true}
				}
				mcp := &ignoreMetadataProvider{}
				calls := 0
				e := newTestExecutor(t, ExecutorDeps{
					Dossier: dossier,
					Blocks:  map[string]config.BlockBinding{tc.block: {Skills: []string{tc.requested}, MCPTools: []string{"mcp__test__read"}}},
					Skills:  provider,
					Tools:   mcp,
					AgentRun: func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
						calls++
						for _, tool := range spec.Tools {
							if tool.Def().Name == "load_skill" {
								result, err := tool.Call(ctx, json.RawMessage(`{"name":"`+name+`"}`))
								if err != nil || !strings.Contains(result.Content, "CANARY_BODY") {
									t.Errorf("skill load = %q, %v", result.Content, err)
								}
							}
						}
						return &agent.RunResult{}, nil
					},
				})
				role, _ := roleForKind(KindCodeReview)
				if tc.block == BlockOnboarding {
					role, _ = roleForKind(KindOnboarding)
				}
				_, err = e.callAgent(context.Background(), dag.NodeInput{}, agentOptions{
					Role: role, Provider: "fake", Block: tc.block, BlockTools: true,
					Submit: agent.SubmitSpec{Schema: json.RawMessage(`{}`), Decode: func(json.RawMessage) (any, error) { return nil, nil }},
				})
				if tc.wantError {
					if err == nil || !strings.Contains(err.Error(), "repository skills are disabled") || calls != 0 || provider.calls != tc.wantBindCalls || mcp.calls != 0 {
						t.Fatalf("err=%v agent calls=%d bind calls=%d MCP calls=%d", err, calls, provider.calls, mcp.calls)
					}
				} else if err != nil || calls != 1 || provider.calls != tc.wantBindCalls || (strict && mcp.calls != 0) || (!strict && mcp.calls != 1) {
					t.Fatalf("legacy skill: err=%v agent calls=%d bind calls=%d MCP calls=%d", err, calls, provider.calls, mcp.calls)
				}
			})
		}
	}
}
