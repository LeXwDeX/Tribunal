package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// #487 replaces publish-key collision fallbacks with position convergence:
// findings folded to one position share one discussion, and a rerun updates it
// instead of posting a second thread.
func TestInlinePositionMergesCollidingFindings(t *testing.T) {
	for _, tc := range []struct {
		name        string
		line        int
		category    string
		wantCreates int
	}{
		{"category", 2, "security", 1},
		{"line distance", 6, "correctness", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			f.gitlab.diffs = []gitlab.FileDiff{{OldPath: "main.go", NewPath: "main.go", Diff: "@@ -0,0 +1,6 @@\n+a\n+bad := 1\n+c\n+d\n+e\n+bad := 1\n"}}
			run := f.runner.Run
			f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
				result, err := run(ctx, spec)
				if err == nil {
					out := result.Output.(*review.CodeReviewOut)
					second := out.Findings[0]
					second.Line, second.Category = tc.line, tc.category
					out.Findings = append(out.Findings, second)
				}
				return result, err
			}
			job := f.seedReview(t, "collision-"+tc.name)
			if err := f.service.Handle(ctx, job); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			findings, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: wf.ID})
			if err != nil || len(findings) != 2 {
				t.Fatalf("findings = %+v, %v", findings, err)
			}
			if f.gitlab.discussionCreates != tc.wantCreates || len(f.gitlab.discussions) != tc.wantCreates || len(f.gitlab.notes) != 1 {
				t.Fatalf("discussions=%d creates=%d notes=%d, want %d", len(f.gitlab.discussions), f.gitlab.discussionCreates, len(f.gitlab.notes), tc.wantCreates)
			}
			if strings.Contains(f.gitlab.notes[0].Body, "标识冲突") {
				t.Fatalf("summary still reports a publish-key collision: %s", f.gitlab.notes[0].Body)
			}
			if err := f.publisher.Publish(ctx, wf.ID); err != nil || f.gitlab.discussionCreates != tc.wantCreates {
				t.Fatalf("retry: discussions=%d err=%v", f.gitlab.discussionCreates, err)
			}
		})
	}
}

func TestInlinePositionUpdatesAcrossCategoryChange(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	if err := f.service.Handle(ctx, f.seedReview(t, "collision-earlier")); err != nil {
		t.Fatal(err)
	}
	run := f.runner.Run
	f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		result, err := run(ctx, spec)
		if err == nil {
			result.Output.(*review.CodeReviewOut).Findings[0].Category = "security"
		}
		return result, err
	}
	if err := f.service.Handle(ctx, f.seedReview(t, "collision-later")); err != nil {
		t.Fatal(err)
	}
	if f.gitlab.discussionCreates != 1 || len(f.gitlab.discussions) != 1 || len(f.gitlab.notes) != 1 {
		t.Fatalf("discussions=%d creates=%d notes=%d", len(f.gitlab.discussions), f.gitlab.discussionCreates, len(f.gitlab.notes))
	}
	if !strings.Contains(f.gitlab.discussions[0].Notes[0].Body, "代次 2") {
		t.Fatalf("same position was not updated in place: %q", f.gitlab.discussions[0].Notes[0].Body)
	}
	if strings.Contains(f.gitlab.notes[0].Body, "标识冲突") {
		t.Fatalf("summary still reports a publish-key collision: %s", f.gitlab.notes[0].Body)
	}
}

