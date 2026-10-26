package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

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
