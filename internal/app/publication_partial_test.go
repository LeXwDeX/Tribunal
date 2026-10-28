package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// partialGitLab fails selected publication steps while the summary note and
// every other call go to the in-memory fake, so one publication can succeed
// partially.
type partialGitLab struct {
	*reviewGitLab
	mu            sync.Mutex
	discussionErr error
	statusErr     error
}

func (g *partialGitLab) fail(discussion, status error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.discussionErr, g.statusErr = discussion, status
}

func (g *partialGitLab) CreateDiscussion(ctx context.Context, token string, projectID, iid int64, d gitlab.NewDiscussion) (*gitlab.Discussion, error) {
	g.mu.Lock()
	err := g.discussionErr
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return g.reviewGitLab.CreateDiscussion(ctx, token, projectID, iid, d)
}

func (g *partialGitLab) SetCommitStatus(ctx context.Context, token string, projectID int64, sha string, st gitlab.CommitStatus) error {
	g.mu.Lock()
	err := g.statusErr
	g.mu.Unlock()
	if err != nil {
		return err
	}
	return g.reviewGitLab.SetCommitStatus(ctx, token, projectID, sha, st)
}

type publicationEffects struct {
	summaries   int
	discussions int
	statuses    int
}

func countPublicationEffects(f *reviewFixture) publicationEffects {
	f.gitlab.mu.Lock()
	defer f.gitlab.mu.Unlock()
	effects := publicationEffects{discussions: f.gitlab.discussionCreates, statuses: len(f.gitlab.statuses)}
	for _, note := range f.gitlab.notes {
		if strings.HasPrefix(note.Body, review.SummaryMarkerPrefix) {
			effects.summaries++
		}
	}
	return effects
}

func usePartialGitLab(f *reviewFixture) *partialGitLab {
	api := &partialGitLab{reviewGitLab: f.gitlab}
	f.service.gitlab = api
	f.publisher.gitlab = api
	f.publisher.retries = 1
	f.defaults.CommitStatus = true
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults
	return api
}

