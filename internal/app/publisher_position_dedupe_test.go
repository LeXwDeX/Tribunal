package app

import (
	"context"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func positionFinding(title, body string) review.FindingOut {
	return review.FindingOut{
		Path:       "main.go",
		Line:       2,
		Side:       "new",
		Severity:   review.SeverityP2,
		Category:   "correctness",
		Title:      title,
		Body:       body,
		Evidence:   "bad := 1",
		Confidence: review.ConfidenceHigh,
	}
}

func runReviewGeneration(t *testing.T, f *reviewFixture, jobID string) *store.Workflow {
	t.Helper()
	job := f.seedReview(t, jobID)
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle %s: %v", jobID, err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob %s: %v", jobID, err)
	}
	return workflow
}

func onlyDiscussion(t *testing.T, f *reviewFixture) gitlab.Discussion {
	t.Helper()
	f.gitlab.mu.Lock()
	defer f.gitlab.mu.Unlock()
	if len(f.gitlab.discussions) != 1 {
		t.Fatalf("discussions = %d, want 1: %+v", len(f.gitlab.discussions), f.gitlab.discussions)
	}
	return f.gitlab.discussions[0]
}

func TestInlinePositionFindingUpdatesAcrossGenerations(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.runner.findings = []review.FindingOut{positionFinding("bad assignment", "the assignment is wrong")}
	first := runReviewGeneration(t, f, "position-gen1")
	if f.gitlab.discussionCreates != 1 {
		t.Fatalf("generation 1 discussion creates = %d, want 1", f.gitlab.discussionCreates)
	}
	firstBody := onlyDiscussion(t, f).Notes[0].Body

	f.runner.findings = []review.FindingOut{positionFinding("unsafe assignment", "the value is never validated")}
	second := runReviewGeneration(t, f, "position-gen2")
	if second.ID == first.ID {
		t.Fatal("second generation did not advance the workflow")
	}
	if f.gitlab.discussionCreates != 1 {
		t.Fatalf("generation 2 created a stacked discussion: creates = %d", f.gitlab.discussionCreates)
	}
	discussion := onlyDiscussion(t, f)
	body := discussion.Notes[0].Body
	if body == firstBody {
		t.Fatal("discussion body was not updated for the new generation")
	}
	if !strings.Contains(body, "unsafe assignment") || !strings.Contains(body, "never validated") {
		t.Fatalf("discussion body does not carry the latest wording: %q", body)
	}
	if strings.Contains(body, "bad assignment") || strings.Contains(body, "assignment is wrong") {
		t.Fatalf("discussion body still carries the previous wording: %q", body)
	}
	if !strings.Contains(body, "代次 2") {
		t.Fatalf("discussion body lacks the generation marker: %q", body)
	}

	f.runner.findings = []review.FindingOut{positionFinding("unchecked assignment", "the assignment result is discarded")}
	third := runReviewGeneration(t, f, "position-gen3")
	if third.ID == second.ID || f.gitlab.discussionCreates != 1 {
		t.Fatalf("generation 3 stacked a discussion: creates = %d", f.gitlab.discussionCreates)
	}
	body = onlyDiscussion(t, f).Notes[0].Body
	if !strings.Contains(body, "unchecked assignment") || !strings.Contains(body, "代次 3") {
		t.Fatalf("generation 3 did not become the thread body: %q", body)
	}
}

func TestInlinePositionUpdateKeepsUserRepliesAndUnresolved(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.runner.findings = []review.FindingOut{positionFinding("bad assignment", "the assignment is wrong")}
	runReviewGeneration(t, f, "position-reply-gen1")
	f.gitlab.mu.Lock()
	f.gitlab.discussions[0].Notes = append(f.gitlab.discussions[0].Notes,
		gitlab.Note{ID: 9001, AuthorID: 12, Body: "please fix this", Resolvable: true})
	f.gitlab.mu.Unlock()

	f.runner.findings = []review.FindingOut{positionFinding("unsafe assignment", "the value is never validated")}
	runReviewGeneration(t, f, "position-reply-gen2")
	discussion := onlyDiscussion(t, f)
	if len(discussion.Notes) != 2 {
		t.Fatalf("discussion notes = %d, want the bot note and the user reply: %+v", len(discussion.Notes), discussion.Notes)
	}
	if discussion.Notes[1].ID != 9001 || discussion.Notes[1].Body != "please fix this" {
		t.Fatalf("user reply was lost or rewritten: %+v", discussion.Notes[1])
	}
	if discussion.Notes[0].Resolved || discussion.Notes[1].Resolved {
		t.Fatalf("discussion was resolved while the finding is still present: %+v", discussion.Notes)
	}
	for _, resolved := range f.gitlab.discussionResolves {
		if resolved {
			t.Fatalf("publisher resolved the position discussion")
		}
	}
}

