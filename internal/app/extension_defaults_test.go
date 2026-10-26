package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

var defaultCodeReview = []config.DefaultSkill{{Name: "code-review", Blocks: []string{"code-review", "review"}}}

// applySkillDefaults runs one startup pass of the default extension seeder.
func applySkillDefaults(t *testing.T, service *ExtensionService, st *sqlite.Store, fileBlocks map[string]config.BlockBinding, specs []config.DefaultSkill) {
	t.Helper()
	defaults, err := service.SkillDefaults(specs)
	if err != nil {
		t.Fatalf("SkillDefaults: %v", err)
	}
	if err := ApplyExtensionDefaults(context.Background(), st, fileBlocks, defaults, nil); err != nil {
		t.Fatalf("ApplyExtensionDefaults: %v", err)
	}
}

func platformSkillNames(t *testing.T, st *sqlite.Store) []string {
	t.Helper()
	rows, err := st.ListSkills(context.Background(), store.ScopePlatform, nil)
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	return names
}

func platformBindings(t *testing.T, st *sqlite.Store) map[string]store.BlockBinding {
	t.Helper()
	rows, err := st.ListBlockBindings(context.Background(), store.ScopePlatform, nil)
	if err != nil {
		t.Fatalf("ListBlockBindings: %v", err)
	}
	out := make(map[string]store.BlockBinding, len(rows))
	for _, row := range rows {
		out[row.Block] = row
	}
	return out
}

func TestSkillDefaultsRegisterAndBindOnFreshStore(t *testing.T) {
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)

	skills, err := service.ListSkills(context.Background(), store.ScopePlatform, nil)
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if len(skills) != 1 || skills[0].Name != "code-review" || !skills[0].Enabled || !skills[0].Builtin {
		t.Fatalf("platform skills = %+v, want enabled built-in code-review", skills)
	}
	rel, err := filepath.Rel(managedSkillsRoot(service.workspace), skills[0].Dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("skill dir %q is outside the managed root", skills[0].Dir)
	}
	if _, managed := service.managedSkillContainer(&store.Skill{Dir: skills[0].Dir}); !managed {
		t.Fatalf("skill dir %q is not deletable as a managed container", skills[0].Dir)
	}
	if !strings.Contains(skills[0].Description, "Standards") {
		t.Fatalf("description = %q", skills[0].Description)
	}

	bindings := platformBindings(t, st)
	for _, block := range []string{"code-review", "review"} {
		if got := bindings[block].Skills; !reflect.DeepEqual(got, []string{"code-review"}) {
			t.Fatalf("%s skills = %v, want [code-review]", block, got)
		}
	}
	if len(bindings) != 2 {
		t.Fatalf("bindings = %+v, want only code-review and review", bindings)
	}
}

func TestSkillDefaultsAreIdempotentAcrossRestarts(t *testing.T) {
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	before := platformBindings(t, st)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)

	if got := platformSkillNames(t, st); !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("skills after restart = %v", got)
	}
	if after := platformBindings(t, st); !reflect.DeepEqual(after, before) {
		t.Fatalf("bindings changed on restart:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestSkillDefaultsRespectAdministratorUnbind(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)

	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: store.ScopePlatform, Block: "review", Skills: []string{},
	}); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	applySkillDefaults(t, service, st, nil, defaultCodeReview)

	bindings := platformBindings(t, st)
	if got := bindings["review"].Skills; len(got) != 0 {
		t.Fatalf("review skills after restart = %v, want unbound", got)
	}
	if got := bindings["code-review"].Skills; !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("code-review skills = %v, want untouched default", got)
	}
}

func TestBuiltinSkillCannotBeDeleted(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	skills, _ := service.ListSkills(ctx, store.ScopePlatform, nil)
	err := service.DeleteSkill(ctx, skills[0].ID)
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Fatalf("DeleteSkill(builtin) = %v, want conflict", err)
	}
	if got := platformSkillNames(t, st); !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("builtin skill removed: %v", got)
	}
	if _, err := os.Stat(filepath.Join(skills[0].Dir, "SKILL.md")); err != nil {
		t.Fatalf("builtin skill files removed: %v", err)
	}
}

func TestDisabledBuiltinSkillStaysDisabled(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	skills, _ := service.ListSkills(ctx, store.ScopePlatform, nil)
	disabled, err := service.SetSkillEnabled(ctx, skills[0].ID, false)
	if err != nil || disabled.Enabled || !disabled.Builtin {
		t.Fatalf("SetSkillEnabled = %+v, %v", disabled, err)
	}
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	skills, _ = service.ListSkills(ctx, store.ScopePlatform, nil)
	if len(skills) != 1 || skills[0].Enabled {
		t.Fatalf("disabled builtin re-enabled on restart: %+v", skills)
	}
	if _, err := service.SetSkillEnabled(ctx, "missing", true); err == nil {
		t.Fatal("SetSkillEnabled(missing) succeeded")
	}
}

