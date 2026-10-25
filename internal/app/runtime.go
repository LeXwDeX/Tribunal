package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/skill"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
	"github.com/LeXwDeX/tribunal/pkg/dag"
	"github.com/LeXwDeX/tribunal/prompts"
)

// repoGit adapts repo.Git to the app git surface.
type repoGit struct {
	git *repo.Git
}

func (r repoGit) EnsureMirror(ctx context.Context, dir, remoteURL string, a repo.Auth) error {
	return r.git.EnsureMirror(ctx, dir, remoteURL, a)
}

func (r repoGit) Fetch(ctx context.Context, mirror string, a repo.Auth, refspecs ...string) error {
	return r.git.Fetch(ctx, mirror, a, refspecs...)
}

func (r repoGit) Resolve(ctx context.Context, dir, ref string) (string, error) {
	return r.git.Resolve(ctx, dir, ref)
}

func (r repoGit) AddWorktree(ctx context.Context, mirror, dir, sha string) error {
	return r.git.AddWorktree(ctx, mirror, dir, sha)
}

func (r repoGit) RemoveWorktree(ctx context.Context, mirror, dir string) error {
	return r.git.RemoveWorktree(ctx, mirror, dir)
}

func (r repoGit) Checkout(ctx context.Context, dir, sha string) error {
	return r.git.Checkout(ctx, dir, sha)
}

func (r repoGit) ChangedFiles(ctx context.Context, dir, from, to string) ([]string, error) {
	return r.git.ChangedFiles(ctx, dir, from, to)
}

func (r repoGit) IsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	return r.git.IsAncestor(ctx, dir, ancestor, descendant)
}

// RuntimeDeps allows tests and the main package to replace external systems.
// Required fields are Store, Cipher, and GitLab.
type RuntimeDeps struct {
	Store     *sqlite.Store
	Cipher    *secret.Cipher
	GitLab    GitLabService
	LLM       review.ClientProvider
	MCP       *mcp.Manager
	Skills    review.SkillProvider
	Workspace *workspace.Manager
	Git       GitOps
	Prompts   prompts.Bundle
	Logger    *slog.Logger
	Now       func() time.Time
}

// Runtime is the composition root below main: store-backed dependencies,
// services, the job handler set, recovery, and the feedback ticker.
type Runtime struct {
	cfg         *config.Config
	store       *sqlite.Store
	cipher      *secret.Cipher
	gitlab      GitLabService
	workspace   *workspace.Manager
	git         GitOps
	engine      *dag.Engine
	executors   *executorRegistry
	projects    *ProjectService
	webhooks    *WebhookService
	commands    *CommandWorker
	reviews     *ReviewService
	workspaces  *WorkspaceService
	feedback    *FeedbackService
	retention   *RetentionService
	runCleanup  *RunCleanupService
	publisher   *Publisher
	worker      *ReviewWorker
	lifecycle   *Lifecycle
	mcp         *mcp.Manager
	projectExts *ProjectExtensionResolver
	clients     review.ClientProvider
	skills      *skill.Loader
	logger      *slog.Logger
	now         func() time.Time

	startOnce sync.Once
	startStop context.CancelFunc
	startDone chan struct{}
}

