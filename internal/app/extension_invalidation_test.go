package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

// newInvalidationFixture wires an ExtensionService to a ProjectExtensionResolver
// the same way the runtime does: successful writes invalidate the resolver.
func newInvalidationFixture(t *testing.T) (*ExtensionService, *ProjectExtensionResolver) {
	t.Helper()
	st := openTestStore(t)
	seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
	seedProject(t, st, newTestCipher(t), 43, "whsec", "{}", "default")
	manager := workspace.NewManager(t.TempDir(), nil)
	resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{
		Store:     st,
		Workspace: manager.Root(),
	})
	if err != nil {
		t.Fatalf("NewProjectExtensionResolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })
	service, err := NewExtensionService(ExtensionServiceConfig{
		Store:             st,
		Workspace:         manager,
		InvalidateProject: resolver.InvalidateProject,
		InvalidateAll:     resolver.InvalidateAll,
	})
	if err != nil {
		t.Fatalf("NewExtensionService: %v", err)
	}
	return service, resolver
}

func resolveInvalidationProject(t *testing.T, resolver *ProjectExtensionResolver, projectID int64) *projectExtensionSet {
	t.Helper()
	set, err := resolver.ForProject(context.Background(), projectID)
	if err != nil {
		t.Fatalf("ForProject(%d): %v", projectID, err)
	}
	t.Cleanup(set.release)
	return set
}

func createInvalidationMCP(t *testing.T, service *ExtensionService, scope string, projectID *int64, name, command string) *MCPServer {
	t.Helper()
	server, err := service.CreateMCPServer(context.Background(), MCPServerInput{
		Scope:     scope,
		ProjectID: projectID,
		Name:      name,
		Enabled:   true,
		Config:    json.RawMessage(`{"command":"` + command + `"}`),
	})
	if err != nil {
		t.Fatalf("CreateMCPServer(%s/%s): %v", scope, name, err)
	}
	return server
}

func registerInvalidationSkill(t *testing.T, service *ExtensionService, scope string, projectID *int64, name string) *Skill {
	t.Helper()
	source := t.TempDir()
	manifest := "---\nname: " + name + "\ndescription: Synthetic skill\n---\n"
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	saved, err := service.RegisterSkillDir(context.Background(), SkillDirInput{
		Scope: scope, ProjectID: projectID, Dir: source,
	})
	if err != nil {
		t.Fatalf("RegisterSkillDir(%s/%s): %v", scope, name, err)
	}
	return saved
}

func TestExtensionWritesTakeEffectOnNextProjectResolution(t *testing.T) {
	ctx := context.Background()
	service, resolver := newInvalidationFixture(t)
	project42, project43 := int64(42), int64(43)

	initial := resolveInvalidationProject(t, resolver, project42)
	other := resolveInvalidationProject(t, resolver, project43)
	if len(initial.mcpConfig.MCPServers) != 0 {
		t.Fatalf("initial servers = %v", initial.mcpConfig.MCPServers)
	}

	created := createInvalidationMCP(t, service, "project", &project42, "alpha", "alpha-one")
	resolved := resolveInvalidationProject(t, resolver, project42)
	if resolved == initial {
		t.Fatal("project write reused the cached resolution")
	}
	if got := resolved.mcpConfig.MCPServers["alpha"].Command; got != "alpha-one" {
		t.Fatalf("created command = %q", got)
	}
	if again := resolveInvalidationProject(t, resolver, project43); again != other {
		t.Fatal("project-scope write invalidated another project")
	}

	command := "alpha-two"
	if _, err := service.UpdateMCPServer(ctx, created.ID, MCPServerUpdate{Config: json.RawMessage(`{"command":"` + command + `"}`)}); err != nil {
		t.Fatalf("UpdateMCPServer: %v", err)
	}
	resolved = resolveInvalidationProject(t, resolver, project42)
	if got := resolved.mcpConfig.MCPServers["alpha"].Command; got != command {
		t.Fatalf("updated command = %q", got)
	}

	disabled := false
	if _, err := service.UpdateMCPServer(ctx, created.ID, MCPServerUpdate{Enabled: &disabled}); err != nil {
		t.Fatalf("disable MCP: %v", err)
	}
	resolved = resolveInvalidationProject(t, resolver, project42)
	if _, ok := resolved.mcpConfig.MCPServers["alpha"]; ok {
		t.Fatal("disabled MCP server is still resolvable")
	}

	enabled := true
	if _, err := service.UpdateMCPServer(ctx, created.ID, MCPServerUpdate{Enabled: &enabled}); err != nil {
		t.Fatalf("enable MCP: %v", err)
	}
	if err := service.DeleteMCPServer(ctx, created.ID); err != nil {
		t.Fatalf("DeleteMCPServer: %v", err)
	}
	resolved = resolveInvalidationProject(t, resolver, project42)
	if _, ok := resolved.mcpConfig.MCPServers["alpha"]; ok {
		t.Fatal("deleted MCP server is still resolvable")
	}
}

func TestPlatformExtensionWritesInvalidateEveryProject(t *testing.T) {
	ctx := context.Background()
	service, resolver := newInvalidationFixture(t)
	project42 := int64(42)

	createInvalidationMCP(t, service, "project", &project42, "project-only", "project-command")
	projectSet := resolveInvalidationProject(t, resolver, project42)
	otherSet := resolveInvalidationProject(t, resolver, 43)

	createInvalidationMCP(t, service, "platform", nil, "shared", "platform-command")
	for _, projectID := range []int64{42, 43} {
		set := resolveInvalidationProject(t, resolver, projectID)
		if got := set.mcpConfig.MCPServers["shared"].Command; got != "platform-command" {
			t.Fatalf("project %d shared command = %q", projectID, got)
		}
	}
	resolved42 := resolveInvalidationProject(t, resolver, project42)
	if resolved42 == projectSet {
		t.Fatal("platform write did not invalidate project 42")
	}
	if _, ok := resolved42.mcpConfig.MCPServers["project-only"]; !ok {
		t.Fatal("platform invalidation dropped the project server")
	}
	if again := resolveInvalidationProject(t, resolver, 43); again == otherSet {
		t.Fatal("platform write did not invalidate project 43")
	}

	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "platform", Block: "code-review", MCPTools: []string{"mcp__shared__echo"},
	}); err != nil {
		t.Fatalf("platform binding: %v", err)
	}
	for _, projectID := range []int64{42, 43} {
		set := resolveInvalidationProject(t, resolver, projectID)
		if got := set.blocks["code-review"].MCPTools; len(got) != 1 || got[0] != "mcp__shared__echo" {
			t.Fatalf("project %d platform binding = %v", projectID, got)
		}
	}

	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "project", ProjectID: &project42, Block: "code-review", MCPTools: []string{"project-tool"},
	}); err != nil {
		t.Fatalf("project binding: %v", err)
	}
	if got := resolveInvalidationProject(t, resolver, 42).blocks["code-review"].MCPTools; len(got) != 1 || got[0] != "project-tool" {
		t.Fatalf("project binding after write = %v", got)
	}
	if got := resolveInvalidationProject(t, resolver, 43).blocks["code-review"].MCPTools; len(got) != 1 || got[0] != "mcp__shared__echo" {
		t.Fatalf("other project binding after write = %v", got)
	}
}