func workflowPublication(t *testing.T, f *reviewFixture, workflowID string) *store.Workflow {
	t.Helper()
	workflow, err := f.db.GetWorkflow(context.Background(), workflowID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	return workflow
}

// One publication step fails after others reached GitLab. Retryable failures
// stay pending; a retry, a restart recovery or a permanent failure must not
// repeat the side effects that already succeeded.
func TestPublisherPartialFailureDoesNotRepeatSideEffects(t *testing.T) {
	unavailable := &gitlab.Error{Op: "upstream", Status: 503}
	for _, tc := range []struct {
		name          string
		discussionErr error
		statusErr     error
		// after the failed attempt
		failed publicationEffects
		// settle drives the follow-up attempt and returns the expected state.
		settle func(*testing.T, *reviewFixture, string) string
		final  publicationEffects
	}{
		{
			name:      "status failure then republish",
			statusErr: unavailable,
			failed:    publicationEffects{summaries: 1, discussions: 1, statuses: 0},
			settle: func(t *testing.T, f *reviewFixture, id string) string {
				if err := f.publisher.Republish(context.Background(), id); err != nil {
					t.Fatalf("Republish: %v", err)
				}
				return store.WorkflowPublicationPublished
			},
			final: publicationEffects{summaries: 1, discussions: 1, statuses: 1},
		},
		{
			name:          "discussion failure then republish",
			discussionErr: unavailable,
			failed:        publicationEffects{summaries: 1, discussions: 0, statuses: 0},
			settle: func(t *testing.T, f *reviewFixture, id string) string {
				if err := f.publisher.Republish(context.Background(), id); err != nil {
					t.Fatalf("Republish: %v", err)
				}
				return store.WorkflowPublicationPublished
			},
			final: publicationEffects{summaries: 1, discussions: 1, statuses: 1},
		},
		{
			name:      "status failure then restart recovery",
			statusErr: unavailable,
			failed:    publicationEffects{summaries: 1, discussions: 1, statuses: 0},
			settle: func(t *testing.T, f *reviewFixture, _ string) string {
				if err := f.service.Recover(context.Background()); err != nil {
					t.Fatalf("Recover: %v", err)
				}
				return store.WorkflowPublicationPublished
			},
			final: publicationEffects{summaries: 1, discussions: 1, statuses: 1},
		},
		{
			name:      "status failure then permanent failure",
			statusErr: unavailable,
			failed:    publicationEffects{summaries: 1, discussions: 1, statuses: 0},
			settle: func(t *testing.T, f *reviewFixture, id string) string {
				f.gitlab.mu.Lock()
				f.gitlab.mrErr = &gitlab.Error{Op: "get_merge_request", Status: 404}
				f.gitlab.mu.Unlock()
				if err := f.publisher.Publish(context.Background(), id); !IsPermanentJob(err) {
					t.Fatalf("Publish error = %v, want permanent", err)
				}
				return store.WorkflowPublicationFailed
			},
			final: publicationEffects{summaries: 1, discussions: 1, statuses: 0},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newReviewFixture(t, reviewTestHead)
			api := usePartialGitLab(f)
			api.fail(tc.discussionErr, tc.statusErr)

			job := f.seedReview(t, "job-partial")
			if err := f.service.RunReview(context.Background(), job); err == nil || IsPermanentJob(err) {
				t.Fatalf("RunReview error = %v, want a retryable publication failure", err)
			}
			workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
			if err != nil {
				t.Fatalf("GetWorkflowByJob: %v", err)
			}
			if workflow.PublicationState != store.WorkflowPublicationPending ||
				workflow.PublicationError != publicationRetryReason {
				t.Fatalf("partial failure publication = %q/%q, want pending", workflow.PublicationState, workflow.PublicationError)
			}
			if got := countPublicationEffects(f); got != tc.failed {
				t.Fatalf("effects after partial failure = %+v, want %+v", got, tc.failed)
			}

			api.fail(nil, nil)
			want := tc.settle(t, f, workflow.ID)
			if got := workflowPublication(t, f, workflow.ID).PublicationState; got != want {
				t.Fatalf("publication state = %q, want %q", got, want)
			}
			if got := countPublicationEffects(f); got != tc.final {
				t.Fatalf("effects after settle = %+v, want %+v", got, tc.final)
			}
			if kinds := publicationKindCounts(t, f, workflow.ID); kinds[discussionPublishKind] != tc.final.discussions ||
				kinds[summaryPublishKind] != 1 {
				t.Fatalf("publication records = %+v", kinds)
			}

			// A settled publication is idempotent: publishing again adds nothing.
			if want == store.WorkflowPublicationPublished {
				if err := f.publisher.Publish(context.Background(), workflow.ID); err != nil {
					t.Fatalf("second Publish: %v", err)
				}
				if got := countPublicationEffects(f); got.summaries != 1 || got.discussions != 1 {
					t.Fatalf("effects after second publish = %+v", got)
				}
			}
		})
	}
}

// A changed MR base after the review marks the publication skipped with the
// reason and publishes nothing from the old result.
func TestPublisherBaseChangeMarksStaleSkipped(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	workflow := reviewWithFailedNote(t, f, "job-publication-base")
	before := countPublicationEffects(f)

	f.gitlab.mu.Lock()
	f.gitlab.version.BaseSHA = strings.Repeat("8", 40)
	f.gitlab.mu.Unlock()
	err := f.publisher.Publish(context.Background(), workflow.ID)
	if !errors.Is(err, ErrStalePublication) {
		t.Fatalf("Publish error = %v, want ErrStalePublication", err)
	}
	stale := workflowPublication(t, f, workflow.ID)
	if stale.PublicationState != store.WorkflowPublicationSkipped ||
		stale.PublicationError != "stale: merge request base changed" {
		t.Fatalf("stale publication = %q/%q", stale.PublicationState, stale.PublicationError)
	}
	if got := countPublicationEffects(f); got != before {
		t.Fatalf("stale publication wrote side effects: %+v -> %+v", before, got)
	}
}
