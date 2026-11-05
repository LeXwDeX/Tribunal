package app

import (
	"context"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
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
	if !strings.Contains(summary, "评审未完成") || !strings.Contains(summary, "没有可评审的变更") || !strings.Contains(summary, "empty diff") || strings.Contains(summary, "PASS") {
		t.Fatalf("summary = %q, want empty-diff INCOMPLETE", summary)
	}
	status, err := f.db.GetReviewStatus(context.Background(), 42, 7)
	if err != nil || status.State != reviewStatusIncomplete {
		t.Fatalf("review status = %+v, %v; want incomplete", status, err)
	}
	if len(f.gitlab.statuses) != 1 || f.gitlab.statuses[0].State != "failed" {
		t.Fatalf("statuses = %+v, want one failed status", f.gitlab.statuses)
	}
	if f.gitlab.statuses[0].Description == "" {
		t.Fatal("failed status has no description")
	}
}

func TestIgnoredOnlyDiffStillPasses(t *testing.T) {
	f := newReviewFixture(t, strings.Repeat("a", 40))
	f.defaults.Ignore = []string{"ignored.txt"}
	f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
	f.gitlab.diffs = []gitlab.FileDiff{{NewPath: "ignored.txt", Diff: "+ignored\n"}}
	job := f.seedReview(t, "ignored-only")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	var summary string
	for _, note := range f.gitlab.notes {
		if strings.Contains(note.Body, "gitlab-ce-review:summary") {
			summary = note.Body
		}
	}
	if !strings.Contains(summary, "PASS") || !strings.Contains(summary, "无需评审") {
		t.Fatalf("summary = %q, want ignored-only PASS", summary)
	}
	if len(f.gitlab.statuses) != 0 {
		t.Fatalf("statuses = %+v, want no commit status", f.gitlab.statuses)
	}
}
