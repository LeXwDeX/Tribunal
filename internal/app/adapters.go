package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/skill"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// GitLabClient is the GitLab surface consumed by review orchestration and
// publication. Every method carries the project token so the client stays
// stateless per project.
type GitLabClient interface {
	CurrentUser(ctx context.Context, token string) (*gitlab.User, error)
	GetMergeRequest(ctx context.Context, token string, projectID, iid int64) (*gitlab.MergeRequest, error)
	LatestMRVersion(ctx context.Context, token string, projectID, iid int64) (*gitlab.Version, error)
	ListMRDiffs(ctx context.Context, token string, projectID, iid int64) ([]gitlab.FileDiff, error)
	ListMRPipelines(ctx context.Context, token string, projectID, iid int64) ([]gitlab.Pipeline, error)
	ListPipelineJobs(ctx context.Context, token string, projectID, pipelineID int64) ([]gitlab.Job, error)
	ListClosingIssues(ctx context.Context, token string, projectID, iid int64) ([]gitlab.Issue, error)
	GetRawFile(ctx context.Context, token string, projectID int64, path, ref string) ([]byte, error)
	ListMRNotes(ctx context.Context, token string, projectID, iid int64) ([]gitlab.Note, error)
	CreateMRNote(ctx context.Context, token string, projectID, iid int64, body string) (*gitlab.Note, error)
	UpdateMRNote(ctx context.Context, token string, projectID, iid, noteID int64, body string) error
	ListDiscussions(ctx context.Context, token string, projectID, iid int64) ([]gitlab.Discussion, error)
	CreateDiscussion(ctx context.Context, token string, projectID, iid int64, d gitlab.NewDiscussion) (*gitlab.Discussion, error)
	SetCommitStatus(ctx context.Context, token string, projectID int64, sha string, st gitlab.CommitStatus) error
	ListNoteAwards(ctx context.Context, token string, projectID, iid, noteID int64) ([]gitlab.Award, error)
	AddNoteAward(ctx context.Context, token string, projectID, iid, noteID int64, name string) (*gitlab.Award, error)
	DeleteNoteAward(ctx context.Context, token string, projectID, iid, noteID, awardID int64) error
}

// GitLabService is the complete GitLab surface used by the app layer:
// administration, command checks, review orchestration, and publication.
type GitLabService interface {
	GitLabClient
	gitlab.API
}

var _ GitLabService = (*gitlab.Client)(nil)

// GitOps is the git surface used by workspace and review orchestration. The
// production implementation wraps repo.Git; tests inject a local fake.
type GitOps interface {
	EnsureMirror(ctx context.Context, dir, remoteURL string, a repo.Auth) error
	Fetch(ctx context.Context, mirror string, a repo.Auth, refspecs ...string) error
	Resolve(ctx context.Context, dir, ref string) (string, error)
	AddWorktree(ctx context.Context, mirror, dir, sha string) error
	RemoveWorktree(ctx context.Context, mirror, dir string) error
	Checkout(ctx context.Context, dir, sha string) error
	ChangedFiles(ctx context.Context, dir, from, to string) ([]string, error)
	IsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error)
}

