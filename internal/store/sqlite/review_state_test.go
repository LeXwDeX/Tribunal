package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

func reviewHead(seed string) string {
	return strings.Repeat(seed, 40)
}

func reviewRequestMeta(base string) map[string]string {
	return map[string]string{
		"trigger":     "mr_open",
		"profile":     "default",
		"base_sha":    base,
		"fingerprint": "fp-" + base,
		"config_hash": "cfg-" + base,
		"prompt_hash": "prm-" + base,
	}
}

func reviewWorkflowRequest(jobID string, generation int64, projectID, mrIID int64, head string) ReviewWorkflowRequest {
	return ReviewWorkflowRequest{
		JobID:              jobID,
		Spec:               testSpec(),
		Meta:               reviewRequestMeta("base-" + head),
		DossierVersion:     1,
		DossierData:        json.RawMessage(`{"head":"` + head + `","files":["a.go"]}`),
		ExpectedGeneration: generation,
		ProjectID:          projectID,
		MRIID:              mrIID,
		HeadSHA:            head,
	}
}

func TestRecordReviewDeliveryAtomicGenerationAndDuplicate(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")

	delivery := store.Delivery{
		UUID: "uuid-1", ProjectID: 42, Event: "Merge Request Hook",
		Outcome: store.DeliveryEnqueued,
	}
	result, err := s.RecordReviewDelivery(ctx, delivery, &store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
		Payload: json.RawMessage(`{"trigger":"mr_open","profile":"default","full":true}`),
	})
	if err != nil {
		t.Fatalf("RecordReviewDelivery: %v", err)
	}
	if result.Duplicate || result.Generation != 1 || result.JobID == "" {
		t.Fatalf("result = %+v, want dup=false generation=1", result)
	}
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewControl: %v", err)
	}
	if control.Generation != 1 || control.Cancelled || control.HeadSHA != head {
		t.Fatalf("control = %+v", control)
	}
	job, err := s.GetJob(ctx, result.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["generation"] != float64(1) || payload["head_sha"] != head ||
		payload["trigger"] != "mr_open" || payload["full"] != true {
		t.Fatalf("payload = %v", payload)
	}

	// A replay with different content is still a duplicate and must not bump.
	replay, err := s.RecordReviewDelivery(ctx, delivery, &store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("b"),
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Duplicate || replay.JobID != result.JobID || replay.Generation != 0 {
		t.Fatalf("replay = %+v", replay)
	}
	control, err = s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if control.Generation != 1 || control.HeadSHA != head {
		t.Fatalf("duplicate bumped control: %+v", control)
	}
	stored, err := s.GetJob(ctx, result.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.Payload) != string(job.Payload) {
		t.Fatalf("duplicate replaced payload: %s", stored.Payload)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE "key" = 'review:42:7'`); got != 1 {
		t.Fatalf("review job count = %d, want 1", got)
	}
}

func TestRecordReviewDeliveryInvalidCommandLeavesNoRows(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")

	cases := []struct {
		name    string
		d       store.Delivery
		command *store.ReviewCommand
	}{
		{
			name: "short head",
			d:    store.Delivery{UUID: "bad-head", ProjectID: 42, Event: "Merge Request Hook", Outcome: store.DeliveryEnqueued},
			command: &store.ReviewCommand{
				ProjectID: 42, MRIID: 7, HeadSHA: "abc",
			},
		},
		{
			name: "cancel with head",
			d:    store.Delivery{UUID: "cancel-head", ProjectID: 42, Event: "Merge Request Hook", Outcome: store.DeliveryEnqueued},
			command: &store.ReviewCommand{
				ProjectID: 42, MRIID: 7, HeadSHA: head, Cancel: true,
			},
		},
		{
			name: "caller generation",
			d:    store.Delivery{UUID: "gen", ProjectID: 42, Event: "Note Hook", Outcome: store.DeliveryEnqueued},
			command: &store.ReviewCommand{
				ProjectID: 42, MRIID: 7, HeadSHA: head, Payload: json.RawMessage(`{"generation":9}`),
			},
		},
		{
			name: "unknown project",
			d:    store.Delivery{UUID: "unknown", ProjectID: 99, Event: "Note Hook", Outcome: store.DeliveryEnqueued},
			command: &store.ReviewCommand{
				ProjectID: 99, MRIID: 7, HeadSHA: head,
			},
		},
		{
			name: "missing iid",
			d:    store.Delivery{UUID: "no-iid", ProjectID: 42, Event: "Note Hook", Outcome: store.DeliveryEnqueued},
			command: &store.ReviewCommand{
				ProjectID: 42, HeadSHA: head,
			},
		},
		{
			name: "project mismatch",
			d:    store.Delivery{UUID: "mismatch", ProjectID: 42, Event: "Note Hook", Outcome: store.DeliveryEnqueued},
			command: &store.ReviewCommand{
				ProjectID: 7, MRIID: 7, HeadSHA: head,
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.RecordReviewDelivery(ctx, tt.d, tt.command); err == nil {
				t.Fatal("expected error")
			}
			if got := rowCount(t, s, `SELECT COUNT(*) FROM webhook_deliveries WHERE uuid = ?`, tt.d.UUID); got != 0 {
				t.Fatalf("delivery rows = %d, want 0", got)
			}
			if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE kind IN ('review.run', 'review.cancel')`); got != 0 {
				t.Fatalf("job rows = %d, want 0", got)
			}
			if got := rowCount(t, s, `SELECT COUNT(*) FROM review_controls`); got != 0 {
				t.Fatalf("control rows = %d, want 0", got)
			}
		})
	}
}

