package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

type cancelledSummary404GitLab struct {
	*reviewGitLab
	entered chan struct{}
}

func (g *cancelledSummary404GitLab) UpdateMRNote(ctx context.Context, _ string, _, _, _ int64, _ string) error {
	close(g.entered)
	<-ctx.Done()
	return &gitlab.Error{Op: "update_note", Status: 404}
}

func TestSummary404DuringOutboundInvalidation(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	ws := f.manager.Get(42)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, err := bindOutbound(parent, ws)
	if err != nil {
		t.Fatal(err)
	}
	api := &cancelledSummary404GitLab{reviewGitLab: f.gitlab, entered: make(chan struct{})}
	f.publisher.gitlab = api
	type result struct {
		notFound bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		notFound, err := f.publisher.updateSummaryNote(ctx, "token", 42, 7, 1, "body", func() error { return nil })
		done <- result{notFound, err}
	}()
	select {
	case <-api.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("summary update did not start")
	}
	if err := ws.ChangeOutbound(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.notFound || !errors.Is(got.err, workspace.ErrOutboundInvalidated) {
		t.Fatalf("update = (notFound %v, error %v), want invalidation without recovery", got.notFound, got.err)
	}
}
