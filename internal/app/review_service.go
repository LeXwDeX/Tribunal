package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
	"github.com/LeXwDeX/tribunal/pkg/dag"
	"github.com/LeXwDeX/tribunal/prompts"
	"go.yaml.in/yaml/v3"
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

// CancelReview executes one review.cancel job. It never publishes.
func (s *ReviewService) CancelReview(ctx context.Context, job *jobs.Job) error {
	var payload reviewCancelPayload
	if err := decodeReviewPayload(job.Payload, &payload); err != nil {
		return err
	}
	if payload.ProjectID <= 0 || payload.MRIID <= 0 || payload.NoteID < 0 {
		return PermanentJob(fmt.Errorf("invalid review.cancel payload"))
	}
	err := s.cancelReview(ctx, payload)
	if err == nil || IsPermanentJob(err) {
		// The /review cancel note is this job's own receipt. Settle it whenever
		// the cancel reaches a conclusion, including when a successor replaced
		// it; the generation fence keeps the successor's status intact.
		s.settleCancellation(ctx, payload)
	}
	return err
}

func (s *ReviewService) cancelReview(ctx context.Context, payload reviewCancelPayload) error {
	control, err := s.store.GetReviewControl(ctx, payload.ProjectID, payload.MRIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return PermanentJob(fmt.Errorf("review control is missing"))
		}
		return err
	}
	if !control.Cancelled {
		// A newer review.run replaced this cancel; do not touch it.
		return nil
	}
	if payload.Generation > 0 && control.Generation != payload.Generation {
		return nil
	}
	active, err := s.store.FindActiveReview(ctx, payload.ProjectID, payload.MRIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The review was still queued; there is no workflow to cancel.
			return nil
		}
		return err
	}
	// The active workflow may belong to a newer generation when a push lands
	// between the control read and this lookup. Never cancel it.
	if payload.Generation > 0 {
		dossier, err := s.store.GetWorkflowDossier(ctx, active.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// A workflow without its dossier cannot be validated or
				// repaired by a retry, so settle the job instead of looping.
				return PermanentJob(fmt.Errorf("%w: workflow %s", store.ErrIncompleteWorkflow, active.ID))
			}
			return err
		}
		if dossier.Generation > payload.Generation {
			// A newer review already owns the MR. Never cancel it, but report
			// the superseded result instead of silently succeeding.
			return PermanentJob(fmt.Errorf("%w: workflow %s generation %d is newer than cancel generation %d",
				ErrReviewSuperseded, active.ID, dossier.Generation, payload.Generation))
		}
	}
	control, err = s.store.GetReviewControl(ctx, payload.ProjectID, payload.MRIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if !control.Cancelled || (payload.Generation > 0 && control.Generation != payload.Generation) {
		return nil
	}
	if err := s.engine.Cancel(ctx, active.ID, "user_cancelled"); err != nil {
		if errors.Is(err, dag.ErrNotFound) {
			return s.skipPublication(ctx, active.ID, reviewCancelledReason)
		}
		return err
	}
	if err := s.waitTerminal(ctx, active.ID); err != nil {
		return err
	}
	return s.skipPublication(ctx, active.ID, reviewCancelledReason)
}

// RepublishReview executes one review.publish job. It delegates to the
// publisher so retries and stale-fingerprint handling stay in one place.
func (s *ReviewService) RepublishReview(ctx context.Context, job *jobs.Job) error {
	var payload reviewPublishPayload
	if err := decodeReviewPayload(job.Payload, &payload); err != nil {
		return err
	}
	if strings.TrimSpace(payload.WorkflowID) == "" {
		return PermanentJob(fmt.Errorf("invalid review.publish payload"))
	}
	if s.publisher == nil {
		return errors.New("app: review publisher is not configured")
	}
	return s.publisher.Republish(ctx, payload.WorkflowID)
}

func (s *ReviewService) projectToken(ctx context.Context, projectID int64) (*store.Project, string, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, "", PermanentJob(fmt.Errorf("project %d is not registered", projectID))
		}
		return nil, "", err
	}
	if project == nil || !project.Enabled {
		return nil, "", PermanentJob(fmt.Errorf("project %d is disabled", projectID))
	}
	token, err := s.cipher.Decrypt(project.TokenEnc)
	if err != nil {
		return nil, "", PermanentJob(fmt.Errorf("project token is unavailable"))
	}
	return project, string(token), nil
}

// resumableWorkflow returns the workflow this job is mapped to and reports
// whether a retry can resume it. A matching workflow is returned even when it
// is terminal, so the caller can compare the stored configuration and prompt
// hashes before deciding to reuse or rebuild it.
func (s *ReviewService) resumableWorkflow(ctx context.Context, jobID string, generation int64, head string) (*store.Workflow, *store.WorkflowDossier, bool, error) {
	workflow, err := s.store.GetWorkflowByJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, false, nil
		}
		return nil, nil, false, err
	}
	if workflow == nil || !strings.EqualFold(workflow.HeadSHA, head) {
		return nil, nil, false, nil
	}
	dossier, err := s.store.GetWorkflowDossier(ctx, workflow.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return workflow, nil, false, nil
		}
		return nil, nil, false, err
	}
	if dossier.Generation != generation {
		return nil, nil, false, nil
	}
	return workflow, dossier, !workflowTerminal(workflow.Status), nil
}

// dossierConfigChanged compares the hashes stored with a dossier against the
// hashes this retry would use now. Rows written before the dossier metadata
// existed fall back to the workflow fingerprint, which binds the dossier,
// configuration and prompt hashes together.
func dossierConfigChanged(workflow *store.Workflow, dossier *store.WorkflowDossier, configHash, promptHash string) (bool, error) {
	if dossier == nil {
		return false, nil
	}
	var stored review.Dossier
	if err := json.Unmarshal(dossier.Data, &stored); err != nil {
		return false, fmt.Errorf("decode dossier: %w", err)
	}
	storedConfig := strings.TrimSpace(stored.Metadata["config_hash"])
	storedPrompt := strings.TrimSpace(stored.Metadata["prompt_hash"])
	if storedConfig != "" && storedPrompt != "" {
		return storedConfig != configHash || storedPrompt != promptHash, nil
	}
	if workflow.Fingerprint == "" {
		return false, nil
	}
	return workflow.Fingerprint != reviewFingerprint(dossier.Hash, configHash, promptHash), nil
}

func workflowTerminal(status string) bool {
	switch status {
	case string(dag.WorkflowCompleted), string(dag.WorkflowFailed), string(dag.WorkflowCancelled):
		return true
	}
	return false
}

// currentControl verifies the persisted generation before any work.
func (s *ReviewService) currentControl(ctx context.Context, projectID, mrIID, generation int64, head string) (*store.ReviewControl, error) {
	control, err := s.store.GetReviewControl(ctx, projectID, mrIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, PermanentJob(fmt.Errorf("review control is missing"))
		}
		return nil, err
	}
	if control.Cancelled || control.Generation != generation || !strings.EqualFold(control.HeadSHA, head) {
		return nil, PermanentJob(fmt.Errorf("%w: generation %d head %s is not current", ErrReviewSuperseded, generation, head))
	}
	return control, nil
}

