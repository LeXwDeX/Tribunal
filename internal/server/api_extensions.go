package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/store"
)

const (
	// mcpTestTimeout keeps one connection test below the HTTP write timeout.
	mcpTestTimeout = 30 * time.Second
	// mcpTestConcurrency bounds simultaneous test connections.
	mcpTestConcurrency = 4
)

// mcpTestSlots is the process-wide connection-test semaphore. Tests that fill
// it must drain it before returning.
var mcpTestSlots = make(chan struct{}, mcpTestConcurrency)

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

// MCPTool is one tool listed by a test connection. Bindable follows the
// runtime annotationsAllow rule; an unannotated tool stays bindable.
type MCPTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ReadOnly    bool   `json:"read_only"`
	Destructive bool   `json:"destructive"`
	Bindable    bool   `json:"bindable"`
	Annotated   bool   `json:"annotated"`
}

// MCPTestResult is one successful test. ToolCount is the total advertised
// count; Tools may be capped, in which case Truncated is true.
type MCPTestResult struct {
	LatencyMS int64
	ToolCount int
	Truncated bool
	Tools     []MCPTool
}

// MCPServerInput creates one server. Config is a single mcpServers entry.
type MCPServerInput struct {
	Scope     string
	ProjectID *int64
	Name      string
	Config    json.RawMessage
	Enabled   bool
}

// MCPImportInput is one pasted {"mcpServers": {...}} document.
type MCPImportInput struct {
	Scope     string
	ProjectID *int64
	Servers   map[string]json.RawMessage
}

