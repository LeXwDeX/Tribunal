package app

import (
	"context"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// TestPublisherFinalBodySurvivesStatusWorker guards the #493 review finding:
// the publisher stores the full rendered summary, and the review.status worker
// must rebuild the same note from the stored row. A store-side clip would make
// the second write lose the tail without any platform-limit notice.
func TestPublisherFinalBodySurvivesStatusWorker(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	if _, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
	}); err != nil {
		t.Fatal(err)
	}
	control, err := f.db.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}

	const tailMarker = "尾部标记：完整正文结束"
	summaryText := strings.Repeat("完整正文段落。\n\n", 20_000) + tailMarker
	body, err := review.RenderSummary(review.SummaryInput{
		Verdict: review.VerdictBlocking, Summary: summaryText, Fingerprint: "fp493", HeadSHA: f.head,
	})
	if err != nil {
		t.Fatalf("RenderSummary: %v", err)
	}
	if runes := len([]rune(body)); runes <= 131_072 {
		t.Fatalf("rendered body runes = %d, want above the removed store cap", runes)
	}

	bound, err := bindOutbound(ctx, f.manager.Get(42))
	if err != nil {
		t.Fatalf("bindOutbound: %v", err)
	}
	if _, err := f.publisher.upsertSummary(bound, 42, 7, "token", "fp493", body, "", control.Generation, f.head, func() error { return nil }); err != nil {
		t.Fatalf("publisher upsertSummary: %v", err)
	}
	drainReviewJobs(t, f, "review.status")

	notes := f.gitlab.noteSnapshot()
	if len(notes) != 1 {
		t.Fatalf("notes = %d, want the single review note", len(notes))
	}
	got := notes[0].Body
	if !strings.HasPrefix(got, reviewStatusMarkerPrefix) {
		t.Fatalf("note does not start with the status marker: %q", got[:min(80, len(got))])
	}
	if !strings.Contains(got, tailMarker) {
		t.Fatalf("status worker lost the rendered tail: note has %d bytes", len(got))
	}
	if strings.Contains(got, "已达 GitLab 注记上限") {
		t.Fatal("status worker published a platform-clipped note")
	}
}