// cancelActive cancels the MR's active review when the control still belongs
// to this worker's generation and the active workflow is not newer. A stale
// worker never cancels a newer generation; a new review still cancels the
// previous generation.
func (s *ReviewService) cancelActive(ctx context.Context, projectID, mrIID, generation int64) error {
	control, err := s.store.GetReviewControl(ctx, projectID, mrIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if control.Cancelled || control.Generation != generation {
		return nil
	}
	active, err := s.store.FindActiveReview(ctx, projectID, mrIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	dossier, err := s.store.GetWorkflowDossier(ctx, active.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if dossier.Generation > generation {
		return nil
	}
	return s.cancelWorkflow(ctx, active.ID)
}

// cancelWorkflow cancels one workflow, waits for its durable terminal state,
// and settles publication as skipped. A superseded review must never keep an
// SSE stream waiting on a publication that will not happen.
func (s *ReviewService) cancelWorkflow(ctx context.Context, workflowID string) error {
	if err := s.engine.Cancel(ctx, workflowID, "superseded"); err != nil {
		if errors.Is(err, dag.ErrNotFound) {
			return s.skipPublication(ctx, workflowID, reviewSupersededReason)
		}
		return err
	}
	if err := s.waitTerminal(ctx, workflowID); err != nil {
		if errors.Is(err, dag.ErrNotFound) {
			return s.skipPublication(ctx, workflowID, reviewSupersededReason)
		}
		return err
	}
	return s.skipPublication(ctx, workflowID, reviewSupersededReason)
}

// skipPublication settles a pending publication without publishing. The CAS
// never overwrites a published or failed result. Cleanup uses a detached
// context so a cancelled run still records its terminal publication state.
func (s *ReviewService) skipPublication(ctx context.Context, workflowID, reason string) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := s.store.SetWorkflowPublicationStateIfPending(writeCtx, workflowID, reason)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

func (s *ReviewService) waitTerminal(ctx context.Context, workflowID string) error {
	deadline := time.Now().Add(s.cancelWait)
	for {
		snapshot, err := s.engine.Snapshot(ctx, workflowID)
		if err != nil {
			return err
		}
		if snapshot.Terminal() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("app: workflow %s did not reach a terminal state", workflowID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.cancelPoll):
		}
	}
}

// runWithControl runs a workflow and watches the persisted review generation.
// A superseded run is cancelled through the engine and never published. When
// ctx ends (shutdown or lost job lease) the run is only interrupted: the
// workflow stays unfinished for recovery and ctx's error is returned so the
// job is not settled as done.
func (s *ReviewService) runWithControl(ctx context.Context, projectID, mrIID int64, workflowID string, generation int64, head string) (bool, error) {
	runCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancel(nil)
	done := make(chan error, 1)
	go func() { done <- s.engine.Run(runCtx, workflowID) }()

	ticker := time.NewTicker(s.cancelPoll)
	defer ticker.Stop()
	superseded := false
	for {
		select {
		case err := <-done:
			if ctx.Err() != nil && !superseded {
				return false, ctx.Err()
			}
			if err != nil {
				if errors.Is(err, context.Canceled) || ctx.Err() != nil {
					return superseded, nil
				}
				var transition *dag.TransitionError
				if errors.As(err, &transition) {
					return superseded, nil
				}
				return superseded, err
			}
			return superseded, nil
		case <-ctx.Done():
			cancel(dag.ErrInterrupted)
			<-done
			if superseded {
				return true, nil
			}
			return false, ctx.Err()
		case <-ticker.C:
			control, err := s.store.GetReviewControl(ctx, projectID, mrIID)
			if err != nil {
				continue
			}
			if control.Cancelled || control.Generation != generation || control.HeadSHA != head {
				if !superseded {
					superseded = true
					if err := s.engine.Cancel(context.WithoutCancel(ctx), workflowID, "superseded"); err != nil && !errors.Is(err, dag.ErrNotFound) {
						s.logger.Warn("review cancel failed", "workflow_id", workflowID)
					}
				}
			}
		}
	}
}

func (s *ReviewService) publish(ctx context.Context, workflowID string) error {
	if s.publisher == nil {
		return errors.New("app: review publisher is not configured")
	}
	return s.publisher.Publish(ctx, workflowID)
}

func (s *ReviewService) publishFailure(ctx context.Context, workflowID string, snapshot dag.WorkflowView) error {
	if s.publisher == nil {
		return errors.New("app: review publisher is not configured")
	}
	return s.publisher.PublishFailure(ctx, workflowID, failureReason(snapshot))
}

// failureReason returns a safe, bounded reason for a published failure note.
// Raw node and workflow errors stay in logs; they can contain upstream text or
// credentials and must not reach the merge request.
func failureReason(snapshot dag.WorkflowView) string {
	for _, id := range snapshot.Order {
		node := snapshot.Nodes[id]
		if node.Status == dag.NodeFailed {
			return "node " + truncateRunes(id, 64) + " failed"
		}
	}
	return reviewExecutionFailedReason
}

// setReviewStatus records the desired status note. A failure here never
// changes the review verdict; the durable row and reconciliation retry it.
func (s *ReviewService) setReviewStatus(ctx context.Context, projectID, mrIID, generation int64, attempt int, head, profile, state, detail string) {
	if generation <= 0 {
		return
	}
	if _, err := s.store.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID:  projectID,
		MRIID:      mrIID,
		Generation: generation,
		Attempt:    attempt,
		HeadSHA:    head,
		Profile:    profile,
		State:      state,
		Detail:     detail,
	}); err != nil && !errors.Is(err, store.ErrSuperseded) {
		s.logger.Warn("review status goal write failed", "project_id", projectID,
			"mr_iid", mrIID, "generation", generation, "error", err.Error())
	}
}

// setReviewReaction records the desired reaction for one source note.
func (s *ReviewService) setReviewReaction(ctx context.Context, projectID, mrIID, noteID, generation int64, desired string) {
	if noteID <= 0 || generation <= 0 {
		return
	}
	if _, err := s.store.SetReviewReaction(ctx, store.ReviewReactionGoal{
		ProjectID:  projectID,
		MRIID:      mrIID,
		NoteID:     noteID,
		Generation: generation,
		Desired:    desired,
	}); err != nil && !errors.Is(err, store.ErrSuperseded) {
		s.logger.Warn("review reaction goal write failed", "project_id", projectID,
			"mr_iid", mrIID, "note_id", noteID, "error", err.Error())
	}
}

func (s *ReviewService) settleReviewStatus(ctx context.Context, projectID, mrIID, generation int64, attempt int, head, profile, state, detail string, noteID int64, reaction string) {
	s.setReviewStatus(ctx, projectID, mrIID, generation, attempt, head, profile, state, detail)
	if noteID > 0 {
		if reaction == "" {
			reaction = reviewAwardNone
		}
		s.setReviewReaction(ctx, projectID, mrIID, noteID, generation, reaction)
	}
}

// estimateReviewMinutes returns a coarse product estimate. A query failure
// keeps the default; it never fails a review or a status job.
func (s *ReviewService) estimateReviewMinutes(ctx context.Context, projectID int64, profile string) int {
	const (
		defaultMinutes = 5
		minMinutes     = 1
		maxMinutes     = 30
	)
	profile = strings.TrimSpace(profile)
	if profile == "" {
		profile = "default"
	}
	durations, err := s.store.RecentReviewDurations(ctx, projectID, profile, s.now().UTC().Add(-30*24*time.Hour), 20)
	if err != nil {
		s.logger.Warn("review estimate query failed", "project_id", projectID, "error", err.Error())
		return defaultMinutes
	}
	if len(durations) == 0 {
		return defaultMinutes
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	median := durations[len(durations)/2]
	if len(durations)%2 == 0 {
		median = (durations[len(durations)/2-1] + durations[len(durations)/2]) / 2
	}
	minutes := int((median + time.Minute - 1) / time.Minute)
	if minutes < minMinutes {
		minutes = minMinutes
	}
	if minutes > maxMinutes {
		minutes = maxMinutes
	}
	return minutes
}

type reviewSideEffectPayload struct {
	ProjectID int64 `json:"project_id"`
	MRIID     int64 `json:"mr_iid"`
	NoteID    int64 `json:"note_id,omitempty"`
}

func decodeReviewSideEffectPayload(raw json.RawMessage, wantNote bool) (reviewSideEffectPayload, error) {
	var payload reviewSideEffectPayload
	if err := decodeReviewPayload(raw, &payload); err != nil {
		return payload, err
	}
	if payload.ProjectID <= 0 || payload.MRIID <= 0 {
		return payload, PermanentJob(fmt.Errorf("invalid review side-effect payload"))
	}
	if wantNote && payload.NoteID <= 0 {
		return payload, PermanentJob(fmt.Errorf("invalid review.reaction payload"))
	}
	return payload, nil
}

// RunStatusJob publishes one durable status row. The row version is a CAS: a
// newer desired state written while GitLab was called stays pending for its
// own job and reconciliation.
func (s *ReviewService) RunStatusJob(ctx context.Context, job *jobs.Job) error {
	payload, err := decodeReviewSideEffectPayload(job.Payload, false)
	if err != nil {
		return err
	}
	row, err := s.store.GetReviewStatus(ctx, payload.ProjectID, payload.MRIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if row.AppliedVersion >= row.Version {
		return nil
	}
	project, token, err := s.projectToken(ctx, row.ProjectID)
	if err != nil {
		// A disabled project keeps the row pending; reconciliation resumes it
		// after the project is enabled again.
		return err
	}
	control, err := s.store.GetReviewControl(ctx, row.ProjectID, row.MRIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return s.markStatusApplied(ctx, row, 0, "", "review control is missing")
		}
		return err
	}
	if control.Generation != row.Generation ||
		(row.HeadSHA != "" && !strings.EqualFold(control.HeadSHA, row.HeadSHA)) {
		return s.markStatusApplied(ctx, row, 0, "", "review superseded")
	}
	if control.Cancelled && row.State != reviewStatusCancelled && row.State != reviewStatusStale {
		return s.markStatusApplied(ctx, row, 0, "", "review cancelled")
	}
	if s.status == nil {
		return errors.New("app: status reporter is not configured")
	}
	update := reviewStatusUpdate{
		State:      row.State,
		Detail:     row.Detail,
		HeadSHA:    row.HeadSHA,
		Generation: row.Generation,
	}
	switch row.State {
	case reviewStatusQueued, reviewStatusRunning, reviewStatusPublishing:
		update.EstimateMinutes = s.estimateReviewMinutes(ctx, row.ProjectID, row.Profile)
	}
	noteID, hash, err := s.status.writeStatus(ctx, project.ID, row.MRIID, token, update)
	if err != nil {
		if reviewSideEffectRetryable(err) {
			return err
		}
		return s.markStatusApplied(ctx, row, 0, "", reviewSideEffectReason(err))
	}
	return s.markStatusApplied(ctx, row, noteID, hash, "")
}

func (s *ReviewService) markStatusApplied(ctx context.Context, row *store.ReviewStatus, noteID int64, bodyHash, reason string) error {
	err := s.store.MarkReviewStatusApplied(ctx, row.ProjectID, row.MRIID, int64(row.Version), noteID, bodyHash, reason)
	if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("app: mark review status applied: %w", err)
	}
	return nil
}

