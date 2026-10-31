package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

type acceptedDiscussionGitLab struct {
	*reviewGitLab
	*blockedOutbound
	lost bool
}

func (g *acceptedDiscussionGitLab) CreateDiscussion(ctx context.Context, token string, projectID, iid int64, d gitlab.NewDiscussion) (*gitlab.Discussion, error) {
	created, err := g.reviewGitLab.CreateDiscussion(ctx, token, projectID, iid, d)
	if err != nil {
		return nil, err
	}
	g.wait(ctx)
	if g.lost {
		return nil, errors.New("response was lost")
	}
	return created, nil
}

type delayedDiscussionsGitLab struct{ *reviewGitLab }

func (g *delayedDiscussionsGitLab) ListDiscussions(context.Context, string, int64, int64) ([]gitlab.Discussion, error) {
	return nil, nil
}

// A failed policy write still invalidates the old epoch, without changing the
// configuration required by the publication's freshness check.
func invalidateWhileBlocked(t *testing.T, ws *workspace.Workspace, block *blockedOutbound) {
	t.Helper()
	rejected := errors.New("policy write rejected")
	done := make(chan error, 1)
	go func() {
		done <- ws.ChangeOutbound(context.Background(), func(context.Context) error { return rejected })
	}()
	awaitOutbound(t, block.cancelled, "policy cancellation")
	block.unblock()
	if err := <-done; !errors.Is(err, rejected) {
		t.Fatalf("policy write = %v", err)
	}
}

func TestGateDiscussionSuccessRecoveredBeforeRetry(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "gate-lost-discussion")
	block := newBlockedOutbound(t)
	f.publisher.gitlab = &acceptedDiscussionGitLab{reviewGitLab: f.gitlab, blockedOutbound: block}
	done := make(chan error, 1)
	go func() { done <- f.publisher.Publish(context.Background(), wf.ID) }()
	t.Cleanup(func() { block.unblock(); <-done })
	awaitOutbound(t, block.entered, "accepted discussion")
	invalidateWhileBlocked(t, f.manager.Get(42), block)
	err := <-done
	done <- err
	if !errors.Is(err, workspace.ErrOutboundInvalidated) {
		t.Fatalf("first publication = %v", err)
	}
	f.publisher.gitlab = f.gitlab
	if err := f.service.PublishPending(context.Background()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Errorf("created %d discussions, want one", got)
	}
	if got := workflowPublication(t, f, wf.ID).PublicationState; got != store.WorkflowPublicationPublished {
		t.Errorf("publication state = %q", got)
	}
}

func TestGateDiscussionLostResponseAndDelayedListFailClosed(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "gate-lost-response")
	block := newBlockedOutbound(t)
	f.publisher.gitlab = &acceptedDiscussionGitLab{reviewGitLab: f.gitlab, blockedOutbound: block, lost: true}
	done := make(chan error, 1)
	go func() { done <- f.publisher.Publish(context.Background(), wf.ID) }()
	t.Cleanup(func() { block.unblock(); <-done })
	awaitOutbound(t, block.entered, "accepted discussion")
	invalidateWhileBlocked(t, f.manager.Get(42), block)
	err := <-done
	done <- err
	if !errors.Is(err, workspace.ErrOutboundInvalidated) {
		t.Fatalf("first publication = %v", err)
	}
	f.publisher.gitlab = &delayedDiscussionsGitLab{f.gitlab}
	if err := f.service.PublishPending(context.Background()); err == nil {
		t.Error("an empty delayed list was treated as proof that creation failed")
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Errorf("created %d discussions while the result was uncertain", got)
	}
	if got := workflowPublication(t, f, wf.ID).PublicationState; got != store.WorkflowPublicationPending {
		t.Errorf("unconfirmed publication state = %q, want pending", got)
	}
	f.publisher.gitlab = f.gitlab
	if err := f.service.PublishPending(context.Background()); err != nil {
		t.Fatalf("recover visible discussion: %v", err)
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Errorf("created %d discussions after confirmation", got)
	}
}

