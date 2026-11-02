package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
	"github.com/LeXwDeX/tribunal/pkg/dag"
	"github.com/LeXwDeX/tribunal/prompts"
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
			err = s.onboard(ctx, payload.ProjectID, job)
		}
	case "workspace.refresh":
		var payload workspaceJobPayload
		if decodeErr := json.Unmarshal(job.Payload, &payload); decodeErr != nil {
			err = PermanentJob(fmt.Errorf("invalid workspace payload"))
		} else if payload.ProjectID <= 0 {
			err = PermanentJob(fmt.Errorf("invalid workspace project"))
		} else {
			err = s.refreshWithOptions(ctx, payload.ProjectID, payload.Ref, WorkspaceRefreshOptions{
				RegenerateContext: payload.RegenerateContext,
				Reindex:           payload.Reindex,
			}, job)
		}
	default:
		return nil
	}
	if err == nil || IsPermanentJob(err) {
		if err != nil {
			s.logger.Info("workspace job settled without retry", "job_id", job.ID, "kind", job.Kind, "error_class", jobSettleClass(err))
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
	unlock, err := s.lockEnqueueProject(ctx, projectID)
	if err != nil {
		return "", err
	}
	defer unlock()
	return enqueueJob(ctx, s.queue, "workspace.onboard", fmt.Sprintf("ws:%d:onboard", projectID), map[string]any{
		"project_id": projectID,
		"trigger":    "register",
	})
}

// RetryOnboarding schedules a fresh onboarding job after a terminal failure.
// The fixed key makes repeated retries idempotent while a queued job exists;
// failed records do not occupy the active-key index.
func (s *WorkspaceService) RetryOnboarding(ctx context.Context, projectID int64) (string, error) {
	if s.queue == nil {
		return "", errors.New("app: job queue is not configured")
	}
	unlock, err := s.lockEnqueueProject(ctx, projectID)
	if err != nil {
		return "", err
	}
	defer unlock()
	key := fmt.Sprintf("workspace.onboard:%d:retry", projectID)
	return enqueueJob(ctx, s.queue, "workspace.onboard", key, map[string]any{
		"project_id": projectID,
		"trigger":    "manual_retry",
	})
}

// EnqueueRefresh schedules one workspace refresh job.
func (s *WorkspaceService) EnqueueRefresh(ctx context.Context, projectID int64, ref, branch, after string) (string, error) {
	if s.queue == nil {
		return "", errors.New("app: job queue is not configured")
	}
	unlock, err := s.lockEnqueueProject(ctx, projectID)
	if err != nil {
		return "", err
	}
	defer unlock()
	return enqueueJob(ctx, s.queue, "workspace.refresh", fmt.Sprintf("ws:%d", projectID), map[string]any{
		"project_id": projectID,
		"ref":        ref,
		"branch":     branch,
		"after_sha":  after,
		"trigger":    "push",
	})
}

func (s *WorkspaceService) lockEnqueueProject(ctx context.Context, projectID int64) (func(), error) {
	ws := s.workspace.Get(projectID)
	if ws == nil {
		return nil, notFoundError("project not found")
	}
	unlock, err := ws.RLock(ctx)
	if err != nil {
		return nil, err
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil || project == nil {
		unlock()
		if errors.Is(err, store.ErrNotFound) || (err == nil && project == nil) {
			return nil, notFoundError("project not found")
		}
		return nil, err
	}
	return unlock, nil
}

func enqueueJob(ctx context.Context, queue JobEnqueuer, kind, key string, payload any) (string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	id, _, err := queue.Enqueue(ctx, store.Job{Kind: kind, Key: key, Payload: encoded, MaxAttempts: 3})
	return id, err
}

// consumerJobExists checks the original persisted job while holding its
// project's workspace lock. Job IDs do not survive project deletion.
func consumerJobExists(ctx context.Context, st interface {
	GetJob(context.Context, string) (*store.Job, error)
}, job *jobs.Job, kind string, projectID int64) error {
	if job == nil || strings.TrimSpace(job.ID) == "" || job.Kind != kind {
		return PermanentJob(fmt.Errorf("invalid consumer job"))
	}
	stored, err := st.GetJob(ctx, job.ID)
	if errors.Is(err, store.ErrNotFound) {
		return PermanentJob(fmt.Errorf("consumer job no longer exists"))
	}
	if err != nil {
		return err
	}
	var identity struct {
		ProjectID int64 `json:"project_id"`
	}
	if stored == nil || stored.ID != job.ID || stored.Kind != kind || json.Unmarshal(stored.Payload, &identity) != nil || identity.ProjectID != projectID {
		return PermanentJob(fmt.Errorf("consumer job identity changed"))
	}
	return nil
}

// Onboard implements F5.
func (s *WorkspaceService) Onboard(ctx context.Context, projectID int64) error {
	return s.onboard(ctx, projectID, nil)
}

func (s *WorkspaceService) onboard(ctx context.Context, projectID int64, job *jobs.Job) error {
	if projectID <= 0 {
		return PermanentJob(fmt.Errorf("invalid project"))
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
	if job != nil {
		if err := consumerJobExists(ctx, s.store, job, "workspace.onboard", projectID); err != nil {
			return err
		}
	}
	project, token, err := s.projectToken(ctx, projectID)
	if err != nil {
		return err
	}
	return s.onboardLocked(ctx, project, token, true)
}

// Refresh implements F4 with the default index and context behavior.
func (s *WorkspaceService) Refresh(ctx context.Context, projectID int64, ref string) error {
	return s.RefreshWithOptions(ctx, projectID, ref, WorkspaceRefreshOptions{})
}

// RefreshWithOptions implements F4 and honors explicit index/context flags.
func (s *WorkspaceService) RefreshWithOptions(ctx context.Context, projectID int64, ref string, options WorkspaceRefreshOptions) error {
	return s.refreshWithOptions(ctx, projectID, ref, options, nil)
}

func (s *WorkspaceService) refreshWithOptions(ctx context.Context, projectID int64, ref string, options WorkspaceRefreshOptions, job *jobs.Job) error {
	ws := s.workspace.Get(projectID)
	if ws == nil {
		return PermanentJob(fmt.Errorf("workspace is unavailable"))
	}
	unlock, err := ws.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if job != nil {
		if err := consumerJobExists(ctx, s.store, job, "workspace.refresh", projectID); err != nil {
			return err
		}
	}
	project, token, err := s.projectToken(ctx, projectID)
	if err != nil {
		return err
	}

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
	docs, cfg, err := s.workspaceReviewConfig(ctx, project, token, newBase)
	if err != nil {
		s.markWorkspaceError(ctx, projectID, err)
		return err
	}
	reindex := options.Reindex == nil || *options.Reindex
	indexSHA := state.IndexSHA
	indexErr := ""
	indexEligible := !reindex && state.State == "ready" && state.LastError == "" && shaRE.MatchString(newBase) && shaRE.MatchString(indexSHA) && strings.EqualFold(indexSHA, newBase)
	if len(cfg.Settings.Ignore) > 0 {
		indexErr = "index: blocked by ignore policy"
	} else if reindex && s.mcpFactory != nil {
		count, err := s.runIndex(ctx, projectID, ws.BaseDir())
		switch {
		case err != nil && ctx.Err() != nil:
			s.markWorkspaceError(ctx, projectID, err)
			return err
		case err != nil:
			// The previous index stays in use; reviews continue and report
			// the missing code graph themselves.
			indexErr = s.indexDegraded(projectID, err)
		case count > 0:
			indexSHA = newBase
			indexEligible = shaRE.MatchString(newBase)
		default:
			indexErr = store.WorkspaceIndexFailed
		}
	} else if !indexEligible && (state.LastError != "" || strings.EqualFold(indexSHA, newBase) && indexSHA != "") {
		indexErr = store.WorkspaceIndexFailed
	}
	regenerate := s.shouldRefreshContext(ctx, projectID, oldBase, newBase, state)
	if options.RegenerateContext != nil {
		regenerate = *options.RegenerateContext && !contextDocLocked(ctx, s.store, projectID)
	}
	contextAfter := state.ContextSHA
	if regenerate {
		if err := s.generateContext(ctx, project, ws, newBase, docs, cfg, indexEligible && indexErr == ""); err != nil {
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
		LastError:     indexErr,
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
	previous, err := s.store.GetWorkspace(ctx, project.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	state := store.Workspace{ProjectID: project.ID, State: "onboarding"}
	if previous != nil {
		state.BaseSHA, state.IndexSHA, state.ContextSHA = previous.BaseSHA, previous.IndexSHA, previous.ContextSHA
		state.LastRefreshAt = previous.LastRefreshAt
	}
	s.saveWorkspace(ctx, state)

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
	docs, cfg, err := s.workspaceReviewConfig(ctx, project, token, base)
	if err != nil {
		s.markWorkspaceError(ctx, project.ID, err)
		return err
	}
	indexed := false
	indexErr := ""
	if len(cfg.Settings.Ignore) > 0 {
		indexErr = "index: blocked by ignore policy"
	} else if s.mcpFactory != nil {
		count, err := s.runIndex(ctx, project.ID, ws.BaseDir())
		switch {
		case err != nil && ctx.Err() != nil:
			s.markWorkspaceError(ctx, project.ID, err)
			return err
		case err != nil:
			// A missing or broken indexer must not block onboarding: the
			// context documents are still generated without a code graph.
			indexErr = s.indexDegraded(project.ID, err)
		default:
			// An MCP manager can exist without an index command. Only a
			// command that actually ran proves this base was indexed.
			indexed = count > 0
		}
	}
	if err := s.generateContext(ctx, project, ws, base, docs, cfg, indexed); err != nil {
		s.markWorkspaceError(ctx, project.ID, err)
		return err
	}
	indexSHA := ""
	if indexed {
		indexSHA = base
	} else if len(cfg.Settings.Ignore) > 0 {
		if state, err := s.store.GetWorkspace(ctx, project.ID); err == nil && state != nil {
			indexSHA = state.IndexSHA
		}
	}
	s.saveWorkspace(ctx, store.Workspace{
		ProjectID:  project.ID,
		State:      "ready",
		BaseSHA:    base,
		IndexSHA:   indexSHA,
		ContextSHA: s.activeContextBase(ctx, project.ID, base),
		LastError:  indexErr,
	})
	return nil
}

func (s *WorkspaceService) generateContext(ctx context.Context, project *store.Project, ws *workspace.Workspace, baseSHA string, docs review.RepoDocs, cfg effectiveReviewConfig, indexEligible bool) error {
	profileCfg, ok := s.profiles["onboarding"]
	if !ok || profileCfg.Agent == nil || strings.TrimSpace(profileCfg.Agent.Provider) == "" {
		return PermanentJob(fmt.Errorf("onboarding profile is not configured"))
	}
	dossier := &review.Dossier{
		Project:             review.ProjectRef{ID: project.ID, Path: project.PathWithNamespace, WebURL: project.WebURL},
		MR:                  review.MRMeta{TargetBranch: project.DefaultBranch},
		DiffVersion:         gitlab.Version{StartSHA: baseSHA},
		WorkspaceProvenance: &review.WorkspaceProvenance{Version: review.WorkspaceProvenanceVersion, TargetSHA: baseSHA, IndexSHA: baseSHA, MCPEligible: indexEligible},
		RepoDocs:            filterRepoDocs(docs, cfg.Settings.Ignore),
		IgnorePolicy:        &review.IgnorePolicy{Version: review.IgnorePolicyVersion, Patterns: append([]string{}, cfg.Settings.Ignore...)},
		CustomRules:         cfg.Settings.CustomRules,
		Language:            cfg.Settings.Language,
		BaseDir:             ws.BaseDir(),
		Files:               []review.FileChange{},
		GeneratedAt:         s.now().UTC(),
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
		"project_id":                 strconv.FormatInt(project.ID, 10),
		"kind":                       "onboarding",
		"trigger":                    "register",
		"base_sha":                   baseSHA,
		"workspace_material_version": contextMaterialVersion,
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
		return &onboardingFailure{class: onboardingErrorClass(node.Reason), status: node.ErrorStatus}
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
	remote, err := currentProjectRemote(ctx, s.gitlab, project, token)
	if err != nil {
		return "", err
	}
	auth := repo.Auth{Token: token}
	if strings.TrimSpace(ref) == "" {
		if strings.TrimSpace(branch) == "" {
			branch = project.DefaultBranch
		}
		ref = "refs/heads/" + branch
	}
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}
	sha, err := fetchProjectRef(ctx, ws, s.git, remote, auth, "+"+ref+":refs/workspace/base", "refs/workspace/base")
	if err != nil {
		return "", err
	}
	if err := ws.PrepareBase(ctx, sha, s.git); err != nil {
		return "", err
	}
	return sha, nil
}

// indexDegraded logs a failed index run and returns a fixed workspace message.
func (s *WorkspaceService) indexDegraded(projectID int64, err error) string {
	s.logger.Warn("code graph index failed; continuing without it", "project_id", projectID, "error_class", workspaceErrorClass(err))
	return store.WorkspaceIndexFailed
}

// onboardingFailure carries the sanitized failure class and upstream status
// from the DAG node that failed, without retaining its error text.
type onboardingFailure struct {
	class  string
	status int
}

func (e *onboardingFailure) Error() string     { return "app: onboarding workflow did not complete" }
func (e *onboardingFailure) ErrorCode() string { return e.class }
func (e *onboardingFailure) ErrorStatus() int  { return e.status }

func onboardingErrorClass(reason string) string {
	switch reason {
	case "auth":
		return "upstream_auth"
	case "transient":
		return "network"
	case "bad_request":
		return "config"
	case "rate_limited", "overloaded", "context_too_long", "timeout", "canceled", "panic", "unsafe_model_response":
		return reason
	default:
		return errclass.Internal
	}
}

func workspaceErrorClass(err error) string {
	var upstream *llm.Error
	if errors.As(err, &upstream) && upstream.Kind != llm.ErrCanceled {
		switch upstream.Kind {
		case llm.ErrAuth:
			return "upstream_auth"
		case llm.ErrRateLimited:
			return "rate_limited"
		case llm.ErrTransient:
			return "network"
		case llm.ErrBadRequest:
			return "config"
		case llm.ErrOverloaded, llm.ErrContextTooLong, "unsafe_model_response":
			return string(upstream.Kind)
		default:
			return errclass.Internal
		}
	}
	switch class := errclass.Of(err); class {
	case errclass.Timeout, errclass.Canceled, errclass.Panic, errclass.Internal:
		return class
	case "upstream_auth", "rate_limited", "network", "config", "overloaded", "context_too_long", "unsafe_model_response":
		return class
	default:
		return errclass.Internal
	}
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
	token, err := s.cipher.Open(project.TokenEnc, secret.TokenAAD(project.ID))
	if err != nil {
		return nil, "", PermanentJob(fmt.Errorf("project token is unavailable"))
	}
	return project, string(token), nil
}

func (s *WorkspaceService) workspaceReviewConfig(ctx context.Context, project *store.Project, token, ref string) (review.RepoDocs, effectiveReviewConfig, error) {
	var docs review.RepoDocs
	for _, item := range []struct {
		path   string
		target *string
	}{
		{".gitlab-review.yml", &docs.GitLabReviewConfig},
		{"AGENTS.md", &docs.AGENTS},
		{"CLAUDE.md", &docs.CLAUDE},
	} {
		data, err := s.gitlab.GetRawFile(ctx, token, project.ID, item.path, ref)
		if err != nil && !gitlab.IsNotFound(err) {
			return review.RepoDocs{}, effectiveReviewConfig{}, err
		}
		*item.target = string(data)
	}
	cfg, err := resolveSecureReviewConfig(project, docs, s.reviewDefaults, s.profiles)
	return docs, cfg, err
}

func (s *WorkspaceService) saveWorkspace(ctx context.Context, ws store.Workspace) {
	if _, err := s.store.SaveWorkspace(ctx, ws); err != nil {
		s.logger.Warn("workspace state save failed", "project_id", ws.ProjectID)
	}
}

func (s *WorkspaceService) markWorkspaceError(ctx context.Context, projectID int64, cause error) {
	attrs := []any{"project_id", projectID, "error_class", workspaceErrorClass(cause)}
	var statused interface{ ErrorStatus() int }
	if errors.As(cause, &statused) {
		if status := statused.ErrorStatus(); status > 0 {
			attrs = append(attrs, "upstream_status", status)
		}
	}
	s.logger.Warn("workspace operation failed", attrs...)
	state, _ := s.store.GetWorkspace(ctx, projectID)
	out := store.Workspace{ProjectID: projectID, State: "error", LastError: store.WorkspaceOperationFailed}
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
