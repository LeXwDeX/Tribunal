package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// Hold the producer's workspace read lock after its retry has been persisted.
type persistedRetryBarrier struct {
	JobEnqueuer
	entered chan string
	release chan struct{}
}

func (b *persistedRetryBarrier) Enqueue(ctx context.Context, job store.Job) (string, bool, error) {
	id, inserted, err := b.JobEnqueuer.Enqueue(ctx, job)
	if err != nil {
		return id, inserted, err
	}
	b.entered <- id
	select {
	case <-b.release:
		return id, inserted, nil
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
}

func TestIssue161RetryDeleteReregisterInterleavings(t *testing.T) {
	t.Run("retry persists before delete", func(t *testing.T) {
		r := newLifecycleRuntime(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Exhaust the registration job so the next claim is the retry.
		var registrationID string
		for attempt := 1; attempt <= 3; attempt++ {
			job, err := r.store.Claim(ctx, "old", []string{"workspace.onboard"}, time.Minute)
			if err != nil || job == nil || job.Key != "workspace.onboard:42" || job.Attempts != attempt {
				t.Fatalf("registration claim: %+v %v", job, err)
			}
			if attempt == 1 {
				registrationID = job.ID
			} else if job.ID != registrationID {
				t.Fatalf("registration retry changed job ID: %+v", job)
			}
			if err := r.store.Fail(ctx, *job, errors.New("synthetic failure"), nil); err != nil {
				t.Fatal(err)
			}
		}
		b := &persistedRetryBarrier{JobEnqueuer: r.store, entered: make(chan string, 1), release: make(chan struct{})}
		var release sync.Once
		defer release.Do(func() { close(b.release) })
		r.workspaces.queue = b
		type retryResult struct {
			id  string
			err error
		}
		retried := make(chan retryResult, 1)
		go func() { id, err := r.workspaces.RetryOnboarding(ctx, 42); retried <- retryResult{id, err} }()
		var retryID string
		select {
		case retryID = <-b.entered:
		case <-ctx.Done():
			t.Fatal("retry did not persist")
		}
		old, err := r.store.Claim(ctx, "old-retry", []string{"workspace.onboard"}, time.Minute)
		if err != nil || old == nil || old.ID != retryID || old.Key != "workspace.onboard:42:retry" {
			t.Fatalf("claim retry: %+v %v", old, err)
		}
		// Seed adjacent IDs after claiming so they cannot affect the claim order.
		neighborIDs := make(map[int64]string)
		for _, id := range []int64{4, 420} {
			seedProject(t, r.store, r.cipher, id, "secret", "", "default")
			jobID, _, err := r.store.Enqueue(ctx, store.Job{Kind: "workspace.onboard", Key: "workspace.onboard:" + fmt.Sprint(id) + ":retry", Payload: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			neighborIDs[id] = jobID
		}
		deleteCtx := &waitContext{Context: ctx, waiting: make(chan struct{})}
		deleted := make(chan error, 1)
		go func() { deleted <- r.Projects().Delete(deleteCtx, 42) }()
		waitConsumer(t, deleteCtx.waiting)
		if _, err := r.store.GetProject(ctx, 42); err != nil {
			t.Fatalf("delete passed retry lock: %v", err)
		}
		release.Do(func() { close(b.release) })
		select {
		case result := <-retried:
			if result.err != nil || result.id != retryID {
				t.Fatalf("retry result: %+v", result)
			}
		case <-ctx.Done():
			t.Fatal("retry deadlocked")
		}
		select {
		case err := <-deleted:
			if err != nil {
				t.Fatalf("delete: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("delete deadlocked")
		}
		if _, err := r.store.GetJob(ctx, retryID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("old retry survived delete: %v", err)
		}
		if _, err := r.store.GetJob(ctx, registrationID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("old registration job survived delete: %v", err)
		}
		for id, jobID := range neighborIDs {
			if _, err := r.store.GetProject(ctx, id); err != nil {
				t.Fatalf("neighbor %d missing: %v", id, err)
			}
			if job, err := r.store.GetJob(ctx, jobID); err != nil || job.State != store.JobQueued {
				t.Fatalf("neighbor retry %d: %+v %v", id, job, err)
			}
		}
		registration, err := r.Projects().Register(ctx, RegisterInput{Project: "42", AccessToken: "synthetic-token"})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.workspaces.onboard(ctx, 42, old); !IsPermanentJob(err) {
			t.Fatalf("old claimed job not fenced: %v", err)
		}
		if err := r.workspaces.Handle(ctx, old); err != nil {
			t.Fatalf("handle old job: %v", err)
		}
		workspace, err := r.store.GetWorkspace(ctx, 42)
		if err != nil || workspace.State != "new" {
			t.Fatalf("old job changed new workspace: %+v %v", workspace, err)
		}
		if _, err := os.Lstat(filepath.Join(r.workspace.Root(), "42")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old job touched workspace: %v", err)
		}
		if job, err := r.store.GetJob(ctx, registration.JobID); err != nil || job.State != store.JobQueued || job.ID == retryID {
			t.Fatalf("new job: %+v %v", job, err)
		}
	})

	t.Run("delete commits before retry", func(t *testing.T) {
		r := newLifecycleRuntime(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		entered, proceed := make(chan struct{}), make(chan struct{})
		var release sync.Once
		defer release.Do(func() { close(proceed) })
		r.Projects().store = &deleteProjectStore{ProjectStore: r.store, afterDelete: func() error {
			close(entered)
			select {
			case <-proceed:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
		deleted := make(chan error, 1)
		go func() { deleted <- r.Projects().Delete(ctx, 42) }()
		waitConsumer(t, entered)
		wait := &waitContext{Context: ctx, waiting: make(chan struct{})}
		retried := make(chan error, 1)
		go func() { _, err := r.workspaces.RetryOnboarding(wait, 42); retried <- err }()
		waitConsumer(t, wait.waiting)
		release.Do(func() { close(proceed) })
		select {
		case err := <-deleted:
			if err != nil {
				t.Fatalf("delete: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("delete deadlocked")
		}
		select {
		case err := <-retried:
			requireAppCode(t, err, "not_found")
		case <-ctx.Done():
			t.Fatal("retry deadlocked")
		}
		registration, err := r.Projects().Register(ctx, RegisterInput{Project: "42", AccessToken: "synthetic-token"})
		if err != nil {
			t.Fatal(err)
		}
		job, err := r.store.Claim(ctx, "new", []string{"workspace.onboard"}, time.Minute)
		if err != nil || job == nil || job.ID != registration.JobID {
			t.Fatalf("new claim: %+v %v", job, err)
		}
		if job, err := r.store.Claim(ctx, "extra", []string{"workspace.onboard"}, time.Minute); err != nil || job != nil {
			t.Fatalf("unexpected old retry: %+v %v", job, err)
		}
	})
}
