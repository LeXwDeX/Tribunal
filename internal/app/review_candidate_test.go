package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func (f *reviewFixture) handleCandidate(t *testing.T, wantID string) {
	t.Helper()
	claimed, err := f.db.Claim(context.Background(), "candidate-test", []string{"review.candidate"}, time.Minute)
	if err != nil {
		t.Fatalf("Claim candidate: %v", err)
	}
	if claimed == nil || claimed.ID != wantID {
		t.Fatalf("claimed candidate = %+v, want %s", claimed, wantID)
	}
	if err := f.service.Handle(context.Background(), &jobs.Job{
		ID:          claimed.ID,
		Kind:        claimed.Kind,
		Payload:     claimed.Payload,
		MaxAttempts: claimed.MaxAttempts,
		LeaseOwner:  claimed.LeaseOwner,
		Attempts:    claimed.Attempts,
	}); err != nil {
		t.Fatalf("Handle candidate: %v", err)
	}
	if err := f.db.Complete(context.Background(), *claimed); err != nil {
		t.Fatalf("Complete candidate: %v", err)
	}
}

func (f *reviewFixture) claimReviewRun(t *testing.T) *store.Job {
	t.Helper()
	job, err := f.db.Claim(context.Background(), "review-run-test", []string{"review.run"}, time.Minute)
	if err != nil {
		t.Fatalf("Claim review.run: %v", err)
	}
	return job
}

func (f *reviewFixture) activateCandidate(t *testing.T, candidate *store.Job) *store.Job {
	t.Helper()
	f.handleCandidate(t, candidate.ID)
	return f.claimReviewRun(t)
}

