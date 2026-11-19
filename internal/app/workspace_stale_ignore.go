package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// staleIgnoreBlockedTrigger marks the workspace refresh queued at startup for a
// workspace whose stored state still says "blocked by ignore policy" although
// the current ignore policy no longer restricts indexing (#618). Like a manual
// refresh it means "the default branch".
const staleIgnoreBlockedTrigger = "stale_ignore_block"

// RefreshStaleIgnoreBlocked is the one-shot startup self-heal for workspaces
// that #457 left stale. The ignore verdict is only evaluated when a workspace
// is onboarded or refreshed and then stored as last_error. Before #457 any
// non-empty ignore list counted as restricted, and the built-in defaults are
// always present, so every workspace written then reads "blocked by ignore
// policy" with state=ready and no index_sha. Nothing evaluated the text again:
// after the upgrade the project kept withholding its shared index until a push
// reached the default branch.
//
// For every enabled project whose workspace is ready with exactly that text and
// whose current policy is no longer restricted, it queues one workspace.refresh
// that reindexes the default branch without regenerating the project context,
// and returns how many it queued. Nothing is queued without MCP. The refresh
// clears the text, so a later startup finds nothing left to do.
//
// The policy is evaluated from the platform review_defaults and the project
// settings only, which needs no GitLab call. A repository .gitlab-review.yml
// that still restricts is caught by the refresh itself, which evaluates the
// full rule and writes the same text back instead of indexing.
//
// The enqueue holds the project lifecycle read lock, like the other producers
// outside a review, and re-reads the project inside it so a concurrent delete
// or disable wins. It deliberately avoids EnqueueRefresh: that takes the
// workspace content read lock, which waits behind a queued refresh writer (see
// syncProjectDefaultBranch). The queue's Enqueue never replaces a refresh that
// is already queued for the project, so an earlier job keeps its payload and no
// second one is added; that job reindexes the stale workspace on its own.
//
// A failure on one project is logged by error class and skipped. Only a
// failure to list the projects, or the end of ctx, is returned.
func (s *WorkspaceService) RefreshStaleIgnoreBlocked(ctx context.Context) (int, error) {
	if s.mcpFactory == nil {
		return 0, nil
	}
	if s.queue == nil {
		return 0, errors.New("app: job queue is not configured")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	queued := 0
	for i := range projects {
		if err := ctx.Err(); err != nil {
			return queued, err
		}
		project := &projects[i]
		// Cheap filter on the listed row: projects that need nothing never
		// touch a lock. The enqueue below checks again under the lock.
		added := false
		stale, err := s.staleIgnoreBlocked(ctx, project)
		if err == nil && stale {
			added, err = s.enqueueStaleIgnoreBlockedRefresh(ctx, project.ID)
		}
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return queued, ctx.Err()
			}
			s.logger.Warn("stale ignore block refresh enqueue failed", "project_id", project.ID, "error_class", errclass.Of(err))
		case added:
			queued++
			s.logger.Info("stale ignore block refresh enqueued", "project_id", project.ID)
		}
	}
	return queued, nil
}

// staleIgnoreBlocked reports whether the project's workspace carries the
// ignore-policy text from a rule that no longer applies: the project is
// enabled, its workspace is ready with exactly that last_error, and the policy
// from the platform defaults and the project settings does not restrict.
func (s *WorkspaceService) staleIgnoreBlocked(ctx context.Context, project *store.Project) (bool, error) {
	if project == nil || !project.Enabled {
		return false, nil
	}
	state, err := s.store.GetWorkspace(ctx, project.ID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if state == nil || state.State != "ready" || state.LastError != store.WorkspaceIndexBlockedByIgnore {
		return false, nil
	}
	cfg, err := resolveSecureReviewConfig(project, review.RepoDocs{}, s.reviewDefaults, s.profiles)
	if err != nil {
		// Settings that cannot be read safely make the refresh fail as well;
		// leave the workspace as it is rather than turn it into an error.
		return false, nil
	}
	return !config.ReviewIgnoreRestricted(cfg.Settings.Ignore), nil
}

// enqueueStaleIgnoreBlockedRefresh queues the reindexing refresh for one
// project and reports whether it added a job; false with no error means the
// project no longer qualifies or a refresh was already queued.
func (s *WorkspaceService) enqueueStaleIgnoreBlockedRefresh(ctx context.Context, projectID int64) (bool, error) {
	ws := s.workspace.Get(projectID)
	if ws == nil {
		return false, nil
	}
	unlock, err := ws.LifecycleRLock(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()
	project, err := s.store.GetProject(ctx, projectID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if stale, err := s.staleIgnoreBlocked(ctx, project); err != nil || !stale {
		return false, err
	}
	_, duplicate, err := enqueueJobOnce(ctx, s.queue, "workspace.refresh", fmt.Sprintf("ws:%d", project.ID), map[string]any{
		"project_id":         project.ID,
		"ref":                "",
		"branch":             "",
		"after_sha":          "",
		"trigger":            staleIgnoreBlockedTrigger,
		"regenerate_context": false,
		"reindex":            true,
	})
	if err != nil {
		return false, err
	}
	return !duplicate, nil
}
