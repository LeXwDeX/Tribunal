package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func TestValidateExtensionScope(t *testing.T) {
	id := int64(7)
	cases := []struct {
		name     string
		scope    string
		project  *int64
		required bool
		ok       bool
	}{
		{name: "all scopes", ok: true},
		{name: "required scope", required: true},
		{name: "platform", scope: "platform", ok: true},
		{name: "platform with project", scope: "platform", project: &id},
		{name: "project", scope: "project", project: &id, required: true, ok: true},
		{name: "project without id", scope: "project", required: true},
		{name: "unknown scope", scope: "global", required: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := store.ValidateExtensionScope(tc.scope, tc.project, tc.required)
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestMCPServerCRUDRoundtrip(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)
	mustProject(t, s, 43)
	projectID := int64(42)

	platform, err := s.CreateMCPServer(ctx, store.MCPServer{
		Scope:   "platform",
		Name:    "github",
		Config:  json.RawMessage(`{"command":"npx","args":["-y","server"]}`),
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateMCPServer platform: %v", err)
	}
	if platform.ID == "" || platform.CreatedAt.IsZero() || platform.UpdatedAt.IsZero() {
		t.Fatalf("platform row missing generated fields: %+v", platform)
	}
	if platform.ProjectID != nil {
		t.Fatalf("platform project id = %v", platform.ProjectID)
	}

	project, err := s.CreateMCPServer(ctx, store.MCPServer{
		Scope:     "project",
		ProjectID: &projectID,
		Name:      "github",
		Config:    json.RawMessage(`{"url":"https://mcp.example.com"}`),
		Enabled:   false,
	})
	if err != nil {
		t.Fatalf("CreateMCPServer project: %v", err)
	}
	if project.ProjectID == nil || *project.ProjectID != 42 {
		t.Fatalf("project id = %v", project.ProjectID)
	}

	got, err := s.GetMCPServer(ctx, platform.ID)
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if string(got.Config) != `{"command":"npx","args":["-y","server"]}` {
		t.Fatalf("config = %s", got.Config)
	}

	all, err := s.ListMCPServers(ctx, "", nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListMCPServers all = %d, %v", len(all), err)
	}
	platformList, err := s.ListMCPServers(ctx, "platform", nil)
	if err != nil || len(platformList) != 1 || platformList[0].ID != platform.ID {
		t.Fatalf("platform list = %+v, %v", platformList, err)
	}
	projectList, err := s.ListMCPServers(ctx, "project", &projectID)
	if err != nil || len(projectList) != 1 || projectList[0].ID != project.ID {
		t.Fatalf("project list = %+v, %v", projectList, err)
	}

	clock.Advance(time.Minute)
	name := "gitlab"
	enabled := true
	updated, err := s.UpdateMCPServer(ctx, project.ID, store.MCPServerUpdate{
		Name:    &name,
		Config:  json.RawMessage(`{"url":"https://gitlab.example.com/mcp"}`),
		Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("UpdateMCPServer: %v", err)
	}
	if updated.Name != "gitlab" || !updated.Enabled {
		t.Fatalf("updated = %+v", updated)
	}
	if string(updated.Config) != `{"url":"https://gitlab.example.com/mcp"}` {
		t.Fatalf("updated config = %s", updated.Config)
	}
	if !updated.UpdatedAt.After(updated.CreatedAt) {
		t.Fatalf("updated_at %v did not advance past created_at %v", updated.UpdatedAt, updated.CreatedAt)
	}

	if err := s.DeleteMCPServer(ctx, project.ID); err != nil {
		t.Fatalf("DeleteMCPServer: %v", err)
	}
	if _, err := s.GetMCPServer(ctx, project.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetMCPServer after delete = %v", err)
	}
	if err := s.DeleteMCPServer(ctx, project.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

func TestMCPServerScopeAndUniqueness(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	mustProject(t, s, 43)
	projectID := int64(42)
	otherProject := int64(43)

	base := store.MCPServer{Scope: "platform", Name: "shared", Config: json.RawMessage(`{"command":"x"}`)}
	if _, err := s.CreateMCPServer(ctx, base); err != nil {
		t.Fatalf("create platform: %v", err)
	}
	if _, err := s.CreateMCPServer(ctx, base); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate platform name = %v, want ErrConflict", err)
	}

	project := base
	project.Scope = "project"
	project.ProjectID = &projectID
	if _, err := s.CreateMCPServer(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.CreateMCPServer(ctx, project); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate project name = %v, want ErrConflict", err)
	}
	other := project
	other.ProjectID = &otherProject
	if _, err := s.CreateMCPServer(ctx, other); err != nil {
		t.Fatalf("same name in another project: %v", err)
	}

	noProject := base
	noProject.Scope = "project"
	if _, err := s.CreateMCPServer(ctx, noProject); err == nil {
		t.Fatal("project scope without project_id accepted")
	}
	unknownScope := base
	unknownScope.Scope = "global"
	if _, err := s.CreateMCPServer(ctx, unknownScope); err == nil {
		t.Fatal("unknown scope accepted")
	}
	badConfig := base
	badConfig.Name = "bad"
	badConfig.Config = json.RawMessage(`[]`)
	if _, err := s.CreateMCPServer(ctx, badConfig); err == nil {
		t.Fatal("non-object config accepted")
	}
	if _, err := s.ListMCPServers(ctx, "project", nil); err == nil {
		t.Fatal("listing project scope without project_id accepted")
	}
}

func TestSkillRoundtripAndUniqueness(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)
	projectID := int64(42)

	platform, err := s.CreateSkill(ctx, store.Skill{
		Scope:       "platform",
		Name:        "go-review",
		Description: "Review Go changes",
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("CreateSkill platform: %v", err)
	}
	if platform.Dir != "" || platform.CreatedAt.IsZero() {
		t.Fatalf("uploaded skill row = %+v", platform)
	}

	registered, err := s.CreateSkill(ctx, store.Skill{
		Scope:       "project",
		ProjectID:   &projectID,
		Name:        "security",
		Description: "Security checks",
		Dir:         "/srv/skills/security",
		Enabled:     false,
	})
	if err != nil {
		t.Fatalf("CreateSkill project: %v", err)
	}
	if registered.Dir != "/srv/skills/security" {
		t.Fatalf("dir = %q", registered.Dir)
	}

	duplicate := *platform
	duplicate.ID = ""
	if _, err := s.CreateSkill(ctx, duplicate); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate platform skill = %v, want ErrConflict", err)
	}
	projectDuplicate := *registered
	projectDuplicate.ID = ""
	if _, err := s.CreateSkill(ctx, projectDuplicate); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate project skill = %v, want ErrConflict", err)
	}

	skills, err := s.ListSkills(ctx, "", nil)
	if err != nil || len(skills) != 2 {
		t.Fatalf("ListSkills all = %d, %v", len(skills), err)
	}
	filtered, err := s.ListSkills(ctx, "platform", nil)
	if err != nil || len(filtered) != 1 || filtered[0].Name != "go-review" {
		t.Fatalf("ListSkills platform = %+v, %v", filtered, err)
	}
	if err := s.DeleteSkill(ctx, registered.ID); err != nil {
		t.Fatalf("DeleteSkill: %v", err)
	}
	if err := s.DeleteSkill(ctx, registered.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second DeleteSkill = %v, want ErrNotFound", err)
	}
}

func TestBlockBindingUpsertAndList(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	projectID := int64(42)

	platform, err := s.UpsertBlockBinding(ctx, store.BlockBinding{
		Scope:    "platform",
		Block:    "code-review",
		MCPTools: []string{"mcp__github__search"},
		Skills:   []string{"go-review"},
	})
	if err != nil {
		t.Fatalf("UpsertBlockBinding platform: %v", err)
	}
	if platform.ProjectID != nil {
		t.Fatalf("platform binding project id = %v", platform.ProjectID)
	}

	project, err := s.UpsertBlockBinding(ctx, store.BlockBinding{
		Scope:     "project",
		ProjectID: &projectID,
		Block:     "code-review",
	})
	if err != nil {
		t.Fatalf("UpsertBlockBinding project: %v", err)
	}
	if project.MCPTools == nil || project.Skills == nil {
		t.Fatalf("empty arrays were not normalized: %+v", project)
	}

	replaced, err := s.UpsertBlockBinding(ctx, store.BlockBinding{
		Scope:     "project",
		ProjectID: &projectID,
		Block:     "code-review",
		MCPTools:  []string{"mcp__gitlab__list"},
		Skills:    []string{"security"},
	})
	if err != nil {
		t.Fatalf("replace binding: %v", err)
	}
	if len(replaced.MCPTools) != 1 || replaced.MCPTools[0] != "mcp__gitlab__list" {
		t.Fatalf("replaced tools = %+v", replaced.MCPTools)
	}

	all, err := s.ListBlockBindings(ctx, "", nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListBlockBindings all = %+v, %v", all, err)
	}
	projectBindings, err := s.ListBlockBindings(ctx, "project", &projectID)
	if err != nil || len(projectBindings) != 1 {
		t.Fatalf("ListBlockBindings project = %+v, %v", projectBindings, err)
	}
	if got := projectBindings[0].Skills; len(got) != 1 || got[0] != "security" {
		t.Fatalf("project skills = %+v", got)
	}
	if _, err := s.UpsertBlockBinding(ctx, store.BlockBinding{Scope: "platform", Block: ""}); err == nil {
		t.Fatal("empty block accepted")
	}
}

func TestProjectDeleteCascadesExtensions(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	mustProject(t, s, 43)
	projectID := int64(42)
	otherProject := int64(43)

	for _, id := range []*int64{&projectID, &otherProject} {
		if _, err := s.CreateMCPServer(ctx, store.MCPServer{
			Scope: "project", ProjectID: id, Name: "srv", Config: json.RawMessage(`{"command":"x"}`),
		}); err != nil {
			t.Fatalf("create mcp server: %v", err)
		}
		if _, err := s.CreateSkill(ctx, store.Skill{
			Scope: "project", ProjectID: id, Name: "skill", Description: "d",
		}); err != nil {
			t.Fatalf("create skill: %v", err)
		}
	}
	if _, err := s.CreateMCPServer(ctx, store.MCPServer{
		Scope: "platform", Name: "shared", Config: json.RawMessage(`{"command":"x"}`),
	}); err != nil {
		t.Fatalf("create platform server: %v", err)
	}

	if _, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, projectID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	servers, err := s.ListMCPServers(ctx, "project", &projectID)
	if err != nil || len(servers) != 0 {
		t.Fatalf("project servers after cascade = %+v, %v", servers, err)
	}
	skills, err := s.ListSkills(ctx, "project", &projectID)
	if err != nil || len(skills) != 0 {
		t.Fatalf("project skills after cascade = %+v, %v", skills, err)
	}
	otherServers, err := s.ListMCPServers(ctx, "project", &otherProject)
	if err != nil || len(otherServers) != 1 {
		t.Fatalf("other project servers = %+v, %v", otherServers, err)
	}
	platformServers, err := s.ListMCPServers(ctx, "platform", nil)
	if err != nil || len(platformServers) != 1 {
		t.Fatalf("platform servers = %+v, %v", platformServers, err)
	}
}

func TestListExtensionsPage(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)
	mustProject(t, s, 43)
	project42 := int64(42)
	project43 := int64(43)

	for _, name := range []string{"p1", "p2", "p3"} {
		if _, err := s.CreateMCPServer(ctx, store.MCPServer{
			Scope: "platform", Name: name, Config: json.RawMessage(`{"command":"x"}`),
		}); err != nil {
			t.Fatalf("create platform mcp %s: %v", name, err)
		}
	}
	for _, name := range []string{"42a", "42b"} {
		if _, err := s.CreateMCPServer(ctx, store.MCPServer{
			Scope: "project", ProjectID: &project42, Name: name, Config: json.RawMessage(`{"command":"x"}`),
		}); err != nil {
			t.Fatalf("create project mcp %s: %v", name, err)
		}
	}
	if _, err := s.CreateMCPServer(ctx, store.MCPServer{
		Scope: "project", ProjectID: &project43, Name: "43a", Config: json.RawMessage(`{"command":"x"}`),
	}); err != nil {
		t.Fatalf("create other mcp: %v", err)
	}

	for _, name := range []string{"s1", "s2", "s3"} {
		if _, err := s.CreateSkill(ctx, store.Skill{
			Scope: "platform", Name: name, Description: "d",
		}); err != nil {
			t.Fatalf("create platform skill %s: %v", name, err)
		}
	}
	for _, name := range []string{"s42a", "s42b"} {
		if _, err := s.CreateSkill(ctx, store.Skill{
			Scope: "project", ProjectID: &project42, Name: name, Description: "d",
		}); err != nil {
			t.Fatalf("create project skill %s: %v", name, err)
		}
	}

	for _, block := range []string{"a", "b", "c"} {
		if _, err := s.UpsertBlockBinding(ctx, store.BlockBinding{
			Scope: "platform", Block: block,
		}); err != nil {
			t.Fatalf("create platform binding %s: %v", block, err)
		}
	}
	for _, block := range []string{"a", "b"} {
		if _, err := s.UpsertBlockBinding(ctx, store.BlockBinding{
			Scope: "project", ProjectID: &project42, Block: block,
		}); err != nil {
			t.Fatalf("create project binding %s: %v", block, err)
		}
	}

	platformServers := collectMCPServerPages(t, s, "platform", nil, 1)
	if len(platformServers) != 3 {
		t.Fatalf("platform servers = %+v", platformServers)
	}
	for i, want := range []string{"p3", "p2", "p1"} {
		if platformServers[i].Name != want {
			t.Fatalf("platform server[%d] = %s, want %s", i, platformServers[i].Name, want)
		}
		if !platformServers[i].CreatedAt.Equal(clock.Now()) {
			t.Fatalf("platform server[%d] created_at = %v", i, platformServers[i].CreatedAt)
		}
	}
	projectServers := collectMCPServerPages(t, s, "project", &project42, 1)
	if len(projectServers) != 2 || projectServers[0].Name != "42b" || projectServers[1].Name != "42a" {
		t.Fatalf("project servers = %+v", projectServers)
	}

	_, cursor, err := s.ListMCPServersPage(ctx, "platform", nil, "", 1)
	if err != nil || cursor == "" {
		t.Fatalf("platform page = %q, %v", cursor, err)
	}
	if _, _, err := s.ListMCPServersPage(ctx, "project", &project42, cursor, 1); !errors.Is(err, store.ErrInvalidCursor) {
		t.Fatalf("cross-scope cursor = %v", err)
	}
	_, projectCursor, err := s.ListMCPServersPage(ctx, "project", &project42, "", 1)
	if err != nil || projectCursor == "" {
		t.Fatalf("project page = %q, %v", projectCursor, err)
	}
	if _, _, err := s.ListMCPServersPage(ctx, "project", &project43, projectCursor, 1); !errors.Is(err, store.ErrInvalidCursor) {
		t.Fatalf("cross-project cursor = %v", err)
	}

	platformSkills := collectSkillPages(t, s, "platform", nil, 1)
	if len(platformSkills) != 3 {
		t.Fatalf("platform skills = %+v", platformSkills)
	}
	projectSkills := collectSkillPages(t, s, "project", &project42, 1)
	if len(projectSkills) != 2 {
		t.Fatalf("project skills = %+v", projectSkills)
	}
	_, skillCursor, err := s.ListSkillsPage(ctx, "platform", nil, "", 1)
	if err != nil || skillCursor == "" {
		t.Fatalf("skill page = %q, %v", skillCursor, err)
	}
	if _, _, err := s.ListSkillsPage(ctx, "project", &project42, skillCursor, 1); !errors.Is(err, store.ErrInvalidCursor) {
		t.Fatalf("cross-scope skill cursor = %v", err)
	}

	platformBindings := collectBindingPages(t, s, "platform", nil, 1)
	if len(platformBindings) != 3 {
		t.Fatalf("platform bindings = %+v", platformBindings)
	}
	for i, want := range []string{"a", "b", "c"} {
		if platformBindings[i].Block != want {
			t.Fatalf("platform binding[%d] = %s, want %s", i, platformBindings[i].Block, want)
		}
	}
	projectBindings := collectBindingPages(t, s, "project", &project42, 1)
	if len(projectBindings) != 2 || projectBindings[0].Block != "a" || projectBindings[1].Block != "b" {
		t.Fatalf("project bindings = %+v", projectBindings)
	}
	allBindings := collectBindingPages(t, s, "", nil, 2)
	if len(allBindings) != 5 {
		t.Fatalf("all bindings = %+v", allBindings)
	}
	_, bindingCursor, err := s.ListBlockBindingsPage(ctx, "platform", nil, "", 1)
	if err != nil || bindingCursor == "" {
		t.Fatalf("binding page = %q, %v", bindingCursor, err)
	}
	if _, _, err := s.ListBlockBindingsPage(ctx, "project", &project42, bindingCursor, 1); !errors.Is(err, store.ErrInvalidCursor) {
		t.Fatalf("cross-scope binding cursor = %v", err)
	}
	if _, _, err := s.ListBlockBindingsPage(ctx, "", nil, "not-a-cursor", 1); !errors.Is(err, store.ErrInvalidCursor) {
		t.Fatalf("invalid binding cursor = %v", err)
	}
}

func collectMCPServerPages(t *testing.T, s *Store, scope string, projectID *int64, limit int) []store.MCPServer {
	t.Helper()
	out := make([]store.MCPServer, 0)
	cursor := ""
	for {
		page, next, err := s.ListMCPServersPage(context.Background(), scope, projectID, cursor, limit)
		if err != nil {
			t.Fatalf("ListMCPServersPage: %v", err)
		}
		out = append(out, page...)
		if next == "" {
			return out
		}
		if next == cursor {
			t.Fatal("repeating mcp cursor")
		}
		cursor = next
	}
}

func collectSkillPages(t *testing.T, s *Store, scope string, projectID *int64, limit int) []store.Skill {
	t.Helper()
	out := make([]store.Skill, 0)
	cursor := ""
	for {
		page, next, err := s.ListSkillsPage(context.Background(), scope, projectID, cursor, limit)
		if err != nil {
			t.Fatalf("ListSkillsPage: %v", err)
		}
		out = append(out, page...)
		if next == "" {
			return out
		}
		if next == cursor {
			t.Fatal("repeating skill cursor")
		}
		cursor = next
	}
}

func collectBindingPages(t *testing.T, s *Store, scope string, projectID *int64, limit int) []store.BlockBinding {
	t.Helper()
	out := make([]store.BlockBinding, 0)
	cursor := ""
	for {
		page, next, err := s.ListBlockBindingsPage(context.Background(), scope, projectID, cursor, limit)
		if err != nil {
			t.Fatalf("ListBlockBindingsPage: %v", err)
		}
		out = append(out, page...)
		if next == "" {
			return out
		}
		if next == cursor {
			t.Fatal("repeating binding cursor")
		}
		cursor = next
	}
}
