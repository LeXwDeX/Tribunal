package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

const (
	// catchUpTrigger marks reviews enqueued by the startup scan.
	catchUpTrigger = "catch_up"
	// catchUpEvent is the delivery event recorded for a catch-up enqueue.
	catchUpEvent = "Catch Up"
	// catchUpRefreshEvent records the workspace refresh requested alongside a
	// catch-up review, so a missed MR also refreshes the base checkout (#457).
	catchUpRefreshEvent = "Catch Up Refresh"

	// Fixed hook-health messages. The console shows them as the failure reason;
	// they never include upstream text or credentials.
	webhookDisabledMessage      = "GitLab disabled the managed project webhook after repeated delivery failures"
	webhookMissingMessage       = "the managed project webhook is missing or was deleted in GitLab"
	webhookTokenRejectedMessage = "project token was rejected by GitLab while checking the webhook; replace the project token"
)

// catchUpNamespace derives the deterministic delivery UUID of a catch-up
// enqueue, so one (project, MR, head) is caught up at most once even if the
// service restarts repeatedly.
var catchUpNamespace = uuid.MustParse("6f1d3a52-7c0e-4b8a-9a54-2d0c5e7b1f33")

// OpenMergeRequestLister lists the open merge requests of a project. The
// production GitLab client implements it; the scan is skipped when the
// configured client does not.
type OpenMergeRequestLister interface {
	ListOpenMergeRequests(ctx context.Context, token string, projectID int64, limit int, updatedAfter time.Time) ([]gitlab.MergeRequest, error)
}

// CatchUpStore is the persistence surface needed by the startup scan.
type CatchUpStore interface {
	ListProjects(ctx context.Context) ([]store.Project, error)
	GetReviewControl(ctx context.Context, projectID, mrIID int64) (*store.ReviewControl, error)
}

// CatchUpServiceConfig configures CatchUpService.
type CatchUpServiceConfig struct {
	Store     CatchUpStore
	GitLab    OpenMergeRequestLister
	Webhooks  *WebhookService
	Workspace *workspace.Manager
	Cipher    *secret.Cipher
	Settings  config.CatchUpConfig
	Logger    *slog.Logger
	Now       func() time.Time
}

// CatchUpService enqueues a review for open MR heads that were never reviewed,
// typically because their webhook arrived while the service was down (#362).
// It runs once after startup recovery and reuses the webhook decision path, so
// draft, label, target-branch and repository-config filters all apply.
type CatchUpService struct {
	store     CatchUpStore
	gitlab    OpenMergeRequestLister
	webhooks  *WebhookService
	workspace *workspace.Manager
	cipher    *secret.Cipher
	settings  config.CatchUpConfig
	logger    *slog.Logger
	now       func() time.Time
}

// CatchUpResult summarizes one scan.
type CatchUpResult struct {
	Projects   int
	MRsChecked int
	Enqueued   int
}

