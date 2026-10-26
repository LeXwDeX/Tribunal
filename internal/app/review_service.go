package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
	"github.com/LeXwDeX/tribunal/pkg/dag"
	"github.com/LeXwDeX/tribunal/prompts"
)

const (
	defaultCancelPoll = 2 * time.Second
	defaultCancelWait = 45 * time.Second

	reviewCancelledReason       = "review cancelled"
	reviewSupersededReason      = "review superseded"
	failureNoteDisabledReason   = "failure note disabled"
	reviewExecutionFailedReason = "review execution failed"
)

// ReviewStore is the persistence surface needed by review orchestration.
type ReviewStore interface {
	dag.EventStore
	GetProject(ctx context.Context, id int64) (*store.Project, error)
	GetReviewControl(ctx context.Context, projectID, mrIID int64) (*store.ReviewControl, error)
	ActivateReviewCandidate(ctx context.Context, activation store.ReviewCandidateActivation) (string, int64, error)
	FindActiveReview(ctx context.Context, projectID, mrIID int64) (*store.Workflow, error)
	CreateReviewWorkflow(ctx context.Context, req sqlite.ReviewWorkflowRequest) (string, bool, error)
	DetachWorkflowJob(ctx context.Context, workflowID, jobID string) error
	GetWorkflowByJob(ctx context.Context, jobID string) (*store.Workflow, error)
	GetWorkflow(ctx context.Context, id string) (*store.Workflow, error)
	GetWorkflowDossier(ctx context.Context, workflowID string) (*store.WorkflowDossier, error)
	SaveWorkflowDossier(ctx context.Context, dossier store.WorkflowDossier) (*store.WorkflowDossier, error)
	SetWorkflowPublicationState(ctx context.Context, workflowID, state, reason string) error
	SetWorkflowPublicationStateIfPending(ctx context.Context, workflowID, reason string) error
	SetWorkflowVerdict(ctx context.Context, id, verdict string) error
	ListPendingPublications(ctx context.Context, limit int) ([]store.PendingPublication, error)
	ListWorkflows(ctx context.Context, filter store.WorkflowFilter, cursor string, limit int) ([]store.Workflow, string, error)
	LastPublishedHead(ctx context.Context, projectID, mrIID int64) (string, error)
	GetPublication(ctx context.Context, projectID, mrIID int64, kind, publishKey string) (*store.Publication, error)
	ListPublications(ctx context.Context, workflowID string) ([]store.Publication, error)
	UpsertPublication(ctx context.Context, p store.Publication) (*store.Publication, error)
	ReplaceFindings(ctx context.Context, workflowID string, findings []store.Finding) error
	ListFindings(ctx context.Context, filter store.FindingFilter) ([]store.Finding, error)
	HasPublishedSevereFinding(ctx context.Context, projectID, mrIID int64) (bool, error)
	GetWorkspace(ctx context.Context, projectID int64) (*store.Workspace, error)
	SaveWorkspace(ctx context.Context, ws store.Workspace) (*store.Workspace, error)
	GetCurrentContextDoc(ctx context.Context, projectID int64, kind string) (*store.ContextDoc, error)
	SaveContextDoc(ctx context.Context, doc store.ContextDoc) (*store.ContextDoc, error)
	SetActiveContext(ctx context.Context, projectID int64, baseSHA string) error
	UpsertFeedback(ctx context.Context, f store.Feedback) (*store.Feedback, error)
	ListFeedback(ctx context.Context, filter store.FeedbackFilter) ([]store.Feedback, string, error)
	Enqueue(ctx context.Context, job store.Job) (string, bool, error)
	SetReviewStatus(ctx context.Context, goal store.ReviewStatusGoal) (bool, error)
	GetReviewStatus(ctx context.Context, projectID, mrIID int64) (*store.ReviewStatus, error)
	MarkReviewStatusApplied(ctx context.Context, projectID, mrIID int64, version, noteID int64, bodyHash, lastError string) error
	ListPendingReviewStatuses(ctx context.Context) ([]store.ReviewStatus, error)
	ListReviewStatusesByState(ctx context.Context, states []string, updatedBefore time.Time) ([]store.ReviewStatus, error)
	SetReviewReaction(ctx context.Context, goal store.ReviewReactionGoal) (bool, error)
	GetReviewReaction(ctx context.Context, projectID, mrIID, noteID int64) (*store.ReviewReaction, error)
	MarkReviewReactionApplied(ctx context.Context, projectID, mrIID, noteID int64, version int64, lastError string) error
	ListPendingReviewReactions(ctx context.Context) ([]store.ReviewReaction, error)
	ListReviewReactionsByDesired(ctx context.Context, desired string, updatedBefore time.Time) ([]store.ReviewReaction, error)
	ClearOlderReviewReactions(ctx context.Context, projectID, mrIID, generation int64) ([]int64, error)
	ClearReviewReactionsForGeneration(ctx context.Context, projectID, mrIID, generation int64) ([]int64, error)
	HasActiveReviewJob(ctx context.Context, projectID, mrIID int64) (bool, error)
	LatestReviewWorkflow(ctx context.Context, projectID, mrIID int64) (*store.Workflow, error)
	RecentReviewDurations(ctx context.Context, projectID int64, profile string, since time.Time, limit int) ([]time.Duration, error)
}