// RunReactionJob publishes one durable reaction row. Only the bot's own awards
// are changed; a stale generation may remove its own eyes but never publishes
// a success or failure over a newer review.
func (s *ReviewService) RunReactionJob(ctx context.Context, job *jobs.Job) error {
	payload, err := decodeReviewSideEffectPayload(job.Payload, true)
	if err != nil {
		return err
	}
	row, err := s.store.GetReviewReaction(ctx, payload.ProjectID, payload.MRIID, payload.NoteID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if row.AppliedVersion >= row.Version {
		return nil
	}
	project, token, err := s.projectToken(ctx, row.ProjectID)
	if err != nil {
		return err
	}
	control, err := s.store.GetReviewControl(ctx, row.ProjectID, row.MRIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return s.markReactionApplied(ctx, row, "review control is missing")
		}
		return err
	}
	if row.Desired != reviewAwardNone {
		if control.Generation != row.Generation {
			return s.markReactionApplied(ctx, row, "generation superseded")
		}
		if control.Cancelled {
			return s.markReactionApplied(ctx, row, "review cancelled")
		}
	}
	if s.status == nil {
		return errors.New("app: status reporter is not configured")
	}
	err = s.status.applyReaction(ctx, project.ID, row.MRIID, token, row.NoteID, row.Desired)
	if err != nil {
		if errors.Is(err, errReactionNoteGone) {
			return s.markReactionApplied(ctx, row, "note not found")
		}
		if reviewSideEffectRetryable(err) {
			return err
		}
		return s.markReactionApplied(ctx, row, reviewSideEffectReason(err))
	}
	return s.markReactionApplied(ctx, row, "")
}

func (s *ReviewService) markReactionApplied(ctx context.Context, row *store.ReviewReaction, reason string) error {
	err := s.store.MarkReviewReactionApplied(ctx, row.ProjectID, row.MRIID, row.NoteID, int64(row.Version), reason)
	if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("app: mark review reaction applied: %w", err)
	}
	return nil
}

// reviewSideEffectRetryable separates a transient upstream failure from a
// terminal one. Transport, timeout, 5xx and 429 retry; authentication,
// permission, missing resource and validation errors settle the row.
func reviewSideEffectRetryable(err error) bool {
	if err == nil || errors.Is(err, errReactionNoteGone) {
		return false
	}
	switch gitlab.StatusOf(err) {
	case 401, 403, 404, 422:
		return false
	}
	return true
}

func reviewSideEffectReason(err error) string {
	if err == nil {
		return ""
	}
	return truncateRunes(sanitizeStatusDetail(err.Error()), 300)
}

// settleCancellation records the terminal cancel status and removes the
// cancel/source eyes. If a newer run already owns the MR the generation fence
// rejects the status write; removing the old note's eyes is still safe.
func (s *ReviewService) settleCancellation(ctx context.Context, payload reviewCancelPayload) {
	if payload.Generation <= 0 {
		return
	}
	s.setReviewStatus(ctx, payload.ProjectID, payload.MRIID, payload.Generation, 0,
		"", "", reviewStatusCancelled, "review cancelled")
	s.setReviewReaction(ctx, payload.ProjectID, payload.MRIID, payload.NoteID, payload.Generation, reviewAwardNone)
	if _, err := s.store.ClearReviewReactionsForGeneration(ctx, payload.ProjectID, payload.MRIID, payload.Generation); err != nil {
		s.logger.Warn("review cancel reaction cleanup failed", "project_id", payload.ProjectID,
			"mr_iid", payload.MRIID, "generation", payload.Generation, "error", err.Error())
	}
}

// ReconcileSideEffects re-enqueues pending durable writes and settles work
// that no live job can finish. It never re-runs a model.
func (s *ReviewService) ReconcileSideEffects(ctx context.Context) error {
	var errs []error
	now := s.now().UTC()

	statuses, err := s.store.ListPendingReviewStatuses(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	for _, row := range statuses {
		if err := s.enqueueReviewSideEffectJob(ctx, "review.status", "review.status:"+itoa(row.ProjectID)+":"+itoa(row.MRIID), map[string]any{
			"project_id": row.ProjectID, "mr_iid": row.MRIID,
		}); err != nil {
			errs = append(errs, err)
		}
	}
	reactions, err := s.store.ListPendingReviewReactions(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	for _, row := range reactions {
		if err := s.enqueueReviewSideEffectJob(ctx, "review.reaction",
			"review.reaction:"+itoa(row.ProjectID)+":"+itoa(row.MRIID)+":"+itoa(row.NoteID), map[string]any{
				"project_id": row.ProjectID, "mr_iid": row.MRIID, "note_id": row.NoteID,
			}); err != nil {
			errs = append(errs, err)
		}
	}

	staleEyes, err := s.store.ListReviewReactionsByDesired(ctx, reviewAwardEyes, now.Add(-15*time.Minute))
	if err != nil {
		errs = append(errs, err)
	}
	for _, row := range staleEyes {
		active, err := s.store.HasActiveReviewJob(ctx, row.ProjectID, row.MRIID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if active {
			continue
		}
		if _, err := s.store.FindActiveReview(ctx, row.ProjectID, row.MRIID); err == nil {
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, err)
			continue
		}
		if _, err := s.store.SetReviewReaction(ctx, store.ReviewReactionGoal{
			ProjectID: row.ProjectID, MRIID: row.MRIID, NoteID: row.NoteID,
			Generation: row.Generation, Desired: store.ReviewReactionNone,
		}); err != nil {
			errs = append(errs, err)
		}
	}

	unfinished, err := s.store.ListReviewStatusesByState(ctx,
		[]string{reviewStatusRunning, reviewStatusPublishing}, now.Add(-2*time.Hour))
	if err != nil {
		errs = append(errs, err)
	}
	for _, row := range unfinished {
		active, err := s.store.HasActiveReviewJob(ctx, row.ProjectID, row.MRIID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if active {
			continue
		}
		if _, err := s.store.FindActiveReview(ctx, row.ProjectID, row.MRIID); err == nil {
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, err)
			continue
		}
		// A completed or cancelled workflow is not "active", but declaring the
		// status failed would contradict a publication that already exists.
		// Recover its terminal state when the generation still matches.
		if wf, err := s.store.LatestReviewWorkflow(ctx, row.ProjectID, row.MRIID); err == nil {
			record, dossierErr := s.store.GetWorkflowDossier(ctx, wf.ID)
			if dossierErr != nil {
				if !errors.Is(dossierErr, store.ErrNotFound) {
					errs = append(errs, dossierErr)
				}
				continue
			}
			var dossier review.Dossier
			if err := json.Unmarshal(record.Data, &dossier); err != nil {
				errs = append(errs, fmt.Errorf("app: decode recovery dossier: %w", err))
				continue
			}
			if record.Generation != row.Generation ||
				!strings.EqualFold(dossier.DiffVersion.HeadSHA, row.HeadSHA) {
				continue
			}
			var state, detail string
			var reaction string
			switch wf.Status {
			case string(dag.WorkflowCompleted):
				state, reaction = reviewStatusForVerdict(wf.Verdict)
			case string(dag.WorkflowFailed):
				state, reaction = reviewStatusFailed, reviewAwardFail
			case string(dag.WorkflowCancelled):
				state, reaction = reviewStatusCancelled, reviewAwardNone
			default:
				continue
			}
			if _, err := s.store.SetReviewStatus(ctx, store.ReviewStatusGoal{
				ProjectID: row.ProjectID, MRIID: row.MRIID, Generation: row.Generation,
				Attempt: row.Attempt, HeadSHA: row.HeadSHA, Profile: row.Profile,
				State: state, Detail: detail,
			}); err != nil && !errors.Is(err, store.ErrSuperseded) {
				errs = append(errs, err)
				continue
			}
			noteID := dossierNoteID(dossier)
			if noteID > 0 {
				if _, err := s.store.SetReviewReaction(ctx, store.ReviewReactionGoal{
					ProjectID: row.ProjectID, MRIID: row.MRIID, NoteID: noteID,
					Generation: row.Generation, Desired: reaction,
				}); err != nil && !errors.Is(err, store.ErrSuperseded) {
					errs = append(errs, err)
				}
			}
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, err)
			continue
		}
		if _, err := s.store.SetReviewStatus(ctx, store.ReviewStatusGoal{
			ProjectID: row.ProjectID, MRIID: row.MRIID, Generation: row.Generation,
			Attempt: row.Attempt, HeadSHA: row.HeadSHA, Profile: row.Profile,
			State: reviewStatusFailed, Detail: "review worker ended without terminal status",
		}); err != nil && !errors.Is(err, store.ErrSuperseded) {
			errs = append(errs, err)
		}
	}

	queued, err := s.store.ListReviewStatusesByState(ctx, []string{reviewStatusQueued}, now.Add(time.Second))
	if err != nil {
		errs = append(errs, err)
	}
	for _, row := range queued {
		control, err := s.store.GetReviewControl(ctx, row.ProjectID, row.MRIID)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				errs = append(errs, err)
			}
			continue
		}
		if !control.Cancelled || control.Generation != row.Generation {
			continue
		}
		active, err := s.store.HasActiveReviewJob(ctx, row.ProjectID, row.MRIID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if active {
			continue
		}
		if _, err := s.store.SetReviewStatus(ctx, store.ReviewStatusGoal{
			ProjectID: row.ProjectID, MRIID: row.MRIID, Generation: row.Generation,
			Attempt: row.Attempt, HeadSHA: row.HeadSHA, Profile: row.Profile,
			State: reviewStatusCancelled, Detail: "review cancelled",
		}); err != nil && !errors.Is(err, store.ErrSuperseded) {
			errs = append(errs, err)
			continue
		}
		if _, err := s.store.ClearReviewReactionsForGeneration(ctx, row.ProjectID, row.MRIID, row.Generation); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *ReviewService) enqueueReviewSideEffectJob(ctx context.Context, kind, key string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, _, err := s.store.Enqueue(ctx, store.Job{
		Kind: kind, Key: key, Payload: raw, MaxAttempts: sqlite.ReviewSideEffectMaxAttempts,
	}); err != nil {
		return err
	}
	return nil
}

func (s *ReviewService) workflowVerdict(ctx context.Context, workflowID string) string {
	workflow, err := s.store.GetWorkflow(ctx, workflowID)
	if err != nil || workflow == nil {
		return ""
	}
	return workflow.Verdict
}

// workflowNoteID reads the command source note from the persisted dossier so a
// retried or recovered workflow can still settle its reaction.
func (s *ReviewService) workflowNoteID(ctx context.Context, workflowID string) int64 {
	if strings.TrimSpace(workflowID) == "" {
		return 0
	}
	record, err := s.store.GetWorkflowDossier(ctx, workflowID)
	if err != nil || len(record.Data) == 0 {
		return 0
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		return 0
	}
	return dossierNoteID(dossier)
}

func dossierNoteID(dossier review.Dossier) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(dossier.Metadata["note_id"]), 10, 64)
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func reviewStatusForVerdict(verdict string) (string, string) {
	switch verdict {
	case review.VerdictPass:
		return reviewStatusPass, reviewAwardPass
	case review.VerdictBlocking:
		return reviewStatusBlocking, reviewAwardFail
	default:
		return reviewStatusIncomplete, reviewAwardFail
	}
}

