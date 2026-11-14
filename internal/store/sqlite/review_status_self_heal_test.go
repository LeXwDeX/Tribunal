package sqlite

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func seedPendingStatus(t *testing.T, st *Store, projectID, mrIID int64) *store.ReviewStatus {
	t.Helper()
	if _, err := st.EnqueueReviewLatest(context.Background(), store.ReviewCommand{
		ProjectID: projectID,
		MRIID:     mrIID,
		HeadSHA:   strings.Repeat("a", 40),
		Payload:   json.RawMessage(`{"trigger":"command"}`),
	}); err != nil {
		t.Fatalf("EnqueueReviewLatest: %v", err)
	}
	row, err := st.GetReviewStatus(context.Background(), projectID, mrIID)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	return row
}

func applyStatus(t *testing.T, st *Store, projectID, mrIID int64, state string) *store.ReviewStatus {
	t.Helper()
	row, err := st.GetReviewStatus(context.Background(), projectID, mrIID)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if _, err := st.SetReviewStatus(context.Background(), store.ReviewStatusGoal{
		ProjectID: projectID, MRIID: mrIID, Generation: row.Generation,
		Attempt: row.Attempt, HeadSHA: row.HeadSHA, Profile: row.Profile, State: state,
	}); err != nil {
		t.Fatalf("SetReviewStatus(%s): %v", state, err)
	}
	row, err = st.GetReviewStatus(context.Background(), projectID, mrIID)
	if err != nil {
		t.Fatalf("GetReviewStatus after %s: %v", state, err)
	}
	if err := st.MarkReviewStatusApplied(context.Background(), projectID, mrIID, int64(row.Version), 55, "hash", ""); err != nil {
		t.Fatalf("MarkReviewStatusApplied(%s): %v", state, err)
	}
	row, err = st.GetReviewStatus(context.Background(), projectID, mrIID)
	if err != nil {
		t.Fatalf("GetReviewStatus applied %s: %v", state, err)
	}
	return row
}

// TestStatusVerificationOnlyReturnsPendingRows pins the high-frequency scan to
// rows whose desired version is not applied yet. A successfully applied
// terminal row must not produce a review.status job every five minutes (#459).
func TestStatusVerificationOnlyReturnsPendingRows(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	mustProject(t, st, 42)

	seedPendingStatus(t, st, 42, 7)
	pending, err := st.ListReviewStatusesForVerification(ctx, 42, 0)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending verification rows = %+v, %v", pending, err)
	}

	applied := applyStatus(t, st, 42, 7, store.ReviewStatusPass)
	if applied.AppliedVersion != applied.Version || applied.LastError != "" {
		t.Fatalf("applied row = %+v", applied)
	}
	rows, err := st.ListReviewStatusesForVerification(ctx, 42, 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("applied success still in high-frequency scan: %+v, %v", rows, err)
	}

	// A newer generation is pending again and stays in the fast scan.
	seedPendingStatus(t, st, 42, 7)
	rows, err = st.ListReviewStatusesForVerification(ctx, 42, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("new generation verification rows = %+v, %v", rows, err)
	}

	// A terminal upstream error settles the row and leaves the fast scan.
	row, err := st.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkReviewStatusApplied(ctx, 42, 7, int64(row.Version), 0, "", "review superseded"); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListReviewStatusesForVerification(ctx, 42, 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("settled error still in high-frequency scan: %+v, %v", rows, err)
	}
}

// TestMarkReviewStatusAppliedRecordsVerificationTime keeps the low-frequency
// self-heal clock on the row so a repeated applied write cannot re-enqueue it.
func TestMarkReviewStatusAppliedRecordsVerificationTime(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	seedPendingStatus(t, st, 42, 7)
	applied := applyStatus(t, st, 42, 7, store.ReviewStatusPass)
	if applied.LastVerifiedAt.IsZero() || !applied.LastVerifiedAt.Equal(clock.Now().UTC()) {
		t.Fatalf("last verified = %v, want %v", applied.LastVerifiedAt, clock.Now().UTC())
	}
	clock.Advance(2 * time.Hour)
	if err := st.MarkReviewStatusApplied(ctx, 42, 7, int64(applied.Version), 0, "", ""); err != nil {
		t.Fatalf("idempotent applied write: %v", err)
	}
	again, err := st.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !again.LastVerifiedAt.Equal(clock.Now().UTC()) {
		t.Fatalf("idempotent applied write did not advance verification time: %+v", again)
	}
}

