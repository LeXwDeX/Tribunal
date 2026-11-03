package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

var (
	mcpNameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	blockNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	skillNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// MCPServer is one stored Claude Code mcpServers entry. Handlers hide Config
// in responses; internal services use the full configuration.
type MCPServer struct {
	ID        string          `json:"id"`
	Scope     string          `json:"scope"`
	ProjectID *int64          `json:"project_id,omitempty"`
	Name      string          `json:"name"`
	Config    json.RawMessage `json:"config"`
	Enabled   bool            `json:"enabled"`
	// Builtin marks a server shipped with the platform that cannot be disabled,
	// renamed, reconfigured or deleted.
	Builtin   bool      `json:"builtin"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MCPTool is one tool listed by a test connection.
type MCPTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// MCPServerUpdate replaces only the provided fields. Config, when set,
// replaces the whole server object.
type MCPServerUpdate struct {
	Name    *string
	Config  json.RawMessage
	Enabled *bool
}

// Skill is one registered skill. Dir is the managed server-side directory
// for both uploaded archives and registered directories.
type Skill struct {
	ID          string `json:"id"`
	Scope       string `json:"scope"`
	ProjectID   *int64 `json:"project_id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Dir         string `json:"dir,omitempty"`
	Enabled     bool   `json:"enabled"`
	// Builtin marks a skill shipped with the platform that cannot be disabled or deleted.
	Builtin   bool      `json:"builtin"`
	CreatedAt time.Time `json:"created_at"`
}

// BlockBinding grants MCP tools and skills to one review block.
type BlockBinding struct {
	Scope     string   `json:"scope"`
	ProjectID *int64   `json:"project_id,omitempty"`
	Block     string   `json:"block"`
	MCPTools  []string `json:"mcp_tools"`
	Skills    []string `json:"skills"`
	Revision  string   `json:"revision"`
}

// BlockBindingInput upserts one binding.
type BlockBindingInput struct {
	Scope     string
	ProjectID *int64
	Block     string
	MCPTools  []string
	Skills    []string
}

// RestoreDefaultsInput asks for a preview or an execution of the shipped
// defaults. The service rebuilds the grants from the process configuration;
// the request never carries grants.
type RestoreDefaultsInput struct {
	Blocks       []string
	DryRun       bool
	PreviewToken string
}

// RestoreGrant is one list of entries a restore adds or removes.
type RestoreGrant struct {
	MCPTools []string `json:"mcp_tools"`
	Skills   []string `json:"skills"`
}

// RestoreDefaultsChange is one block's restore outcome. OwnershipChanged
// reports a newly default-owned row or block snapshot; a pure grant ledger
// update leaves it false.
type RestoreDefaultsChange struct {
	Block            string       `json:"block"`
	Status           string       `json:"status"`
	Reason           string       `json:"reason"`
	Add              RestoreGrant `json:"add"`
	Remove           RestoreGrant `json:"remove"`
	OwnershipChanged bool         `json:"ownership_changed"`
}

// RestoreDefaultsResult is the preview and execution response.
type RestoreDefaultsResult struct {
	Changes         []RestoreDefaultsChange `json:"changes"`
	PreviewToken    string                  `json:"preview_token"`
	RestartRequired bool                    `json:"restart_required"`
}

// parseExtensionScope validates the shared scope/project_id selector. An
// empty scope is allowed for list endpoints and means "all scopes".
func parseExtensionScope(r *http.Request, required bool) (extensionScope, error) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	projectID, err := queryInt64(r, "project_id")
	if err != nil {
		return extensionScope{}, err
	}
	return validateExtensionScope(scope, projectID, required)
}

func validateExtensionScope(scope string, projectID *int64, required bool) (extensionScope, error) {
	if scope == "" {
		if required {
			return extensionScope{}, errors.New("scope is required")
		}
		if projectID != nil {
			return extensionScope{}, errors.New("project_id requires project scope")
		}
		return extensionScope{}, nil
	}
	switch scope {
	case "platform":
		if projectID != nil {
			return extensionScope{}, errors.New("project_id must be omitted for platform scope")
		}
	case "project":
		if projectID == nil {
			return extensionScope{}, errors.New("project_id is required for project scope")
		}
	default:
		return extensionScope{}, errors.New("scope must be platform or project")
	}
	return extensionScope{scope: scope, projectID: projectID}, nil
}

