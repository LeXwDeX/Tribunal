package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// GET /stats/reviews must distinguish "the per-call detail was deleted" from
// "there were no calls at all". Both views use the same data-driven rule as
// GET /stats (#466).
func TestReviewStatsModelDetailsPurgedIsDataDriven(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	wfID := "wf-review-stats-purged"
	mustCreateWorkflow(t, st, wfID, reviewMeta(42, 9, "head"), clock.Now())

	// No calls: the zero model view is real, not a retention artifact.
	stats, err := st.ReviewStats(ctx, store.ReviewStatsFilter{ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("ReviewStats without calls: %v", err)
	}
	if stats.ModelDetailsPurged {
		t.Fatal("a window with no calls claimed purged details")
	}

	// Details still exist: the exact per-call rows serve the window.
	recordAggregateCall(t, st, wfID, "a", 1, 10, 4, time.Second, nil)
	stats, err = st.ReviewStats(ctx, store.ReviewStatsFilter{ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("ReviewStats with details: %v", err)
	}
	if stats.ModelDetailsPurged {
		t.Fatal("details are present, but the response claimed they were purged")
	}

	// Retention deleted the detail rows and the aggregate still carries them.
	deleteLLMCallDetails(t, st)
	stats, err = st.ReviewStats(ctx, store.ReviewStatsFilter{ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("ReviewStats after retention: %v", err)
	}
	if !stats.ModelDetailsPurged {
		t.Fatal("purged details were not marked")
	}

	// A window that excludes the aggregate day has no calls to report.
	from := clock.Now().Add(48 * time.Hour)
	stats, err = st.ReviewStats(ctx, store.ReviewStatsFilter{
		From: from, To: from.Add(24 * time.Hour), ProjectID: ptrInt64(42),
	})
	if err != nil {
		t.Fatalf("ReviewStats outside the aggregate day: %v", err)
	}
	if stats.ModelDetailsPurged {
		t.Fatal("a window without calls claimed purged details")
	}
}

// Access filtering applies to the retention flag: another user's project does
// not turn the caller's window into a purged one.
func TestReviewStatsModelDetailsPurgedRespectsAccess(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	wfID := "wf-review-stats-access"
	mustCreateWorkflow(t, st, wfID, reviewMeta(42, 9, "head"), clock.Now())
	recordAggregateCall(t, st, wfID, "a", 1, 10, 4, time.Second, nil)
	deleteLLMCallDetails(t, st)

	stranger := int64(77)
	stats, err := st.ReviewStats(ctx, store.ReviewStatsFilter{AccessUserID: &stranger})
	if err != nil {
		t.Fatalf("ReviewStats for a stranger: %v", err)
	}
	if stats.ModelDetailsPurged {
		t.Fatal("an inaccessible project leaked into the purged flag")
	}
	stats, err = st.ReviewStats(ctx, store.ReviewStatsFilter{})
	if err != nil {
		t.Fatalf("ReviewStats trusted path: %v", err)
	}
	if !stats.ModelDetailsPurged {
		t.Fatal("trusted path did not see the purged window")
	}
}
