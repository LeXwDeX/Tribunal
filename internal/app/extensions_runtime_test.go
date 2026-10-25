package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/mcp"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/review"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/skill"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func TestUploadSkillExtractsAndIsLoadable(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	projectID := int64(42)

	uploaded, err := service.UploadSkill(ctx, SkillUpload{
		Scope:     "project",
		ProjectID: &projectID,
		Data: testSkillArchiveFiles(t, map[string]string{
			"skill/SKILL.md":     "---\nname: go-review\ndescription: Review Go changes\n---\n",
			"skill/references/a": "reference",
		}),
	})
	if err != nil {
		t.Fatalf("UploadSkill: %v", err)
	}
	if uploaded.Name != "go-review" {
		t.Fatalf("name = %q", uploaded.Name)
	}
	if _, err := os.Stat(filepath.Join(uploaded.Dir, "SKILL.md")); err != nil {
		t.Fatalf("extracted manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(uploaded.Dir, "references", "a")); err != nil {
		t.Fatalf("extracted resource: %v", err)
	}
	if !strings.HasPrefix(uploaded.Dir, filepath.Join(service.workspace, "skills")) {
		t.Fatalf("dir = %q is outside the managed root", uploaded.Dir)
	}
	loader, err := skill.New(skill.Options{PlatformDirs: []string{filepath.Dir(uploaded.Dir)}})
	if err != nil {
		t.Fatalf("skill.New: %v", err)
	}
	names := catalogueNames(loader)
	if !containsString(names, "go-review") {
		t.Fatalf("catalogue = %v", names)
	}

	if err := service.DeleteSkill(ctx, uploaded.ID); err != nil {
		t.Fatalf("DeleteSkill: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(uploaded.Dir)); !os.IsNotExist(err) {
		t.Fatalf("managed directory still exists: %v", err)
	}
}

func TestUploadSkillRejectsUnsafeArchives(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)

	cases := []struct {
		name string
		data []byte
	}{
		{name: "traversal", data: testSkillArchiveFiles(t, map[string]string{
			"skill/SKILL.md":         testSkillBody,
			"skill/../../../evil.go": "package evil",
		})},
		{name: "missing manifest", data: testSkillArchiveFiles(t, map[string]string{
			"skill/README.md": "# nope",
		})},
		{name: "symlink", data: symlinkArchive(t)},
		{name: "oversize", data: oversizeArchive(t)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.UploadSkill(ctx, SkillUpload{Scope: "platform", Data: tc.data}); err == nil {
				t.Fatal("unsafe archive was accepted")
			}
			if _, err := os.Stat(filepath.Join(service.workspace, "evil.go")); err == nil {
				t.Fatal("traversal wrote outside the managed root")
			}
		})
	}
}

func symlinkArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(testSkillBody)); err != nil {
		t.Fatal(err)
	}
	header := &zip.FileHeader{Name: "link.md"}
	header.SetMode(os.ModeSymlink | 0o777)
	link, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := link.Write([]byte("SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// oversizeArchive stays well below the compressed limit while expanding past
// the extracted total limit: five 4 MiB files of zeros.
func oversizeArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	if entry, err := writer.Create("SKILL.md"); err != nil {
		t.Fatal(err)
	} else if _, err := entry.Write([]byte(testSkillBody)); err != nil {
		t.Fatal(err)
	}
	zeros := make([]byte, 4<<20)
	for i := 0; i < 5; i++ {
		entry, err := writer.Create(fmt.Sprintf("skill/blob-%d.bin", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(zeros); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestRegisterSkillDirCopiesIntoManagedRoot(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"),
		[]byte("---\nname: security\ndescription: Security review\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "references"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "references", "check.md"), []byte("check"), 0o600); err != nil {
		t.Fatal(err)
	}
	registered, err := service.RegisterSkillDir(ctx, SkillDirInput{Scope: "platform", Dir: source})
	if err != nil {
		t.Fatalf("RegisterSkillDir: %v", err)
	}
	if registered.Dir == source || !strings.HasPrefix(registered.Dir, filepath.Join(service.workspace, "skills")) {
		t.Fatalf("dir = %q", registered.Dir)
	}
	if _, err := os.Stat(filepath.Join(registered.Dir, "references", "check.md")); err != nil {
		t.Fatalf("copied resource: %v", err)
	}
}

func TestProjectExtensionResolverMergesOverrides(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	projectID := int64(42)
	project := int64(42)

	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "alpha", Enabled: true,
		Config: json.RawMessage(`{"command":"platform-alpha"}`),
	}); err != nil {
		t.Fatalf("platform MCP: %v", err)
	}
	// gamma exists only in the database; the file configuration does not
	// mention it, so the project manager must receive the merged platform
	// configuration rather than the file snapshot.
	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "gamma", Enabled: true,
		Config: json.RawMessage(`{"command":"platform-gamma"}`),
	}); err != nil {
		t.Fatalf("platform MCP gamma: %v", err)
	}
	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "project", ProjectID: &project, Name: "alpha", Enabled: true,
		Config: json.RawMessage(`{"command":"project-alpha"}`),
	}); err != nil {
		t.Fatalf("project MCP: %v", err)
	}
	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "project", ProjectID: &project, Name: "beta", Enabled: true,
		Config: json.RawMessage(`{"command":"project-beta"}`),
	}); err != nil {
		t.Fatalf("project MCP beta: %v", err)
	}
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "platform", Block: "code-review",
		MCPTools: []string{"platform-tool"}, Skills: []string{"platform-skill"},
	}); err != nil {
		t.Fatalf("platform binding: %v", err)
	}
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "project", ProjectID: &project, Block: "code-review",
		MCPTools: []string{"project-tool"}, Skills: []string{"project-skill"},
	}); err != nil {
		t.Fatalf("project binding: %v", err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"),
		[]byte("---\nname: security\ndescription: Security review\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterSkillDir(ctx, SkillDirInput{Scope: "project", ProjectID: &project, Dir: source}); err != nil {
		t.Fatalf("project skill: %v", err)
	}

	platformBlocks := map[string]config.BlockBinding{
		"code-review": {MCPTools: []string{"platform-tool"}, Skills: []string{"platform-skill"}},
	}
	resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{
		Store:      service.store,
		Blocks:     platformBlocks,
		MCP:        mcpFileConfig(),
		Workspace:  service.workspace,
		BaseSkills: NewSkillProvider(mustLoader(t)),
	})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })

	set, err := resolver.ForProject(ctx, projectID)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	binding := set.blocks["code-review"]
	if len(binding.MCPTools) != 1 || binding.MCPTools[0] != "project-tool" {
		t.Fatalf("project tools = %v", binding.MCPTools)
	}
	if len(binding.Skills) != 1 || binding.Skills[0] != "project-skill" {
		t.Fatalf("project skills = %v", binding.Skills)
	}
	if set.mcp == nil || set.loader == nil {
		t.Fatalf("project runtime not built: mcp=%v loader=%v", set.mcp, set.loader)
	}
	if servers, want := strings.Join(set.mcp.Servers(), ","), "alpha,beta,gamma,shared"; servers != want {
		t.Fatalf("servers = %q, want %q", servers, want)
	}
	if got := set.mcpConfig.MCPServers["alpha"].Command; got != "project-alpha" {
		t.Fatalf("alpha command = %q, want project override", got)
	}
	if got := set.mcpConfig.MCPServers["gamma"].Command; got != "platform-gamma" {
		t.Fatalf("gamma command = %q, want platform database server", got)
	}
	if names := catalogueNames(set.loader); !containsString(names, "security") {
		t.Fatalf("project skill not loaded: %v", names)
	}
	provider, ok := set.tools.(*mcpToolProvider)
	if !ok || provider.blocks["code-review"].MCPTools[0] != "project-tool" {
		t.Fatalf("tool provider does not carry project bindings: %#v", set.tools)
	}
	if _, err := review.NewExecutor(review.ExecutorDeps{
		Profile: review.Profile{Name: "default", Config: testReviewProfile()},
		Blocks:  set.blocks,
		Tools:   set.tools,
		Skills:  set.skills,
	}); err != nil {
		t.Fatalf("executor deps rejected project bindings: %v", err)
	}

	platformSet, err := resolver.ForProject(ctx, 43)
	if err != nil {
		t.Fatalf("ForProject platform-only: %v", err)
	}
	if platformSet.mcp == nil {
		t.Fatal("platform-only project did not rebuild an MCP manager")
	}
	if platformSet.loader != nil {
		t.Fatal("platform-only project built a project skill loader")
	}
	if got := strings.Join(platformSet.mcp.Servers(), ","); got != "alpha,gamma,shared" {
		t.Fatalf("platform-only servers = %q", got)
	}
	if got := platformSet.blocks["code-review"]; len(got.MCPTools) != 1 || got.MCPTools[0] != "platform-tool" {
		t.Fatalf("platform-only blocks = %+v", got)
	}
}

