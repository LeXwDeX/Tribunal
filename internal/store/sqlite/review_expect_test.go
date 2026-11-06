package sqlite

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// A producer that decided from a snapshot must not overwrite a review that a
// live webhook enqueued after the snapshot (#362).
func TestRecordReviewDeliveryExpectSkipsWhenControlChanged(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	delivery := func(uuid string) store.Delivery {
		return store.Delivery{UUID: uuid, ProjectID: 42, Event: "Catch Up", Outcome: store.DeliveryEnqueued}
	}
	payload := json.RawMessage(`{"trigger":"catch_up","profile":"default"}`)

	// Observed "no control", then a webhook for head b lands.
	if _, err := s.RecordReviewDelivery(ctx, store.Delivery{UUID: "hook", ProjectID: 42, Event: "Merge Request Hook", Outcome: store.DeliveryEnqueued},
		&store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("b"), Payload: payload}); err != nil {
		t.Fatal(err)
	}
	res, err := s.RecordReviewDelivery(ctx, delivery("cu-1"), &store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("a"), Payload: payload,
		Expect: &store.ReviewControlExpect{Exists: false},
	})
	if err != nil || !res.Skipped || res.JobID != "" {
		t.Fatalf("result = %+v, err = %v; want skipped", res, err)
	}
	control, _ := s.GetReviewControl(ctx, 42, 7)
	if control.HeadSHA != reviewHead("b") || control.Generation != 1 {
		t.Fatalf("control = %+v; the live webhook's head must stay current", control)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM webhook_deliveries WHERE uuid = 'cu-1'`); got != 0 {
		t.Fatalf("skipped enqueue consumed its delivery row (%d)", got)
	}

	// Observed generation 1: still matches, so the enqueue proceeds.
	res, err = s.RecordReviewDelivery(ctx, delivery("cu-2"), &store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("a"), Payload: payload,
		Expect: &store.ReviewControlExpect{Exists: true, Generation: 1},
	})
	if err != nil || res.Skipped || res.Generation != 2 {
		t.Fatalf("result = %+v, err = %v; want generation 2", res, err)
	}
	// The stale observation (generation 1) no longer matches.
	res, err = s.RecordReviewDelivery(ctx, delivery("cu-3"), &store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("c"), Payload: payload,
		Expect: &store.ReviewControlExpect{Exists: true, Generation: 1},
	})
	if err != nil || !res.Skipped {
		t.Fatalf("result = %+v, err = %v; want skipped", res, err)
	}
}

func TestRecordReviewCandidateDeliveryExpectSkipsWhenCandidateArrived(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	if _, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("hook"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("b"), Payload: candidatePayload(t, "mr_update"),
	}); err != nil {
		t.Fatal(err)
	}
	res, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("cu"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("a"), Payload: candidatePayload(t, "catch_up"),
		Expect: &store.ReviewControlExpect{Exists: false},
	})
	if err != nil || !res.Skipped {
		t.Fatalf("result = %+v, err = %v; want skipped", res, err)
	}
	control, _ := s.GetReviewControl(ctx, 42, 7)
	if control.CandidateSeq != 1 {
		t.Fatalf("control = %+v; the skipped candidate must not allocate a sequence", control)
	}
}