func fetchProjectRef(ctx context.Context, ws *workspace.Workspace, git GitOps, remote string, auth repo.Auth, refspec, ref string) (string, error) {
	unlock, err := ws.LockMirror(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := git.EnsureMirror(ctx, ws.MirrorDir(), remote, auth); err != nil {
		return "", err
	}
	if err := git.Fetch(ctx, ws.MirrorDir(), auth, refspec); err != nil {
		return "", err
	}
	return git.Resolve(ctx, ws.MirrorDir(), ref)
}

// mcpToolProvider exposes only the explicitly bound MCP tools for one block.
// A nil manager means MCP is not configured.
type mcpToolProvider struct {
	manager *mcp.Manager
	blocks  map[string]config.BlockBinding
}

// NewMCPToolProvider adapts an MCP manager to review.ToolProvider.
func NewMCPToolProvider(manager *mcp.Manager, blocks map[string]config.BlockBinding) review.ToolProvider {
	return &mcpToolProvider{manager: manager, blocks: blocks}
}

func (p *mcpToolProvider) Tools(ctx context.Context, block string) ([]agent.Tool, error) {
	if p == nil || p.manager == nil || len(p.blocks) == 0 {
		return nil, nil
	}
	// Tools of a disabled or deleted server stay in the binding but are not
	// offered: the administrator turned the server off on purpose.
	var allowed []string
	for _, tool := range p.blocks[block].MCPTools {
		if server, _, ok := strings.Cut(strings.TrimPrefix(tool, "mcp__"), "__"); !ok || p.manager.HasServer(server) {
			allowed = append(allowed, tool)
		}
	}
	if len(allowed) == 0 {
		return nil, nil
	}
	set, err := p.manager.Tools(ctx, allowed)
	if err != nil {
		return nil, fmt.Errorf("app: bind MCP tools for block %q: %w", block, err)
	}
	// Tools that are bound but not advertised, or skipped as unsafe, leave
	// the review without part of its evidence: report them as gaps.
	if len(set.Warnings) > 0 {
		return nil, &review.PartialToolsError{
			Tools: set.Tools,
			Gaps:  append([]string(nil), set.Warnings...),
		}
	}
	return set.Tools, nil
}

func (p *mcpToolProvider) BoundToolNames(_ context.Context, block string) ([]string, error) {
	if p == nil || p.manager == nil {
		return nil, nil
	}
	var allowed []string
	for _, tool := range p.blocks[block].MCPTools {
		if server, _, ok := strings.Cut(strings.TrimPrefix(tool, "mcp__"), "__"); !ok || p.manager.HasServer(server) {
			allowed = append(allowed, tool)
		}
	}
	return allowed, nil
}

// skillProvider binds the skills named by a block and exposes load_skill.
type skillProvider struct {
	loader *skill.Loader
}

// NewSkillProvider adapts a skill loader to review.SkillProvider.
func NewSkillProvider(loader *skill.Loader) review.SkillProvider {
	return &skillProvider{loader: loader}
}

func (p *skillProvider) BindSkills(ctx context.Context, names []string) (review.SkillBinding, error) {
	if p == nil || p.loader == nil {
		return review.SkillBinding{}, errors.New("app: skill loader is not configured")
	}
	binding, err := p.loader.Bind(names)
	if err != nil {
		return review.SkillBinding{}, fmt.Errorf("app: bind skills: %w", err)
	}
	catalogue := make(map[string]skill.Skill)
	for _, item := range p.loader.Catalogue() {
		catalogue[item.Name] = item
	}
	out := review.SkillBinding{Roots: binding.Roots(), Tool: binding.Tool()}
	for _, name := range binding.Names() {
		if item, ok := catalogue[name]; ok {
			out.Skills = append(out.Skills, review.Skill{Name: item.Name, Description: item.Description})
		}
	}
	return out, nil
}

// engineSnapshot adapts Engine.Snapshot to review.WorkflowSnapshot.
func engineSnapshot(engine *dag.Engine) review.WorkflowSnapshot {
	return func(ctx context.Context, workflowID string) (dag.WorkflowView, error) {
		if engine == nil {
			return dag.WorkflowView{}, errors.New("app: workflow engine is not configured")
		}
		return engine.Snapshot(ctx, workflowID)
	}
}

// locateFunc adapts a DiffIndex to review.LocateFunc.
func locateFunc(index *gitlab.DiffIndex) review.LocateFunc {
	if index == nil {
		return func(string, int, review.Side) bool { return false }
	}
	return func(path string, line int, side review.Side) bool {
		mapped := gitlab.SideNew
		if side == review.SideOld {
			mapped = gitlab.SideOld
		}
		_, ok := index.LocateUnambiguous(path, line, mapped)
		return ok
	}
}

// newCostFunc converts provider usage to micro-dollars using the configured
// per-million-token prices. Unknown providers cost zero.
func newCostFunc(providers map[string]config.ProviderConfig) review.CostFunc {
	return func(provider string, usage llm.Usage) int64 {
		cfg, ok := providers[provider]
		if !ok || cfg.Price == nil {
			return 0
		}
		price := cfg.Price
		micros := float64(usage.InputTokens)*price.InputPerMTok +
			float64(usage.OutputTokens)*price.OutputPerMTok
		if micros <= 0 {
			return 0
		}
		return int64(math.Round(micros))
	}
}

// executorRegistry dispatches engine calls to the per-workflow Executor that
// owns the sanitized dossier and diff index.
type executorRegistry struct {
	mu            sync.RWMutex
	exec          map[string]*review.Executor
	owned         map[string]struct{}
	projectLookup func(context.Context, string) (int64, error)
}

func newExecutorRegistry() *executorRegistry {
	return &executorRegistry{
		exec:  make(map[string]*review.Executor),
		owned: make(map[string]struct{}),
	}
}

// Acquire claims exclusive application-level ownership of one workflow run.
// Ownership is keyed by the job ID so startup recovery and a lease-reclaimed
// handler contend for the same key. The winner alone may prepare the worktree,
// register an executor, or remove either of them.
func (r *executorRegistry) Acquire(workflowID string) bool {
	if r == nil || workflowID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, held := r.owned[workflowID]; held {
		return false
	}
	r.owned[workflowID] = struct{}{}
	return true
}

// Release drops ownership acquired by Acquire. Only the owner calls it.
func (r *executorRegistry) Release(workflowID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.owned, workflowID)
	r.mu.Unlock()
}