func TestSkillWritesTakeEffectOnNextProjectResolution(t *testing.T) {
	ctx := context.Background()
	service, resolver := newInvalidationFixture(t)
	project42 := int64(42)

	saved := registerInvalidationSkill(t, service, "project", &project42, "project-skill")
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "project", ProjectID: &project42, Block: "code-review", Skills: []string{"project-skill"},
	}); err != nil {
		t.Fatalf("binding: %v", err)
	}
	set := resolveInvalidationProject(t, resolver, project42)
	if set.loader == nil || !containsString(catalogueNames(set.loader), "project-skill") {
		t.Fatalf("uploaded skill not loaded: %v", set.loader)
	}
	bound, err := set.skills.BindSkills(ctx, set.blocks["code-review"].Skills)
	if err != nil {
		t.Fatalf("BindSkills: %v", err)
	}
	if len(bound.Skills) != 1 || bound.Skills[0].Name != "project-skill" {
		t.Fatalf("bound skills = %+v", bound.Skills)
	}

	if _, err := service.SetSkillEnabled(ctx, saved.ID, false); err != nil {
		t.Fatalf("SetSkillEnabled(false): %v", err)
	}
	set = resolveInvalidationProject(t, resolver, project42)
	if set.skills == nil {
		t.Fatal("disabled skill removed the provider")
	}
	bound, err = set.skills.BindSkills(ctx, set.blocks["code-review"].Skills)
	if err != nil {
		t.Fatalf("BindSkills after disable: %v", err)
	}
	if containedSkill(bound, "project-skill") {
		t.Fatal("disabled skill is still bindable")
	}

	if _, err := service.SetSkillEnabled(ctx, saved.ID, true); err != nil {
		t.Fatalf("SetSkillEnabled(true): %v", err)
	}
	if err := service.DeleteSkill(ctx, saved.ID); err != nil {
		t.Fatalf("DeleteSkill: %v", err)
	}
	set = resolveInvalidationProject(t, resolver, project42)
	if set.loader != nil && containsString(catalogueNames(set.loader), "project-skill") {
		t.Fatal("deleted skill is still loaded")
	}
}

