package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// This uses the real command and review handlers, then removes only the 0020
// tables to model the state of an installation being upgraded from 0019.
func TestPrunedCommandAndRunDossierRedeliveryAfterUpgrade(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, sha40)
	db, path := openTestStorePath(t)
	cipher := newTestCipher(t)
	seedProject(t, db, cipher, 42, "whsec", "{}", "default")
	f.db = db
	f.engine = dag.NewEngine(db, f.executors, f.executors, dag.Options{})
	f.service.store, f.service.engine = db, f.engine
	f.publisher.store, f.publisher.engine = db, f.engine
	webhook, err := NewWebhookService(WebhookServiceConfig{
		Store: db, Workspace: f.manager, Cipher: cipher, Profiles: f.profiles, ReviewDefaults: f.defaults,
	})
	if err != nil {
		t.Fatal(err)
	}
	recording := &recordingCommandStore{CommandStore: db}
	worker, err := NewCommandWorker(CommandWorkerConfig{
		Store: recording, GitLab: defaultFakeGitLab(), Cipher: cipher, Profiles: f.profiles, Workspace: f.manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	deliver := func(noteID int64, text string) *store.Job {
		t.Helper()
		result, err := webhook.Handle(ctx, WebhookRequest{
			Event: "Note Hook", Token: "whsec", UUID: uuid.NewString(),
			Body: noteBody(42, 7, noteID, 12, "alice", text),
		})
		if err != nil || !result.Accepted {
			t.Fatalf("deliver %s: %+v, %v", text, result, err)
		}
		job, err := db.Claim(ctx, "command", []string{"review.command"}, time.Minute)
		if err != nil || job == nil || job.ID != result.JobID {
			t.Fatalf("claim %s: %+v, %v", text, job, err)
		}
		if err := worker.Handle(ctx, job); err != nil {
			t.Fatalf("handle %s: %v", text, err)
		}
		if err := db.Complete(ctx, *job); err != nil {
			t.Fatalf("complete %s: %v", text, err)
		}
		return job
	}
	command := deliver(555, "/review")
	run, err := db.Claim(ctx, "review", []string{"review.run"}, time.Minute)
	if err != nil || run == nil {
		t.Fatalf("claim run: %+v, %v", run, err)
	}
	if err := f.service.RunReview(ctx, run); err != nil {
		t.Fatalf("run review: %v", err)
	}
	workflow, err := db.GetWorkflowByJob(ctx, run.ID)
	if err != nil {
		t.Fatalf("workflow: %v", err)
	}
	dossier, err := db.GetWorkflowDossier(ctx, workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot review.Dossier
	if err := json.Unmarshal(dossier.Data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if workflow.Kind != "review" || workflow.Trigger != "command" || workflow.ProjectID != 42 ||
		workflow.MRIID == nil || *workflow.MRIID != 7 || snapshot.Project.ID != 42 || snapshot.MR.IID != 7 ||
		snapshot.Metadata["trigger"] != "command" || snapshot.Metadata["note_id"] != "555" {
		t.Fatalf("handler did not persist command provenance: workflow=%+v snapshot=%+v", workflow, snapshot.Metadata)
	}
	if err := db.Complete(ctx, *run); err != nil {
		t.Fatalf("complete run: %v", err)
	}
	deliver(556, "/review cancel")
	if len(recording.ids) != 2 || recording.dup[0] || recording.dup[1] {
		t.Fatalf("initial commands: ids=%v duplicates=%v", recording.ids, recording.dup)
	}
	cancelID := recording.ids[1]
	beforeCancel, err := db.GetJob(ctx, cancelID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeleteTerminalJobsBefore(ctx, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("production job retention: %v", err)
	}
	for _, id := range []string{command.ID, run.ID} {
		if _, err := db.GetJob(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("job %s survived retention: %v", id, err)
		}
	}
	if _, err := db.GetWorkflowDossier(ctx, workflow.ID); err != nil {
		t.Fatalf("dossier did not survive retention: %v", err)
	}
	beforeControl, err := db.GetReviewControl(ctx, 42, 7)
	if err != nil || !beforeControl.Cancelled || beforeControl.Generation != 2 {
		t.Fatalf("initial cancel: %+v, %v", beforeControl, err)
	}
	beforeStatus, err := db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	beforeReaction, err := db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil {
		t.Fatal(err)
	}
	beforeCancelReaction, err := db.GetReviewReaction(ctx, 42, 7, 556)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`DROP TABLE review_command_replays;
		DROP TABLE review_command_quarantine;
		DROP TABLE review_command_replay_backfill;
		DELETE FROM schema_migrations WHERE version = 20`); err != nil {
		t.Fatalf("model 0019: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	webhook.store = db
	recording = &recordingCommandStore{CommandStore: db}
	worker.store = recording
	deliver(555, "/review")
	if len(recording.dup) != 1 || !recording.dup[0] || recording.ids[0] != run.ID {
		t.Errorf("redelivery enqueue = ids %v, duplicates %v; want duplicate of %s", recording.ids, recording.dup, run.ID)
	}
	afterControl, err := db.GetReviewControl(ctx, 42, 7)
	if err != nil || !reflect.DeepEqual(beforeControl, afterControl) {
		t.Errorf("control changed: before=%+v after=%+v, %v", beforeControl, afterControl, err)
	}
	afterStatus, err := db.GetReviewStatus(ctx, 42, 7)
	if err != nil || !reflect.DeepEqual(beforeStatus, afterStatus) {
		t.Errorf("status changed: before=%+v after=%+v, %v", beforeStatus, afterStatus, err)
	}
	afterReaction, err := db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil || !reflect.DeepEqual(beforeReaction, afterReaction) {
		t.Errorf("reaction changed: before=%+v after=%+v, %v", beforeReaction, afterReaction, err)
	}
	afterCancelReaction, err := db.GetReviewReaction(ctx, 42, 7, 556)
	if err != nil || !reflect.DeepEqual(beforeCancelReaction, afterCancelReaction) {
		t.Errorf("cancel reaction changed: before=%+v after=%+v, %v", beforeCancelReaction, afterCancelReaction, err)
	}
	afterCancel, err := db.GetJob(ctx, cancelID)
	if err != nil || !reflect.DeepEqual(beforeCancel, afterCancel) {
		t.Errorf("cancel job changed: before=%+v after=%+v, %v", beforeCancel, afterCancel, err)
	}
	if job, err := db.Claim(ctx, "unexpected-run", []string{"review.run"}, time.Minute); err != nil || job != nil {
		t.Errorf("unexpected run: %+v, %v", job, err)
	}
}

func TestReplacedRetryDossierRedeliveryAfterUpgrade(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, sha40)
	db, path := openTestStorePath(t)
	cipher := newTestCipher(t)
	seedProject(t, db, cipher, 42, "whsec", "{}", "default")
	f.db = db
	f.engine = dag.NewEngine(db, f.executors, f.executors, dag.Options{})
	f.service.store, f.service.engine = db, f.engine
	f.publisher.store, f.publisher.engine = db, f.engine
	webhook, err := NewWebhookService(WebhookServiceConfig{
		Store: db, Workspace: f.manager, Cipher: cipher, Profiles: f.profiles, ReviewDefaults: f.defaults,
	})
	if err != nil {
		t.Fatal(err)
	}
	recording := &recordingCommandStore{CommandStore: db}
	worker, err := NewCommandWorker(CommandWorkerConfig{
		Store: recording, GitLab: defaultFakeGitLab(), Cipher: cipher, Profiles: f.profiles, Workspace: f.manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	deliver := func(noteID int64, text string) string {
		t.Helper()
		result, err := webhook.Handle(ctx, WebhookRequest{
			Event: "Note Hook", Token: "whsec", UUID: uuid.NewString(),
			Body: noteBody(42, 7, noteID, 12, "alice", text),
		})
		if err != nil || !result.Accepted {
			t.Fatalf("deliver %d: %+v, %v", noteID, result, err)
		}
		job, err := db.Claim(ctx, "command", []string{"review.command"}, time.Minute)
		if err != nil || job == nil || job.ID != result.JobID {
			t.Fatalf("claim command %d: %+v, %v", noteID, job, err)
		}
		if err := worker.Handle(ctx, job); err != nil {
			t.Fatalf("handle command %d: %v", noteID, err)
		}
		if err := db.Complete(ctx, *job); err != nil {
			t.Fatalf("complete command %d: %v", noteID, err)
		}
		return job.ID
	}

	commandA := deliver(555, "/review")
	run, err := db.Claim(ctx, "review", []string{"review.run"}, time.Minute)
	if err != nil || run == nil {
		t.Fatalf("claim run: %+v, %v", run, err)
	}
	f.gitlab.noteErr = errors.New("temporary publication failure")
	runErr := f.service.RunReview(ctx, run)
	if runErr == nil || IsPermanentJob(runErr) {
		t.Fatalf("review error = %v, want retryable publication failure", runErr)
	}
	workflow, err := db.GetWorkflowByJob(ctx, run.ID)
	if err != nil || workflow.Status != string(dag.WorkflowCompleted) ||
		workflow.PublicationState != store.WorkflowPublicationPending || workflow.PublicationError != publicationRetryReason {
		t.Fatalf("completed pending workflow = %+v, %v", workflow, err)
	}
	dossier, err := db.GetWorkflowDossier(ctx, workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot review.Dossier
	if err := json.Unmarshal(dossier.Data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if dossier.Generation != 1 || dossier.JobID != run.ID || snapshot.Metadata["note_id"] != "555" || snapshot.Metadata["trigger"] != "command" {
		t.Fatalf("original dossier = %+v, metadata=%v", dossier, snapshot.Metadata)
	}
	retryAt := time.Now().Add(time.Hour)
	if err := db.Fail(ctx, *run, runErr, &retryAt); err != nil {
		t.Fatalf("schedule failed run retry: %v", err)
	}
	requeued, err := db.GetJob(ctx, run.ID)
	if err != nil || requeued.State != store.JobQueued || requeued.Attempts != run.Attempts {
		t.Fatalf("requeued run = %+v, %v", requeued, err)
	}
	f.gitlab.noteErr = nil
	commandB := deliver(556, "/review")
	if len(recording.ids) != 2 || recording.ids[0] != run.ID || recording.ids[1] != run.ID || recording.dup[1] {
		t.Fatalf("replacement enqueues = ids %v, duplicates %v", recording.ids, recording.dup)
	}
	replaced, err := db.GetJob(ctx, run.ID)
	if err != nil || replaced.State != store.JobQueued || replaced.Attempts != run.Attempts {
		t.Fatalf("replaced job = %+v, %v", replaced, err)
	}
	var payload reviewRunPayload
	if err := json.Unmarshal(replaced.Payload, &payload); err != nil || payload.NoteID != 556 || payload.Generation != 2 {
		t.Fatalf("replacement payload = %+v, %v", payload, err)
	}
	mapped, err := db.GetWorkflowByJob(ctx, run.ID)
	if err != nil || mapped.ID != workflow.ID {
		t.Fatalf("old workflow mapping after replacement = %+v, %v", mapped, err)
	}
	commandC := deliver(557, "/review cancel")
	beforeControl, err := db.GetReviewControl(ctx, 42, 7)
	if err != nil || beforeControl.Generation != 3 || !beforeControl.Cancelled {
		t.Fatalf("cancellation = %+v, %v", beforeControl, err)
	}
	beforeStatus, err := db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	beforeReactions := make(map[int64]*store.ReviewReaction)
	for _, noteID := range []int64{555, 556, 557} {
		beforeReactions[noteID], err = db.GetReviewReaction(ctx, 42, 7, noteID)
		if err != nil {
			t.Fatal(err)
		}
	}
	cancelID := recording.ids[2]
	beforeCancel, err := db.GetJob(ctx, cancelID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeleteTerminalJobsBefore(ctx, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("prune terminal commands: %v", err)
	}
	for _, id := range []string{commandA, commandB, commandC} {
		if _, err := db.GetJob(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("terminal command %s survived retention: %v", id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`DROP TABLE review_command_replays;
		DROP TABLE review_command_quarantine;
		DROP TABLE review_command_replay_backfill;
		DELETE FROM schema_migrations WHERE version = 20`); err != nil {
		t.Fatalf("model 0019: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	webhook.store = db
	recording = &recordingCommandStore{CommandStore: db}
	worker.store = recording
	deliver(555, "/review")
	if len(recording.dup) != 1 || !recording.dup[0] || recording.ids[0] != run.ID {
		t.Errorf("redelivery enqueue = ids %v, duplicates %v; want duplicate of %s", recording.ids, recording.dup, run.ID)
	}
	deliver(556, "/review")
	if len(recording.dup) != 2 || !recording.dup[1] || recording.ids[1] != run.ID {
		t.Errorf("replacement redelivery = ids %v, duplicates %v; want duplicate of %s", recording.ids, recording.dup, run.ID)
	}
	afterControl, err := db.GetReviewControl(ctx, 42, 7)
	if err != nil || !reflect.DeepEqual(beforeControl, afterControl) {
		t.Errorf("redelivery changed control: before=%+v after=%+v, %v", beforeControl, afterControl, err)
	}
	afterStatus, err := db.GetReviewStatus(ctx, 42, 7)
	if err != nil || !reflect.DeepEqual(beforeStatus, afterStatus) {
		t.Errorf("redelivery changed status: before=%+v after=%+v, %v", beforeStatus, afterStatus, err)
	}
	for noteID, before := range beforeReactions {
		after, err := db.GetReviewReaction(ctx, 42, 7, noteID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Errorf("redelivery changed reaction %d: before=%+v after=%+v, %v", noteID, before, after, err)
		}
	}
	for id, before := range map[string]*store.Job{run.ID: replaced, cancelID: beforeCancel} {
		after, err := db.GetJob(ctx, id)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Errorf("redelivery changed job %s: before=%+v after=%+v, %v", id, before, after, err)
		}
	}
}