// reviewInputs is the sanitized GitLab input for one review.
type reviewInputs struct {
	MR       *gitlab.MergeRequest
	Version  *gitlab.Version
	Diffs    []gitlab.FileDiff
	Pipeline *review.PipelineSummary
	Issues   []review.IssueRef
	RepoDocs review.RepoDocs
}

func (s *ReviewService) collectInputs(ctx context.Context, project *store.Project, token string, mrIID int64, head string) (*reviewInputs, error) {
	mr, err := s.gitlab.GetMergeRequest(ctx, token, project.ID, mrIID)
	if err != nil {
		if gitlab.IsNotFound(err) {
			return nil, PermanentJob(fmt.Errorf("%w: merge request not found", ErrMRClosed))
		}
		return nil, err
	}
	if mr == nil || !isOpenState(mr.State) {
		return nil, PermanentJob(fmt.Errorf("%w: state %s", ErrMRClosed, mrState(mr)))
	}
	if mr.TargetProjectID > 0 && mr.TargetProjectID != project.ID {
		return nil, PermanentJob(fmt.Errorf("%w: target project mismatch", ErrReviewSuperseded))
	}
	if !strings.EqualFold(strings.TrimSpace(mr.SHA), head) {
		return nil, PermanentJob(fmt.Errorf("%w: merge request head changed", ErrReviewSuperseded))
	}
	if strings.TrimSpace(mr.TargetBranch) == "" {
		return nil, PermanentJob(fmt.Errorf("merge request target branch is empty"))
	}

	version, err := s.gitlab.LatestMRVersion(ctx, token, project.ID, mrIID)
	if err != nil {
		return nil, err
	}
	if version == nil || !strings.EqualFold(strings.TrimSpace(version.HeadSHA), head) {
		return nil, PermanentJob(fmt.Errorf("%w: diff version head changed", ErrReviewSuperseded))
	}
	diffs, err := s.gitlab.ListMRDiffs(ctx, token, project.ID, mrIID)
	if err != nil {
		return nil, err
	}
	inputs := &reviewInputs{MR: mr, Version: version, Diffs: diffs}
	inputs.Pipeline = s.collectPipeline(ctx, token, project.ID, mrIID)
	inputs.Issues = s.collectIssues(ctx, token, project.ID, mrIID)
	docs, err := s.repoDocs(ctx, token, project.ID, mr.TargetBranch)
	if err != nil {
		return nil, err
	}
	inputs.RepoDocs = docs
	return inputs, nil
}

// repoDocs reads the target-branch documents used by review and publication.
// A missing file is an empty document; every other error is retryable so a
// transient upstream failure never changes the configuration fingerprint.
func (s *ReviewService) repoDocs(ctx context.Context, token string, projectID int64, ref string) (review.RepoDocs, error) {
	configDoc, err := s.rawDoc(ctx, token, projectID, ".gitlab-review.yml", ref)
	if err != nil {
		return review.RepoDocs{}, err
	}
	agents, err := s.rawDoc(ctx, token, projectID, "AGENTS.md", ref)
	if err != nil {
		return review.RepoDocs{}, err
	}
	claude, err := s.rawDoc(ctx, token, projectID, "CLAUDE.md", ref)
	if err != nil {
		return review.RepoDocs{}, err
	}
	return review.RepoDocs{GitLabReviewConfig: configDoc, AGENTS: agents, CLAUDE: claude}, nil
}

func mrState(mr *gitlab.MergeRequest) string {
	if mr == nil {
		return "missing"
	}
	return mr.State
}

func (s *ReviewService) collectPipeline(ctx context.Context, token string, projectID, mrIID int64) *review.PipelineSummary {
	pipelines, err := s.gitlab.ListMRPipelines(ctx, token, projectID, mrIID)
	if err != nil {
		s.logger.Warn("pipeline lookup failed", "project_id", projectID, "mr_iid", mrIID)
		return nil
	}
	var latest *gitlab.Pipeline
	for i := range pipelines {
		if latest == nil || pipelines[i].ID > latest.ID {
			latest = &pipelines[i]
		}
	}
	if latest == nil {
		return nil
	}
	summary := &review.PipelineSummary{
		ID:     latest.ID,
		Status: latest.Status,
		Ref:    latest.Ref,
		SHA:    latest.SHA,
		WebURL: latest.WebURL,
	}
	if latest.Status != "failed" {
		return summary
	}
	jobs, err := s.gitlab.ListPipelineJobs(ctx, token, projectID, latest.ID)
	if err != nil {
		s.logger.Warn("pipeline jobs lookup failed", "project_id", projectID, "pipeline_id", latest.ID)
		return summary
	}
	for _, job := range jobs {
		if job.Status != "failed" || job.AllowFailure {
			continue
		}
		summary.FailedJobs = append(summary.FailedJobs, review.JobSummary{
			Name:          job.Name,
			Stage:         job.Stage,
			Status:        job.Status,
			FailureReason: job.FailureReason,
			WebURL:        job.WebURL,
		})
	}
	return summary
}

func (s *ReviewService) collectIssues(ctx context.Context, token string, projectID, mrIID int64) []review.IssueRef {
	issues, err := s.gitlab.ListClosingIssues(ctx, token, projectID, mrIID)
	if err != nil {
		s.logger.Warn("closing issues lookup failed", "project_id", projectID, "mr_iid", mrIID)
		return nil
	}
	out := make([]review.IssueRef, 0, len(issues))
	for _, issue := range issues {
		out = append(out, review.IssueRef{
			IID:    issue.IID,
			Title:  issue.Title,
			State:  issue.State,
			WebURL: issue.WebURL,
		})
	}
	return out
}

func (s *ReviewService) rawDoc(ctx context.Context, token string, projectID int64, path, ref string) (string, error) {
	data, err := s.gitlab.GetRawFile(ctx, token, projectID, path, ref)
	if err != nil {
		if !gitlab.IsNotFound(err) {
			s.logger.Warn("repository document lookup failed", "project_id", projectID, "path", path)
			return "", err
		}
		return "", nil
	}
	return string(data), nil
}

func mrMeta(mr *gitlab.MergeRequest) review.MRMeta {
	out := review.MRMeta{
		IID:          mr.IID,
		Title:        mr.Title,
		Description:  mr.Description,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
		Draft:        mr.Draft,
		Labels:       append([]string(nil), mr.Labels...),
		WebURL:       mr.WebURL,
	}
	if mr.Author != nil {
		out.Author = mr.Author.Username
	}
	return out
}

// effectiveReviewConfig is the merged platform, project, and repo view.
type effectiveReviewConfig struct {
	Settings config.ProjectSettings
	Profile  string
	RepoDocs review.RepoDocs
	Warnings []string
}

type repoReviewConfig struct {
	Version  int      `yaml:"version"`
	Language string   `yaml:"language"`
	Profile  string   `yaml:"profile"`
	Ignore   []string `yaml:"ignore"`
	Rules    []string `yaml:"rules"`
	Review   struct {
		SkipDraft     *bool    `yaml:"skip_draft"`
		LabelsExclude []string `yaml:"labels_exclude"`
	} `yaml:"review"`
}

func (s *ReviewService) resolveConfig(project *store.Project, docs review.RepoDocs) effectiveReviewConfig {
	return resolveReviewConfig(project, docs, s.reviewDefaults, s.profiles)
}

