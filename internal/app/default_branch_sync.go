package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
)

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
		"trigger":    "default_branch_changed",
	})
	if err != nil {
		loggerOrDiscard(s.logger).Warn("default branch refresh enqueue failed", "project_id", project.ID, "error_class", errclass.Of(err))
	}
}
