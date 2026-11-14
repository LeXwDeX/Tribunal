package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func recordDeliveryAt(t *testing.T, st *Store, d store.Delivery, at time.Time) {
	t.Helper()
	d.ReceivedAt = at
	if _, _, err := st.RecordDelivery(context.Background(), d, nil); err != nil {
		t.Fatalf("RecordDelivery %s: %v", d.UUID, err)
	}
}

func TestListWebhookDeliverySummariesGroupsByMR(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	mustProject(t, st, 43)
	base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)

	recordDeliveryAt(t, st, store.Delivery{
		UUID: "ignored-1", ProjectID: 42, MRIID: 7, Event: "Note Hook",
		Outcome: store.DeliveryIgnored, Reason: "unsupported_action",
	}, base)
	recordDeliveryAt(t, st, store.Delivery{
		UUID: "ignored-2", ProjectID: 42, MRIID: 7, Event: "Note Hook",
		Outcome: store.DeliveryIgnored, Reason: "unsupported_action",
	}, base.Add(time.Minute))
	recordDeliveryAt(t, st, store.Delivery{
		UUID: "rejected-1", ProjectID: 42, MRIID: 7, Event: "Note Hook",
		Outcome: store.DeliveryRejected, Reason: "invalid_secret",
	}, base.Add(2*time.Minute))
	recordDeliveryAt(t, st, store.Delivery{
		UUID: "ignored-3", ProjectID: 42, MRIID: 8, Event: "Merge Request Hook",
		Outcome: store.DeliveryIgnored, Reason: "branch_filtered",
	}, base.Add(3*time.Minute))
	recordDeliveryAt(t, st, store.Delivery{
		UUID: "ignored-4", ProjectID: 43, MRIID: 9, Event: "Push Hook",
		Outcome: store.DeliveryIgnored, Reason: "unsupported_action",
	}, base.Add(4*time.Minute))

	all, next, err := st.ListWebhookDeliverySummaries(ctx, store.WebhookDeliveryFilter{}, "", 50)
	if err != nil {
		t.Fatalf("ListWebhookDeliverySummaries: %v", err)
	}
	if next != "" || len(all) != 3 {
		t.Fatalf("all = %+v, next = %q", all, next)
	}
	// Newest activity first: MR 9, then MR 8, then MR 7.
	if all[0].ProjectID != 43 || all[0].MRIID != 9 ||
		all[1].ProjectID != 42 || all[1].MRIID != 8 ||
		all[2].ProjectID != 42 || all[2].MRIID != 7 {
		t.Fatalf("order = %+v", all)
	}
	mr7 := all[2]
	if mr7.Deliveries != 3 || mr7.Ignored != 2 || mr7.Rejected != 1 || mr7.RejectedInvalidSecret != 1 {
		t.Fatalf("mr7 = %+v", mr7)
	}
	if mr7.Reasons["unsupported_action"] != 2 {
		t.Fatalf("mr7 reasons = %+v", mr7.Reasons)
	}
	if !mr7.LastReceivedAt.Equal(base.Add(2 * time.Minute)) {
		t.Fatalf("mr7 last = %v", mr7.LastReceivedAt)
	}

	project42 := int64(42)
	filtered, _, err := st.ListWebhookDeliverySummaries(ctx, store.WebhookDeliveryFilter{ProjectID: &project42}, "", 50)
	if err != nil || len(filtered) != 2 {
		t.Fatalf("project filter = %+v, %v", filtered, err)
	}
	mr := int64(8)
	one, _, err := st.ListWebhookDeliverySummaries(ctx, store.WebhookDeliveryFilter{MRIID: &mr}, "", 50)
	if err != nil || len(one) != 1 || one[0].MRIID != 8 || one[0].Reasons["branch_filtered"] != 1 {
		t.Fatalf("mr filter = %+v, %v", one, err)
	}

	page, cursor, err := st.ListWebhookDeliverySummaries(ctx, store.WebhookDeliveryFilter{}, "", 1)
	if err != nil || len(page) != 1 || page[0].MRIID != 9 || cursor == "" {
		t.Fatalf("first page = %+v, cursor=%q, %v", page, cursor, err)
	}
	page, cursor, err = st.ListWebhookDeliverySummaries(ctx, store.WebhookDeliveryFilter{}, cursor, 2)
	if err != nil || len(page) != 2 || page[0].MRIID != 8 || page[1].MRIID != 7 || cursor != "" {
		t.Fatalf("second page = %+v, cursor=%q, %v", page, cursor, err)
	}
}

