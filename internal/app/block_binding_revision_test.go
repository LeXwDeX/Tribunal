package app

import (
	"context"
	"errors"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestReplaceBlockBindingWaitsForProjectLifecycleLock(t *testing.T) {
	ctx := context.Background()
	r := newLifecycleRuntime(t)
	service, err := NewExtensionService(ExtensionServiceConfig{Store: r.store, Workspace: r.workspace})
	if err != nil {
		t.Fatal(err)
	}
	id := int64(42)
	current, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "project", ProjectID: &id, Block: "review", Skills: []string{"old"},
	})
	if err != nil {
		t.Fatal(err)
	}

	unlock, err := r.workspace.Get(id).Lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wait := &waitContext{Context: ctx, waiting: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := service.ReplaceBlockBinding(wait, BlockBindingInput{
			Scope: "project", ProjectID: &id, Block: "review", Skills: []string{"new"},
		}, current.Revision)
		done <- err
	}()
	waitConsumer(t, wait.waiting)

	rows, err := r.store.ListBlockBindings(ctx, "project", &id)
	if err != nil || len(rows) != 1 || rows[0].Skills[0] != "old" {
		t.Fatalf("write reached the database before the lifecycle lock was released: %+v, %v", rows, err)
	}

	unlock()
	if err := <-done; err != nil {
		t.Fatalf("ReplaceBlockBinding after unlock: %v", err)
	}
	rows, err = r.store.ListBlockBindings(ctx, "project", &id)
	if err != nil || len(rows) != 1 || len(rows[0].Skills) != 1 || rows[0].Skills[0] != "new" {
		t.Fatalf("binding after unlock = %+v, %v", rows, err)
	}
}

func TestReplaceBlockBindingRejectsDeletedProjectAfterLock(t *testing.T) {
	ctx := context.Background()
	r := newLifecycleRuntime(t)
	service, err := NewExtensionService(ExtensionServiceConfig{Store: r.store, Workspace: r.workspace})
	if err != nil {
		t.Fatal(err)
	}
	id := int64(42)
	current, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "project", ProjectID: &id, Block: "review", Skills: []string{"old"},
	})
	if err != nil {
		t.Fatal(err)
	}

	unlock, err := r.workspace.Get(id).Lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wait := &waitContext{Context: ctx, waiting: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := service.ReplaceBlockBinding(wait, BlockBindingInput{
			Scope: "project", ProjectID: &id, Block: "review", Skills: []string{"late"},
		}, current.Revision)
		done <- err
	}()
	waitConsumer(t, wait.waiting)
	if err := r.store.DeleteProject(ctx, id); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()

	if err := <-done; err == nil {
		t.Fatal("ReplaceBlockBinding accepted a deleted project")
	}
	if rows, err := r.store.ListBlockBindings(ctx, "project", &id); err != nil || len(rows) != 0 {
		t.Fatalf("deleted project bindings = %+v, %v", rows, err)
	}
	if _, err := r.store.GetProject(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project still exists: %v", err)
	}
}