func TestPlatformSkillWriteInvalidatesEveryProject(t *testing.T) {
	service, resolver := newInvalidationFixture(t)
	saved := registerInvalidationSkill(t, service, "platform", nil, "platform-skill")
	for _, projectID := range []int64{42, 43} {
		set := resolveInvalidationProject(t, resolver, projectID)
		if set.loader == nil || !containsString(catalogueNames(set.loader), "platform-skill") {
			t.Fatalf("project %d did not load the platform skill", projectID)
		}
	}
	if err := service.DeleteSkill(context.Background(), saved.ID); err != nil {
		t.Fatalf("DeleteSkill: %v", err)
	}
	for _, projectID := range []int64{42, 43} {
		set := resolveInvalidationProject(t, resolver, projectID)
		if set.loader != nil && containsString(catalogueNames(set.loader), "platform-skill") {
			t.Fatalf("project %d still loads the deleted platform skill", projectID)
		}
	}
}

func TestFailedExtensionWriteDoesNotInvalidate(t *testing.T) {
	st := openTestStore(t)
	seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
	var projectCalls, allCalls atomic.Int32
	service, err := NewExtensionService(ExtensionServiceConfig{
		Store:     st,
		Workspace: workspace.NewManager(t.TempDir(), nil),
		InvalidateProject: func(int64) {
			projectCalls.Add(1)
		},
		InvalidateAll: func() {
			allCalls.Add(1)
		},
	})
	if err != nil {
		t.Fatalf("NewExtensionService: %v", err)
	}
	ctx := context.Background()
	missing := int64(999)
	operations := []struct {
		name string
		run  func() error
	}{
		{"invalid MCP config", func() error {
			_, err := service.CreateMCPServer(ctx, MCPServerInput{
				Scope: "project", ProjectID: &missing, Name: "gamma", Enabled: true,
				Config: json.RawMessage(`{"command":""}`),
			})
			return err
		}},
		{"missing project scope", func() error {
			_, err := service.CreateMCPServer(ctx, MCPServerInput{
				Scope: "project", ProjectID: &missing, Name: "gamma", Enabled: true,
				Config: json.RawMessage(`{"command":"gamma"}`),
			})
			return err
		}},
		{"unknown MCP server", func() error {
			_, err := service.UpdateMCPServer(ctx, "missing", MCPServerUpdate{})
			return err
		}},
		{"invalid skill archive", func() error {
			_, err := service.UploadSkill(ctx, SkillUpload{Scope: "platform", Data: []byte("not a zip")})
			return err
		}},
		{"missing binding project", func() error {
			_, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
				Scope: "project", ProjectID: &missing, Block: "code-review",
			})
			return err
		}},
	}
	for _, operation := range operations {
		if err := operation.run(); err == nil {
			t.Fatalf("%s: expected an error", operation.name)
		}
	}
	if projectCalls.Load() != 0 || allCalls.Load() != 0 {
		t.Fatalf("failed writes invalidated: project=%d all=%d", projectCalls.Load(), allCalls.Load())
	}

	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "project", ProjectID: func() *int64 { id := int64(42); return &id }(), Name: "alpha", Enabled: true,
		Config: json.RawMessage(`{"command":"alpha"}`),
	}); err != nil {
		t.Fatalf("project write: %v", err)
	}
	if projectCalls.Load() != 1 || allCalls.Load() != 0 {
		t.Fatalf("project write callbacks = %d/%d", projectCalls.Load(), allCalls.Load())
	}
	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "shared", Enabled: true,
		Config: json.RawMessage(`{"command":"shared"}`),
	}); err != nil {
		t.Fatalf("platform write: %v", err)
	}
	if projectCalls.Load() != 1 || allCalls.Load() != 1 {
		t.Fatalf("platform write callbacks = %d/%d", projectCalls.Load(), allCalls.Load())
	}
}