// SetProjectLookup installs the workflow-to-project resolver used to attach a
// project ID to node contexts. A nil lookup is allowed only for standalone test
// registries; production must set it so every workflow resolves its project.
func (r *executorRegistry) SetProjectLookup(lookup func(context.Context, string) (int64, error)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.projectLookup = lookup
	r.mu.Unlock()
}

type projectIDContextKey struct{}

func projectIDFromContext(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(projectIDContextKey{}).(int64)
	return id, ok && id > 0
}

func (r *executorRegistry) withProject(ctx context.Context, workflowID string) (context.Context, error) {
	if r == nil || workflowID == "" {
		return ctx, nil
	}
	r.mu.RLock()
	lookup := r.projectLookup
	r.mu.RUnlock()
	if lookup == nil {
		return ctx, nil
	}
	id, err := lookup(ctx, workflowID)
	if err != nil {
		return nil, fmt.Errorf("app: resolve workflow %q project: %w", workflowID, err)
	}
	if id <= 0 {
		return nil, fmt.Errorf("app: workflow %q has no project", workflowID)
	}
	return context.WithValue(ctx, projectIDContextKey{}, id), nil
}

// Register attaches an executor to a workflow ID. It replaces a previous
// registration so recovery can rebuild the executor.
func (r *executorRegistry) Register(workflowID string, executor *review.Executor) {
	if r == nil || workflowID == "" || executor == nil {
		return
	}
	r.mu.Lock()
	r.exec[workflowID] = executor
	r.mu.Unlock()
}

// Unregister drops the executor for a terminal workflow.
func (r *executorRegistry) Unregister(workflowID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.exec, workflowID)
	r.mu.Unlock()
}

func (r *executorRegistry) lookup(workflowID string) (*review.Executor, error) {
	if r == nil {
		return nil, errors.New("app: executor registry is not configured")
	}
	r.mu.RLock()
	executor := r.exec[workflowID]
	r.mu.RUnlock()
	if executor == nil {
		return nil, fmt.Errorf("app: no executor registered for workflow %q", workflowID)
	}
	return executor, nil
}

// NodeMetadata implements dag.NodeMetadataProvider. The engine asks before
// node.started; an unregistered or legacy executor stays unattributed.
func (r *executorRegistry) NodeMetadata(in dag.NodeInput) dag.NodeMetadata {
	executor, err := r.lookup(in.WorkflowID)
	if err != nil {
		return dag.NodeMetadata{}
	}
	return executor.NodeMetadata(in)
}

// Execute implements dag.NodeExecutor.
func (r *executorRegistry) Execute(ctx context.Context, in dag.NodeInput) (dag.NodeOutput, error) {
	executor, err := r.lookup(in.WorkflowID)
	if err != nil {
		return dag.NodeOutput{}, err
	}
	projectCtx, err := r.withProject(ctx, in.WorkflowID)
	if err != nil {
		return dag.NodeOutput{}, err
	}
	return executor.Execute(projectCtx, in)
}

// Expand implements dag.Expander.
func (r *executorRegistry) Expand(ctx context.Context, wf dag.WorkflowView, from dag.NodeSpec, out json.RawMessage) (dag.Extension, error) {
	executor, err := r.lookup(wf.ID)
	if err != nil {
		return dag.Extension{}, err
	}
	projectCtx, err := r.withProject(ctx, wf.ID)
	if err != nil {
		return dag.Extension{}, err
	}
	return executor.Expand(projectCtx, wf, from, out)
}

