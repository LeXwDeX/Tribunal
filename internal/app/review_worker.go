package app

import (
	"context"
	"errors"

	"github.com/LeXwDeX/tribunal/internal/jobs"
)

// ReviewWorker composes the review and workspace job handlers registered with
// the persistent worker.
type ReviewWorker struct {
	reviews   *ReviewService
	workspace *WorkspaceService
}

// NewReviewWorker builds the job dispatch surface.
func NewReviewWorker(reviews *ReviewService, workspace *WorkspaceService) (*ReviewWorker, error) {
	if reviews == nil || workspace == nil {
		return nil, errors.New("app: review worker dependencies are required")
	}
	return &ReviewWorker{reviews: reviews, workspace: workspace}, nil
}

// Handlers returns every job kind owned by the application layer.
func (w *ReviewWorker) Handlers() map[string]jobs.Handler {
	return map[string]jobs.Handler{
		"review.run":        w.reviews.Handle,
		"review.cancel":     w.reviews.Handle,
		"review.candidate":  w.reviews.Handle,
		"review.publish":    w.reviews.Handle,
		"review.status":     w.reviews.Handle,
		"review.reaction":   w.reviews.Handle,
		"workspace.onboard": w.workspace.Handle,
		"workspace.refresh": w.workspace.Handle,
	}
}

// Handle dispatches one job by kind.
func (w *ReviewWorker) Handle(ctx context.Context, job *jobs.Job) error {
	if job == nil {
		return nil
	}
	switch job.Kind {
	case "review.run", "review.cancel", "review.candidate", "review.publish",
		"review.status", "review.reaction":
		return w.reviews.Handle(ctx, job)
	case "workspace.onboard", "workspace.refresh":
		return w.workspace.Handle(ctx, job)
	default:
		return nil
	}
}
