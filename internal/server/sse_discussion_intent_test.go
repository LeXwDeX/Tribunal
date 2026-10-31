package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

type intentStreamStore struct {
	*fakeReviewStore
	db *sqlite.Store
}

func (s *intentStreamStore) ListPublications(ctx context.Context, workflowID string) ([]store.Publication, error) {
	return s.db.ListPublications(ctx, workflowID)
}

func TestStreamDoesNotPublishUnknownDiscussionIntent(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "stream.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fake := newFakeReviewStore()
	fake.setWorkflow(store.Workflow{ID: "wf", ProjectID: 42, Status: "completed", PublicationState: store.WorkflowPublicationPending})
	fake.setNodes("wf")
	s := &Server{store: &intentStreamStore{fakeReviewStore: fake, db: db}}
	before, err := s.loadStreamSnapshot(ctx, "wf")
	if err != nil {
		t.Fatal(err)
	}
	if created, _, err := db.ReserveDiscussionIntent(ctx, 42, 7, "key"); err != nil || !created {
		t.Fatalf("reserve = %v, %v", created, err)
	}
	unknown, err := s.loadStreamSnapshot(ctx, "wf")
	if err != nil {
		t.Fatal(err)
	}
	if unknown.terminal() {
		t.Fatal("pending stream closed")
	}
	for _, event := range diffStreamSnapshots(before, unknown) {
		if event.name == "published" {
			t.Fatalf("unknown intent emitted a published event: %+v", event)
		}
	}
	if err := db.ConfirmDiscussionIntent(ctx, 42, 7, "key", "remote"); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.loadStreamSnapshot(ctx, "wf")
	if err != nil {
		t.Fatal(err)
	}
	if events := diffStreamSnapshots(unknown, receipt); len(events) != 0 {
		t.Fatalf("receipt alone emitted events: %+v", events)
	}
	if _, err := db.UpsertPublication(ctx, store.Publication{ProjectID: 42, MRIID: 7, Kind: "discussion", PublishKey: "key", GitlabID: "remote", WorkflowID: "wf", Fingerprint: "fp"}); err != nil {
		t.Fatal(err)
	}
	confirmed, err := s.loadStreamSnapshot(ctx, "wf")
	if err != nil {
		t.Fatal(err)
	}
	events := diffStreamSnapshots(unknown, confirmed)
	if len(events) != 1 || events[0].name != "published" {
		t.Fatalf("confirmed publication events = %+v", events)
	}
}