// MCPDraftInput tests a configuration that is not necessarily stored. When ID
// is set the stored row supplies the scope and any masked secrets.
type MCPDraftInput struct {
	ID        string
	Scope     string
	ProjectID *int64
	Name      string
	Config    json.RawMessage
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
		if *projectID <= 0 {
			return extensionScope{}, errors.New("project_id must be positive")
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

type mcpServerRequest struct {
	Scope     string          `json:"scope"`
	ProjectID *int64          `json:"project_id,omitempty"`
	Name      string          `json:"name"`
	Config    json.RawMessage `json:"config"`
	Enabled   *bool           `json:"enabled,omitempty"`
}

func (s *Server) handleCreateMCPServer(w http.ResponseWriter, r *http.Request) {
	if !s.requireExtensions(w, r) {
		return
	}
	var request mcpServerRequest
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
	name := strings.TrimSpace(request.Name)
	if !mcpNameRE.MatchString(name) {
		validationFailed(w, r, "name", "name must match "+mcpNameRE.String())
		return
	}
	if err := validateMCPConfigObject(request.Config); err != nil {
		validationFailed(w, r, "config", err.Error())
		return
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	server, err := s.extensions.CreateMCPServer(r.Context(), MCPServerInput{
		Scope:     scope.scope,
		ProjectID: scope.projectID,
		Name:      name,
		Config:    request.Config,
		Enabled:   enabled,
	})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.logger.Info("mcp server changed", "event", "platform_audit", "actor_id", currentUserID(r),
		"target_id", server.ID, "action", "create", "scope", server.Scope,
		"project_id", auditProjectID(server.ProjectID), "name", server.Name, "enabled", server.Enabled)
	writeJSON(w, http.StatusCreated, maskMCPServer(*server))
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
	if !s.requireExtensions(w, r) {
		return
	}
	scope, err := parseExtensionScope(r, true)
	if err != nil {
		validationFailed(w, r, "scope", err.Error())
		return
	}
	if !s.authorizeExtensionScope(w, r, scope, true) {
		return
	}
	// The body is the pasted document itself: {"mcpServers": {...}}. Unknown
	// top-level fields are rejected so no wrapper layer sneaks in.
	var document struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := decodeJSON(w, r, adminBodyLimit, &document); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if len(document.MCPServers) == 0 {
		validationFailed(w, r, "mcpServers", "mcpServers must contain at least one server")
		return
	}
	names := make([]string, 0, len(document.MCPServers))
	for name := range document.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !mcpNameRE.MatchString(name) {
			validationFailed(w, r, "mcpServers", fmt.Sprintf("invalid server name %q", name))
			return
		}
		if err := validateMCPConfigObject(document.MCPServers[name]); err != nil {
			validationFailed(w, r, "mcpServers."+name, err.Error())
			return
		}
	}
	servers, err := s.extensions.ImportMCPServers(r.Context(), MCPImportInput{
		Scope:     scope.scope,
		ProjectID: scope.projectID,
		Servers:   document.MCPServers,
	})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	ids := make([]string, 0, len(servers))
	imported := make([]MCPServer, 0, len(servers))
	for _, server := range servers {
		ids = append(ids, server.ID)
		imported = append(imported, maskMCPServer(server))
	}
	s.logger.Info("mcp server changed", "event", "platform_audit", "actor_id", currentUserID(r),
		"target_id", strings.Join(ids, ","), "action", "import", "scope", scope.scope,
		"project_id", auditProjectID(scope.projectID), "names", names)
	writeJSON(w, http.StatusCreated, map[string]any{"servers": imported})
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
	s.runMCPTest(w, r, "test", server.ID, server.Scope, server.ProjectID, func(ctx context.Context) (MCPTestResult, error) {
		return s.mcp.TestMCPServer(ctx, *server)
	})
}

// mcpDraftRequest is the create body plus an optional stored-server reference.
// With id set the stored scope wins and config/name are optional; any masked
// value is restored from the stored configuration before the probe.
type mcpDraftRequest struct {
	ID        string          `json:"id,omitempty"`
	Scope     string          `json:"scope"`
	ProjectID *int64          `json:"project_id,omitempty"`
	Name      string          `json:"name"`
	Config    json.RawMessage `json:"config"`
	Enabled   *bool           `json:"enabled,omitempty"`
}

// handleTestMCPDraft probes an unsaved configuration without writing a row.
func (s *Server) handleTestMCPDraft(w http.ResponseWriter, r *http.Request) {
	if !s.requireExtensions(w, r) {
		return
	}
	if s.mcp == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "MCP tester is not configured", nil)
		return
	}
	var request mcpDraftRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if id := strings.TrimSpace(request.ID); id != "" {
		// Check the platform role before the lookup so an unauthorized caller
		// gets the same answer for an existing and a missing id.
		if !s.requireExtensionWriter(w, r) {
			return
		}
		if name := strings.TrimSpace(request.Name); name != "" && !mcpNameRE.MatchString(name) {
			validationFailed(w, r, "name", "name must match "+mcpNameRE.String())
			return
		}
		if len(request.Config) > 0 {
			if err := validateMCPConfigObject(request.Config); err != nil {
				validationFailed(w, r, "config", err.Error())
				return
			}
		}
		current, err := s.extensions.GetMCPServer(r.Context(), id)
		if err != nil {
			s.writeServiceError(w, r, err)
			return
		}
		if !s.authorizeExtensionResource(w, r, current.Scope, current.ProjectID, true) {
			return
		}
		in := MCPDraftInput{
			ID:        current.ID,
			Scope:     current.Scope,
			ProjectID: current.ProjectID,
			Name:      request.Name,
			Config:    request.Config,
		}
		s.runMCPTest(w, r, "test_draft", current.ID, current.Scope, current.ProjectID, func(ctx context.Context) (MCPTestResult, error) {
			return s.mcp.TestMCPDraft(ctx, in)
		})
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
	name := strings.TrimSpace(request.Name)
	if !mcpNameRE.MatchString(name) {
		validationFailed(w, r, "name", "name must match "+mcpNameRE.String())
		return
	}
	if err := validateMCPConfigObject(request.Config); err != nil {
		validationFailed(w, r, "config", err.Error())
		return
	}
	in := MCPDraftInput{
		Scope:     scope.scope,
		ProjectID: scope.projectID,
		Name:      name,
		Config:    request.Config,
	}
	s.runMCPTest(w, r, "test_draft", "draft", scope.scope, scope.projectID, func(ctx context.Context) (MCPTestResult, error) {
		return s.mcp.TestMCPDraft(ctx, in)
	})
}

// runMCPTest applies the concurrency limit and handler timeout, then writes
// the fixed test response or a sanitized error category.
func (s *Server) runMCPTest(w http.ResponseWriter, r *http.Request, action, targetID, scope string,
	projectID *int64, test func(context.Context) (MCPTestResult, error)) {
	release, ok := acquireMCPTestSlot(r.Context())
	if !ok {
		writeError(w, r, http.StatusTooManyRequests, "test_busy", "too many concurrent MCP tests", nil)
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), mcpTestTimeout)
	defer cancel()
	result, err := test(ctx)
	if err != nil {
		category := mcpTestCategory(err)
		s.logger.Warn("mcp server test failed", "event", "platform_audit", "actor_id", currentUserID(r),
			"target_id", targetID, "action", action, "scope", scope,
			"project_id", auditProjectID(projectID), "ok", false, "error_class", category)
		s.writeMCPTestError(w, r, err)
		return
	}
	s.logger.Info("mcp server tested", "event", "platform_audit", "actor_id", currentUserID(r),
		"target_id", targetID, "action", action, "scope", scope,
		"project_id", auditProjectID(projectID), "ok", true,
		"latency_ms", result.LatencyMS, "tool_count", result.ToolCount, "truncated", result.Truncated)
	tools := result.Tools
	if tools == nil {
		tools = []MCPTool{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"latency_ms": result.LatencyMS,
		"tool_count": result.ToolCount,
		"truncated":  result.Truncated,
		"tools":      tools,
	})
}

