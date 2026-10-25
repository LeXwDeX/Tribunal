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
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/mcp"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/review"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
)

const maskedExtensionSecret = "****"

const (
	maxSkillArchiveBytes   = 8 << 20
	maxSkillExtractedBytes = 16 << 20
	maxSkillFileBytes      = 4 << 20
	maxSkillArchiveEntries = 512
)

var (
	extensionSensitiveKeyRE = regexp.MustCompile(`(?i)(authorization|token|secret|password|api[-_]?key|cookie|credential)`)
	skillDirNameRE          = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// MCPServer is the app view of one stored MCP server.
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

// MCPTool is one tool advertised by a test connection.
type MCPTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
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
	ID          string    `json:"id"`
	Scope       string    `json:"scope"`
	ProjectID   *int64    `json:"project_id,omitempty"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Dir         string    `json:"dir,omitempty"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
}

// SkillUpload carries a validated archive. Uploads are extracted below the
// managed skills root, and the resulting directory is persisted with the row.
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
	Workspace string
	DeniedEnv []string
	Logger    *slog.Logger
	Now       func() time.Time
}

// ExtensionService persists MCP servers, skills and block bindings, tests MCP
// connections, and probes configured providers.
type ExtensionService struct {
	store     *sqlite.Store
	mcp       *mcp.Manager
	clients   review.ClientProvider
	workspace string
	deniedEnv []string
	logger    *slog.Logger
	now       func() time.Time
}