// ReviewService implements F1/F2: MR review orchestration. It owns workflow
// creation, execution, and the decision to publish.
type ReviewService struct {
	store          ReviewStore
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
	providerNames  []string
	cost           review.CostFunc
	publisher      *Publisher
	publicURL      string
	status         *statusReporter
	logger         *slog.Logger
	now            func() time.Time
	cancelPoll     time.Duration
	cancelWait     time.Duration
}

// ReviewServiceConfig configures ReviewService.
type ReviewServiceConfig struct {
	Store            ReviewStore
	GitLab           GitLabClient
	Cipher           *secret.Cipher
	Workspace        *workspace.Manager
	Git              GitOps
	Engine           *dag.Engine
	Executors        *executorRegistry
	Profiles         map[string]config.ProfileConfig
	Blocks           map[string]config.BlockBinding
	ReviewDefaults   config.ProjectSettings
	Prompts          prompts.Bundle
	Clients          review.ClientProvider
	AgentRun         review.AgentRunner
	Tools            review.ToolProvider
	Skills           review.SkillProvider
	Cost             review.CostFunc
	Publisher        *Publisher
	Status           *statusReporter
	AllowedProviders []string
	PublicURL        string
	Logger           *slog.Logger
	Now              func() time.Time
	CancelPoll       time.Duration
	CancelWait       time.Duration
}

// NewReviewService validates dependencies and builds the F1/F2 service.
func NewReviewService(cfg ReviewServiceConfig) (*ReviewService, error) {
	if cfg.Store == nil || cfg.GitLab == nil || cfg.Cipher == nil ||
		cfg.Workspace == nil || cfg.Git == nil || cfg.Prompts.Files == nil {
		return nil, errors.New("app: review service dependencies are required")
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
	poll := cfg.CancelPoll
	if poll <= 0 {
		poll = defaultCancelPoll
	}
	wait := cfg.CancelWait
	if wait <= 0 {
		wait = defaultCancelWait
	}
	return &ReviewService{
		store:          cfg.Store,
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
		providerNames:  cfg.AllowedProviders,
		publisher:      cfg.Publisher,
		publicURL:      strings.TrimRight(cfg.PublicURL, "/"),
		status:         cfg.Status,
		logger:         logger,
		now:            now,
		cancelPoll:     poll,
		cancelWait:     wait,
	}, nil
}

// Handle dispatches review jobs. Permanent failures are logged and settled.
func (s *ReviewService) Handle(ctx context.Context, job *jobs.Job) error {
	if job == nil {
		return nil
	}
	var err error
	switch job.Kind {
	case "review.run":
		err = s.RunReview(ctx, job)
	case "review.cancel":
		err = s.CancelReview(ctx, job)
	case "review.candidate":
		err = s.RunCandidate(ctx, job)
	case "review.publish":
		err = s.RepublishReview(ctx, job)
	case "review.status":
		err = s.RunStatusJob(ctx, job)
	case "review.reaction":
		err = s.RunReactionJob(ctx, job)
	default:
		return nil
	}
	if err == nil || IsPermanentJob(err) {
		if err != nil {
			s.logger.Info("review job settled without retry", "job_id", job.ID, "kind", job.Kind, "reason", err.Error())
		}
		return nil
	}
	return err
}

type reviewRunPayload struct {
	ProjectID       int64  `json:"project_id"`
	MRIID           int64  `json:"mr_iid"`
	HeadSHA         string `json:"head_sha"`
	Generation      int64  `json:"generation"`
	Profile         string `json:"profile"`
	ProfileExplicit *bool  `json:"profile_explicit,omitempty"`
	Trigger         string `json:"trigger"`
	Full            bool   `json:"full"`
	NoteID          int64  `json:"note_id,omitempty"`
	ActivationSeq   int64  `json:"activation_seq,omitempty"`
}

type reviewCandidatePayload struct {
	ProjectID     int64  `json:"project_id"`
	MRIID         int64  `json:"mr_iid"`
	HeadSHA       string `json:"head_sha"`
	CandidateSeq  int64  `json:"candidate_seq"`
	Generation    int64  `json:"generation,omitempty"`
	ActivationSeq int64  `json:"activation_seq,omitempty"`
	Profile       string `json:"profile"`
	Trigger       string `json:"trigger"`
	Full          bool   `json:"full"`
}

type reviewCancelPayload struct {
	ProjectID  int64  `json:"project_id"`
	MRIID      int64  `json:"mr_iid"`
	Generation int64  `json:"generation"`
	Trigger    string `json:"trigger"`
	NoteID     int64  `json:"note_id,omitempty"`
}

type reviewPublishPayload struct {
	WorkflowID string `json:"workflow_id"`
}

func decodeReviewPayload(raw json.RawMessage, target any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("%w: invalid review payload", ErrPermanentJob)
	}
	return nil
}