func resolveReviewConfig(project *store.Project, docs review.RepoDocs, defaults config.ProjectSettings, profiles map[string]config.ProfileConfig) effectiveReviewConfig {
	settings, err := ResolveSettings(project.Settings, defaults)
	out := effectiveReviewConfig{Settings: settings, RepoDocs: docs}
	if err != nil {
		out.Settings = cloneSettings(defaults)
		out.Warnings = append(out.Warnings, "stored project settings are invalid; platform defaults are used")
	}
	// Normalize once so the review service and publisher hash the same value.
	if out.Settings.MaxFiles <= 0 {
		out.Settings.MaxFiles = review.DefaultMaxFiles
	}
	out.Settings.Language = strings.Join(strings.Fields(out.Settings.Language), " ")
	if !out.Settings.AllowRepoConfig || strings.TrimSpace(docs.GitLabReviewConfig) == "" {
		return out
	}
	var repo repoReviewConfig
	if err := yaml.Unmarshal([]byte(docs.GitLabReviewConfig), &repo); err != nil {
		out.Warnings = append(out.Warnings, ".gitlab-review.yml is invalid and was ignored")
		return out
	}
	out.Warnings = append(out.Warnings, repoReviewConfigWarnings(docs.GitLabReviewConfig)...)
	if language := strings.Join(strings.Fields(repo.Language), " "); language != "" {
		if len([]rune(language)) > 128 {
			out.Warnings = append(out.Warnings, ".gitlab-review.yml: language is too long and was ignored")
		} else {
			out.Settings.Language = language
		}
	}
	if strings.TrimSpace(repo.Profile) != "" {
		if _, ok := profiles[repo.Profile]; ok {
			out.Profile = repo.Profile
		} else {
			out.Warnings = append(out.Warnings, "repository profile is not configured and was ignored")
		}
	}
	out.Settings.Ignore = appendUnique(out.Settings.Ignore, repo.Ignore)
	out.Settings.CustomRules = appendUnique(out.Settings.CustomRules, repo.Rules)
	if repo.Review.SkipDraft != nil {
		out.Settings.SkipDraft = *repo.Review.SkipDraft
	}
	labels := make([]string, 0, len(repo.Review.LabelsExclude))
	for _, label := range repo.Review.LabelsExclude {
		if strings.TrimSpace(label) == "" {
			out.Warnings = append(out.Warnings, ".gitlab-review.yml: empty review.labels_exclude entry ignored")
			continue
		}
		labels = append(labels, label)
	}
	out.Settings.LabelsExclude = appendUnique(out.Settings.LabelsExclude, labels)
	return out
}

// repoReviewConfigWarnings reports fields outside the repository whitelist.
// They stay unapplied; the warning is surfaced in the published summary.
func repoReviewConfigWarnings(raw string) []string {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	var warnings []string
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i].Value
		switch key {
		case "version", "language", "profile", "ignore", "rules":
		case "review":
			review := root.Content[i+1]
			if review.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(review.Content); j += 2 {
				switch review.Content[j].Value {
				case "skip_draft", "labels_exclude":
				default:
					warnings = append(warnings, fmt.Sprintf(".gitlab-review.yml: unknown field review.%s ignored", review.Content[j].Value))
				}
			}
		default:
			warnings = append(warnings, fmt.Sprintf(".gitlab-review.yml: unknown field %s ignored", key))
		}
	}
	return warnings
}

// profileExplicit distinguishes an operator-selected profile from the
// automatic webhook default. Legacy queued command/manual payloads carry no
// explicit flag, so their trigger remains the only available evidence.
func profileExplicit(payload reviewRunPayload) bool {
	if payload.ProfileExplicit != nil {
		return *payload.ProfileExplicit
	}
	switch payload.Trigger {
	case "command", "manual":
		return true
	default:
		return false
	}
}

// reviewFilterReason applies draft, label, and branch filters to automatic
// triggers only. Manual commands keep their existing bypass semantics.
func reviewFilterReason(trigger string, mr *gitlab.MergeRequest, project *store.Project, settings config.ProjectSettings) string {
	switch trigger {
	case "mr_open", "mr_update":
	default:
		return ""
	}
	if mr == nil {
		return ""
	}
	if settings.SkipDraft && mergeRequestDraft(mr) {
		return "draft"
	}
	if !LabelsAllowed(mr.Labels, settings.LabelsInclude, settings.LabelsExclude) {
		return "labels_filtered"
	}
	if !BranchAllowed(mr.TargetBranch, project.DefaultBranch, settings.TargetBranches) {
		return "branch_filtered"
	}
	return ""
}

func mergeRequestDraft(mr *gitlab.MergeRequest) bool {
	if mr == nil {
		return false
	}
	if mr.Draft {
		return true
	}
	title := strings.ToLower(strings.TrimSpace(mr.Title))
	return strings.HasPrefix(title, "draft:") || strings.HasPrefix(title, "wip:")
}

func reviewConfigHash(cfg effectiveReviewConfig) string {
	payload, err := json.Marshal(struct {
		Settings config.ProjectSettings `json:"settings"`
		Profile  string                 `json:"profile"`
		RepoDocs review.RepoDocs        `json:"repo_docs"`
	}{cfg.Settings, cfg.Profile, cfg.RepoDocs})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// reviewFingerprint binds a workflow to the exact dossier, configuration, and
// prompt bundle used to produce it. Format:
// sha256("<dossier hash>\n<config hash>\n<prompt hash>").
func reviewFingerprint(dossierHash, configHash, promptHash string) string {
	return sha256Hex(dossierHash + "\n" + configHash + "\n" + promptHash)
}

func (s *ReviewService) profileFor(project *store.Project, requested, repoProfile string, labels []string, explicit bool) (string, config.ProfileConfig) {
	var candidates []string
	if explicit {
		candidates = []string{requested, repoProfile}
	} else {
		candidates = []string{repoProfile, requested}
	}
	candidates = append(candidates, ProfileForLabels(labels, project.Profile, s.reviewDefaults), "default")
	seen := make(map[string]struct{}, len(candidates))
	for _, name := range candidates {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		if cfg, ok := s.profiles[name]; ok {
			return name, cfg
		}
	}
	return "custom", config.ProfileConfig{}
}

// allowedProviders returns the provider allowlist used by graph validation.
func (s *ReviewService) allowedProviders() []string {
	return append([]string(nil), s.providerNames...)
}

func (s *ReviewService) contextDoc(ctx context.Context, projectID int64) string {
	doc, err := s.store.GetCurrentContextDoc(ctx, projectID, "agents")
	if err != nil {
		return ""
	}
	return doc.Content
}

func (s *ReviewService) incrementalRange(ctx context.Context, projectID int64, payload reviewRunPayload, control *store.ReviewControl, head string, files []review.FileChange) *review.IncrementalRange {
	if payload.Trigger != "mr_update" || payload.Full {
		return &review.IncrementalRange{Head: head, Full: true, Reason: "full review"}
	}
	last, err := s.store.LastPublishedHead(ctx, projectID, control.MRIID)
	if err != nil || last == "" || strings.EqualFold(last, head) {
		return &review.IncrementalRange{Head: head, Full: true, Reason: "no previous reviewed head"}
	}
	// A previous published P0/P1 may live in a file this push did not touch.
	// The incremental scope cannot recheck it, so fall back to the full MR.
	severe, err := s.store.HasPublishedSevereFinding(ctx, projectID, control.MRIID)
	if err != nil {
		return &review.IncrementalRange{Head: head, LastHead: last, Full: true, Reason: "previous finding lookup failed"}
	}
	if severe {
		return &review.IncrementalRange{Head: head, LastHead: last, Full: true, Reason: "previous severe finding was not rechecked"}
	}
	ws := s.workspace.Get(projectID)
	if ws == nil {
		return nil
	}
	ancestor, err := s.git.IsAncestor(ctx, ws.MirrorDir(), last, head)
	if err != nil {
		s.logger.Warn("incremental ancestry check failed", "project_id", projectID, "mr_iid", control.MRIID)
		return &review.IncrementalRange{Head: head, LastHead: last, Full: true, Reason: "ancestry check failed"}
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	scope, err := review.ComputeIncrementalScope(review.IncrementalInput{
		LastHead:   last,
		Head:       head,
		MRFiles:    paths,
		IsAncestor: ancestor,
		LoadChanged: func(from, to string) ([]string, error) {
			return s.git.ChangedFiles(ctx, ws.MirrorDir(), from, to)
		},
	})
	if err != nil {
		return &review.IncrementalRange{Head: head, LastHead: last, Full: true, Reason: "incremental scope failed"}
	}
	if len(scope.Files) == 0 {
		return &review.IncrementalRange{Head: head, LastHead: last, Full: true, Reason: "empty incremental scope"}
	}
	return &review.IncrementalRange{
		LastHead: last,
		Head:     head,
		Files:    scope.Files,
		Full:     scope.Full,
		Reason:   scope.Reason,
	}
}

// prepareRun fetches the MR head and creates a detached worktree keyed by the
// retried job ID. The returned cleanup removes the worktree and releases the
// workspace read lock in every path.
func (s *ReviewService) prepareRun(ctx context.Context, project *store.Project, token string, mrIID int64, runID, head string) (string, func(), error) {
	ws := s.workspace.Get(project.ID)
	if ws == nil {
		return "", nil, PermanentJob(fmt.Errorf("workspace is unavailable"))
	}
	unlock, err := ws.RLock(ctx)
	if err != nil {
		return "", nil, err
	}
	var once sync.Once
	release := func() { once.Do(unlock) }
	fail := func(err error) (string, func(), error) {
		release()
		return "", nil, err
	}
	remote := trimProjectWebURL(project.WebURL)
	if remote == "" {
		return fail(PermanentJob(fmt.Errorf("project web url is empty")))
	}
	if err := ensureReviewRoot(ws.Root); err != nil {
		return fail(err)
	}
	auth := repo.Auth{Token: token}
	if err := s.git.EnsureMirror(ctx, ws.MirrorDir(), remote, auth); err != nil {
		return fail(err)
	}
	ref := fmt.Sprintf("refs/merge-requests/%d/head", mrIID)
	refspec := fmt.Sprintf("+%s:%s", ref, ref)
	if err := s.git.Fetch(ctx, ws.MirrorDir(), auth, refspec); err != nil {
		return fail(err)
	}
	resolved, err := s.git.Resolve(ctx, ws.MirrorDir(), ref)
	if err != nil {
		return fail(err)
	}
	if !strings.EqualFold(resolved, head) {
		// The mirror may lag behind the API; retry and re-check the generation.
		return fail(fmt.Errorf("app: merge request ref does not match the job head"))
	}

	runsDir := filepath.Join(ws.Root, "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		return fail(fmt.Errorf("app: create run directory: %w", err))
	}
	if info, err := os.Lstat(runsDir); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fail(fmt.Errorf("app: run directory is unsafe"))
	}
	runDir := filepath.Join(runsDir, runID)
	switch info, err := os.Lstat(runDir); {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fail(fmt.Errorf("app: run path is not a directory"))
		}
		current, resolveErr := s.git.Resolve(ctx, runDir, "HEAD")
		if resolveErr == nil && strings.EqualFold(current, head) {
			break
		}
		// A missing or interrupted worktree is rebuilt from the mirror.
		if err := removeRunDir(runDir); err != nil {
			return fail(err)
		}
		if err := s.git.AddWorktree(ctx, ws.MirrorDir(), runDir, head); err != nil {
			return fail(err)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := s.git.AddWorktree(ctx, ws.MirrorDir(), runDir, head); err != nil {
			return fail(err)
		}
	default:
		return fail(err)
	}

	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := s.git.RemoveWorktree(cleanupCtx, ws.MirrorDir(), runDir); err != nil {
			s.logger.Warn("run worktree removal failed", "project_id", project.ID, "run_id", runID)
		}
		_ = os.RemoveAll(runDir)
		release()
	}
	return runDir, cleanup, nil
}