func TestProjectScopeWriteFallsBackToAllInvalidation(t *testing.T) {
	st := openTestStore(t)
	seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
	var allCalls atomic.Int32
	service, err := NewExtensionService(ExtensionServiceConfig{
		Store:     st,
		Workspace: workspace.NewManager(t.TempDir(), nil),
		InvalidateAll: func() {
			allCalls.Add(1)
		},
	})
	if err != nil {
		t.Fatalf("NewExtensionService: %v", err)
	}
	project42 := int64(42)
	if _, err := service.CreateMCPServer(context.Background(), MCPServerInput{
		Scope: "project", ProjectID: &project42, Name: "alpha", Enabled: true,
		Config: json.RawMessage(`{"command":"alpha"}`),
	}); err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}
	if allCalls.Load() != 1 {
		t.Fatalf("all-invalidation calls = %d, want 1", allCalls.Load())
	}
}

func TestProjectExtensionResolverDiscardsBuildInvalidatedInFlight(t *testing.T) {
	for _, invalidate := range []struct {
		name string
		run  func(*ProjectExtensionResolver)
	}{
		{"project", func(r *ProjectExtensionResolver) { r.InvalidateProject(42) }},
		{"all", func(r *ProjectExtensionResolver) { r.InvalidateAll() }},
	} {
		t.Run(invalidate.name, func(t *testing.T) {
			st := openTestStore(t)
			seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
			resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{
				Store:     st,
				Workspace: t.TempDir(),
			})
			if err != nil {
				t.Fatalf("resolver: %v", err)
			}
			t.Cleanup(func() { _ = resolver.Close() })

			stale, fresh := &projectExtensionSet{}, &projectExtensionSet{}
			started, release := make(chan struct{}), make(chan struct{})
			var builds atomic.Int32
			resolver.buildSet = func(context.Context, int64) (*projectExtensionSet, error) {
				if builds.Add(1) == 1 {
					close(started)
					<-release
					return stale, nil
				}
				return fresh, nil
			}

			result := make(chan *projectExtensionSet, 1)
			errorsSeen := make(chan error, 1)
			go func() {
				set, err := resolver.ForProject(context.Background(), 42)
				errorsSeen <- err
				result <- set
			}()
			<-started
			invalidate.run(resolver)
			close(release)

			if err := <-errorsSeen; err != nil {
				t.Fatalf("ForProject: %v", err)
			}
			if got := <-result; got != fresh {
				t.Fatal("a build invalidated in flight was returned or cached")
			}
			cached, err := resolver.ForProject(context.Background(), 42)
			if err != nil {
				t.Fatalf("second ForProject: %v", err)
			}
			if cached != fresh || builds.Load() != 2 {
				t.Fatalf("cache = %p builds = %d, want the fresh set and 2 builds", cached, builds.Load())
			}
		})
	}
}

func TestInvalidationDoesNotBlockWorkspaceReadersAndClosesLeasedSetOnRelease(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
	manager := workspace.NewManager(t.TempDir(), nil)
	resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{
		Store:     st,
		Workspace: manager.Root(),
	})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })

	set, err := resolver.ForProject(ctx, 42)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	t.Cleanup(func() { _ = set.mcp.Close() })
	closer := &lifecycleCloser{entered: make(chan struct{})}
	set.closer = closer

	// A review holds the project workspace read lock while it uses the set.
	ws := manager.Get(42)
	unlock, err := ws.RLock(ctx)
	if err != nil {
		t.Fatalf("RLock: %v", err)
	}

	invalidated := make(chan struct{})
	go func() {
		resolver.InvalidateProject(42)
		close(invalidated)
	}()
	select {
	case <-invalidated:
	case <-time.After(2 * time.Second):
		t.Fatal("InvalidateProject blocked on the workspace lock")
	}

	// Writer preference must not turn the invalidation into a queued writer
	// that blocks new readers.
	readAcquired := make(chan struct{})
	go func() {
		unlockReader, err := ws.RLock(ctx)
		if err != nil {
			t.Errorf("second RLock: %v", err)
			return
		}
		close(readAcquired)
		unlockReader()
	}()
	select {
	case <-readAcquired:
	case <-time.After(2 * time.Second):
		t.Fatal("workspace reader blocked after invalidation")
	}

	// The leased set stays open; a new resolution builds a fresh set.
	if next := resolveInvalidationProject(t, resolver, 42); next == set {
		t.Fatal("invalidated set was returned again")
	}
	select {
	case <-closer.entered:
		t.Fatal("leased set closed while a holder still uses it")
	default:
	}

	unlock()
	set.release()
	select {
	case <-closer.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("retired set was not closed after the last release")
	}
	if calls := closer.calls.Load(); calls != 1 {
		t.Fatalf("retired close calls = %d", calls)
	}
}