// RunCandidate executes one deferred review.candidate job. It reads only the
// MR and target-branch configuration, so a filtered event completes without a
// control write, status note, or cancellation of the active review. Acceptance
// activates the candidate and enqueues review.run in one store transaction.
func (s *ReviewService) RunCandidate(ctx context.Context, job *jobs.Job) error {
	var payload reviewCandidatePayload
	if err := decodeReviewPayload(job.Payload, &payload); err != nil {
		return err
	}
	if payload.ProjectID <= 0 || payload.MRIID <= 0 || !shaRE.MatchString(payload.HeadSHA) ||
		payload.CandidateSeq <= 0 || payload.Generation != 0 || payload.ActivationSeq != 0 {
		return PermanentJob(fmt.Errorf("invalid review.candidate payload"))
	}
	payload.HeadSHA = strings.ToLower(payload.HeadSHA)
	if strings.TrimSpace(job.ID) == "" || strings.TrimSpace(job.LeaseOwner) == "" || job.Attempts <= 0 {
		return PermanentJob(fmt.Errorf("review candidate job lease is required"))
	}

	project, token, err := s.projectToken(ctx, payload.ProjectID)
	if err != nil {
		return err
	}
	control, err := s.store.GetReviewControl(ctx, payload.ProjectID, payload.MRIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return PermanentJob(fmt.Errorf("review control is missing"))
		}
		return err
	}
	if control.BarrierSeq >= payload.CandidateSeq {
		// A newer run/cancel or this candidate's earlier activation already
		// consumed the event. No GitLab call and no control write is needed.
		s.logger.Info("review candidate already consumed",
			"project_id", project.ID, "mr_iid", payload.MRIID, "candidate_seq", payload.CandidateSeq)
		return nil
	}

	mr, docs, err := s.collectFilterInputs(ctx, project, token, payload.MRIID, payload.HeadSHA)
	if err != nil {
		if errors.Is(err, ErrMRClosed) || errors.Is(err, ErrReviewSuperseded) {
			s.logger.Info("review candidate event is no longer applicable",
				"project_id", project.ID, "mr_iid", payload.MRIID, "reason", err.Error())
			return nil
		}
		return err
	}
	cfg := s.resolveConfig(project, docs)
	if reason := reviewFilterReason(payload.Trigger, mr, project, cfg.Settings); reason != "" {
		// The effective configuration rejected this event. Keep the active
		// review untouched; a later event can still supersede it.
		s.logger.Info("review candidate filtered after configuration merge",
			"project_id", project.ID, "mr_iid", payload.MRIID, "trigger", payload.Trigger, "reason", reason)
		return nil
	}

	runJobID, generation, err := s.store.ActivateReviewCandidate(ctx, store.ReviewCandidateActivation{
		JobID:        job.ID,
		LeaseOwner:   job.LeaseOwner,
		Attempts:     job.Attempts,
		ProjectID:    payload.ProjectID,
		MRIID:        payload.MRIID,
		HeadSHA:      payload.HeadSHA,
		CandidateSeq: payload.CandidateSeq,
	})
	switch {
	case errors.Is(err, store.ErrSuperseded):
		s.logger.Info("review candidate superseded before activation",
			"project_id", project.ID, "mr_iid", payload.MRIID, "candidate_seq", payload.CandidateSeq)
		return nil
	case errors.Is(err, store.ErrNotFound):
		return PermanentJob(fmt.Errorf("review control is missing"))
	case err != nil:
		return err
	}
	s.logger.Info("review candidate activated", "project_id", project.ID, "mr_iid", payload.MRIID,
		"candidate_seq", payload.CandidateSeq, "generation", generation, "job_id", runJobID)
	return nil
}