func TestEnqueueReviewLatestReplacesQueuedAndPreservesRunningFence(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)
	headA := reviewHead("a")
	headB := reviewHead("b")
	headC := reviewHead("c")

	first, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headA,
		Payload: json.RawMessage(`{"trigger":"mr_open"}`),
	})
	if err != nil || first.Generation != 1 {
		t.Fatalf("first enqueue = %+v, %v", first, err)
	}
	running, err := s.Claim(ctx, "worker-1", []string{reviewRunKind}, time.Hour)
	if err != nil || running == nil {
		t.Fatalf("Claim = %+v, %v", running, err)
	}
	if running.ID != first.JobID || running.Attempts != 1 {
		t.Fatalf("running = %+v", running)
	}

	second, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headB,
		Payload: json.RawMessage(`{"trigger":"mr_update"}`),
	})
	if err != nil || second.Generation != 2 {
		t.Fatalf("second enqueue = %+v, %v", second, err)
	}
	if second.JobID == first.JobID {
		t.Fatal("successor reused the running job id")
	}
	third, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headC,
		Payload: json.RawMessage(`{"trigger":"command"}`),
	})
	if err != nil || third.Generation != 3 {
		t.Fatalf("third enqueue = %+v, %v", third, err)
	}
	if third.JobID != second.JobID {
		t.Fatalf("queued successor was not replaced: %q want %q", third.JobID, second.JobID)
	}

	old, err := s.GetJob(ctx, first.JobID)
	if err != nil {
		t.Fatal(err)
	}
	var oldPayload map[string]any
	if err := json.Unmarshal(old.Payload, &oldPayload); err != nil {
		t.Fatal(err)
	}
	if oldPayload["head_sha"] != headA || oldPayload["generation"] != float64(1) ||
		old.State != store.JobRunning || old.Attempts != 1 {
		t.Fatalf("running job mutated: %+v payload=%v", old, oldPayload)
	}
	queued, err := s.GetJob(ctx, third.JobID)
	if err != nil {
		t.Fatal(err)
	}
	var queuedPayload map[string]any
	if err := json.Unmarshal(queued.Payload, &queuedPayload); err != nil {
		t.Fatal(err)
	}
	if queuedPayload["head_sha"] != headC || queuedPayload["generation"] != float64(3) {
		t.Fatalf("queued payload = %v", queuedPayload)
	}

	if err := s.Complete(ctx, *running); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	next, err := s.Claim(ctx, "worker-2", []string{reviewRunKind}, time.Hour)
	if err != nil || next == nil {
		t.Fatalf("successor claim = %+v, %v", next, err)
	}
	if next.ID != third.JobID || next.Attempts != 1 {
		t.Fatalf("successor = %+v", next)
	}
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if control.Generation != 3 || control.HeadSHA != headC {
		t.Fatalf("control = %+v", control)
	}
}