// A newer release ships different skill files: the next start refreshes the
// installed copy in place and keeps the administrator's enabled state.
func TestBuiltinSkillUpgradesInstalledContent(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	skills, _ := service.ListSkills(ctx, store.ScopePlatform, nil)
	installed := skills[0]
	if _, err := service.SetSkillEnabled(ctx, installed.ID, false); err != nil {
		t.Fatal(err)
	}
	// Simulate the copy an older release installed.
	writeSkill(t, installed.Dir, "code-review", "Old description")
	if err := os.WriteFile(filepath.Join(installed.Dir, "OBSOLETE.md"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSkillDescription(ctx, installed.ID, "Old description"); err != nil {
		t.Fatal(err)
	}

	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	skills, _ = service.ListSkills(ctx, store.ScopePlatform, nil)
	if len(skills) != 1 {
		t.Fatalf("skills after upgrade = %+v", skills)
	}
	upgraded := skills[0]
	if upgraded.ID != installed.ID || upgraded.Dir != installed.Dir {
		t.Fatalf("upgrade replaced the row: before %+v after %+v", installed, upgraded)
	}
	if upgraded.Enabled {
		t.Fatal("upgrade re-enabled a disabled builtin skill")
	}
	if !strings.Contains(upgraded.Description, "Standards") {
		t.Fatalf("description not refreshed: %q", upgraded.Description)
	}
	body, err := os.ReadFile(filepath.Join(upgraded.Dir, "SKILL.md"))
	if err != nil || !strings.Contains(string(body), "Refused Bequest") {
		t.Fatalf("SKILL.md not refreshed: %v\n%s", err, body)
	}
	if _, err := os.Stat(filepath.Join(upgraded.Dir, "OBSOLETE.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale file kept after upgrade: %v", err)
	}
	if got := platformBindings(t, st)["review"].Skills; !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("upgrade changed bindings: %v", got)
	}
}

func TestSkillDefaultsKeepExistingCustomBindings(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	// An upgraded deployment already customised the review block.
	custom := []string{"mcp__codebase-memory__search_graph"}
	if _, err := st.UpsertBlockBinding(ctx, store.BlockBinding{
		Scope: store.ScopePlatform, Block: "review", MCPTools: custom, Skills: []string{},
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	applySkillDefaults(t, service, st, nil, defaultCodeReview)

	bindings := platformBindings(t, st)
	if got := bindings["review"]; !reflect.DeepEqual(got.MCPTools, custom) || len(got.Skills) != 0 {
		t.Fatalf("custom review binding overwritten: %+v", got)
	}
	if got := bindings["code-review"].Skills; !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("code-review skills = %v, want default", got)
	}

	// The skipped block stays skipped even if the administrator later clears it.
	if _, err := st.UpsertBlockBinding(ctx, store.BlockBinding{Scope: store.ScopePlatform, Block: "review"}); err != nil {
		t.Fatal(err)
	}
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	if got := platformBindings(t, st)["review"].Skills; len(got) != 0 {
		t.Fatalf("skipped default applied on a later start: %v", got)
	}
}

func TestSkillDefaultsKeepFileConfiguredBlocks(t *testing.T) {
	service, st := newExtensionFixture(t)
	fileBlocks := map[string]config.BlockBinding{"review": {Skills: []string{"coding-standards"}}}
	applySkillDefaults(t, service, st, fileBlocks, defaultCodeReview)

	bindings := platformBindings(t, st)
	if _, ok := bindings["review"]; ok {
		t.Fatalf("file-configured review block received a database row: %+v", bindings["review"])
	}
	if got := bindings["code-review"].Skills; !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("code-review skills = %v", got)
	}
}

func TestSkillDefaultsDoNotReplaceExistingSkill(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	dir := t.TempDir()
	writeSkill(t, dir, "code-review", "Team review rules")
	if _, err := service.RegisterSkillDir(ctx, SkillDirInput{Scope: store.ScopePlatform, Dir: dir}); err != nil {
		t.Fatalf("RegisterSkillDir: %v", err)
	}
	applySkillDefaults(t, service, st, nil, defaultCodeReview)

	skills, _ := service.ListSkills(ctx, store.ScopePlatform, nil)
	if len(skills) != 1 || skills[0].Description != "Team review rules" {
		t.Fatalf("existing skill replaced: %+v", skills)
	}
	if bindings := platformBindings(t, st); len(bindings) != 0 {
		t.Fatalf("existing skill was bound by default: %+v", bindings)
	}
}

func TestSkillDefaultsRejectUnknownBuiltin(t *testing.T) {
	service, _ := newExtensionFixture(t)
	if _, err := service.SkillDefaults([]config.DefaultSkill{{Name: "missing"}}); err == nil {
		t.Fatal("SkillDefaults accepted an unknown built-in skill")
	}
}

// Defaults from different kinds merge into one default-owned row, so a later
// release can add another default to a block it seeded before.
func TestExtensionDefaultsMergeIntoDefaultOwnedBinding(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)

	installed := 0
	mcpDefault := ExtensionDefault{
		Kind:     "mcp",
		Name:     "codebase-memory",
		Exists:   func(context.Context) (bool, error) { return false, nil },
		Install:  func(context.Context) error { installed++; return nil },
		Bindings: map[string]DefaultGrant{"review": {MCPTools: []string{"mcp__codebase-memory__search_graph"}}},
	}
	for i := 0; i < 2; i++ {
		if err := ApplyExtensionDefaults(ctx, st, nil, []ExtensionDefault{mcpDefault}, nil); err != nil {
			t.Fatalf("ApplyExtensionDefaults: %v", err)
		}
	}
	if installed != 1 {
		t.Fatalf("install calls = %d, want 1", installed)
	}
	got := platformBindings(t, st)["review"]
	if !reflect.DeepEqual(got.Skills, []string{"code-review"}) ||
		!reflect.DeepEqual(got.MCPTools, []string{"mcp__codebase-memory__search_graph"}) {
		t.Fatalf("merged review binding = %+v", got)
	}
}

