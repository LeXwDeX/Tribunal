package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestBlockBindingRevisionDigestIsContentAddressed(t *testing.T) {
	empty := store.BlockBindingRevision("platform", nil, "review", nil, nil)
	if want := store.BlockBindingRevision("platform", nil, "review", []string{}, []string{}); empty != want {
		t.Fatalf("nil and empty arrays differ: %q != %q", empty, want)
	}
	if !store.ValidBlockBindingRevision(empty) || empty == store.BlockBindingAbsentRevision {
		t.Fatalf("digest %q is not a valid non-absent revision", empty)
	}
	if store.ValidBlockBindingRevision("") || store.ValidBlockBindingRevision("ABSENT") {
		t.Fatal("invalid revisions accepted")
	}

	projectID := int64(42)
	project := store.BlockBindingRevision("project", &projectID, "review", nil, nil)
	if project == empty {
		t.Fatal("scope and project ID do not affect the digest")
	}
	if ordered, reordered :=
		store.BlockBindingRevision("platform", nil, "review", []string{"a", "b"}, nil),
		store.BlockBindingRevision("platform", nil, "review", []string{"b", "a"}, nil); ordered == reordered {
		t.Fatal("array order does not affect the digest")
	}
	if unique, duplicate :=
		store.BlockBindingRevision("platform", nil, "review", []string{"a"}, nil),
		store.BlockBindingRevision("platform", nil, "review", []string{"a", "a"}, nil); unique == duplicate {
		t.Fatal("duplicate entries do not affect the digest")
	}
	if tools, skills :=
		store.BlockBindingRevision("platform", nil, "review", []string{"a"}, nil),
		store.BlockBindingRevision("platform", nil, "review", nil, []string{"a"}); tools == skills {
		t.Fatal("MCP tools and skills are not separated")
	}
}

func TestConditionalBlockBindingWriteRejectsStaleRevision(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())

	created, err := s.UpsertBlockBindingIfRevision(ctx, store.BlockBinding{
		Scope: store.ScopePlatform, Block: "review", MCPTools: []string{"a"},
	}, store.BlockBindingAbsentRevision)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Revision != store.BlockBindingRevision("platform", nil, "review", []string{"a"}, nil) {
		t.Fatalf("created revision = %q", created.Revision)
	}

	updated, err := s.UpsertBlockBindingIfRevision(ctx, store.BlockBinding{
		Scope: store.ScopePlatform, Block: "review", MCPTools: []string{"b"},
	}, created.Revision)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Revision == created.Revision {
		t.Fatal("revision did not change")
	}

	if _, err := s.UpsertBlockBindingIfRevision(ctx, store.BlockBinding{
		Scope: store.ScopePlatform, Block: "review", MCPTools: []string{"c"},
	}, created.Revision); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("stale write error = %v, want ErrStaleRevision", err)
	}
	rows, err := s.ListBlockBindings(ctx, store.ScopePlatform, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if len(rows[0].MCPTools) != 1 || rows[0].MCPTools[0] != "b" || rows[0].Revision != updated.Revision {
		t.Fatalf("stale write changed the row: %+v", rows[0])
	}
}

func TestConditionalBlockBindingWriteSerializesConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())

	created, err := s.UpsertBlockBindingIfRevision(ctx, store.BlockBinding{
		Scope: store.ScopePlatform, Block: "review",
	}, store.BlockBindingAbsentRevision)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	type result struct {
		row *store.BlockBinding
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, tool := range []string{"one", "two"} {
		tool := tool
		go func() {
			<-start
			row, err := s.UpsertBlockBindingIfRevision(ctx, store.BlockBinding{
				Scope: store.ScopePlatform, Block: "review", MCPTools: []string{tool},
			}, created.Revision)
			results <- result{row: row, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results

	success, stale := 0, 0
	for _, got := range []result{first, second} {
		switch {
		case got.err == nil:
			success++
		case errors.Is(got.err, store.ErrStaleRevision):
			stale++
		default:
			t.Fatalf("unexpected error: %v", got.err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("success/stale = %d/%d, want 1/1", success, stale)
	}
	rows, err := s.ListBlockBindings(ctx, store.ScopePlatform, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if len(rows[0].MCPTools) != 1 || (rows[0].MCPTools[0] != "one" && rows[0].MCPTools[0] != "two") {
		t.Fatalf("stored tools = %v", rows[0].MCPTools)
	}
}

func TestConditionalBlockBindingCreateRaceOnlyOneWins(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, tool := range []string{"one", "two"} {
		tool := tool
		go func() {
			<-start
			_, err := s.UpsertBlockBindingIfRevision(ctx, store.BlockBinding{
				Scope: store.ScopePlatform, Block: "verify", MCPTools: []string{tool},
			}, store.BlockBindingAbsentRevision)
			results <- err
		}()
	}
	close(start)
	errs := []error{<-results, <-results}
	success, stale := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, store.ErrStaleRevision):
			stale++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("success/stale = %d/%d, want 1/1", success, stale)
	}
	rows, err := s.ListBlockBindings(ctx, store.ScopePlatform, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
}

func TestConditionalBlockBindingWriteRejectsRestoredRevision(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())

	custom, err := s.UpsertBlockBindingIfRevision(ctx, store.BlockBinding{
		Scope: store.ScopePlatform, Block: "review", Skills: []string{"custom"},
	}, store.BlockBindingAbsentRevision)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetDefaultBlockBinding(ctx, "review", &store.BlockBinding{
		Scope: store.ScopePlatform, Block: "review", Skills: []string{"default"},
	}); err != nil {
		t.Fatalf("restore write: %v", err)
	}
	if _, err := s.UpsertBlockBindingIfRevision(ctx, store.BlockBinding{
		Scope: store.ScopePlatform, Block: "review", Skills: []string{"stale"},
	}, custom.Revision); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("stale after restore error = %v, want ErrStaleRevision", err)
	}
	rows, err := s.ListBlockBindings(ctx, store.ScopePlatform, nil)
	if err != nil || len(rows) != 1 || rows[0].Skills[0] != "default" {
		t.Fatalf("restored row = %+v, %v", rows, err)
	}
}

func TestConditionalBlockBindingWriteRejectsDeletedRow(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	projectID := int64(42)

	created, err := s.UpsertBlockBindingIfRevision(ctx, store.BlockBinding{
		Scope: store.ScopeProject, ProjectID: &projectID, Block: "review",
	}, store.BlockBindingAbsentRevision)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.DeleteProject(ctx, projectID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := s.UpsertBlockBindingIfRevision(ctx, store.BlockBinding{
		Scope: store.ScopeProject, ProjectID: &projectID, Block: "review",
	}, created.Revision); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("write after project delete error = %v, want ErrStaleRevision", err)
	}
}
