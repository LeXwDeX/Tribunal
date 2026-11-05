package app

import (
	"context"
	"strings"
	"testing"
)

func TestEmptyDiffPublishesIncomplete(t *testing.T) {
	f := newReviewFixture(t, strings.Repeat("a", 40))
	f.defaults.CommitStatus = true
	f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
	f.gitlab.diffs = nil
	job := f.seedReview(t, "empty-diff")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	var summary string
	for _, note := range f.gitlab.notes {
		if strings.Contains(note.Body, "gitlab-ce-review:summary") {
			summary = note.Body
		}
	}
	if !strings.Contains(summary, "评审未完成") {
		t.Fatalf("summary = %q, want INCOMPLETE", summary)
	}
	if len(f.gitlab.statuses) != 1 || f.gitlab.statuses[0].State != "failed" {
		t.Fatalf("statuses = %+v, want one failed status", f.gitlab.statuses)
	}
	if f.gitlab.statuses[0].Description == "" {
		t.Fatal("failed status has no description")
	}
}
