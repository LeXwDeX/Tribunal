package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

// TestFeedbackRunOnceRecordsSuccessfulCollection makes an empty pass
// distinguishable from a failing collector (#459).
func TestFeedbackRunOnceRecordsSuccessfulCollection(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	service := newFeedbackService(t, f, &repo.Git{}, f.manager)
	fixed := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return fixed }

	service.RunOnce(ctx)
	state, err := f.db.FeedbackCollectionState(ctx)
	if err != nil {
		t.Fatalf("FeedbackCollectionState: %v", err)
	}
	if state == nil || state.LastCollectedAt == nil || !state.LastCollectedAt.Equal(fixed) {
		t.Fatalf("collection state = %+v, want last_collected_at %v", state, fixed)
	}
	if state.LastErrorClass != "" {
		t.Fatalf("empty pass recorded an error class: %+v", state)
	}
}

// TestFeedbackRunOnceRecordsErrorClassAndWarn covers a persistent upstream
// failure: the last successful collection stays put, the fixed class is
// exposed, and the WARN never carries raw error text.
func TestFeedbackRunOnceRecordsErrorClassAndWarn(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-feedback-observability")
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	service := newFeedbackService(t, f, &repo.Git{}, f.manager)
	var logs bytes.Buffer
	service.logger = slog.New(slog.NewTextHandler(&logs, nil))
	fixed := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return fixed }

	f.gitlab.mu.Lock()
	f.gitlab.mrErr = &gitlab.Error{Op: "get_merge_request", Status: 500}
	f.gitlab.mu.Unlock()
	service.RunOnce(ctx)
	state, err := f.db.FeedbackCollectionState(ctx)
	if err != nil {
		t.Fatalf("FeedbackCollectionState: %v", err)
	}
	if state.LastErrorClass != "upstream" || state.LastErrorAt == nil {
		t.Fatalf("failed collection state = %+v, want upstream", state)
	}
	if state.LastCollectedAt != nil {
		t.Fatalf("failed pass advanced last_collected_at: %+v", state)
	}
	logged := logs.String()
	if !strings.Contains(logged, "error_class=upstream") || !strings.Contains(logged, "project_id=42") {
		t.Fatalf("WARN missing class or identifiers: %s", logged)
	}
	if strings.Contains(logged, "get_merge_request") || strings.Contains(logged, "500") {
		t.Fatalf("WARN leaked raw upstream detail: %s", logged)
	}

	f.gitlab.mu.Lock()
	f.gitlab.mrErr = nil
	f.gitlab.mu.Unlock()
	service.RunOnce(ctx)
	state, err = f.db.FeedbackCollectionState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastCollectedAt == nil || !state.LastCollectedAt.Equal(fixed) {
		t.Fatalf("recovered collection state = %+v", state)
	}
	if state.LastErrorClass != "upstream" {
		t.Fatalf("recovery erased the last error class: %+v", state)
	}
}

// TestFeedbackErrorClassIsFixed pins the sanitized mapping.
func TestFeedbackErrorClassIsFixed(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{&gitlab.Error{Op: "x", Status: 401}, "auth"},
		{&gitlab.Error{Op: "x", Status: 403}, "auth"},
		{&gitlab.Error{Op: "x", Status: 404}, "not_found"},
		{&gitlab.Error{Op: "x", Status: 429}, "rate_limited"},
		{&gitlab.Error{Op: "x", Status: 502}, "upstream"},
		{&repo.Error{Op: "fetch", Reason: repo.ReasonNetwork}, "git"},
		{workspace.ErrUnsafePath, "workspace"},
		{errFeedbackCredential, "credential"},
	}
	for _, tc := range cases {
		if got := feedbackErrorClass(tc.err); got != tc.want {
			t.Fatalf("feedbackErrorClass(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