// NewRuntime validates configuration and constructs every application service.
func NewRuntime(ctx context.Context, cfg *config.Config, deps RuntimeDeps) (*Runtime, error) {
	if cfg == nil {
		return nil, errors.New("app: runtime configuration is required")
	}
	if deps.Store == nil || deps.Cipher == nil || deps.GitLab == nil {
		return nil, errors.New("app: runtime store, cipher, and GitLab client are required")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	bundle := deps.Prompts
	if bundle.Files == nil {
		loaded, err := prompts.Load(cfg.PromptsDir)
		if err != nil {
			return nil, fmt.Errorf("app: load prompts: %w", err)
		}
		bundle = loaded
	}
	gitBackend := deps.Git
	if gitBackend == nil {
		gitBackend = repoGit{git: &repo.Git{Bin: cfg.Workspace.GitBin}}
	}
	workspaceManager := deps.Workspace
	if workspaceManager == nil {
		workspaceManager = workspace.NewManager(cfg.Workspace.Root, &repo.Git{Bin: cfg.Workspace.GitBin})
	}
	clients := deps.LLM
	if clients == nil {
		registry, err := llm.NewRegistry(cfg.Providers, llm.WithRecorder(deps.Store))
		if err != nil {
			return nil, fmt.Errorf("app: build LLM registry: %w", err)
		}
		clients = registry
	}
	platformMCP, err := loadPlatformMCP(cfg)
	if err != nil {
		return nil, err
	}
	blocks, platformMCP, skillDirs, err := loadStoredPlatformExtensions(
		ctx, deps.Store, cfg.Blocks, platformMCP, cfg.Skills.Dirs, deniedEnvNames(cfg)...)
	if err != nil {
		return nil, err
	}
	skillProvider := deps.Skills
	var loader *skill.Loader
	if skillProvider == nil {
		created, err := skill.New(skill.Options{PlatformDirs: skillDirs})
		if err != nil {
			return nil, fmt.Errorf("app: load skills: %w", err)
		}
		loader = created
		skillProvider = NewSkillProvider(created)
	}
	mcpManager := deps.MCP
	ownedMCP := false
	if mcpManager == nil {
		created, err := mcp.New(mcp.Options{
			Platform:  platformMCP,
			Workspace: cfg.Workspace.Root,
			DeniedEnv: deniedEnvNames(cfg),
			Now:       now,
		})
		if err != nil {
			return nil, fmt.Errorf("app: build MCP manager: %w", err)
		}
		mcpManager = created
		ownedMCP = true
	}

	executors := newExecutorRegistry()
	executors.SetProjectLookup(func(ctx context.Context, workflowID string) (int64, error) {
		workflow, err := deps.Store.GetWorkflow(ctx, workflowID)
		if err != nil {
			return 0, err
		}
		if workflow == nil {
			return 0, fmt.Errorf("workflow %q is missing", workflowID)
		}
		if workflow.ProjectID <= 0 {
			return 0, fmt.Errorf("workflow %q has no project", workflowID)
		}
		return workflow.ProjectID, nil
	})
	engine := dag.NewEngine(deps.Store, executors, executors, dag.Options{
		MaxConcurrentNodes: cfg.Engine.MaxConcurrentNodes,
		DefaultNodeTimeout: cfg.Engine.DefaultNodeTimeout,
		Clock:              now,
	})
	publisher, err := NewPublisher(PublisherConfig{
		Store:          deps.Store,
		GitLab:         deps.GitLab,
		Cipher:         deps.Cipher,
		Engine:         engine,
		ReviewDefaults: cfg.ReviewDefaults,
		Profiles:       cfg.Profiles,
		PromptsHash:    bundle.Hash,
		PublicURL:      cfg.Server.PublicURL,
		Logger:         logger,
		Now:            now,
	})
	if err != nil {
		if ownedMCP {
			_ = mcpManager.Close()
		}
		return nil, err
	}
	toolProvider := NewMCPToolProvider(mcpManager, blocks)
	projectExts, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{
		Store:      deps.Store,
		Blocks:     blocks,
		MCP:        platformMCP,
		SkillDirs:  skillDirs,
		Workspace:  cfg.Workspace.Root,
		DeniedEnv:  deniedEnvNames(cfg),
		Now:        now,
		Logger:     logger,
		BaseSkills: skillProvider,
	})
	if err != nil {
		if ownedMCP {
			_ = mcpManager.Close()
		}
		return nil, err
	}
	// Workspace indexing uses the project manager built from the effective
	// platform and project configuration.
	mcpFactory := func(ctx context.Context, projectID int64) (*mcp.Manager, error) {
		set, err := projectExts.ForProject(ctx, projectID)
		if err != nil {
			return nil, err
		}
		if set != nil && set.mcp != nil {
			return set.mcp, nil
		}
		return mcpManager, nil
	}
	projectTools := &projectAwareToolProvider{resolver: projectExts, base: toolProvider}
	projectSkills := &projectAwareSkillProvider{resolver: projectExts, base: skillProvider, blocks: blocks}
	reviewBlocks := projectBindingStubs(blocks)
	statusFeed := newStatusReporter(deps.GitLab, logger)
	reviews, err := NewReviewService(ReviewServiceConfig{
		Store:            deps.Store,
		GitLab:           deps.GitLab,
		Cipher:           deps.Cipher,
		Workspace:        workspaceManager,
		Git:              gitBackend,
		Engine:           engine,
		Executors:        executors,
		Profiles:         cfg.Profiles,
		Blocks:           reviewBlocks,
		ReviewDefaults:   cfg.ReviewDefaults,
		Prompts:          bundle,
		Clients:          clients,
		Tools:            projectTools,
		Skills:           projectSkills,
		Cost:             newCostFunc(cfg.Providers),
		Publisher:        publisher,
		Status:           statusFeed,
		AllowedProviders: providerNames(cfg.Providers),
		PublicURL:        cfg.Server.PublicURL,
		Logger:           logger,
		Now:              now,
		CancelPoll:       2 * time.Second,
	})
	if err != nil {
		if ownedMCP {
			_ = mcpManager.Close()
		}
		return nil, err
	}
	workspaces, err := NewWorkspaceService(WorkspaceServiceConfig{
		Store:          deps.Store,
		Queue:          deps.Store,
		GitLab:         deps.GitLab,
		Cipher:         deps.Cipher,
		Workspace:      workspaceManager,
		Git:            gitBackend,
		Engine:         engine,
		Executors:      executors,
		Profiles:       cfg.Profiles,
		Blocks:         reviewBlocks,
		ReviewDefaults: cfg.ReviewDefaults,
		Prompts:        bundle,
		Clients:        clients,
		Tools:          projectTools,
		Skills:         projectSkills,
		ContextRefresh: cfg.Workspace.ContextRefresh,
		MCP:            mcpFactory,
		Logger:         logger,
		Now:            now,
	})
	if err != nil {
		if ownedMCP {
			_ = mcpManager.Close()
		}
		return nil, err
	}
	feedback, err := NewFeedbackService(FeedbackServiceConfig{
		Store:     deps.Store,
		GitLab:    deps.GitLab,
		Cipher:    deps.Cipher,
		Git:       gitBackend,
		Workspace: workspaceManager,
		Logger:    logger,
		Now:       now,
	})
	if err != nil {
		if ownedMCP {
			_ = mcpManager.Close()
		}
		return nil, err
	}
	retention, err := NewRetentionService(RetentionConfig{
		Store:             deps.Store,
		LLMCallRetention:  cfg.Storage.LLMCallRetention,
		DeliveryRetention: cfg.Storage.DeliveryRetention,
		JobRetention:      cfg.Storage.JobRetention,
		ContextVersions:   cfg.Storage.ContextVersions,
		Logger:            logger,
		Now:               now,
	})
	if err != nil {
		if ownedMCP {
			_ = mcpManager.Close()
		}
		return nil, err
	}
	worker, err := NewReviewWorker(reviews, workspaces)
	if err != nil {
		if ownedMCP {
			_ = mcpManager.Close()
		}
		return nil, err
	}
	runCleanup, err := NewRunCleanupService(RunCleanupConfig{
		Store:     deps.Store,
		Workspace: workspaceManager,
		Git:       gitBackend,
		After:     cfg.Workspace.RunCleanupAfter,
		Logger:    logger,
		Now:       now,
	})
	if err != nil {
		if ownedMCP {
			_ = mcpManager.Close()
		}
		return nil, err
	}
	projects, err := NewProjectService(ProjectServiceConfig{
		Store:     deps.Store,
		GitLab:    deps.GitLab,
		Cipher:    deps.Cipher,
		Profiles:  cfg.Profiles,
		PublicURL: cfg.Server.PublicURL,
		Workspace: workspaceManager.Root(),
		GitLabURL: cfg.GitLab.URL,
		ProbeRemote: func(ctx context.Context, remoteURL, token string) (string, error) {
			git := &repo.Git{Bin: cfg.Workspace.GitBin}
			return git.RemoteHEAD(ctx, remoteURL, repo.Auth{Token: token})
		},
		ValidationTimeout: cfg.GitLab.RequestTimeout,
		Logger:            logger,
		Now:               now,
	})
	if err != nil {
		if ownedMCP {
			_ = mcpManager.Close()
		}
		return nil, err
	}
	webhooks, err := NewWebhookService(WebhookServiceConfig{
		Store:          deps.Store,
		Cipher:         deps.Cipher,
		Profiles:       cfg.Profiles,
		ReviewDefaults: cfg.ReviewDefaults,
		BotUsernames:   cfg.GitLab.BotUsernames,
		Logger:         logger,
		Now:            now,
	})
	if err != nil {
		if ownedMCP {
			_ = mcpManager.Close()
		}
		return nil, err
	}
	commands, err := NewCommandWorker(CommandWorkerConfig{
		Store:        deps.Store,
		GitLab:       deps.GitLab,
		Cipher:       deps.Cipher,
		Profiles:     cfg.Profiles,
		BotUsernames: cfg.GitLab.BotUsernames,
		Logger:       logger,
	})
	if err != nil {
		if ownedMCP {
			_ = mcpManager.Close()
		}
		return nil, err
	}
	return &Runtime{
		cfg:         cfg,
		store:       deps.Store,
		cipher:      deps.Cipher,
		gitlab:      deps.GitLab,
		workspace:   workspaceManager,
		git:         gitBackend,
		engine:      engine,
		executors:   executors,
		projects:    projects,
		webhooks:    webhooks,
		commands:    commands,
		reviews:     reviews,
		workspaces:  workspaces,
		feedback:    feedback,
		retention:   retention,
		runCleanup:  runCleanup,
		publisher:   publisher,
		worker:      worker,
		lifecycle:   &Lifecycle{},
		mcp:         mcpManager,
		projectExts: projectExts,
		clients:     clients,
		skills:      loader,
		logger:      logger,
		now:         now,
		startDone:   make(chan struct{}),
	}, nil
}

