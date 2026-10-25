package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/jobs"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/mcp"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/review"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/secret"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/workspace"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/prompts"
)

// MCPFactory builds the MCP manager for one project workspace. A nil factory
// disables MCP tools and indexing.
type MCPFactory func(ctx context.Context, projectID int64) (*mcp.Manager, error)

// JobEnqueuer is the enqueue surface used by workspace service helpers.
type JobEnqueuer interface {
	Enqueue(ctx context.Context, job store.Job) (string, bool, error)
}

// WorkspaceService implements F4/F5: onboarding and base refresh.
type WorkspaceService struct {
	store          ReviewStore
	queue          JobEnqueuer
	gitlab         GitLabClient
	cipher         *secret.Cipher
	workspace      *workspace.Manager
	git            GitOps
	engine         *dag.Engine
	executors      *executorRegistry
	profiles       map[string]config.ProfileConfig
	blocks         map[string]config.BlockBinding
	reviewDefaults config.ProjectSettings
	prompts        prompts.Bundle
	clients        review.ClientProvider
	agentRun       review.AgentRunner
	tools          review.ToolProvider
	skills         review.SkillProvider
	contextRefresh config.ContextRefreshConfig
	mcpFactory     MCPFactory
	logger         *slog.Logger
	now            func() time.Time
}

// WorkspaceServiceConfig configures WorkspaceService.
type WorkspaceServiceConfig struct {
	Store          ReviewStore
	Queue          JobEnqueuer
	GitLab         GitLabClient
	Cipher         *secret.Cipher
	Workspace      *workspace.Manager
	Git            GitOps
	Engine         *dag.Engine
	Executors      *executorRegistry
	Profiles       map[string]config.ProfileConfig
	Blocks         map[string]config.BlockBinding
	ReviewDefaults config.ProjectSettings
	Prompts        prompts.Bundle
	Clients        review.ClientProvider
	AgentRun       review.AgentRunner
	Tools          review.ToolProvider
	Skills         review.SkillProvider
	ContextRefresh config.ContextRefreshConfig
	MCP            MCPFactory
	Logger         *slog.Logger
	Now            func() time.Time
}

