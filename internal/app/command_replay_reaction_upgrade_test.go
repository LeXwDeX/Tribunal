package app

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// A transient side-effect write failure leaves eyes for the next cancel to clear.
type failEmptyReactionStore struct{ ReviewStore }

func (s failEmptyReactionStore) SetReviewReaction(ctx context.Context, goal store.ReviewReactionGoal) (bool, error) {
	if goal.NoteID == 555 && goal.Desired == store.ReviewReactionPass {
		return false, errors.New("temporary reaction write failure")
	}
	return s.ReviewStore.SetReviewReaction(ctx, goal)
}

// Model 0019 by removing only 0020's tables after exercising the real handlers.
func TestPrunedEmptyReviewReactionRedeliveryAfterUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name         string
		failReaction bool
		wantReaction string
	}{
		{"successful reaction", false, store.ReviewReactionPass},
		{"failed reaction cleared by cancel", true, store.ReviewReactionNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, sha40)
			db, path := openTestStorePath(t)
			cipher := newTestCipher(t)
			seedProject(t, db, cipher, 42, "whsec", "{}", "default")
			f.db = db
			f.engine = dag.NewEngine(db, f.executors, f.executors, dag.Options{})
			f.service.store, f.service.engine = db, f.engine
			f.publisher.store, f.publisher.engine = db, f.engine
			f.gitlab.diffs = []gitlab.FileDiff{{NewPath: "go.sum", Diff: "@@ -1 +1 @@\n-a\n+b\n"}}
			f.defaults.Ignore = []string{"**/*.sum"}
			f.service.reviewDefaults = f.defaults
			webhook, err := NewWebhookService(WebhookServiceConfig{Store: db, Workspace: f.manager, Cipher: cipher, Profiles: f.profiles, ReviewDefaults: f.defaults})
			if err != nil {
				t.Fatal(err)
			}
			api := defaultFakeGitLab()
			recording := &recordingCommandStore{CommandStore: db}
			worker, err := NewCommandWorker(CommandWorkerConfig{Store: recording, GitLab: api, Cipher: cipher, Profiles: f.profiles, Workspace: f.manager})
			if err != nil {
				t.Fatal(err)
			}
			deliver := func(noteID int64, text string) *store.Job {
				t.Helper()
				result, err := webhook.Handle(ctx, WebhookRequest{Event: "Note Hook", Token: "whsec", UUID: uuid.NewString(), Body: noteBody(42, 7, noteID, 12, "alice", text)})
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
					t.Fatal(err)
				}
				return job
			}
			command := deliver(555, "/review")
			run, err := db.Claim(ctx, "review", []string{"review.run"}, time.Minute)
			if err != nil || run == nil {
				t.Fatalf("claim run: %+v, %v", run, err)
			}
			if tc.failReaction {
				f.service.store = failEmptyReactionStore{db}
			}
			if err := f.service.RunReview(ctx, run); err != nil {
				t.Fatalf("empty review: %v", err)
			}
			f.service.store = db
			if _, err := db.GetWorkflowByJob(ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("empty review unexpectedly has workflow/dossier: %v", err)
			}
			if f.gitlab.noteCount() != 1 || !strings.Contains(f.gitlab.notes[0].Body, review.SummaryMarkerPrefix) {
				t.Fatal("empty review did not publish its summary")
			}
			if err := db.Complete(ctx, *run); err != nil {
				t.Fatal(err)
			}
			if _, err := db.DeleteTerminalJobsBefore(ctx, time.Now().Add(time.Minute)); err != nil {
				t.Fatalf("production retention: %v", err)
			}
			for _, id := range []string{command.ID, run.ID} {
				if _, err := db.GetJob(ctx, id); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("terminal job %s survived retention: %v", id, err)
				}
			}
			deliver(556, "/review cancel")
			cancel, err := db.Claim(ctx, "cancel", []string{"review.cancel"}, time.Minute)
			if err != nil || cancel == nil {
				t.Fatalf("claim cancel: %+v, %v", cancel, err)
			}
			if err := f.service.CancelReview(ctx, cancel); err != nil {
				t.Fatalf("cancel: %v", err)
			}
			if err := db.Complete(ctx, *cancel); err != nil {
				t.Fatal(err)
			}
			beforeControl, err := db.GetReviewControl(ctx, 42, 7)
			if err != nil || !beforeControl.Cancelled || beforeControl.Generation != 2 {
				t.Fatalf("cancel control: %+v, %v", beforeControl, err)
			}
			beforeStatus, _ := db.GetReviewStatus(ctx, 42, 7)
			beforeReaction, err := db.GetReviewReaction(ctx, 42, 7, 555)
			if err != nil || beforeReaction.Desired != tc.wantReaction {
				t.Fatalf("retained reaction: %+v, %v; want %s", beforeReaction, err, tc.wantReaction)
			}
			beforeCancelReaction, _ := db.GetReviewReaction(ctx, 42, 7, 556)
			beforeCancel, _ := db.GetJob(ctx, cancel.ID)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			legacy, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			var workflows, dossiers int
			if err := legacy.QueryRow(`SELECT (SELECT COUNT(*) FROM workflows), (SELECT COUNT(*) FROM workflow_dossiers)`).Scan(&workflows, &dossiers); err != nil || workflows != 0 || dossiers != 0 {
				t.Fatalf("empty review left workflows=%d dossiers=%d: %v", workflows, dossiers, err)
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
			// This opposite command was never issued before the upgrade. The
			// retained reaction cannot distinguish it from a pruned old cancel.
			deliver(555, "/review cancel")
			if len(recording.ids) != 0 {
				t.Errorf("unproven reaction was accepted or reported duplicate: ids=%v duplicates=%v", recording.ids, recording.dup)
			}
			if notes := api.noteSnapshot(); len(notes) != 1 || !strings.Contains(notes[0].Body, "kind=uncertain") || !strings.Contains(notes[0].Body, "新的") {
				t.Errorf("missing actionable uncertainty reply: %+v", notes)
			}
			afterControl, _ := db.GetReviewControl(ctx, 42, 7)
			afterStatus, _ := db.GetReviewStatus(ctx, 42, 7)
			afterReaction, _ := db.GetReviewReaction(ctx, 42, 7, 555)
			afterCancelReaction, _ := db.GetReviewReaction(ctx, 42, 7, 556)
			afterCancel, _ := db.GetJob(ctx, cancel.ID)
			if !reflect.DeepEqual(beforeControl, afterControl) || !reflect.DeepEqual(beforeStatus, afterStatus) ||
				!reflect.DeepEqual(beforeReaction, afterReaction) || !reflect.DeepEqual(beforeCancelReaction, afterCancelReaction) || !reflect.DeepEqual(beforeCancel, afterCancel) {
				t.Errorf("old redelivery changed cancellation, status, reactions, or cancel job: control %+v -> %+v", beforeControl, afterControl)
			}
			if job, err := db.Claim(ctx, "unexpected", []string{"review.run"}, time.Minute); err != nil || job != nil {
				t.Errorf("old redelivery created run: %+v, %v", job, err)
			}
			deliver(777, "/review")
			fresh, err := db.GetReviewControl(ctx, 42, 7)
			if err != nil || fresh.Cancelled || fresh.Generation != 3 || len(recording.ids) != 1 || recording.dup[0] {
				t.Errorf("new note did not run: control=%+v enqueues=%v duplicates=%v err=%v", fresh, recording.ids, recording.dup, err)
			}
		})
	}
}