// collectFilterInputs reads only the MR and its target-branch repository
// configuration. The candidate filter decision must not require the full diff.
func (s *ReviewService) collectFilterInputs(ctx context.Context, project *store.Project, token string, mrIID int64, head string) (*gitlab.MergeRequest, review.RepoDocs, error) {
	mr, err := s.gitlab.GetMergeRequest(ctx, token, project.ID, mrIID)
	if err != nil {
		if gitlab.IsNotFound(err) {
			return nil, review.RepoDocs{}, PermanentJob(fmt.Errorf("%w: merge request not found", ErrMRClosed))
		}
		return nil, review.RepoDocs{}, err
	}
	if mr == nil || !isOpenState(mr.State) {
		return nil, review.RepoDocs{}, PermanentJob(fmt.Errorf("%w: state %s", ErrMRClosed, mrState(mr)))
	}
	if mr.TargetProjectID > 0 && mr.TargetProjectID != project.ID {
		return nil, review.RepoDocs{}, PermanentJob(fmt.Errorf("%w: target project mismatch", ErrReviewSuperseded))
	}
	if !strings.EqualFold(strings.TrimSpace(mr.SHA), head) {
		return nil, review.RepoDocs{}, PermanentJob(fmt.Errorf("%w: merge request head changed", ErrReviewSuperseded))
	}
	if strings.TrimSpace(mr.TargetBranch) == "" {
		return nil, review.RepoDocs{}, PermanentJob(fmt.Errorf("merge request target branch is empty"))
	}
	configDoc, err := s.rawDoc(ctx, token, project.ID, ".gitlab-review.yml", mr.TargetBranch)
	if err != nil {
		return nil, review.RepoDocs{}, err
	}
	return mr, review.RepoDocs{GitLabReviewConfig: configDoc}, nil
}

