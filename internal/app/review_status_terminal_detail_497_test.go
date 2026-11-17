package app

import (
	"context"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// TestTerminalFailedNoteKeepsRowDetail pins display consistency between the
// terminal failure note and the detail the recovery/reconcile writers stored on
// the row: a non-empty row detail wins over the generic failure reason (#497).
func TestTerminalFailedNoteKeepsRowDetail(t *testing.T) {
	cases := []struct {
		name       string
		rowDetail  string
		failure    string
		wantDetail string
	}{
		{name: "row detail wins", rowDetail: "publication failed", failure: reviewExecutionFailedReason, wantDetail: "publication failed"},
		{name: "failure reason fallback", rowDetail: "", failure: reviewExecutionFailedReason, wantDetail: reviewExecutionFailedReason},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			f.service.status = testStatusReporter(f.gitlab)
			run, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
				ProjectID: 42, MRIID: 7, HeadSHA: f.head,
			})
			if err != nil {
				t.Fatalf("EnqueueReviewLatest: %v", err)
			}
			if _, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{
				ProjectID: 42, MRIID: 7, Generation: run.Generation, HeadSHA: f.head,
				State: reviewStatusFailed, Detail: tc.rowDetail,
			}); err != nil {
				t.Fatalf("SetReviewStatus: %v", err)
			}
			row, err := f.db.GetReviewStatus(ctx, 42, 7)
			if err != nil {
				t.Fatalf("GetReviewStatus: %v", err)
			}
			ws := f.manager.Get(42)
			if ws == nil {
				t.Fatal("workspace is unavailable")
			}
			bound, err := bindOutbound(ctx, ws)
			if err != nil {
				t.Fatalf("bindOutbound: %v", err)
			}

			if err := f.service.writeTerminalStatusVisit(bound, ws, row, 42, "token",
				reviewStatusFailed, tc.failure, "", false); err != nil {
				t.Fatalf("writeTerminalStatusVisit: %v", err)
			}

			notes := f.gitlab.noteSnapshot()
			if len(notes) != 1 {
				t.Fatalf("notes = %+v, want one terminal status note", notes)
			}
			if cells := statusTableCells(t, notes[0].Body); cells["详情"] != tc.wantDetail {
				t.Fatalf("terminal detail = %q, want %q: %q", cells["详情"], tc.wantDetail, notes[0].Body)
			}
		})
	}
}
