package server

import (
	"context"
	"errors"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/app"
)

// AppExtensions adapts the app extension service to the admin HTTP
// interfaces. The adapter is required because internal/app must not import
// internal/server (the HTTP layer already imports app for its error and
// registration types). It only converts transport types; persistence and
// validation stay in app.
type AppExtensions struct {
	service *app.ExtensionService
}

// NewAppExtensions wraps the app service for the HTTP layer.
func NewAppExtensions(service *app.ExtensionService) *AppExtensions {
	return &AppExtensions{service: service}
}

var (
	_ ExtensionService = (*AppExtensions)(nil)
	_ MCPTester        = (*AppExtensions)(nil)
	_ ProviderTester   = (*AppExtensions)(nil)
)

func (a *AppExtensions) ready() error {
	if a == nil || a.service == nil {
		return errors.New("server: extension service is not configured")
	}
	return nil
}

func (a *AppExtensions) ListMCPServers(ctx context.Context, scope string, projectID *int64) ([]MCPServer, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	servers, err := a.service.ListMCPServers(ctx, scope, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]MCPServer, 0, len(servers))
	for _, server := range servers {
		out = append(out, mcpServerFromApp(server))
	}
	return out, nil
}

func (a *AppExtensions) ListMCPServersPage(ctx context.Context, scope string, projectID *int64, cursor string, limit int) ([]MCPServer, string, error) {
	if err := a.ready(); err != nil {
		return nil, "", err
	}
	servers, next, err := a.service.ListMCPServersPage(ctx, scope, projectID, cursor, limit)
	if err != nil {
		return nil, "", err
	}
	out := make([]MCPServer, 0, len(servers))
	for _, server := range servers {
		out = append(out, mcpServerFromApp(server))
	}
	return out, next, nil
}

func (a *AppExtensions) GetMCPServer(ctx context.Context, id string) (*MCPServer, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	server, err := a.service.GetMCPServer(ctx, id)
	if err != nil {
		return nil, err
	}
	out := mcpServerFromApp(*server)
	return &out, nil
}

func (a *AppExtensions) CreateMCPServer(ctx context.Context, in MCPServerInput) (*MCPServer, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	server, err := a.service.CreateMCPServer(ctx, app.MCPServerInput{
		Scope:     in.Scope,
		ProjectID: in.ProjectID,
		Name:      in.Name,
		Config:    in.Config,
		Enabled:   in.Enabled,
	})
	if err != nil {
		return nil, err
	}
	out := mcpServerFromApp(*server)
	return &out, nil
}

func (a *AppExtensions) UpdateMCPServer(ctx context.Context, id string, in MCPServerUpdate) (*MCPServer, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	server, err := a.service.UpdateMCPServer(ctx, id, app.MCPServerUpdate{
		Name:    in.Name,
		Config:  in.Config,
		Enabled: in.Enabled,
	})
	if err != nil {
		return nil, err
	}
	out := mcpServerFromApp(*server)
	return &out, nil
}

func (a *AppExtensions) DeleteMCPServer(ctx context.Context, id string) error {
	if err := a.ready(); err != nil {
		return err
	}
	return a.service.DeleteMCPServer(ctx, id)
}

func (a *AppExtensions) ImportMCPServers(ctx context.Context, in MCPImportInput) ([]MCPServer, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	servers, err := a.service.ImportMCPServers(ctx, app.MCPImportInput{
		Scope:     in.Scope,
		ProjectID: in.ProjectID,
		Servers:   in.Servers,
	})
	if err != nil {
		return nil, err
	}
	out := make([]MCPServer, 0, len(servers))
	for _, server := range servers {
		out = append(out, mcpServerFromApp(server))
	}
	return out, nil
}

func (a *AppExtensions) ListSkills(ctx context.Context, scope string, projectID *int64) ([]Skill, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	skills, err := a.service.ListSkills(ctx, scope, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]Skill, 0, len(skills))
	for _, skill := range skills {
		out = append(out, skillFromApp(skill))
	}
	return out, nil
}

func (a *AppExtensions) ListSkillsPage(ctx context.Context, scope string, projectID *int64, cursor string, limit int) ([]Skill, string, error) {
	if err := a.ready(); err != nil {
		return nil, "", err
	}
	skills, next, err := a.service.ListSkillsPage(ctx, scope, projectID, cursor, limit)
	if err != nil {
		return nil, "", err
	}
	out := make([]Skill, 0, len(skills))
	for _, skill := range skills {
		out = append(out, skillFromApp(skill))
	}
	return out, next, nil
}

