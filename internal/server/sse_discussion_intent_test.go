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

// LoadStreamSnapshot keeps the shared publication filter: when the revision
// moved, publications come from the database so discussion receipts are
// subject to the same intent check as the real stream.
func (s *intentStreamStore) LoadStreamSnapshot(ctx context.Context, workflowID string, revision int64) (*store.StreamSnapshot, error) {
	snap, err := s.fakeReviewStore.LoadStreamSnapshot(ctx, workflowID, revision)
	if err != nil || snap.Publications == nil {
		return snap, err
	}
	publications, err := s.db.ListPublications(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	snap.Publications = make([]store.StreamPublication, 0, len(publications))
	for _, publication := range publications {
		snap.Publications = append(snap.Publications, store.StreamPublication{
			ID: publication.ID, Kind: publication.Kind, GitlabID: publication.GitlabID,
		})
	}
	return snap, nil
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
	before, err := s.loadStreamSnapshot(ctx, "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	if created, _, err := db.ReserveDiscussionIntent(ctx, 42, 7, "key"); err != nil || !created {
		t.Fatalf("reserve = %v, %v", created, err)
	}
	unknown, err := s.loadStreamSnapshot(ctx, "wf", before)
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
	receipt, err := s.loadStreamSnapshot(ctx, "wf", unknown)
	if err != nil {
		t.Fatal(err)
	}
	if events := diffStreamSnapshots(unknown, receipt); len(events) != 0 {
		t.Fatalf("receipt alone emitted events: %+v", events)
	}
	if _, err := db.UpsertPublication(ctx, store.Publication{ProjectID: 42, MRIID: 7, Kind: "discussion", PublishKey: "key", GitlabID: "remote", WorkflowID: "wf", Fingerprint: "fp"}); err != nil {
		t.Fatal(err)
	}
	// Writing the publication bypasses the fake store, so advance its revision
	// to force the next poll to reload publications from the database.
	fake.setPublications("wf")
	confirmed, err := s.loadStreamSnapshot(ctx, "wf", receipt)
	if err != nil {
		t.Fatal(err)
	}
	events := diffStreamSnapshots(unknown, confirmed)
	if len(events) != 1 || events[0].name != "published" {
		t.Fatalf("confirmed publication events = %+v", events)
	}
}
