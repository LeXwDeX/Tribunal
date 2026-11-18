package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// TestLoadStreamSnapshotRevisionBounds verifies the review stream poll: the
// workflow row is always returned, but the narrow node, finding and
// publication projections are only loaded when the revision moved.
func TestLoadStreamSnapshotRevisionBounds(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	rev := mustCreateWorkflow(t, st, "wf-stream", reviewMeta(42, 9, "head"), clock.Now())

	first, err := st.LoadStreamSnapshot(ctx, "wf-stream", -1)
	if err != nil {
		t.Fatalf("initial LoadStreamSnapshot: %v", err)
	}
	if first.ProjectID != 42 || first.Status != "pending" {
		t.Fatalf("workflow row = %+v", first)
	}
	if len(first.Nodes) == 0 || first.Findings == nil || first.Publications == nil {
		t.Fatalf("initial collections: nodes=%d findings=%v publications=%v",
			len(first.Nodes), first.Findings, first.Publications)
	}

	unchanged, err := st.LoadStreamSnapshot(ctx, "wf-stream", first.Revision)
	if err != nil {
		t.Fatalf("unchanged LoadStreamSnapshot: %v", err)
	}
	if unchanged.Revision != first.Revision {
		t.Fatalf("revision moved without a write: %d -> %d", first.Revision, unchanged.Revision)
	}
	if unchanged.Nodes != nil || unchanged.Findings != nil || unchanged.Publications != nil {
		t.Fatalf("unchanged revision reloaded collections: nodes=%v findings=%v publications=%v",
			unchanged.Nodes, unchanged.Findings, unchanged.Publications)
	}

	eventRev := mustAppendEvent(t, st, "wf-stream", rev, queuedEvent(t, "a", 1, clock.Now()))
	if eventRev == rev {
		t.Fatalf("AppendEvent did not advance the workflow revision: still %d", eventRev)
	}
	nodesMoved, err := st.LoadStreamSnapshot(ctx, "wf-stream", first.Revision)
	if err != nil {
		t.Fatalf("node LoadStreamSnapshot: %v", err)
	}
	if nodesMoved.Revision == first.Revision {
		t.Fatal("node event did not advance the stream revision")
	}
	if len(nodesMoved.Nodes) == 0 {
		t.Fatal("node event did not reload nodes")
	}

	if err := st.ReplaceFindings(ctx, "wf-stream", []store.Finding{{
		ID: "f1", OriginNode: "a", OriginIndex: 0, Severity: "P1",
		Category: "bug", Title: "t", Body: "b", Decision: "publish",
	}}); err != nil {
		t.Fatalf("ReplaceFindings: %v", err)
	}
	findingsMoved, err := st.LoadStreamSnapshot(ctx, "wf-stream", nodesMoved.Revision)
	if err != nil {
		t.Fatalf("finding LoadStreamSnapshot: %v", err)
	}
	if findingsMoved.Revision == nodesMoved.Revision {
		t.Fatal("finding replace did not advance the stream revision")
	}
	if len(findingsMoved.Findings) != 1 || findingsMoved.Findings[0].ID != "f1" ||
		findingsMoved.Findings[0].Decision != "publish" {
		t.Fatalf("findings = %+v", findingsMoved.Findings)
	}

	if _, err := st.UpsertPublication(ctx, store.Publication{
		ProjectID: 42, MRIID: 9, Kind: "summary", PublishKey: "k1",
		WorkflowID: "wf-stream", Fingerprint: "fp", GitlabID: "note-1",
	}); err != nil {
		t.Fatalf("UpsertPublication: %v", err)
	}
	publicationsMoved, err := st.LoadStreamSnapshot(ctx, "wf-stream", findingsMoved.Revision)
	if err != nil {
		t.Fatalf("publication LoadStreamSnapshot: %v", err)
	}
	if publicationsMoved.Revision == findingsMoved.Revision {
		t.Fatal("publication upsert did not advance the stream revision")
	}
	if len(publicationsMoved.Publications) != 1 || publicationsMoved.Publications[0].Kind != "summary" {
		t.Fatalf("publications = %+v", publicationsMoved.Publications)
	}

	// Deleting the project removes the counter row instead of leaving it
	// orphaned, and the cascade deletes do not recreate it.
	if err := st.DeleteProject(ctx, 42); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if got := rowCount(t, st, `SELECT COUNT(*) FROM workflow_stream_revisions`); got != 0 {
		t.Fatalf("stream revision rows after project delete = %d", got)
	}
}

func TestLoadStreamSnapshotMissingWorkflow(t *testing.T) {
	clock := newFakeClock()
	st := openTestStore(t, clock)
	_, err := st.LoadStreamSnapshot(context.Background(), "wf-missing", -1)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LoadStreamSnapshot error = %v, want not found", err)
	}
}