func TestExtensionDefaultsSurfaceInstallErrors(t *testing.T) {
	ctx := context.Background()
	_, st := newExtensionFixture(t)
	failing := ExtensionDefault{
		Kind:     "mcp",
		Name:     "broken",
		Exists:   func(context.Context) (bool, error) { return false, nil },
		Install:  func(context.Context) error { return errors.New("boom") },
		Bindings: map[string]DefaultGrant{"review": {MCPTools: []string{"mcp__broken__x"}}},
	}
	if err := ApplyExtensionDefaults(ctx, st, nil, []ExtensionDefault{failing}, nil); err == nil {
		t.Fatal("install error was swallowed")
	}
	rows, err := st.ListExtensionDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("failed install was recorded: %+v", rows)
	}
	if bindings := platformBindings(t, st); len(bindings) != 0 {
		t.Fatalf("binding written for a failed install: %+v", bindings)
	}
}

func TestExtensionDefaultsRejectInvalidDefinitions(t *testing.T) {
	ctx := context.Background()
	_, st := newExtensionFixture(t)
	ok := func(context.Context) (bool, error) { return false, nil }
	install := func(context.Context) error { return nil }
	for _, d := range []ExtensionDefault{
		{Kind: "", Name: "x", Exists: ok, Install: install},
		{Kind: "skill", Name: " ", Exists: ok, Install: install},
		{Kind: "skill", Name: "x", Install: install},
		{Kind: "skill", Name: "x", Exists: ok},
		{Kind: "skill", Name: "x", Exists: ok, Install: install, Bindings: map[string]DefaultGrant{" ": {Skills: []string{"x"}}}},
	} {
		if err := ApplyExtensionDefaults(ctx, st, nil, []ExtensionDefault{d}, nil); err == nil {
			t.Fatalf("invalid default accepted: %+v", d)
		}
	}
}