// NewWorkspaceService builds the F4/F5 service.
func NewWorkspaceService(cfg WorkspaceServiceConfig) (*WorkspaceService, error) {
	if cfg.Store == nil || cfg.GitLab == nil || cfg.Cipher == nil ||
		cfg.Workspace == nil || cfg.Git == nil || cfg.Prompts.Files == nil {
		return nil, errors.New("app: workspace service dependencies are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	registry := cfg.Executors
	if registry == nil {
		registry = newExecutorRegistry()
	}
	engine := cfg.Engine
	if engine == nil {
		engine = dag.NewEngine(cfg.Store, registry, registry, dag.Options{})
	}
	return &WorkspaceService{
		store:          cfg.Store,
		queue:          cfg.Queue,
		gitlab:         cfg.GitLab,
		cipher:         cfg.Cipher,
		workspace:      cfg.Workspace,
		git:            cfg.Git,
		engine:         engine,
		executors:      registry,
		profiles:       cfg.Profiles,
		blocks:         cfg.Blocks,
		reviewDefaults: cfg.ReviewDefaults,
		prompts:        cfg.Prompts,
		clients:        cfg.Clients,
		agentRun:       cfg.AgentRun,
		tools:          cfg.Tools,
		skills:         cfg.Skills,
		contextRefresh: cfg.ContextRefresh,
		mcpFactory:     cfg.MCP,
		logger:         logger,
		now:            now,
	}, nil
}

type workspaceJobPayload struct {
	ProjectID int64  `json:"project_id"`
	Ref       string `json:"ref"`
	Branch    string `json:"branch"`
	AfterSHA  string `json:"after_sha"`
	Trigger   string `json:"trigger"`
	// Nil keeps the pre-existing refresh behavior: reindex always, regenerate
	// context only when the freshness rules ask for it.
	RegenerateContext *bool `json:"regenerate_context,omitempty"`
	Reindex           *bool `json:"reindex,omitempty"`
}

// WorkspaceRefreshOptions overrides the refresh defaults. A nil field keeps
// the default behavior.
type WorkspaceRefreshOptions struct {
	RegenerateContext *bool
	Reindex           *bool
}

// Handle dispatches workspace jobs. Permanent failures are logged and settled.
func (s *WorkspaceService) Handle(ctx context.Context, job *jobs.Job) error {
	if job == nil {
		return nil
	}
	var err error
	switch job.Kind {
	case "workspace.onboard":
		var payload workspaceJobPayload
		if decodeErr := json.Unmarshal(job.Payload, &payload); decodeErr != nil {
			err = PermanentJob(fmt.Errorf("invalid workspace payload"))
		} else {
			err = s.Onboard(ctx, payload.ProjectID)
		}
	case "workspace.refresh":
		var payload workspaceJobPayload
		if decodeErr := json.Unmarshal(job.Payload, &payload); decodeErr != nil {
			err = PermanentJob(fmt.Errorf("invalid workspace payload"))
		} else if payload.ProjectID <= 0 {
			err = PermanentJob(fmt.Errorf("invalid workspace project"))
		} else {
			err = s.RefreshWithOptions(ctx, payload.ProjectID, payload.Ref, WorkspaceRefreshOptions{
				RegenerateContext: payload.RegenerateContext,
				Reindex:           payload.Reindex,
			})
		}
	default:
		return nil
	}
	if err == nil || IsPermanentJob(err) {
		if err != nil {
			s.logger.Info("workspace job settled without retry", "job_id", job.ID, "kind", job.Kind, "reason", err.Error())
		}
		return nil
	}
	return err
}

// EnqueueOnboard schedules one onboarding job.
func (s *WorkspaceService) EnqueueOnboard(ctx context.Context, projectID int64) (string, error) {
	if s.queue == nil {
		return "", errors.New("app: job queue is not configured")
	}
	return enqueueJob(ctx, s.queue, "workspace.onboard", fmt.Sprintf("ws:%d:onboard", projectID), map[string]any{
		"project_id": projectID,
		"trigger":    "register",
	})
}

// EnqueueRefresh schedules one workspace refresh job.
func (s *WorkspaceService) EnqueueRefresh(ctx context.Context, projectID int64, ref, branch, after string) (string, error) {
	if s.queue == nil {
		return "", errors.New("app: job queue is not configured")
	}
	return enqueueJob(ctx, s.queue, "workspace.refresh", fmt.Sprintf("ws:%d", projectID), map[string]any{
		"project_id": projectID,
		"ref":        ref,
		"branch":     branch,
		"after_sha":  after,
		"trigger":    "push",
	})
}

func enqueueJob(ctx context.Context, queue JobEnqueuer, kind, key string, payload any) (string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	id, _, err := queue.Enqueue(ctx, store.Job{Kind: kind, Key: key, Payload: encoded, MaxAttempts: 3})
	return id, err
}

// Onboard implements F5.
func (s *WorkspaceService) Onboard(ctx context.Context, projectID int64) error {
	if projectID <= 0 {
		return PermanentJob(fmt.Errorf("invalid project"))
	}
	project, token, err := s.projectToken(ctx, projectID)
	if err != nil {
		return err
	}
	ws := s.workspace.Get(projectID)
	if ws == nil {
		return PermanentJob(fmt.Errorf("workspace is unavailable"))
	}
	unlock, err := ws.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return s.onboardLocked(ctx, project, token, true)
}

// Refresh implements F4 with the default index and context behavior.
func (s *WorkspaceService) Refresh(ctx context.Context, projectID int64, ref string) error {
	return s.RefreshWithOptions(ctx, projectID, ref, WorkspaceRefreshOptions{})
}

// RefreshWithOptions implements F4 and honors explicit index/context flags.
func (s *WorkspaceService) RefreshWithOptions(ctx context.Context, projectID int64, ref string, options WorkspaceRefreshOptions) error {
	project, token, err := s.projectToken(ctx, projectID)
	if err != nil {
		return err
	}
	ws := s.workspace.Get(projectID)
	if ws == nil {
		return PermanentJob(fmt.Errorf("workspace is unavailable"))
	}
	unlock, err := ws.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()

	state, err := s.store.GetWorkspace(ctx, projectID)
	if err != nil || state == nil {
		state = &store.Workspace{ProjectID: projectID, State: "new"}
	}
	s.saveWorkspace(ctx, store.Workspace{ProjectID: projectID, State: "refreshing", BaseSHA: state.BaseSHA, IndexSHA: state.IndexSHA, ContextSHA: state.ContextSHA})

	oldBase := state.BaseSHA
	newBase, err := s.refreshBase(ctx, ws, project, token, "", ref)
	if err != nil {
		s.markWorkspaceError(ctx, projectID, err)
		return err
	}
	reindex := options.Reindex == nil || *options.Reindex
	indexSHA := state.IndexSHA
	if reindex && s.mcpFactory != nil {
		count, err := s.runIndex(ctx, projectID, ws.BaseDir())
		if err != nil {
			s.markWorkspaceError(ctx, projectID, err)
			return err
		}
		if count > 0 {
			indexSHA = newBase
		}
	}
	regenerate := s.shouldRefreshContext(ctx, projectID, oldBase, newBase, state)
	if options.RegenerateContext != nil {
		regenerate = *options.RegenerateContext && !contextDocLocked(ctx, s.store, projectID)
	}
	contextAfter := state.ContextSHA
	if regenerate {
		if err := s.generateContext(ctx, project, token, ws, newBase, false); err != nil {
			if errors.Is(err, ErrContextProjection) {
				// The database owns the active revision; a failed disk projection
				// must not be reported as a successful refresh.
				s.markWorkspaceError(ctx, projectID, err)
				return err
			}
			// Context generation is best effort on refresh: the base moved and
			// reviews can still run.
			s.logger.Warn("context refresh failed", "project_id", projectID)
		} else {
			// A locked current revision keeps the workspace on its own base; a
			// successful generation only moves it when the store activated it.
			contextAfter = s.activeContextBase(ctx, projectID, newBase)
			_ = ws.SetContextStale(contextAfter, contextAfter != newBase)
		}
	}
	s.saveWorkspace(ctx, store.Workspace{
		ProjectID:     projectID,
		State:         "ready",
		BaseSHA:       newBase,
		IndexSHA:      indexSHA,
		ContextSHA:    contextAfter,
		LastRefreshAt: timePtr(s.now().UTC()),
	})
	return nil
}

func contextDocLocked(ctx context.Context, st ReviewStore, projectID int64) bool {
	current, err := st.GetCurrentContextDoc(ctx, projectID, "agents")
	return err == nil && current.Locked
}

// activeContextBase returns the base of the store's active agents revision.
// The fallback applies before the first document exists.
func (s *WorkspaceService) activeContextBase(ctx context.Context, projectID int64, fallback string) string {
	current, err := s.store.GetCurrentContextDoc(ctx, projectID, "agents")
	if err == nil && current != nil && current.BaseSHA != "" {
		return current.BaseSHA
	}
	return fallback
}

func (s *WorkspaceService) onboardLocked(ctx context.Context, project *store.Project, token string, fetchBase bool) error {
	ws := s.workspace.Get(project.ID)
	if ws == nil {
		return PermanentJob(fmt.Errorf("workspace is unavailable"))
	}
	s.saveWorkspace(ctx, store.Workspace{ProjectID: project.ID, State: "onboarding"})

	_, ok := s.profiles["onboarding"]
	if !ok {
		err := PermanentJob(fmt.Errorf("onboarding profile is not configured"))
		s.markWorkspaceError(ctx, project.ID, err)
		return err
	}
	base := ws.BaseSHA()
	if fetchBase || base == "" {
		var err error
		base, err = s.refreshBase(ctx, ws, project, token, project.DefaultBranch, "")
		if err != nil {
			s.markWorkspaceError(ctx, project.ID, err)
			return err
		}
	}
	indexed := false
	if s.mcpFactory != nil {
		count, err := s.runIndex(ctx, project.ID, ws.BaseDir())
		if err != nil {
			s.markWorkspaceError(ctx, project.ID, err)
			return err
		}
		// An MCP manager can exist without an index command. Only a command
		// that actually ran proves this base was indexed.
		indexed = count > 0
	}
	if err := s.generateContext(ctx, project, token, ws, base, false); err != nil {
		s.markWorkspaceError(ctx, project.ID, err)
		return err
	}
	indexSHA := ""
	if indexed {
		indexSHA = base
	}
	s.saveWorkspace(ctx, store.Workspace{
		ProjectID:  project.ID,
		State:      "ready",
		BaseSHA:    base,
		IndexSHA:   indexSHA,
		ContextSHA: s.activeContextBase(ctx, project.ID, base),
	})
	return nil
}

func (s *WorkspaceService) generateContext(ctx context.Context, project *store.Project, token string, ws *workspace.Workspace, baseSHA string, saveState bool) error {
	profileCfg, ok := s.profiles["onboarding"]
	if !ok || profileCfg.Agent == nil || strings.TrimSpace(profileCfg.Agent.Provider) == "" {
		return PermanentJob(fmt.Errorf("onboarding profile is not configured"))
	}
	docs := review.RepoDocs{
		GitLabReviewConfig: s.rawDoc(ctx, token, project.ID, ".gitlab-review.yml", project.DefaultBranch),
		AGENTS:             s.rawDoc(ctx, token, project.ID, "AGENTS.md", project.DefaultBranch),
		CLAUDE:             s.rawDoc(ctx, token, project.ID, "CLAUDE.md", project.DefaultBranch),
	}
	dossier := &review.Dossier{
		Project:     review.ProjectRef{ID: project.ID, Path: project.PathWithNamespace, WebURL: project.WebURL},
		MR:          review.MRMeta{TargetBranch: project.DefaultBranch},
		RepoDocs:    docs,
		BaseDir:     ws.BaseDir(),
		Files:       []review.FileChange{},
		GeneratedAt: s.now().UTC(),
	}
	profile := review.Profile{Name: "onboarding", Config: profileCfg}
	graph, err := review.BuildGraph(profile, dossier, review.GraphConfig{})
	if err != nil {
		return PermanentJob(fmt.Errorf("build onboarding graph: %w", err))
	}
	executor, err := review.NewExecutor(review.ExecutorDeps{
		Dossier:  dossier,
		Profile:  profile,
		Blocks:   s.blocks,
		Clients:  s.clients,
		Prompts:  s.prompts,
		AgentRun: s.agentRun,
		Snapshot: engineSnapshot(s.engine),
		Tools:    s.tools,
		Skills:   s.skills,
	})
	if err != nil {
		return PermanentJob(fmt.Errorf("build onboarding executor: %w", err))
	}
	meta := map[string]string{
		"project_id": strconv.FormatInt(project.ID, 10),
		"kind":       "onboarding",
		"trigger":    "register",
		"base_sha":   baseSHA,
	}
	wfID, err := s.engine.Create(ctx, graph, meta)
	if err != nil {
		return err
	}
	s.executors.Register(wfID, executor)
	defer s.executors.Unregister(wfID)
	if err := s.engine.Run(ctx, wfID); err != nil {
		return err
	}
	snapshot, err := s.engine.Snapshot(ctx, wfID)
	if err != nil {
		return err
	}
	node, ok := snapshot.Nodes["onboarding"]
	if !ok || node.Status != dag.NodeCompleted {
		return errors.New("app: onboarding workflow did not complete")
	}
	var out review.OnboardingOut
	if err := json.Unmarshal(node.Output, &out); err != nil {
		return PermanentJob(fmt.Errorf("decode onboarding output: %w", err))
	}
	content := renderOnboardingMarkdown(out)
	savedDoc, err := s.store.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID:  project.ID,
		BaseSHA:    baseSHA,
		Kind:       "agents",
		Content:    content,
		Model:      profileCfg.Agent.Provider,
		WorkflowID: wfID,
	})
	if err != nil {
		return err
	}
	if err := projectContextDoc(ws, savedDoc, false); err != nil {
		return fmt.Errorf("%w: %v", ErrContextProjection, err)
	}
	if saveState {
		state, _ := s.store.GetWorkspace(ctx, project.ID)
		indexSHA := ""
		if state != nil {
			indexSHA = state.IndexSHA
		}
		contextSHA := s.activeContextBase(ctx, project.ID, savedDoc.BaseSHA)
		s.saveWorkspace(ctx, store.Workspace{
			ProjectID:  project.ID,
			State:      "ready",
			BaseSHA:    baseSHA,
			IndexSHA:   indexSHA,
			ContextSHA: contextSHA,
		})
	}
	return nil
}