// NewCatchUpService builds the startup catch-up scan.
func NewCatchUpService(cfg CatchUpServiceConfig) (*CatchUpService, error) {
	if cfg.Store == nil || cfg.GitLab == nil || cfg.Webhooks == nil || cfg.Workspace == nil || cfg.Cipher == nil {
		return nil, errors.New("app: catch-up service dependencies are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &CatchUpService{
		store:     cfg.Store,
		gitlab:    cfg.GitLab,
		webhooks:  cfg.Webhooks,
		workspace: cfg.Workspace,
		cipher:    cfg.Cipher,
		settings:  cfg.Settings,
		logger:    logger,
		now:       now,
	}, nil
}

const (
	// catchUpListLimit bounds how many open MRs one project contributes to a
	// scan (one GitLab page). max_per_project and max_total limit enqueued
	// reviews, so already-reviewed MRs never consume the review budget.
	catchUpListLimit = 100
	// catchUpDeadline bounds the whole scan; catchUpMaxFailures stops it after
	// that many consecutive project list failures (GitLab down).
	catchUpDeadline    = 2 * time.Minute
	catchUpMaxFailures = 3
)

// Run performs one scan. It never fails the caller for a single project: a
// GitLab or storage error skips that project and is logged by class only.
func (s *CatchUpService) Run(ctx context.Context) (CatchUpResult, error) {
	var result CatchUpResult
	if !s.settings.Enabled {
		s.logger.Info("catch-up scan disabled")
		return result, nil
	}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return result, err
	}
	scanCtx, cancel := context.WithTimeout(ctx, catchUpDeadline)
	defer cancel()
	failures := 0
	for _, project := range projects {
		if scanCtx.Err() != nil {
			break
		}
		remaining := s.settings.MaxTotal - result.Enqueued
		if remaining <= 0 {
			s.logger.Info("catch-up scan reached the overall enqueue limit", "max_total", s.settings.MaxTotal)
			break
		}
		checked, enqueued, outcome := s.scanProject(scanCtx, project.ID, min(s.settings.MaxPerProject, remaining))
		switch outcome {
		case catchUpSkipped:
			continue
		case catchUpListFailed:
			if failures++; failures >= catchUpMaxFailures {
				s.logger.Warn("catch-up scan stopped after consecutive GitLab failures", "failures", failures)
				goto finished
			}
			continue
		}
		failures = 0
		result.Projects++
		result.MRsChecked += checked
		result.Enqueued += enqueued
	}
finished:
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if scanCtx.Err() != nil {
		s.logger.Warn("catch-up scan hit its deadline", "deadline", catchUpDeadline.String())
	}
	s.logger.Info("catch-up scan finished",
		"projects", result.Projects, "mrs_checked", result.MRsChecked, "enqueued", result.Enqueued)
	return result, nil
}

type catchUpOutcome int

const (
	catchUpScanned catchUpOutcome = iota
	catchUpSkipped
	catchUpListFailed
)

// scanProject lists the project's recently updated open MRs without holding
// any project lock, then decides and enqueues each MR under the lifecycle read
// lock. At most budget reviews are enqueued.
func (s *CatchUpService) scanProject(ctx context.Context, projectID int64, budget int) (checked, enqueued int, outcome catchUpOutcome) {
	project, token, usable := s.projectToken(ctx, projectID)
	if !usable {
		return 0, 0, catchUpSkipped
	}
	s.reconcileHookHealth(ctx, project, token)
	if _, err := ResolveSettings(project.Settings, s.webhooks.reviewDefaults); err != nil {
		s.logger.Warn("catch-up skipped project with invalid settings", "project_id", projectID)
		return 0, 0, catchUpSkipped
	}
	// Anything the control saw after this instant is newer than the listing.
	listStart := s.now()
	var since time.Time
	if s.settings.MaxAge > 0 {
		since = listStart.Add(-s.settings.MaxAge)
	}
	mrs, err := s.gitlab.ListOpenMergeRequests(ctx, token, projectID, catchUpListLimit, since)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("catch-up could not list open merge requests",
				"project_id", projectID, "error_class", errclass.Of(err))
		}
		return 0, 0, catchUpListFailed
	}
	refreshHead := ""
	for _, mr := range mrs {
		if ctx.Err() != nil || enqueued >= budget {
			break
		}
		checked++
		queued, err := s.catchUpMR(ctx, projectID, mr, listStart)
		if err != nil {
			s.logger.Warn("catch-up enqueue failed",
				"project_id", projectID, "mr_iid", mr.IID, "error_class", errclass.Of(err))
			continue
		}
		if queued {
			enqueued++
			if refreshHead == "" {
				refreshHead = strings.ToLower(strings.TrimSpace(mr.SHA))
			}
		}
	}
	if enqueued > 0 {
		// A missed review also means the workspace base may be stale. The review
		// path fetches the MR base itself; this keeps the shared base current.
		s.webhooks.enqueueCatchUpRefresh(ctx, project, refreshHead)
	}
	return checked, enqueued, catchUpScanned
}