// authorizeExtensionScope applies the project boundary to extension lists and
// writes. A project-scoped read needs a stored grant. Global scope and any
// write/test/delete of host execution configuration need the platform admin
// role; project-scoped writes additionally need the manage grant.
func (s *Server) authorizeExtensionScope(w http.ResponseWriter, r *http.Request, scope extensionScope, write bool) bool {
	switch scope.scope {
	case "project":
		if scope.projectID == nil {
			validationFailed(w, r, "project_id", "project_id is required for project scope")
			return false
		}
		if write && !s.requirePlatformAdmin(w, r) {
			return false
		}
		_, ok := s.requireProjectAccess(w, r, *scope.projectID, write)
		return ok
	case "platform":
		return s.requirePlatformAdmin(w, r)
	default:
		// An empty selector means "all scopes". That global view is admin-only.
		return s.requirePlatformAdmin(w, r)
	}
}

// authorizeExtensionResource reuses the stored resource scope for update,
// delete and test operations, which do not receive a scope selector.
func (s *Server) authorizeExtensionResource(w http.ResponseWriter, r *http.Request, scope string, projectID *int64, write bool) bool {
	return s.authorizeExtensionScope(w, r, extensionScope{scope: scope, projectID: projectID}, write)
}

// requireExtensionWriter is the part of the write check that does not depend
// on the resource: every extension write, in either scope, needs the platform
// admin role. Id routes call it before the lookup so a caller who cannot write
// gets the same answer for an existing and a missing id; the stored scope is
// checked afterwards with authorizeExtensionResource.
func (s *Server) requireExtensionWriter(w http.ResponseWriter, r *http.Request) bool {
	return s.requirePlatformAdmin(w, r)
}

func (s *Server) handleListMCPServers(w http.ResponseWriter, r *http.Request) {
	if !s.requireExtensions(w, r) {
		return
	}
	scope, err := parseExtensionScope(r, false)
	if err != nil {
		validationFailed(w, r, "scope", err.Error())
		return
	}
	if !s.authorizeExtensionScope(w, r, scope, false) {
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		validationFailed(w, r, "limit", err.Error())
		return
	}
	servers, next, err := s.extensions.ListMCPServersPage(r.Context(),
		scope.scope, scope.projectID, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	out := make([]MCPServer, 0, len(servers))
	for _, server := range servers {
		out = append(out, maskMCPServer(server))
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": out, "next_cursor": next})
}

func (s *Server) handleCreateMCPServer(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusForbidden, "custom_mcp_creation_disabled", "custom MCP server creation is disabled", nil)
}

type mcpServerUpdateRequest struct {
	Name    *string         `json:"name,omitempty"`
	Config  json.RawMessage `json:"config,omitempty"`
	Enabled *bool           `json:"enabled,omitempty"`
}

func (s *Server) handleUpdateMCPServer(w http.ResponseWriter, r *http.Request) {
	if !s.requireExtensions(w, r) {
		return
	}
	id := r.PathValue("id")
	if strings.TrimSpace(id) == "" {
		validationFailed(w, r, "id", "id is required")
		return
	}
	if !s.requireExtensionWriter(w, r) {
		return
	}
	current, err := s.extensions.GetMCPServer(r.Context(), id)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if !s.authorizeExtensionResource(w, r, current.Scope, current.ProjectID, true) {
		return
	}
	var request mcpServerUpdateRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if request.Name == nil && request.Config == nil && request.Enabled == nil {
		validationFailed(w, r, "body", "at least one of name, config or enabled is required")
		return
	}
	update := MCPServerUpdate{Config: request.Config, Enabled: request.Enabled}
	if request.Name != nil {
		name := strings.TrimSpace(*request.Name)
		if !mcpNameRE.MatchString(name) {
			validationFailed(w, r, "name", "name must match "+mcpNameRE.String())
			return
		}
		update.Name = &name
	}
	if request.Config != nil {
		if err := validateMCPConfigObject(request.Config); err != nil {
			validationFailed(w, r, "config", err.Error())
			return
		}
	}
	server, err := s.extensions.UpdateMCPServer(r.Context(), id, update)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, maskMCPServer(*server))
}