func TestGateSameSettingsDoNotCancelProvider(t *testing.T) {
	f := newPolicyGateFixture(t)
	block := newBlockedOutbound(t)
	client := &blockedTurnClient{blockedOutbound: block}
	f.f.service.clients = staticProviderClients{"fake": client}
	f.f.service.agentRun = nil
	job := f.f.seedReview(t, "same-settings")
	done := make(chan error, 1)
	go func() { done <- f.f.service.RunReview(context.Background(), job) }()
	t.Cleanup(func() { block.unblock(); <-done })
	awaitOutbound(t, block.entered, "provider")
	settings := json.RawMessage(`{"ignore":[],"allow_repo_skills":true}`)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := f.projects.Update(ctx, 42, UpdateProjectInput{Settings: &settings}); err != nil {
		t.Errorf("identical settings update = %v", err)
	}
	select {
	case <-block.cancelled:
		t.Error("identical settings cancelled provider")
	default:
	}
	block.unblock()
	err := <-done
	done <- err
	if errors.Is(err, workspace.ErrOutboundInvalidated) {
		t.Errorf("review invalidated by identical settings: %v", err)
	}
}

func TestGatePublicationRetrySettlesCompletedReview(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus = true
	f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
	block := newBlockedOutbound(t)
	api := &blockedPublicationGitLab{reviewGitLab: f.gitlab, blockedOutbound: block, stage: "status"}
	f.publisher.gitlab = api
	job := f.seedReview(t, "gate-status")
	done := make(chan error, 1)
	go func() { done <- f.service.RunReview(context.Background(), job) }()
	t.Cleanup(func() { block.unblock(); <-done })
	awaitOutbound(t, block.entered, "commit status")
	invalidateWhileBlocked(t, f.manager.Get(42), block)
	err := <-done
	done <- err
	if !errors.Is(err, workspace.ErrOutboundInvalidated) {
		t.Fatalf("review publication = %v", err)
	}
	status, err := f.db.GetReviewStatus(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != reviewStatusPublishing {
		t.Errorf("pending publication review status = %q, want publishing", status.State)
	}
	api.stage = ""
	if err := f.service.PublishPending(context.Background()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	status, err = f.db.GetReviewStatus(context.Background(), 42, 7)
	if err != nil || status.State != reviewStatusPass {
		t.Errorf("retried review status = %+v, %v, want PASS", status, err)
	}
}

func TestGateProfileChangeCancelsProvider(t *testing.T) {
	f := newPolicyGateFixture(t)
	other := testReviewProfile()
	other.Propose[0].Provider = "other-provider"
	f.f.profiles["other"] = other
	block := newBlockedOutbound(t)
	f.f.service.clients = staticProviderClients{"fake": &blockedTurnClient{blockedOutbound: block}}
	f.f.service.agentRun = nil
	job := f.f.seedReview(t, "profile-change")
	done := make(chan error, 1)
	go func() { done <- f.f.service.RunReview(context.Background(), job) }()
	t.Cleanup(func() { block.unblock(); <-done })
	awaitOutbound(t, block.entered, "provider")
	profile := "other"
	writeDone := make(chan error, 1)
	go func() {
		_, err := f.projects.Update(context.Background(), 42, UpdateProjectInput{Profile: &profile})
		writeDone <- err
	}()
	select {
	case <-block.cancelled:
	case err := <-writeDone:
		t.Errorf("profile changed without canceling provider: %v", err)
		block.unblock()
		return
	case <-time.After(2 * time.Second):
		t.Error("profile update stalled")
		block.unblock()
		return
	}
	block.unblock()
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	err := <-done
	done <- err
	if !errors.Is(err, workspace.ErrOutboundInvalidated) {
		t.Errorf("review = %v, want invalidated", err)
	}
}
