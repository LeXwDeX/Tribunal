package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

const repositorySkillEvidenceReason = "repository skill provenance unavailable or unsupported; rerun review"

func repositorySkillPublicationError(unknown bool, err error) error {
	if err != nil {
		return err
	}
	if unknown {
		return errDiscussionUnknown
	}
	return PermanentJob(fmt.Errorf("%w: %s", ErrStalePublication, repositorySkillEvidenceReason))
}

func (s *ReviewService) cancelRepositorySkillWorkflow(ctx context.Context, id string) error {
	if err := s.engine.Cancel(ctx, id, repositorySkillEvidenceReason); err != nil && !errors.Is(err, dag.ErrNotFound) {
		return err
	}
	if err := s.waitTerminal(ctx, id); err != nil && !errors.Is(err, dag.ErrNotFound) {
		return err
	}
	if s.publisher == nil {
		return s.skipPublication(ctx, id, repositorySkillEvidenceReason)
	}
	if _, _, err := s.publisher.rejectRepositorySkillPublication(ctx, s.store, id); err != nil && !errors.Is(err, ErrStalePublication) {
		return err
	}
	return nil
}

// The caller holds the workspace read lock; publication and status share the
// same MR-then-workflow lock order as the publisher.
func (p *Publisher) rejectRepositorySkillPublication(ctx context.Context, st ReviewStore, id string) (bool, bool, error) {
	mrUnlock, err := p.lockReviewMR(ctx, id)
	if err != nil {
		return false, false, err
	}
	defer mrUnlock()
	unlock, err := p.locks.lock(ctx, id)
	if err != nil {
		return false, false, err
	}
	defer unlock()
	return p.rejectRepositorySkillPublicationLocked(ctx, st, id)
}

func (p *Publisher) rejectRepositorySkillPublicationLocked(ctx context.Context, st ReviewStore, id string) (bool, bool, error) {
	record, err := st.GetWorkflowDossier(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if record == nil || review.DossierHasNoRepositorySkillCapability(record.Data) {
		return false, false, nil
	}
	var dossier review.Dossier
	if json.Unmarshal(record.Data, &dossier) != nil || !dossier.RepositorySkillCapabilitySupported() {
		return p.rejectRepositorySkillPublicationLockedNow(ctx, st, id)
	}
	if dossier.RepositorySkillProvenance.Mode == "repository" {
		wf, err := st.GetWorkflow(ctx, id)
		if err != nil {
			return true, false, err
		}
		if wf == nil {
			return true, false, nil
		}
		project, err := st.GetProject(ctx, wf.ProjectID)
		if err != nil {
			return true, false, err
		}
		if err := p.repositorySkillPublicationCurrent(ctx, project, &dossier); err != nil {
			return p.rejectRepositorySkillPublicationLockedNow(ctx, st, id)
		}
		return false, false, nil
	}
	return false, false, nil
}

// rejectRepositorySkillPublicationLockedNow settles a workflow whose recorded
// repository capability cannot be revalidated. It never sends repository
// content or a review result derived from it.
func (p *Publisher) rejectRepositorySkillPublicationLockedNow(ctx context.Context, st ReviewStore, id string) (bool, bool, error) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := st.SetWorkflowPublicationStateIfPending(writeCtx, id, repositorySkillEvidenceReason); err != nil {
		return true, false, err
	}
	wf, err := st.GetWorkflow(ctx, id)
	if err != nil {
		return true, false, err
	}
	record, err := st.GetWorkflowDossier(ctx, id)
	if err != nil {
		return true, false, err
	}
	if wf == nil || record == nil || wf.MRIID == nil {
		return true, false, nil
	}
	unknown, err := st.HasUnknownDiscussionIntent(ctx, wf.ProjectID, *wf.MRIID)
	if err != nil {
		return true, false, err
	}
	if err := settleRepositorySkillOutcome(ctx, st, wf, record, unknown); err != nil {
		return true, unknown, err
	}
	return true, unknown, nil
}