// TestSelfHealScanSelectsStaleAppliedTerminalRows covers the low-frequency
// query: applied, error-free, not cancelled, and not verified recently.
func TestSelfHealScanSelectsStaleAppliedTerminalRows(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)

	states := []string{
		store.ReviewStatusCancelled,
		store.ReviewStatusPass,
		store.ReviewStatusBlocking,
		store.ReviewStatusIncomplete,
		store.ReviewStatusFailed,
		store.ReviewStatusStale,
	}
	// An applied but still-active row is owned by the unfinished reconciliation,
	// never by the terminal self-heal scan.
	seedPendingStatus(t, st, 42, 7)
	applyStatus(t, st, 42, 7, store.ReviewStatusRunning)
	for i, state := range states {
		seedPendingStatus(t, st, 42, int64(i+1))
		applyStatus(t, st, 42, int64(i+1), state)
		clock.Advance(5 * time.Hour)
	}
	// MR 1 (cancelled), MR 2 (pass) and MR 7 (running) were applied more than
	// 24h before now.
	cutoff := clock.Now().Add(-24 * time.Hour)
	rows, err := st.ListAppliedReviewStatusesForSelfHeal(ctx, 42, 0, cutoff)
	if err != nil {
		t.Fatalf("ListAppliedReviewStatusesForSelfHeal: %v", err)
	}
	if len(rows) != 1 || rows[0].MRIID != 2 || rows[0].State != store.ReviewStatusPass {
		t.Fatalf("self-heal rows = %+v, want only the applied non-cancelled MR 2", rows)
	}
	rows, err = st.ListAppliedReviewStatusesForSelfHeal(ctx, 42, 2, cutoff)
	if err != nil || len(rows) != 0 {
		t.Fatalf("keyset page = %+v, %v, want none after MR 2", rows, err)
	}
	// A pending row is the fast scan's job, never the self-heal scan's.
	seedPendingStatus(t, st, 42, 9)
	rows, err = st.ListAppliedReviewStatusesForSelfHeal(ctx, 42, 0, cutoff)
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending row leaked into self-heal scan: %+v, %v", rows, err)
	}
}

// TestMarkReviewStatusVerifiedAdvancesClockWithoutChangingVersion lets the
// self-heal writer record a check even when the remote body already matches.
func TestMarkReviewStatusVerifiedAdvancesClockWithoutChangingVersion(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	seedPendingStatus(t, st, 42, 7)
	applied := applyStatus(t, st, 42, 7, store.ReviewStatusPass)
	clock.Advance(time.Hour)
	if err := st.MarkReviewStatusVerified(ctx, 42, 7, int64(applied.Version), 0, "", clock.Now().UTC()); err != nil {
		t.Fatalf("MarkReviewStatusVerified: %v", err)
	}
	row, err := st.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if row.Version != applied.Version || row.AppliedVersion != applied.AppliedVersion ||
		!row.LastVerifiedAt.Equal(clock.Now().UTC()) {
		t.Fatalf("verified row = %+v, want same version with a new verification time", row)
	}
	// A stale version is ignored rather than overwriting a newer receipt.
	if err := st.MarkReviewStatusVerified(ctx, 42, 7, int64(applied.Version)-1, 0, "", clock.Now().Add(time.Hour).UTC()); err != nil {
		t.Fatalf("stale version verify: %v", err)
	}
	row, err = st.GetReviewStatus(ctx, 42, 7)
	if err != nil || !row.LastVerifiedAt.Equal(clock.Now().UTC()) {
		t.Fatalf("stale version changed row: %+v, %v", row, err)
	}
}
