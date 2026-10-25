package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/agent"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/review"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

type recordingToolProvider struct {
	calls []string
	tools []agent.Tool
	err   error
}

func (p *recordingToolProvider) Tools(_ context.Context, block string) ([]agent.Tool, error) {
	p.calls = append(p.calls, block)
	return p.tools, p.err
}

type recordingSkillProvider struct {
	calls [][]string
	bind  review.SkillBinding
	err   error
}

func (p *recordingSkillProvider) BindSkills(_ context.Context, names []string) (review.SkillBinding, error) {
	p.calls = append(p.calls, append([]string(nil), names...))
	return p.bind, p.err
}

func TestProjectBindingStubsForceProviderDispatch(t *testing.T) {
	blocks := map[string]config.BlockBinding{
		"code-review": {MCPTools: []string{"platform-tool"}, Skills: []string{"platform-skill"}},
	}
	stubs := projectBindingStubs(blocks)
	binding := stubs["code-review"]
	if len(binding.MCPTools) != 1 || binding.MCPTools[0] != projectMCPStub {
		t.Fatalf("tools stub = %v", binding.MCPTools)
	}
	if len(binding.Skills) != 1 || binding.Skills[0] != projectSkillStubBase+"code-review" {
		t.Fatalf("skills stub = %v", binding.Skills)
	}
	if got := blocks["code-review"]; got.MCPTools[0] != "platform-tool" || got.Skills[0] != "platform-skill" {
		t.Fatalf("source blocks were mutated: %+v", got)
	}
}

func TestProjectAwareSkillProviderUsesProjectBinding(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	projectID := int64(42)
	project := int64(42)

	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"),
		[]byte("---\nname: project-skill\ndescription: Project skill\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	saved, err := service.RegisterSkillDir(ctx, SkillDirInput{Scope: "project", ProjectID: &project, Dir: source})
	if err != nil {
		t.Fatalf("RegisterSkillDir: %v", err)
	}
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "project", ProjectID: &project, Block: "code-review", Skills: []string{"project-skill"},
	}); err != nil {
		t.Fatalf("UpsertBlockBinding: %v", err)
	}
	base := &recordingSkillProvider{bind: review.SkillBinding{Skills: []review.Skill{{Name: "platform-skill"}}}}
	resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{
		Store:      service.store,
		Blocks:     map[string]config.BlockBinding{"code-review": {Skills: []string{"platform-skill"}}},
		Workspace:  service.workspace,
		BaseSkills: base,
	})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })
	provider := &projectAwareSkillProvider{
		resolver: resolver,
		base:     base,
		blocks:   map[string]config.BlockBinding{"code-review": {Skills: []string{"platform-skill"}}},
	}

	projectCtx := context.WithValue(ctx, projectIDContextKey{}, projectID)
	binding, err := provider.BindSkills(projectCtx, []string{projectSkillStubBase + "code-review"})
	if err != nil {
		t.Fatalf("BindSkills: %v", err)
	}
	names := make([]string, 0, len(binding.Skills))
	for _, item := range binding.Skills {
		names = append(names, item.Name)
	}
	if strings.Join(names, ",") != "project-skill" {
		t.Fatalf("skills = %v", names)
	}
	if len(base.calls) != 0 {
		t.Fatalf("platform provider was used for a project binding: %v", base.calls)
	}
	if _, err := os.Stat(saved.Dir); err != nil {
		t.Fatalf("project skill dir: %v", err)
	}

	// Without a project in the context the platform binding is used.
	platformBase := &recordingSkillProvider{bind: review.SkillBinding{Skills: []review.Skill{{Name: "platform-skill"}}}}
	provider.base = platformBase
	binding, err = provider.BindSkills(ctx, []string{projectSkillStubBase + "code-review"})
	if err != nil {
		t.Fatalf("platform BindSkills: %v", err)
	}
	if len(binding.Skills) != 1 || binding.Skills[0].Name != "platform-skill" {
		t.Fatalf("platform skills = %+v", binding.Skills)
	}
	if len(platformBase.calls) != 1 || platformBase.calls[0][0] != "platform-skill" {
		t.Fatalf("platform calls = %v", platformBase.calls)
	}
}

func TestProjectAwareToolProviderDelegatesByProject(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	routerBase := &recordingToolProvider{err: errors.New("platform tool provider must not be called")}
	resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{
		Store:     service.store,
		Workspace: service.workspace,
	})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })
	provider := &projectAwareToolProvider{resolver: resolver, base: routerBase}

	projectCtx := context.WithValue(ctx, projectIDContextKey{}, int64(42))
	tools, err := provider.Tools(projectCtx, "code-review")
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if tools != nil {
		t.Fatalf("tools = %v", tools)
	}
	if len(routerBase.calls) != 0 {
		t.Fatalf("project path fell back to platform: %v", routerBase.calls)
	}

	routerBase.calls = nil
	routerBase.err = nil
	routerBase.tools = []agent.Tool{nil}
	if _, err := provider.Tools(ctx, "code-review"); err != nil {
		t.Fatalf("platform Tools: %v", err)
	}
	if len(routerBase.calls) != 1 || routerBase.calls[0] != "code-review" {
		t.Fatalf("platform calls = %v", routerBase.calls)
	}
}