// End to end through the runtime: the first start registers and binds the
// skill, the bound block lists it and load_skill returns the adapted body; a
// later start keeps the administrator's unbind.
func TestRuntimeSeedsDefaultSkillForReviewBlocks(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	root := t.TempDir()
	cfg := &config.Config{
		Workspace: config.WorkspaceConfig{Root: root, RunCleanupAfter: time.Hour},
		Storage: config.StorageConfig{
			LLMCallRetention: time.Hour, DeliveryRetention: time.Hour, JobRetention: time.Hour, ContextVersions: 1,
		},
		Engine:            config.EngineConfig{MaxConcurrentNodes: 1, DefaultNodeTimeout: time.Minute},
		Profiles:          testProfiles(),
		DefaultExtensions: config.DefaultExtensionsConfig{Skills: defaultCodeReview},
	}
	start := func() *Runtime {
		t.Helper()
		runtime, err := NewRuntime(ctx, cfg, RuntimeDeps{
			Store: st, Cipher: cipher, GitLab: newRuntimeTestGitLab(), LLM: runtimeTestClients{}, Git: repoGit{git: &repo.Git{}},
		})
		if err != nil {
			t.Fatalf("NewRuntime: %v", err)
		}
		t.Cleanup(func() { _ = runtime.Close() })
		return runtime
	}

	runtime := start()
	seedProject(t, st, cipher, 42, "webhook-secret", "", "default")
	provider := &projectAwareSkillProvider{resolver: runtime.projectExts}
	projectCtx := context.WithValue(ctx, projectIDContextKey{}, int64(42))
	for _, block := range []string{"code-review", "review"} {
		binding, err := provider.BindSkills(projectCtx, []string{projectSkillStubBase + block})
		if err != nil {
			t.Fatalf("BindSkills %s: %v", block, err)
		}
		if len(binding.Skills) != 1 || binding.Skills[0].Name != "code-review" || binding.Tool == nil {
			t.Fatalf("%s binding = %+v, want code-review with load_skill", block, binding)
		}
		raw, _ := json.Marshal(map[string]string{"name": "code-review"})
		result, err := binding.Tool.Call(ctx, raw)
		if err != nil {
			t.Fatalf("load_skill: %v", err)
		}
		if !strings.Contains(result.Content, "Refused Bequest") || strings.Contains(strings.ToLower(result.Content), "sub-agent") {
			t.Fatalf("load_skill returned an unexpected body:\n%s", result.Content)
		}
	}
	for _, block := range []string{"verify", "reasoner"} {
		binding, err := provider.BindSkills(projectCtx, []string{projectSkillStubBase + block})
		if err != nil || len(binding.Skills) != 0 {
			t.Fatalf("%s binding = %+v, %v; want none", block, binding, err)
		}
	}

	if _, err := st.UpsertBlockBinding(ctx, store.BlockBinding{Scope: store.ScopePlatform, Block: "code-review"}); err != nil {
		t.Fatal(err)
	}
	restarted := start()
	set, err := restarted.projectExts.ForProject(ctx, 42)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	if got := set.blocks["code-review"].Skills; len(got) != 0 {
		t.Fatalf("unbound default restored on restart: %v", got)
	}
	if got := set.blocks["review"].Skills; !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("review skills after restart = %v", got)
	}
}

// A disabled skill that a block still names is skipped at bind time instead
// of failing the review node.
func TestRuntimeSkipsDisabledBoundSkill(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	cfg := &config.Config{
		Workspace: config.WorkspaceConfig{Root: t.TempDir(), RunCleanupAfter: time.Hour},
		Storage: config.StorageConfig{
			LLMCallRetention: time.Hour, DeliveryRetention: time.Hour, JobRetention: time.Hour, ContextVersions: 1,
		},
		Engine:            config.EngineConfig{MaxConcurrentNodes: 1, DefaultNodeTimeout: time.Minute},
		Profiles:          testProfiles(),
		DefaultExtensions: config.DefaultExtensionsConfig{Skills: defaultCodeReview},
	}
	deps := RuntimeDeps{Store: st, Cipher: cipher, GitLab: newRuntimeTestGitLab(), LLM: runtimeTestClients{}, Git: repoGit{git: &repo.Git{}}}
	first, err := NewRuntime(ctx, cfg, deps)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	_ = first.Close()
	rows, err := st.ListSkills(ctx, store.ScopePlatform, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListSkills = %+v, %v", rows, err)
	}
	if err := st.SetSkillEnabled(ctx, rows[0].ID, false); err != nil {
		t.Fatal(err)
	}

	runtime, err := NewRuntime(ctx, cfg, deps)
	if err != nil {
		t.Fatalf("NewRuntime after disable: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	seedProject(t, st, cipher, 42, "webhook-secret", "", "default")
	projectCtx := context.WithValue(ctx, projectIDContextKey{}, int64(42))
	providers := map[string]*projectAwareSkillProvider{
		"project":  {resolver: runtime.projectExts},
		"platform": runtime.skillBinder,
	}
	for name, provider := range providers {
		callCtx := projectCtx
		if name == "platform" {
			callCtx = ctx
		}
		binding, err := provider.BindSkills(callCtx, []string{projectSkillStubBase + "code-review"})
		if err != nil {
			t.Fatalf("%s BindSkills with a disabled skill: %v", name, err)
		}
		if len(binding.Skills) != 0 || binding.Tool != nil {
			t.Fatalf("%s binding = %+v, want disabled skill skipped", name, binding)
		}
	}
}

func writeSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\nBody.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
