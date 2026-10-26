package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
)

// gatedGitLab holds discussion creation until release is closed, so a test
// can overlap two publishers at the check-create-record window.
type gatedGitLab struct {
	*reviewGitLab
	arrived chan struct{}
	release chan struct{}
}

func (g *gatedGitLab) CreateDiscussion(ctx context.Context, token string, projectID, iid int64, d gitlab.NewDiscussion) (*gitlab.Discussion, error) {
	g.arrived <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.reviewGitLab.CreateDiscussion(ctx, token, projectID, iid, d)
}

func TestPublisherConcurrentPublishCreatesOneDiscussion(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	workflow := reviewWithFailedNote(t, f, "job-concurrent-publish")
	if f.gitlab.discussionCreates != 0 {
		t.Fatalf("discussions before concurrent publish = %d, want 0", f.gitlab.discussionCreates)
	}
	gated := &gatedGitLab{reviewGitLab: f.gitlab, arrived: make(chan struct{}, 8), release: make(chan struct{})}
	f.publisher.gitlab = gated

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = f.publisher.Publish(ctx, workflow.ID)
		}(i)
	}
	select {
	case <-gated.arrived:
	case <-time.After(10 * time.Second):
		close(gated.release)
		wg.Wait()
		t.Fatalf("no discussion creation started: %v", errs)
	}
	// Give an unserialized second publisher time to pass its publication
	// check and reach GitLab too.
	select {
	case <-gated.arrived:
	case <-time.After(500 * time.Millisecond):
	}
	close(gated.release)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}
	if f.gitlab.discussionCreates != 1 {
		t.Fatalf("discussion creates = %d, want 1 for one finding", f.gitlab.discussionCreates)
	}
	if kinds := publicationKindCounts(t, f, workflow.ID); kinds[discussionPublishKind] != 1 {
		t.Fatalf("publications = %+v, want one discussion", kinds)
	}
}