func loadPlatformMCP(cfg *config.Config) (mcp.Config, error) {
	if strings.TrimSpace(cfg.MCP.Config) == "" {
		return mcp.Config{MCPServers: map[string]mcp.ServerConfig{}}, nil
	}
	loaded, err := mcp.LoadFile(cfg.MCP.Config)
	if err != nil {
		return mcp.Config{}, fmt.Errorf("app: load MCP configuration: %w", err)
	}
	return loaded, nil
}

// DeniedEnvNames exposes the environment variables that MCP servers must
// never inherit: provider keys and platform secrets.
func DeniedEnvNames(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	return deniedEnvNames(cfg)
}

func deniedEnvNames(cfg *config.Config) []string {
	var names []string
	for _, provider := range cfg.Providers {
		if provider.APIKeyEnv != "" {
			names = append(names, provider.APIKeyEnv)
		}
	}
	for _, name := range []string{cfg.Secrets.MasterKeyEnv, cfg.Auth.BootstrapTokenEnv} {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func providerNames(providers map[string]config.ProviderConfig) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Store returns the shared SQLite store.
func (r *Runtime) Store() *sqlite.Store { return r.store }

// Projects returns the project administration service.
func (r *Runtime) Projects() *ProjectService { return r.projects }

// Webhooks returns the webhook ingress service.
func (r *Runtime) Webhooks() *WebhookService { return r.webhooks }

// Commands returns the review command worker.
func (r *Runtime) Commands() *CommandWorker { return r.commands }

// Reviews returns the review orchestration service.
func (r *Runtime) Reviews() *ReviewService { return r.reviews }

// WorkspaceService returns the onboarding and refresh service.
func (r *Runtime) WorkspaceService() *WorkspaceService { return r.workspaces }

// WorkspaceManager exposes the shared per-project workspace lock and
// directory owner. Context projections must reuse it instead of opening a
// second manager.
func (r *Runtime) WorkspaceManager() *workspace.Manager { return r.workspace }

// Feedback returns the feedback collector.
func (r *Runtime) Feedback() *FeedbackService { return r.feedback }

// Retention returns the periodic cleanup service.
func (r *Runtime) Retention() *RetentionService { return r.retention }

// RunCleanup returns the periodic stale-run cleanup service.
func (r *Runtime) RunCleanup() *RunCleanupService { return r.runCleanup }

// Publisher exposes manual republish for the review API.
func (r *Runtime) Publisher() *Publisher { return r.publisher }

// MCPManager returns the shared MCP manager used by the extension tester.
func (r *Runtime) MCPManager() *mcp.Manager { return r.mcp }

// ProjectExtensions returns the project-scoped extension resolver used when
// building review executors.
func (r *Runtime) ProjectExtensions() *ProjectExtensionResolver { return r.projectExts }

// LLMClients returns the provider client registry used by the admin probe.
func (r *Runtime) LLMClients() review.ClientProvider { return r.clients }

// Lifecycle returns the worker readiness flag.
func (r *Runtime) Lifecycle() *Lifecycle { return r.lifecycle }

// Handlers returns every registered job handler.
func (r *Runtime) Handlers() map[string]jobs.Handler {
	handlers := r.worker.Handlers()
	handlers["review.command"] = r.commands.Handle
	return handlers
}

// Recover runs engine recovery, rebuilds review executors, and retries pending
// publications. It must be called after the job worker starts.
func (r *Runtime) Recover(ctx context.Context) error {
	if err := r.engine.Recover(ctx); err != nil {
		r.logger.Warn("workflow recovery failed", "error", err.Error())
	}
	return r.reviews.Recover(ctx)
}

// reviewSideEffectReconcileInterval bounds how long a pending status or
// reaction survives a worker crash.
const reviewSideEffectReconcileInterval = 5 * time.Minute

// reconcileReviewSideEffects periodically re-enqueues pending side-effect
// goals and settles rows that no live job can finish.
func (r *Runtime) reconcileReviewSideEffects(ctx context.Context, interval time.Duration) {
	if r.reviews == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.reviews.ReconcileSideEffects(ctx); err != nil {
				r.logger.Warn("review side-effect reconciliation failed", "error", err.Error())
			}
		}
	}
}