func TestCancelWhileReviewRunningStaysObservable(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")

	run, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
	})
	if err != nil {
		t.Fatal(err)
	}
	running, err := s.Claim(ctx, "worker", []string{reviewRunKind}, time.Hour)
	if err != nil || running == nil {
		t.Fatalf("Claim = %+v, %v", running, err)
	}
	cancelled, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, Cancel: true,
	})
	if err != nil || cancelled.Generation != 2 {
		t.Fatalf("cancel = %+v, %v", cancelled, err)
	}
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !control.Cancelled || control.Generation != 2 || control.HeadSHA != head {
		t.Fatalf("control = %+v", control)
	}
	runJob, err := s.GetJob(ctx, run.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if runJob.State != store.JobRunning {
		t.Fatalf("running review was disturbed: %+v", runJob)
	}
	cancelJob, err := s.GetJob(ctx, cancelled.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelJob.Kind != reviewCancelKind || cancelJob.Key != "review.cancel:42:7" ||
		cancelJob.State != store.JobQueued {
		t.Fatalf("cancel job = %+v", cancelJob)
	}
	var payload map[string]any
	if err := json.Unmarshal(cancelJob.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["head_sha"]; ok {
		t.Fatalf("cancel payload carries head: %v", payload)
	}
	if payload["generation"] != float64(2) {
		t.Fatalf("cancel payload generation = %v", payload["generation"])
	}
}

func TestCreateReviewWorkflowRequiresCurrentGeneration(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	headA := reviewHead("a")
	headB := reviewHead("b")

	first, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headA,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headB,
	})
	if err != nil || second.Generation != 2 {
		t.Fatalf("second = %+v, %v", second, err)
	}

	stale := reviewWorkflowRequest(first.JobID, 1, 42, 7, headA)
	if _, _, err := s.CreateReviewWorkflow(ctx, stale); !errors.Is(err, store.ErrSuperseded) {
		t.Fatalf("stale create error = %v, want ErrSuperseded", err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM workflows`); got != 0 {
		t.Fatalf("workflow rows = %d, want 0", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM workflow_dossiers`); got != 0 {
		t.Fatalf("dossier rows = %d, want 0", got)
	}

	current := reviewWorkflowRequest(second.JobID, 2, 42, 7, headB)
	wfID, reused, err := s.CreateReviewWorkflow(ctx, current)
	if err != nil {
		t.Fatalf("CreateReviewWorkflow: %v", err)
	}
	if reused {
		t.Fatal("first create reported reuse")
	}
	wf, err := s.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.HeadSHA != headB || wf.Kind != "review" || wf.Status != "pending" {
		t.Fatalf("workflow = %+v", wf)
	}

	// An identical retry is still idempotent.
	reusedID, reused, err := s.CreateReviewWorkflow(ctx, current)
	if err != nil || !reused || reusedID != wfID {
		t.Fatalf("retry = %q reused=%v err=%v, want %q", reusedID, reused, err, wfID)
	}

	// A generation bump makes the old job request stale even though its
	// workflow row still exists.
	if _, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("c"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateReviewWorkflow(ctx, current); !errors.Is(err, store.ErrSuperseded) {
		t.Fatalf("stale retry error = %v, want ErrSuperseded", err)
	}
	headChanged := reviewWorkflowRequest(second.JobID, 2, 42, 7, reviewHead("c"))
	if _, _, err := s.CreateReviewWorkflow(ctx, headChanged); !errors.Is(err, store.ErrSuperseded) {
		t.Fatalf("head change error = %v, want ErrSuperseded", err)
	}

	meta := workflowCreatedMeta(t, s, wfID)
	if meta["generation"] != "2" || meta["head_sha"] != headB || meta["kind"] != "review" ||
		meta["base_sha"] == "" || meta["started_at"] == "" {
		t.Fatalf("meta = %v", meta)
	}

	dossier, err := s.GetWorkflowDossier(ctx, wfID)
	if err != nil {
		t.Fatal(err)
	}
	if dossier.JobID != second.JobID || dossier.Generation != 2 ||
		dossier.PublicationState != store.WorkflowPublicationPending {
		t.Fatalf("dossier = %+v", dossier)
	}
	if !bytes.Contains(dossier.Data, []byte(headB)) {
		t.Fatalf("dossier data = %s", dossier.Data)
	}
	byJob, err := s.GetWorkflowByJob(ctx, second.JobID)
	if err != nil || byJob.ID != wfID {
		t.Fatalf("GetWorkflowByJob = %+v, %v", byJob, err)
	}
	if _, err := s.GetWorkflowByJob(ctx, "unknown-job"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown job error = %v", err)
	}
}

func workflowCreatedMeta(t *testing.T, s *Store, wfID string) map[string]string {
	t.Helper()
	events, err := s.Load(context.Background(), wfID)
	if err != nil || len(events) == 0 {
		t.Fatalf("Load = %d events, %v", len(events), err)
	}
	var payload struct {
		Meta map[string]string `json:"meta"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	return payload.Meta
}

func TestCreateReviewWorkflowRejectsCancelledControl(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")

	run, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, Cancel: true,
	})
	if err != nil || cancel.Generation != 2 {
		t.Fatalf("cancel = %+v, %v", cancel, err)
	}
	req := reviewWorkflowRequest(run.JobID, 2, 42, 7, head)
	if _, _, err := s.CreateReviewWorkflow(ctx, req); !errors.Is(err, store.ErrSuperseded) {
		t.Fatalf("cancelled create error = %v, want ErrSuperseded", err)
	}
}

func TestCreateReviewWorkflowResumesOrphanDossier(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")

	queued, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
	})
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := s.SaveWorkflowDossier(ctx, store.WorkflowDossier{
		WorkflowID:       "reserved-workflow",
		JobID:            queued.JobID,
		Generation:       1,
		Version:          1,
		Data:             json.RawMessage(`{"partial":true}`),
		PublicationState: store.WorkflowPublicationPending,
	})
	if err != nil {
		t.Fatalf("SaveWorkflowDossier: %v", err)
	}
	if reserved.Hash == "" {
		t.Fatal("hash was not computed")
	}
	if _, err := s.GetWorkflowByJob(ctx, queued.JobID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("orphan job lookup error = %v, want ErrNotFound", err)
	}

	wfID, reused, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(queued.JobID, 1, 42, 7, head))
	if err != nil {
		t.Fatalf("CreateReviewWorkflow: %v", err)
	}
	if reused || wfID != "reserved-workflow" {
		t.Fatalf("create = %q reused=%v, want reserved-workflow", wfID, reused)
	}
	dossier, err := s.GetWorkflowDossier(ctx, wfID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(dossier.Data, []byte(head)) {
		t.Fatalf("dossier was not replaced: %s", dossier.Data)
	}
}

func TestReviewStateSurvivesReopenAndPublicationScan(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	path := filepath.Join(t.TempDir(), "review.db")
	s, err := Open(ctx, path, WithClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	mustProject(t, s, 42)
	head := reviewHead("a")
	queued, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
	})
	if err != nil {
		t.Fatal(err)
	}
	wfID, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(queued.JobID, 1, 42, 7, head))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(ctx, path, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil || control.Generation != 1 {
		t.Fatalf("control after reopen = %+v, %v", control, err)
	}
	dossier, err := s.GetWorkflowDossier(ctx, wfID)
	if err != nil || dossier.JobID != queued.JobID {
		t.Fatalf("dossier after reopen = %+v, %v", dossier, err)
	}
	byJob, err := s.GetWorkflowByJob(ctx, queued.JobID)
	if err != nil || byJob.ID != wfID {
		t.Fatalf("job link after reopen = %+v, %v", byJob, err)
	}

	// Pending workflows are recoverable by the engine, not publishable.
	unfinished, err := s.ListUnfinished(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfinished) != 1 || unfinished[0] != wfID {
		t.Fatalf("unfinished = %v", unfinished)
	}
	pending, err := s.ListPendingPublications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending publications before completion = %v", pending)
	}

	rev, err := s.Load(ctx, wfID)
	if err != nil {
		t.Fatal(err)
	}
	_ = finishWorkflow(t, s, wfID, int64(len(rev)), clock.Now())
	unfinished, err = s.ListUnfinished(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfinished) != 0 {
		t.Fatalf("completed workflow still unfinished: %v", unfinished)
	}
	pending, err = s.ListPendingPublications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].WorkflowID != wfID ||
		pending[0].JobID != queued.JobID || pending[0].Generation != 1 ||
		pending[0].ProjectID != 42 || pending[0].MRIID != 7 || pending[0].HeadSHA != head ||
		pending[0].Status != string(dag.WorkflowCompleted) {
		t.Fatalf("pending publication = %+v", pending)
	}
	if err := s.SetWorkflowPublicationState(ctx, wfID, store.WorkflowPublicationPublished, ""); err != nil {
		t.Fatalf("SetWorkflowPublicationState: %v", err)
	}
	pending, err = s.ListPendingPublications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("published workflow still pending: %v", pending)
	}
	if err := s.SetWorkflowPublicationState(ctx, "missing", store.WorkflowPublicationPublished, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown workflow error = %v", err)
	}
	if err := s.SetWorkflowPublicationState(ctx, wfID, "bogus", ""); err == nil {
		t.Fatal("invalid state was accepted")
	}
}

func TestWorkflowDossierValidationAndStorage(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())

	invalid := []struct {
		name    string
		dossier store.WorkflowDossier
	}{
		{name: "missing id", dossier: store.WorkflowDossier{Version: 1, Data: json.RawMessage(`{}`)}},
		{name: "missing version", dossier: store.WorkflowDossier{WorkflowID: "wf", Data: json.RawMessage(`{}`)}},
		{name: "bad json", dossier: store.WorkflowDossier{WorkflowID: "wf", Version: 1, Data: json.RawMessage(`{`)}},
		{name: "empty data", dossier: store.WorkflowDossier{WorkflowID: "wf", Version: 1}},
		{
			name: "oversized",
			dossier: store.WorkflowDossier{
				WorkflowID: "wf", Version: 1,
				Data: json.RawMessage(`{"pad":"` + strings.Repeat("x", maxWorkflowDossierBytes) + `"}`),
			},
		},
		{
			name: "hash mismatch",
			dossier: store.WorkflowDossier{
				WorkflowID: "wf", Version: 1, Data: json.RawMessage(`{"a":1}`),
				Hash: strings.Repeat("0", 64),
			},
		},
		{
			name: "bad state",
			dossier: store.WorkflowDossier{
				WorkflowID: "wf", Version: 1, Data: json.RawMessage(`{}`),
				PublicationState: "unknown",
			},
		},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.SaveWorkflowDossier(ctx, tt.dossier); err == nil {
				t.Fatal("expected error")
			}
			if got := rowCount(t, s, `SELECT COUNT(*) FROM workflow_dossiers`); got != 0 {
				t.Fatalf("dossier rows = %d, want 0", got)
			}
		})
	}

	// The store cannot detect secrets; the caller must sanitize Data. This
	// payload is what a sanitizer would leave behind.
	data := json.RawMessage(`{"token":"[redacted]","head_sha":"` + reviewHead("a") + `","root":"/workspace/repo","files":["a.go"]}`)
	saved, err := s.SaveWorkflowDossier(ctx, store.WorkflowDossier{
		WorkflowID: "wf-1", JobID: "job-1", Generation: 3, Version: 2, Data: data,
	})
	if err != nil {
		t.Fatalf("SaveWorkflowDossier: %v", err)
	}
	if saved.PublicationState != store.WorkflowPublicationPending || saved.Hash == "" {
		t.Fatalf("saved = %+v", saved)
	}
	stored, err := s.GetWorkflowDossier(ctx, "wf-1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored.Data, data) || stored.Generation != 3 || stored.Version != 2 ||
		stored.JobID != "job-1" || stored.Hash != saved.Hash {
		t.Fatalf("round trip = %+v", stored)
	}

	// Raw bytes are gzip, not plain JSON, so the dossier is not duplicated in
	// the database page cache/log as cleartext.
	var raw []byte
	if err := s.db.QueryRowContext(ctx, `SELECT data FROM workflow_dossiers WHERE workflow_id = 'wf-1'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		t.Fatalf("dossier is not gzip: %x", raw[:2])
	}

	if _, err := s.SaveWorkflowDossier(ctx, store.WorkflowDossier{
		WorkflowID: "wf-2", JobID: "job-1", Version: 1, Data: json.RawMessage(`{}`),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate job error = %v, want ErrConflict", err)
	}
	if _, err := s.GetWorkflowDossier(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing dossier error = %v", err)
	}
}
func TestDetachWorkflowJobRebindsOnceConcurrently(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")

	queued, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
	})
	if err != nil {
		t.Fatal(err)
	}
	original, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(queued.JobID, 1, 42, 7, head))
	if err != nil {
		t.Fatal(err)
	}

	const workers = 8
	type outcome struct {
		detached  bool
		bound     bool
		detachErr error
		bindErr   error
	}
	results := make([]outcome, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			err := s.DetachWorkflowJob(ctx, original, queued.JobID)
			switch {
			case err == nil:
				results[i].detached = true
			case errors.Is(err, store.ErrNotFound):
			default:
				results[i].detachErr = err
				return
			}
			_, err = s.SaveWorkflowDossier(ctx, store.WorkflowDossier{
				WorkflowID: fmt.Sprintf("replacement-%d", i),
				JobID:      queued.JobID,
				Generation: 1,
				Version:    1,
				Data:       json.RawMessage(`{"replacement":true}`),
			})
			switch {
			case err == nil:
				results[i].bound = true
			case errors.Is(err, store.ErrConflict):
			default:
				results[i].bindErr = err
			}
		}(i)
	}
	close(start)
	wg.Wait()

	detached, bound := 0, 0
	for i, result := range results {
		if result.detachErr != nil {
			t.Errorf("worker %d detach: %v", i, result.detachErr)
		}
		if result.bindErr != nil {
			t.Errorf("worker %d bind: %v", i, result.bindErr)
		}
		if result.detached {
			detached++
		}
		if result.bound {
			bound++
		}
	}
	if detached != 1 {
		t.Fatalf("detaches = %d, want exactly 1", detached)
	}
	if bound != 1 {
		t.Fatalf("binds = %d, want exactly 1", bound)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM workflow_dossiers WHERE job_id = ?`, queued.JobID); got != 1 {
		t.Fatalf("dossiers bound to the job = %d, want 1", got)
	}
	mapped, err := s.dossierWorkflowIDByJob(ctx, queued.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if mapped == "" || mapped == original {
		t.Fatalf("mapped workflow = %q, want a replacement", mapped)
	}
	old, err := s.GetWorkflowDossier(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	if old.JobID != "" || old.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("old dossier = job %q state %q, want detached and skipped", old.JobID, old.PublicationState)
	}
}
