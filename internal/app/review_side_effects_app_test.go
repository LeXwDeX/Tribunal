package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
)

func TestReviewRunClosedMRWritesStaleStatus(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.mr.State = "merged"
	job := f.seedReview(t, "job-run-6")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("closed MR should settle the job, got %v", err)
	}
	status, err := f.db.GetReviewStatus(context.Background(), 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if status.State != reviewStatusStale {
		t.Fatalf("closed MR left status %+v, want stale", status)
	}
}

func TestReviewRunIncompleteWorkflowWritesFailedStatus(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-run-incomplete-status")
	f.service.store = &incompleteWorkflowStore{Store: f.db}

	err := f.service.RunReview(context.Background(), job)
	if !IsPermanentJob(err) || !errors.Is(err, store.ErrIncompleteWorkflow) {
		t.Fatalf("RunReview error = %v, want a permanent incomplete workflow", err)
	}
	status, statusErr := f.db.GetReviewStatus(context.Background(), 42, 7)
	if statusErr != nil {
		t.Fatalf("GetReviewStatus: %v", statusErr)
	}
	if status.State != reviewStatusFailed {
		t.Fatalf("incomplete workflow status = %+v, want failed", status)
	}
}

func TestStatusJobTerminalUpstreamErrorSettlesApplied(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	f.gitlab.statusNoteErr = &gitlab.Error{Op: "create_note", Status: 403}
	job := f.seedReview(t, "job-status-terminal")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle review: %v", err)
	}
	claimed, err := f.db.Claim(context.Background(), "status-worker", []string{"review.status"}, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim status job = %+v, %v", claimed, err)
	}
	if err := f.service.Handle(context.Background(), claimed); err != nil {
		t.Fatalf("terminal upstream failure should settle, got %v", err)
	}
	status, err := f.db.GetReviewStatus(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if status.AppliedVersion != status.Version {
		t.Fatalf("terminal upstream failure was not applied: %+v", status)
	}
	if !strings.Contains(status.LastError, "403") {
		t.Fatalf("terminal failure reason = %q, want bounded status", status.LastError)
	}
	next, err := f.db.Claim(context.Background(), "status-worker", []string{"review.status"}, time.Minute)
	if err != nil || next != nil {
		t.Fatalf("terminal status failure kept retrying: %+v, %v", next, err)
	}
}