// NewExtensionService validates dependencies and builds the service.
func NewExtensionService(cfg ExtensionServiceConfig) (*ExtensionService, error) {
	if cfg.Store == nil {
		return nil, errors.New("app: extension store is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	workspace := strings.TrimSpace(cfg.Workspace)
	if workspace != "" {
		absolute, err := filepath.Abs(workspace)
		if err != nil {
			return nil, fmt.Errorf("app: resolve extension workspace: %w", err)
		}
		if filepath.Dir(absolute) == absolute {
			return nil, errors.New("app: extension workspace must not be a filesystem root")
		}
		workspace = absolute
	}
	return &ExtensionService{
		store:     cfg.Store,
		mcp:       cfg.MCP,
		clients:   cfg.Clients,
		workspace: workspace,
		deniedEnv: append([]string(nil), cfg.DeniedEnv...),
		logger:    logger,
		now:       now,
	}, nil
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
	if name == "" {
		return nil, validationError("name is required", Detail{Field: "name", Message: "name is required"})
	}
	if err := validateMCPServerName(name); err != nil {
		return nil, err
	}
	if err := guardMaskedConfig(in.Config); err != nil {
		return nil, err
	}
	if err := s.validateMCPConfig(in.Config); err != nil {
		return nil, err
	}
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
	return &server, nil
}

// UpdateMCPServer replaces only the provided fields.
func (s *ExtensionService) UpdateMCPServer(ctx context.Context, id string, update MCPServerUpdate) (*MCPServer, error) {
	if update.Name != nil {
		name := strings.TrimSpace(*update.Name)
		if err := validateMCPServerName(name); err != nil {
			return nil, err
		}
		update.Name = &name
	}
	if update.Config != nil {
		if err := guardMaskedConfig(update.Config); err != nil {
			return nil, err
		}
		if err := s.validateMCPConfig(update.Config); err != nil {
			return nil, err
		}
	}
	row, err := s.store.UpdateMCPServer(ctx, id, store.MCPServerUpdate{
		Name:    update.Name,
		Config:  update.Config,
		Enabled: update.Enabled,
	})
	if err != nil {
		return nil, extensionStoreError(err)
	}
	server := mcpServerFromStore(*row)
	return &server, nil
}

// DeleteMCPServer removes one server.
func (s *ExtensionService) DeleteMCPServer(ctx context.Context, id string) error {
	if err := s.store.DeleteMCPServer(ctx, id); err != nil {
		return extensionStoreError(err)
	}
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
	for _, name := range names {
		if err := validateMCPServerName(name); err != nil {
			return nil, err
		}
		config := in.Servers[name]
		if err := guardMaskedConfig(config); err != nil {
			return nil, err
		}
		if err := s.validateMCPConfig(config); err != nil {
			return nil, err
		}
	}
	existing, err := s.store.ListMCPServers(ctx, scope, projectID)
	if err != nil {
		return nil, extensionStoreError(err)
	}
	byName := make(map[string]store.MCPServer, len(existing))
	for _, row := range existing {
		byName[row.Name] = row
	}
	out := make([]MCPServer, 0, len(in.Servers))
	for _, name := range names {
		config := in.Servers[name]
		if current, ok := byName[name]; ok {
			enabled := current.Enabled
			row, err := s.store.UpdateMCPServer(ctx, current.ID, store.MCPServerUpdate{
				Config:  config,
				Enabled: &enabled,
			})
			if err != nil {
				return nil, extensionStoreError(err)
			}
			out = append(out, mcpServerFromStore(*row))
			continue
		}
		row, err := s.store.CreateMCPServer(ctx, store.MCPServer{
			Scope:     scope,
			ProjectID: projectID,
			Name:      name,
			Config:    config,
			Enabled:   true,
		})
		if err != nil {
			return nil, extensionStoreError(err)
		}
		out = append(out, mcpServerFromStore(*row))
	}
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
	scopeRoot, err := s.skillScopeRoot(scope, projectID)
	if err != nil {
		return nil, err
	}
	extracted, name, description, err := extractSkillArchive(in.Data, scopeRoot)
	if err != nil {
		return nil, validationError(err.Error(), Detail{Field: "file", Message: err.Error()})
	}
	return s.persistSkillDir(ctx, scope, projectID, name, description, scopeRoot, extracted)
}

// RegisterSkillDir copies the registered directory into the managed skills
// root so the loader can scan it and later deletes cannot remove the source.
func (s *ExtensionService) RegisterSkillDir(ctx context.Context, in SkillDirInput) (*Skill, error) {
	scope, projectID, err := extensionScope(in.Scope, in.ProjectID)
	if err != nil {
		return nil, err
	}
	dir := strings.TrimSpace(in.Dir)
	if dir == "" || !filepath.IsAbs(dir) {
		return nil, validationError("dir must be an absolute server path", Detail{Field: "dir", Message: "dir must be an absolute server path"})
	}
	name, description, err := readSkillManifest(dir)
	if err != nil {
		return nil, validationError(err.Error(), Detail{Field: "dir", Message: err.Error()})
	}
	scopeRoot, err := s.skillScopeRoot(scope, projectID)
	if err != nil {
		return nil, err
	}
	extracted, err := copySkillDir(dir, scopeRoot)
	if err != nil {
		return nil, validationError(err.Error(), Detail{Field: "dir", Message: err.Error()})
	}
	return s.persistSkillDir(ctx, scope, projectID, name, description, scopeRoot, extracted)
}

// persistSkillDir renames the staged directory to <scope>/<id>/skill, stores
// the row, and records the final path.
func (s *ExtensionService) persistSkillDir(ctx context.Context, scope string, projectID *int64, name, description, scopeRoot, staged string) (*Skill, error) {
	if !skillDirNameRE.MatchString(name) {
		_ = os.RemoveAll(staged)
		return nil, validationError("invalid skill name", Detail{Field: "name", Message: "must match " + skillDirNameRE.String()})
	}
	row, err := s.store.CreateSkill(ctx, store.Skill{
		Scope:       scope,
		ProjectID:   projectID,
		Name:        name,
		Description: description,
		Enabled:     true,
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
	return &skill, nil
}

// DeleteSkill removes one skill row and its managed directory. Directories
// outside the managed root (legacy rows) are left untouched.
func (s *ExtensionService) DeleteSkill(ctx context.Context, id string) error {
	row, err := s.store.GetSkill(ctx, id)
	if err != nil {
		return extensionStoreError(err)
	}
	if err := s.store.DeleteSkill(ctx, id); err != nil {
		return extensionStoreError(err)
	}
	if root, managed := s.managedSkillContainer(row); managed {
		if err := os.RemoveAll(root); err != nil {
			s.logger.Warn("skill directory cleanup failed", "skill_id", id)
		}
	}
	return nil
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
		return "", "", "", fmt.Errorf("app: create skill directory: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(staged)
		}
	}()

	root, err := os.OpenRoot(staged)
	if err != nil {
		return "", "", "", fmt.Errorf("app: open skill directory: %w", err)
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
				return "", "", "", fmt.Errorf("app: create skill directory: %w", err)
			}
			continue
		}
		if parent := path.Dir(rel); parent != "." {
			if err := root.MkdirAll(parent, 0o700); err != nil {
				return "", "", "", fmt.Errorf("app: create skill directory: %w", err)
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
// the loader can scan it from the managed root.
func copySkillDir(source, parent string) (string, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return "", errors.New("dir must be an existing directory")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("dir must be a real directory")
	}
	staged, err := os.MkdirTemp(parent, ".register-*")
	if err != nil {
		return "", fmt.Errorf("app: create skill directory: %w", err)
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
			return os.MkdirAll(target, 0o700)
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
		defer file.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("app: create skill directory: %w", err)
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("app: create skill file: %w", err)
		}
		_, copyErr := io.Copy(out, io.LimitReader(file, maxSkillFileBytes+1))
		closeErr := out.Close()
		if copyErr != nil {
			return errors.New("dir could not be read")
		}
		if closeErr != nil {
			return fmt.Errorf("app: create skill file: %w", closeErr)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if _, _, err := readSkillManifest(staged); err != nil {
		return "", err
	}
	ok = true
	return staged, nil
}

func writeArchiveFile(root *os.Root, rel string, stream io.Reader, size int64) error {
	out, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("app: create skill file: %w", err)
	}
	written, copyErr := io.Copy(out, io.LimitReader(stream, size+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		return fmt.Errorf("archive entry %q could not be written", rel)
	}
	if written != size {
		return fmt.Errorf("archive entry %q size does not match its header", rel)
	}
	return nil
}

// cleanArchivePath rejects absolute paths, backslashes, drive letters and any
// parent traversal component.
func cleanArchivePath(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") ||
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
	return &binding, nil
}

// TestMCPServer starts a short-lived manager from the submitted configuration.
// It never reuses the runtime manager: a project server can share a name with
// a platform server and carry different configuration.
func (s *ExtensionService) TestMCPServer(ctx context.Context, server MCPServer) ([]MCPTool, error) {
	name := strings.TrimSpace(server.Name)
	if name == "" {
		return nil, validationError("name is required", Detail{Field: "name", Message: "name is required"})
	}
	config, err := mcp.ParseServer(server.Config, s.deniedEnv...)
	if err != nil {
		return nil, fmt.Errorf("app: MCP server test failed: %w", err)
	}
	workspace := s.workspace
	if strings.TrimSpace(workspace) == "" {
		dir, err := os.MkdirTemp("", "mcp-test-")
		if err != nil {
			return nil, errors.New("app: cannot prepare MCP test workspace")
		}
		defer os.RemoveAll(dir)
		workspace = dir
	}
	if server.Scope == store.ScopeProject {
		if server.ProjectID == nil || *server.ProjectID <= 0 {
			return nil, validationError("project_id is required for project scope", Detail{Field: "project_id", Message: "must be positive"})
		}
		workspace = filepath.Join(workspace, fmt.Sprintf("%d", *server.ProjectID))
	}
	manager, err := mcp.New(mcp.Options{
		Platform:  mcp.Config{MCPServers: map[string]mcp.ServerConfig{name: config}},
		Workspace: workspace,
		DeniedEnv: s.deniedEnv,
		Now:       s.now,
	})
	if err != nil {
		return nil, fmt.Errorf("app: MCP server test failed: %w", err)
	}
	defer func() {
		if err := manager.Close(); err != nil {
			s.logger.Warn("mcp test manager close failed")
		}
	}()
	infos, err := manager.ListTools(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("app: MCP server test failed: %w", err)
	}
	tools := make([]MCPTool, 0, len(infos))
	for _, info := range infos {
		tools = append(tools, MCPTool{Name: info.Name, Description: info.Description})
	}
	return tools, nil
}

// TestProvider sends one minimal request to a configured provider. Errors are
// classified without upstream URLs, bodies, or credentials.
func (s *ExtensionService) TestProvider(ctx context.Context, name string) (ProviderTestResult, error) {
	if s.clients == nil {
		return ProviderTestResult{}, errors.New("app: provider clients are not configured")
	}
	client, err := s.clients.Client(name)
	if err != nil {
		return ProviderTestResult{}, errors.New("app: provider is not configured")
	}
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

// guardMaskedConfig refuses to persist the display placeholder the admin API
// writes on read. The server restores stored secrets before calling this
// service; the guard is a second line of defence.
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
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}

func skillFromStore(row store.Skill) Skill {
	return Skill{
		ID:          row.ID,
		Scope:       row.Scope,
		ProjectID:   row.ProjectID,
		Name:        row.Name,
		Description: row.Description,
		Dir:         row.Dir,
		Enabled:     row.Enabled,
		CreatedAt:   row.CreatedAt,
	}
}

func blockBindingFromStore(row store.BlockBinding) BlockBinding {
	return BlockBinding{
		Scope:     row.Scope,
		ProjectID: row.ProjectID,
		Block:     row.Block,
		MCPTools:  append([]string(nil), row.MCPTools...),
		Skills:    append([]string(nil), row.Skills...),
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

// readSkillManifest reads the small frontmatter subset SKILL.md uses. The
// directory itself is validated by the HTTP layer; this repeats the file
// checks so the service is safe when called directly.
func readSkillManifest(dir string) (string, string, error) {
	file, err := os.Open(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return "", "", errors.New("dir must contain SKILL.md")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 64<<10))
	if err != nil {
		return "", "", errors.New("SKILL.md could not be read")
	}
	text := strings.ReplaceAll(strings.TrimPrefix(string(raw), "\ufeff"), "\r\n", "\n")
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
		value = strings.Trim(strings.TrimSpace(value), `"'`)
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
	if name == "" || description == "" {
		return "", "", errors.New("SKILL.md must define name and description")
	}
	return name, description, nil
}