func (a *AppExtensions) UploadSkill(ctx context.Context, in SkillUpload) (*Skill, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	skill, err := a.service.UploadSkill(ctx, app.SkillUpload{
		Scope:       in.Scope,
		ProjectID:   in.ProjectID,
		Name:        in.Name,
		Description: in.Description,
		Data:        in.Data,
	})
	if err != nil {
		return nil, err
	}
	out := skillFromApp(*skill)
	return &out, nil
}

func (a *AppExtensions) RegisterSkillDir(ctx context.Context, in SkillDirInput) (*Skill, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	skill, err := a.service.RegisterSkillDir(ctx, app.SkillDirInput{
		Scope:     in.Scope,
		ProjectID: in.ProjectID,
		Dir:       in.Dir,
	})
	if err != nil {
		return nil, err
	}
	out := skillFromApp(*skill)
	return &out, nil
}

func (a *AppExtensions) DeleteSkill(ctx context.Context, id string) error {
	if err := a.ready(); err != nil {
		return err
	}
	return a.service.DeleteSkill(ctx, id)
}

func (a *AppExtensions) ListBlockBindings(ctx context.Context, scope string, projectID *int64) ([]BlockBinding, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	bindings, err := a.service.ListBlockBindings(ctx, scope, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]BlockBinding, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, blockBindingFromApp(binding))
	}
	return out, nil
}

func (a *AppExtensions) ListBlockBindingsPage(ctx context.Context, scope string, projectID *int64, cursor string, limit int) ([]BlockBinding, string, error) {
	if err := a.ready(); err != nil {
		return nil, "", err
	}
	bindings, next, err := a.service.ListBlockBindingsPage(ctx, scope, projectID, cursor, limit)
	if err != nil {
		return nil, "", err
	}
	out := make([]BlockBinding, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, blockBindingFromApp(binding))
	}
	return out, next, nil
}

func (a *AppExtensions) UpsertBlockBinding(ctx context.Context, in BlockBindingInput) (*BlockBinding, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	binding, err := a.service.UpsertBlockBinding(ctx, app.BlockBindingInput{
		Scope:     in.Scope,
		ProjectID: in.ProjectID,
		Block:     in.Block,
		MCPTools:  in.MCPTools,
		Skills:    in.Skills,
	})
	if err != nil {
		return nil, err
	}
	out := blockBindingFromApp(*binding)
	return &out, nil
}

func (a *AppExtensions) TestMCPServer(ctx context.Context, server MCPServer) ([]MCPTool, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	tools, err := a.service.TestMCPServer(ctx, app.MCPServer{
		ID:        server.ID,
		Scope:     server.Scope,
		ProjectID: server.ProjectID,
		Name:      server.Name,
		Config:    server.Config,
		Enabled:   server.Enabled,
		CreatedAt: server.CreatedAt,
		UpdatedAt: server.UpdatedAt,
	})
	if err != nil {
		return nil, err
	}
	out := make([]MCPTool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, MCPTool{Name: tool.Name, Description: tool.Description})
	}
	return out, nil
}

func (a *AppExtensions) TestProvider(ctx context.Context, name string) (ProviderTestResult, error) {
	if err := a.ready(); err != nil {
		return ProviderTestResult{}, err
	}
	result, err := a.service.TestProvider(ctx, name)
	if err != nil {
		return ProviderTestResult{}, err
	}
	return ProviderTestResult{Model: result.Model, LatencyMS: result.LatencyMS}, nil
}

func mcpServerFromApp(server app.MCPServer) MCPServer {
	return MCPServer{
		ID:        server.ID,
		Scope:     server.Scope,
		ProjectID: server.ProjectID,
		Name:      server.Name,
		Config:    server.Config,
		Enabled:   server.Enabled,
		CreatedAt: server.CreatedAt,
		UpdatedAt: server.UpdatedAt,
	}
}

func skillFromApp(skill app.Skill) Skill {
	return Skill{
		ID:          skill.ID,
		Scope:       skill.Scope,
		ProjectID:   skill.ProjectID,
		Name:        skill.Name,
		Description: skill.Description,
		Dir:         skill.Dir,
		Enabled:     skill.Enabled,
		CreatedAt:   skill.CreatedAt,
	}
}

func blockBindingFromApp(binding app.BlockBinding) BlockBinding {
	return BlockBinding{
		Scope:     binding.Scope,
		ProjectID: binding.ProjectID,
		Block:     binding.Block,
		MCPTools:  binding.MCPTools,
		Skills:    binding.Skills,
	}
}