var _ dag.NodeExecutor = (*executorRegistry)(nil)
var _ dag.NodeMetadataProvider = (*executorRegistry)(nil)
var _ dag.Expander = (*executorRegistry)(nil)

// trimProjectWebURL normalizes a GitLab project URL to a clone URL.
func trimProjectWebURL(webURL string) string {
	value := strings.TrimRight(strings.TrimSpace(webURL), "/")
	if value == "" {
		return ""
	}
	if strings.HasSuffix(value, ".git") {
		return value
	}
	return value + ".git"
}

// currentProjectRemote resolves a project by its registered numeric ID before
// sending its token to a Git remote. The stored URL is only an origin anchor.
func currentProjectRemote(ctx context.Context, client GitLabClient, project *store.Project, token string) (string, error) {
	lookup, ok := client.(interface {
		GetProject(context.Context, string, string) (*gitlab.Project, error)
	})
	if !ok || project == nil || project.ID <= 0 {
		return "", errors.New("app: project identity unavailable")
	}
	current, err := lookup.GetProject(ctx, token, fmt.Sprint(project.ID))
	if err != nil {
		return "", err
	}
	if current == nil || current.ID != project.ID {
		return "", errors.New("app: project identity mismatch")
	}
	ref, err := gitlab.ParseProjectRef(current.PathWithNamespace)
	if err != nil || ref.ID != 0 || ref.Path != current.PathWithNamespace {
		return "", errors.New("app: project path invalid")
	}
	old, err := url.Parse(project.WebURL)
	if err != nil || strings.TrimSpace(project.WebURL) != project.WebURL || strings.ContainsAny(project.WebURL, " \\%#") || hasControlChar(project.WebURL) ||
		old.Opaque != "" || old.Host == "" || old.Hostname() == "" || old.User != nil || old.RawQuery != "" || old.ForceQuery || old.Fragment != "" || (old.Scheme != "http" && old.Scheme != "https") {
		return "", errors.New("app: project origin invalid")
	}
	base := ""
	if project.PathWithNamespace != "" {
		suffix := "/" + project.PathWithNamespace
		if !strings.HasSuffix(strings.TrimRight(old.Path, "/"), suffix) {
			return "", errors.New("app: project origin invalid")
		}
		base = strings.TrimSuffix(strings.TrimRight(old.Path, "/"), suffix)
	}
	origin, err := parseGitLabOrigin(old.Scheme + "://" + old.Host + base)
	if err != nil || origin == nil {
		return "", errors.New("app: project origin invalid")
	}
	oldPath, err := origin.projectPath(project.WebURL, false)
	if err != nil || (project.PathWithNamespace != "" && oldPath != project.PathWithNamespace) {
		return "", errors.New("app: project origin invalid")
	}
	webPath, err := origin.projectPath(current.WebURL, false)
	if err != nil || webPath != current.PathWithNamespace {
		return "", errors.New("app: project web URL invalid")
	}
	remote := current.HTTPURLToRepo
	if remote == "" {
		remote = trimProjectWebURL(current.WebURL)
	}
	clonePath, err := origin.projectPath(remote, true)
	if err != nil || clonePath != current.PathWithNamespace {
		return "", errors.New("app: project clone URL invalid")
	}
	return remote, nil
}

// ProjectExtensionResolver builds and caches project-scoped review inputs:
// merged block bindings, a project-aware MCP manager and a project skill
// loader. Each project manager is rebuilt from the effective platform and
// project configuration, including when the project adds no MCP servers, so
// merged bindings and the project workspace apply. The cache is built on first
// use and requires a restart to observe extension changes.
type ProjectExtensionResolver struct {
	store      *sqlite.Store
	blocks     map[string]config.BlockBinding
	mcpConfig  mcp.Config
	skillDirs  []string
	workspace  string
	deniedEnv  []string
	now        func() time.Time
	logger     *slog.Logger
	baseSkills review.SkillProvider

	mu     sync.Mutex
	sets   map[int64]*projectExtensionSet
	closed bool
}

// ProjectExtensionResolverConfig configures ProjectExtensionResolver. The base
// skill provider is platform-only and is never closed here.
type ProjectExtensionResolverConfig struct {
	Store      *sqlite.Store
	Blocks     map[string]config.BlockBinding
	MCP        mcp.Config
	SkillDirs  []string
	Workspace  string
	DeniedEnv  []string
	Now        func() time.Time
	Logger     *slog.Logger
	BaseSkills review.SkillProvider
}