func TestProjectExtensionResolverConcurrentResolveAndInvalidate(t *testing.T) {
	service, resolver := newInvalidationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	project42 := int64(42)
	server := createInvalidationMCP(t, service, "project", &project42, "alpha", "v0")

	done := make(chan struct{})
	var wg sync.WaitGroup
	for reader := 0; reader < 3; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				set, err := resolver.ForProject(ctx, project42)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					t.Errorf("ForProject: %v", err)
					return
				}
				_ = set.mcpConfig.MCPServers["alpha"].Command
				_ = set.blocks["code-review"]
				set.release()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for index := 0; index < 25; index++ {
			command := "v" + strings.Repeat("x", index%3)
			if _, err := service.UpdateMCPServer(context.Background(), server.ID,
				MCPServerUpdate{Config: json.RawMessage(`{"command":"` + command + `"}`)}); err != nil {
				t.Errorf("UpdateMCPServer: %v", err)
				return
			}
			enabled := index%2 == 0
			if _, err := service.UpdateMCPServer(context.Background(), server.ID,
				MCPServerUpdate{Enabled: &enabled}); err != nil {
				t.Errorf("toggle MCP: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	enabled := true
	if _, err := service.UpdateMCPServer(context.Background(), server.ID,
		MCPServerUpdate{Enabled: &enabled, Config: json.RawMessage(`{"command":"final"}`)}); err != nil {
		t.Fatalf("final update: %v", err)
	}
	set := resolveInvalidationProject(t, resolver, 42)
	if got := set.mcpConfig.MCPServers["alpha"].Command; got != "final" {
		t.Fatalf("final command = %q", got)
	}
}

func TestRestoreDefaultsInvalidatesEveryProject(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
	seedProject(t, st, newTestCipher(t), 43, "whsec", "{}", "default")
	manager := workspace.NewManager(t.TempDir(), nil)
	resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{
		Store:     st,
		Workspace: manager.Root(),
	})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })
	service, err := NewExtensionService(ExtensionServiceConfig{
		Store:             st,
		Workspace:         manager,
		Platform:          restoreConfig(defaultCodeReview, nil),
		InvalidateProject: resolver.InvalidateProject,
		InvalidateAll:     resolver.InvalidateAll,
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	custom := []string{"mcp__team__lookup"}
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "platform", Block: "review", MCPTools: custom,
	}); err != nil {
		t.Fatalf("custom binding: %v", err)
	}
	for _, projectID := range []int64{42, 43} {
		if got := resolveInvalidationProject(t, resolver, projectID).blocks["review"].Skills; len(got) != 0 {
			t.Fatalf("project %d pre-restore skills = %v", projectID, got)
		}
	}

	preview, err := service.RestoreDefaults(ctx, RestoreDefaultsRequest{
		Blocks: []string{"review"}, DryRun: true,
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.RestartRequired {
		t.Fatal("preview requires a restart despite wired invalidation")
	}
	result, err := service.RestoreDefaults(ctx, RestoreDefaultsRequest{
		Blocks: []string{"review"}, PreviewToken: preview.PreviewToken,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if result.RestartRequired {
		t.Fatal("restore requires a restart despite wired invalidation")
	}
	for _, projectID := range []int64{42, 43} {
		binding := resolveInvalidationProject(t, resolver, projectID).blocks["review"]
		if len(binding.Skills) != 1 || binding.Skills[0] != "code-review" {
			t.Fatalf("project %d restored skills = %v", projectID, binding.Skills)
		}
		if len(binding.MCPTools) != 1 || binding.MCPTools[0] != custom[0] {
			t.Fatalf("project %d restored custom tools = %v", projectID, binding.MCPTools)
		}
	}
}

func containedSkill(binding review.SkillBinding, name string) bool {
	for _, skill := range binding.Skills {
		if skill.Name == name {
			return true
		}
	}
	return false
}
