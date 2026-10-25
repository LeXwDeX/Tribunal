package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func candidateDelivery(uuid string) store.Delivery {
	return store.Delivery{
		UUID:      uuid,
		ProjectID: 42,
		Event:     "Merge Request Hook",
		Outcome:   store.DeliveryEnqueued,
	}
}

func candidatePayload(t *testing.T, trigger string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"trigger": trigger,
		"profile": "default",
		"full":    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func claimCandidate(t *testing.T, s *Store, worker string) *store.Job {
	t.Helper()
	job, err := s.Claim(context.Background(), worker, []string{reviewCandidateKind}, time.Minute)
	if err != nil {
		t.Fatalf("Claim candidate: %v", err)
	}
	if job == nil {
		t.Fatal("no candidate job claimed")
	}
	return job
}

func activationFor(job *store.Job, head string, seq int64) store.ReviewCandidateActivation {
	return store.ReviewCandidateActivation{
		JobID:        job.ID,
		LeaseOwner:   job.LeaseOwner,
		Attempts:     job.Attempts,
		ProjectID:    42,
		MRIID:        7,
		HeadSHA:      head,
		CandidateSeq: seq,
	}
}

func TestRecordReviewCandidateDeliveryAllocatesAndDeduplicates(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	headA := reviewHead("a")
	headB := reviewHead("b")

	first, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("candidate-1"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: headA,
		Payload: candidatePayload(t, "mr_open"), MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("RecordReviewCandidateDelivery: %v", err)
	}
	if first.Duplicate || first.JobID == "" || first.Generation != 0 {
		t.Fatalf("first = %+v", first)
	}
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewControl: %v", err)
	}
	if control.Generation != 0 || control.CandidateSeq != 1 || control.BarrierSeq != 0 ||
		control.Cancelled || control.HeadSHA != "" {
		t.Fatalf("control after ingress = %+v", control)
	}
	job, err := s.GetJob(ctx, first.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Kind != reviewCandidateKind || job.Key != "review.candidate:42:7" || job.State != store.JobQueued {
		t.Fatalf("candidate job = %+v", job)
	}
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["candidate_seq"] != float64(1) || payload["head_sha"] != headA || payload["trigger"] != "mr_open" {
		t.Fatalf("candidate payload = %v", payload)
	}
	if _, ok := payload["generation"]; ok {
		t.Fatalf("candidate payload carries generation: %v", payload)
	}
	if _, ok := payload["activation_seq"]; ok {
		t.Fatalf("candidate payload carries activation_seq: %v", payload)
	}

	// A replayed delivery keeps the original job and sequence.
	replay, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("candidate-1"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: headB,
		Payload: candidatePayload(t, "mr_update"), MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Duplicate || replay.JobID != first.JobID {
		t.Fatalf("replay = %+v", replay)
	}
	control, err = s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if control.CandidateSeq != 1 || control.Generation != 0 {
		t.Fatalf("duplicate allocated a sequence: %+v", control)
	}

	// A newer candidate replaces the queued payload and allocates the next
	// sequence without touching the accepted generation.
	second, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("candidate-2"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: headB,
		Payload: candidatePayload(t, "mr_update"), MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.JobID != first.JobID || second.Duplicate {
		t.Fatalf("second = %+v", second)
	}
	control, err = s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if control.CandidateSeq != 2 || control.Generation != 0 || control.BarrierSeq != 0 {
		t.Fatalf("control after second = %+v", control)
	}
	job, err = s.GetJob(ctx, first.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["candidate_seq"] != float64(2) || payload["head_sha"] != headB || payload["trigger"] != "mr_update" {
		t.Fatalf("replaced payload = %v", payload)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE "key" = 'review.candidate:42:7'`); got != 1 {
		t.Fatalf("candidate jobs = %d, want 1", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM webhook_deliveries`); got != 2 {
		t.Fatalf("delivery rows = %d, want 2", got)
	}
}

func TestRecordReviewCandidateDeliveryRejectsReservedFields(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")

	tests := []struct {
		name      string
		candidate *store.ReviewCandidate
	}{
		{name: "nil candidate"},
		{name: "generation", candidate: &store.ReviewCandidate{
			ProjectID: 42, MRIID: 7, HeadSHA: head,
			Payload: json.RawMessage(`{"generation":1}`),
		}},
		{name: "candidate seq", candidate: &store.ReviewCandidate{
			ProjectID: 42, MRIID: 7, HeadSHA: head,
			Payload: json.RawMessage(`{"candidate_seq":1}`),
		}},
		{name: "activation seq", candidate: &store.ReviewCandidate{
			ProjectID: 42, MRIID: 7, HeadSHA: head,
			Payload: json.RawMessage(`{"activation_seq":1}`),
		}},
		{name: "invalid head", candidate: &store.ReviewCandidate{
			ProjectID: 42, MRIID: 7, HeadSHA: "not-a-sha",
			Payload: candidatePayload(t, "mr_open"),
		}},
		{name: "project mismatch", candidate: &store.ReviewCandidate{
			ProjectID: 43, MRIID: 7, HeadSHA: head,
			Payload: candidatePayload(t, "mr_open"),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("bad-"+tt.name), tt.candidate); err == nil {
				t.Fatal("expected error")
			}
			if got := rowCount(t, s, `SELECT COUNT(*) FROM webhook_deliveries`); got != 0 {
				t.Fatalf("delivery rows = %d, want rollback", got)
			}
			if got := rowCount(t, s, `SELECT COUNT(*) FROM review_controls`); got != 0 {
				t.Fatalf("control rows = %d, want rollback", got)
			}
			if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE kind = 'review.candidate'`); got != 0 {
				t.Fatalf("candidate jobs = %d, want rollback", got)
			}
		})
	}
}

func TestActivateReviewCandidateBumpsOnceAndEnqueuesRun(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")

	ingress, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("candidate-1"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
		Payload: candidatePayload(t, "mr_open"), MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed := claimCandidate(t, s, "worker-1")
	if claimed.ID != ingress.JobID {
		t.Fatalf("claimed %s, want %s", claimed.ID, ingress.JobID)
	}

	// A stale lease owner cannot activate.
	wrong := activationFor(claimed, head, 1)
	wrong.LeaseOwner = "other-worker"
	if _, _, err := s.ActivateReviewCandidate(ctx, wrong); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("wrong lease error = %v, want ErrLeaseLost", err)
	}
	if control, err := s.GetReviewControl(ctx, 42, 7); err != nil || control.Generation != 0 {
		t.Fatalf("wrong lease changed control: %+v, %v", control, err)
	}

	runJobID, generation, err := s.ActivateReviewCandidate(ctx, activationFor(claimed, head, 1))
	if err != nil {
		t.Fatalf("ActivateReviewCandidate: %v", err)
	}
	if generation != 1 || runJobID == "" {
		t.Fatalf("activation = %q generation=%d", runJobID, generation)
	}
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if control.Generation != 1 || control.CandidateSeq != 1 || control.BarrierSeq != 1 ||
		control.Cancelled || control.HeadSHA != head {
		t.Fatalf("control after activation = %+v", control)
	}
	runJob, err := s.GetJob(ctx, runJobID)
	if err != nil {
		t.Fatal(err)
	}
	if runJob.Kind != reviewRunKind || runJob.Key != "review:42:7" {
		t.Fatalf("review job = %+v", runJob)
	}
	var payload map[string]any
	if err := json.Unmarshal(runJob.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["generation"] != float64(1) || payload["activation_seq"] != float64(1) ||
		payload["head_sha"] != head || payload["trigger"] != "mr_open" {
		t.Fatalf("review payload = %v", payload)
	}
	if _, ok := payload["candidate_seq"]; ok {
		t.Fatalf("review payload kept candidate_seq: %v", payload)
	}

	// Re-activating the same candidate is idempotent: barrier_seq already
	// covers it, so it neither bumps the generation nor enqueues again.
	if _, _, err := s.ActivateReviewCandidate(ctx, activationFor(claimed, head, 1)); !errors.Is(err, store.ErrSuperseded) {
		t.Fatalf("second activation error = %v, want ErrSuperseded", err)
	}
	control, err = s.GetReviewControl(ctx, 42, 7)
	if err != nil || control.Generation != 1 {
		t.Fatalf("second activation changed generation: %+v, %v", control, err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE kind = 'review.run'`); got != 1 {
		t.Fatalf("review.run jobs = %d, want 1", got)
	}
}

func TestActivateReviewCandidateOrderingAndSupersession(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)
	headA := reviewHead("a")
	headB := reviewHead("b")

	first, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("c1"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: headA,
		Payload: candidatePayload(t, "mr_open"), MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	c1 := claimCandidate(t, s, "worker-1")
	if c1.ID != first.JobID {
		t.Fatalf("claimed %s, want %s", c1.ID, first.JobID)
	}
	// A running candidate leaves the fixed key free for a queued successor.
	if _, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("c2"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: headB,
		Payload: candidatePayload(t, "mr_update"), MaxAttempts: 3,
	}); err != nil {
		t.Fatal(err)
	}

	runID1, gen1, err := s.ActivateReviewCandidate(ctx, activationFor(c1, headA, 1))
	if err != nil || gen1 != 1 {
		t.Fatalf("activate c1 = %q generation=%d err=%v", runID1, gen1, err)
	}
	// The successor C2 is still queued, not claimable while C1 runs.
	if claimed, err := s.Claim(ctx, "worker-2", []string{reviewCandidateKind}, time.Minute); err != nil || claimed != nil {
		t.Fatalf("claimed running successor: %+v, %v", claimed, err)
	}
	if err := s.Complete(ctx, *c1); err != nil {
		t.Fatalf("complete c1: %v", err)
	}
	c2 := claimCandidate(t, s, "worker-2")
	runID2, gen2, err := s.ActivateReviewCandidate(ctx, activationFor(c2, headB, 2))
	if err != nil || gen2 != 2 {
		t.Fatalf("activate c2 = %q generation=%d err=%v", runID2, gen2, err)
	}
	if runID2 != runID1 {
		t.Fatalf("queued review.run was not replaced: %s != %s", runID2, runID1)
	}
	if err := s.Complete(ctx, *c2); err != nil {
		t.Fatalf("complete c2: %v", err)
	}
	runJob, err := s.GetJob(ctx, runID1)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(runJob.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["generation"] != float64(2) || payload["activation_seq"] != float64(2) ||
		payload["head_sha"] != headB {
		t.Fatalf("final review payload = %v", payload)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE kind = 'review.run'`); got != 1 {
		t.Fatalf("review.run jobs = %d, want 1", got)
	}
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil || control.Generation != 2 || control.BarrierSeq != 2 || control.HeadSHA != headB {
		t.Fatalf("control after c2 = %+v, %v", control, err)
	}

	// An immediate enqueue consumes every allocated candidate sequence.
	third, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("c3"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("c"),
		Payload: candidatePayload(t, "mr_open"), MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	c3 := claimCandidate(t, s, "worker-3")
	immediate, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("d"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if immediate.Generation != 3 {
		t.Fatalf("immediate generation = %d, want 3", immediate.Generation)
	}
	control, err = s.GetReviewControl(ctx, 42, 7)
	if err != nil || control.BarrierSeq != 3 || control.CandidateSeq != 3 {
		t.Fatalf("immediate barrier = %+v, %v", control, err)
	}
	if _, _, err := s.ActivateReviewCandidate(ctx, activationFor(c3, reviewHead("c"), 3)); !errors.Is(err, store.ErrSuperseded) {
		t.Fatalf("superseded activation error = %v, want ErrSuperseded", err)
	}
	control, err = s.GetReviewControl(ctx, 42, 7)
	if err != nil || control.Generation != 3 || control.HeadSHA != reviewHead("d") {
		t.Fatalf("superseded activation changed control: %+v, %v", control, err)
	}
	if third.JobID == "" {
		t.Fatal("c3 ingress did not return a job")
	}
}

func TestActivateReviewCandidateRejectsChangedPayloadAndExpiredLease(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)
	head := reviewHead("a")

	if _, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("c1"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
		Payload: candidatePayload(t, "mr_open"), MaxAttempts: 3,
	}); err != nil {
		t.Fatal(err)
	}
	claimed := claimCandidate(t, s, "worker-1")

	// A request for a different head or sequence never activates the job.
	mismatched := activationFor(claimed, reviewHead("b"), 1)
	if _, _, err := s.ActivateReviewCandidate(ctx, mismatched); err == nil {
		t.Fatal("mismatched head activated")
	}
	if control, err := s.GetReviewControl(ctx, 42, 7); err != nil || control.Generation != 0 {
		t.Fatalf("mismatched payload changed control: %+v, %v", control, err)
	}

	// Once the lease expires, the same attempt cannot activate.
	clock.Advance(2 * time.Minute)
	if _, _, err := s.ActivateReviewCandidate(ctx, activationFor(claimed, head, 1)); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("expired lease error = %v, want ErrLeaseLost", err)
	}
	if control, err := s.GetReviewControl(ctx, 42, 7); err != nil || control.Generation != 0 || control.BarrierSeq != 0 {
		t.Fatalf("expired lease changed control: %+v, %v", control, err)
	}
	if !strings.EqualFold(head, strings.ToLower(head)) {
		t.Fatal("test head must be lowercase")
	}
}

func TestRecordReviewCandidateDeliveryUnknownProjectRollsBack(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	if _, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("c1"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("a"),
		Payload: candidatePayload(t, "mr_open"),
	}); err == nil {
		t.Fatal("expected unknown project error")
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM webhook_deliveries`); got != 0 {
		t.Fatalf("delivery rows = %d, want rollback", got)
	}
}

func TestExpiredCandidateWithSuccessorRecoveryBlocksStaleActivation(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)
	headA := reviewHead("a")
	headB := reviewHead("b")

	if _, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("c1"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: headA,
		Payload: candidatePayload(t, "mr_open"), MaxAttempts: 3,
	}); err != nil {
		t.Fatal(err)
	}
	c1 := claimCandidate(t, s, "worker-1")
	if _, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("c2"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: headB,
		Payload: candidatePayload(t, "mr_update"), MaxAttempts: 3,
	}); err != nil {
		t.Fatal(err)
	}

	// The running attempt expires. Claim recovers it and cancels the stale
	// attempt because a queued successor exists.
	clock.Advance(2 * time.Minute)
	c2 := claimCandidate(t, s, "worker-2")
	stale, err := s.GetJob(ctx, c1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stale.State != store.JobCancelled {
		t.Fatalf("expired candidate state = %s, want cancelled", stale.State)
	}
	if _, _, err := s.ActivateReviewCandidate(ctx, activationFor(c1, headA, 1)); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("stale activation error = %v, want ErrLeaseLost", err)
	}
	if control, err := s.GetReviewControl(ctx, 42, 7); err != nil || control.Generation != 0 {
		t.Fatalf("stale activation changed control: %+v, %v", control, err)
	}
	_, generation, err := s.ActivateReviewCandidate(ctx, activationFor(c2, headB, 2))
	if err != nil || generation != 1 {
		t.Fatalf("successor activation generation = %d, err = %v", generation, err)
	}
}

func TestActivateReviewCandidateReplacesQueuedManualRun(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")

	manual, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
		Payload: json.RawMessage(`{"trigger":"manual","full":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	candidateHead := reviewHead("b")
	if _, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("c1"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: candidateHead,
		Payload: candidatePayload(t, "mr_open"), MaxAttempts: 3,
	}); err != nil {
		t.Fatal(err)
	}
	claimed := claimCandidate(t, s, "worker-1")
	runID, generation, err := s.ActivateReviewCandidate(ctx, activationFor(claimed, candidateHead, 1))
	if err != nil || generation != 2 {
		t.Fatalf("activation = %q generation=%d err=%v", runID, generation, err)
	}
	if runID != manual.JobID {
		t.Fatalf("queued manual job was not replaced: %s != %s", runID, manual.JobID)
	}
	job, err := s.GetJob(ctx, manual.JobID)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["generation"] != float64(2) || payload["activation_seq"] != float64(1) ||
		payload["head_sha"] != candidateHead || payload["trigger"] != "mr_open" {
		t.Fatalf("replaced manual payload = %v", payload)
	}
	if control, err := s.GetReviewControl(ctx, 42, 7); err != nil ||
		control.Generation != 2 || control.HeadSHA != candidateHead || control.Cancelled {
		t.Fatalf("control = %+v, %v", control, err)
	}
}