func TestInlinePositionFindingsAtDifferentLinesStaySeparate(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.diffs = []gitlab.FileDiff{{NewPath: "main.go", Diff: "@@ -0,0 +1,4 @@\n+one\n+two\n+three\n+four\n"}}
	first := positionFinding("first problem", "the first line is wrong")
	first.Line = 2
	second := positionFinding("second problem", "the fourth line is also wrong")
	second.Line = 4
	f.runner.findings = []review.FindingOut{first, second}
	runReviewGeneration(t, f, "position-distinct-gen1")
	if f.gitlab.discussionCreates != 2 {
		t.Fatalf("generation 1 discussion creates = %d, want 2", f.gitlab.discussionCreates)
	}

	first.Body = "the first line is definitely wrong"
	second.Body = "the fourth line is definitely wrong"
	f.runner.findings = []review.FindingOut{first, second}
	runReviewGeneration(t, f, "position-distinct-gen2")
	if f.gitlab.discussionCreates != 2 {
		t.Fatalf("distinct positions were merged: creates = %d", f.gitlab.discussionCreates)
	}
	f.gitlab.mu.Lock()
	defer f.gitlab.mu.Unlock()
	if len(f.gitlab.discussions) != 2 {
		t.Fatalf("discussions = %d, want 2", len(f.gitlab.discussions))
	}
	titles := 0
	for _, discussion := range f.gitlab.discussions {
		body := discussion.Notes[0].Body
		if strings.Contains(body, "first problem") || strings.Contains(body, "second problem") {
			titles++
		}
	}
	if titles != 2 {
		t.Fatalf("distinct positions did not keep their own discussion: %+v", f.gitlab.discussions)
	}
}

func TestInlinePositionSharesOneDiscussionForDifferentProblems(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	first := positionFinding("first problem", "the assignment is wrong")
	second := positionFinding("second problem", "the assignment hides an error")
	f.runner.findings = []review.FindingOut{first, second}
	runReviewGeneration(t, f, "position-shared-gen1")
	if f.gitlab.discussionCreates != 1 {
		t.Fatalf("same position created %d discussions, want 1", f.gitlab.discussionCreates)
	}
	discussion := onlyDiscussion(t, f)
	body := discussion.Notes[0].Body
	if !strings.Contains(body, "### ") || !strings.Contains(body, "first problem") || !strings.Contains(body, "second problem") {
		t.Fatalf("same-position problems were not rendered as stable sections: %q", body)
	}
}

func TestLegacyFingerprintDiscussionIsAdoptedByPosition(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	legacyKey := review.PublishKeyFor("main.go", "old wording", "old evidence")
	legacyBody := review.FindingMarkerPrefix + " key=" + legacyKey + " -->\n**old wording**\n"
	f.gitlab.discussions = []gitlab.Discussion{
		{ID: "legacy-older", Notes: []gitlab.Note{{
			ID: 500, AuthorID: 9, Body: legacyBody,
			Position: &gitlab.Position{NewPath: "main.go", NewLine: 2, HeadSHA: reviewTestHead},
		}}},
		{ID: "legacy-newer", Notes: []gitlab.Note{{
			ID: 700, AuthorID: 9, Body: legacyBody,
			Position: &gitlab.Position{NewPath: "main.go", NewLine: 2, HeadSHA: reviewTestHead},
		}}},
	}
	f.runner.findings = []review.FindingOut{positionFinding("bad assignment", "the assignment is wrong")}
	runReviewGeneration(t, f, "position-legacy-gen1")
	if f.gitlab.discussionCreates != 0 {
		t.Fatalf("legacy position discussion was duplicated: creates = %d", f.gitlab.discussionCreates)
	}
	if f.gitlab.discussionUpdates != 1 {
		t.Fatalf("discussion updates = %d, want 1", f.gitlab.discussionUpdates)
	}
	f.gitlab.mu.Lock()
	defer f.gitlab.mu.Unlock()
	if len(f.gitlab.discussions) != 2 {
		t.Fatalf("legacy discussions were removed: %d", len(f.gitlab.discussions))
	}
	for _, discussion := range f.gitlab.discussions {
		body := discussion.Notes[0].Body
		switch discussion.ID {
		case "legacy-newer":
			if !strings.Contains(body, "bad assignment") || !strings.Contains(body, "代次 1") {
				t.Fatalf("most recent legacy discussion was not updated: %q", body)
			}
		case "legacy-older":
			if body != legacyBody {
				t.Fatalf("older legacy discussion was modified: %q", body)
			}
		}
	}
}

func TestInlineFindingWithoutPositionStaysInSummary(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	finding := positionFinding("unlocatable problem", "the position is unknown")
	finding.Line = 0
	f.runner.findings = []review.FindingOut{finding}
	runReviewGeneration(t, f, "position-missing")
	if f.gitlab.discussionCreates != 0 {
		t.Fatalf("finding without a position created %d discussions", f.gitlab.discussionCreates)
	}
	if len(f.gitlab.notes) != 1 || !strings.Contains(f.gitlab.notes[0].Body, "unlocatable problem") {
		t.Fatalf("finding without a position is missing from the summary: %+v", f.gitlab.notes)
	}
}