func TestProjectExtensionResolverAppliesMergedBindingsWithoutProjectServers(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	httpServer := newHTTPMCPTestServer(t)
	project42 := int64(42)
	project43 := int64(43)

	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "remote", Enabled: true,
		Config: json.RawMessage(`{"url":"` + httpServer.URL + `"}`),
	}); err != nil {
		t.Fatalf("platform MCP: %v", err)
	}
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "project", ProjectID: &project42, Block: "code-review",
	}); err != nil {
		t.Fatalf("narrowed binding: %v", err)
	}
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "project", ProjectID: &project43, Block: "code-review",
		MCPTools: []string{"mcp__remote__echo"},
	}); err != nil {
		t.Fatalf("widened binding: %v", err)
	}

	resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{
		Store: service.store,
		Blocks: map[string]config.BlockBinding{
			"code-review": {MCPTools: []string{"mcp__remote__echo"}},
		},
		Workspace: service.workspace,
	})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })

	narrowed, err := resolver.ForProject(ctx, project42)
	if err != nil {
		t.Fatalf("ForProject narrowed: %v", err)
	}
	if narrowed.mcp == nil {
		t.Fatal("empty project binding skipped the MCP manager rebuild")
	}
	if got := narrowed.mcpConfig.MCPServers["remote"].URL; got != httpServer.URL {
		t.Fatalf("database-only platform server missing: %q", got)
	}
	if tools, err := narrowed.tools.Tools(ctx, "code-review"); err != nil || len(tools) != 0 {
		t.Fatalf("empty project binding did not revoke platform tools: %v, %v", tools, err)
	}

	widened, err := resolver.ForProject(ctx, project43)
	if err != nil {
		t.Fatalf("ForProject widened: %v", err)
	}
	if widened.mcp == nil {
		t.Fatal("widened project binding skipped the MCP manager rebuild")
	}
	tools, err := widened.tools.Tools(ctx, "code-review")
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Def().Name != "mcp__remote__echo" {
		t.Fatalf("widened tools = %+v", tools)
	}
	if got := strings.Join(widened.mcp.Servers(), ","); got != "remote" {
		t.Fatalf("widened servers = %q", got)
	}
}

func TestProjectExtensionResolverExpandsStoredMCPServerEnv(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	t.Setenv("MCP_STORED_VALUE", "expanded-value")
	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "stored", Enabled: true,
		Config: json.RawMessage(`{"command":"echo","env":{"VALUE":"${MCP_STORED_VALUE}"}}`),
	}); err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}

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
	if got := set.mcpConfig.MCPServers["stored"].Env["VALUE"]; got != "expanded-value" {
		t.Fatalf("stored env = %q, want expanded value", got)
	}
}

func TestProjectExtensionResolverFailsClosedOnStoredMCPServer(t *testing.T) {
	cases := []struct {
		name      string
		config    json.RawMessage
		env       map[string]string
		denied    []string
		wantErr   error
		wantText  string
		forbidden string
	}{
		{
			name:      "denied template",
			config:    json.RawMessage(`{"command":"echo","env":{"CUSTOM":"${CUSTOM_DENIED_FLAG}"}}`),
			env:       map[string]string{"CUSTOM_DENIED_FLAG": "super-secret-value"},
			denied:    []string{"CUSTOM_DENIED_FLAG"},
			wantErr:   mcp.ErrSensitiveEnv,
			forbidden: "super-secret-value",
		},
		{
			name:     "missing environment",
			config:   json.RawMessage(`{"command":"echo","env":{"CUSTOM":"${MCP_TEST_MISSING_NEVER_SET}"}}`),
			wantErr:  mcp.ErrInvalidConfig,
			wantText: "environment variable MCP_TEST_MISSING_NEVER_SET is not set",
		},
		{
			name:      "unknown field",
			config:    json.RawMessage(`{"command":"echo","bogus":"super-secret-value"}`),
			wantErr:   mcp.ErrInvalidConfig,
			forbidden: "super-secret-value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, _ := newExtensionFixture(t)
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			if _, err := service.store.CreateMCPServer(ctx, store.MCPServer{
				Scope: store.ScopePlatform, Name: "stored", Config: tc.config, Enabled: true,
			}); err != nil {
				t.Fatalf("store server: %v", err)
			}
			resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{
				Store:     service.store,
				Workspace: service.workspace,
				DeniedEnv: tc.denied,
			})
			if err != nil {
				t.Fatalf("resolver: %v", err)
			}
			t.Cleanup(func() { _ = resolver.Close() })

			_, err = resolver.ForProject(ctx, 42)
			if err == nil {
				t.Fatal("invalid stored MCP server did not fail closed")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error = %v, want %q", err, tc.wantText)
			}
			if tc.forbidden != "" && strings.Contains(err.Error(), tc.forbidden) {
				t.Fatalf("error leaked a config or environment value: %v", err)
			}
		})
	}
}

func mcpFileConfig() mcp.Config {
	return mcp.Config{MCPServers: map[string]mcp.ServerConfig{
		"shared": {Command: "platform-shared"},
	}}
}

func mustLoader(t *testing.T) *skill.Loader {
	t.Helper()
	loader, err := skill.New(skill.Options{})
	if err != nil {
		t.Fatalf("skill.New: %v", err)
	}
	return loader
}

