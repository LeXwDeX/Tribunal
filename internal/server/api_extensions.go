package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	maskedSecret   = "****"
	skillUploadMax = 8 << 20
	skillFileMax   = 64 << 10
)

var (
	mcpNameRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	blockNameRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	skillNameRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	sensitiveKeyRE = regexp.MustCompile(`(?i)(authorization|token|secret|password|api[-_]?key|cookie|credential)`)
)

// MCPServer is one stored Claude Code mcpServers entry. Config holds the raw
// server object; handlers mask env and sensitive header values on read.
type MCPServer struct {
	ID        string          `json:"id"`
	Scope     string          `json:"scope"`
	ProjectID *int64          `json:"project_id,omitempty"`
	Name      string          `json:"name"`
	Config    json.RawMessage `json:"config"`
	Enabled   bool            `json:"enabled"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// MCPTool is one tool listed by a test connection.
type MCPTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// MCPServerInput creates one server. Config is a single mcpServers entry.
type MCPServerInput struct {
	Scope     string
	ProjectID *int64
	Name      string
	Config    json.RawMessage
	Enabled   bool
}

// MCPServerUpdate replaces only the provided fields. Config, when set,
// replaces the whole server object.
type MCPServerUpdate struct {
	Name    *string
	Config  json.RawMessage
	Enabled *bool
}

// MCPImportInput is one pasted {"mcpServers": {...}} document.
type MCPImportInput struct {
	Scope     string
	ProjectID *int64
	Servers   map[string]json.RawMessage
}

// Skill is one registered skill. Dir is the managed server-side directory
// for both uploaded archives and registered directories.
type Skill struct {
	ID          string    `json:"id"`
	Scope       string    `json:"scope"`
	ProjectID   *int64    `json:"project_id,omitempty"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Dir         string    `json:"dir,omitempty"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
}

// SkillUpload carries the validated archive and manifest fields. Data is the
// original zip; the service decides where to store it.
type SkillUpload struct {
	Scope       string
	ProjectID   *int64
	Name        string
	Description string
	Data        []byte
}

// SkillDirInput registers an existing server-side directory.
type SkillDirInput struct {
	Scope     string
	ProjectID *int64
	Dir       string
}

// BlockBinding grants MCP tools and skills to one review block.
type BlockBinding struct {
	Scope     string   `json:"scope"`
	ProjectID *int64   `json:"project_id,omitempty"`
	Block     string   `json:"block"`
	MCPTools  []string `json:"mcp_tools"`
	Skills    []string `json:"skills"`
}

// BlockBindingInput upserts one binding.
type BlockBindingInput struct {
	Scope     string
	ProjectID *int64
	Block     string
	MCPTools  []string
	Skills    []string
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
		// Masked placeholders are display-only. Restore the stored value (or
		// drop the key) so a masked placeholder is never persisted.
		update.Config = restoreMaskedMCPConfig(request.Config, current.Config)
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
	for name, config := range document.MCPServers {
		if !mcpNameRE.MatchString(name) {
			validationFailed(w, r, "mcpServers", fmt.Sprintf("invalid server name %q", name))
			return
		}
		if err := validateMCPConfigObject(config); err != nil {
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
	out := make([]MCPServer, 0, len(servers))
	for _, server := range servers {
		out = append(out, maskMCPServer(server))
	}
	writeJSON(w, http.StatusCreated, map[string]any{"servers": out})
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

// maskMCPServer returns a copy with env values and sensitive header values
// replaced by a placeholder.
func maskMCPServer(server MCPServer) MCPServer {
	if masked, ok := maskMCPConfig(server.Config); ok {
		server.Config = masked
	}
	return server
}

func maskMCPConfig(raw json.RawMessage) (json.RawMessage, bool) {
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return raw, false
	}
	if env, ok := object["env"].(map[string]any); ok {
		for key := range env {
			env[key] = maskedSecret
		}
	}
	if headers, ok := object["headers"].(map[string]any); ok {
		for key := range headers {
			if sensitiveKeyRE.MatchString(key) {
				headers[key] = maskedSecret
			}
		}
	}
	masked, err := json.Marshal(object)
	if err != nil {
		return raw, false
	}
	return masked, true
}

// restoreMaskedMCPConfig merges a submitted config over the stored one: a
// masked value keeps the stored secret, and a masked key with no stored
// counterpart is dropped rather than written back.
func restoreMaskedMCPConfig(next, current json.RawMessage) json.RawMessage {
	var nextObject map[string]any
	if err := json.Unmarshal(next, &nextObject); err != nil {
		return next
	}
	var currentObject map[string]any
	_ = json.Unmarshal(current, &currentObject)
	if env, ok := nextObject["env"].(map[string]any); ok {
		stored, _ := currentObject["env"].(map[string]any)
		restoreMCPMap(env, stored, func(string) bool { return true })
	}
	if headers, ok := nextObject["headers"].(map[string]any); ok {
		stored, _ := currentObject["headers"].(map[string]any)
		restoreMCPMap(headers, stored, sensitiveKeyRE.MatchString)
	}
	restored, err := json.Marshal(nextObject)
	if err != nil {
		return next
	}
	return restored
}

func restoreMCPMap(next, stored map[string]any, sensitive func(string) bool) {
	for key, value := range next {
		if value != maskedSecret || !sensitive(key) {
			continue
		}
		if stored != nil {
			if original, ok := stored[key]; ok {
				next[key] = original
				continue
			}
		}
		delete(next, key)
	}
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

// handleUploadSkillOrRegisterDirectory dispatches the two documented shapes
// of POST /api/v1/skills: multipart archive upload and JSON directory
// registration.
func (s *Server) handleUploadSkillOrRegisterDirectory(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		s.handleUploadSkill(w, r)
		return
	}
	s.handleRegisterSkillDirectory(w, r)
}

func (s *Server) handleUploadSkill(w http.ResponseWriter, r *http.Request) {
	if !s.requireExtensions(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, skillUploadMax)
	if err := r.ParseMultipartForm(skillUploadMax); err != nil {
		badRequest(w, r, "invalid multipart upload")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	scope, err := validateExtensionScope(strings.TrimSpace(r.FormValue("scope")),
		parseFormInt64(r.FormValue("project_id")), true)
	if err != nil {
		validationFailed(w, r, "scope", err.Error())
		return
	}
	if !s.authorizeExtensionScope(w, r, scope, true) {
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		validationFailed(w, r, "file", "a zip file is required in the file field")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, skillUploadMax+1))
	if err != nil || len(data) > skillUploadMax {
		badRequest(w, r, "skill archive is too large")
		return
	}
	name, description, err := parseSkillArchive(data)
	if err != nil {
		validationFailed(w, r, "file", err.Error())
		return
	}
	skill, err := s.extensions.UploadSkill(r.Context(), SkillUpload{
		Scope:       scope.scope,
		ProjectID:   scope.projectID,
		Name:        name,
		Description: description,
		Data:        data,
	})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, skill)
}

// parseFormInt64 interprets an empty form value as nil and rejects malformed
// values by returning nil as well; scope validation then reports the missing
// project.
func parseFormInt64(raw string) *int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return nil
	}
	return &value
}

type skillDirectoryRequest struct {
	Scope     string `json:"scope"`
	ProjectID *int64 `json:"project_id,omitempty"`
	Dir       string `json:"dir"`
}

func (s *Server) handleRegisterSkillDirectory(w http.ResponseWriter, r *http.Request) {
	if !s.requireExtensions(w, r) {
		return
	}
	var request skillDirectoryRequest
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
	dir := strings.TrimSpace(request.Dir)
	if dir == "" || !filepath.IsAbs(dir) {
		validationFailed(w, r, "dir", "dir must be an absolute server path")
		return
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		validationFailed(w, r, "dir", "dir must be an existing directory")
		return
	}
	manifest, err := os.Stat(filepath.Join(dir, "SKILL.md"))
	if err != nil || !manifest.Mode().IsRegular() {
		validationFailed(w, r, "dir", "dir must contain a regular SKILL.md")
		return
	}
	skill, err := s.extensions.RegisterSkillDir(r.Context(), SkillDirInput{
		Scope:     scope.scope,
		ProjectID: scope.projectID,
		Dir:       dir,
	})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, skill)
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

// parseSkillArchive validates the uploaded zip and reads name/description from
// SKILL.md. The archive is never extracted to disk.
func parseSkillArchive(data []byte) (string, string, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", "", errors.New("file must be a valid zip archive")
	}
	var manifest *zip.File
	for _, file := range reader.File {
		if file.FileInfo().IsDir() || filepath.Base(file.Name) != "SKILL.md" {
			continue
		}
		if manifest == nil || strings.Count(file.Name, "/") < strings.Count(manifest.Name, "/") {
			manifest = file
		}
	}
	if manifest == nil {
		return "", "", errors.New("archive must contain SKILL.md")
	}
	stream, err := manifest.Open()
	if err != nil {
		return "", "", errors.New("SKILL.md could not be read")
	}
	defer stream.Close()
	raw, err := io.ReadAll(io.LimitReader(stream, skillFileMax+1))
	if err != nil || len(raw) > skillFileMax {
		return "", "", errors.New("SKILL.md is too large")
	}
	return parseSkillManifest(raw)
}

// parseSkillManifest reads the small YAML frontmatter subset SKILL.md uses.
// Only the name and description scalars are needed; the rest is ignored.
func parseSkillManifest(raw []byte) (string, string, error) {
	text := strings.TrimPrefix(string(raw), "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", errors.New("SKILL.md must start with a frontmatter block")
	}
	name, description := "", ""
	closed := false
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			closed = true
			break
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		switch strings.TrimSpace(key) {
		case "name":
			name = value
		case "description":
			description = value
		}
	}
	if !closed {
		return "", "", errors.New("SKILL.md frontmatter is not closed")
	}
	if !skillNameRE.MatchString(name) || strings.Contains(name, "__") {
		return "", "", errors.New("SKILL.md name must match " + skillNameRE.String())
	}
	if description == "" || len(description) > 4096 || strings.ContainsRune(description, 0) {
		return "", "", errors.New("SKILL.md description is required and must be at most 4096 bytes")
	}
	return name, description, nil
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
	Scope     string   `json:"scope"`
	ProjectID *int64   `json:"project_id,omitempty"`
	MCPTools  []string `json:"mcp_tools"`
	Skills    []string `json:"skills"`
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
	tools := request.MCPTools
	if tools == nil {
		tools = []string{}
	}
	skills := request.Skills
	if skills == nil {
		skills = []string{}
	}
	binding, err := s.extensions.UpsertBlockBinding(r.Context(), BlockBindingInput{
		Scope:     scope.scope,
		ProjectID: scope.projectID,
		Block:     block,
		MCPTools:  tools,
		Skills:    skills,
	})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, binding)
}