func TestReconcileSideEffectsReenqueuesPendingGoals(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	if _, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
		Payload: json.RawMessage(`{"trigger":"command","note_id":555}`),
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate a worker that crashed after claiming both jobs: the goals stay
	// pending while the queue rows are gone.
	for _, kind := range []string{"review.status", "review.reaction"} {
		job, err := f.db.Claim(ctx, "crashed-worker", []string{kind}, time.Minute)
		if err != nil || job == nil {
			t.Fatalf("claim %s = %+v, %v", kind, job, err)
		}
		if err := f.db.Complete(ctx, *job); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.ReconcileSideEffects(ctx); err != nil {
		t.Fatalf("ReconcileSideEffects: %v", err)
	}
	for _, kind := range []string{"review.status", "review.reaction"} {
		job, err := f.db.Claim(ctx, "reconciled-worker", []string{kind}, time.Minute)
		if err != nil || job == nil {
			t.Fatalf("reconciled %s = %+v, %v", kind, job, err)
		}
	}
}

func TestReconcileSideEffectsSettlesStaleQueuedCancellation(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	run, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
		Payload: json.RawMessage(`{"trigger":"command","note_id":555}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, Cancel: true,
		Payload: json.RawMessage(`{"trigger":"command","note_id":777}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"review.run", "review.cancel"} {
		job, err := f.db.Claim(ctx, "crashed-worker", []string{kind}, time.Minute)
		if err != nil || job == nil {
			t.Fatalf("claim %s = %+v, %v", kind, job, err)
		}
		if err := f.db.Complete(ctx, *job); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.ReconcileSideEffects(ctx); err != nil {
		t.Fatalf("ReconcileSideEffects: %v", err)
	}
	status, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != reviewStatusCancelled || status.Generation != cancelled.Generation {
		t.Fatalf("stale queued cancellation = %+v", status)
	}
	for _, noteID := range []int64{555, 777} {
		reaction, err := f.db.GetReviewReaction(ctx, 42, 7, noteID)
		if err != nil {
			t.Fatalf("GetReviewReaction(%d): %v", noteID, err)
		}
		if reaction.Desired != reviewAwardNone {
			t.Fatalf("note %d reaction = %+v, want none", noteID, reaction)
		}
	}
	if run.Generation >= cancelled.Generation {
		t.Fatalf("seeded generations did not advance: %d -> %d", run.Generation, cancelled.Generation)
	}
}

// stateListStore returns controlled rows for reconciliation tests without
// waiting for the wall clock to age real rows.
type stateListStore struct {
	*sqlite.Store
	statuses  []store.ReviewStatus
	reactions []store.ReviewReaction
}

func (s *stateListStore) ListReviewStatusesByState(context.Context, []string, time.Time) ([]store.ReviewStatus, error) {
	return append([]store.ReviewStatus(nil), s.statuses...), nil
}

func (s *stateListStore) ListReviewReactionsByDesired(context.Context, string, time.Time) ([]store.ReviewReaction, error) {
	return append([]store.ReviewReaction(nil), s.reactions...), nil
}

func TestReconcileSideEffectsFailsStaleRunningWithoutJob(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	result, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
		Payload: json.RawMessage(`{"trigger":"command"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := f.db.Claim(ctx, "crashed-worker", []string{"review.run"}, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim run = %+v, %v", job, err)
	}
	if err := f.db.Complete(ctx, *job); err != nil {
		t.Fatal(err)
	}
	st := &stateListStore{Store: f.db}
	st.statuses = []store.ReviewStatus{{
		ProjectID: 42, MRIID: 7, Generation: result.Generation,
		HeadSHA: f.head, State: reviewStatusRunning, Version: 1,
	}}
	f.service.store = st
	if err := f.service.ReconcileSideEffects(ctx); err != nil {
		t.Fatalf("ReconcileSideEffects: %v", err)
	}
	status, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != reviewStatusFailed ||
		status.Detail != "review worker ended without terminal status" {
		t.Fatalf("stale running status = %+v", status)
	}
}

func TestReconcileSideEffectsClearsStaleEyes(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	if _, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
		Payload: json.RawMessage(`{"trigger":"command","note_id":555}`),
	}); err != nil {
		t.Fatal(err)
	}
	job, err := f.db.Claim(ctx, "crashed-worker", []string{"review.run"}, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim run = %+v, %v", job, err)
	}
	if err := f.db.Complete(ctx, *job); err != nil {
		t.Fatal(err)
	}
	st := &stateListStore{Store: f.db}
	st.reactions = []store.ReviewReaction{{
		ProjectID: 42, MRIID: 7, NoteID: 555, Generation: 1,
		Desired: reviewAwardEyes, Version: 1,
	}}
	f.service.store = st
	if err := f.service.ReconcileSideEffects(ctx); err != nil {
		t.Fatalf("ReconcileSideEffects: %v", err)
	}
	reaction, err := f.db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil {
		t.Fatal(err)
	}
	if reaction.Desired != reviewAwardNone {
		t.Fatalf("stale eyes reaction = %+v, want none", reaction)
	}
}

func TestReviewRunHeadChangeWritesStaleStatus(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-run-head-change")
	f.gitlab.mr.SHA = strings.Repeat("c", 40)
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	status, err := f.db.GetReviewStatus(context.Background(), 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if status.State != reviewStatusStale {
		t.Fatalf("head change status = %+v, want stale", status)
	}
	if f.runner.count() != 0 {
		t.Fatalf("a superseded run executed %d model calls", f.runner.count())
	}
}

func TestReconcileSideEffectsRecoversCompletedWorkflowInsteadOfFailing(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-reconcile-completed")
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	completed, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || completed.State != reviewStatusPass {
		t.Fatalf("completed status = %+v, %v", completed, err)
	}
	st := &stateListStore{Store: f.db}
	st.statuses = []store.ReviewStatus{{
		ProjectID: 42, MRIID: 7, Generation: completed.Generation,
		HeadSHA: f.head, State: reviewStatusRunning, Version: 1, Profile: "default",
	}}
	f.service.store = st
	if err := f.service.ReconcileSideEffects(ctx); err != nil {
		t.Fatalf("ReconcileSideEffects: %v", err)
	}
	recovered, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != reviewStatusPass || recovered.Detail != "" {
		t.Fatalf("reconciled completed status = %+v, want PASS", recovered)
	}
}
