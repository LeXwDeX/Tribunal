package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		remaining := s.settings.MaxTotal - result.MRsChecked
		if remaining <= 0 {
			s.logger.Warn("catch-up scan reached the overall MR limit", "max_total", s.settings.MaxTotal)
			break
		}
		limit := min(s.settings.MaxPerProject, remaining)
		checked, enqueued, ok := s.scanProject(ctx, project.ID, limit)
		if !ok {
			continue
		}
		result.Projects++
		result.MRsChecked += checked
		result.Enqueued += enqueued
	}
	s.logger.Info("catch-up scan finished",
		"projects", result.Projects, "mrs_checked", result.MRsChecked, "enqueued", result.Enqueued)
	return result, nil
}

// scanProject reports ok=false when the project was skipped entirely.
func (s *CatchUpService) scanProject(ctx context.Context, projectID int64, limit int) (checked, enqueued int, ok bool) {
	ws := s.workspace.Get(projectID)
	if ws == nil {
		return 0, 0, false
	}
	// Hold the lifecycle read lock like a webhook delivery does, so project
	// deletion cannot race a catch-up enqueue.
	unlock, err := ws.LifecycleRLock(ctx)
	if err != nil {
		return 0, 0, false
	}
	defer unlock()

	project, token, usable := s.projectToken(ctx, projectID)
	if !usable {
		return 0, 0, false
	}
	settings, err := ResolveSettings(project.Settings, s.webhooks.reviewDefaults)
	if err != nil {
		s.logger.Warn("catch-up skipped project with invalid settings", "project_id", projectID)
		return 0, 0, false
	}
	var since time.Time
	if s.settings.MaxAge > 0 {
		since = s.now().Add(-s.settings.MaxAge)
	}
	mrs, err := s.gitlab.ListOpenMergeRequests(ctx, token, projectID, limit, since)
	if err != nil {
		s.logger.Warn("catch-up could not list open merge requests",
			"project_id", projectID, "error_class", errclass.Of(err))
		return 0, 0, false
	}
	for _, mr := range mrs {
		if ctx.Err() != nil {
			break
		}
		checked++
		queued, err := s.catchUpMR(ctx, project, settings, mr)
		if err != nil {
			s.logger.Warn("catch-up enqueue failed",
				"project_id", projectID, "mr_iid", mr.IID, "error_class", errclass.Of(err))
			continue
		}
		if queued {
			enqueued++
		}
	}
	return checked, enqueued, true
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
// is queued, was filtered after enqueue, or was cancelled on purpose.
func (s *CatchUpService) catchUpMR(ctx context.Context, project *store.Project, settings config.ProjectSettings, mr gitlab.MergeRequest) (bool, error) {
	head := strings.ToLower(strings.TrimSpace(mr.SHA))
	if mr.IID <= 0 || !shaRE.MatchString(head) {
		return false, nil
	}
	control, err := s.store.GetReviewControl(ctx, project.ID, mr.IID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	if err == nil && control != nil && strings.EqualFold(control.HeadSHA, head) {
		return false, nil
	}
	queued, err := s.webhooks.enqueueCatchUp(ctx, project, settings, mr, head)
	if queued {
		s.logger.Info("catch-up enqueued review",
			"project_id", project.ID, "mr_iid", mr.IID, "head", head[:8])
	}
	return queued, err
}

// enqueueCatchUp runs the webhook filter chain on a synthetic merge request
// event and records the resulting job with trigger catch_up.
func (s *WebhookService) enqueueCatchUp(ctx context.Context, project *store.Project, settings config.ProjectSettings, mr gitlab.MergeRequest, head string) (bool, error) {
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
	job, reason := s.newReviewJob(project, settings, payload, mr.IID, catchUpTrigger, true)
	if reason != "" {
		s.logger.Info("catch-up skipped merge request",
			"project_id", project.ID, "mr_iid", mr.IID, "reason", reason)
		return false, nil
	}
	eventUUID := uuid.NewSHA1(catchUpNamespace, []byte(itoa(project.ID)+":"+itoa(mr.IID)+":"+head)).String()
	result, err := s.recordJob(ctx, project.ID, catchUpEvent, eventUUID, job)
	if err != nil {
		return false, err
	}
	return result.Accepted, nil
}
