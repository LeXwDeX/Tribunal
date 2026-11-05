package sqlite

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// settleStatusOnly settles one MR's review status the way an empty-diff run
// does: terminal state with a recognizable detail and no workflow.
func settleStatusOnly(t *testing.T, s *Store, project, mr int64, head, state, detail string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	run, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: project, MRIID: mr, HeadSHA: head,
		Payload: json.RawMessage(`{"trigger":"mr_open","profile":"default"}`),
	})
	if err != nil {
		t.Fatalf("enqueue %d/%d: %v", project, mr, err)
	}
	if written, err := s.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: project, MRIID: mr, Generation: run.Generation, Attempt: 1,
		HeadSHA: head, State: state, Detail: detail,
	}); err != nil || !written {
		t.Fatalf("settle %d/%d = %v, %v", project, mr, written, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE review_status SET updated_at = ? WHERE project_id = ? AND mr_iid = ?`,
		formatTime(at), project, mr); err != nil {
		t.Fatal(err)
	}
}

func statusOnlyKeys(rows []store.StatusOnlyReview) [][2]int64 {
	out := make([][2]int64, 0, len(rows))
	for _, row := range rows {
		out = append(out, [2]int64{row.ProjectID, row.MRIID})
	}
	return out
}

func TestListStatusOnlyReviewsConditionsAndAccess(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	mustProject(t, s, 43)
	mustAccessUser(t, s, 7, "alice", "viewer")
	if err := s.SetProjectGrant(ctx, 42, 7, store.ProjectAccessRead); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	headA, headB := reviewHead("a"), reviewHead("b")

	settleStatusOnly(t, s, 42, 1, headA, store.ReviewStatusIncomplete, store.StatusDetailEmptyDiff, base.Add(1*time.Minute))
	settleStatusOnly(t, s, 42, 2, headA, store.ReviewStatusPass, store.StatusDetailNoReviewable, base.Add(2*time.Minute))
	// Same terminal state but no recognizable detail: legacy data, not listed.
	settleStatusOnly(t, s, 42, 3, headA, store.ReviewStatusIncomplete, "", base.Add(3*time.Minute))
	// A workflow exists for the same head: the review is workflow-backed.
	settleStatusOnly(t, s, 42, 4, headA, store.ReviewStatusIncomplete, store.StatusDetailEmptyDiff, base.Add(4*time.Minute))
	mustCreateWorkflow(t, s, "wf-42-4", reviewMeta(42, 4, headA), base)
	// A workflow for an older head does not hide the newer empty-diff run.
	settleStatusOnly(t, s, 42, 5, headB, store.ReviewStatusIncomplete, store.StatusDetailEmptyDiff, base.Add(5*time.Minute))
	mustCreateWorkflow(t, s, "wf-42-5", reviewMeta(42, 5, headA), base)
	// Another project the reader holds no grant on.
	settleStatusOnly(t, s, 43, 1, headA, store.ReviewStatusIncomplete, store.StatusDetailEmptyDiff, base.Add(6*time.Minute))

	rows, next, err := s.ListStatusOnlyReviews(ctx, store.StatusOnlyFilter{}, "", 50)
	if err != nil || next != "" {
		t.Fatalf("unrestricted = %v, next %q, %v", rows, next, err)
	}
	want := [][2]int64{{43, 1}, {42, 5}, {42, 2}, {42, 1}}
	if got := statusOnlyKeys(rows); len(got) != len(want) {
		t.Fatalf("unrestricted rows = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("unrestricted rows = %v, want %v", got, want)
			}
		}
	}
	if rows[3].State != store.ReviewStatusIncomplete || rows[3].Detail != store.StatusDetailEmptyDiff || rows[3].HeadSHA != headA {
		t.Fatalf("row = %+v", rows[3])
	}

	alice := int64(7)
	rows, _, err = s.ListStatusOnlyReviews(ctx, store.StatusOnlyFilter{AccessUserID: &alice}, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ProjectID != 42 {
			t.Fatalf("ungranted project leaked: %+v", row)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("alice rows = %v", statusOnlyKeys(rows))
	}
	nobody := int64(0)
	if rows, _, err = s.ListStatusOnlyReviews(ctx, store.StatusOnlyFilter{AccessUserID: &nobody}, "", 50); err != nil || len(rows) != 0 {
		t.Fatalf("no-access rows = %v, %v", rows, err)
	}

	// Filters: verdict matches the settled state; MR and project narrow.
	rows, _, _ = s.ListStatusOnlyReviews(ctx, store.StatusOnlyFilter{Verdict: "PASS"}, "", 50)
	if len(rows) != 1 || rows[0].MRIID != 2 {
		t.Fatalf("verdict filter = %v", statusOnlyKeys(rows))
	}
	mr := int64(1)
	rows, _, _ = s.ListStatusOnlyReviews(ctx, store.StatusOnlyFilter{MRIID: &mr}, "", 50)
	if len(rows) != 2 {
		t.Fatalf("mr filter = %v", statusOnlyKeys(rows))
	}
	rows, _, _ = s.ListStatusOnlyReviews(ctx, store.StatusOnlyFilter{Since: base.Add(5 * time.Minute)}, "", 50)
	if len(rows) != 2 {
		t.Fatalf("since filter = %v", statusOnlyKeys(rows))
	}
}

func TestListStatusOnlyReviewsPaginates(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	base := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for mr := int64(1); mr <= 5; mr++ {
		// MR 4 and 5 share a timestamp to exercise the tie breaker.
		at := base.Add(time.Duration(mr) * time.Minute)
		if mr == 5 {
			at = base.Add(4 * time.Minute)
		}
		settleStatusOnly(t, s, 42, mr, reviewHead("a"), store.ReviewStatusIncomplete, store.StatusDetailEmptyDiff, at)
	}
	var got []int64
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		rows, next, err := s.ListStatusOnlyReviews(ctx, store.StatusOnlyFilter{}, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			got = append(got, row.MRIID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	want := []int64{5, 4, 3, 2, 1}
	if len(got) != len(want) {
		t.Fatalf("paged = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paged = %v, want %v", got, want)
		}
	}
	if _, _, err := s.ListStatusOnlyReviews(ctx, store.StatusOnlyFilter{}, "not-a-cursor", 2); err == nil {
		t.Fatal("invalid cursor accepted")
	}
}

func TestListStatusOnlyReviewsGrantOnlyGlobalReadAndStrictness(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	mustProject(t, s, 43)
	mustAccessUser(t, s, 7, "alice", "viewer")
	if err := s.SetProjectGrant(ctx, 42, 7, store.ProjectAccessRead); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	settleStatusOnly(t, s, 42, 1, reviewHead("a"), store.ReviewStatusIncomplete, store.StatusDetailEmptyDiff, base)
	settleStatusOnly(t, s, 43, 1, reviewHead("a"), store.ReviewStatusPass, store.StatusDetailNoReviewable, base.Add(time.Minute))

	alice := int64(7)
	rows, _, err := s.ListStatusOnlyReviews(ctx, store.StatusOnlyFilter{AccessUserID: &alice, GrantOnly: true}, "", 10)
	if err != nil || len(rows) != 1 || rows[0].ProjectID != 42 {
		t.Fatalf("grant-only = %v, %v", statusOnlyKeys(rows), err)
	}

	enabled, revision := true, int64(1)
	if _, _, _, err := s.UpdatePlatformUser(ctx, alice, store.PlatformUserUpdate{GlobalRead: &enabled, Revision: &revision}); err != nil {
		t.Fatal(err)
	}
	rows, _, _ = s.ListStatusOnlyReviews(ctx, store.StatusOnlyFilter{AccessUserID: &alice}, "", 10)
	if len(rows) != 2 {
		t.Fatalf("global read = %v, want both projects", statusOnlyKeys(rows))
	}
	rows, _, _ = s.ListStatusOnlyReviews(ctx, store.StatusOnlyFilter{AccessUserID: &alice, GrantOnly: true}, "", 10)
	if len(rows) != 1 || rows[0].ProjectID != 42 {
		t.Fatalf("grant-only with global read = %v", statusOnlyKeys(rows))
	}

	// Verdict is an exact match like ListWorkflows.
	if rows, _, _ = s.ListStatusOnlyReviews(ctx, store.StatusOnlyFilter{Verdict: "pass"}, "", 10); len(rows) != 0 {
		t.Fatalf("lowercase verdict matched %v", statusOnlyKeys(rows))
	}
	if rows, _, _ = s.ListStatusOnlyReviews(ctx, store.StatusOnlyFilter{Verdict: "PASS"}, "", 10); len(rows) != 1 {
		t.Fatalf("exact verdict = %v", statusOnlyKeys(rows))
	}
}

func TestListStatusOnlyReviewsHeadCaseInsensitiveWorkflowMatch(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	settleStatusOnly(t, s, 42, 1, strings.Repeat("a", 40), store.ReviewStatusIncomplete, store.StatusDetailEmptyDiff,
		time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	mustCreateWorkflow(t, s, "wf-upper", reviewMeta(42, 1, strings.Repeat("A", 40)), time.Date(2026, 10, 22, 11, 0, 0, 0, time.UTC))
	rows, _, err := s.ListStatusOnlyReviews(ctx, store.StatusOnlyFilter{}, "", 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("workflow with differently cased head should hide the row: %v, %v", statusOnlyKeys(rows), err)
	}
}