// RunReview executes one review.run job.
func (s *ReviewService) RunReview(ctx context.Context, job *jobs.Job) (err error) {
	var payload reviewRunPayload
	if err := decodeReviewPayload(job.Payload, &payload); err != nil {
		return err
	}
	if payload.ProjectID <= 0 || payload.MRIID <= 0 || payload.Generation <= 0 ||
		!shaRE.MatchString(payload.HeadSHA) || payload.NoteID < 0 || payload.ActivationSeq < 0 {
		return PermanentJob(fmt.Errorf("invalid review.run payload"))
	}
	payload.HeadSHA = strings.ToLower(payload.HeadSHA)
	if strings.TrimSpace(job.ID) == "" {
		return PermanentJob(fmt.Errorf("review job id is required"))
	}

	project, token, err := s.projectToken(ctx, payload.ProjectID)
	if err != nil {
		return err
	}
	defer func() {
		if err == nil || ctx.Err() != nil || !IsPermanentJob(err) {
			return
		}
		state, detail := reviewStatusFailed, reviewExecutionFailedReason
		if errors.Is(err, ErrReviewSuperseded) || errors.Is(err, ErrMRClosed) {
			state, detail = reviewStatusStale, "review is no longer current"
		}
		s.settleReviewStatus(ctx, payload.ProjectID, payload.MRIID, payload.Generation,
			job.Attempts, payload.HeadSHA, payload.Profile, state, detail,
			payload.NoteID, reviewAwardNone)
	}()
	control, err := s.currentControl(ctx, payload.ProjectID, payload.MRIID, payload.Generation, payload.HeadSHA)
	if err != nil {
		return err
	}
	// The generation fence passed, so this worker still owns the status note.
	s.setReviewStatus(ctx, project.ID, control.MRIID, payload.Generation, job.Attempts,
		payload.HeadSHA, payload.Profile, reviewStatusRunning, "")
	// Claim application ownership before preparing a worktree or registering
	// an executor. A concurrent recovery or lease-reclaimed handler shares the
	// same job ID and must stop here; Engine.Run's late ErrAlreadyRunning would
	// let the loser unregister the winner's executor and delete its worktree.
	if !s.executors.Acquire(job.ID) {
		return fmt.Errorf("app: review job %s is already owned by another handler", job.ID)
	}
	defer s.executors.Release(job.ID)
	workflow, storedDossier, resumable, err := s.resumableWorkflow(ctx, job.ID, payload.Generation, payload.HeadSHA)
	if err != nil {
		return err
	}
	inputs, err := s.collectInputs(ctx, project, token, control.MRIID, payload.HeadSHA)
	if err != nil {
		return err
	}

	cfg := s.resolveConfig(project, inputs.RepoDocs)
	if payload.ActivationSeq == 0 {
		if reason := reviewFilterReason(payload.Trigger, inputs.MR, project, cfg.Settings); reason != "" {
			// Webhook filtering cannot read repository configuration without a
			// network call. When repo config is enabled the worker makes the final
			// decision here, after the target-branch config is merged. An
			// activated candidate already made that decision.
			s.logger.Info("review job filtered after configuration merge",
				"project_id", project.ID, "mr_iid", control.MRIID, "trigger", payload.Trigger, "reason", reason)
			s.settleReviewStatus(ctx, project.ID, control.MRIID, payload.Generation,
				job.Attempts, payload.HeadSHA, payload.Profile, reviewStatusStale,
				"review skipped: "+reason, payload.NoteID, reviewAwardNone)
			return nil
		}
	}
	if !resumable {
		// Filtering happens first so a draft or excluded-label update never
		// cancels the previous review before the worker knows it is filtered.
		if err := s.cancelActive(ctx, control.ProjectID, control.MRIID, payload.Generation); err != nil {
			return err
		}
	}
	configHash := reviewConfigHash(cfg)
	promptHash := s.prompts.Hash
	if workflow != nil {
		changed, err := dossierConfigChanged(workflow, storedDossier, configHash, promptHash)
		if err != nil {
			return err
		}
		if changed {
			// A successor generation may already own this MR. Re-check before
			// cancelling or detaching so a stale worker never touches it.
			if _, err := s.currentControl(ctx, project.ID, control.MRIID, payload.Generation, payload.HeadSHA); err != nil {
				return err
			}
			// The stored dossier was built for another configuration or prompt
			// bundle. Keep it for audit, cancel this workflow only, and free the
			// job mapping so this retry can record a fresh dossier.
			if resumable {
				if err := s.cancelWorkflow(ctx, workflow.ID); err != nil {
					return err
				}
			}
			if err := s.store.DetachWorkflowJob(ctx, workflow.ID, job.ID); err != nil &&
				!errors.Is(err, store.ErrNotFound) {
				return err
			}
			s.logger.Info("review workflow superseded by configuration or prompt change",
				"workflow_id", workflow.ID, "job_id", job.ID)
			resumable = false
		}
	}
	files, ignored, summary, warnings := buildReviewFiles(inputs.Diffs, cfg.Settings.Ignore, cfg.Settings.MaxFiles, cfg.Settings.MaxDiffLines)
	cfg.Warnings = append(cfg.Warnings, warnings...)
	if len(files) == 0 {
		// Everything is ignored: publish an informational summary without a
		// workflow. Re-check the generation because collection takes time.
		if _, err := s.currentControl(ctx, project.ID, control.MRIID, payload.Generation, payload.HeadSHA); err != nil {
			return err
		}
		return s.publishEmptyWithNote(ctx, project, token, control, payload.NoteID, inputs, cfg, summary, ignored)
	}
	head, cleanup, err := s.prepareRun(ctx, project, token, control.MRIID, job.ID, payload.HeadSHA)
	if err != nil {
		return err
	}
	defer cleanup()

	profileName, profileCfg := s.profileFor(project, payload.Profile, cfg.Profile, inputs.MR.Labels, profileExplicit(payload))
	profile := review.Profile{Name: profileName, Config: profileCfg}
	contextDoc := s.contextDoc(ctx, project.ID)

	trigger := payload.Trigger
	if trigger == "" || trigger == "manual" {
		// "manual" is an ingress-only marker; persisted workflows use "api".
		trigger = "api"
	}
	incremental := s.incrementalRange(ctx, project.ID, payload, control, payload.HeadSHA, files)
	dossier, err := review.BuildDossier(review.DossierInput{
		Project: review.ProjectRef{ID: project.ID, Path: project.PathWithNamespace, WebURL: project.WebURL},
		MR:      mrMeta(inputs.MR),
		DiffVersion: gitlab.Version{
			ID:       inputs.Version.ID,
			BaseSHA:  inputs.Version.BaseSHA,
			StartSHA: inputs.Version.StartSHA,
			HeadSHA:  inputs.Version.HeadSHA,
		},
		DiffSummary: summary,
		Files:       files,
		Ignored:     ignored,
		Pipeline:    inputs.Pipeline,
		Issues:      inputs.Issues,
		RepoDocs:    inputs.RepoDocs,
		ContextDocs: contextDoc,
		Language:    cfg.Settings.Language,
		CustomRules: cfg.Settings.CustomRules,
		Incremental: incremental,
		HeadDir:     head,
		BaseDir:     s.workspace.Get(project.ID).BaseDir(),
		Metadata: map[string]string{
			"trigger":     trigger,
			"base_sha":    inputs.Version.BaseSHA,
			"config_hash": configHash,
			"prompt_hash": promptHash,
			"note_id":     itoa(payload.NoteID),
		},
		GeneratedAt: s.now().UTC(),
	})
	if err != nil {
		return PermanentJob(fmt.Errorf("build dossier: %w", err))
	}
	dossierData, err := review.MarshalDossier(dossier)
	if err != nil {
		return PermanentJob(fmt.Errorf("marshal dossier: %w", err))
	}
	dossierHash, err := review.DossierHash(dossier)
	if err != nil {
		return PermanentJob(fmt.Errorf("hash dossier: %w", err))
	}

	graphCfg := review.GraphConfig{AllowedProviders: s.allowedProviders()}
	spec, err := review.BuildGraph(profile, dossier, graphCfg)
	if err != nil {
		return PermanentJob(fmt.Errorf("build review graph: %w", err))
	}

	meta := map[string]string{
		"trigger":     trigger,
		"base_sha":    inputs.Version.BaseSHA,
		"fingerprint": reviewFingerprint(dossierHash, configHash, promptHash),
		"config_hash": configHash,
		"prompt_hash": promptHash,
		"profile":     profileName,
	}
	wfID, reused, err := s.store.CreateReviewWorkflow(ctx, sqlite.ReviewWorkflowRequest{
		JobID:              job.ID,
		Spec:               spec,
		Meta:               meta,
		DossierVersion:     review.DossierSchemaVersion,
		DossierHash:        dossierHash,
		DossierData:        dossierData,
		ExpectedGeneration: payload.Generation,
		ProjectID:          project.ID,
		MRIID:              control.MRIID,
		HeadSHA:            payload.HeadSHA,
	})
	if err != nil {
		if errors.Is(err, store.ErrSuperseded) {
			s.logger.Info("review workflow superseded before creation", "project_id", project.ID, "mr_iid", payload.MRIID)
			return nil
		}
		if errors.Is(err, store.ErrIncompleteWorkflow) {
			return PermanentJob(fmt.Errorf("resume review workflow: %w", err))
		}
		return err
	}

	// A push may land between CreateReviewWorkflow and the event append.
	if _, err := s.currentControl(ctx, project.ID, control.MRIID, payload.Generation, payload.HeadSHA); err != nil {
		if IsPermanentJob(err) {
			if cancelErr := s.engine.Cancel(context.WithoutCancel(ctx), wfID, "superseded"); cancelErr != nil && !errors.Is(cancelErr, dag.ErrNotFound) {
				s.logger.Warn("cancel superseded workflow failed", "workflow_id", wfID, "error", cancelErr.Error())
			}
			return nil
		}
		return err
	}

	if reused || resumable {
		// A retried job can reuse a workflow left with running nodes by a
		// crash; restart those nodes before Engine.Run rejects the graph.
		if err := s.engine.Recover(ctx); err != nil {
			return err
		}
	}

	diffIndex := gitlab.BuildDiffIndex(gitlab.Version{
		BaseSHA:  inputs.Version.BaseSHA,
		StartSHA: inputs.Version.StartSHA,
		HeadSHA:  inputs.Version.HeadSHA,
	}, inputs.Diffs)
	executor, err := review.NewExecutor(review.ExecutorDeps{
		Dossier:  dossier,
		Profile:  profile,
		Graph:    graphCfg,
		Blocks:   s.blocks,
		Clients:  s.clients,
		Prompts:  s.prompts,
		AgentRun: s.agentRun,
		Snapshot: engineSnapshot(s.engine),
		Tools:    s.tools,
		Skills:   s.skills,
		Locate:   locateFunc(diffIndex),
		Cost:     s.cost,
	})
	if err != nil {
		return PermanentJob(fmt.Errorf("build review executor: %w", err))
	}
	s.executors.Register(wfID, executor)
	defer s.executors.Unregister(wfID)

	superseded, runErr := s.runWithControl(ctx, project.ID, control.MRIID, wfID, payload.Generation, payload.HeadSHA)
	if runErr != nil {
		return runErr
	}
	if superseded {
		s.logger.Info("review superseded while running", "workflow_id", wfID)
		s.settleRunSuperseded(ctx, payload, wfID)
		return s.skipPublication(ctx, wfID, reviewSupersededReason)
	}

	snapshot, err := s.engine.Snapshot(ctx, wfID)
	if err != nil {
		return err
	}
	switch snapshot.Status {
	case dag.WorkflowCompleted:
		s.setReviewStatus(ctx, project.ID, control.MRIID, payload.Generation, job.Attempts,
			payload.HeadSHA, payload.Profile, reviewStatusPublishing, "")
		if err := s.publish(ctx, wfID); err != nil {
			s.settlePublishFailure(ctx, payload, job.Attempts, err)
			return err
		}
		state, reaction := reviewStatusForVerdict(s.workflowVerdict(ctx, wfID))
		s.settleReviewStatus(ctx, project.ID, control.MRIID, payload.Generation, job.Attempts,
			payload.HeadSHA, payload.Profile, state, "", payload.NoteID, reaction)
		return nil
	case dag.WorkflowFailed:
		s.setReviewStatus(ctx, project.ID, control.MRIID, payload.Generation, job.Attempts,
			payload.HeadSHA, payload.Profile, reviewStatusPublishing, "")
		var publishErr error
		if cfg.Settings.PostFailureNote {
			publishErr = s.publishFailure(ctx, wfID, snapshot)
		} else {
			publishErr = s.skipPublication(ctx, wfID, failureNoteDisabledReason)
		}
		s.settleReviewStatus(ctx, project.ID, control.MRIID, payload.Generation, job.Attempts,
			payload.HeadSHA, payload.Profile, reviewStatusFailed, "", payload.NoteID, reviewAwardFail)
		return publishErr
	case dag.WorkflowCancelled:
		publishErr := s.skipPublication(ctx, wfID, reviewCancelledReason)
		s.settleReviewStatus(ctx, project.ID, control.MRIID, payload.Generation, job.Attempts,
			payload.HeadSHA, payload.Profile, reviewStatusCancelled, "", payload.NoteID, reviewAwardNone)
		return publishErr
	default:
		return nil
	}
}