// projectHookLister lists GitLab project hooks. The production client
// implements it; a test or reduced client skips the health scan.
type projectHookLister interface {
	ListProjectHooks(ctx context.Context, token string, projectID int64) ([]gitlab.Hook, error)
}

type projectWebhookWriter interface {
	UpdateProjectWebhook(ctx context.Context, id int64, update store.ProjectWebhookUpdate) error
}

// ReconcileHookHealth checks every registered project with a managed hook.
// The startup catch-up scan only sees this once; a hook disabled later keeps
// failing silently until the next process start. The periodic pass reuses the
// same reconciliation, so a recovered hook clears the failure this feature
// wrote and unrelated failures stay. One project's error never aborts the pass.
func (s *CatchUpService) ReconcileHookHealth(ctx context.Context) {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("webhook health scan failed", "error_class", errclass.Of(err))
		}
		return
	}
	for _, project := range projects {
		if ctx.Err() != nil {
			return
		}
		if !project.Enabled || project.HookID == nil {
			continue
		}
		loaded, token, usable := s.projectToken(ctx, project.ID)
		if !usable {
			continue
		}
		s.reconcileHookHealth(ctx, loaded, token)
	}
}

// reconcileHookHealth compares the recorded managed hook with GitLab and writes
// the health back to the project. GitLab disables a hook after repeated
// delivery failures, and the console previously stayed green (#457). It never
// fails a scan: an unreachable GitLab leaves the previous state.
func (s *CatchUpService) reconcileHookHealth(ctx context.Context, project *store.Project, token string) {
	if project == nil || project.HookID == nil {
		return
	}
	lister, ok := s.gitlab.(projectHookLister)
	if !ok {
		return
	}
	writer, ok := s.webhooks.store.(projectWebhookWriter)
	if !ok {
		return
	}
	hooks, err := lister.ListProjectHooks(ctx, token, project.ID)
	if err != nil {
		if gitlab.StatusOf(err) == http.StatusUnauthorized || gitlab.StatusOf(err) == http.StatusForbidden {
			s.setHookHealth(ctx, writer, project, store.WebhookStateFailed, webhookTokenRejectedMessage)
		}
		if ctx.Err() == nil {
			s.logger.Warn("webhook health check failed", "project_id", project.ID, "error_class", errclass.Of(err))
		}
		return
	}
	var managed *gitlab.Hook
	for i := range hooks {
		if hooks[i].ID == *project.HookID {
			managed = &hooks[i]
			break
		}
	}
	switch {
	case managed == nil:
		s.setHookHealth(ctx, writer, project, store.WebhookStateFailed, webhookMissingMessage)
	case managed.AlertStatus == "disabled" || managed.AlertStatus == "temporarily_disabled":
		s.setHookHealth(ctx, writer, project, store.WebhookStateFailed, webhookDisabledMessage)
	case project.WebhookState == store.WebhookStateFailed &&
		(project.WebhookError == webhookDisabledMessage || project.WebhookError == webhookMissingMessage || project.WebhookError == webhookTokenRejectedMessage):
		// A hook that recovered is healthy again; unrelated failures stay.
		s.setHookHealth(ctx, writer, project, store.WebhookStateEnabled, "")
	}
}

func (s *CatchUpService) setHookHealth(ctx context.Context, writer projectWebhookWriter, project *store.Project, state, message string) {
	if project.WebhookState == state && project.WebhookError == message {
		return
	}
	if err := writer.UpdateProjectWebhook(ctx, project.ID, store.ProjectWebhookUpdate{
		State: state, Error: message, Events: project.WebhookEvents,
	}); err != nil {
		s.logger.Warn("webhook health persistence failed", "project_id", project.ID)
	}
}

func (s *CatchUpService) projectToken(ctx context.Context, projectID int64) (*store.Project, string, bool) {
	project, err := s.webhooks.store.GetProject(ctx, projectID)
	if err != nil || project == nil || !project.Enabled {
		return nil, "", false
	}
	token, err := s.cipher.Open(project.TokenEnc, secret.TokenAAD(project.ID))
	if err != nil || len(token) == 0 {
		return nil, "", false
	}
	return project, string(token), true
}

