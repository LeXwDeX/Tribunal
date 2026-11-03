package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

func TestBuiltinExtensionsCannotBeDisabled(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, graphDefault(analysisArgs))
	skills, err := st.ListSkills(ctx, store.ScopePlatform, nil)
	if err != nil || len(skills) != 1 {
		t.Fatalf("skills = %+v, %v", skills, err)
	}
	server := platformMCPServer(t, st, "codebase-memory")
	var appErr *Error
	if _, err := service.SetSkillEnabled(ctx, skills[0].ID, false); !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Errorf("disable builtin skill = %v, want conflict", err)
	}
	disabled := false
	if _, err := service.UpdateMCPServer(ctx, server.ID, MCPServerUpdate{Enabled: &disabled}); !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Errorf("disable builtin MCP = %v, want conflict", err)
	}
	afterSkill, err := st.GetSkill(ctx, skills[0].ID)
	if err != nil || !reflect.DeepEqual(*afterSkill, skills[0]) {
		t.Errorf("skill changed after rejection: %+v, %v", afterSkill, err)
	}
	afterServer := platformMCPServer(t, st, "codebase-memory")
	if !afterServer.Enabled || !afterServer.UpdatedAt.Equal(server.UpdatedAt) {
		t.Errorf("MCP changed after rejection: %+v", afterServer)
	}
	if err := st.SetSkillEnabled(ctx, skills[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateMCPServer(ctx, server.ID, store.MCPServerUpdate{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	enabled := true
	if got, err := service.SetSkillEnabled(ctx, skills[0].ID, true); err != nil || !got.Enabled {
		t.Errorf("enable builtin skill = %+v, %v", got, err)
	}
	if got, err := service.UpdateMCPServer(ctx, server.ID, MCPServerUpdate{Enabled: &enabled}); err != nil || !got.Enabled {
		t.Errorf("enable builtin MCP = %+v, %v", got, err)
	}
	customSkill, err := st.CreateSkill(ctx, store.Skill{Scope: store.ScopePlatform, Name: "custom", Description: "custom skill", Dir: t.TempDir(), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	customServer, err := st.CreateMCPServer(ctx, store.MCPServer{Scope: store.ScopePlatform, Name: "custom", Config: json.RawMessage(`{"command":"npx"}`), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := service.SetSkillEnabled(ctx, customSkill.ID, false); err != nil || got.Enabled {
		t.Errorf("disable custom skill = %+v, %v", got, err)
	}
	if got, err := service.UpdateMCPServer(ctx, customServer.ID, MCPServerUpdate{Enabled: &disabled}); err != nil || got.Enabled {
		t.Errorf("disable custom MCP = %+v, %v", got, err)
	}
}

func TestBuiltinEnableRecoveryPersistsAcrossStoreReopen(t *testing.T) {
	ctx := context.Background()
	st, path := openTestStorePath(t)
	service, err := NewExtensionService(ExtensionServiceConfig{Store: st, Workspace: workspace.NewManager(t.TempDir(), nil)})
	if err != nil {
		t.Fatal(err)
	}
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, graphDefault(analysisArgs))
	skills, err := st.ListSkills(ctx, store.ScopePlatform, nil)
	if err != nil || len(skills) != 1 {
		t.Fatalf("skills = %+v, %v", skills, err)
	}
	server := platformMCPServer(t, st, "codebase-memory")
	if err := st.SetSkillEnabled(ctx, skills[0].ID, false); err != nil {
		t.Fatal(err)
	}
	disabled := false
	if _, err := st.UpdateMCPServer(ctx, server.ID, store.MCPServerUpdate{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, graphDefault(analysisArgs))
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.ConfigureMCPSecrets(ctx, newTestCipher(t)); err != nil {
		t.Fatal(err)
	}
	gotSkill, err := reopened.GetSkill(ctx, skills[0].ID)
	if err != nil || !gotSkill.Enabled {
		t.Errorf("reopened skill = %+v, %v", gotSkill, err)
	}
	gotServer, err := reopened.GetMCPServer(ctx, server.ID)
	if err != nil || !gotServer.Enabled {
		t.Errorf("reopened MCP = %+v, %v", gotServer, err)
	}
}

func TestBuiltinEnableRecoveryFailureStopsSync(t *testing.T) {
	ctx := context.Background()
	st, path := openTestStorePath(t)
	service, err := NewExtensionService(ExtensionServiceConfig{Store: st, Workspace: workspace.NewManager(t.TempDir(), nil)})
	if err != nil {
		t.Fatal(err)
	}
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	applyMCPDefaults(t, service, st, nil, mcp.Config{}, graphDefault(analysisArgs))
	skills, err := st.ListSkills(ctx, store.ScopePlatform, nil)
	if err != nil || len(skills) != 1 {
		t.Fatalf("skills = %+v, %v", skills, err)
	}
	server := platformMCPServer(t, st, "codebase-memory")
	writeSkill(t, skills[0].Dir, "code-review", "Old description")
	oldSkill, err := os.ReadFile(filepath.Join(skills[0].Dir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSkillEnabled(ctx, skills[0].ID, false); err != nil {
		t.Fatal(err)
	}
	disabled := false
	if _, err := st.UpdateMCPServer(ctx, server.ID, store.MCPServerUpdate{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, trigger := range []string{
		`CREATE TRIGGER fail_skill_enable BEFORE UPDATE OF enabled ON skills WHEN NEW.enabled = 1 BEGIN SELECT RAISE(ABORT, 'injected'); END`,
		`CREATE TRIGGER fail_mcp_enable BEFORE UPDATE OF enabled ON mcp_servers WHEN NEW.enabled = 1 BEGIN SELECT RAISE(ABORT, 'injected'); END`,
	} {
		if _, err := raw.ExecContext(ctx, trigger); err != nil {
			t.Fatal(err)
		}
	}
	skillDefaults, err := service.SkillDefaults(defaultCodeReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := skillDefaults[0].Sync(ctx); err == nil {
		t.Error("skill sync swallowed enable failure")
	}
	gotSkill, err := os.ReadFile(filepath.Join(skills[0].Dir, "SKILL.md"))
	if err != nil || !bytes.Equal(gotSkill, oldSkill) {
		t.Errorf("skill refreshed despite failed enable: %v", err)
	}
	mcpDefaults, err := service.MCPDefaults(graphDefault([]string{"--new-flag"}), mcp.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mcpDefaults[0].Sync(ctx); err == nil {
		t.Error("MCP sync swallowed enable failure")
	}
	gotServer := platformMCPServer(t, st, "codebase-memory")
	if gotServer.Enabled || !bytes.Equal(gotServer.Config, server.Config) {
		t.Errorf("MCP changed despite failed enable: %+v", gotServer)
	}
}
