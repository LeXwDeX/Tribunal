package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestRecoveredDiscussionIntentDoesNotClaimNewWorkflowContent(t *testing.T) {
	for _, mode := range []string{"response lost", "publication write failed"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			profile := f.profiles["default"]
			profile.Challenge.Only = []string{"P0"}
			f.profiles["default"] = profile
			old := reviewWithFailedNote(t, f, "intent-content-old")
			if mode == "response lost" {
				f.publisher.gitlab = &lostDiscussionResponse{f.gitlab}
			} else {
				f.publisher.store = failedDiscussionPublication{f.db}
			}
			if err := f.publisher.Publish(ctx, old.ID); err == nil {
				t.Fatal("expected failed first publication")
			}
			oldFindings, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: old.ID})
			if err != nil || len(oldFindings) != 1 || len(f.gitlab.discussions) != 1 {
				t.Fatalf("old findings/discussion: %+v / %+v, %v", oldFindings, f.gitlab.discussions, err)
			}
			key := oldFindings[0].PublishKey
			oldBody := f.gitlab.discussions[0].Notes[0].Body
			restartFixturePublisher(t, f, f.gitlab)
			run := f.runner.Run
			f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
				result, err := run(ctx, spec)
				if err == nil {
					finding := &result.Output.(*review.CodeReviewOut).Findings[0]
					finding.Body = "new explanation"
					finding.Severity = review.SeverityP1
					finding.Suggestion = "new suggested fix"
				}
				return result, err
			}
			job := f.seedReview(t, "intent-content-new")
			if err := f.service.RunReview(ctx, job); err != nil {
				t.Fatalf("new review: %v", err)
			}
			wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if attempt > 0 {
					if err := f.publisher.Publish(ctx, wf.ID); err != nil {
						t.Fatal(err)
					}
				}
				if f.gitlab.discussionCreates != 1 || f.gitlab.discussions[0].Notes[0].Body != oldBody {
					t.Fatalf("old discussion changed or duplicated: %+v", f.gitlab.discussions)
				}
				if len(f.gitlab.notes) != 1 {
					t.Fatalf("summaries: %+v", f.gitlab.notes)
				}
				summary := f.gitlab.notes[0].Body
				for _, want := range []string{"历史讨论内容", "new explanation", "[P1]", "new suggested fix"} {
					if !strings.Contains(summary, want) {
						t.Errorf("summary missing %q: %s", want, summary)
					}
				}
				findings, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: wf.ID})
				if err != nil || len(findings) != 1 || findings[0].PublishKey != key || findings[0].PositionOK == nil || *findings[0].PositionOK {
					t.Fatalf("current findings: %+v, %v", findings, err)
				}
				if pub, err := f.db.GetPublication(ctx, 42, 7, discussionPublishKind, key); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("unverified publication attributed: %+v, %v", pub, err)
				}
				prior, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: old.ID})
				if err != nil || len(prior) != 1 || prior[0].ID != oldFindings[0].ID || prior[0].Body != oldFindings[0].Body {
					t.Fatalf("old findings changed: %+v, %v", prior, err)
				}
				if wf = workflowPublication(t, f, wf.ID); wf.PublicationState != store.WorkflowPublicationPublished {
					t.Fatalf("publication state: %+v", wf)
				}
			}
		})
	}
}

func TestRecoveredDiscussionIntentSameWorkflowIsUnproven(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "intent-same-workflow")
	f.publisher.gitlab = &lostDiscussionResponse{f.gitlab}
	if err := f.publisher.Publish(ctx, wf.ID); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("first publication: %v", err)
	}
	restartFixturePublisher(t, f, f.gitlab)
	for attempt := 0; attempt < 2; attempt++ {
		if err := f.publisher.Publish(ctx, wf.ID); err != nil {
			t.Fatal(err)
		}
		findings, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: wf.ID})
		if err != nil || len(findings) != 1 || findings[0].PositionOK == nil || *findings[0].PositionOK {
			t.Fatalf("findings: %+v, %v", findings, err)
		}
		if pub, err := f.db.GetPublication(ctx, 42, 7, discussionPublishKind, findings[0].PublishKey); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unverified publication attributed: %+v, %v", pub, err)
		}
		if f.gitlab.discussionCreates != 1 || len(f.gitlab.notes) != 1 || !strings.Contains(f.gitlab.notes[0].Body, "历史讨论内容") || !strings.Contains(f.gitlab.notes[0].Body, findings[0].Body) {
			t.Fatalf("recovery effects: discussions=%d summaries=%+v", f.gitlab.discussionCreates, f.gitlab.notes)
		}
	}
}

func TestSameWorkflowPublicationWithoutBodyHashFallsBack(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	if err := f.service.Handle(ctx, f.seedReview(t, "intent-legacy-row")); err != nil {
		t.Fatal(err)
	}
	wf, publication := feedbackPublication(t, f)
	publication.BodyHash = ""
	if _, err := f.db.UpsertPublication(ctx, publication); err != nil {
		t.Fatal(err)
	}
	if err := f.publisher.Publish(ctx, wf.ID); err != nil {
		t.Fatal(err)
	}
	findings, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: wf.ID})
	if err != nil || len(findings) != 1 || findings[0].PositionOK == nil || *findings[0].PositionOK {
		t.Fatalf("findings: %+v, %v", findings, err)
	}
	stored, err := f.db.GetPublication(ctx, 42, 7, discussionPublishKind, publication.PublishKey)
	if err != nil || stored.ID != publication.ID || stored.WorkflowID != publication.WorkflowID || stored.BodyHash != "" {
		t.Fatalf("publication changed: %+v, %v", stored, err)
	}
	if f.gitlab.discussionCreates != 1 || len(f.gitlab.notes) != 1 || !strings.Contains(f.gitlab.notes[0].Body, "历史讨论内容") || !strings.Contains(f.gitlab.notes[0].Body, findings[0].Body) {
		t.Fatalf("recovery effects: discussions=%d summaries=%+v", f.gitlab.discussionCreates, f.gitlab.notes)
	}
}