// ensureReviewRoot creates the project workspace root and rejects a symlinked
// root or parent before a git or worktree write can follow it. Deeper
// configured ancestors are trusted.
func ensureReviewRoot(root string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("app: workspace root is not absolute")
	}
	info, err := os.Lstat(root)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("app: workspace root is unsafe")
		}
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("app: inspect workspace root: %w", err)
	}
	parent := filepath.Dir(root)
	parentInfo, err := os.Lstat(parent)
	switch {
	case err == nil:
		if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
			return fmt.Errorf("app: workspace parent is unsafe")
		}
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return fmt.Errorf("app: create workspace parent: %w", err)
		}
		parentInfo, err = os.Lstat(parent)
		if err != nil || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
			return fmt.Errorf("app: workspace parent is unsafe")
		}
	default:
		return fmt.Errorf("app: inspect workspace parent: %w", err)
	}
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("app: create workspace root: %w", err)
	}
	info, err = os.Lstat(root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("app: workspace root is unsafe")
	}
	return nil
}

// publishEmpty publishes the "nothing to review" summary without a workflow.
func (s *ReviewService) publishEmpty(ctx context.Context, project *store.Project, token string, control *store.ReviewControl, inputs *reviewInputs, cfg effectiveReviewConfig, summary review.DiffSummary, ignored []string) error {
	return s.publishEmptyWithNote(ctx, project, token, control, 0, inputs, cfg, summary, ignored)
}

func (s *ReviewService) publishEmptyWithNote(ctx context.Context, project *store.Project, token string, control *store.ReviewControl, noteID int64, inputs *reviewInputs, cfg effectiveReviewConfig, summary review.DiffSummary, ignored []string) error {
	if inputs == nil || inputs.MR == nil || inputs.Version == nil {
		return PermanentJob(errors.New("app: publish empty summary: incomplete review inputs"))
	}
	if s.publisher == nil {
		return errors.New("app: review publisher is not configured")
	}
	recheck := func() error {
		_, err := s.currentControl(ctx, project.ID, control.MRIID, control.Generation, inputs.Version.HeadSHA)
		return err
	}
	if err := recheck(); err != nil {
		return err
	}
	s.setReviewStatus(ctx, project.ID, control.MRIID, control.Generation, 0,
		inputs.Version.HeadSHA, "", reviewStatusPublishing, "")
	profileName, _ := s.profileFor(project, "", cfg.Profile, inputs.MR.Labels, false)
	fingerprint := sha256Hex(strings.Join([]string{
		inputs.Version.HeadSHA, reviewConfigHash(cfg), profileName,
	}, "\n"))
	summaryInput := review.SummaryInput{
		Verdict:     review.VerdictPass,
		Summary:     "无需评审：所有变更文件均被忽略规则过滤。",
		Fingerprint: fingerprint,
		HeadSHA:     inputs.Version.HeadSHA,
		Profile:     profileName,
		Warnings:    append(cfg.Warnings, "no reviewable files after ignore rules"),
	}
	if summary.Truncated || len(summary.Omitted) > 0 {
		total := summary.Total
		if total == 0 {
			total = summary.Files + len(summary.Omitted) + len(inputs.Diffs)
		}
		reason := fmt.Sprintf("omitted %d of %d changed file(s) (included=%d, ignored=%d)",
			len(summary.Omitted), total, summary.Files, len(ignored))
		if len(summary.Omitted) > 0 {
			reason += " before review:" + omittedPathList(summary.Omitted, 4)
			reasons := make(map[string]int, len(summary.Omitted))
			for _, omitted := range summary.Omitted {
				reasons[omitted.Reason]++
			}
			parts := make([]string, 0, len(reasons))
			for name, count := range reasons {
				parts = append(parts, fmt.Sprintf("%s=%d", name, count))
			}
			sort.Strings(parts)
			reason += "; reasons: " + strings.Join(parts, ", ")
		} else {
			reason += "; omitted paths were not recorded in this older snapshot"
		}
		summaryInput.Verdict = review.VerdictIncomplete
		summaryInput.Summary = "没有可评审的文件：材料快照遗漏了变更文件。"
		summaryInput.IncompleteReason = reason
		summaryInput.Warnings = append(cfg.Warnings,
			fmt.Sprintf("no reviewable files: %d changed file(s) omitted before review", len(summary.Omitted)))
	}
	body, err := review.RenderSummary(summaryInput)
	if err != nil {
		return PermanentJob(fmt.Errorf("render empty summary: %w", err))
	}
	// A push can land while the summary is rendered; never publish stale text.
	if err := recheck(); err != nil {
		return err
	}
	// A push can still land between this recheck and the summary upsert below.
	// That window is accepted: the summary marker keys the publication, so a
	// late write is replaced by the next review instead of creating a second
	// summary.
	if err := s.publisher.PublishStandaloneSummary(ctx, project.ID, control.MRIID, token, fingerprint, body); err != nil {
		state := reviewStatusFailed
		reaction := reviewAwardFail
		detail := "publication failed"
		if errors.Is(err, ErrStalePublication) {
			state = reviewStatusStale
			reaction = reviewAwardNone
			detail = "review is no longer current"
		}
		s.settleReviewStatus(ctx, project.ID, control.MRIID, control.Generation, 0,
			inputs.Version.HeadSHA, "", state, detail, noteID, reaction)
		return err
	}
	state, reaction := reviewStatusForVerdict(summaryInput.Verdict)
	s.settleReviewStatus(ctx, project.ID, control.MRIID, control.Generation, 0,
		inputs.Version.HeadSHA, "", state, "", noteID, reaction)
	return nil
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// removeRunDir clears an interrupted worktree before rebuilding it.
func removeRunDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("app: remove interrupted run worktree: %w", err)
	}
	return nil
}

func truncateRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max])
}

// ignoreMatcher compiles GitLab-style glob ignore patterns.
type ignoreMatcher struct {
	patterns []*regexp.Regexp
	basename []*regexp.Regexp
}

func newIgnoreMatcher(patterns []string) *ignoreMatcher {
	out := &ignoreMatcher{}
	for _, raw := range patterns {
		pattern := strings.TrimSpace(raw)
		if pattern == "" || strings.HasPrefix(pattern, "!") {
			continue
		}
		if strings.HasSuffix(pattern, "/") {
			pattern += "**"
		}
		re, err := compileGlob(pattern)
		if err != nil {
			continue
		}
		out.patterns = append(out.patterns, re)
		if !strings.Contains(pattern, "/") {
			if re, err := compileGlob("**/" + pattern); err == nil {
				out.basename = append(out.basename, re)
			}
		}
	}
	return out
}

