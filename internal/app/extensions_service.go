package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/skill"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

const maskedExtensionSecret = "****"

// maxTestTools caps the tools returned by one test response. The connection
// still lists up to the MCP limit; the response reports the total in ToolCount
// and sets Truncated when the returned list was capped.
const maxTestTools = 200

const (
	maxSkillArchiveBytes   = 8 << 20
	maxSkillExtractedBytes = 16 << 20
	maxSkillFileBytes      = 4 << 20
	maxSkillArchiveEntries = 512
	// maxSkillArchivePath bounds one archive entry name. It matches the
	// loader's resource path limit and keeps a long name from failing later
	// as a local filesystem write error.
	maxSkillArchivePath = 512
)

var (
	extensionSensitiveKeyRE = regexp.MustCompile(`(?i)(authorization|token|secret|password|api[-_]?key|cookie|credential)`)
	// errSkillStorage classifies local filesystem failures while staging an
	// upload or directory registration. Those errors may carry absolute paths,
	// so callers answer with a generic internal error instead of validation
	// text.
	errSkillStorage = errors.New("app: skill storage failed")
)

// MCPServer is the app view of one stored MCP server.
type MCPServer struct {
	ID        string          `json:"id"`
	Scope     string          `json:"scope"`
	ProjectID *int64          `json:"project_id,omitempty"`
	Name      string          `json:"name"`
	Config    json.RawMessage `json:"config"`
	Enabled   bool            `json:"enabled"`
	// Builtin marks a server shipped with the platform. Its config follows
	// the release; it cannot be disabled, renamed, edited or deleted.
	Builtin   bool      `json:"builtin"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MCPTool is one tool advertised by a test connection. Bindable follows the
// runtime annotationsAllow rule: unannotated tools stay bindable, annotated
// tools must be read-only and non-destructive.
type MCPTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ReadOnly    bool   `json:"read_only"`
	Destructive bool   `json:"destructive"`
	Bindable    bool   `json:"bindable"`
	Annotated   bool   `json:"annotated"`
}

// MCPTestResult is one successful connection test. ToolCount is the number of
// distinct valid advertised tools after duplicate-name collapsing; Tools may be
// capped, in which case Truncated is true.
type MCPTestResult struct {
	LatencyMS int64
	ToolCount int
	Truncated bool
	Tools     []MCPTool
}

// MCPDraftInput tests a server that is not necessarily stored. When ID is set
// the stored server supplies the scope and any masked config values.
type MCPDraftInput struct {
	ID        string
	Scope     string
	ProjectID *int64
	Name      string
	Config    json.RawMessage
}

// MCPServerInput creates one server.
type MCPServerInput struct {
	Scope     string
	ProjectID *int64
	Name      string
	Config    json.RawMessage
	Enabled   bool
}

// MCPServerUpdate replaces only the provided fields.
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

// Skill is one registered skill.
type Skill struct {
	ID          string `json:"id"`
	Scope       string `json:"scope"`
	ProjectID   *int64 `json:"project_id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Dir         string `json:"dir,omitempty"`
	Enabled     bool   `json:"enabled"`
	// Builtin marks a skill shipped with the platform; it cannot be disabled or deleted.
	Builtin   bool      `json:"builtin"`
	CreatedAt time.Time `json:"created_at"`
}