type projectExtensionSet struct {
	blocks    map[string]config.BlockBinding
	tools     review.ToolProvider
	skills    review.SkillProvider
	mcp       *mcp.Manager
	closer    io.Closer
	loader    *skill.Loader
	mcpConfig mcp.Config
	closing   bool
	closeErr  error
}

func (set *projectExtensionSet) close() error {
	if set.closer == nil {
		return nil
	}
	return set.closer.Close()
}

// NewProjectExtensionResolver validates dependencies and copies base state.
func NewProjectExtensionResolver(cfg ProjectExtensionResolverConfig) (*ProjectExtensionResolver, error) {
	if cfg.Store == nil {
		return nil, errors.New("app: project extension store is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &ProjectExtensionResolver{
		store:      cfg.Store,
		blocks:     cloneBlockBindings(cfg.Blocks),
		mcpConfig:  mcp.Merge(cfg.MCP, mcp.Config{}),
		skillDirs:  append([]string(nil), cfg.SkillDirs...),
		workspace:  cfg.Workspace,
		deniedEnv:  append([]string(nil), cfg.DeniedEnv...),
		now:        now,
		logger:     logger,
		baseSkills: cfg.BaseSkills,
		sets:       make(map[int64]*projectExtensionSet),
	}, nil
}

var errProjectExtensionResolverClosed = errors.New("app: project extension resolver is closed")
var errProjectExtensionsUnavailable = errors.New("app: project extensions are unavailable")

// CloseProject closes cached inputs under the caller's project workspace write
// lock. ForProject callers must hold the same workspace lock for reading.
func (r *ProjectExtensionResolver) CloseProject(projectID int64) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errProjectExtensionResolverClosed
	}
	set := r.sets[projectID]
	if set == nil {
		r.mu.Unlock()
		return nil
	}
	if set.closing || set.closeErr != nil {
		r.mu.Unlock()
		return errProjectExtensionsUnavailable
	}
	set.closing = true
	r.mu.Unlock()

	err := set.close()
	r.mu.Lock()
	set.closing = false
	set.closeErr = err
	if err == nil && r.sets[projectID] == set {
		delete(r.sets, projectID)
	}
	r.mu.Unlock()
	if err != nil {
		return errProjectExtensionsUnavailable
	}
	return nil
}

// ForProject returns the project-scoped inputs, building them on first use.
// A read error is returned instead of silently running with platform-only
// configuration. Callers must hold the shared project workspace lock.
func (r *ProjectExtensionResolver) ForProject(ctx context.Context, projectID int64) (*projectExtensionSet, error) {
	if projectID <= 0 {
		return nil, errors.New("app: project id is required")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errProjectExtensionResolverClosed
	}
	if existing := r.sets[projectID]; existing != nil {
		unavailable := existing.closing || existing.closeErr != nil
		r.mu.Unlock()
		if unavailable {
			return nil, errProjectExtensionsUnavailable
		}
		return existing, nil
	}
	r.mu.Unlock()

	set, err := r.build(ctx, projectID)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = set.close()
		return nil, errProjectExtensionResolverClosed
	}
	if existing := r.sets[projectID]; existing != nil {
		unavailable := existing.closing || existing.closeErr != nil
		r.mu.Unlock()
		_ = set.close()
		if unavailable {
			return nil, errProjectExtensionsUnavailable
		}
		return existing, nil
	}
	r.sets[projectID] = set
	r.mu.Unlock()
	return set, nil
}

// Close releases project MCP managers. Platform managers are owned elsewhere.
func (r *ProjectExtensionResolver) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	sets := r.sets
	r.sets = make(map[int64]*projectExtensionSet)
	r.mu.Unlock()
	var errs []error
	for _, set := range sets {
		errs = append(errs, set.close())
	}
	return errors.Join(errs...)
}