func (m *ignoreMatcher) Match(path string) bool {
	if m == nil {
		return false
	}
	for _, re := range m.patterns {
		if re.MatchString(path) {
			return true
		}
	}
	for _, re := range m.basename {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

func compileGlob(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// buildReviewFiles filters diffs, applies ignore rules and bounds, and counts
// lines. Invalid repository paths are skipped rather than failing the review.
func buildReviewFiles(diffs []gitlab.FileDiff, ignores []string, maxFiles, maxDiffLines int) ([]review.FileChange, []string, review.DiffSummary, []string) {
	matcher := newIgnoreMatcher(ignores)
	files := make([]review.FileChange, 0, len(diffs))
	ignored := make([]string, 0)
	warnings := make([]string, 0)
	summary := review.DiffSummary{Total: len(diffs)}
	lines := 0
	omittedByFiles := 0
	omittedByLines := 0
	omit := func(filePath, reason string) {
		summary.Truncated = true
		summary.Omitted = append(summary.Omitted, review.OmittedFile{Path: filePath, Reason: reason})
	}
	for _, diff := range diffs {
		filePath := strings.TrimSpace(diff.NewPath)
		if filePath == "" {
			filePath = strings.TrimSpace(diff.OldPath)
		}
		if !validRepoPath(filePath) {
			label := invalidPathLabel(filePath)
			omit(label, "invalid_path")
			warnings = append(warnings, fmt.Sprintf(
				"review coverage gap: skipped a changed file with an invalid path (%s)", label))
			continue
		}
		if matcher.Match(filePath) {
			ignored = append(ignored, filePath)
			continue
		}
		if maxFiles > 0 && len(files) >= maxFiles {
			omittedByFiles++
			omit(filePath, "max_files")
			continue
		}
		if diff.TooLarge || diff.Collapsed {
			// GitLab withheld the body. Record the path and reason so the
			// coverage gap is visible; never present it as reviewed content.
			cause := "too_large"
			if diff.Collapsed {
				cause = "collapsed"
			}
			omit(filePath, cause)
			warnings = append(warnings, fmt.Sprintf(
				"review coverage gap: GitLab did not provide a diff for %s (%s)", filePath, cause))
			continue
		}
		additions, deletions := countDiffLines(diff.Diff)
		if maxDiffLines > 0 && lines > 0 && lines+additions+deletions > maxDiffLines {
			omittedByLines++
			omit(filePath, "max_diff_lines")
			continue
		}
		status := "modified"
		switch {
		case diff.NewFile:
			status = "added"
		case diff.DeletedFile:
			status = "deleted"
		case diff.RenamedFile:
			status = "renamed"
		}
		oldPath := diff.OldPath
		if !validRepoPath(oldPath) {
			oldPath = ""
		}
		files = append(files, review.FileChange{
			Path:      filePath,
			OldPath:   oldPath,
			Status:    status,
			Diff:      diff.Diff,
			Additions: additions,
			Deletions: deletions,
			Generated: diff.GeneratedFile,
		})
		lines += additions + deletions
		summary.Files++
		summary.Additions += additions
		summary.Deletions += deletions
	}
	if omittedByFiles > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"review truncated: %d changed file(s) omitted by max_files", omittedByFiles))
	}
	if omittedByLines > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"review truncated: %d changed file(s) omitted by max_diff_lines", omittedByLines))
	}
	sort.Strings(ignored)
	return files, ignored, summary, warnings
}

// invalidPathLabel turns an unsafe upstream path into a bounded repository-
// relative label. The label is diagnostic only and must never be sent to
// repository tools as a real path.
func invalidPathLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "<invalid-path/empty>"
	}
	escaped := url.PathEscape(truncateRunes(value, 200))
	escaped = strings.ReplaceAll(escaped, "..", "%2E%2E")
	switch escaped {
	case "", ".", "..":
		escaped = "%2E"
	}
	escaped = truncateRunes(escaped, review.MaxPathBytes-len("<invalid-path/>"))
	return "<invalid-path/" + escaped + ">"
}

// omittedPathList renders a bounded, safe list of omitted paths for summaries.
func omittedPathList(omitted []review.OmittedFile, max int) string {
	if len(omitted) == 0 {
		return ""
	}
	if max <= 0 || max > len(omitted) {
		max = len(omitted)
	}
	shown := make([]string, 0, max)
	for _, file := range omitted[:max] {
		shown = append(shown, truncateRunes(file.Path, 120))
	}
	out := " [" + strings.Join(shown, ", ")
	if len(omitted) > max {
		out += fmt.Sprintf(", ... (+%d more)", len(omitted)-max)
	}
	return out + "]"
}

func validRepoPath(value string) bool {
	if value == "" || len(value) > review.MaxPathBytes {
		return false
	}
	if strings.ContainsAny(value, "\\\x00\r\n") || strings.HasPrefix(value, "/") {
		return false
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func countDiffLines(diff string) (int, int) {
	additions, deletions := 0, 0
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			additions++
		case strings.HasPrefix(line, "-"):
			deletions++
		}
	}
	return additions, deletions
}