// SkillUpload carries one archive. The manifest name and description are
// parsed from the extracted SKILL.md; callers cannot supply them.
type SkillUpload struct {
	Scope     string
	ProjectID *int64
	Data      []byte
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

// ProviderTestResult is the outcome of a provider connectivity probe.
type ProviderTestResult struct {
	Model     string
	LatencyMS int64
}

// ExtensionServiceConfig configures the admin extension and system services.
type ExtensionServiceConfig struct {
	Store     *sqlite.Store
	MCP       *mcp.Manager
	Clients   review.ClientProvider
	Workspace *workspace.Manager
	DeniedEnv []string
	Logger    *slog.Logger
	Now       func() time.Time
	// Platform is the configuration this process loaded. RestoreDefaults uses
	// it to rebuild the shipped defaults; it never re-reads the configuration
	// files.
	Platform *config.Config
	// InvalidateProject drops the cached runtime inputs of one project after a
	// successful project-scope write. The runtime wires it to the project
	// extension resolver; nil leaves the cache untouched.
	InvalidateProject func(projectID int64)
	// InvalidateAll drops every project's cached runtime inputs after a
	// successful platform-scope write. nil leaves the cache untouched.
	InvalidateAll func()
}

// ExtensionService persists MCP servers, skills and block bindings, tests MCP
// connections, and probes configured providers.
type ExtensionService struct {
	store             *sqlite.Store
	mcp               *mcp.Manager
	clients           review.ClientProvider
	workspace         string
	workspaces        *workspace.Manager
	deniedEnv         []string
	logger            *slog.Logger
	now               func() time.Time
	newMCPTest        func(mcp.Options) (mcpTestSession, error)
	invalidateProject func(int64)
	invalidateAll     func()

	platform    *config.Config
	restoreOnce sync.Once
	restoreSet  []ExtensionDefault
	restoreFile map[string]config.BlockBinding
	restoreErr  error
}

type mcpTestSession interface {
	ListTools(context.Context, string) ([]mcp.ToolInfo, error)
	Close() error
}

// NewExtensionService validates dependencies and builds the service.
func NewExtensionService(cfg ExtensionServiceConfig) (*ExtensionService, error) {
	if cfg.Store == nil || cfg.Workspace == nil {
		return nil, errors.New("app: extension store and workspace manager are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	root := cfg.Workspace.Root()
	if !filepath.IsAbs(root) || filepath.Dir(root) == root {
		return nil, errors.New("app: extension workspace must not be a filesystem root")
	}
	return &ExtensionService{
		store:             cfg.Store,
		mcp:               cfg.MCP,
		clients:           cfg.Clients,
		workspace:         root,
		workspaces:        cfg.Workspace,
		deniedEnv:         append([]string(nil), cfg.DeniedEnv...),
		logger:            logger,
		now:               now,
		platform:          cfg.Platform,
		invalidateProject: cfg.InvalidateProject,
		invalidateAll:     cfg.InvalidateAll,
		newMCPTest:        func(opts mcp.Options) (mcpTestSession, error) { return mcp.New(opts) },
	}, nil
}

// invalidateExtensions drops cached runtime inputs after a successful write.
// A project-scope write only affects its project; a platform-scope write
// affects every project's merged configuration. When only the all-projects
// hook is wired, a project-scope write falls back to it instead of silently
// leaving the cache stale.
func (s *ExtensionService) invalidateExtensions(scope string, projectID *int64) {
	if scope == store.ScopeProject && projectID != nil && s.invalidateProject != nil {
		s.invalidateProject(*projectID)
		return
	}
	if s.invalidateAll != nil {
		s.invalidateAll()
	}
}

// lockScope protects project configuration and paths through the full operation.
func (s *ExtensionService) lockScope(ctx context.Context, scope string, projectID *int64) (func(), error) {
	if scope != store.ScopeProject {
		return func() {}, nil
	}
	if projectID == nil || *projectID <= 0 {
		return nil, validationError("project_id is required for project scope", Detail{Field: "project_id", Message: "must be positive"})
	}
	unlock, err := s.workspaces.Get(*projectID).RLock(ctx)
	if err != nil {
		return nil, err
	}
	project, err := s.store.GetProject(ctx, *projectID)
	if err != nil || project == nil {
		unlock()
		if errors.Is(err, store.ErrNotFound) || (err == nil && project == nil) {
			return nil, notFoundError("project not found")
		}
		return nil, err
	}
	return unlock, nil
}

func sameExtensionScope(scope string, projectID *int64, currentScope string, currentID *int64) bool {
	if scope != currentScope || (projectID == nil) != (currentID == nil) {
		return false
	}
	return projectID == nil || *projectID == *currentID
}

func (s *ExtensionService) lockMCPServer(ctx context.Context, id string) (*store.MCPServer, func(), error) {
	row, err := s.store.GetMCPServer(ctx, id)
	if err != nil {
		return nil, nil, extensionStoreError(err)
	}
	if row == nil {
		return nil, nil, notFoundError("extension not found")
	}
	unlock, err := s.lockScope(ctx, row.Scope, row.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	current, err := s.store.GetMCPServer(ctx, id)
	if err != nil || current == nil || !sameExtensionScope(row.Scope, row.ProjectID, current.Scope, current.ProjectID) {
		unlock()
		if err == nil {
			err = store.ErrNotFound
		}
		return nil, nil, extensionStoreError(err)
	}
	return current, unlock, nil
}

func (s *ExtensionService) lockSkill(ctx context.Context, id string) (*store.Skill, func(), error) {
	row, err := s.store.GetSkill(ctx, id)
	if err != nil {
		return nil, nil, extensionStoreError(err)
	}
	if row == nil {
		return nil, nil, notFoundError("extension not found")
	}
	unlock, err := s.lockScope(ctx, row.Scope, row.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	current, err := s.store.GetSkill(ctx, id)
	if err != nil || current == nil || !sameExtensionScope(row.Scope, row.ProjectID, current.Scope, current.ProjectID) {
		unlock()
		if err == nil {
			err = store.ErrNotFound
		}
		return nil, nil, extensionStoreError(err)
	}
	return current, unlock, nil
}

// ListMCPServers lists stored servers for the scope selector.
func (s *ExtensionService) ListMCPServers(ctx context.Context, scope string, projectID *int64) ([]MCPServer, error) {
	rows, err := s.store.ListMCPServers(ctx, scope, projectID)
	if err != nil {
		return nil, extensionStoreError(err)
	}
	out := make([]MCPServer, 0, len(rows))
	for _, row := range rows {
		out = append(out, mcpServerFromStore(row))
	}
	return out, nil
}

// ListMCPServersPage returns one bounded page for the scope selector.
func (s *ExtensionService) ListMCPServersPage(ctx context.Context, scope string, projectID *int64, cursor string, limit int) ([]MCPServer, string, error) {
	rows, next, err := s.store.ListMCPServersPage(ctx, scope, projectID, cursor, limit)
	if err != nil {
		return nil, "", extensionStoreError(err)
	}
	out := make([]MCPServer, 0, len(rows))
	for _, row := range rows {
		out = append(out, mcpServerFromStore(row))
	}
	return out, next, nil
}

// GetMCPServer returns one stored server.
func (s *ExtensionService) GetMCPServer(ctx context.Context, id string) (*MCPServer, error) {
	row, err := s.store.GetMCPServer(ctx, id)
	if err != nil {
		return nil, extensionStoreError(err)
	}
	server := mcpServerFromStore(*row)
	return &server, nil
}

// CreateMCPServer validates and stores one server.
func (s *ExtensionService) CreateMCPServer(ctx context.Context, in MCPServerInput) (*MCPServer, error) {
	scope, projectID, err := extensionScope(in.Scope, in.ProjectID)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if err := validateMCPServerName(name); err != nil {
		return nil, err
	}
	if err := guardMaskedConfig(in.Config); err != nil {
		return nil, err
	}
	if err := s.validateMCPConfig(in.Config); err != nil {
		return nil, err
	}
	unlock, err := s.lockScope(ctx, scope, projectID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	row, err := s.store.CreateMCPServer(ctx, store.MCPServer{
		Scope:     scope,
		ProjectID: projectID,
		Name:      name,
		Config:    in.Config,
		Enabled:   in.Enabled,
	})
	if err != nil {
		return nil, extensionStoreError(err)
	}
	server := mcpServerFromStore(*row)
	s.invalidateExtensions(scope, projectID)
	return &server, nil
}

// UpdateMCPServer replaces only the provided fields.
func (s *ExtensionService) UpdateMCPServer(ctx context.Context, id string, update MCPServerUpdate) (*MCPServer, error) {
	current, unlock, err := s.lockMCPServer(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if current.Builtin != "" &&
		(update.Config != nil || (update.Name != nil && strings.TrimSpace(*update.Name) != current.Name) ||
			(update.Enabled != nil && !*update.Enabled)) {
		return nil, conflictError(errBuiltinMCPServerManaged)
	}
	if update.Name != nil {
		name := strings.TrimSpace(*update.Name)
		if err := validateMCPServerName(name); err != nil {
			return nil, err
		}
		update.Name = &name
	}
	var mergeConfig func(submitted, current json.RawMessage) json.RawMessage
	if update.Config != nil {
		// Validate against the read config, but let the store merge with the
		// value read inside its transaction so a concurrent key update wins.
		if err := s.validateMCPConfig(restoreMaskedMCPConfig(update.Config, current.Config)); err != nil {
			return nil, err
		}
		mergeConfig = restoreMaskedMCPConfig
	}
	row, err := s.store.UpdateMCPServer(ctx, id, store.MCPServerUpdate{
		Name:        update.Name,
		Config:      update.Config,
		MergeConfig: mergeConfig,
		Enabled:     update.Enabled,
	})
	if err != nil {
		return nil, extensionStoreError(err)
	}
	server := mcpServerFromStore(*row)
	s.invalidateExtensions(current.Scope, current.ProjectID)
	return &server, nil
}

// DeleteMCPServer removes one server.
// Built-in servers cannot be deleted.
func (s *ExtensionService) DeleteMCPServer(ctx context.Context, id string) error {
	current, unlock, err := s.lockMCPServer(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	if current.Builtin != "" {
		return conflictError("built-in MCP servers cannot be deleted")
	}
	if err := s.store.DeleteMCPServer(ctx, id); err != nil {
		return extensionStoreError(err)
	}
	s.invalidateExtensions(current.Scope, current.ProjectID)
	return nil
}

// ImportMCPServers upserts a pasted mcpServers document by name.
func (s *ExtensionService) ImportMCPServers(ctx context.Context, in MCPImportInput) ([]MCPServer, error) {
	scope, projectID, err := extensionScope(in.Scope, in.ProjectID)
	if err != nil {
		return nil, err
	}
	if len(in.Servers) == 0 {
		return nil, validationError("mcpServers must contain at least one server", Detail{Field: "mcpServers", Message: "at least one server is required"})
	}
	names := make([]string, 0, len(in.Servers))
	for name := range in.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	// Validate every name first so a bad name always wins over a bad config,
	// matching the order the HTTP layer documents. The detail field stays the
	// stable name/config form; the offending mcpServers key is not a field.
	for _, name := range names {
		if err := validateMCPServerName(name); err != nil {
			return nil, err
		}
	}
	for _, name := range names {
		config := in.Servers[name]
		if err := guardMaskedConfig(config); err != nil {
			return nil, err
		}
		if err := s.validateMCPConfig(config); err != nil {
			return nil, err
		}
	}
	imports := make([]store.MCPServerImport, 0, len(names))
	for _, name := range names {
		imports = append(imports, store.MCPServerImport{Name: name, Config: in.Servers[name]})
	}
	unlock, err := s.lockScope(ctx, scope, projectID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// One transaction: a built-in name or any write failure leaves no partial
	// import behind.
	rows, err := s.store.ImportMCPServers(ctx, scope, projectID, imports)
	if errors.Is(err, store.ErrBuiltinManaged) {
		return nil, conflictError(errBuiltinMCPServerManaged)
	}
	if err != nil {
		return nil, extensionStoreError(err)
	}
	out := make([]MCPServer, 0, len(rows))
	for _, row := range rows {
		out = append(out, mcpServerFromStore(row))
	}
	s.invalidateExtensions(scope, projectID)
	return out, nil
}

// ListSkills lists stored skills for the scope selector.
func (s *ExtensionService) ListSkills(ctx context.Context, scope string, projectID *int64) ([]Skill, error) {
	rows, err := s.store.ListSkills(ctx, scope, projectID)
	if err != nil {
		return nil, extensionStoreError(err)
	}
	out := make([]Skill, 0, len(rows))
	for _, row := range rows {
		out = append(out, skillFromStore(row))
	}
	return out, nil
}

// ListSkillsPage returns one bounded page for the scope selector.
func (s *ExtensionService) ListSkillsPage(ctx context.Context, scope string, projectID *int64, cursor string, limit int) ([]Skill, string, error) {
	rows, next, err := s.store.ListSkillsPage(ctx, scope, projectID, cursor, limit)
	if err != nil {
		return nil, "", extensionStoreError(err)
	}
	out := make([]Skill, 0, len(rows))
	for _, row := range rows {
		out = append(out, skillFromStore(row))
	}
	return out, next, nil
}

// UploadSkill extracts the archive below the managed skills root and persists
// the extracted directory. The archive is re-validated here so a direct
// service caller cannot bypass the HTTP checks.
func (s *ExtensionService) UploadSkill(ctx context.Context, in SkillUpload) (*Skill, error) {
	scope, projectID, err := extensionScope(in.Scope, in.ProjectID)
	if err != nil {
		return nil, err
	}
	if len(in.Data) == 0 {
		return nil, validationError("skill archive is required", Detail{Field: "file", Message: "must not be empty"})
	}
	if len(in.Data) > maxSkillArchiveBytes {
		return nil, validationError("skill archive is too large", Detail{Field: "file", Message: "must be at most 8 MiB"})
	}
	unlock, err := s.lockScope(ctx, scope, projectID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	scopeRoot, err := s.skillScopeRoot(scope, projectID)
	if err != nil {
		return nil, err
	}
	extracted, name, description, err := extractSkillArchive(in.Data, scopeRoot)
	if err != nil {
		return nil, s.skillStageError(err, "upload", scope, "file")
	}
	return s.persistSkillDir(ctx, scope, projectID, name, description, "", scopeRoot, extracted)
}

// RegisterSkillDir copies the registered directory into the managed skills
// root so the loader can scan it and later deletes cannot remove the source.
// The copied tree, not the source, supplies the manifest that is persisted.
func (s *ExtensionService) RegisterSkillDir(ctx context.Context, in SkillDirInput) (*Skill, error) {
	scope, projectID, err := extensionScope(in.Scope, in.ProjectID)
	if err != nil {
		return nil, err
	}
	dir := strings.TrimSpace(in.Dir)
	if dir == "" || !filepath.IsAbs(dir) {
		return nil, validationError("dir must be an absolute server path", Detail{Field: "dir", Message: "dir must be an absolute server path"})
	}
	unlock, err := s.lockScope(ctx, scope, projectID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	scopeRoot, err := s.skillScopeRoot(scope, projectID)
	if err != nil {
		return nil, err
	}
	staged, name, description, err := copySkillDir(dir, scopeRoot)
	if err != nil {
		return nil, s.skillStageError(err, "register_dir", scope, "dir")
	}
	return s.persistSkillDir(ctx, scope, projectID, name, description, "", scopeRoot, staged)
}

// skillStageError keeps validation reasons visible and turns local storage
// failures into a generic internal error, because wrapped filesystem errors
// may contain absolute paths. The log line carries only the operation and
// scope.
func (s *ExtensionService) skillStageError(err error, op, scope, field string) error {
	if errors.Is(err, errSkillStorage) {
		s.logger.Warn("skill storage failed", "op", op, "scope", scope)
		return internalError()
	}
	return validationError(err.Error(), Detail{Field: field, Message: err.Error()})
}

// persistSkillDir renames the staged directory to <scope>/<id>/skill, stores
// the row, and records the final path. builtin is the bundled skill name, or
// empty for administrator skills.
func (s *ExtensionService) persistSkillDir(ctx context.Context, scope string, projectID *int64, name, description, builtin, scopeRoot, staged string) (*Skill, error) {
	if !skill.ValidName(name) {
		_ = os.RemoveAll(staged)
		return nil, validationError("invalid skill name",
			Detail{Field: "name", Message: "must be a lowercase name of at most 64 characters without \"__\""})
	}
	row, err := s.store.CreateSkill(ctx, store.Skill{
		Scope:       scope,
		ProjectID:   projectID,
		Name:        name,
		Description: description,
		Enabled:     true,
		Builtin:     builtin,
	})
	if err != nil {
		_ = os.RemoveAll(staged)
		return nil, extensionStoreError(err)
	}
	container := filepath.Join(scopeRoot, row.ID)
	dir := filepath.Join(container, "skill")
	if err := os.MkdirAll(container, 0o700); err != nil {
		_ = s.store.DeleteSkill(ctx, row.ID)
		_ = os.RemoveAll(staged)
		return nil, internalError()
	}
	if err := os.Rename(staged, dir); err != nil {
		_ = s.store.DeleteSkill(ctx, row.ID)
		_ = os.RemoveAll(staged)
		s.logger.Warn("skill directory move failed", "skill_id", row.ID)
		return nil, internalError()
	}
	if err := s.store.SetSkillDir(ctx, row.ID, dir); err != nil {
		_ = s.store.DeleteSkill(ctx, row.ID)
		_ = os.RemoveAll(container)
		s.logger.Warn("skill directory persistence failed", "skill_id", row.ID)
		return nil, internalError()
	}
	row.Dir = dir
	skill := skillFromStore(*row)
	s.invalidateExtensions(scope, projectID)
	return &skill, nil
}

// DeleteSkill removes one skill row and its managed directory. Directories
// outside the managed root (legacy rows) are left untouched. Built-in skills
// cannot be deleted.
func (s *ExtensionService) DeleteSkill(ctx context.Context, id string) error {
	row, unlock, err := s.lockSkill(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	if row.Builtin != "" {
		return conflictError("built-in skills cannot be deleted")
	}
	if err := s.store.DeleteSkill(ctx, id); err != nil {
		return extensionStoreError(err)
	}
	if root, managed := s.managedSkillContainer(row); managed {
		if err := os.RemoveAll(root); err != nil {
			s.logger.Warn("skill directory cleanup failed", "skill_id", id)
		}
	}
	s.invalidateExtensions(row.Scope, row.ProjectID)
	return nil
}

// GetSkill returns one stored skill.
func (s *ExtensionService) GetSkill(ctx context.Context, id string) (*Skill, error) {
	row, err := s.store.GetSkill(ctx, id)
	if err != nil {
		return nil, extensionStoreError(err)
	}
	skill := skillFromStore(*row)
	return &skill, nil
}

// SetSkillEnabled enables or disables one skill. A disabled skill is not
// loaded, and blocks that still name it run without it.
func (s *ExtensionService) SetSkillEnabled(ctx context.Context, id string, enabled bool) (*Skill, error) {
	current, unlock, err := s.lockSkill(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if current.Builtin != "" && !enabled {
		return nil, conflictError("built-in skills cannot be disabled")
	}
	if err := s.store.SetSkillEnabled(ctx, id, enabled); err != nil {
		return nil, extensionStoreError(err)
	}
	s.invalidateExtensions(current.Scope, current.ProjectID)
	return s.GetSkill(ctx, id)
}

// skillScopeRoot returns <workspace>/skills/<platform|project-id>.
func (s *ExtensionService) skillScopeRoot(scope string, projectID *int64) (string, error) {
	root := managedSkillsRoot(s.workspace)
	if !filepath.IsAbs(root) {
		return "", errors.New("app: skills workspace is not configured")
	}
	scopeRoot := filepath.Join(root, "platform")
	if scope == store.ScopeProject {
		if projectID == nil || *projectID <= 0 {
			return "", validationError("project_id is required for project scope", Detail{Field: "project_id", Message: "must be positive"})
		}
		scopeRoot = projectSkillsContainer(s.workspace, *projectID)
	}
	if err := os.MkdirAll(scopeRoot, 0o700); err != nil {
		return "", fmt.Errorf("app: create skills directory: %w", err)
	}
	return scopeRoot, nil
}

// managedSkillContainer reports the per-skill container for a row whose
// directory lives below the managed skills root.
func (s *ExtensionService) managedSkillContainer(row *store.Skill) (string, bool) {
	if row == nil || strings.TrimSpace(row.Dir) == "" {
		return "", false
	}
	root := managedSkillsRoot(s.workspace)
	rel, err := filepath.Rel(root, row.Dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.Dir(row.Dir), true
}

// extractSkillArchive expands the archive into a new staging directory below
// parent and returns that directory plus the manifest name and description.
// Only regular files and directories are accepted; paths escaping the archive
// root are rejected.
func extractSkillArchive(data []byte, parent string) (string, string, string, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", "", "", errors.New("file must be a valid zip archive")
	}
	if len(reader.File) == 0 || len(reader.File) > maxSkillArchiveEntries {
		return "", "", "", fmt.Errorf("archive must contain between 1 and %d entries", maxSkillArchiveEntries)
	}
	var manifest *zip.File
	manifestDepth := 0
	seen := make(map[string]bool, len(reader.File))
	for _, file := range reader.File {
		if file.Mode()&os.ModeSymlink != 0 {
			return "", "", "", errors.New("archive must not contain symlinks")
		}
		if !file.Mode().IsRegular() && !file.FileInfo().IsDir() {
			return "", "", "", errors.New("archive must contain only files and directories")
		}
		clean, err := cleanArchivePath(file.Name)
		if err != nil {
			return "", "", "", err
		}
		if seen[clean] {
			return "", "", "", errors.New("archive contains duplicate paths")
		}
		seen[clean] = true
		if file.FileInfo().IsDir() || path.Base(clean) != "SKILL.md" {
			continue
		}
		depth := strings.Count(clean, "/")
		if manifest == nil || depth < manifestDepth {
			manifest = file
			manifestDepth = depth
			continue
		}
		if depth == manifestDepth {
			return "", "", "", errors.New("archive must contain exactly one SKILL.md")
		}
	}
	if manifest == nil {
		return "", "", "", errors.New("archive must contain SKILL.md")
	}
	manifestPath, err := cleanArchivePath(manifest.Name)
	if err != nil {
		return "", "", "", err
	}
	prefix := path.Dir(manifestPath)
	if prefix == "." {
		prefix = ""
	}
	staged, err := os.MkdirTemp(parent, ".upload-*")
	if err != nil {
		return "", "", "", fmt.Errorf("%w: create staging directory", errSkillStorage)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(staged)
		}
	}()

	root, err := os.OpenRoot(staged)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: open staging directory", errSkillStorage)
	}
	defer root.Close()

	var total int64
	for _, file := range reader.File {
		clean, err := cleanArchivePath(file.Name)
		if err != nil {
			return "", "", "", err
		}
		if prefix != "" && clean != prefix && !strings.HasPrefix(clean, prefix+"/") {
			continue
		}
		rel := clean
		if prefix != "" {
			rel = strings.TrimPrefix(strings.TrimPrefix(clean, prefix), "/")
		}
		if rel == "" {
			continue
		}
		if file.FileInfo().IsDir() {
			if err := root.MkdirAll(rel, 0o700); err != nil {
				if isArchivePathConflict(err) {
					return "", "", "", fmt.Errorf("archive path %q conflicts with a file", rel)
				}
				return "", "", "", fmt.Errorf("%w: create directory", errSkillStorage)
			}
			continue
		}
		if parent := path.Dir(rel); parent != "." {
			if err := root.MkdirAll(parent, 0o700); err != nil {
				if isArchivePathConflict(err) {
					return "", "", "", fmt.Errorf("archive path %q conflicts with a file", rel)
				}
				return "", "", "", fmt.Errorf("%w: create directory", errSkillStorage)
			}
		}
		size := int64(file.UncompressedSize64)
		if size > maxSkillFileBytes {
			return "", "", "", fmt.Errorf("archive file %q is too large", rel)
		}
		total += size
		if total > maxSkillExtractedBytes {
			return "", "", "", errors.New("archive expands beyond the size limit")
		}
		stream, err := file.Open()
		if err != nil {
			return "", "", "", fmt.Errorf("archive entry %q could not be read", rel)
		}
		if err := writeArchiveFile(root, rel, stream, size); err != nil {
			stream.Close()
			return "", "", "", err
		}
		stream.Close()
	}
	name, description, err := readSkillManifest(staged)
	if err != nil {
		return "", "", "", err
	}
	ok = true
	return staged, name, description, nil
}

// copySkillDir stages a copy of an existing server-side skill directory so
// the loader can scan it from the managed root. It returns the staged
// directory and the manifest parsed from the copy.
func copySkillDir(source, parent string) (string, string, string, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return "", "", "", errors.New("dir must be an existing directory")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", "", "", errors.New("dir must be a real directory")
	}
	staged, err := os.MkdirTemp(parent, ".register-*")
	if err != nil {
		return "", "", "", fmt.Errorf("%w: create staging directory", errSkillStorage)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(staged)
		}
	}()
	entries := 0
	var total int64
	err = filepath.WalkDir(source, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("dir could not be read")
		}
		entries++
		if entries > maxSkillArchiveEntries {
			return errors.New("dir contains too many entries")
		}
		rel, err := filepath.Rel(source, current)
		if err != nil || rel == "." {
			return nil
		}
		target := filepath.Join(staged, rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("dir must not contain symlinks")
		}
		if entry.IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("%w: create directory", errSkillStorage)
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("dir must contain only regular files")
		}
		info, err := entry.Info()
		if err != nil {
			return errors.New("dir could not be read")
		}
		if info.Size() > maxSkillFileBytes {
			return fmt.Errorf("file %q is too large", rel)
		}
		total += info.Size()
		if total > maxSkillExtractedBytes {
			return errors.New("dir exceeds the size limit")
		}
		file, err := os.Open(current)
		if err != nil {
			return errors.New("dir could not be read")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			_ = file.Close()
			return fmt.Errorf("%w: create directory", errSkillStorage)
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_ = file.Close()
			return fmt.Errorf("%w: create file", errSkillStorage)
		}
		source := &trackedReader{reader: file}
		written, copyErr := io.Copy(out, io.LimitReader(source, maxSkillFileBytes+1))
		closeOutErr := out.Close()
		closeInErr := file.Close()
		if copyErr != nil {
			if source.readErr != nil {
				return errors.New("dir could not be read")
			}
			return fmt.Errorf("%w: write file", errSkillStorage)
		}
		if closeInErr != nil {
			return errors.New("dir could not be read")
		}
		if written != info.Size() {
			return fmt.Errorf("file %q changed while copying", rel)
		}
		if closeOutErr != nil {
			return fmt.Errorf("%w: close file", errSkillStorage)
		}
		return nil
	})
	if err != nil {
		return "", "", "", err
	}
	name, description, err := readSkillManifest(staged)
	if err != nil {
		return "", "", "", err
	}
	ok = true
	return staged, name, description, nil
}

// trackedReader records an error returned by the archive stream so a failed
// copy can distinguish a corrupt entry from a local write failure.
type trackedReader struct {
	reader  io.Reader
	readErr error
}

func (t *trackedReader) Read(p []byte) (int, error) {
	n, err := t.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		t.readErr = err
	}
	return n, err
}

// isArchivePathConflict reports whether an archive entry could not be created
// because the archive maps a file and a directory to the same path. That is
// invalid input, not a local storage failure.
func isArchivePathConflict(err error) bool {
	return errors.Is(err, fs.ErrExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EISDIR)
}

func writeArchiveFile(root *os.Root, rel string, stream io.Reader, size int64) error {
	out, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if isArchivePathConflict(err) {
			return fmt.Errorf("archive path %q conflicts with a directory", rel)
		}
		return fmt.Errorf("%w: create file", errSkillStorage)
	}
	source := &trackedReader{reader: stream}
	written, copyErr := io.Copy(out, io.LimitReader(source, size+1))
	closeErr := out.Close()
	if copyErr != nil {
		if source.readErr != nil {
			return fmt.Errorf("archive entry %q could not be read", rel)
		}
		return fmt.Errorf("%w: write archive entry", errSkillStorage)
	}
	if closeErr != nil {
		return fmt.Errorf("%w: write archive entry", errSkillStorage)
	}
	if written != size {
		return fmt.Errorf("archive entry %q size does not match its header", rel)
	}
	return nil
}

// cleanArchivePath rejects absolute paths, backslashes, drive letters, NUL
// bytes, overlong names and any parent traversal component.
func cleanArchivePath(name string) (string, error) {
	if name == "" || len(name) > maxSkillArchivePath || strings.ContainsRune(name, 0) ||
		strings.HasPrefix(name, "/") || strings.Contains(name, "\\") ||
		strings.Contains(name, ":") {
		return "", errors.New("archive contains an unsafe path")
	}
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("archive contains an unsafe path")
	}
	for _, part := range strings.Split(cleaned, "/") {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("archive contains an unsafe path")
		}
	}
	return cleaned, nil
}

func managedSkillsRoot(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return ""
	}
	return filepath.Join(workspace, "skills")
}

func projectSkillsContainer(workspace string, projectID int64) string {
	return filepath.Join(managedSkillsRoot(workspace), fmt.Sprintf("project-%d", projectID))
}

// ListBlockBindings lists bindings for the scope selector.
func (s *ExtensionService) ListBlockBindings(ctx context.Context, scope string, projectID *int64) ([]BlockBinding, error) {
	rows, err := s.store.ListBlockBindings(ctx, scope, projectID)
	if err != nil {
		return nil, extensionStoreError(err)
	}
	out := make([]BlockBinding, 0, len(rows))
	for _, row := range rows {
		out = append(out, blockBindingFromStore(row))
	}
	return out, nil
}

// ListBlockBindingsPage returns one bounded page for the scope selector.
func (s *ExtensionService) ListBlockBindingsPage(ctx context.Context, scope string, projectID *int64, cursor string, limit int) ([]BlockBinding, string, error) {
	rows, next, err := s.store.ListBlockBindingsPage(ctx, scope, projectID, cursor, limit)
	if err != nil {
		return nil, "", extensionStoreError(err)
	}
	out := make([]BlockBinding, 0, len(rows))
	for _, row := range rows {
		out = append(out, blockBindingFromStore(row))
	}
	return out, next, nil
}

// UpsertBlockBinding creates or replaces one binding.
func (s *ExtensionService) UpsertBlockBinding(ctx context.Context, in BlockBindingInput) (*BlockBinding, error) {
	scope, projectID, err := extensionScope(in.Scope, in.ProjectID)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lockScope(ctx, scope, projectID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	row, err := s.store.UpsertBlockBinding(ctx, store.BlockBinding{
		Scope:     scope,
		ProjectID: projectID,
		Block:     strings.TrimSpace(in.Block),
		MCPTools:  in.MCPTools,
		Skills:    in.Skills,
	})
	if err != nil {
		return nil, extensionStoreError(err)
	}
	binding := blockBindingFromStore(*row)
	s.invalidateExtensions(scope, projectID)
	return &binding, nil
}

// ReplaceBlockBinding creates or replaces one binding only when the stored
// content still matches expectedRevision. HTTP writes use this method; the
// unconditional UpsertBlockBinding stays for trusted internal writers.
func (s *ExtensionService) ReplaceBlockBinding(ctx context.Context, in BlockBindingInput,
	expectedRevision string) (*BlockBinding, error) {
	if !store.ValidBlockBindingRevision(expectedRevision) {
		return nil, validationError("expected_revision must be absent or a lowercase SHA-256 revision",
			Detail{Field: "expected_revision", Message: "must be absent or a lowercase SHA-256 revision"})
	}
	scope, projectID, err := extensionScope(in.Scope, in.ProjectID)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lockScope(ctx, scope, projectID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	row, err := s.store.UpsertBlockBindingIfRevision(ctx, store.BlockBinding{
		Scope:     scope,
		ProjectID: projectID,
		Block:     strings.TrimSpace(in.Block),
		MCPTools:  in.MCPTools,
		Skills:    in.Skills,
	}, expectedRevision)
	if err != nil {
		if errors.Is(err, store.ErrStaleRevision) {
			return nil, conflictError("block binding changed since it was loaded; reload and retry")
		}
		return nil, extensionStoreError(err)
	}
	binding := blockBindingFromStore(*row)
	s.invalidateExtensions(scope, projectID)
	return &binding, nil
}

// TestMCPServer starts a short-lived manager from the stored configuration.
// It never reuses the runtime manager: a project server can share a name with
// a platform server and carry different configuration.
func (s *ExtensionService) TestMCPServer(ctx context.Context, server MCPServer) (MCPTestResult, error) {
	unlock, err := s.lockScope(ctx, server.Scope, server.ProjectID)
	if err != nil {
		return MCPTestResult{}, err
	}
	defer unlock()
	if server.Scope == store.ScopeProject {
		row, err := s.store.GetMCPServer(ctx, server.ID)
		if err != nil {
			return MCPTestResult{}, extensionStoreError(err)
		}
		if row == nil || !sameExtensionScope(server.Scope, server.ProjectID, row.Scope, row.ProjectID) {
			return MCPTestResult{}, notFoundError("extension not found")
		}
		server = mcpServerFromStore(*row)
	}
	name := strings.TrimSpace(server.Name)
	if name == "" {
		return MCPTestResult{}, validationError("name is required", Detail{Field: "name", Message: "name is required"})
	}
	return s.testMCPConnection(ctx, name, server.Scope, server.ProjectID, server.Config)
}

// TestMCPDraft tests a submitted configuration without writing it. When ID is
// set the stored row supplies the scope, the name fallback and any masked
// secret values; nothing is persisted either way.
func (s *ExtensionService) TestMCPDraft(ctx context.Context, in MCPDraftInput) (MCPTestResult, error) {
	prepared, err := s.prepareMCPDraft(ctx, in)
	if err != nil {
		return MCPTestResult{}, err
	}
	defer prepared.unlock()
	return s.testMCPConnection(ctx, prepared.input.Name, prepared.input.Scope, prepared.input.ProjectID, prepared.input.Config)
}

// ValidateMCPDraft applies the same name/config and stored-row resolution as
// TestMCPDraft without connecting or taking a test slot. The HTTP layer calls
// it before the concurrency limit so invalid input answers 400, not 429.
func (s *ExtensionService) ValidateMCPDraft(ctx context.Context, in MCPDraftInput) error {
	prepared, err := s.prepareMCPDraft(ctx, in)
	if err != nil {
		return err
	}
	prepared.unlock()
	return nil
}

// preparedMCPDraft is a validated draft plus the scope lock that keeps the
// referenced stored server stable while the connection test runs.
type preparedMCPDraft struct {
	input  MCPDraftInput
	unlock func()
}

// prepareMCPDraft resolves an id-backed draft and validates its name/config.
// It holds the scope lock until the caller runs the test or releases it.
func (s *ExtensionService) prepareMCPDraft(ctx context.Context, in MCPDraftInput) (*preparedMCPDraft, error) {
	id := strings.TrimSpace(in.ID)
	scope := strings.TrimSpace(in.Scope)
	projectID := in.ProjectID
	name := strings.TrimSpace(in.Name)
	config := append(json.RawMessage(nil), in.Config...)
	unlock := func() {}
	fail := func(err error) (*preparedMCPDraft, error) {
		unlock()
		return nil, err
	}
	if id != "" {
		current, storedUnlock, err := s.lockMCPServer(ctx, id)
		if err != nil {
			return nil, err
		}
		unlock = storedUnlock
		scope, projectID = current.Scope, current.ProjectID
		if name == "" {
			name = strings.TrimSpace(current.Name)
		}
		if len(config) == 0 {
			config = append(json.RawMessage(nil), current.Config...)
		} else {
			restored, err := restoreMaskedMCPConfigRequired(config, current.Config)
			if err != nil {
				return fail(err)
			}
			config = restored
		}
	}
	if len(config) == 0 {
		return fail(validationError("config is required", Detail{Field: "config", Message: "config is required"}))
	}
	if err := validateMCPServerName(name); err != nil {
		return fail(err)
	}
	if err := guardMaskedConfig(config); err != nil {
		return fail(err)
	}
	if err := s.validateMCPConfig(config); err != nil {
		return fail(err)
	}
	if id == "" {
		var err error
		scope, projectID, err = extensionScope(scope, projectID)
		if err != nil {
			return fail(err)
		}
		scopeUnlock, err := s.lockScope(ctx, scope, projectID)
		if err != nil {
			return fail(err)
		}
		unlock = scopeUnlock
	}
	return &preparedMCPDraft{
		input:  MCPDraftInput{ID: id, Scope: scope, ProjectID: projectID, Name: name, Config: config},
		unlock: unlock,
	}, nil
}

// testMCPConnection is the shared draft/saved test path. Errors are classified
// with a fixed category and never carry configuration or upstream text.
func (s *ExtensionService) testMCPConnection(ctx context.Context, name, scope string, projectID *int64,
	rawConfig json.RawMessage) (result MCPTestResult, err error) {
	started := time.Now()
	config, err := mcp.ParseServer(rawConfig, s.deniedEnv...)
	if err != nil {
		return MCPTestResult{}, mcp.NewTestError(mcp.TestCategoryInvalidConfig, err)
	}
	workspace := s.workspace
	if scope == store.ScopeProject {
		if projectID == nil || *projectID <= 0 {
			return MCPTestResult{}, validationError("project_id is required for project scope", Detail{Field: "project_id", Message: "must be positive"})
		}
		workspace = filepath.Join(workspace, fmt.Sprintf("%d", *projectID))
	}
	manager, err := s.newMCPTest(mcp.Options{
		Platform:  mcp.Config{MCPServers: map[string]mcp.ServerConfig{name: config}},
		Workspace: workspace,
		DeniedEnv: s.deniedEnv,
		Now:       s.now,
	})
	if err != nil {
		return MCPTestResult{}, mcp.NewTestError(mcp.TestCategoryInvalidConfig, err)
	}
	defer func() {
		if closeErr := manager.Close(); closeErr != nil {
			s.logger.Warn("mcp test manager close failed")
			cleanupErr := mcp.NewTestError(mcp.TestCategoryCleanupFailed, mcp.ErrCleanupFailed)
			if err == nil {
				result = MCPTestResult{}
				err = cleanupErr
			} else {
				err = errors.Join(err, cleanupErr)
			}
		}
	}()
	infos, err := manager.ListTools(ctx, name)
	if err != nil {
		return MCPTestResult{}, err
	}
	return mcpTestResult(infos, time.Since(started).Milliseconds()), nil
}

// mcpTestResult converts the advertised tools into the bounded test response.
func mcpTestResult(infos []mcp.ToolInfo, latencyMS int64) MCPTestResult {
	shown := infos
	if len(shown) > maxTestTools {
		shown = shown[:maxTestTools]
	}
	tools := make([]MCPTool, 0, len(shown))
	for _, info := range shown {
		tools = append(tools, MCPTool{
			Name:        info.Name,
			Description: info.Description,
			ReadOnly:    info.ReadOnly,
			Destructive: info.Destructive,
			Bindable:    info.Bindable,
			Annotated:   info.Annotated,
		})
	}
	return MCPTestResult{
		LatencyMS: latencyMS,
		ToolCount: len(infos),
		Truncated: len(infos) > len(shown),
		Tools:     tools,
	}
}

// TestProvider sends one minimal request to a configured provider. Errors are
// classified without upstream URLs, bodies, or credentials.
// ProviderInfos returns the registry metadata, including startup egress
// availability. A provider client that does not expose Infos yields nil.
func (s *ExtensionService) ProviderInfos() []llm.ProviderInfo {
	if s == nil || s.clients == nil {
		return nil
	}
	if provider, ok := s.clients.(interface{ Infos() []llm.ProviderInfo }); ok {
		return provider.Infos()
	}
	return nil
}

func (s *ExtensionService) TestProvider(ctx context.Context, name string) (ProviderTestResult, error) {
	if s.clients == nil {
		return ProviderTestResult{}, errors.New("app: provider clients are not configured")
	}
	client, err := s.clients.Client(name)
	if err != nil {
		return ProviderTestResult{}, errors.New("app: provider is not configured")
	}
	return ProbeProvider(ctx, client)
}

// ProbeProvider sends the minimal one-token request used by provider tests.
// Errors are classified without upstream URLs, bodies, or credentials.
func ProbeProvider(ctx context.Context, client llm.Client) (ProviderTestResult, error) {
	started := time.Now()
	response, err := client.Generate(ctx, &llm.Request{
		Messages: []llm.Message{{
			Role:  llm.RoleUser,
			Parts: []llm.Part{{Type: llm.PartText, Text: "ping"}},
		}},
		MaxTokens: 1,
	})
	latency := time.Since(started).Milliseconds()
	if err != nil {
		return ProviderTestResult{}, providerTestError(err)
	}
	model := ""
	if response != nil {
		model = response.Model
	}
	if model == "" {
		model = client.Info().Model
	}
	return ProviderTestResult{Model: model, LatencyMS: latency}, nil
}

func providerTestError(err error) error {
	var classified *llm.Error
	if errors.As(err, &classified) {
		if classified.Kind == llm.ErrEgress {
			return &llm.Error{Kind: llm.ErrEgress, Err: llm.ErrEgressDenied}
		}
		return fmt.Errorf("provider test failed: %s", classified.Kind)
	}
	return errors.New("provider test failed")
}

func extensionScope(scope string, projectID *int64) (string, *int64, error) {
	scope = strings.TrimSpace(scope)
	if err := store.ValidateExtensionScope(scope, projectID, true); err != nil {
		return "", nil, validationError(err.Error(), Detail{Field: "scope", Message: err.Error()})
	}
	return scope, projectID, nil
}

func validateMCPServerName(name string) error {
	if err := mcp.ValidateServerName(name); err != nil {
		return validationError("invalid server name", Detail{Field: "name", Message: "server name is invalid"})
	}
	return nil
}

// validateMCPConfig applies the runtime MCP parser without expanding ${NAME}
// templates, so stored configuration keeps its references and secret values
// are never copied into the row. The error stays generic to avoid echoing
// unknown field names or values.
func (s *ExtensionService) validateMCPConfig(raw json.RawMessage) error {
	if err := mcp.ValidateServerJSON("", raw, s.deniedEnv...); err != nil {
		return validationError("config must be a valid MCP server object", Detail{Field: "config", Message: "config must be a valid MCP server object"})
	}
	return nil
}

// restoreMaskedMCPConfig merges a submitted config over the stored one: a
// masked value keeps the stored secret, and a masked key with no stored
// counterpart is dropped rather than written back.
func restoreMaskedMCPConfig(submitted, current json.RawMessage) json.RawMessage {
	var nextObject map[string]any
	if err := json.Unmarshal(submitted, &nextObject); err != nil {
		return submitted
	}
	var currentObject map[string]any
	_ = json.Unmarshal(current, &currentObject)
	if env, ok := nextObject["env"].(map[string]any); ok {
		stored, _ := currentObject["env"].(map[string]any)
		restoreMCPMap(env, stored, func(string) bool { return true })
	}
	if headers, ok := nextObject["headers"].(map[string]any); ok {
		stored, _ := currentObject["headers"].(map[string]any)
		restoreMCPMap(headers, stored, extensionSensitiveKeyRE.MatchString)
	}
	restored, err := json.Marshal(nextObject)
	if err != nil {
		return submitted
	}
	return restored
}

func restoreMCPMap(next, stored map[string]any, sensitive func(string) bool) {
	for key, value := range next {
		if value != maskedExtensionSecret || !sensitive(key) {
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

// restoreMaskedMCPConfigRequired is the draft-test variant: a masked value
// with no stored counterpart is rejected instead of silently dropped, because
// the caller asked to test a secret that does not exist. Invalid JSON is left
// for the config parser to reject.
func restoreMaskedMCPConfigRequired(submitted, current json.RawMessage) (json.RawMessage, error) {
	var nextObject map[string]any
	if err := json.Unmarshal(submitted, &nextObject); err != nil {
		return submitted, nil
	}
	var currentObject map[string]any
	_ = json.Unmarshal(current, &currentObject)
	if env, ok := nextObject["env"].(map[string]any); ok {
		stored, _ := currentObject["env"].(map[string]any)
		if err := restoreMCPMapRequired(env, stored, func(string) bool { return true }); err != nil {
			return nil, err
		}
	}
	if headers, ok := nextObject["headers"].(map[string]any); ok {
		stored, _ := currentObject["headers"].(map[string]any)
		if err := restoreMCPMapRequired(headers, stored, extensionSensitiveKeyRE.MatchString); err != nil {
			return nil, err
		}
	}
	restored, err := json.Marshal(nextObject)
	if err != nil {
		return submitted, nil
	}
	return restored, nil
}

func restoreMCPMapRequired(next, stored map[string]any, sensitive func(string) bool) error {
	for key, value := range next {
		if value != maskedExtensionSecret || !sensitive(key) {
			continue
		}
		if original, ok := stored[key]; ok {
			next[key] = original
			continue
		}
		return validationError("masked secret placeholder has no stored value",
			Detail{Field: "config", Message: "masked secret placeholder has no stored value"})
	}
	return nil
}

// guardMaskedConfig refuses to persist the display placeholder the admin API
// writes on read. Updates restore stored secrets before saving; create and
// import keep the guard as a second line of defence.
func guardMaskedConfig(raw json.RawMessage) error {
	var object map[string]any
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	if env, ok := object["env"].(map[string]any); ok {
		for _, value := range env {
			if value == maskedExtensionSecret {
				return maskedConfigError()
			}
		}
	}
	if headers, ok := object["headers"].(map[string]any); ok {
		for key, value := range headers {
			if value == maskedExtensionSecret && extensionSensitiveKeyRE.MatchString(key) {
				return maskedConfigError()
			}
		}
	}
	return nil
}

func maskedConfigError() error {
	return validationError("masked secret placeholder must be restored before saving",
		Detail{Field: "config", Message: "masked secret placeholder must be restored before saving"})
}

func extensionStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return notFoundError("extension not found")
	case errors.Is(err, store.ErrConflict):
		return conflictError("extension name already exists in this scope")
	default:
		return err
	}
}

func mcpServerFromStore(row store.MCPServer) MCPServer {
	return MCPServer{
		ID:        row.ID,
		Scope:     row.Scope,
		ProjectID: row.ProjectID,
		Name:      row.Name,
		Config:    append(json.RawMessage(nil), row.Config...),
		Enabled:   row.Enabled,
		Builtin:   row.Builtin != "",
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}

// errBuiltinMCPServerManaged rejects changes the release owns.
const errBuiltinMCPServerManaged = "built-in MCP server configuration follows the platform release and servers cannot be disabled"

func skillFromStore(row store.Skill) Skill {
	return Skill{
		ID:          row.ID,
		Scope:       row.Scope,
		ProjectID:   row.ProjectID,
		Name:        row.Name,
		Description: row.Description,
		Dir:         row.Dir,
		Enabled:     row.Enabled,
		Builtin:     row.Builtin != "",
		CreatedAt:   row.CreatedAt,
	}
}

func blockBindingFromStore(row store.BlockBinding) BlockBinding {
	return BlockBinding{
		Scope:     row.Scope,
		ProjectID: row.ProjectID,
		Block:     row.Block,
		MCPTools:  append([]string{}, row.MCPTools...),
		Skills:    append([]string{}, row.Skills...),
		Revision:  row.Revision,
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// readSkillManifest applies the runtime loader's strict parser to SKILL.md so
// upload and directory registration accept exactly what the loader will load.
// Errors carry the parser's reason but never the directory or file content.
func readSkillManifest(dir string) (string, string, error) {
	path := filepath.Join(dir, "SKILL.md")
	info, err := os.Lstat(path)
	if err != nil {
		return "", "", errors.New("dir must contain SKILL.md")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", "", errors.New("SKILL.md must be a regular file, not a symlink")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", "", errors.New("SKILL.md could not be read")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, skill.MaxManifestBytes+1))
	if err != nil {
		return "", "", errors.New("SKILL.md could not be read")
	}
	if len(raw) > skill.MaxManifestBytes {
		return "", "", errors.New("SKILL.md is too large")
	}
	manifest, err := skill.ParseManifest(raw)
	if err != nil {
		return "", "", manifestValidationError(err)
	}
	return manifest.Name, manifest.Description, nil
}

// manifestValidationError drops the internal package prefix while keeping the
// stable validation reason.
func manifestValidationError(err error) error {
	return errors.New(strings.TrimPrefix(err.Error(), skill.ErrInvalidSkill.Error()+": "))
}
