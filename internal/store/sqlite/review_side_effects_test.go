package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func mustReviewStatus(t *testing.T, s *Store, projectID, mrIID int64) *store.ReviewStatus {
	t.Helper()
	status, err := s.GetReviewStatus(context.Background(), projectID, mrIID)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	return status
}

func mustReviewReaction(t *testing.T, s *Store, projectID, mrIID, noteID int64) *store.ReviewReaction {
	t.Helper()
	reaction, err := s.GetReviewReaction(context.Background(), projectID, mrIID, noteID)
	if err != nil {
		t.Fatalf("GetReviewReaction(%d): %v", noteID, err)
	}
	return reaction
}

func jobWithKeyState(t *testing.T, s *Store, kind, key, state string) *store.Job {
	t.Helper()
	row := s.db.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE kind = ? AND "key" = ? AND state = ?`,
		kind, key, state)
	job, err := scanJob(row)
	if err != nil {
		t.Fatalf("find job %s %s %s: %v", kind, key, state, err)
	}
	return job
}

func TestEnqueueReviewRecordsStatusAndNoteReaction(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)
	head := reviewHead("a")

	result, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID:   42,
		MRIID:       7,
		HeadSHA:     head,
		Payload:     json.RawMessage(`{"trigger":"command","profile":"default","note_id":555}`),
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest: %v", err)
	}
	status := mustReviewStatus(t, s, 42, 7)
	if status.State != store.ReviewStatusQueued || status.Generation != result.Generation ||
		status.HeadSHA != head || status.Profile != "default" {
		t.Fatalf("status after enqueue = %+v", status)
	}
	if status.AppliedVersion != 0 || status.Version == 0 {
		t.Fatalf("status version = %d applied = %d", status.Version, status.AppliedVersion)
	}
	if job := jobWithKeyState(t, s, reviewStatusKind, "review.status:42:7", store.JobQueued); job == nil {
		t.Fatal("review.status job was not queued")
	}
	reaction := mustReviewReaction(t, s, 42, 7, 555)
	if reaction.Desired != store.ReviewReactionEyes || reaction.Generation != result.Generation {
		t.Fatalf("note reaction after enqueue = %+v", reaction)
	}
	jobWithKeyState(t, s, reviewReactionKind, "review.reaction:42:7:555", store.JobQueued)

	// A replacement queued run owns a new note and clears the previous eyes.
	nextHead := reviewHead("b")
	replaced, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID:   42,
		MRIID:       7,
		HeadSHA:     nextHead,
		Payload:     json.RawMessage(`{"trigger":"command","profile":"deep","note_id":556}`),
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest replacement: %v", err)
	}
	if replaced.Generation <= result.Generation {
		t.Fatalf("generation did not advance: %d -> %d", result.Generation, replaced.Generation)
	}
	status = mustReviewStatus(t, s, 42, 7)
	if status.State != store.ReviewStatusQueued || status.Generation != replaced.Generation ||
		status.HeadSHA != nextHead || status.Profile != "deep" {
		t.Fatalf("status after replacement = %+v", status)
	}
	failedReaction := mustReviewReaction(t, s, 42, 7, 555)
	if failedReaction.Desired != store.ReviewReactionNone {
		t.Fatalf("previous note reaction = %+v, want none", failedReaction)
	}
	if current := mustReviewReaction(t, s, 42, 7, 556); current.Desired != store.ReviewReactionEyes {
		t.Fatalf("replacement note reaction = %+v", current)
	}
	jobWithKeyState(t, s, reviewReactionKind, "review.reaction:42:7:555", store.JobQueued)
	jobWithKeyState(t, s, reviewReactionKind, "review.reaction:42:7:556", store.JobQueued)
}

func TestEnqueueCancelMovesEyesToCancelNote(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")
	run, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
		Payload: json.RawMessage(`{"trigger":"command","note_id":555}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, Cancel: true,
		Payload: json.RawMessage(`{"trigger":"command","note_id":777}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Generation <= run.Generation {
		t.Fatalf("cancel generation = %d, run = %d", cancelled.Generation, run.Generation)
	}
	status := mustReviewStatus(t, s, 42, 7)
	if status.State != store.ReviewStatusQueued || status.Generation != cancelled.Generation || status.HeadSHA != head {
		t.Fatalf("cancel status = %+v", status)
	}
	// A cancellation without a head must preserve the accepted head for
	// fencing and recovery.
	if written, err := s.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: cancelled.Generation,
		State: store.ReviewStatusCancelled, Detail: "review cancelled",
	}); err != nil || !written {
		t.Fatalf("cancel terminal write = %v, %v", written, err)
	}
	if status = mustReviewStatus(t, s, 42, 7); status.HeadSHA != head {
		t.Fatalf("cancel terminal write cleared the head: %+v", status)
	}
	if old := mustReviewReaction(t, s, 42, 7, 555); old.Desired != store.ReviewReactionNone {
		t.Fatalf("run note reaction = %+v", old)
	}
	if current := mustReviewReaction(t, s, 42, 7, 777); current.Desired != store.ReviewReactionEyes ||
		current.Generation != cancelled.Generation {
		t.Fatalf("cancel note reaction = %+v", current)
	}
}

func TestSetReviewStatusFencesGenerationHeadAndTerminalState(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	headA := reviewHead("a")
	headB := reviewHead("b")
	first, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headA,
		Payload: json.RawMessage(`{"trigger":"command","profile":"default"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	goal := store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: first.Generation, Attempt: 1,
		HeadSHA: headA, Profile: "default", State: store.ReviewStatusRunning,
	}
	written, err := s.SetReviewStatus(ctx, goal)
	if err != nil || !written {
		t.Fatalf("running write = %v, %v", written, err)
	}
	goal.State = store.ReviewStatusPass
	if written, err := s.SetReviewStatus(ctx, goal); err != nil || !written {
		t.Fatalf("terminal write = %v, %v", written, err)
	}
	status := mustReviewStatus(t, s, 42, 7)
	if status.State != store.ReviewStatusPass || status.AppliedVersion != 0 {
		t.Fatalf("status after terminal write = %+v", status)
	}
	beforeVersion := status.Version

	// A same-attempt terminal status is final: no non-terminal or other
	// terminal state may overwrite it. A higher attempt may retry the job.
	goal.State = store.ReviewStatusFailed
	if written, err := s.SetReviewStatus(ctx, goal); err != nil || written {
		t.Fatalf("same-attempt terminal overwrite = %v, %v", written, err)
	}
	goal.State = store.ReviewStatusRunning
	if written, err := s.SetReviewStatus(ctx, goal); err != nil || written {
		t.Fatalf("same-attempt non-terminal overwrite = %v, %v", written, err)
	}
	if status = mustReviewStatus(t, s, 42, 7); status.Version != beforeVersion || status.State != store.ReviewStatusPass {
		t.Fatalf("rejected writes changed the row: %+v", status)
	}

	// A newer generation owns the MR. An older worker may not write.
	successor, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headB,
		Payload: json.RawMessage(`{"trigger":"mr_update","profile":"default"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	stale := goal
	stale.Attempt = 2
	if written, err := s.SetReviewStatus(ctx, stale); !errors.Is(err, store.ErrSuperseded) || written {
		t.Fatalf("stale generation write = %v, %v; want ErrSuperseded", written, err)
	}
	if status = mustReviewStatus(t, s, 42, 7); status.Generation != successor.Generation || status.State != store.ReviewStatusQueued {
		t.Fatalf("successor status was overwritten: %+v", status)
	}
	stale.Generation = successor.Generation
	if written, err := s.SetReviewStatus(ctx, stale); !errors.Is(err, store.ErrSuperseded) || written {
		t.Fatalf("stale head write = %v, %v; want ErrSuperseded", written, err)
	}
	current := store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: successor.Generation, Attempt: 1,
		HeadSHA: headB, Profile: "default", State: store.ReviewStatusRunning,
	}
	if written, err := s.SetReviewStatus(ctx, current); err != nil || !written {
		t.Fatalf("current generation write = %v, %v", written, err)
	}
}

func TestReviewStatusAppliedVersionIsFenced(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")
	result, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
		Payload: json.RawMessage(`{"trigger":"command"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	queued := mustReviewStatus(t, s, 42, 7)
	if _, err := s.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: result.Generation, Attempt: 1,
		HeadSHA: head, State: store.ReviewStatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	running := mustReviewStatus(t, s, 42, 7)
	if running.Version <= queued.Version {
		t.Fatalf("version did not advance: %d -> %d", queued.Version, running.Version)
	}
	if err := s.MarkReviewStatusApplied(ctx, 42, 7, int64(queued.Version), 900, "old-hash", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale applied write = %v, want ErrConflict", err)
	}
	if status := mustReviewStatus(t, s, 42, 7); status.AppliedVersion != 0 {
		t.Fatalf("stale applied write changed the row: %+v", status)
	}
	if err := s.MarkReviewStatusApplied(ctx, 42, 7, int64(running.Version), 901, "body-hash", ""); err != nil {
		t.Fatalf("current applied write: %v", err)
	}
	status := mustReviewStatus(t, s, 42, 7)
	if status.AppliedVersion != running.Version || status.NoteID != 901 || status.BodyHash != "body-hash" {
		t.Fatalf("status after applied write = %+v", status)
	}
	if err := s.MarkReviewStatusApplied(ctx, 42, 7, int64(running.Version), 0, "", ""); err != nil {
		t.Fatalf("idempotent applied write: %v", err)
	}
	if again := mustReviewStatus(t, s, 42, 7); again.NoteID != 901 || again.BodyHash != "body-hash" {
		t.Fatalf("idempotent applied write cleared metadata: %+v", again)
	}
}

func TestSetReviewReactionProtectsNewerGeneration(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")
	first, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
		Payload: json.RawMessage(`{"trigger":"command","note_id":555}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	successor, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
		Payload: json.RawMessage(`{"trigger":"command","note_id":555}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if successor.Generation <= first.Generation {
		t.Fatal("successor generation did not advance")
	}
	stale := store.ReviewReactionGoal{
		ProjectID: 42, MRIID: 7, NoteID: 555, Generation: first.Generation,
		Desired: store.ReviewReactionPass,
	}
	if written, err := s.SetReviewReaction(ctx, stale); err != nil || written {
		t.Fatalf("stale reaction write = %v, %v", written, err)
	}
	if current := mustReviewReaction(t, s, 42, 7, 555); current.Desired != store.ReviewReactionEyes ||
		current.Generation != successor.Generation {
		t.Fatalf("stale reaction overwrote current row: %+v", current)
	}
	// A stale generation may never remove a successor's eyes either.
	stale.Desired = store.ReviewReactionNone
	if written, err := s.SetReviewReaction(ctx, stale); err != nil || written {
		t.Fatalf("stale none write = %v, %v", written, err)
	}
	if written, err := s.SetReviewReaction(ctx, store.ReviewReactionGoal{
		ProjectID: 42, MRIID: 7, NoteID: 555, Generation: successor.Generation, Desired: store.ReviewReactionNone,
	}); err != nil || !written {
		t.Fatalf("none write = %v, %v", written, err)
	}
	if current := mustReviewReaction(t, s, 42, 7, 555); current.Desired != store.ReviewReactionNone {
		t.Fatalf("reaction after none = %+v", current)
	}
}

func TestHasActiveReviewJobCoversRunCancelCandidate(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	if active, err := s.HasActiveReviewJob(ctx, 42, 7); err != nil || active {
		t.Fatalf("empty active = %v, %v", active, err)
	}
	result, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("a"),
		Payload: json.RawMessage(`{"trigger":"command"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if active, err := s.HasActiveReviewJob(ctx, 42, 7); err != nil || !active {
		t.Fatalf("queued run active = %v, %v", active, err)
	}
	claimed, err := s.Claim(ctx, "worker", []string{reviewRunKind}, time.Minute)
	if err != nil || claimed == nil || claimed.ID != result.JobID {
		t.Fatalf("claim run = %+v, %v", claimed, err)
	}
	if err := s.Complete(ctx, *claimed); err != nil {
		t.Fatal(err)
	}
	if active, err := s.HasActiveReviewJob(ctx, 42, 7); err != nil || active {
		t.Fatalf("completed run active = %v, %v", active, err)
	}
	if _, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("candidate-active"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("b"),
		Payload: candidatePayload(t, "mr_update"),
	}); err != nil {
		t.Fatal(err)
	}
	if active, err := s.HasActiveReviewJob(ctx, 42, 7); err != nil || !active {
		t.Fatalf("queued candidate active = %v, %v", active, err)
	}
}

func TestRecentReviewDurationsUseRecentMatchingSamples(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)
	mustProject(t, s, 43)
	now := clock.Now()
	insert := func(id string, projectID int64, profile, status, verdict string, started, finished time.Time) {
		t.Helper()
		_, err := s.db.Exec(`INSERT INTO workflows
			(id, kind, project_id, mr_iid, head_sha, profile, trigger, status, verdict,
			 created_at, started_at, finished_at)
			VALUES (?, 'review', ?, 7, ?, ?, 'api', ?, ?, ?, ?, ?)`,
			id, projectID, reviewHead("a"), profile, status, verdict,
			formatTime(started), formatTime(started), formatTime(finished))
		if err != nil {
			t.Fatalf("insert workflow %s: %v", id, err)
		}
	}
	insert("wf-2m", 42, "default", "completed", "PASS", now.Add(-4*time.Hour), now.Add(-4*time.Hour+2*time.Minute))
	insert("wf-4m", 42, "default", "completed", "BLOCKING", now.Add(-3*time.Hour), now.Add(-3*time.Hour+4*time.Minute))
	insert("wf-9m", 42, "default", "completed", "INCOMPLETE", now.Add(-2*time.Hour), now.Add(-2*time.Hour+9*time.Minute))
	insert("wf-failed", 42, "default", "failed", "", now.Add(-time.Hour), now.Add(-time.Hour+time.Minute))
	insert("wf-other-profile", 42, "deep", "completed", "PASS", now.Add(-time.Hour), now.Add(-time.Hour+time.Minute))
	insert("wf-other-project", 43, "default", "completed", "PASS", now.Add(-time.Hour), now.Add(-time.Hour+time.Minute))
	insert("wf-old", 42, "default", "completed", "PASS", now.Add(-40*24*time.Hour), now.Add(-40*24*time.Hour+time.Minute))

	durations, err := s.RecentReviewDurations(ctx, 42, "default", now.Add(-30*24*time.Hour), 20)
	if err != nil {
		t.Fatalf("RecentReviewDurations: %v", err)
	}
	if len(durations) != 3 {
		t.Fatalf("durations = %v, want three samples", durations)
	}
	seen := map[time.Duration]bool{}
	for _, d := range durations {
		seen[d] = true
	}
	for _, want := range []time.Duration{2 * time.Minute, 4 * time.Minute, 9 * time.Minute} {
		if !seen[want] {
			t.Fatalf("durations = %v, missing %s", durations, want)
		}
	}
	limited, err := s.RecentReviewDurations(ctx, 42, "default", now.Add(-30*24*time.Hour), 2)
	if err != nil || len(limited) != 2 {
		t.Fatalf("limited durations = %v, %v", limited, err)
	}
}

func TestActivateCandidateRecordsQueuedStatusAndClearsRunEyes(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	headA := reviewHead("a")
	headB := reviewHead("b")
	run, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headA,
		Payload: json.RawMessage(`{"trigger":"command","note_id":555}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("candidate-1"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: headB,
		Payload: candidatePayload(t, "mr_update"),
	}); err != nil {
		t.Fatal(err)
	}
	job := claimCandidate(t, s, "candidate-worker")
	var payload struct {
		CandidateSeq int64 `json:"candidate_seq"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	activatedID, generation, err := s.ActivateReviewCandidate(ctx, activationFor(job, headB, payload.CandidateSeq))
	if err != nil {
		t.Fatalf("ActivateReviewCandidate: %v", err)
	}
	if activatedID == "" || generation <= run.Generation {
		t.Fatalf("activation = %q generation %d", activatedID, generation)
	}
	status := mustReviewStatus(t, s, 42, 7)
	if status.State != store.ReviewStatusQueued || status.Generation != generation || status.HeadSHA != headB {
		t.Fatalf("candidate status = %+v", status)
	}
	if old := mustReviewReaction(t, s, 42, 7, 555); old.Desired != store.ReviewReactionNone {
		t.Fatalf("run note eyes survived candidate activation: %+v", old)
	}
	if count := rowCount(t, s, `SELECT COUNT(*) FROM review_reactions WHERE project_id = 42 AND mr_iid = 7`); count != 1 {
		t.Fatalf("candidate fabricated a reaction row: %d", count)
	}
	if active, err := s.HasActiveReviewJob(ctx, 42, 7); err != nil || !active {
		t.Fatalf("activated run active = %v, %v", active, err)
	}
}

func TestClearReviewReactionsForGeneration(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	result, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("a"),
		Payload: json.RawMessage(`{"trigger":"command","note_id":555}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	notes, err := s.ClearReviewReactionsForGeneration(ctx, 42, 7, result.Generation)
	if err != nil {
		t.Fatalf("ClearReviewReactionsForGeneration: %v", err)
	}
	if len(notes) != 1 || notes[0] != 555 {
		t.Fatalf("cleared notes = %v", notes)
	}
	if reaction := mustReviewReaction(t, s, 42, 7, 555); reaction.Desired != store.ReviewReactionNone {
		t.Fatalf("reaction after clear = %+v", reaction)
	}
	if again, err := s.ClearReviewReactionsForGeneration(ctx, 42, 7, result.Generation); err != nil || len(again) != 0 {
		t.Fatalf("second clear = %v, %v", again, err)
	}
}

func TestEnqueueRejectsReservedPayloadWithoutSideEffects(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	_, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("a"),
		Payload: json.RawMessage(`{"trigger":"command","note_id":555,"generation":1}`),
	})
	if err == nil {
		t.Fatal("reserved generation in the command payload was accepted")
	}
	if _, err := s.GetReviewStatus(ctx, 42, 7); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed enqueue left a status goal: %v", err)
	}
	if _, err := s.GetReviewReaction(ctx, 42, 7, 555); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed enqueue left a reaction goal: %v", err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE kind IN ('review.run', 'review.status', 'review.reaction')`); got != 0 {
		t.Fatalf("failed enqueue left %d jobs", got)
	}
	if _, err := s.GetReviewControl(ctx, 42, 7); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed enqueue committed a control row: %v", err)
	}
}

