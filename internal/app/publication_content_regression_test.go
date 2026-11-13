package app

import (
	"context"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
)

func TestHistoricalDiscussionContentReuse(t *testing.T) {
	for _, tc := range []struct {
		name, body            string
		severity              review.Severity
		missingHash, fallback bool
	}{
		{name: "same body"},
		{name: "changed body", body: "a different current explanation", fallback: true},
		{name: "changed severity", severity: review.SeverityP1, fallback: true},
		{name: "missing hash", missingHash: true, fallback: true},
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
				if f.gitlab.discussionCreates != 1 || feedbackDiscussionNote(t, f, oldPublication).Body != oldBody {
					t.Fatalf("old discussion changed: creates=%d", f.gitlab.discussionCreates)
				}
				if len(f.gitlab.notes) != 1 {
					t.Fatalf("summary notes: %+v", f.gitlab.notes)
				}
				summary := f.gitlab.notes[0].Body
				if tc.fallback {
					if !strings.Contains(summary, "历史讨论内容") || !strings.Contains(summary, "bad assignment") {
						t.Fatalf("missing fallback: %s", summary)
					}
					if tc.body != "" && !strings.Contains(summary, tc.body) {
						t.Fatalf("missing current body: %s", summary)
					}
					if tc.severity != "" && !strings.Contains(summary, "[P1]") {
						t.Fatalf("missing current severity: %s", summary)
					}
				} else {
					if strings.Contains(summary, "历史讨论内容") {
						t.Fatalf("unchanged finding was treated as changed content: %s", summary)
					}
					if !strings.Contains(summary, "### 行内问题（1）") ||
						!strings.Contains(summary, "- **[P2]bad assignment** · `main.go`:2") {
						t.Fatalf("inline issue index is missing: %s", summary)
					}
				}
				stored, err := f.db.GetPublication(ctx, 42, 7, discussionPublishKind, oldPublication.PublishKey)
				if err != nil || stored.ID != oldPublication.ID || stored.WorkflowID != oldWorkflow.ID || stored.GitlabID != oldPublication.GitlabID || stored.BodyHash != oldPublication.BodyHash {
					t.Fatalf("old publication: %+v, %v", stored, err)
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