func catalogueNames(loader *skill.Loader) []string {
	names := make([]string, 0)
	for _, item := range loader.Catalogue() {
		names = append(names, item.Name)
	}
	return names
}

func TestLoadStoredPlatformExtensionsExpandsStoredMCPEnv(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	t.Setenv("MCP_PLATFORM_STORED_VALUE", "expanded-platform-value")
	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "stored", Enabled: true,
		Config: json.RawMessage(`{"command":"stored","env":{"VALUE":"${MCP_PLATFORM_STORED_VALUE}"}}`),
	}); err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}

	_, mcpConfig, _, err := loadStoredPlatformExtensions(ctx, st, nil, mcp.Config{}, nil)
	if err != nil {
		t.Fatalf("loadStoredPlatformExtensions: %v", err)
	}
	if got := mcpConfig.MCPServers["stored"].Env["VALUE"]; got != "expanded-platform-value" {
		t.Fatalf("stored env = %q, want expanded value", got)
	}
}

func TestLoadStoredPlatformExtensionsFailsClosedOnStoredMCP(t *testing.T) {
	cases := []struct {
		name      string
		config    json.RawMessage
		env       map[string]string
		denied    []string
		wantErr   error
		wantText  string
		forbidden string
	}{
		{
			name:      "denied environment",
			config:    json.RawMessage(`{"command":"stored","env":{"CUSTOM":"${CUSTOM_DENIED_FLAG}"}}`),
			env:       map[string]string{"CUSTOM_DENIED_FLAG": "super-secret-value"},
			denied:    []string{"CUSTOM_DENIED_FLAG"},
			wantErr:   mcp.ErrSensitiveEnv,
			forbidden: "super-secret-value",
		},
		{
			name:     "missing environment",
			config:   json.RawMessage(`{"command":"stored","env":{"VALUE":"${MCP_PLATFORM_MISSING_NEVER_SET}"}}`),
			wantErr:  mcp.ErrInvalidConfig,
			wantText: "environment variable MCP_PLATFORM_MISSING_NEVER_SET is not set",
		},
		{
			name:      "unknown field",
			config:    json.RawMessage(`{"command":"stored","bogus":"super-secret-value"}`),
			wantErr:   mcp.ErrInvalidConfig,
			forbidden: "super-secret-value",
		},
		{
			name:      "sensitive reference",
			config:    json.RawMessage(`{"command":"stored","env":{"VALUE":"${OPENAI_API_KEY}"}}`),
			env:       map[string]string{"OPENAI_API_KEY": "super-secret-value"},
			wantErr:   mcp.ErrSensitiveEnv,
			forbidden: "super-secret-value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := openTestStore(t)
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			if _, err := st.CreateMCPServer(ctx, store.MCPServer{
				Scope: store.ScopePlatform, Name: "stored", Config: tc.config, Enabled: true,
			}); err != nil {
				t.Fatalf("store server: %v", err)
			}
			_, _, _, err := loadStoredPlatformExtensions(ctx, st, nil, mcp.Config{}, nil, tc.denied...)
			if err == nil {
				t.Fatal("invalid stored MCP server did not fail closed")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error = %v, want %q", err, tc.wantText)
			}
			if tc.forbidden != "" && strings.Contains(err.Error(), tc.forbidden) {
				t.Fatalf("error leaked a config or environment value: %v", err)
			}
		})
	}
}

func TestLoadStoredPlatformExtensions(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"),
		[]byte("---\nname: platform-skill\ndescription: Platform skill\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterSkillDir(ctx, SkillDirInput{Scope: "platform", Dir: source}); err != nil {
		t.Fatalf("platform skill: %v", err)
	}
	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "stored", Enabled: true,
		Config: json.RawMessage(`{"command":"stored-server"}`),
	}); err != nil {
		t.Fatalf("platform MCP: %v", err)
	}
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "platform", Block: "code-review", MCPTools: []string{"stored-tool"},
	}); err != nil {
		t.Fatalf("platform binding: %v", err)
	}

	blocks, mcpConfig, dirs, err := loadStoredPlatformExtensions(ctx, st, nil, mcp.Config{}, nil)
	if err != nil {
		t.Fatalf("loadStoredPlatformExtensions: %v", err)
	}
	if got := blocks["code-review"].MCPTools; len(got) != 1 || got[0] != "stored-tool" {
		t.Fatalf("blocks = %+v", blocks)
	}
	if got := mcpConfig.MCPServers["stored"].Command; got != "stored-server" {
		t.Fatalf("mcp config = %+v", mcpConfig)
	}
	loader, err := skill.New(skill.Options{PlatformDirs: dirs})
	if err != nil {
		t.Fatalf("skill.New: %v", err)
	}
	if names := catalogueNames(loader); !containsString(names, "platform-skill") {
		t.Fatalf("platform skill not loaded: %v", names)
	}
}