func (s *Server) handleDeleteMCPServer(w http.ResponseWriter, r *http.Request) {
	if !s.requireExtensions(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.requireExtensionWriter(w, r) {
		return
	}
	server, err := s.extensions.GetMCPServer(r.Context(), id)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if !s.authorizeExtensionResource(w, r, server.Scope, server.ProjectID, true) {
		return
	}
	if err := s.extensions.DeleteMCPServer(r.Context(), id); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleImportMCPServers(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusForbidden, "custom_mcp_creation_disabled", "custom MCP server creation is disabled", nil)
}

func (s *Server) handleTestMCPServer(w http.ResponseWriter, r *http.Request) {
	if !s.requireExtensions(w, r) {
		return
	}
	if s.mcp == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "MCP tester is not configured", nil)
		return
	}
	id := r.PathValue("id")
	if !s.requireExtensionWriter(w, r) {
		return
	}
	server, err := s.extensions.GetMCPServer(r.Context(), id)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if !s.authorizeExtensionResource(w, r, server.Scope, server.ProjectID, true) {
		return
	}
	tools, err := s.mcp.TestMCPServer(r.Context(), *server)
	if err != nil {
		s.logger.Warn("mcp server test failed",
			"request_id", requestID(r.Context()), "server_id", id)
		writeError(w, r, http.StatusInternalServerError, "internal", "MCP server test failed", nil)
		return
	}
	if tools == nil {
		tools = []MCPTool{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": tools})
}

// validateMCPConfigObject rejects non-object or empty server configs. It runs
// before persistence so a bad import fails as one request.
func validateMCPConfigObject(raw json.RawMessage) error {
	if len(raw) == 0 {
		return errors.New("config is required")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || len(object) == 0 {
		return errors.New("config must be a non-empty JSON object")
	}
	return nil
}

// Config can contain secrets in any field, including keys and unknown fields.
func maskMCPServer(server MCPServer) MCPServer {
	server.Config = json.RawMessage(`{}`)
	return server
}

func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	if !s.requireExtensions(w, r) {
		return
	}
	scope, err := parseExtensionScope(r, false)
	if err != nil {
		validationFailed(w, r, "scope", err.Error())
		return
	}
	if !s.authorizeExtensionScope(w, r, scope, false) {
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		validationFailed(w, r, "limit", err.Error())
		return
	}
	skills, next, err := s.extensions.ListSkillsPage(r.Context(),
		scope.scope, scope.projectID, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if skills == nil {
		skills = []Skill{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": skills, "next_cursor": next})
}

// handleUploadSkillOrRegisterDirectory closes both custom skill creation paths.
func (s *Server) handleUploadSkillOrRegisterDirectory(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusForbidden, "custom_skill_creation_disabled", "custom skill creation is disabled", nil)
}

func (s *Server) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	if !s.requireExtensions(w, r) {
		return
	}
	id := r.PathValue("id")
	// The delete endpoint has no scope selector. Resolve the skill's stored
	// scope so a project-scoped skill also requires the manage grant; the
	// route guard already requires the platform admin role.
	skills, err := s.extensions.ListSkills(r.Context(), "", nil)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	for _, skill := range skills {
		if skill.ID != id {
			continue
		}
		if !s.authorizeExtensionResource(w, r, skill.Scope, skill.ProjectID, true) {
			return
		}
		break
	}
	if err := s.extensions.DeleteSkill(r.Context(), id); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type skillUpdateRequest struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// handleUpdateSkill enables or disables one skill.
func (s *Server) handleUpdateSkill(w http.ResponseWriter, r *http.Request) {
	if !s.requireExtensions(w, r) {
		return
	}
	id := r.PathValue("id")
	if strings.TrimSpace(id) == "" {
		validationFailed(w, r, "id", "id is required")
		return
	}
	current, err := s.extensions.GetSkill(r.Context(), id)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if !s.authorizeExtensionResource(w, r, current.Scope, current.ProjectID, true) {
		return
	}
	var request skillUpdateRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if request.Enabled == nil {
		validationFailed(w, r, "enabled", "enabled is required")
		return
	}
	skill, err := s.extensions.SetSkillEnabled(r.Context(), id, *request.Enabled)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, skill)
}

func (s *Server) handleListBlockBindings(w http.ResponseWriter, r *http.Request) {
	if !s.requireExtensions(w, r) {
		return
	}
	scope, err := parseExtensionScope(r, false)
	if err != nil {
		validationFailed(w, r, "scope", err.Error())
		return
	}
	if !s.authorizeExtensionScope(w, r, scope, false) {
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		validationFailed(w, r, "limit", err.Error())
		return
	}
	bindings, next, err := s.extensions.ListBlockBindingsPage(r.Context(),
		scope.scope, scope.projectID, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if bindings == nil {
		bindings = []BlockBinding{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"bindings": bindings, "next_cursor": next})
}

type blockBindingRequest struct {
	Scope            string   `json:"scope"`
	ProjectID        *int64   `json:"project_id,omitempty"`
	MCPTools         []string `json:"mcp_tools"`
	Skills           []string `json:"skills"`
	ExpectedRevision *string  `json:"expected_revision"`
}

func (s *Server) handleUpsertBlockBinding(w http.ResponseWriter, r *http.Request) {
	if !s.requireExtensions(w, r) {
		return
	}
	block := strings.TrimSpace(r.PathValue("block"))
	if !blockNameRE.MatchString(block) {
		validationFailed(w, r, "block", "block must match "+blockNameRE.String())
		return
	}
	var request blockBindingRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	scope, err := validateExtensionScope(strings.TrimSpace(request.Scope), request.ProjectID, true)
	if err != nil {
		validationFailed(w, r, "scope", err.Error())
		return
	}
	if !s.authorizeExtensionScope(w, r, scope, true) {
		return
	}
	if request.ExpectedRevision == nil {
		validationFailed(w, r, "expected_revision", "expected_revision is required")
		return
	}
	if !store.ValidBlockBindingRevision(*request.ExpectedRevision) {
		validationFailed(w, r, "expected_revision", "expected_revision must be absent or a lowercase SHA-256 revision")
		return
	}
	tools := request.MCPTools
	if tools == nil {
		tools = []string{}
	}
	skills := request.Skills
	if skills == nil {
		skills = []string{}
	}
	binding, err := s.extensions.ReplaceBlockBinding(r.Context(), BlockBindingInput{
		Scope:     scope.scope,
		ProjectID: scope.projectID,
		Block:     block,
		MCPTools:  tools,
		Skills:    skills,
	}, *request.ExpectedRevision)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, binding)
}

type restoreDefaultsRequest struct {
	Blocks       []string `json:"blocks"`
	DryRun       *bool    `json:"dry_run"`
	PreviewToken string   `json:"preview_token"`
}

// handleRestoreDefaultBlockBindings previews or applies the defaults shipped
// by the running process. Only a platform administrator may call it; the
// request never supplies grants, so a client cannot restore its own values.
func (s *Server) handleRestoreDefaultBlockBindings(w http.ResponseWriter, r *http.Request) {
	if !s.requireExtensions(w, r) {
		return
	}
	if !s.requirePlatformAdmin(w, r) {
		return
	}
	var request restoreDefaultsRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if request.DryRun == nil {
		validationFailed(w, r, "dry_run", "dry_run is required")
		return
	}
	if !*request.DryRun && strings.TrimSpace(request.PreviewToken) == "" {
		validationFailed(w, r, "preview_token", "preview_token is required when dry_run is false")
		return
	}
	result, err := s.extensions.RestoreDefaults(r.Context(), RestoreDefaultsInput{
		Blocks:       request.Blocks,
		DryRun:       *request.DryRun,
		PreviewToken: request.PreviewToken,
	})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