func (r *ProjectExtensionResolver) build(ctx context.Context, projectID int64) (*projectExtensionSet, error) {
	projectRows, err := r.store.ListBlockBindings(ctx, store.ScopeProject, &projectID)
	if err != nil {
		return nil, fmt.Errorf("app: load project block bindings: %w", err)
	}
	blocks := cloneBlockBindings(r.blocks)
	if blocks == nil {
		blocks = make(map[string]config.BlockBinding)
	}
	for _, row := range projectRows {
		if strings.TrimSpace(row.Block) == "" {
			continue
		}
		blocks[row.Block] = config.BlockBinding{
			MCPTools: append([]string(nil), row.MCPTools...),
			Skills:   append([]string(nil), row.Skills...),
		}
	}

	platformConfig := mcp.Merge(r.mcpConfig, mcp.Config{})
	platformServers, err := r.store.ListMCPServers(ctx, store.ScopePlatform, nil)
	if err != nil {
		return nil, fmt.Errorf("app: load platform MCP servers: %w", err)
	}
	for _, row := range platformServers {
		if !row.Enabled {
			continue
		}
		server, err := mcp.ParseServer(row.Config, r.deniedEnv...)
		if err != nil {
			return nil, fmt.Errorf("app: parse platform MCP server %q: %w", row.Name, err)
		}
		platformConfig.MCPServers[row.Name] = server
	}
	servers, err := r.store.ListMCPServers(ctx, store.ScopeProject, &projectID)
	if err != nil {
		return nil, fmt.Errorf("app: load project MCP servers: %w", err)
	}
	projectMCP := mcp.Config{MCPServers: make(map[string]mcp.ServerConfig, len(servers))}
	for _, row := range servers {
		if !row.Enabled {
			continue
		}
		server, err := mcp.ParseServer(row.Config, r.deniedEnv...)
		if err != nil {
			return nil, fmt.Errorf("app: parse project MCP server %q: %w", row.Name, err)
		}
		projectMCP.MCPServers[row.Name] = server
	}
	// Build a project manager even when the project adds no servers: the
	// merged bindings must apply and stdio servers must run in the project
	// workspace rather than the platform workspace.
	set := &projectExtensionSet{
		blocks:    blocks,
		skills:    r.baseSkills,
		mcpConfig: mcp.Merge(platformConfig, projectMCP),
	}
	manager, err := mcp.New(mcp.Options{
		Platform:  platformConfig,
		Project:   projectMCP,
		Workspace: r.projectWorkspace(projectID),
		DeniedEnv: r.deniedEnv,
		Now:       r.now,
	})
	if err != nil {
		return nil, fmt.Errorf("app: build project MCP manager: %w", err)
	}
	set.mcp = manager
	set.closer = manager
	set.tools = NewMCPToolProvider(manager, blocks)

	// Platform skills are re-read on every project build so skills uploaded
	// after startup are visible to the next review. The runtime base loader is
	// a startup snapshot and cannot see them.
	dirs := append([]string(nil), r.skillDirs...)
	seen := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		seen[dir] = true
	}
	platformSkills, err := r.store.ListSkills(ctx, store.ScopePlatform, nil)
	if err != nil {
		if set.mcp != nil {
			_ = set.mcp.Close()
		}
		return nil, fmt.Errorf("app: load platform skills: %w", err)
	}
	dirs, added := appendSkillContainers(dirs, seen, platformSkills)
	projectSkills, err := r.store.ListSkills(ctx, store.ScopeProject, &projectID)
	if err != nil {
		if set.mcp != nil {
			_ = set.mcp.Close()
		}
		return nil, fmt.Errorf("app: load project skills: %w", err)
	}
	dirs, projectAdded := appendSkillContainers(dirs, seen, projectSkills)
	added = added || projectAdded
	if added {
		loader, err := skill.New(skill.Options{PlatformDirs: dirs})
		if err != nil {
			if set.mcp != nil {
				_ = set.mcp.Close()
			}
			return nil, fmt.Errorf("app: load project skills: %w", err)
		}
		set.loader = loader
		set.skills = NewSkillProvider(loader)
	}
	set.skills = withoutDisabledSkills(set.skills, disabledSkillNames(platformSkills, projectSkills))
	return set, nil
}

// disabledSkillNames returns names that only disabled rows carry. A name that
// also has an enabled row in a visible scope stays bindable.
func disabledSkillNames(groups ...[]store.Skill) map[string]bool {
	disabled := make(map[string]bool)
	enabled := make(map[string]bool)
	for _, rows := range groups {
		for _, row := range rows {
			if row.Enabled {
				enabled[row.Name] = true
			} else {
				disabled[row.Name] = true
			}
		}
	}
	for name := range enabled {
		delete(disabled, name)
	}
	return disabled
}

// withoutDisabledSkills drops disabled names before binding, so a block that
// still names a disabled skill runs without it instead of failing.
func withoutDisabledSkills(base review.SkillProvider, disabled map[string]bool) review.SkillProvider {
	if len(disabled) == 0 {
		return base
	}
	return &enabledSkillProvider{base: base, disabled: disabled}
}