// settleRepositorySkillOutcome derives the visible result from durable state.
// A gate-initiated cancellation is stale, not a user cancellation.
func settleRepositorySkillOutcome(ctx context.Context, st ReviewStore, wf *store.Workflow, record *store.WorkflowDossier, unknown bool) error {
	if wf.Kind != "review" || wf.MRIID == nil || *wf.MRIID <= 0 || wf.ProjectID <= 0 ||
		record.WorkflowID != wf.ID || record.ProjectID != wf.ProjectID || record.Generation <= 0 || wf.HeadSHA == "" {
		return nil
	}
	var dossier struct {
		Project     review.ProjectRef `json:"project"`
		MR          review.MRMeta     `json:"mr"`
		DiffVersion struct {
			HeadSHA string
		} `json:"diff_version"`
		Metadata map[string]string `json:"metadata"`
	}
	dossierMatches := json.Unmarshal(record.Data, &dossier) == nil && dossier.Project.ID == wf.ProjectID &&
		dossier.MR.IID == *wf.MRIID && strings.EqualFold(wf.HeadSHA, dossier.DiffVersion.HeadSHA)
	latest, err := st.LatestReviewWorkflow(ctx, wf.ProjectID, *wf.MRIID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	control, err := st.GetReviewControl(ctx, wf.ProjectID, *wf.MRIID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if latest == nil || latest.ID != wf.ID || latest.ProjectID != wf.ProjectID || latest.MRIID == nil || *latest.MRIID != *wf.MRIID ||
		!strings.EqualFold(latest.HeadSHA, wf.HeadSHA) || control.ProjectID != wf.ProjectID || control.MRIID != *wf.MRIID ||
		control.Cancelled || control.Generation != record.Generation || !strings.EqualFold(control.HeadSHA, wf.HeadSHA) {
		return nil
	}
	noteID := int64(0)
	if dossierMatches {
		noteID = dossierNoteID(review.Dossier{Metadata: dossier.Metadata})
	} else if record.JobID != "" {
		job, err := st.GetJob(ctx, record.JobID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		var payload reviewRunPayload
		if job != nil && job.ID == record.JobID && job.Kind == "review.run" && json.Unmarshal(job.Payload, &payload) == nil &&
			payload.ProjectID == wf.ProjectID && payload.MRIID == *wf.MRIID && payload.Generation == record.Generation &&
			strings.EqualFold(payload.HeadSHA, wf.HeadSHA) && payload.NoteID > 0 {
			mapped, err := st.GetWorkflowByJob(ctx, job.ID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if mapped != nil && mapped.ID == wf.ID && mapped.ProjectID == wf.ProjectID && mapped.MRIID != nil &&
				*mapped.MRIID == *wf.MRIID && strings.EqualFold(mapped.HeadSHA, wf.HeadSHA) {
				noteID = payload.NoteID
			}
		}
	}
	state, reaction, detail := "", "", ""
	switch wf.Status {
	case string(dag.WorkflowFailed):
		state, reaction = reviewStatusFailed, reviewAwardFail
	case string(dag.WorkflowCancelled):
		state, reaction = reviewStatusCancelled, reviewAwardNone
		if wf.CancelReason == repositorySkillEvidenceReason {
			state, detail = reviewStatusStale, repositorySkillEvidenceReason
		}
	case string(dag.WorkflowCompleted):
		if unknown {
			state, reaction, detail = reviewStatusStale, reviewAwardNone, repositorySkillEvidenceReason
			break
		}
		switch record.PublicationState {
		case store.WorkflowPublicationSkipped:
			state, reaction, detail = reviewStatusStale, reviewAwardNone, repositorySkillEvidenceReason
		case store.WorkflowPublicationPublished, store.WorkflowPublicationFailed:
			state, reaction, detail = completedPublicationStatus(wf.Verdict, record.PublicationState)
		}
	}
	if state == "" {
		return nil
	}
	if unknown {
		if detail != "" {
			detail += "; "
		}
		detail += store.WorkflowPublicationUnknownReason
	}
	if !dossierMatches && noteID == 0 {
		if detail != "" {
			detail += "; "
		}
		detail += "reaction identity unavailable"
	}
	current, err := st.GetReviewStatus(ctx, wf.ProjectID, *wf.MRIID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	attempt := 0
	if current == nil || current.Generation != record.Generation || current.State != state || current.Detail != detail {
		if current != nil && current.Generation == record.Generation && terminalReviewStatus(current.State) {
			attempt = current.Attempt + 1
		}
		written, err := st.SetReviewStatus(ctx, store.ReviewStatusGoal{
			ProjectID: wf.ProjectID, MRIID: *wf.MRIID, Generation: record.Generation,
			Attempt: attempt, HeadSHA: wf.HeadSHA, Profile: wf.Profile, State: state, Detail: detail,
		})
		if errors.Is(err, store.ErrSuperseded) || (err == nil && !written) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	if noteID > 0 {
		currentReaction, err := st.GetReviewReaction(ctx, wf.ProjectID, *wf.MRIID, noteID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if currentReaction != nil && currentReaction.Generation == record.Generation && currentReaction.Desired == reaction {
			return nil
		}
		_, err = st.SetReviewReaction(ctx, store.ReviewReactionGoal{
			ProjectID: wf.ProjectID, MRIID: *wf.MRIID, NoteID: noteID,
			Generation: record.Generation, Desired: reaction,
		})
		if !errors.Is(err, store.ErrSuperseded) {
			return err
		}
	}
	return nil
}