func (s *WorkspaceService) shouldRefreshContext(ctx context.Context, projectID int64, oldBase, newBase string, state *store.Workspace) bool {
	if oldBase == "" || newBase == "" || strings.EqualFold(oldBase, newBase) {
		return oldBase == ""
	}
	if current, err := s.store.GetCurrentContextDoc(ctx, projectID, "agents"); err == nil && current.Locked {
		return false
	}
	if s.contextRefresh.MaxAge > 0 {
		if state == nil || state.LastRefreshAt == nil || s.now().Sub(*state.LastRefreshAt) >= s.contextRefresh.MaxAge {
			return true
		}
	}
	if s.contextRefresh.ChangedFiles <= 0 {
		return false
	}
	ws := s.workspace.Get(projectID)
	if ws == nil {
		return false
	}
	changed, err := s.git.ChangedFiles(ctx, ws.MirrorDir(), oldBase, newBase)
	if err != nil {
		return false
	}
	return len(changed) >= s.contextRefresh.ChangedFiles
}

func (s *WorkspaceService) refreshBase(ctx context.Context, ws *workspace.Workspace, project *store.Project, token, branch, ref string) (string, error) {
	remote := trimProjectWebURL(project.WebURL)
	if remote == "" {
		return "", PermanentJob(fmt.Errorf("project web url is empty"))
	}
	auth := repo.Auth{Token: token}
	if err := s.git.EnsureMirror(ctx, ws.MirrorDir(), remote, auth); err != nil {
		return "", err
	}
	if strings.TrimSpace(ref) == "" {
		if strings.TrimSpace(branch) == "" {
			branch = project.DefaultBranch
		}
		ref = "refs/heads/" + branch
	}
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}
	if err := s.git.Fetch(ctx, ws.MirrorDir(), auth, "+"+ref+":refs/workspace/base"); err != nil {
		return "", err
	}
	sha, err := s.git.Resolve(ctx, ws.MirrorDir(), "refs/workspace/base")
	if err != nil {
		return "", err
	}
	baseDir := ws.BaseDir()
	if info, statErr := os.Lstat(baseDir); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("app: base directory is not a directory")
		}
		if err := s.git.Checkout(ctx, baseDir, sha); err != nil {
			return "", err
		}
	} else if errors.Is(statErr, os.ErrNotExist) {
		if err := s.git.AddWorktree(ctx, ws.MirrorDir(), baseDir, sha); err != nil {
			return "", err
		}
	} else {
		return "", statErr
	}
	return sha, nil
}