// StartBackground starts the feedback, retention, run-cleanup, and review
// side-effect tickers.
// The returned function stops them and waits for the goroutines.
func (r *Runtime) StartBackground(ctx context.Context) func() {
	r.startOnce.Do(func() {
		bgCtx, cancel := context.WithCancel(ctx)
		r.startStop = cancel
		var wg sync.WaitGroup
		wg.Add(4)
		go func() {
			defer wg.Done()
			r.feedback.Start(bgCtx, defaultFeedbackInterval)
		}()
		go func() {
			defer wg.Done()
			r.retention.Start(bgCtx, 0)
		}()
		go func() {
			defer wg.Done()
			r.runCleanup.Start(bgCtx, 0)
		}()
		go func() {
			defer wg.Done()
			r.reconcileReviewSideEffects(bgCtx, reviewSideEffectReconcileInterval)
		}()
		go func() {
			wg.Wait()
			close(r.startDone)
		}()
	})
	return func() {
		if r.startStop != nil {
			r.startStop()
		}
		select {
		case <-r.startDone:
		case <-time.After(5 * time.Second):
		}
	}
}

// Close releases runtime-owned resources that are not shared with main.
func (r *Runtime) Close() error {
	var errs []error
	if r.projectExts != nil {
		if err := r.projectExts.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if r.mcp != nil {
		if err := r.mcp.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
