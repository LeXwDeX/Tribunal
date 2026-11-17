package app

import (
	"context"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
)

// TestHistoricalDiscussionContentUpdatesInPlace covers the #487 contract: a
// finding at the same diff position updates the existing discussion instead of
// stacking a new thread, whatever wording, severity, or stored body hash the
// previous generation had.
func TestHistoricalDiscussionContentUpdatesInPlace(t *testing.T) {
	for _, tc := range []struct {
		name, body   string
		severity     review.Severity
		missingHash  bool
		indexedTitle string
	}{
		{name: "same body", indexedTitle: "- **[P2]bad assignment** · `main.go`:2"},
		{name: "changed body", body: "a different current explanation", indexedTitle: "- **[P2]bad assignment** · `main.go`:2"},
		{name: "changed severity", severity: review.SeverityP1, indexedTitle: "- **[P1]bad assignment** · `main.go`:2"},
		{name: "missing hash", missingHash: true, indexedTitle: "- **[P2]bad assignment** · `main.go`:2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			profile := f.profiles["default"]
			profile.Challenge.Only = []string{"P0"}
			f.profiles["default"] = profile
			if err := f.service.Handle(ctx, f.seedReview(t, "content-old")); err != nil {
				t.Fatal(err)
			}
			oldWorkflow, oldPublication := feedbackPublication(t, f)
			oldFinding := feedbackFindings(t, f, oldWorkflow.ID, oldPublication.PublishKey)[0]
			note := feedbackDiscussionNote(t, f, oldPublication)
			oldBody := note.Body
			f.gitlab.awards[note.ID] = []gitlab.Award{{ID: 1, Name: "thumbsup", UserID: 5}}
			service := newFeedbackService(t, f, f.git, f.manager)
			project, err := f.db.GetProject(ctx, 42)
			if err != nil {
				t.Fatal(err)
			}
			service.collectDiscussion(ctx, "token", project, oldPublication, *oldWorkflow, f.gitlab.mr, 7)
			before := feedbackRowsFor(t, f, oldPublication.ID)
			if len(before) != 1 || before[0].FindingID != oldFinding.ID {
				t.Fatalf("feedback before: %+v", before)
			}
			if tc.missingHash {
				oldPublication.BodyHash = ""
				if _, err := f.db.UpsertPublication(ctx, oldPublication); err != nil {
					t.Fatal(err)
				}
			}
			run := f.runner.Run
			f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
				result, err := run(ctx, spec)
				if err == nil {
					finding := &result.Output.(*review.CodeReviewOut).Findings[0]
					if tc.body != "" {
						finding.Body = tc.body
					}
					if tc.severity != "" {
						finding.Severity = tc.severity
					}
				}
				return result, err
			}
			job := f.seedReview(t, "content-new")
			if err := f.service.Handle(ctx, job); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			check := func() {
				t.Helper()
				if f.gitlab.discussionCreates != 1 || len(f.gitlab.discussions) != 1 {
					t.Fatalf("same position was stacked: creates=%d discussions=%d", f.gitlab.discussionCreates, len(f.gitlab.discussions))
				}
				updated := feedbackDiscussionNote(t, f, oldPublication).Body
				if updated == oldBody || !strings.Contains(updated, "代次 2") {
					t.Fatalf("discussion was not updated in place for generation 2: %q", updated)
				}
				if tc.body != "" && !strings.Contains(updated, tc.body) {
					t.Fatalf("updated discussion lacks the current body: %q", updated)
				}
				if tc.severity != "" && !strings.Contains(updated, "[P1]") {
					t.Fatalf("updated discussion lacks the current severity: %q", updated)
				}
				if len(f.gitlab.notes) != 1 {
					t.Fatalf("summary notes: %+v", f.gitlab.notes)
				}
				summary := f.gitlab.notes[0].Body
				if strings.Contains(summary, "历史讨论内容") {
					t.Fatalf("in-place update was treated as unproven content: %s", summary)
				}
				if !strings.Contains(summary, "### 行内问题（1）") || !strings.Contains(summary, tc.indexedTitle) {
					t.Fatalf("inline issue index is missing: %s", summary)
				}
				stored, err := f.db.GetPublication(ctx, 42, 7, discussionPublishKind, oldPublication.PublishKey)
				if err != nil || stored.ID != oldPublication.ID || stored.GitlabID != oldPublication.GitlabID || stored.WorkflowID != wf.ID {
					t.Fatalf("position publication: %+v, %v", stored, err)
				}
				old := feedbackFindings(t, f, oldWorkflow.ID, oldPublication.PublishKey)
				if len(old) != 1 || old[0].ID != oldFinding.ID || old[0].Body != oldFinding.Body {
					t.Fatalf("old finding: %+v", old)
				}
				after := feedbackRowsFor(t, f, oldPublication.ID)
				if len(after) != 1 || after[0].ID != before[0].ID || after[0].FindingID != oldFinding.ID {
					t.Fatalf("feedback after: %+v", after)
				}
			}
			check()
			if err := f.publisher.Publish(ctx, wf.ID); err != nil {
				t.Fatal(err)
			}
			check()
		})
	}
}