func acquireMCPTestSlot(ctx context.Context) (func(), bool) {
	select {
	case <-ctx.Done():
		return nil, false
	default:
	}
	select {
	case mcpTestSlots <- struct{}{}:
		return func() { <-mcpTestSlots }, true
	case <-ctx.Done():
		return nil, false
	default:
		return nil, false
	}
}

// writeMCPTestError maps app validation/store errors as usual and every other
// failure to one fixed category and message; upstream text is never returned.
func (s *Server) writeMCPTestError(w http.ResponseWriter, r *http.Request, err error) {
	var appErr *app.Error
	if errors.As(err, &appErr) {
		s.writeServiceError(w, r, err)
		return
	}
	category := mcpTestCategory(err)
	status, message := mcpTestFailure(category)
	writeError(w, r, status, category, message, nil)
}

// mcpTestCategory always returns one of the fixed categories or internal.
func mcpTestCategory(err error) string {
	if category := mcp.TestCategory(err); category != "" {
		return category
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return mcp.TestCategoryConnectTimeout
	}
	return "internal"
}

func mcpTestFailure(category string) (int, string) {
	switch category {
	case mcp.TestCategoryInvalidConfig:
		return http.StatusBadRequest, "MCP server configuration is invalid"
	case mcp.TestCategoryConnectTimeout:
		return http.StatusGatewayTimeout, "MCP server connection timed out"
	case mcp.TestCategoryConnectFailed:
		return http.StatusBadGateway, "MCP server connection failed"
	case mcp.TestCategoryHandshakeFailed:
		return http.StatusBadGateway, "MCP server handshake failed"
	case mcp.TestCategoryListToolsTimeout:
		return http.StatusGatewayTimeout, "listing MCP tools timed out"
	case mcp.TestCategoryListToolsFailed:
		return http.StatusBadGateway, "listing MCP tools failed"
	case mcp.TestCategoryTooManyTools:
		return http.StatusUnprocessableEntity, "MCP server advertises too many tools"
	case mcp.TestCategoryCleanupFailed:
		return http.StatusInternalServerError, "MCP server test cleanup failed"
	default:
		return http.StatusInternalServerError, "MCP server test failed"
	}
}

func auditProjectID(projectID *int64) any {
	if projectID == nil {
		return int64(0)
	}
	return *projectID
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
