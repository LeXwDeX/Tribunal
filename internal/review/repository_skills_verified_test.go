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
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func repositorySkillTestDossier(t *testing.T) *Dossier {
	t.Helper()
	return &Dossier{
		IgnorePolicy: &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{}},
		RepositorySkillProvenance: &RepositorySkillProvenance{
			Version:        RepositorySkillProvenanceVersion,
			Mode:           "repository",
			TargetBranch:   "main",
			TargetSHA:      strings.Repeat("a", 40),
			SnapshotSHA256: strings.Repeat("b", 64),
			GrantRevisions: map[string]string{BlockCodeReview: strings.Repeat("c", 64)},
		},
	}
}

func repositorySkillTestBinding(t *testing.T, dir string) SkillBinding {
	t.Helper()
	child := filepath.Join(dir, "demo")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "SKILL.md"), []byte("---\nname: demo\ndescription: visible\n---\nrepository body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loader, err := skill.New(skill.Options{RepoSkillsDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := loader.Bind([]string{"repo:demo"})
	if err != nil {
		t.Fatal(err)
	}
	return SkillBinding{Skills: []Skill{{Name: "repo:demo", Description: "visible"}}, Roots: binding.Roots(), Tool: binding.Tool()}
}

func TestRepositorySkillsRequireVerifiedProvenance(t *testing.T) {
	dir := t.TempDir()
	provider := &disabledRepositorySkillProvider{binding: repositorySkillTestBinding(t, dir)}
	calls := 0
	deps := ExecutorDeps{
		Dossier: repositorySkillTestDossier(t),
		Blocks:  map[string]config.BlockBinding{BlockCodeReview: {Skills: []string{"repo:demo"}}},
		Skills:  provider,
		AgentRun: func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
			calls++
			for _, tool := range spec.Tools {
				if tool.Def().Name == "load_skill" {
					result, err := tool.Call(ctx, json.RawMessage(`{"name":"repo:demo"}`))
					if err != nil || !strings.Contains(result.Content, "repository body") {
						t.Errorf("repository load = %q, %v", result.Content, err)
					}
				}
			}
			return &agent.RunResult{}, nil
		},
	}
	deps.RepositorySkillsVerified = true
	e := newTestExecutor(t, deps)
	role, _ := roleForKind(KindCodeReview)
	if _, err := e.callAgent(context.Background(), dag.NodeInput{}, agentOptions{
		Role: role, Provider: "fake", Block: BlockCodeReview,
		Submit: agent.SubmitSpec{Schema: json.RawMessage(`{}`), Decode: func(json.RawMessage) (any, error) { return nil, nil }},
	}); err != nil || calls != 1 || provider.calls != 1 {
		t.Fatalf("verified repository skill: err=%v calls=%d bind=%d", err, calls, provider.calls)
	}

	deps.RepositorySkillsVerified = false
	unverified := newTestExecutor(t, deps)
	_, err := unverified.callAgent(context.Background(), dag.NodeInput{}, agentOptions{
		Role: role, Provider: "fake", Block: BlockCodeReview,
		Submit: agent.SubmitSpec{Schema: json.RawMessage(`{}`), Decode: func(json.RawMessage) (any, error) { return nil, nil }},
	})
	if err == nil || !strings.Contains(err.Error(), "repository skills are disabled") {
		t.Fatalf("unverified repository skill = %v", err)
	}
}

func TestVerifiedRepositorySkillsNeedRepositoryProvenance(t *testing.T) {
	dossier := repositorySkillTestDossier(t)
	dossier.RepositorySkillProvenance = &RepositorySkillProvenance{Version: RepositorySkillProvenanceVersion, Mode: "none"}
	if _, err := NewExecutor(ExecutorDeps{Dossier: dossier, RepositorySkillsVerified: true}); err == nil {
		t.Fatal("verified repository skills accepted none provenance")
	}
	dossier.RepositorySkillProvenance = &RepositorySkillProvenance{
		Version: RepositorySkillProvenanceVersion, Mode: "repository",
		TargetBranch: "main", TargetSHA: strings.Repeat("a", 40), SnapshotSHA256: "invalid",
		GrantRevisions: map[string]string{BlockCodeReview: store.BlockBindingAbsentRevision},
	}
	if _, err := NewExecutor(ExecutorDeps{Dossier: dossier, RepositorySkillsVerified: true}); err == nil {
		t.Fatal("verified repository skills accepted invalid provenance")
	}
}