func TestExecutorRegistryInjectsProjectContext(t *testing.T) {
	registry := newExecutorRegistry()
	registry.SetProjectLookup(func(_ context.Context, workflowID string) (int64, error) {
		if workflowID == "wf-42" {
			return 42, nil
		}
		return 0, errors.New("unknown workflow")
	})
	ctx, err := registry.withProject(context.Background(), "wf-42")
	if err != nil {
		t.Fatalf("withProject: %v", err)
	}
	id, ok := projectIDFromContext(ctx)
	if !ok || id != 42 {
		t.Fatalf("project id = %d, %v", id, ok)
	}
	if _, err := registry.withProject(context.Background(), "missing"); err == nil {
		t.Fatal("failed lookup returned a project context")
	}

	standalone := context.Background()
	unresolved, err := newExecutorRegistry().withProject(standalone, "wf-42")
	if err != nil || unresolved != standalone {
		t.Fatalf("standalone registry = %v, %v", unresolved, err)
	}
}

func TestExecutorRegistryFailsClosedOnProjectLookup(t *testing.T) {
	ctx := context.Background()
	lookupErr := errors.New("workflow lookup failed")
	calls := 0
	executor, err := review.NewExecutor(review.ExecutorDeps{
		Profile: review.Profile{Name: "default", Config: testReviewProfile()},
		AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			calls++
			return &agent.RunResult{Output: map[string]any{}}, nil
		},
	})
	if err != nil {
		t.Fatalf("review.NewExecutor: %v", err)
	}
	registry := newExecutorRegistry()
	registry.Register("wf-failed", executor)
	registry.SetProjectLookup(func(context.Context, string) (int64, error) {
		return 0, lookupErr
	})

	if _, err := registry.Execute(ctx, dag.NodeInput{
		WorkflowID: "wf-failed",
		Node:       dag.NodeSpec{ID: "node", Kind: review.KindReasoner},
	}); err == nil || !strings.Contains(err.Error(), "resolve workflow") || !errors.Is(err, lookupErr) {
		t.Fatalf("Execute error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("executor ran after lookup failure: %d calls", calls)
	}
	if _, err := registry.Expand(ctx, dag.WorkflowView{ID: "wf-failed"}, dag.NodeSpec{ID: "node"}, nil); err == nil || !strings.Contains(err.Error(), "resolve workflow") {
		t.Fatalf("Expand error = %v", err)
	}

	registry.SetProjectLookup(func(context.Context, string) (int64, error) {
		return 0, nil
	})
	if _, err := registry.Execute(ctx, dag.NodeInput{
		WorkflowID: "wf-failed",
		Node:       dag.NodeSpec{ID: "node", Kind: review.KindReasoner},
	}); err == nil || !strings.Contains(err.Error(), "has no project") {
		t.Fatalf("missing project error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("executor ran without a project: %d calls", calls)
	}
}

func TestProjectAwareProvidersFailClosedOnResolverError(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	projectID := int64(42)
	baseTools := &recordingToolProvider{err: errors.New("platform fallback must not run")}
	baseSkills := &recordingSkillProvider{err: errors.New("platform fallback must not run")}
	resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{
		Store:      service.store,
		Workspace:  service.workspace,
		BaseSkills: baseSkills,
	})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })
	if err := service.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	projectCtx := context.WithValue(ctx, projectIDContextKey{}, projectID)

	tools := &projectAwareToolProvider{resolver: resolver, base: baseTools}
	if _, err := tools.Tools(projectCtx, "code-review"); err == nil {
		t.Fatal("tool provider fell back after a resolver error")
	}
	if len(baseTools.calls) != 0 {
		t.Fatalf("platform tool provider was called: %v", baseTools.calls)
	}

	skills := &projectAwareSkillProvider{
		resolver: resolver,
		base:     baseSkills,
		blocks:   map[string]config.BlockBinding{"code-review": {Skills: []string{"platform-skill"}}},
	}
	if _, err := skills.BindSkills(projectCtx, []string{projectSkillStubBase + "code-review"}); err == nil {
		t.Fatal("skill provider fell back after a resolver error")
	}
	if len(baseSkills.calls) != 0 {
		t.Fatalf("platform skill provider was called: %v", baseSkills.calls)
	}
}

func TestProjectExtensionResolverCloseRejectsForProject(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{
		Store:     service.store,
		Workspace: service.workspace,
	})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })

	if err := resolver.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := resolver.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := resolver.ForProject(ctx, 42); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("ForProject after Close = %v, want a closed error", err)
	}
}

func TestProjectResolverSeesPlatformSkillsUploadedAfterStartup(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"),
		[]byte("---\nname: later-skill\ndescription: Uploaded after startup\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterSkillDir(ctx, SkillDirInput{Scope: "platform", Dir: source}); err != nil {
		t.Fatalf("platform skill: %v", err)
	}
	// The resolver starts from the startup snapshot (no platform skills).
	resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{
		Store:     service.store,
		Workspace: service.workspace,
	})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })
	set, err := resolver.ForProject(ctx, 42)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	if set.loader == nil {
		t.Fatal("platform skill was not loaded")
	}
	if names := catalogueNames(set.loader); !containsString(names, "later-skill") {
		t.Fatalf("catalogue = %v", names)
	}
}
