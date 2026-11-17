package app

import (
	"context"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// TestFeedbackCountsAdoptedPositionThreadOnce covers a thread whose legacy
// fingerprint row and current position row both point at the same discussion.
// The thread carries the position key after adoption, so only that row may
// report awards and resolve signals.
func TestFeedbackCountsAdoptedPositionThreadOnce(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.runner.findings = []review.FindingOut{positionFinding("bad assignment", "the assignment is wrong")}
	workflow := runReviewGeneration(t, f, "feedback-dedupe-gen1")
	positionWorkflow, positionPublication := feedbackPublication(t, f)
	finding := feedbackFindings(t, f, positionWorkflow.ID, positionPublication.PublishKey)[0]
	publisherNote := feedbackDiscussionNote(t, f, positionPublication)

	legacyKey := review.PublishKeyFor("main.go", "legacy wording", "legacy evidence")
	if legacyKey == positionPublication.PublishKey {
		t.Fatal("legacy key collided with the position key")
	}
	legacyPublication, err := f.db.UpsertPublication(ctx, store.Publication{
		ProjectID: 42, MRIID: 7, Kind: discussionPublishKind, PublishKey: legacyKey,
		GitlabID: positionPublication.GitlabID, WorkflowID: workflow.ID, Fingerprint: "legacy",
	})
	if err != nil {
		t.Fatalf("UpsertPublication: %v", err)
	}

	f.gitlab.mu.Lock()
	for i := range f.gitlab.discussions {
		if f.gitlab.discussions[i].ID == positionPublication.GitlabID {
			f.gitlab.discussions[i].Notes[0].Resolved = true
		}
	}
	f.gitlab.awards[publisherNote.ID] = []gitlab.Award{{ID: 1, Name: "thumbsup", UserID: 5}}
	f.gitlab.mu.Unlock()

	service := newFeedbackService(t, f, f.git, f.manager)
	service.now = func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }
	service.RunOnce(ctx)

	if rows := feedbackRowsFor(t, f, legacyPublication.ID); len(rows) != 0 {
		t.Fatalf("superseded row reported feedback: %+v", rows)
	}
	rows := feedbackRowsFor(t, f, positionPublication.ID)
	if len(rows) != 2 {
		t.Fatalf("position row feedback = %+v, want one award and one resolve signal", rows)
	}
	seen := map[string]store.Feedback{}
	for _, row := range rows {
		seen[row.Signal] = row
		if row.FindingID != finding.ID {
			t.Fatalf("%s finding = %q, want the position finding %q", row.Signal, row.FindingID, finding.ID)
		}
	}
	if _, ok := seen["thumbsup"]; !ok {
		t.Fatalf("feedback rows = %+v, want thumbsup", rows)
	}
	if _, ok := seen["resolved_unchanged"]; !ok {
		t.Fatalf("feedback rows = %+v, want resolved_unchanged", rows)
	}
}
