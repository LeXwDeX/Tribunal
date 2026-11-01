package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

func TestLegacyQuarantineWorkerRecoveryAndNewNote(t *testing.T) {
	for _, kind := range []string{"run", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "legacy.db")
			legacy, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer legacy.Close()
			if _, err := legacy.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			const stamp = "2026-01-01T00:00:00.000000000Z"
			entries, err := migrations.FS.ReadDir(".")
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
					continue
				}
				version, err := strconv.Atoi(entry.Name()[:4])
				if err != nil {
					t.Fatal(err)
				}
				if version >= 20 {
					continue
				}
				body, err := migrations.FS.ReadFile(entry.Name())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := legacy.Exec(string(body)); err != nil {
					t.Fatal(err)
				}
				if _, err := legacy.Exec(`INSERT INTO schema_migrations VALUES (?, ?, ?)`, version, entry.Name(), stamp); err != nil {
					t.Fatal(err)
				}
			}
			cipher := newTestCipher(t)
			token, err := cipher.Seal([]byte("private-test-token"), secret.TokenAAD(42))
			if err != nil {
				t.Fatal(err)
			}
			hook, err := cipher.Seal([]byte("whsec-test"), secret.WebhookSecretAAD(42))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := legacy.Exec(`INSERT INTO projects
				(id, path_with_namespace, web_url, default_branch, token_enc, webhook_secret_enc, created_at, updated_at)
				VALUES (42, 'group/repo', 'https://gitlab.example.com/group/repo', 'main', ?, ?, ?, ?)`, token, hook, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			payload := fmt.Sprintf(`{"project_id":42,"mr_iid":7,"note_id":555,"author_id":12,"author_username":"alice","command":%q,"trigger":"command"}`, kind)
			if _, err := legacy.Exec(`INSERT INTO jobs
				(id, kind, key, payload, state, attempts, max_attempts, run_after, created_at, updated_at)
				VALUES ('legacy-command', 'review.command', 'review.command:42:555', ?, 'running', 1, 3, ?, ?, ?)`, payload, stamp, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			cancelled := kind == "run" // a later opposite intent
			if _, err := legacy.Exec(`INSERT INTO review_controls (project_id, mr_iid, generation, head_sha, cancelled, updated_at)
				VALUES (42, 7, 2, ?, ?, ?)`, sha40, cancelled, stamp); err != nil {
				t.Fatal(err)
			}
			if _, err := legacy.Exec(`INSERT INTO review_status (project_id, mr_iid, generation, head_sha, state, updated_at)
				VALUES (42, 7, 2, ?, 'cancelled', ?)`, sha40, stamp); err != nil {
				t.Fatal(err)
			}
			if _, err := legacy.Exec(`INSERT INTO review_reactions (project_id, mr_iid, note_id, generation, desired, updated_at)
				VALUES (42, 7, 555, 1, 'none', ?)`, stamp); err != nil {
				t.Fatal(err)
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := sqlite.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			control, _ := db.GetReviewControl(ctx, 42, 7)
			status, _ := db.GetReviewStatus(ctx, 42, 7)
			reaction, _ := db.GetReviewReaction(ctx, 42, 7, 555)
			manager := workspace.NewManager(t.TempDir(), nil)
			api := defaultFakeGitLab()
			worker, err := NewCommandWorker(CommandWorkerConfig{Store: db, GitLab: api, Cipher: cipher, Profiles: testProfiles(), Workspace: manager})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := db.Claim(ctx, "legacy-recovery", []string{"review.command"}, time.Minute)
			if err != nil || claimed == nil || claimed.ID != "legacy-command" {
				t.Fatalf("recover legacy command: %+v, %v", claimed, err)
			}
			job := &jobs.Job{ID: claimed.ID, Kind: claimed.Kind, Payload: claimed.Payload}
			api.access = 20
			err = worker.Handle(ctx, job)
			deniedNotes := api.noteSnapshot()
			if err != nil || len(deniedNotes) != 1 || !strings.Contains(deniedNotes[0].Body, "Developer") {
				t.Fatalf("unauthorized command: %v, notes=%+v", err, deniedNotes)
			}
			api.access = 30
			api.updateNoteErr = errors.New("private-test-token upstream failure")
			if err := worker.Handle(ctx, job); err == nil || strings.Contains(err.Error(), "private-test-token") {
				t.Fatalf("failed diagnostic must retry without secrets: %v", err)
			}
			api.updateNoteErr = nil
			if err := worker.Handle(ctx, job); err != nil {
				t.Fatalf("publish uncertainty: %v", err)
			}
			if err := worker.Handle(ctx, job); err != nil {
				t.Fatalf("retry uncertainty: %v", err)
			}
			notes := api.noteSnapshot()
			if len(notes) != 1 || !strings.Contains(notes[0].Body, "kind=uncertain") ||
				!strings.Contains(notes[0].Body, "新的") || strings.Contains(notes[0].Body, "private-test-token") {
				t.Fatalf("operator diagnostic = %+v", notes)
			}
			if err := db.Complete(ctx, *claimed); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = sqlite.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			worker, err = NewCommandWorker(CommandWorkerConfig{Store: db, GitLab: api, Cipher: cipher, Profiles: testProfiles(), Workspace: manager})
			if err != nil {
				t.Fatal(err)
			}
			webhook, err := NewWebhookService(WebhookServiceConfig{Store: db, Workspace: manager, Cipher: cipher, Profiles: testProfiles(), ReviewDefaults: testDefaults()})
			if err != nil {
				t.Fatal(err)
			}
			text := "/review"
			if kind == "cancel" {
				text = "/review cancel"
			}
			request := WebhookRequest{Event: "Note Hook", Token: "whsec-test", UUID: uuid.NewString(), Body: noteBody(42, 7, 555, 12, "alice", text)}
			invalidRequest := request
			invalidRequest.Token = "wrong-secret"
			invalid, err := webhook.Handle(ctx, invalidRequest)
			if err != nil || !invalid.Unauthorized || invalid.Accepted {
				t.Fatalf("invalid webhook token = %+v, %v", invalid, err)
			}
			redelivery, err := webhook.Handle(ctx, request)
			if err != nil || !redelivery.Accepted {
				t.Fatalf("new UUID delivery = %+v, %v", redelivery, err)
			}
			duplicate, err := webhook.Handle(ctx, request)
			if err != nil || duplicate.Ignored != "duplicate" {
				t.Fatalf("same UUID delivery = %+v, %v", duplicate, err)
			}
			claimed, err = db.Claim(ctx, "redelivery", []string{"review.command"}, time.Minute)
			if err != nil || claimed == nil || claimed.ID != redelivery.JobID {
				t.Fatalf("claim redelivery = %+v, %v", claimed, err)
			}
			if err := worker.Handle(ctx, &jobs.Job{ID: claimed.ID, Kind: claimed.Kind, Payload: claimed.Payload}); err != nil {
				t.Fatal(err)
			}
			if err := db.Complete(ctx, *claimed); err != nil {
				t.Fatal(err)
			}
			controlAfter, _ := db.GetReviewControl(ctx, 42, 7)
			statusAfter, _ := db.GetReviewStatus(ctx, 42, 7)
			reactionAfter, _ := db.GetReviewReaction(ctx, 42, 7, 555)
			if !reflect.DeepEqual(control, controlAfter) || !reflect.DeepEqual(status, statusAfter) || !reflect.DeepEqual(reaction, reactionAfter) || len(api.noteSnapshot()) != 1 {
				t.Fatal("legacy replay changed review state or repeated the diagnostic")
			}
			unexpected, err := db.Claim(ctx, "unexpected", []string{"review.run", "review.cancel"}, time.Minute)
			if err != nil || unexpected != nil {
				t.Fatalf("legacy replay queued a review: %+v, %v", unexpected, err)
			}
			request.UUID, request.Body = uuid.NewString(), noteBody(42, 7, 777, 12, "alice", text)
			fresh, err := webhook.Handle(ctx, request)
			if err != nil || !fresh.Accepted {
				t.Fatalf("new note = %+v, %v", fresh, err)
			}
			claimed, err = db.Claim(ctx, "fresh", []string{"review.command"}, time.Minute)
			if err != nil || claimed == nil || claimed.ID != fresh.JobID {
				t.Fatalf("claim new note = %+v, %v", claimed, err)
			}
			if err := worker.Handle(ctx, &jobs.Job{ID: claimed.ID, Kind: claimed.Kind, Payload: claimed.Payload}); err != nil {
				t.Fatal(err)
			}
			final, err := db.GetReviewControl(ctx, 42, 7)
			if err != nil || final.Generation != 3 || final.Cancelled != (kind == "cancel") {
				t.Fatalf("new note did not execute: %+v, %v", final, err)
			}
		})
	}
}