// Recover rebuilds executors for unfinished review workflows and re-runs them,
// then publishes completed workflows whose publication is still pending.
func (s *ReviewService) Recover(ctx context.Context) error {
	var errs []error
	// Running nodes left by a crash must restart before Engine.Run sees them.
	if err := s.engine.Recover(ctx); err != nil {
		errs = append(errs, err)
	}
statuses:
	for _, status := range []string{"running", "pending"} {
		cursor := ""
		for {
			if err := ctx.Err(); err != nil {
				errs = append(errs, err)
				break statuses
			}
			workflows, next, err := s.store.ListWorkflows(ctx, store.WorkflowFilter{Kind: "review", Status: status}, cursor, 50)
			if err != nil {
				errs = append(errs, err)
				break
			}
			if len(workflows) == 0 {
				break
			}
			for _, workflow := range workflows {
				if err := ctx.Err(); err != nil {
					errs = append(errs, err)
					break statuses
				}
				if err := s.recoverWorkflow(ctx, workflow); err != nil {
					errs = append(errs, fmt.Errorf("app: recover workflow %s: %w", workflow.ID, err))
				}
			}
			if next == "" || next == cursor {
				break
			}
			cursor = next
		}
	}
	if ctx.Err() == nil {
		if err := s.PublishPending(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if ctx.Err() == nil {
		if err := s.ReconcileSideEffects(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *ReviewService) recoverWorkflow(ctx context.Context, workflow store.Workflow) error {
	if workflow.MRIID == nil || s.publisher == nil {
		return nil
	}
	record, err := s.store.GetWorkflowDossier(ctx, workflow.ID)
	if err != nil {
		return err
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		return fmt.Errorf("decode dossier: %w", err)
	}
	if err := review.ValidateDossier(&dossier); err != nil {
		return err
	}
	ownershipKey := strings.TrimSpace(record.JobID)
	if ownershipKey == "" {
		ownershipKey = workflow.ID
	}
	if !s.executors.Acquire(ownershipKey) {
		s.logger.Info("recovery skipped workflow owned by another handler", "workflow_id", workflow.ID)
		return nil
	}
	defer s.executors.Release(ownershipKey)
	project, token, err := s.projectToken(ctx, workflow.ProjectID)
	if err != nil {
		return err
	}
	if _, err := s.currentControl(ctx, project.ID, *workflow.MRIID, record.Generation, dossier.DiffVersion.HeadSHA); err != nil {
		if IsPermanentJob(err) {
			if cancelErr := s.engine.Cancel(context.WithoutCancel(ctx), workflow.ID, "superseded"); cancelErr != nil &&
				!errors.Is(cancelErr, dag.ErrNotFound) {
				return cancelErr
			}
			return s.skipPublication(ctx, workflow.ID, reviewSupersededReason)
		}
		return err
	}
	if storedHash := strings.TrimSpace(dossier.Metadata["config_hash"]); storedHash != "" {
		docs, err := s.repoDocs(ctx, token, project.ID, dossier.MR.TargetBranch)
		if err != nil {
			return err
		}
		if reviewConfigHash(s.resolveConfig(project, docs)) != storedHash {
			// The same hash comparison gates Publish; a resumed workflow built
			// for an older configuration must not run or publish.
			if err := s.cancelWorkflow(ctx, workflow.ID); err != nil {
				return err
			}
			s.logger.Info("recovery skipped workflow with changed configuration",
				"workflow_id", workflow.ID, "config_hash", storedHash)
			return nil
		}
	}
	headDir, cleanup, err := s.recoverRun(ctx, project, token, *workflow.MRIID, record.JobID, workflow.ID, dossier)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	profileName, profileCfg := s.profileFor(project, workflow.Profile, "", nil, true)
	profile := review.Profile{Name: profileName, Config: profileCfg}
	diffs := dossierDiffs(&dossier)
	index := gitlab.BuildDiffIndex(dossier.DiffVersion, diffs)
	resolved := dossier
	resolved.HeadDir = headDir
	resolved.BaseDir = s.workspace.Get(project.ID).BaseDir()
	executor, err := review.NewExecutor(review.ExecutorDeps{
		Dossier:  &resolved,
		Profile:  profile,
		Graph:    review.GraphConfig{AllowedProviders: s.allowedProviders()},
		Blocks:   s.blocks,
		Clients:  s.clients,
		Prompts:  s.prompts,
		AgentRun: s.agentRun,
		Snapshot: engineSnapshot(s.engine),
		Tools:    s.tools,
		Skills:   s.skills,
		Locate:   locateFunc(index),
		Cost:     s.cost,
	})
	if err != nil {
		return err
	}
	s.executors.Register(workflow.ID, executor)
	defer s.executors.Unregister(workflow.ID)
	noteID := dossierNoteID(dossier)
	s.setReviewStatus(ctx, project.ID, *workflow.MRIID, record.Generation, 0,
		dossier.DiffVersion.HeadSHA, profileName, reviewStatusRunning, "")
	superseded, err := s.runWithControl(ctx, project.ID, *workflow.MRIID, workflow.ID, record.Generation, dossier.DiffVersion.HeadSHA)
	if err != nil {
		return err
	}
	if superseded {
		s.settleRecoveredSuperseded(ctx, project.ID, *workflow.MRIID, record.Generation,
			dossier.DiffVersion.HeadSHA, profileName, noteID, workflow.ID)
		return s.skipPublication(ctx, workflow.ID, reviewSupersededReason)
	}
	snapshot, err := s.engine.Snapshot(ctx, workflow.ID)
	if err != nil {
		return err
	}
	cfg := s.resolveConfig(project, dossier.RepoDocs)
	switch snapshot.Status {
	case dag.WorkflowCompleted:
		s.setReviewStatus(ctx, project.ID, *workflow.MRIID, record.Generation, 0,
			dossier.DiffVersion.HeadSHA, profileName, reviewStatusPublishing, "")
		if err := s.publish(ctx, workflow.ID); err != nil {
			state := reviewStatusFailed
			reaction := reviewAwardFail
			detail := "publication failed"
			if errors.Is(err, ErrStalePublication) {
				state = reviewStatusStale
				reaction = reviewAwardNone
				detail = "review is no longer current"
			}
			s.settleReviewStatus(ctx, project.ID, *workflow.MRIID, record.Generation, 0,
				dossier.DiffVersion.HeadSHA, profileName, state, detail, noteID, reaction)
			return err
		}
		state, reaction := reviewStatusForVerdict(s.workflowVerdict(ctx, workflow.ID))
		s.settleReviewStatus(ctx, project.ID, *workflow.MRIID, record.Generation, 0,
			dossier.DiffVersion.HeadSHA, profileName, state, "", noteID, reaction)
		return nil
	case dag.WorkflowFailed:
		s.setReviewStatus(ctx, project.ID, *workflow.MRIID, record.Generation, 0,
			dossier.DiffVersion.HeadSHA, profileName, reviewStatusPublishing, "")
		var publishErr error
		if cfg.Settings.PostFailureNote {
			publishErr = s.publishFailure(ctx, workflow.ID, snapshot)
		} else {
			publishErr = s.skipPublication(ctx, workflow.ID, failureNoteDisabledReason)
		}
		s.settleReviewStatus(ctx, project.ID, *workflow.MRIID, record.Generation, 0,
			dossier.DiffVersion.HeadSHA, profileName, reviewStatusFailed, "", noteID, reviewAwardFail)
		return publishErr
	case dag.WorkflowCancelled:
		publishErr := s.skipPublication(ctx, workflow.ID, reviewCancelledReason)
		s.settleReviewStatus(ctx, project.ID, *workflow.MRIID, record.Generation, 0,
			dossier.DiffVersion.HeadSHA, profileName, reviewStatusCancelled, "", noteID, reviewAwardNone)
		return publishErr
	}
	return nil
}

func (s *ReviewService) settleRecoveredSuperseded(ctx context.Context, projectID, mrIID, generation int64, head, profile string, noteID int64, workflowID string) {
	control, err := s.store.GetReviewControl(ctx, projectID, mrIID)
	if err != nil || control.Generation != generation {
		return
	}
	state := reviewStatusStale
	detail := "merge request changed before publication"
	if control.Cancelled {
		state = reviewStatusCancelled
		detail = "review cancelled"
	}
	if noteID <= 0 {
		noteID = s.workflowNoteID(ctx, workflowID)
	}
	s.settleReviewStatus(ctx, projectID, mrIID, generation, 0, head, profile, state, detail, noteID, reviewAwardNone)
}

func (s *ReviewService) recoverRun(ctx context.Context, project *store.Project, token string, mrIID int64, jobID, workflowID string, dossier review.Dossier) (string, func(), error) {
	ws := s.workspace.Get(project.ID)
	if dossier.HeadDir != "" && ws != nil && runDirOwned(ws, dossier.HeadDir) {
		info, statErr := os.Lstat(dossier.HeadDir)
		switch {
		case statErr == nil && info.Mode()&os.ModeSymlink != 0:
			return "", nil, fmt.Errorf("app: review worktree path is a symlink")
		case statErr == nil && !info.IsDir():
			return "", nil, fmt.Errorf("app: review worktree path is not a directory")
		case statErr == nil:
			head, resolveErr := s.git.Resolve(ctx, dossier.HeadDir, "HEAD")
			if resolveErr == nil && strings.EqualFold(head, dossier.DiffVersion.HeadSHA) {
				unlock, err := ws.RLock(ctx)
				if err != nil {
					return "", nil, err
				}
				return dossier.HeadDir, unlock, nil
			}
		case !errors.Is(statErr, os.ErrNotExist):
			return "", nil, statErr
		}
		// A stale, unreadable, or missing worktree is rebuilt from the mirror.
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		if err := s.git.RemoveWorktree(ctx, ws.MirrorDir(), dossier.HeadDir); err != nil {
			return "", nil, fmt.Errorf("app: remove stale review worktree: %w", err)
		}
		if err := removeRunDir(dossier.HeadDir); err != nil {
			return "", nil, err
		}
	}
	runID := jobID
	if runID == "" {
		runID = workflowID
	}
	return s.prepareRun(ctx, project, token, mrIID, runID, dossier.DiffVersion.HeadSHA)
}

// runDirOwned reports whether dir is a direct child of the workspace runs
// directory, so recovery never deletes a path outside its workspace.
func runDirOwned(ws *workspace.Workspace, dir string) bool {
	if ws == nil || ws.Root == "" || dir == "" || !filepath.IsAbs(dir) {
		return false
	}
	runsDir := filepath.Join(ws.Root, "runs")
	if info, err := os.Lstat(runsDir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	rel, err := filepath.Rel(runsDir, filepath.Clean(dir))
	if err != nil || rel == "." || rel == ".." || strings.ContainsRune(rel, filepath.Separator) {
		return false
	}
	return true
}

// failureNoteEnabled reads the persisted configuration snapshot for a failed
// workflow. It avoids publishing a failure note that the project disabled when
// the process died between the workflow failure and its publication state.
func (s *ReviewService) failureNoteEnabled(ctx context.Context, workflowID string) (bool, error) {
	workflow, err := s.store.GetWorkflow(ctx, workflowID)
	if err != nil {
		return false, err
	}
	record, err := s.store.GetWorkflowDossier(ctx, workflowID)
	if err != nil {
		return false, err
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		return false, fmt.Errorf("decode dossier: %w", err)
	}
	projectRow, err := s.store.GetProject(ctx, workflow.ProjectID)
	if err != nil {
		return false, err
	}
	if projectRow == nil {
		return false, fmt.Errorf("project %d is missing", workflow.ProjectID)
	}
	return s.resolveConfig(projectRow, dossier.RepoDocs).Settings.PostFailureNote, nil
}

// PublishPending retries terminal workflows whose publication state is still
// pending. It is safe to call at startup and after a crash.
func (s *ReviewService) PublishPending(ctx context.Context) error {
	if s.publisher == nil {
		return nil
	}
	var errs []error
	for round := 0; round < 3; round++ {
		pending, err := s.store.ListPendingPublications(ctx, 50)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return errors.Join(errs...)
		}
		progress := false
		for _, item := range pending {
			var err error
			switch item.Status {
			case string(dag.WorkflowFailed):
				wantsNote, configErr := s.failureNoteEnabled(ctx, item.WorkflowID)
				if configErr != nil {
					errs = append(errs, configErr)
					continue
				}
				if !wantsNote {
					if skipErr := s.skipPublication(ctx, item.WorkflowID, failureNoteDisabledReason); skipErr != nil {
						errs = append(errs, skipErr)
						continue
					}
					progress = true
					continue
				}
				err = s.publisher.PublishFailure(ctx, item.WorkflowID, reviewExecutionFailedReason)
			case string(dag.WorkflowCompleted):
				err = s.publisher.Publish(ctx, item.WorkflowID)
			default:
				continue
			}
			if err != nil {
				if IsPermanentJob(err) {
					progress = true
					continue
				}
				errs = append(errs, err)
				continue
			}
			progress = true
		}
		if !progress {
			break
		}
	}
	return errors.Join(errs...)
}

func dossierDiffs(d *review.Dossier) []gitlab.FileDiff {
	out := make([]gitlab.FileDiff, 0, len(d.Files))
	for _, file := range d.Files {
		diff := gitlab.FileDiff{
			OldPath: file.OldPath,
			NewPath: file.Path,
			Diff:    file.Diff,
		}
		switch file.Status {
		case "added", "new":
			diff.NewFile = true
		case "deleted":
			diff.DeletedFile = true
		case "renamed":
			diff.RenamedFile = true
		}
		out = append(out, diff)
	}
	return out
}