// settleRunSuperseded writes a terminal state only while this generation still
// owns the MR. A successor's status is left untouched.
func (s *ReviewService) settleRunSuperseded(ctx context.Context, payload reviewRunPayload, workflowID string) {
	control, err := s.store.GetReviewControl(ctx, payload.ProjectID, payload.MRIID)
	if err != nil || control.Generation != payload.Generation {
		return
	}
	state := reviewStatusStale
	detail := "merge request changed before publication"
	if control.Cancelled {
		state = reviewStatusCancelled
		detail = "review cancelled"
	}
	noteID := payload.NoteID
	if noteID <= 0 {
		noteID = s.workflowNoteID(ctx, workflowID)
	}
	s.settleReviewStatus(ctx, payload.ProjectID, payload.MRIID, payload.Generation, 0,
		payload.HeadSHA, payload.Profile, state, detail, noteID, reviewAwardNone)
}

// settlePublishFailure records a failed or stale publication without changing
// the workflow verdict.
func (s *ReviewService) settlePublishFailure(ctx context.Context, payload reviewRunPayload, attempt int, cause error) {
	state := reviewStatusFailed
	reaction := reviewAwardFail
	detail := "publication failed"
	if errors.Is(cause, ErrStalePublication) {
		state = reviewStatusStale
		reaction = reviewAwardNone
		detail = "review is no longer current"
	}
	s.settleReviewStatus(ctx, payload.ProjectID, payload.MRIID, payload.Generation, attempt,
		payload.HeadSHA, payload.Profile, state, detail, payload.NoteID, reaction)
}
