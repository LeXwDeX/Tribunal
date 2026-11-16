package sqlite

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// TestWorkflowMetaAllowsPersistedTriggers guards the stored trigger allow-list.
// Every trigger a workflow can persist must be accepted: review runs carry the
// ingress trigger (mr_open, mr_update, command, api, catch_up, mention) and
// onboarding workflows carry register (#485).
func TestWorkflowMetaAllowsPersistedTriggers(t *testing.T) {
	cases := []struct {
		trigger string
		kind    string
		mrIID   string
	}{
		{trigger: "mr_open", kind: "review", mrIID: "7"},
		{trigger: "mr_update", kind: "review", mrIID: "7"},
		{trigger: "command", kind: "review", mrIID: "7"},
		{trigger: "api", kind: "review", mrIID: "7"},
		{trigger: "catch_up", kind: "review", mrIID: "7"},
		{trigger: "mention", kind: "review", mrIID: "7"},
		{trigger: "register", kind: "onboarding"},
	}
	for _, tc := range cases {
		t.Run(tc.trigger, func(t *testing.T) {
			meta := map[string]string{
				"project_id": "42",
				"kind":       tc.kind,
				"trigger":    tc.trigger,
			}
			if tc.mrIID != "" {
				meta["mr_iid"] = tc.mrIID
			}
			if _, err := workflowMetaFromView(dag.WorkflowView{Meta: meta}, "wf-"+tc.trigger); err != nil {
				t.Fatalf("trigger %q rejected: %v", tc.trigger, err)
			}
		})
	}
	// The allow-list still rejects unknown values.
	if _, err := workflowMetaFromView(dag.WorkflowView{Meta: map[string]string{
		"project_id": "42", "kind": "review", "mr_iid": "7", "trigger": "made_up",
	}}, "wf-unknown"); err == nil {
		t.Fatal("unknown trigger was accepted")
	}
}

// TestCreateReviewWorkflowAcceptsMentionTrigger reproduces #485: a review run
// queued from a note mention persists metadata trigger=mention. The storage
// allow-list used to reject it, failing every mention-triggered generation.
func TestCreateReviewWorkflowAcceptsMentionTrigger(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")

	run, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
		Payload:     json.RawMessage(`{"trigger":"mention","note_id":555}`),
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest: %v", err)
	}
	req := reviewWorkflowRequest(run.JobID, run.Generation, 42, 7, head)
	req.Meta["trigger"] = "mention"
	wfID, reused, err := s.CreateReviewWorkflow(ctx, req)
	if err != nil {
		t.Fatalf("CreateReviewWorkflow with mention trigger: %v", err)
	}
	if reused {
		t.Fatalf("workflow was reported reused")
	}
	wf, err := s.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if wf.Trigger != "mention" {
		t.Fatalf("stored trigger = %q, want mention", wf.Trigger)
	}
}

// TestReviewStatusPreservesNoteIDAcrossGenerations reproduces the receipt
// defect of #485: advancing the generation kept one status note on GitLab but
// cleared its durable id, so the mention thread receipt omitted the permalink
// until the next status job ran.
func TestReviewStatusPreservesNoteIDAcrossGenerations(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")

	first, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
		Payload: json.RawMessage(`{"trigger":"command","note_id":555}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	queued := mustReviewStatus(t, s, 42, 7)
	if err := s.MarkReviewStatusApplied(ctx, 42, 7, int64(queued.Version), 8801, "hash", ""); err != nil {
		t.Fatalf("MarkReviewStatusApplied: %v", err)
	}

	// A state change in the same generation keeps the note.
	if written, err := s.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: first.Generation, Attempt: 1,
		HeadSHA: head, State: store.ReviewStatusRunning,
	}); err != nil || !written {
		t.Fatalf("SetReviewStatus running = %v, %v", written, err)
	}
	if row := mustReviewStatus(t, s, 42, 7); row.NoteID != 8801 {
		t.Fatalf("note id after state change = %d, want 8801", row.NoteID)
	}

	// A successor generation writes its own queued row before any status job
	// runs; the single status note id must survive that window.
	second, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
		Payload: json.RawMessage(`{"trigger":"command","note_id":556}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation <= first.Generation {
		t.Fatalf("generation did not advance: %d -> %d", first.Generation, second.Generation)
	}
	row := mustReviewStatus(t, s, 42, 7)
	if row.Generation != second.Generation || row.NoteID != 8801 {
		t.Fatalf("row after generation advance = %+v, want generation %d note 8801", row, second.Generation)
	}

	// Only a status write that creates or recreates the note replaces the id.
	if err := s.MarkReviewStatusApplied(ctx, 42, 7, int64(row.Version), 8802, "new-hash", ""); err != nil {
		t.Fatalf("MarkReviewStatusApplied replacement: %v", err)
	}
	if replaced := mustReviewStatus(t, s, 42, 7); replaced.NoteID != 8802 {
		t.Fatalf("note id after recreation = %d, want 8802", replaced.NoteID)
	}
}
