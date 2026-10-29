package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// CancelReview executes one review.cancel job. It never publishes.
func (s *ReviewService) CancelReview(ctx context.Context, job *jobs.Job) error {
	var payload reviewCancelPayload
	if err := decodeReviewPayload(job.Payload, &payload); err != nil {
		return err
	}
	if payload.ProjectID <= 0 || payload.MRIID <= 0 || payload.NoteID < 0 {
		return PermanentJob(fmt.Errorf("invalid review.cancel payload"))
	}
	ws := s.workspace.Get(payload.ProjectID)
	if ws == nil {
		return PermanentJob(fmt.Errorf("workspace is unavailable"))
	}
	unlock, err := ws.LifecycleRLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := consumerJobExists(ctx, s.store, job, "review.cancel", payload.ProjectID); err != nil {
		return err
	}
	project, err := s.store.GetProject(ctx, payload.ProjectID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && project == nil) {
		return PermanentJob(fmt.Errorf("project is not registered"))
	}
	if err != nil {
		return err
	}
	err = s.cancelReview(ctx, payload)
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
	unlock, err := s.publisher.lockWorkflow(ctx, payload.WorkflowID)
	if errors.Is(err, store.ErrNotFound) {
		return PermanentJob(fmt.Errorf("workflow is missing"))
	}
	if err != nil {
		return err
	}
	defer unlock()
	stored, err := s.store.GetJob(ctx, job.ID)
	if errors.Is(err, store.ErrNotFound) {
		return PermanentJob(fmt.Errorf("consumer job no longer exists"))
	}
	if err != nil {
		return err
	}
	var original reviewPublishPayload
	if stored == nil || stored.ID != job.ID || stored.Kind != "review.publish" || job.Kind != "review.publish" || json.Unmarshal(stored.Payload, &original) != nil || original.WorkflowID != payload.WorkflowID {
		return PermanentJob(fmt.Errorf("consumer job identity changed"))
	}
	return s.publisher.republishLocked(ctx, payload.WorkflowID)
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
	token, err := s.cipher.Open(project.TokenEnc, secret.TokenAAD(project.ID))
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

// cancelInvalidRepoConfig terminates a workflow whose repository configuration
// cannot be applied. It keeps the durable terminal state and leaves a visible
// explanation instead of reporting the review as superseded.
func (s *ReviewService) cancelInvalidRepoConfig(ctx context.Context, workflow *store.Workflow, dossier *review.Dossier, generation int64, token string) error {
	if err := s.engine.Cancel(ctx, workflow.ID, invalidRepoConfigDetail); err != nil && !errors.Is(err, dag.ErrNotFound) {
		return err
	}
	if err := s.waitTerminal(ctx, workflow.ID); err != nil && !errors.Is(err, dag.ErrNotFound) {
		return err
	}
	if err := s.skipPublication(ctx, workflow.ID, invalidRepoConfigDetail); err != nil {
		return err
	}
	profile := strings.TrimSpace(dossier.Metadata["requested_profile"])
	if profile == "" {
		profile = "default"
	}
	mrIID := dossier.MR.IID
	s.noteInvalidRepoConfig(ctx, token, workflow.ProjectID, mrIID)
	s.settleReviewStatus(ctx, workflow.ProjectID, mrIID, generation, 0,
		dossier.DiffVersion.HeadSHA, profile, reviewStatusFailed, invalidRepoConfigDetail,
		s.workflowNoteID(ctx, workflow.ID), reviewAwardNone)
	return nil
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
	return s.publisher.publishLocked(ctx, workflowID)
}

func (s *ReviewService) publishFailure(ctx context.Context, workflowID string, snapshot dag.WorkflowView) error {
	if s.publisher == nil {
		return errors.New("app: review publisher is not configured")
	}
	return s.publisher.publishFailureLocked(ctx, workflowID, failureReason(snapshot))
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
