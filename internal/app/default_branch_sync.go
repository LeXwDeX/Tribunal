package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
)

const (
	// defaultBranchChangedTrigger marks a workspace refresh queued because the
	// default branch moved. It means "the default branch": the refresh does not
	// use a branch captured when it was queued.
	defaultBranchChangedTrigger = "default_branch_changed"
	// defaultBranchRefreshEvent names the delivery record of a refresh that a
	// webhook payload requested.
	defaultBranchRefreshEvent = "Default Branch Refresh"
	// defaultBranchRefreshBucket bounds those refreshes to one delivery record
	// per project and reported branch in each bucket. Every event of a project
	// carries the changed default until a refresh has synchronized it, so a
	// busy branch would otherwise queue one refresh per event; a refresh that
	// could not synchronize (GitLab unreachable) is retried by a later bucket.
	defaultBranchRefreshBucket = time.Minute
)

// defaultBranchRefreshNamespace derives the deterministic delivery UUID of a
// webhook-requested default branch refresh.
var defaultBranchRefreshNamespace = uuid.MustParse("d9d73817-5457-4c19-902c-a32157980b67")

// defaultBranchStore is the persistence surface of syncDefaultBranch.
type defaultBranchStore interface {
	GetProject(ctx context.Context, id int64) (*store.Project, error)
	UpdateProjectDefaultBranch(ctx context.Context, id int64, previous, next string) (bool, error)
}

// validDefaultBranch reports whether name may replace a stored default branch:
// a short branch name that the Git layer accepts as a ref, so a value that
// passes here can always be fetched later. A name that is blank, padded,
// option-like, a full "refs/..." ref, or otherwise malformed never passes.
func validDefaultBranch(name string) bool {
	if name == "" || strings.TrimSpace(name) != name || strings.HasPrefix(name, "refs/") {
		return false
	}
	_, err := repo.NormalizeRef(name)
	return err == nil
}

func loggerOrDiscard(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// syncDefaultBranch follows the default branch GitLab reports for project. The
// default branch is written once, at registration, so without this a project
// whose default branch changed keeps refreshing and indexing the old branch and
// filtering the new branch's webhooks out (#604). Callers have already looked
// the project up in GitLab to verify its identity and pass that record as
// current.
//
// It reports whether this call changed the stored value; on a change project is
// updated in place so the caller continues with the new default. A lost
// compare-and-set means another writer got there first: the stored value is
// adopted and false is returned, so one change is acted on once. An empty or
// unusable GitLab value (an empty repository has no default branch), a missing
// dependency and a store failure all leave the stored value alone. The
// synchronization never fails the caller's own work.
func syncDefaultBranch(ctx context.Context, st defaultBranchStore, logger *slog.Logger, project *store.Project, current *gitlab.Project) bool {
	if st == nil || project == nil || project.ID <= 0 || current == nil {
		return false
	}
	previous, next := project.DefaultBranch, current.DefaultBranch
	if next == previous || !validDefaultBranch(next) {
		return false
	}
	logger = loggerOrDiscard(logger)
	changed, err := st.UpdateProjectDefaultBranch(ctx, project.ID, previous, next)
	if err != nil {
		logger.Warn("project default branch sync failed", "project_id", project.ID, "error_class", errclass.Of(err))
		return false
	}
	if changed {
		project.DefaultBranch = next
		logger.Info("project default branch synchronized", "project_id", project.ID)
		return true
	}
	// The row no longer holds the value this call read, so a concurrent writer
	// (another run observing the same change, or an operator) already moved it.
	latest, err := st.GetProject(ctx, project.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// The project was deleted; there is nothing to follow.
	case err != nil:
		logger.Warn("project default branch sync failed", "project_id", project.ID, "error_class", errclass.Of(err))
	case latest != nil && strings.TrimSpace(latest.DefaultBranch) != "":
		project.DefaultBranch = latest.DefaultBranch
	}
	return false
}

// syncProjectDefaultBranch follows a default branch change that GitLab
// reported to a review run, and queues the workspace refresh that moves the
// shared base onto the new branch. The run holds the workspace read lock, which
// WorkspaceService.EnqueueRefresh takes as well: a second read lock behind a
// waiting writer would deadlock, so the job is enqueued directly. A failed
// enqueue is logged and never fails the review; the next refresh or push
// re-bases the workspace.
func (s *ReviewService) syncProjectDefaultBranch(ctx context.Context, project *store.Project, current *gitlab.Project) {
	if !syncDefaultBranch(ctx, s.store, s.logger, project, current) || s.queue == nil {
		return
	}
	// The new default is stored already; a cancelled run must not lose the
	// refresh that goes with it.
	_, err := enqueueJob(context.WithoutCancel(ctx), s.queue, "workspace.refresh", fmt.Sprintf("ws:%d", project.ID), map[string]any{
		"project_id": project.ID,
		"ref":        "",
		"branch":     "",
		"after_sha":  "",
		"trigger":    defaultBranchChangedTrigger,
	})
	if err != nil {
		loggerOrDiscard(s.logger).Warn("default branch refresh enqueue failed", "project_id", project.ID, "error_class", errclass.Of(err))
	}
}

// requestDefaultBranchRefresh queues a workspace refresh when an authenticated
// webhook payload names a default branch other than the stored one (#604). The
// stored value filters every event, so after a rename in GitLab each event on
// the new default branch is dropped as branch_filtered, and the ingress never
// asks GitLab: without this the change would wait for an unrelated review run
// or a manual refresh. The payload is only a hint. It never writes the project;
// the refresh reads the project from GitLab and syncDefaultBranch stores the
// value only when GitLab reports it. The event's own decision is unchanged.
//
// The job is recorded like the catch-up refresh. It takes no workspace lock:
// the webhook path holds the project lifecycle read lock, and the workspace
// read lock that WorkspaceService.EnqueueRefresh takes could wait behind a
// queued refresh writer. A failure is logged and never fails the event; a later
// event carries the hint again.
func (s *WebhookService) requestDefaultBranchRefresh(ctx context.Context, project *store.Project, payload *gitlab.Webhook) {
	if project == nil || payload == nil || payload.Project.ID != project.ID {
		return
	}
	hint := payload.Project.DefaultBranch
	if hint == project.DefaultBranch || !validDefaultBranch(hint) {
		return
	}
	now := s.now().UTC()
	bucket := now.Truncate(defaultBranchRefreshBucket).Format(time.RFC3339)
	job := newJob("workspace.refresh", "ws:"+itoa(project.ID), map[string]any{
		"project_id": project.ID,
		"ref":        "",
		"branch":     "",
		"after_sha":  "",
		"trigger":    defaultBranchChangedTrigger,
	})
	if _, _, err := s.store.RecordDelivery(ctx, store.Delivery{
		UUID:       uuid.NewSHA1(defaultBranchRefreshNamespace, []byte(itoa(project.ID)+":"+hint+":"+bucket)).String(),
		ProjectID:  project.ID,
		Event:      defaultBranchRefreshEvent,
		Outcome:    store.DeliveryEnqueued,
		ReceivedAt: now,
	}, job); err != nil {
		s.logger.Warn("default branch refresh enqueue failed", "project_id", project.ID, "error_class", errclass.Of(err))
	}
}
