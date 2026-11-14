package sqlite

import (
	"context"
	"testing"
)

// Deleting a project keeps its discussion receipts because the remote thread
// still exists, but a deleted project's rows can never become actionable
// again. The purge removes exactly those orphans and leaves live projects
// untouched (#466).
func TestPurgeOrphanedDiscussionIntents(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	mustProject(t, st, 42)
	mustProject(t, st, 43)

	for _, key := range []string{"confirmed", "pending"} {
		if created, _, err := st.ReserveDiscussionIntent(ctx, 43, 7, key); err != nil || !created {
			t.Fatalf("reserve live %q: %v", key, err)
		}
	}
	if err := st.ConfirmDiscussionIntent(ctx, 43, 7, "confirmed", "remote-1"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"old-confirmed", "old-pending"} {
		if created, _, err := st.ReserveDiscussionIntent(ctx, 42, 7, key); err != nil || !created {
			t.Fatalf("reserve deleted %q: %v", key, err)
		}
	}
	if err := st.ConfirmDiscussionIntent(ctx, 42, 7, "old-confirmed", "remote-2"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteProject(ctx, 42); err != nil {
		t.Fatal(err)
	}
	// DeleteProject fences the receipts and keeps them; only the explicit
	// purge may remove the orphaned rows.
	var orphaned int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM discussion_intents WHERE project_id = 42`).Scan(&orphaned); err != nil {
		t.Fatal(err)
	}
	if orphaned != 2 {
		t.Fatalf("orphaned intents after project deletion = %d, want 2", orphaned)
	}

	removed, err := st.PurgeOrphanedDiscussionIntents(ctx)
	if err != nil {
		t.Fatalf("PurgeOrphanedDiscussionIntents: %v", err)
	}
	if removed != 2 {
		t.Fatalf("purged = %d, want 2", removed)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM discussion_intents WHERE project_id = 42`).Scan(&orphaned); err != nil {
		t.Fatal(err)
	}
	if orphaned != 0 {
		t.Fatalf("deleted project kept %d intents", orphaned)
	}
	var live int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM discussion_intents WHERE project_id = 43`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 2 {
		t.Fatalf("live project intents = %d, want 2 (never purge a live project)", live)
	}
	if created, _, err := st.ReserveDiscussionIntent(ctx, 43, 7, "confirmed"); err != nil || created {
		t.Fatalf("live receipt changed: created=%v err=%v", created, err)
	}

	// A second pass is a no-op.
	removed, err = st.PurgeOrphanedDiscussionIntents(ctx)
	if err != nil || removed != 0 {
		t.Fatalf("second purge = %d, %v", removed, err)
	}
}