func TestListWebhookDeliverySummariesCountsEnqueued(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	mustProject(t, st, 42)
	job := &store.Job{Kind: "review.candidate", Key: "review:42:7"}
	delivery := store.Delivery{
		UUID: "enqueued-1", ProjectID: 42, MRIID: 7, Event: "Merge Request Hook",
		Outcome: store.DeliveryEnqueued,
	}
	if _, _, err := st.RecordDelivery(ctx, delivery, job); err != nil {
		t.Fatalf("RecordDelivery: %v", err)
	}
	rows, _, err := st.ListWebhookDeliverySummaries(ctx, store.WebhookDeliveryFilter{}, "", 50)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if rows[0].Enqueued != 1 || rows[0].Deliveries != 1 || rows[0].Ignored != 0 {
		t.Fatalf("row = %+v", rows[0])
	}
}

func TestListWebhookDeliverySummariesRespectsAccess(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	mustProject(t, st, 42)
	mustProject(t, st, 43)
	mustAccessUser(t, st, 7, "alice", "viewer")
	if err := st.SetProjectGrant(ctx, 42, 7, store.ProjectAccessRead); err != nil {
		t.Fatalf("SetProjectGrant: %v", err)
	}
	recordDeliveryAt(t, st, store.Delivery{
		UUID: "a", ProjectID: 42, MRIID: 7, Event: "Note Hook",
		Outcome: store.DeliveryIgnored, Reason: "unsupported_action",
	}, time.Now().UTC())
	recordDeliveryAt(t, st, store.Delivery{
		UUID: "b", ProjectID: 43, MRIID: 9, Event: "Note Hook",
		Outcome: store.DeliveryIgnored, Reason: "unsupported_action",
	}, time.Now().UTC())

	user := int64(7)
	rows, _, err := st.ListWebhookDeliverySummaries(ctx, store.WebhookDeliveryFilter{AccessUserID: &user}, "", 50)
	if err != nil || len(rows) != 1 || rows[0].ProjectID != 42 {
		t.Fatalf("granted rows = %+v, %v", rows, err)
	}
	stranger := int64(8)
	rows, _, err = st.ListWebhookDeliverySummaries(ctx, store.WebhookDeliveryFilter{AccessUserID: &stranger}, "", 50)
	if err != nil || len(rows) != 0 {
		t.Fatalf("stranger rows = %+v, %v", rows, err)
	}
}

func TestListWebhookDeliverySummariesRejectsChangedCursorFilter(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	mustProject(t, st, 42)
	recordDeliveryAt(t, st, store.Delivery{
		UUID: "a", ProjectID: 42, MRIID: 7, Event: "Note Hook",
		Outcome: store.DeliveryIgnored, Reason: "unsupported_action",
	}, time.Now().UTC())
	recordDeliveryAt(t, st, store.Delivery{
		UUID: "b", ProjectID: 42, MRIID: 8, Event: "Note Hook",
		Outcome: store.DeliveryIgnored, Reason: "unsupported_action",
	}, time.Now().UTC().Add(time.Second))
	_, cursor, err := st.ListWebhookDeliverySummaries(ctx, store.WebhookDeliveryFilter{}, "", 1)
	if err != nil || cursor == "" {
		t.Fatalf("cursor = %q, %v", cursor, err)
	}
	project := int64(42)
	if _, _, err := st.ListWebhookDeliverySummaries(ctx, store.WebhookDeliveryFilter{ProjectID: &project}, cursor, 1); err == nil {
		t.Fatal("a cursor from another filter was accepted")
	}
}