// catchUpMR enqueues a review when the MR's current head has no review
// control. A control that already names this head means the head was reviewed,
// is queued, was filtered after enqueue, or was cancelled on purpose. A
// control touched since the listing started is newer than the listed head, and
// the enqueue itself is conditional on the control being unchanged, so a live
// webhook that lands in between is never replaced.
func (s *CatchUpService) catchUpMR(ctx context.Context, projectID int64, mr gitlab.MergeRequest, listStart time.Time) (bool, error) {
	head := strings.ToLower(strings.TrimSpace(mr.SHA))
	if mr.IID <= 0 || !shaRE.MatchString(head) {
		return false, nil
	}
	ws := s.workspace.Get(projectID)
	if ws == nil {
		return false, nil
	}
	// Like a webhook delivery, hold the lifecycle read lock only for the
	// decision and enqueue so project deletion cannot race it.
	unlock, err := ws.LifecycleRLock(ctx)
	if err != nil {
		return false, nil
	}
	defer unlock()

	project, err := s.webhooks.store.GetProject(ctx, projectID)
	if err != nil || project == nil || !project.Enabled {
		return false, nil
	}
	settings, err := ResolveSettings(project.Settings, s.webhooks.reviewDefaults)
	if err != nil {
		return false, nil
	}
	control, err := s.store.GetReviewControl(ctx, projectID, mr.IID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	expect := &store.ReviewControlExpect{}
	if err == nil && control != nil {
		if strings.EqualFold(control.HeadSHA, head) || !control.UpdatedAt.Before(listStart) {
			return false, nil
		}
		expect = &store.ReviewControlExpect{Exists: true, Generation: control.Generation, CandidateSeq: control.CandidateSeq}
	}
	queued, err := s.webhooks.enqueueCatchUp(ctx, project, settings, mr, head, expect)
	if queued {
		s.logger.Info("catch-up enqueued review",
			"project_id", project.ID, "mr_iid", mr.IID, "head", head[:8])
	}
	return queued, err
}

// enqueueCatchUp runs the webhook filter chain on a synthetic merge request
// event and records the resulting job with trigger catch_up.
func (s *WebhookService) enqueueCatchUp(ctx context.Context, project *store.Project, settings config.ProjectSettings, mr gitlab.MergeRequest, head string, expect *store.ReviewControlExpect) (bool, error) {
	if mr.TargetProjectID > 0 && mr.TargetProjectID != project.ID {
		return false, nil
	}
	state := mr.State
	if state == "" {
		state = "opened"
	}
	payload := &gitlab.Webhook{
		ObjectKind: "merge_request",
		ProjectID:  project.ID,
		Project:    gitlab.WebhookProject{ID: project.ID},
		ObjectAttributes: gitlab.WebhookObjectAttributes{
			IID:             mr.IID,
			TargetProjectID: project.ID,
			TargetBranch:    mr.TargetBranch,
			SourceBranch:    mr.SourceBranch,
			State:           state,
			SHA:             head,
			Labels:          gitlab.Labels(mr.Labels),
			Draft:           mr.Draft,
			Title:           mr.Title,
		},
	}
	// Not a full review: like mr_update, an earlier published head narrows the
	// scope to the files changed since it.
	job, reason := s.newReviewJob(project, settings, payload, mr.IID, catchUpTrigger, false)
	if reason != "" {
		s.logger.Info("catch-up skipped merge request",
			"project_id", project.ID, "mr_iid", mr.IID, "reason", reason)
		return false, nil
	}
	eventUUID := uuid.NewSHA1(catchUpNamespace, []byte(itoa(project.ID)+":"+itoa(mr.IID)+":"+head)).String()
	result, err := s.recordJob(ctx, project.ID, catchUpEvent, eventUUID, mr.IID, job, expect)
	if err != nil {
		return false, err
	}
	return result.Accepted, nil
}
