package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/workspace"
)

// queueWriterBehindReader simulates a long review (content RLock) with a
// refresh (content Lock) queued behind it. The writer-preferring lock then
// blocks every new content reader until the review ends.
func queueWriterBehindReader(t *testing.T, manager *workspace.Manager, id int64) {
	t.Helper()
	ws := manager.Get(id)
	unlockRead, err := ws.RLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	wait := &waitContext{Context: ctx, waiting: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if unlock, err := ws.Lock(wait); err == nil {
			unlock()
		}
	}()
	waitConsumer(t, wait.waiting)
	t.Cleanup(func() {
		cancel()
		<-done
		unlockRead()
	})
}

func requireReturns(t *testing.T, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s blocked behind a queued workspace writer", name)
	}
}

func TestEntriesDoNotBlockBehindQueuedWorkspaceWriter(t *testing.T) {
	st := openTestStore(t)
	cipher := newTestCipher(t)
	seedProject(t, st, cipher, 42, "whsec-test", "{}", "default")
	manager := workspace.NewManager(t.TempDir(), nil)
	webhooks, err := NewWebhookService(WebhookServiceConfig{
		Store: &recordingDeliveryStore{WebhookStore: st}, Workspace: manager, Cipher: cipher,
		Profiles: testProfiles(), ReviewDefaults: testDefaults(),
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(DispatcherConfig{
		Store: st, Workspace: manager, GitLab: defaultFakeGitLab(), Cipher: cipher,
		Profiles: testProfiles(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	queueWriterBehindReader(t, manager, 42)

	requireReturns(t, "WebhookService.Handle", func() {
		result, err := webhooks.Handle(context.Background(), WebhookRequest{
			Event: "Merge Request Hook", Token: "whsec-test", UUID: uuid.NewString(), Body: mrBody(42, 7, nil),
		})
		if err != nil || result.Ignored == "storage_error" {
			t.Errorf("Handle = %+v, %v", result, err)
		}
	})
	requireReturns(t, "Dispatcher.StartReview", func() {
		if _, err := dispatcher.StartReview(context.Background(), 42, 7, "default", false); err != nil {
			t.Errorf("StartReview: %v", err)
		}
	})
	requireReturns(t, "Dispatcher.RefreshWorkspace", func() {
		if _, err := dispatcher.RefreshWorkspace(context.Background(), 42, false, false); err != nil {
			t.Errorf("RefreshWorkspace: %v", err)
		}
	})
}

func TestWebhookWaitsForProjectDeleteThenSeesNoProject(t *testing.T) {
	r := newLifecycleRuntime(t)
	ws := r.workspace.Get(42)
	unlockLifecycle, err := ws.LifecycleLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			unlockLifecycle()
		}
	}()
	type outcome struct {
		result WebhookResult
		err    error
	}
	out := make(chan outcome, 1)
	go func() {
		res, err := r.Webhooks().Handle(context.Background(), WebhookRequest{
			Event: "Merge Request Hook", Token: "secret", UUID: uuid.NewString(), Body: mrBody(42, 7, nil),
		})
		out <- outcome{res, err}
	}()
	select {
	case o := <-out:
		t.Fatalf("webhook ran during lifecycle write lock: %+v", o)
	case <-time.After(200 * time.Millisecond):
	}
	// The deletion commits while the lifecycle lock is held.
	if err := r.store.DeleteProject(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	released = true
	unlockLifecycle()
	select {
	case o := <-out:
		if o.err != nil || o.result.Ignored != "project_not_registered" {
			t.Fatalf("webhook after delete = %+v, %v", o.result, o.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook did not resume after lifecycle unlock")
	}
}

func TestProjectDeleteWaitsForEntryLifecycleReader(t *testing.T) {
	r := newLifecycleRuntime(t)
	unlock, err := r.workspace.Get(42).LifecycleRLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = r.Projects().Delete(ctx, 42)
	unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Delete while an entry holds the lifecycle read lock = %v", err)
	}
	if err := r.Projects().Delete(context.Background(), 42); err != nil {
		t.Fatalf("Delete after unlock: %v", err)
	}
}
