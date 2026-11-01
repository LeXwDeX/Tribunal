package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

// A GitLab note ID is an int64. The command path must keep it exact after the
// store rewrites the job payload, otherwise a reaction targets another note.
func TestNoteCommandLargeSourceNoteIDPersistsExactly(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	cipher := newTestCipher(t)
	seedProject(t, db, cipher, 42, "whsec", "{}", "default")
	manager := workspace.NewManager(t.TempDir(), nil)
	webhook, err := NewWebhookService(WebhookServiceConfig{
		Store: db, Workspace: manager, Cipher: cipher, Profiles: testProfiles(), ReviewDefaults: testDefaults(),
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewCommandWorker(CommandWorkerConfig{
		Store: db, GitLab: defaultFakeGitLab(), Cipher: cipher, Profiles: testProfiles(), Workspace: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimCommand := func(sourceNote int64, note string) {
		t.Helper()
		result, err := webhook.Handle(ctx, WebhookRequest{
			Event: "Note Hook", Token: "whsec", UUID: uuid.NewString(),
			Body: noteBody(42, 7, sourceNote, 12, "alice", note),
		})
		if err != nil || !result.Accepted {
			t.Fatalf("%q webhook = %+v, %v", note, result, err)
		}
		command, err := db.Claim(ctx, "command", []string{"review.command"}, time.Minute)
		if err != nil || command == nil || command.ID != result.JobID {
			t.Fatalf("claim %q: %+v, %v", note, command, err)
		}
		if err := worker.Handle(ctx, &jobs.Job{
			ID: command.ID, Kind: command.Kind, Payload: command.Payload, MaxAttempts: command.MaxAttempts,
		}); err != nil {
			t.Fatalf("handle %q: %v", note, err)
		}
		if err := db.Complete(ctx, *command); err != nil {
			t.Fatal(err)
		}
	}
	const runNote = int64(1<<53 + 1)
	claimCommand(runNote, "/review")
	run, err := db.Claim(ctx, "review", []string{"review.run"}, time.Minute)
	if err != nil || run == nil {
		t.Fatalf("claim review.run: %+v, %v", run, err)
	}
	var runPayload reviewRunPayload
	if err := json.Unmarshal(run.Payload, &runPayload); err != nil {
		t.Fatal(err)
	}
	if runPayload.NoteID != runNote {
		t.Errorf("review.run note_id = %d, want %d (payload %s)", runPayload.NoteID, runNote, run.Payload)
	}
	reaction, err := db.GetReviewReaction(ctx, 42, 7, runNote)
	if err != nil {
		t.Fatalf("get source reaction: %v", err)
	}
	if reaction.NoteID != runNote || reaction.Desired != "eyes" {
		t.Errorf("source reaction = %+v, want exact note %d", reaction, runNote)
	}

	const cancelNote = int64(1<<53 + 3)
	claimCommand(cancelNote, "/review cancel")
	cancel, err := db.Claim(ctx, "review", []string{"review.cancel"}, time.Minute)
	if err != nil || cancel == nil {
		t.Fatalf("claim review.cancel: %+v, %v", cancel, err)
	}
	var cancelPayload reviewCancelPayload
	if err := json.Unmarshal(cancel.Payload, &cancelPayload); err != nil {
		t.Fatal(err)
	}
	if cancelPayload.NoteID != cancelNote {
		t.Errorf("review.cancel note_id = %d, want %d (payload %s)", cancelPayload.NoteID, cancelNote, cancel.Payload)
	}
	cancelReaction, err := db.GetReviewReaction(ctx, 42, 7, cancelNote)
	if err != nil {
		t.Fatalf("get cancel reaction: %v", err)
	}
	if cancelReaction.NoteID != cancelNote || cancelReaction.Desired != "eyes" {
		t.Errorf("cancel reaction = %+v, want exact note %d", cancelReaction, cancelNote)
	}
	runReaction, err := db.GetReviewReaction(ctx, 42, 7, runNote)
	if err != nil {
		t.Fatalf("get cleared run reaction: %v", err)
	}
	if runReaction.Desired != "none" {
		t.Errorf("run reaction = %+v, want cleared", runReaction)
	}
}