// A rename source change moves the position, so the two old-side findings are
// separate threads. The older publication and its feedback attribution stay
// untouched (legacy rows are read-only history).
func TestInlinePositionKeepsRenameSourceDiscussionsSeparate(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	source := "a.go"
	f.gitlab.diffs = []gitlab.FileDiff{renameDiff(source, "dst.go", "@@ -1 +1 @@\n-old\n+new\n")}
	run := f.runner.Run
	f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		result, err := run(ctx, spec)
		if err == nil {
			finding := &result.Output.(*review.CodeReviewOut).Findings[0]
			finding.Path, finding.Side, finding.Line = source, "old", 1
		}
		return result, err
	}
	oldJob := f.seedReview(t, "rename-source-earlier")
	if err := f.service.Handle(ctx, oldJob); err != nil {
		t.Fatal(err)
	}
	oldWorkflow, oldPublication := feedbackPublication(t, f)
	oldFinding := feedbackFindings(t, f, oldWorkflow.ID, oldPublication.PublishKey)[0]
	note := feedbackDiscussionNote(t, f, oldPublication)
	f.gitlab.awards[note.ID] = []gitlab.Award{{ID: 1, Name: "thumbsup", UserID: 5}}
	service := newFeedbackService(t, f, f.git, f.manager)
	project, err := f.db.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	service.collectDiscussion(ctx, "token", project, oldPublication, *oldWorkflow, f.gitlab.mr, 7)
	before := feedbackRowsFor(t, f, oldPublication.ID)
	if len(before) != 1 || before[0].FindingID != oldFinding.ID {
		t.Fatalf("initial feedback = %+v", before)
	}

	source = "b.go"
	f.gitlab.diffs = []gitlab.FileDiff{renameDiff(source, "dst.go", "@@ -1 +1 @@\n-old\n+new\n")}
	newJob := f.seedReview(t, "rename-source-later")
	if err := f.service.Handle(ctx, newJob); err != nil {
		t.Fatal(err)
	}
	newWorkflow, err := f.db.GetWorkflowByJob(ctx, newJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: newWorkflow.ID})
	if err != nil || len(findings) != 1 || findings[0].Path != source || findings[0].PublishKey == oldPublication.PublishKey {
		t.Fatalf("new findings = %+v, err = %v", findings, err)
	}
	if f.gitlab.discussionCreates != 2 || len(f.gitlab.discussions) != 2 || len(f.gitlab.notes) != 1 {
		t.Fatalf("discussions=%d creates=%d notes=%d", len(f.gitlab.discussions), f.gitlab.discussionCreates, len(f.gitlab.notes))
	}
	if strings.Contains(f.gitlab.notes[0].Body, "标识冲突") {
		t.Fatalf("summary still reports a publish-key collision: %s", f.gitlab.notes[0].Body)
	}
	if err := f.publisher.Publish(ctx, newWorkflow.ID); err != nil || f.gitlab.discussionCreates != 2 {
		t.Fatalf("retry discussions=%d err=%v", f.gitlab.discussionCreates, err)
	}
	stored, err := f.db.GetPublication(ctx, 42, 7, discussionPublishKind, oldPublication.PublishKey)
	if err != nil || stored.ID != oldPublication.ID || stored.WorkflowID != oldWorkflow.ID || stored.GitlabID != oldPublication.GitlabID {
		t.Fatalf("old publication = %+v err=%v", stored, err)
	}
	oldFindings := feedbackFindings(t, f, oldWorkflow.ID, oldPublication.PublishKey)
	if len(oldFindings) != 1 || oldFindings[0].ID != oldFinding.ID || oldFindings[0].Path != "a.go" {
		t.Fatalf("old findings = %+v", oldFindings)
	}
	service.collectDiscussion(ctx, "token", project, oldPublication, *oldWorkflow, f.gitlab.mr, 7)
	after := feedbackRowsFor(t, f, oldPublication.ID)
	if len(after) != 1 || after[0].ID != before[0].ID || after[0].FindingID != oldFinding.ID {
		t.Fatalf("old feedback = %+v", after)
	}
}

// The position record, not the historical finding fields, owns the thread now.
// Mutating the old path/side must not create a duplicate or leave the summary
// for the current run.
func TestInlinePositionIgnoresHistoricalFindingProvenance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*store.Finding)
	}{
		{"missing path", func(f *store.Finding) { f.Path = "" }},
		{"unknown side", func(f *store.Finding) { f.Side = "unknown" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			if err := f.service.Handle(ctx, f.seedReview(t, "source-unavailable-earlier")); err != nil {
				t.Fatal(err)
			}
			wf, publication := feedbackPublication(t, f)
			findings := feedbackFindings(t, f, wf.ID, publication.PublishKey)
			tc.mutate(&findings[0])
			if err := f.db.ReplaceFindings(ctx, wf.ID, findings); err != nil {
				t.Fatal(err)
			}
			if err := f.service.Handle(ctx, f.seedReview(t, "source-unavailable-later")); err != nil {
				t.Fatal(err)
			}
			if f.gitlab.discussionCreates != 1 || len(f.gitlab.discussions) != 1 || len(f.gitlab.notes) != 1 {
				t.Fatalf("discussions=%d creates=%d notes=%d", len(f.gitlab.discussions), f.gitlab.discussionCreates, len(f.gitlab.notes))
			}
			if !strings.Contains(f.gitlab.discussions[0].Notes[0].Body, "代次 2") {
				t.Fatalf("position discussion was not updated: %q", f.gitlab.discussions[0].Notes[0].Body)
			}
			if strings.Contains(f.gitlab.notes[0].Body, "标识冲突") || strings.Contains(f.gitlab.notes[0].Body, "历史讨论内容") {
				t.Fatalf("summary reports a stale fallback: %s", f.gitlab.notes[0].Body)
			}
		})
	}
}

func TestPublicationSameSideAndPathReusesEarlierDiscussion(t *testing.T) {
	for _, side := range []string{"old", "new"} {
		t.Run(side, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			f.gitlab.diffs = []gitlab.FileDiff{renameDiff("a.go", "dst.go", "@@ -1 +1 @@\n-old\n+new\n")}
			run := f.runner.Run
			f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
				result, err := run(ctx, spec)
				if err == nil {
					finding := &result.Output.(*review.CodeReviewOut).Findings[0]
					finding.Path, finding.Side, finding.Line = "a.go", side, 1
					if side == "new" {
						finding.Path = "dst.go"
					}
				}
				return result, err
			}
			if err := f.service.Handle(ctx, f.seedReview(t, "same-source-earlier")); err != nil {
				t.Fatal(err)
			}
			wf, publication := feedbackPublication(t, f)
			if side == "new" {
				findings := feedbackFindings(t, f, wf.ID, publication.PublishKey)
				findings[0].Side = ""
				if err := f.db.ReplaceFindings(ctx, wf.ID, findings); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.Handle(ctx, f.seedReview(t, "same-source-later")); err != nil {
				t.Fatal(err)
			}
			if f.gitlab.discussionCreates != 1 || len(f.gitlab.discussions) != 1 || len(f.gitlab.notes) != 1 ||
				strings.Contains(f.gitlab.notes[0].Body, "标识冲突") {
				t.Fatalf("discussions=%d creates=%d notes=%+v", len(f.gitlab.discussions), f.gitlab.discussionCreates, f.gitlab.notes)
			}
		})
	}
}