type enabledSkillProvider struct {
	base     review.SkillProvider
	disabled map[string]bool
}

func (p *enabledSkillProvider) BindSkills(ctx context.Context, names []string) (review.SkillBinding, error) {
	kept := make([]string, 0, len(names))
	for _, name := range names {
		if !p.disabled[strings.TrimSpace(name)] {
			kept = append(kept, name)
		}
	}
	if len(kept) == 0 || p.base == nil {
		return review.SkillBinding{}, nil
	}
	return p.base.BindSkills(ctx, kept)
}

func appendSkillContainers(dirs []string, seen map[string]bool, rows []store.Skill) ([]string, bool) {
	added := false
	for _, row := range rows {
		if !row.Enabled || strings.TrimSpace(row.Dir) == "" {
			continue
		}
		container := filepath.Dir(row.Dir)
		if seen[container] {
			continue
		}
		seen[container] = true
		dirs = append(dirs, container)
		added = true
	}
	return dirs, added
}

func (r *ProjectExtensionResolver) projectWorkspace(projectID int64) string {
	if strings.TrimSpace(r.workspace) == "" {
		return r.workspace
	}
	return filepath.Join(r.workspace, fmt.Sprintf("%d", projectID))
}

// loadStoredPlatformExtensions merges database-backed platform extensions over
// file configuration. Database rows win on a name conflict.
func loadStoredPlatformExtensions(ctx context.Context, st *sqlite.Store, blocks map[string]config.BlockBinding,
	mcpConfig mcp.Config, skillDirs []string, denied ...string) (map[string]config.BlockBinding, mcp.Config, []string, error) {
	if st == nil {
		return cloneBlockBindings(blocks), mcpConfig, append([]string(nil), skillDirs...), nil
	}
	mergedBlocks := cloneBlockBindings(blocks)
	if mergedBlocks == nil {
		mergedBlocks = make(map[string]config.BlockBinding)
	}
	rows, err := st.ListBlockBindings(ctx, store.ScopePlatform, nil)
	if err != nil {
		return nil, mcp.Config{}, nil, fmt.Errorf("app: load platform block bindings: %w", err)
	}
	for _, row := range rows {
		if strings.TrimSpace(row.Block) == "" {
			continue
		}
		mergedBlocks[row.Block] = config.BlockBinding{
			MCPTools: append([]string(nil), row.MCPTools...),
			Skills:   append([]string(nil), row.Skills...),
		}
	}

	servers, err := st.ListMCPServers(ctx, store.ScopePlatform, nil)
	if err != nil {
		return nil, mcp.Config{}, nil, fmt.Errorf("app: load platform MCP servers: %w", err)
	}
	if mcpConfig.MCPServers == nil {
		mcpConfig.MCPServers = make(map[string]mcp.ServerConfig)
	}
	for _, row := range servers {
		if !row.Enabled {
			continue
		}
		server, err := mcp.ParseServer(row.Config, denied...)
		if err != nil {
			return nil, mcp.Config{}, nil, fmt.Errorf("app: parse platform MCP server %q: %w", row.Name, err)
		}
		mcpConfig.MCPServers[row.Name] = server
	}

	storedSkills, err := st.ListSkills(ctx, store.ScopePlatform, nil)
	if err != nil {
		return nil, mcp.Config{}, nil, fmt.Errorf("app: load platform skills: %w", err)
	}
	mergedDirs := append([]string(nil), skillDirs...)
	seen := make(map[string]bool, len(storedSkills))
	for _, row := range storedSkills {
		if !row.Enabled || strings.TrimSpace(row.Dir) == "" {
			continue
		}
		container := filepath.Dir(row.Dir)
		if seen[container] {
			continue
		}
		seen[container] = true
		mergedDirs = append(mergedDirs, container)
	}
	return mergedBlocks, mcpConfig, mergedDirs, nil
}

func cloneBlockBindings(in map[string]config.BlockBinding) map[string]config.BlockBinding {
	if in == nil {
		return nil
	}
	out := make(map[string]config.BlockBinding, len(in))
	for name, binding := range in {
		out[name] = config.BlockBinding{
			MCPTools: append([]string(nil), binding.MCPTools...),
			Skills:   append([]string(nil), binding.Skills...),
		}
	}
	return out
}

