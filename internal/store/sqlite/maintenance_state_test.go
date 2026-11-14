package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestFeedbackCollectionStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)

	state, err := st.FeedbackCollectionState(ctx)
	if err != nil {
		t.Fatalf("FeedbackCollectionState: %v", err)
	}
	if state == nil || state.LastCollectedAt != nil || state.LastErrorClass != "" {
		t.Fatalf("empty collection state = %+v", state)
	}

	first := clock.Now().UTC()
	if err := st.RecordFeedbackCollection(ctx, first, ""); err != nil {
		t.Fatalf("record success: %v", err)
	}
	clock.Advance(time.Hour)
	failureAt := clock.Now().UTC()
	if err := st.RecordFeedbackCollection(ctx, failureAt, "upstream"); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	state, err = st.FeedbackCollectionState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastCollectedAt == nil || !state.LastCollectedAt.Equal(first) ||
		state.LastErrorClass != "upstream" || state.LastErrorAt == nil || !state.LastErrorAt.Equal(failureAt) {
		t.Fatalf("failed state = %+v", state)
	}

	clock.Advance(time.Hour)
	recovered := clock.Now().UTC()
	if err := st.RecordFeedbackCollection(ctx, recovered, ""); err != nil {
		t.Fatal(err)
	}
	state, err = st.FeedbackCollectionState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A success advances the collection clock but keeps the most recent error
	// visible; LastCollectedAt after LastErrorAt is what shows recovery.
	if state.LastCollectedAt == nil || !state.LastCollectedAt.Equal(recovered) ||
		state.LastErrorClass != "upstream" || state.LastErrorAt == nil {
		t.Fatalf("recovered state = %+v", state)
	}
}

func TestStatsExposesFeedbackCollectionState(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	at := clock.Now().UTC()
	if err := st.RecordFeedbackCollection(ctx, at, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordFeedbackCollection(ctx, at.Add(time.Minute), "auth"); err != nil {
		t.Fatal(err)
	}
	stats, err := st.Stats(ctx, store.StatsFilter{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.FeedbackCollection == nil || stats.FeedbackCollection.LastErrorClass != "auth" ||
		stats.FeedbackCollection.LastCollectedAt == nil {
		t.Fatalf("stats feedback collection = %+v", stats.FeedbackCollection)
	}
}

func TestWorkspaceCapacityRoundTrip(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	measured := clock.Now().UTC()
	capacity := store.WorkspaceCapacity{
		MeasuredAt: measured, TotalBytes: 30, LimitBytes: 20, OverLimit: true,
	}
	if err := st.RecordWorkspaceCapacity(ctx, capacity); err != nil {
		t.Fatalf("RecordWorkspaceCapacity: %v", err)
	}
	got, err := st.WorkspaceCapacity(ctx)
	if err != nil || got == nil {
		t.Fatalf("WorkspaceCapacity = %+v, %v", got, err)
	}
	if !got.MeasuredAt.Equal(measured) || got.TotalBytes != 30 || got.LimitBytes != 20 || !got.OverLimit {
		t.Fatalf("capacity = %+v", got)
	}
	stats, err := st.Stats(ctx, store.StatsFilter{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.WorkspaceCapacity == nil || stats.WorkspaceCapacity.TotalBytes != 30 || !stats.WorkspaceCapacity.OverLimit {
		t.Fatalf("stats capacity = %+v", stats.WorkspaceCapacity)
	}
}

func TestWorkspaceReclaimStatusFailsClosed(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	onboardJob := mustProject(t, st, 42)
	if job, err := st.Claim(ctx, "maintenance-test", []string{"workspace.onboard"}, time.Minute); err != nil || job == nil {
		t.Fatalf("claim onboarding job %s: %+v, %v", onboardJob, job, err)
	} else if err := st.Complete(ctx, *job); err != nil {
		t.Fatalf("complete onboarding job: %v", err)
	}

	// Registering the project is activity; it is not protected until a review
	// starts.
	status, err := st.WorkspaceReclaimStatus(ctx, 42)
	if err != nil {
		t.Fatalf("WorkspaceReclaimStatus: %v", err)
	}
	if status.Protected || status.LastActivity.IsZero() {
		t.Fatalf("fresh project status = %+v, want unprotected with activity", status)
	}

	// A completed workflow is not protected, but its timestamps are activity.
	wfID := "wf-reclaim"
	mustCreateWorkflow(t, st, wfID, reviewMeta(42, 7, "head"), clock.Now())
	finished := clock.Now().Add(time.Minute)
	if _, err := st.db.ExecContext(ctx, `UPDATE workflows SET status = 'completed', finished_at = ? WHERE id = ?`,
		formatTime(finished), wfID); err != nil {
		t.Fatalf("complete workflow: %v", err)
	}
	status, err = st.WorkspaceReclaimStatus(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if status.Protected {
		t.Fatalf("completed workflow is protected: %+v", status)
	}
	if status.LastActivity.IsZero() {
		t.Fatal("completed workflow left no activity timestamp")
	}

	// A pending workflow protects the workspace again.
	mustCreateWorkflow(t, st, "wf-active", reviewMeta(42, 8, "head-active"), clock.Now())
	status, err = st.WorkspaceReclaimStatus(ctx, 42)
	if err != nil || !status.Protected {
		t.Fatalf("pending workflow status = %+v, %v, want protected", status, err)
	}
}

func TestWorkspaceReclaimStatusProtectsQueuedReviewJob(t *testing.T) {
	for _, key := range []string{
		"review:43:9",
		"review.cancel:43:9",
		"review.candidate:43:9",
		"review.status:43:9",
		"review.reaction:43:9:100",
		"ws:43",
	} {
		t.Run(key, func(t *testing.T) {
			ctx := context.Background()
			clock := newFakeClock()
			st := openTestStore(t, clock)
			mustProject(t, st, 43)
			if _, _, err := st.Enqueue(ctx, store.Job{
				Kind: "review.run", Key: key, Payload: []byte(`{}`), MaxAttempts: 3,
			}); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			status, err := st.WorkspaceReclaimStatus(ctx, 43)
			if err != nil || !status.Protected {
				t.Fatalf("queued job %q status = %+v, %v, want protected", key, status, err)
			}
		})
	}
}

func TestWorkspaceReclaimStatusProtectsPendingDossier(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 44)
	now := formatTime(clock.Now().UTC())
	if _, err := st.db.ExecContext(ctx, `INSERT INTO workflow_dossiers
		(workflow_id, generation, version, hash, data, publication_state, project_id, created_at, updated_at)
		VALUES ('wf-dossier', 0, 1, 'hash', x'00', 'pending', 44, ?, ?)`, now, now); err != nil {
		t.Fatalf("insert pending dossier: %v", err)
	}
	status, err := st.WorkspaceReclaimStatus(ctx, 44)
	if err != nil || !status.Protected {
		t.Fatalf("pending dossier status = %+v, %v, want protected", status, err)
	}
}

func TestProtectedMergeRequestIIDs(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	seedPendingStatus(t, st, 42, 7)
	seedPendingStatus(t, st, 42, 8)
	applyStatus(t, st, 42, 8, store.ReviewStatusPass)

	ids, err := st.ProtectedMergeRequestIIDs(ctx, 42)
	if err != nil {
		t.Fatalf("ProtectedMergeRequestIIDs: %v", err)
	}
	if _, ok := ids[8]; ok {
		t.Fatalf("terminal MR 8 is protected: %v", ids)
	}
	if _, ok := ids[7]; !ok {
		t.Fatalf("queued MR 7 is not protected: %v", ids)
	}
}