func (s *WorkspaceService) runIndex(ctx context.Context, projectID int64, baseDir string) (int, error) {
	if s.mcpFactory == nil {
		return 0, nil
	}
	manager, err := s.mcpFactory(ctx, projectID)
	if err != nil {
		return 0, err
	}
	if manager == nil {
		return 0, nil
	}
	// The manager is owned by the runtime and shared with review tools; do not
	// close it here.
	return manager.RunIndex(ctx, baseDir)
}

func (s *WorkspaceService) projectToken(ctx context.Context, projectID int64) (*store.Project, string, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, "", PermanentJob(fmt.Errorf("project is not registered"))
		}
		return nil, "", err
	}
	if project == nil || !project.Enabled {
		return nil, "", PermanentJob(fmt.Errorf("project is disabled"))
	}
	token, err := s.cipher.Decrypt(project.TokenEnc)
	if err != nil {
		return nil, "", PermanentJob(fmt.Errorf("project token is unavailable"))
	}
	return project, string(token), nil
}

func (s *WorkspaceService) rawDoc(ctx context.Context, token string, projectID int64, path, ref string) string {
	data, err := s.gitlab.GetRawFile(ctx, token, projectID, path, ref)
	if err != nil {
		return ""
	}
	return string(data)
}

func (s *WorkspaceService) saveWorkspace(ctx context.Context, ws store.Workspace) {
	if _, err := s.store.SaveWorkspace(ctx, ws); err != nil {
		s.logger.Warn("workspace state save failed", "project_id", ws.ProjectID)
	}
}