type failingFindingLookup struct{ ReviewStore }

func (s failingFindingLookup) ListFindings(context.Context, store.FindingFilter) ([]store.Finding, error) {
	return nil, errors.New("temporary database error")
}

func TestFeedbackMissingFindingDoesNotUnlinkPriorAttribution(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "feedback-missing-on-refresh")
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	wf, publication := feedbackPublication(t, f)
	finding := feedbackFindings(t, f, wf.ID, publication.PublishKey)[0]
	note := feedbackDiscussionNote(t, f, publication)
	f.gitlab.awards[note.ID] = []gitlab.Award{{ID: 1, Name: "thumbsup", UserID: 5}}
	service := newFeedbackService(t, f, f.git, f.manager)
	project, err := f.db.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	service.collectDiscussion(ctx, "token", project, publication, *wf, f.gitlab.mr, 7)
	finding.PublishKey = "changed-key"
	if err := f.db.ReplaceFindings(ctx, wf.ID, []store.Finding{finding}); err != nil {
		t.Fatal(err)
	}
	service.collectDiscussion(ctx, "token", project, publication, *wf, f.gitlab.mr, 7)
	if rows := feedbackRowsFor(t, f, publication.ID); len(rows) != 1 || rows[0].FindingID != finding.ID {
		t.Fatalf("refreshed feedback = %+v", rows)
	}
}

func TestFeedbackLookupFailureDoesNotUnlinkPriorAttribution(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "feedback-lookup-failure")
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	wf, publication := feedbackPublication(t, f)
	finding := feedbackFindings(t, f, wf.ID, publication.PublishKey)[0]
	note := feedbackDiscussionNote(t, f, publication)
	f.gitlab.awards[note.ID] = []gitlab.Award{{ID: 1, Name: "thumbsup", UserID: 5}}
	service := newFeedbackService(t, f, f.git, f.manager)
	project, err := f.db.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	service.collectDiscussion(ctx, "token", project, publication, *wf, f.gitlab.mr, 7)
	if rows := feedbackRowsFor(t, f, publication.ID); len(rows) != 1 || rows[0].FindingID != finding.ID {
		t.Fatalf("initial feedback = %+v", rows)
	}
	service.store = failingFindingLookup{f.db}
	service.collectDiscussion(ctx, "token", project, publication, *wf, f.gitlab.mr, 7)
	if rows := feedbackRowsFor(t, f, publication.ID); len(rows) != 1 || rows[0].FindingID != finding.ID {
		t.Fatalf("refreshed feedback = %+v", rows)
	}
}

func TestOldPromptIdentityPreventsRepublishBeforeFindingReplacement(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	if err := f.service.Handle(ctx, f.seedReview(t, "old-prompt-publication")); err != nil {
		t.Fatal(err)
	}
	wf, publication := feedbackPublication(t, f)
	finding := feedbackFindings(t, f, wf.ID, publication.PublishKey)[0]
	f.publisher.promptsHash = "new-prompt-identity"
	if err := f.publisher.Publish(ctx, wf.ID); !errors.Is(err, ErrStalePublication) {
		t.Fatalf("old prompt publish error = %v", err)
	}
	findings := feedbackFindings(t, f, wf.ID, publication.PublishKey)
	if len(findings) != 1 || findings[0].ID != finding.ID {
		t.Fatalf("old findings changed after rejected publish: %+v", findings)
	}
}

func TestFeedbackDoesNotGuessBetweenCollidingFindings(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "feedback-collision")
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	wf, publication := feedbackPublication(t, f)
	findings := feedbackFindings(t, f, wf.ID, publication.PublishKey)
	second := findings[0]
	second.ID = "other-colliding-finding"
	findings = append(findings, second)
	if err := f.db.ReplaceFindings(ctx, wf.ID, findings); err != nil {
		t.Fatal(err)
	}
	note := feedbackDiscussionNote(t, f, publication)
	f.gitlab.awards[note.ID] = []gitlab.Award{{ID: 1, Name: "thumbsup", UserID: 5}}
	service := newFeedbackService(t, f, f.git, f.manager)
	project, err := f.db.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	service.collectDiscussion(ctx, "token", project, publication, *wf, f.gitlab.mr, 7)
	if rows := feedbackRowsFor(t, f, publication.ID); len(rows) != 0 {
		t.Fatalf("ambiguous feedback = %+v", rows)
	}
}
