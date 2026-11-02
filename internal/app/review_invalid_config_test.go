package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

type invalidateConfigGitLab struct {
	*reviewGitLab
	ws  *workspace.Workspace
	err error
}

func (g *invalidateConfigGitLab) GetRawFile(ctx context.Context, token string, projectID int64, path, ref string) ([]byte, error) {
	body, err := g.reviewGitLab.GetRawFile(ctx, token, projectID, path, ref)
	if path == ".gitlab-review.yml" {
		g.err = g.ws.ChangeOutbound(ctx, func(context.Context) error { return nil })
	}
	return body, err
}

func TestInvalidRepoConfigNoteOutboundPolicy(t *testing.T) {
	for _, mode := range []string{"valid", "revoked", "cancelled", "unbound"} {
		t.Run(mode, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			ws := f.manager.Get(42)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode != "unbound" {
				var err error
				ctx, err = bindOutbound(ctx, ws)
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "revoked" {
				if err := ws.ChangeOutbound(context.Background(), func(context.Context) error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "cancelled" {
				cancel()
			}
			f.service.noteInvalidRepoConfig(ctx, "token", 42, 7)
			want := 0
			if mode == "valid" {
				want = 1
			}
			if got := f.gitlab.noteCount(); got != want {
				t.Fatalf("notes = %d, want %d", got, want)
			}
		})
	}
}

func TestInvalidRepoConfigNoteRevocationDrainsSend(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	ws := f.manager.Get(42)
	ctx, err := bindOutbound(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	block := newBlockedOutbound(t)
	f.service.gitlab = &blockedSummaryGitLab{reviewGitLab: f.gitlab, blockedOutbound: block}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.service.noteInvalidRepoConfig(ctx, "token", 42, 7)
	}()
	awaitOutbound(t, block.entered, "invalid config note")
	changed := make(chan error, 1)
	go func() { changed <- ws.ChangeOutbound(context.Background(), func(context.Context) error { return nil }) }()
	awaitOutbound(t, block.cancelled, "invalid config note cancellation")
	select {
	case err := <-changed:
		t.Fatalf("policy changed before send drained: %v", err)
	default:
	}
	block.unblock()
	awaitOutbound(t, done, "invalid config note completion")
	if err := <-changed; err != nil {
		t.Fatal(err)
	}
}

func TestInvalidRepoConfigNoteSanitizesError(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	var logs bytes.Buffer
	f.service.logger = slog.New(slog.NewTextHandler(&logs, nil))
	f.gitlab.noteErr = errors.New("secret-token-in-error")
	ctx, err := bindOutbound(context.Background(), f.manager.Get(42))
	if err != nil {
		t.Fatal(err)
	}
	f.service.noteInvalidRepoConfig(ctx, "token", 42, 7)
	if !strings.Contains(logs.String(), "error_class") || strings.Contains(logs.String(), "secret-token-in-error") {
		t.Fatalf("unsanitized note error log: %s", logs.String())
	}
}

func TestInvalidRepoConfigRevokedDuringCollection(t *testing.T) {
	for _, candidate := range []bool{false, true} {
		name := "run"
		if candidate {
			name = "candidate"
		}
		t.Run(name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			f.gitlab.docs[".gitlab-review.yml"] = "review: ["
			api := &invalidateConfigGitLab{reviewGitLab: f.gitlab, ws: f.manager.Get(42)}
			f.service.gitlab = api
			if candidate {
				job, _ := f.sendMergeRequestWebhook(t, f.repoConfigWebhook(t), nil)
				f.handleCandidate(t, job.ID)
				if run := f.claimReviewRun(t); run != nil {
					t.Fatalf("candidate queued review: %+v", run)
				}
			} else {
				job := f.seedRunFor(t, "invalid-revoked", reviewTestHead, "mr_open")
				err := f.service.RunReview(context.Background(), job)
				if !errors.Is(err, ErrInvalidRepoConfig) {
					t.Fatalf("RunReview = %v", err)
				}
				status, err := f.db.GetReviewStatus(context.Background(), 42, 7)
				if err != nil || status.State != reviewStatusFailed || status.Detail != invalidRepoConfigDetail {
					t.Fatalf("status = %+v, %v", status, err)
				}
			}
			if api.err != nil {
				t.Fatalf("policy change: %v", api.err)
			}
			if got := f.gitlab.noteCount(); got != 0 {
				t.Fatalf("notes = %d, want 0", got)
			}
		})
	}
}

func TestRunReviewInvalidRepoConfigIsPermanentAndCommented(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.docs[".gitlab-review.yml"] = "ignore: [\n"
	job := f.seedRunFor(t, "invalid-config", reviewTestHead, "mr_open")

	err := f.service.RunReview(ctx, job)
	if err == nil || !IsPermanentJob(err) {
		t.Fatalf("error = %v, want a permanent job error", err)
	}
	if !errors.Is(err, ErrInvalidRepoConfig) {
		t.Fatalf("error = %v, want ErrInvalidRepoConfig", err)
	}
	if _, err := f.db.GetWorkflowByJob(ctx, job.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("workflow was created for invalid configuration: %v", err)
	}
	if f.gitlab.noteCount() != 1 {
		t.Fatalf("notes = %d, want one explanation comment", f.gitlab.noteCount())
	}
	if body := f.gitlab.notes[0].Body; body != invalidRepoConfigNote {
		t.Fatalf("note = %q, want %q", body, invalidRepoConfigNote)
	}
}

func TestStoredInvalidIgnorePolicyIsPermanent(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	if _, err := f.db.UpdateProject(ctx, 42, store.ProjectUpdate{Settings: []byte(`{"ignore":["!keep"]}`)}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	job := f.seedRunFor(t, "invalid-stored-policy", reviewTestHead, "mr_open")
	calls := 0
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		calls++
		return nil, nil
	}

	err := f.service.RunReview(ctx, job)
	if err == nil || !IsPermanentJob(err) || !errors.Is(err, ErrInvalidRepoConfig) {
		t.Fatalf("error = %v, want a permanent ErrInvalidRepoConfig", err)
	}
	if calls != 0 {
		t.Fatalf("model calls = %d, want 0", calls)
	}
	if f.gitlab.noteCount() != 1 {
		t.Fatalf("notes = %d, want one explanation", f.gitlab.noteCount())
	}
	if _, err := f.db.GetWorkflowByJob(ctx, job.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("workflow was created for a stored invalid policy: %v", err)
	}
}
