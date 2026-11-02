package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// preWorkflowFailureDescription is fixed because raw configuration errors can
// contain repository content and must never reach GitLab.
const preWorkflowFailureDescription = "AI review: configuration invalid"

// PublishPreWorkflowFailure writes a failed commit status for a review that
// cannot create a workflow. The MR lock and the generation/head recheck keep a
// stale failure from replacing a newer result. No workflow exists, so this
// path records no publication row.
func (p *Publisher) PublishPreWorkflowFailure(ctx context.Context, projectID, mrIID, generation int64, head, token string) error {
	if projectID <= 0 || mrIID <= 0 || generation <= 0 || token == "" || !shaRE.MatchString(head) {
		return PermanentJob(fmt.Errorf("pre-workflow failure target is incomplete"))
	}
	unlock, err := p.lockMR(ctx, projectID, mrIID)
	if err != nil {
		return err
	}
	defer unlock()
	recheck := func() error {
		control, err := p.store.GetReviewControl(ctx, projectID, mrIID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return PermanentJob(fmt.Errorf("%w: review control is missing", ErrStalePublication))
			}
			return err
		}
		if control.Cancelled || control.Generation != generation || !strings.EqualFold(control.HeadSHA, head) {
			return PermanentJob(fmt.Errorf("%w: review generation changed", ErrStalePublication))
		}
		return nil
	}
	if err := recheck(); err != nil {
		return err
	}
	status := gitlab.CommitStatus{
		State:       "failed",
		Name:        "ai-review",
		Description: preWorkflowFailureDescription,
	}
	return p.setCommitStatus(ctx, token, projectID, strings.ToLower(head), status, recheck)
}