func (s *WorkspaceService) markWorkspaceError(ctx context.Context, projectID int64, cause error) {
	state, _ := s.store.GetWorkspace(ctx, projectID)
	out := store.Workspace{ProjectID: projectID, State: "error", LastError: truncateRunes(cause.Error(), 1000)}
	if state != nil {
		out.BaseSHA = state.BaseSHA
		out.IndexSHA = state.IndexSHA
		out.ContextSHA = state.ContextSHA
		out.LastRefreshAt = state.LastRefreshAt
	}
	s.saveWorkspace(ctx, out)
}

func timePtr(value time.Time) *time.Time { return &value }

func renderOnboardingMarkdown(out review.OnboardingOut) string {
	var b strings.Builder
	b.WriteString("# 项目上下文\n\n")
	if strings.TrimSpace(out.Overview) != "" {
		b.WriteString("## 概览\n\n")
		b.WriteString(out.Overview)
		b.WriteString("\n\n")
	}
	if strings.TrimSpace(out.Architecture) != "" {
		b.WriteString("## 架构\n\n")
		b.WriteString(out.Architecture)
		b.WriteString("\n\n")
	}
	if len(out.Modules) > 0 {
		b.WriteString("## 模块\n\n| 路径 | 用途 |\n|---|---|\n")
		for _, module := range out.Modules {
			fmt.Fprintf(&b, "| %s | %s |\n", escapeTable(module.Path), escapeTable(module.Purpose))
		}
		b.WriteByte('\n')
	}
	writeList := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "## %s\n\n", title)
		for _, item := range items {
			b.WriteString("- ")
			b.WriteString(item)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	writeList("约定", out.Conventions)
	writeList("构建与测试", out.BuildAndTest)
	writeList("风险热点", out.RiskHotspots)
	writeList("评审重点", out.ReviewFocus)
	return b.String()
}

func escapeTable(value string) string {
	value = strings.ReplaceAll(value, "|", "\\|")
	return strings.ReplaceAll(value, "\n", " ")
}