func TestReviewReactionAppliedVersionIsFenced(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	if _, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("a"),
		Payload: json.RawMessage(`{"trigger":"command","note_id":555}`),
	}); err != nil {
		t.Fatal(err)
	}
	initial := mustReviewReaction(t, s, 42, 7, 555)
	if written, err := s.SetReviewReaction(ctx, store.ReviewReactionGoal{
		ProjectID: 42, MRIID: 7, NoteID: 555, Generation: initial.Generation,
		Desired: store.ReviewReactionPass,
	}); err != nil || !written {
		t.Fatalf("reaction write = %v, %v", written, err)
	}
	current := mustReviewReaction(t, s, 42, 7, 555)
	if current.Version <= initial.Version {
		t.Fatalf("reaction version did not advance: %d -> %d", initial.Version, current.Version)
	}
	if err := s.MarkReviewReactionApplied(ctx, 42, 7, 555, int64(initial.Version), ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale reaction applied write = %v, want ErrConflict", err)
	}
	if reaction := mustReviewReaction(t, s, 42, 7, 555); reaction.AppliedVersion != 0 {
		t.Fatalf("stale reaction applied write changed the row: %+v", reaction)
	}
	if err := s.MarkReviewReactionApplied(ctx, 42, 7, 555, int64(current.Version), ""); err != nil {
		t.Fatalf("current reaction applied write: %v", err)
	}
	if reaction := mustReviewReaction(t, s, 42, 7, 555); reaction.AppliedVersion != current.Version {
		t.Fatalf("reaction applied version = %d, want %d", reaction.AppliedVersion, current.Version)
	}
}