func (f *reviewFixture) runClaimedReview(t *testing.T) *store.Job {
	t.Helper()
	job := f.claimReviewRun(t)
	if job == nil {
		t.Fatal("no queued review.run job")
	}
	if err := f.runQueuedReview(t, job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if err := f.db.Complete(context.Background(), *job); err != nil {
		t.Fatalf("Complete review.run: %v", err)
	}
	return job
}

func (f *reviewFixture) candidateControl(t *testing.T) *store.ReviewControl {
	t.Helper()
	control, err := f.db.GetReviewControl(context.Background(), 42, 7)
	if err != nil {
		t.Fatalf("GetReviewControl: %v", err)
	}
	return control
}

func TestCandidateFilteredPreservesActiveReview(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	manual := f.seedReview(t, "manual-review")
	if manual.ID == "" {
		t.Fatal("manual review job")
	}
	manual = f.runClaimedReview(t)
	workflow, err := f.db.GetWorkflowByJob(context.Background(), manual.ID)
	if err != nil {
		t.Fatalf("manual workflow: %v", err)
	}
	if workflow.Status != "completed" {
		t.Fatalf("manual workflow status = %q", workflow.Status)
	}
	notes := f.gitlab.noteCount()
	before := f.candidateControl(t)
	if before.Generation != 1 || before.HeadSHA != reviewTestHead || before.Cancelled {
		t.Fatalf("manual control = %+v", before)
	}

	f.setMRDraft(true)
	f.setRepoConfig(t, "version: 1\nreview:\n  skip_draft: true\n")
	candidate, result := f.sendMergeRequestWebhook(t, f.repoConfigWebhook(t), func(body map[string]any) {
		patchAttr(body, "draft", true)
	})
	if !result.Accepted {
		t.Fatalf("webhook result = %+v", result)
	}
	if candidate.Kind != "review.candidate" || candidate.Key != "review.candidate:42:7" {
		t.Fatalf("ingress job = %+v, want a deferred candidate", candidate)
	}
	afterIngress := f.candidateControl(t)
	if afterIngress.Generation != 1 || afterIngress.HeadSHA != reviewTestHead ||
		afterIngress.Cancelled || afterIngress.CandidateSeq != 1 || afterIngress.BarrierSeq != 0 {
		t.Fatalf("ingress changed the active intent: %+v", afterIngress)
	}

	f.handleCandidate(t, candidate.ID)
	after := f.candidateControl(t)
	if after.Generation != 1 || after.HeadSHA != reviewTestHead ||
		after.Cancelled || after.CandidateSeq != 1 || after.BarrierSeq != 0 {
		t.Fatalf("filtered candidate changed the active intent: %+v", after)
	}
	if run := f.claimReviewRun(t); run != nil {
		t.Fatalf("filtered candidate enqueued review.run: %+v", run)
	}
	if got := f.gitlab.noteCount(); got != notes {
		t.Fatalf("filtered candidate published %d notes, want %d", got, notes)
	}
	workflow, err = f.db.GetWorkflowByJob(context.Background(), manual.ID)
	if err != nil || workflow.Status != "completed" {
		t.Fatalf("manual workflow after filtered candidate = %+v, %v", workflow, err)
	}
}

func TestCandidateActivatesOnceAndSkipsRefilter(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	manual := f.seedReview(t, "manual-review")
	if manual.ID == "" {
		t.Fatal("manual review job")
	}
	f.runClaimedReview(t)
	notes := f.gitlab.noteCount()

	f.setMRDraft(true)
	f.setRepoConfig(t, "version: 1\nreview:\n  skip_draft: false\n")
	candidate, result := f.sendMergeRequestWebhook(t, f.repoConfigWebhook(t), func(body map[string]any) {
		patchAttr(body, "draft", true)
	})
	if !result.Accepted || candidate.Kind != "review.candidate" {
		t.Fatalf("candidate ingress = %+v job=%+v", result, candidate)
	}
	if control := f.candidateControl(t); control.Generation != 1 || control.CandidateSeq != 1 {
		t.Fatalf("candidate ingress control = %+v", control)
	}
	if got := f.gitlab.noteCount(); got != notes {
		t.Fatalf("pending candidate published %d notes", got-notes)
	}

	f.handleCandidate(t, candidate.ID)
	control := f.candidateControl(t)
	if control.Generation != 2 || control.HeadSHA != reviewTestHead ||
		control.Cancelled || control.BarrierSeq != 1 {
		t.Fatalf("activated control = %+v", control)
	}
	if got := f.gitlab.noteCount(); got != notes {
		t.Fatalf("activation cancelled or published an old review: %d -> %d", notes, got)
	}
	run := f.claimReviewRun(t)
	if run == nil {
		t.Fatal("candidate activation enqueued no review.run")
	}
	var payload map[string]any
	if err := json.Unmarshal(run.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["generation"] != float64(2) || payload["activation_seq"] != float64(1) ||
		payload["head_sha"] != reviewTestHead {
		t.Fatalf("activated review.run payload = %v", payload)
	}
	if _, ok := payload["candidate_seq"]; ok {
		t.Fatalf("review.run kept candidate_seq: %v", payload)
	}

	// The effective config already accepted the event, so a draft MR must not
	// be filtered a second time when review.run executes.
	if err := f.runQueuedReview(t, run); err != nil {
		t.Fatalf("activated RunReview: %v", err)
	}
	if err := f.db.Complete(context.Background(), *run); err != nil {
		t.Fatalf("Complete activated review.run: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("activated workflow: %v", err)
	}
	if workflow.Status != "completed" {
		t.Fatalf("activated workflow status = %q", workflow.Status)
	}
	dossier, err := f.db.GetWorkflowDossier(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("activated dossier: %v", err)
	}
	if dossier.PublicationState != "published" {
		t.Fatalf("activated review was not published: %+v", dossier)
	}
	if f.gitlab.noteCount() == 0 {
		t.Fatal("activated review published no note")
	}
}

func TestCandidateNewerCancelInvalidatesPendingActivation(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	candidate, result := f.sendMergeRequestWebhook(t, f.repoConfigWebhook(t), nil)
	if !result.Accepted || candidate.Kind != "review.candidate" {
		t.Fatalf("candidate ingress = %+v job=%+v", result, candidate)
	}
	if control := f.candidateControl(t); control.Generation != 0 || control.CandidateSeq != 1 {
		t.Fatalf("candidate control = %+v", control)
	}

	closeJob, closeResult := f.sendMergeRequestWebhook(t, f.repoConfigWebhook(t), func(body map[string]any) {
		patchAttr(body, "action", "close")
	})
	if !closeResult.Accepted || closeJob.Kind != "review.cancel" {
		t.Fatalf("close ingress = %+v job=%+v", closeResult, closeJob)
	}
	control := f.candidateControl(t)
	if control.Generation != 1 || !control.Cancelled || control.BarrierSeq != 1 || control.CandidateSeq != 1 {
		t.Fatalf("cancel control = %+v", control)
	}

	f.handleCandidate(t, candidate.ID)
	control = f.candidateControl(t)
	if control.Generation != 1 || !control.Cancelled || control.BarrierSeq != 1 {
		t.Fatalf("stale candidate changed control: %+v", control)
	}
	if run := f.claimReviewRun(t); run != nil {
		t.Fatalf("stale candidate enqueued review.run: %+v", run)
	}

	// A candidate that arrives after the cancel has a higher sequence and can
	// clear the cancellation by activating.
	reopened, reopenResult := f.sendMergeRequestWebhook(t, f.repoConfigWebhook(t), func(body map[string]any) {
		patchAttr(body, "action", "reopen")
	})
	if !reopenResult.Accepted || reopened.Kind != "review.candidate" {
		t.Fatalf("reopen ingress = %+v job=%+v", reopenResult, reopened)
	}
	if control := f.candidateControl(t); control.Generation != 1 || control.CandidateSeq != 2 {
		t.Fatalf("reopen control = %+v", control)
	}
	f.handleCandidate(t, reopened.ID)
	control = f.candidateControl(t)
	if control.Generation != 2 || control.Cancelled || control.BarrierSeq != 2 {
		t.Fatalf("reopened activation control = %+v", control)
	}
	if run := f.claimReviewRun(t); run == nil {
		t.Fatal("reopened candidate did not enqueue review.run")
	}
}

func TestCandidateDuplicateDeliveryKeepsOneJobAndSequence(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	service := f.repoConfigWebhook(t)
	eventUUID := uuid.NewString()
	body := mrBody(42, 7, func(b map[string]any) {
		patchAttr(b, "last_commit", map[string]any{"id": f.head})
		patchMR(b, "last_commit", map[string]any{"id": f.head})
	})
	first, err := service.Handle(context.Background(), WebhookRequest{
		Event: "Merge Request Hook", Token: "whsec", UUID: eventUUID, Body: body,
	})
	if err != nil || !first.Accepted {
		t.Fatalf("first candidate = %+v, %v", first, err)
	}
	second, err := service.Handle(context.Background(), WebhookRequest{
		Event: "Merge Request Hook", Token: "whsec", UUID: eventUUID, Body: body,
	})
	if err != nil || second.Ignored != "duplicate" {
		t.Fatalf("duplicate candidate = %+v, %v", second, err)
	}
	stored, err := f.db.GetJob(context.Background(), first.JobID)
	if err != nil || stored.Kind != "review.candidate" {
		t.Fatalf("candidate job = %+v, %v", stored, err)
	}
	if control := f.candidateControl(t); control.CandidateSeq != 1 {
		t.Fatalf("duplicate allocated a sequence: %+v", control)
	}
}

func TestImmediateWebhookKeepsRunPathWhenRepoConfigDisabled(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.AllowRepoConfig = false
	webhook := f.repoConfigWebhook(t)
	job, result := f.sendMergeRequestWebhook(t, webhook, nil)
	if !result.Accepted || job.Kind != "review.run" || job.Key != "review:42:7" {
		t.Fatalf("immediate ingress = %+v job=%+v", result, job)
	}
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["generation"] != float64(1) || payload["head_sha"] != reviewTestHead {
		t.Fatalf("immediate payload = %v", payload)
	}
	if _, ok := payload["activation_seq"]; ok {
		t.Fatalf("immediate payload carries activation_seq: %v", payload)
	}
	if control := f.candidateControl(t); control.Generation != 1 || control.CandidateSeq != 0 {
		t.Fatalf("immediate control = %+v", control)
	}
}

func TestFilteredCandidateDoesNotDisturbEffectiveReview(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	first, result := f.sendMergeRequestWebhook(t, f.repoConfigWebhook(t), nil)
	if !result.Accepted || first.Kind != "review.candidate" {
		t.Fatalf("first candidate = %+v job=%+v", result, first)
	}
	f.handleCandidate(t, first.ID)
	effective := f.claimReviewRun(t)
	if effective == nil {
		t.Fatal("accepted candidate enqueued no review.run")
	}
	if err := f.db.Complete(context.Background(), *effective); err != nil {
		t.Fatalf("Complete effective review.run: %v", err)
	}

	f.setMRDraft(true)
	f.setRepoConfig(t, "version: 1\nreview:\n  skip_draft: true\n")
	second, result := f.sendMergeRequestWebhook(t, f.repoConfigWebhook(t), func(body map[string]any) {
		patchAttr(body, "draft", true)
	})
	if !result.Accepted || second.Kind != "review.candidate" {
		t.Fatalf("second candidate = %+v job=%+v", result, second)
	}
	f.handleCandidate(t, second.ID)
	control := f.candidateControl(t)
	if control.Generation != 1 || control.BarrierSeq != 1 || control.CandidateSeq != 2 {
		t.Fatalf("filtered successor changed control: %+v", control)
	}
	if extra := f.claimReviewRun(t); extra != nil {
		t.Fatalf("filtered successor enqueued review.run: %+v", extra)
	}
	stored, err := f.db.GetJob(context.Background(), effective.ID)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(stored.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["activation_seq"] != float64(1) || payload["generation"] != float64(1) {
		t.Fatalf("effective review payload changed: %v", payload)
	}
}

func TestFilteredCandidateThenAcceptedSuccessor(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.setMRDraft(true)
	f.setRepoConfig(t, "version: 1\nreview:\n  skip_draft: true\n")
	first, result := f.sendMergeRequestWebhook(t, f.repoConfigWebhook(t), func(body map[string]any) {
		patchAttr(body, "draft", true)
	})
	if !result.Accepted || first.Kind != "review.candidate" {
		t.Fatalf("filtered candidate = %+v job=%+v", result, first)
	}
	f.handleCandidate(t, first.ID)
	if control := f.candidateControl(t); control.Generation != 0 || control.BarrierSeq != 0 {
		t.Fatalf("filtered candidate changed control: %+v", control)
	}

	f.setMRDraft(false)
	f.setRepoConfig(t, "version: 1\nreview:\n  skip_draft: false\n")
	second, result := f.sendMergeRequestWebhook(t, f.repoConfigWebhook(t), func(body map[string]any) {
		patchAttr(body, "action", "reopen")
	})
	if !result.Accepted || second.Kind != "review.candidate" {
		t.Fatalf("accepted successor = %+v job=%+v", result, second)
	}
	f.handleCandidate(t, second.ID)
	control := f.candidateControl(t)
	if control.Generation != 1 || control.BarrierSeq != 2 || control.CandidateSeq != 2 {
		t.Fatalf("accepted successor control = %+v", control)
	}
	run := f.claimReviewRun(t)
	if run == nil {
		t.Fatal("accepted successor enqueued no review.run")
	}
	var payload map[string]any
	if err := json.Unmarshal(run.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["activation_seq"] != float64(2) || payload["generation"] != float64(1) {
		t.Fatalf("successor review payload = %v", payload)
	}
}

func TestCandidateTransientFailureRetriesActivationOnce(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	candidate, result := f.sendMergeRequestWebhook(t, f.repoConfigWebhook(t), nil)
	if !result.Accepted || candidate.Kind != "review.candidate" {
		t.Fatalf("candidate = %+v job=%+v", result, candidate)
	}
	claimed, err := f.db.Claim(context.Background(), "retry-test", []string{"review.candidate"}, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("Claim candidate = %+v, %v", claimed, err)
	}

	f.gitlab.mrErr = errors.New("temporary upstream failure")
	err = f.service.Handle(context.Background(), &jobs.Job{
		ID: claimed.ID, Kind: claimed.Kind, Payload: claimed.Payload,
		MaxAttempts: claimed.MaxAttempts, LeaseOwner: claimed.LeaseOwner, Attempts: claimed.Attempts,
	})
	if err == nil || IsPermanentJob(err) {
		t.Fatalf("transient candidate error = %v", err)
	}
	if control := f.candidateControl(t); control.Generation != 0 || control.BarrierSeq != 0 {
		t.Fatalf("failed attempt changed control: %+v", control)
	}
	if run := f.claimReviewRun(t); run != nil {
		t.Fatalf("failed attempt enqueued review.run: %+v", run)
	}

	// A worker retry re-claims the queued attempt with a new lease.
	if err := f.db.Fail(context.Background(), *claimed, err, nil); err != nil {
		t.Fatalf("Fail candidate: %v", err)
	}
	f.gitlab.mrErr = nil
	retry, err := f.db.Claim(context.Background(), "retry-test", []string{"review.candidate"}, time.Minute)
	if err != nil || retry == nil {
		t.Fatalf("Claim retry = %+v, %v", retry, err)
	}
	if err := f.service.Handle(context.Background(), &jobs.Job{
		ID: retry.ID, Kind: retry.Kind, Payload: retry.Payload,
		MaxAttempts: retry.MaxAttempts, LeaseOwner: retry.LeaseOwner, Attempts: retry.Attempts,
	}); err != nil {
		t.Fatalf("retry Handle: %v", err)
	}
	if err := f.db.Complete(context.Background(), *retry); err != nil {
		t.Fatalf("Complete retry: %v", err)
	}
	control := f.candidateControl(t)
	if control.Generation != 1 || control.BarrierSeq != 1 {
		t.Fatalf("retry control = %+v", control)
	}
	if run := f.claimReviewRun(t); run == nil {
		t.Fatal("retry did not enqueue review.run")
	}
}

func TestActivatedCandidateQueuesBehindRunningReview(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	block := make(chan struct{})
	f.runner.block = block
	f.runner.started = make(chan struct{})
	f.seedReview(t, "manual-review")
	claimed, err := f.db.Claim(context.Background(), "manual", []string{"review.run"}, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("Claim manual = %+v, %v", claimed, err)
	}
	done := make(chan error, 1)
	go func() { done <- f.runQueuedReview(t, claimed) }()
	select {
	case <-f.runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("manual review did not start")
	}

	candidate, result := f.sendMergeRequestWebhook(t, f.repoConfigWebhook(t), nil)
	if !result.Accepted || candidate.Kind != "review.candidate" {
		t.Fatalf("candidate = %+v job=%+v", result, candidate)
	}
	f.handleCandidate(t, candidate.ID)
	close(block)
	select {
	case manualErr := <-done:
		// Losing the generation fence is expected. It may appear as an
		// explicit supersession or as a stale publication at the final check.
		if manualErr != nil && !errors.Is(manualErr, ErrStalePublication) {
			t.Fatalf("superseded manual review error = %v", manualErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("superseded manual review did not stop")
	}
	if err := f.db.Complete(context.Background(), *claimed); err != nil {
		t.Fatalf("Complete manual: %v", err)
	}
	oldWorkflow, err := f.db.GetWorkflowByJob(context.Background(), claimed.ID)
	if err != nil {
		t.Fatalf("old workflow: %v", err)
	}
	if oldWorkflow.Status == "running" || oldWorkflow.Status == "pending" {
		t.Fatalf("old workflow status = %q, want terminal", oldWorkflow.Status)
	}
	if f.gitlab.noteCount() != 0 {
		t.Fatalf("superseded review published %d notes", f.gitlab.noteCount())
	}

	run := f.claimReviewRun(t)
	if run == nil {
		t.Fatal("activated candidate enqueued no queued successor")
	}
	if err := f.runQueuedReview(t, run); err != nil {
		t.Fatalf("successor RunReview: %v", err)
	}
	if err := f.db.Complete(context.Background(), *run); err != nil {
		t.Fatalf("Complete successor: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("successor workflow: %v", err)
	}
	if workflow.Status != "completed" {
		t.Fatalf("successor status = %q", workflow.Status)
	}
	if f.gitlab.noteCount() == 0 {
		t.Fatal("successor published no note")
	}
	if _, err := f.db.GetWorkflow(context.Background(), oldWorkflow.ID); err != nil {
		t.Fatalf("historical workflow was removed: %v", err)
	}
}

func TestCandidateMRClosedCompletesWithoutControlWrite(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	candidate, result := f.sendMergeRequestWebhook(t, f.repoConfigWebhook(t), nil)
	if !result.Accepted || candidate.Kind != "review.candidate" {
		t.Fatalf("candidate = %+v job=%+v", result, candidate)
	}
	f.gitlab.mr.State = "closed"
	f.handleCandidate(t, candidate.ID)
	if control := f.candidateControl(t); control.Generation != 0 || control.BarrierSeq != 0 || control.CandidateSeq != 1 {
		t.Fatalf("closed MR changed control: %+v", control)
	}
	if run := f.claimReviewRun(t); run != nil {
		t.Fatalf("closed MR enqueued review.run: %+v", run)
	}
}