const (
	projectMCPStub       = "__project_mcp__"
	projectSkillStubBase = "__project_skill__:"
)

// projectBindingStubs makes the static executor bindings admit project-scoped
// overrides. The executor only observes non-empty lists; the project-aware
// providers replace the stub with the merged project binding before use.
func projectBindingStubs(blocks map[string]config.BlockBinding) map[string]config.BlockBinding {
	out := cloneBlockBindings(blocks)
	if out == nil {
		out = make(map[string]config.BlockBinding)
	}
	for _, block := range config.BindableBlocks {
		out[block] = config.BlockBinding{
			MCPTools: []string{projectMCPStub},
			Skills:   []string{projectSkillStubBase + block},
		}
	}
	for block, binding := range out {
		if len(binding.MCPTools) == 0 {
			binding.MCPTools = []string{projectMCPStub}
		}
		if len(binding.Skills) == 0 {
			binding.Skills = []string{projectSkillStubBase + block}
		}
		out[block] = binding
	}
	return out
}

// projectAwareToolProvider resolves the project from the node context and
// delegates to that project's merged tool provider.
type projectAwareToolProvider struct {
	resolver *ProjectExtensionResolver
	base     review.ToolProvider
}

func (p *projectAwareToolProvider) Tools(ctx context.Context, block string) ([]agent.Tool, error) {
	if p == nil {
		return nil, nil
	}
	if id, ok := projectIDFromContext(ctx); ok {
		if p.resolver == nil {
			return nil, errors.New("app: project extension resolver is not configured")
		}
		set, err := p.resolver.ForProject(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("app: resolve project tools: %w", err)
		}
		if set == nil || set.tools == nil {
			return nil, nil
		}
		return set.tools.Tools(ctx, block)
	}
	if p.base == nil {
		return nil, nil
	}
	return p.base.Tools(ctx, block)
}

func (p *projectAwareToolProvider) BoundToolNames(ctx context.Context, block string) ([]string, error) {
	if p == nil {
		return nil, nil
	}
	if id, ok := projectIDFromContext(ctx); ok {
		if p.resolver == nil {
			return nil, errors.New("app: project extension resolver is not configured")
		}
		set, err := p.resolver.ForProject(ctx, id)
		if err != nil {
			return nil, err
		}
		if set == nil || set.tools == nil {
			return nil, nil
		}
		provider, ok := set.tools.(review.ToolBindingProvider)
		if !ok {
			return nil, errors.New("app: MCP binding metadata unavailable")
		}
		return provider.BoundToolNames(ctx, block)
	}
	if p.base == nil {
		return nil, nil
	}
	provider, ok := p.base.(review.ToolBindingProvider)
	if !ok {
		return nil, errors.New("app: MCP binding metadata unavailable")
	}
	return provider.BoundToolNames(ctx, block)
}

// projectAwareSkillProvider resolves the project from the node context and
// replaces the stub names with that project's merged skill list.
type projectAwareSkillProvider struct {
	resolver *ProjectExtensionResolver
	base     review.SkillProvider
	blocks   map[string]config.BlockBinding
}

func (p *projectAwareSkillProvider) BindSkills(ctx context.Context, names []string) (review.SkillBinding, error) {
	if p == nil {
		return review.SkillBinding{}, nil
	}
	block := ""
	if len(names) == 1 && strings.HasPrefix(names[0], projectSkillStubBase) {
		block = strings.TrimPrefix(names[0], projectSkillStubBase)
	}
	if id, ok := projectIDFromContext(ctx); ok {
		if p.resolver == nil {
			return review.SkillBinding{}, errors.New("app: project extension resolver is not configured")
		}
		set, err := p.resolver.ForProject(ctx, id)
		if err != nil {
			return review.SkillBinding{}, fmt.Errorf("app: resolve project skills: %w", err)
		}
		if block != "" {
			names = set.blocks[block].Skills
		}
		if block != "" && len(names) == 0 {
			return review.SkillBinding{}, nil
		}
		if set.skills == nil {
			return review.SkillBinding{}, nil
		}
		return set.skills.BindSkills(ctx, names)
	}
	if block != "" {
		names = p.blocks[block].Skills
		if len(names) == 0 {
			return review.SkillBinding{}, nil
		}
	}
	if p.base == nil {
		return review.SkillBinding{}, nil
	}
	return p.base.BindSkills(ctx, names)
}
