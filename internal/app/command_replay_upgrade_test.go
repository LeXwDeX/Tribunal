package app

import (
	"context"
	"database/sql"
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

func TestCompletedLegacyCommandRedeliveryAfterUpgrade(t *testing.T) {
	testLegacyCommandRedeliveryAfterUpgrade(t, false)
}

func TestPrunedLegacyCommandRedeliveryAfterUpgrade(t *testing.T) {
	testLegacyCommandRedeliveryAfterUpgrade(t, true)
}

func testLegacyCommandRedeliveryAfterUpgrade(t *testing.T, pruneCommand bool) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	if _, err := legacy.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	const stamp = "2026-01-01T00:00:00.000000000Z"
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		version, err := strconv.Atoi(name[:4])
		if err != nil {
			t.Fatal(err)
		}
		if version >= 20 {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := legacy.Exec(`INSERT INTO schema_migrations VALUES (?, ?, ?)`, version, name, stamp); err != nil {
			t.Fatal(err)
		}
	}
	cipher := newTestCipher(t)
	token, err := cipher.Seal([]byte("test-token"), secret.TokenAAD(42))
	if err != nil {
		t.Fatal(err)
	}
	hookSecret, err := cipher.Seal([]byte("whsec-test"), secret.WebhookSecretAAD(42))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO projects
		(id, path_with_namespace, web_url, default_branch, token_enc, webhook_secret_enc, created_at, updated_at)
		VALUES (42, 'group/repo', 'https://gitlab.example.com/group/repo', 'main', ?, ?, ?, ?)`, token, hookSecret, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	insertJob := func(id, kind, key, payload, state string) {
		t.Helper()
		if _, err := legacy.Exec(`INSERT INTO jobs
			(id, kind, key, payload, state, attempts, max_attempts, run_after, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 1, 3, ?, ?, ?)`, id, kind, key, payload, state, stamp, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	// The command worker completed /review and its review.run job. A later
	// /review cancel changed the control before the old database was upgraded.
	insertJob("legacy-command", "review.command", "review.command:42:555",
		`{"project_id":42,"mr_iid":7,"note_id":555,"author_id":12,"author_username":"alice","command":"run","trigger":"command"}`, "done")
	insertJob("legacy-run", "review.run", "review:42:7",
		`{"project_id":42,"mr_iid":7,"note_id":555,"generation":1,"trigger":"command"}`, "done")
	insertJob("legacy-cancel-command", "review.command", "review.command:42:556",
		`{"project_id":42,"mr_iid":7,"note_id":556,"author_id":12,"command":"cancel","trigger":"command"}`, "done")
	insertJob("legacy-cancel", "review.cancel", "review.cancel:42:7",
		`{"project_id":42,"mr_iid":7,"note_id":556,"generation":2,"trigger":"command"}`, "done")
	if pruneCommand {
		// Retention may remove the older terminal command before the downstream
		// run, whose updated_at can be newer or which can still be queued.
		res, err := legacy.Exec(`DELETE FROM jobs WHERE id = 'legacy-command'`)
		if err != nil {
			t.Fatal(err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			t.Fatalf("prune legacy command: rows=%d, %v", n, err)
		}
	}
	if _, err := legacy.Exec(`INSERT INTO review_controls
		(project_id, mr_iid, generation, head_sha, cancelled, updated_at) VALUES (42, 7, 2, ?, 1, ?)`, sha40, stamp); err != nil {
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
	if _, err := legacy.Exec(`INSERT INTO review_reactions (project_id, mr_iid, note_id, generation, desired, updated_at)
		VALUES (42, 7, 556, 2, 'none', ?)`, stamp); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	statusBefore, err := db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	reactionBefore, err := db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil {
		t.Fatal(err)
	}
	cancelReactionBefore, err := db.GetReviewReaction(ctx, 42, 7, 556)
	if err != nil {
		t.Fatal(err)
	}
	manager := workspace.NewManager(t.TempDir(), nil)
	webhook, err := NewWebhookService(WebhookServiceConfig{
		Store: db, Workspace: manager, Cipher: cipher, Profiles: testProfiles(), ReviewDefaults: testDefaults(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := webhook.Handle(ctx, WebhookRequest{
		Event: "Note Hook", Token: "whsec-test", UUID: uuid.NewString(),
		Body: noteBody(42, 7, 555, 12, "alice", "/review"),
	})
	if err != nil || !result.Accepted || result.JobID == "legacy-command" {
		t.Fatalf("fresh webhook delivery = %+v, %v", result, err)
	}
	command, err := db.Claim(ctx, "upgrade-redelivery", []string{"review.command"}, time.Minute)
	if err != nil || command == nil || command.ID != result.JobID {
		t.Fatalf("claim redelivery = %+v, %v", command, err)
	}
	recording := &recordingCommandStore{CommandStore: db}
	worker, err := NewCommandWorker(CommandWorkerConfig{
		Store: recording, GitLab: defaultFakeGitLab(), Cipher: cipher, Profiles: testProfiles(), Workspace: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Handle(ctx, &jobs.Job{ID: command.ID, Kind: command.Kind, Payload: command.Payload}); err != nil {
		t.Fatalf("handle redelivery: %v", err)
	}
	if len(recording.dup) != 1 || !recording.dup[0] || recording.ids[0] != "legacy-run" {
		t.Errorf("worker enqueue: ids=%v duplicates=%v", recording.ids, recording.dup)
	}
	control, err := db.GetReviewControl(ctx, 42, 7)
	if err != nil || control.Generation != 2 || !control.Cancelled || control.HeadSHA != sha40 {
		t.Errorf("redelivery changed cancelled control: %+v, %v", control, err)
	}
	statusAfter, err := db.GetReviewStatus(ctx, 42, 7)
	if err != nil || !reflect.DeepEqual(statusBefore, statusAfter) {
		t.Errorf("redelivery changed status: before=%+v after=%+v, %v", statusBefore, statusAfter, err)
	}
	reactionAfter, err := db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil || !reflect.DeepEqual(reactionBefore, reactionAfter) {
		t.Errorf("redelivery changed reaction: before=%+v after=%+v, %v", reactionBefore, reactionAfter, err)
	}
	cancelReactionAfter, err := db.GetReviewReaction(ctx, 42, 7, 556)
	if err != nil || !reflect.DeepEqual(cancelReactionBefore, cancelReactionAfter) {
		t.Errorf("redelivery changed cancel reaction: before=%+v after=%+v, %v", cancelReactionBefore, cancelReactionAfter, err)
	}
	run, err := db.Claim(ctx, "unexpected-run", []string{"review.run"}, time.Minute)
	if err != nil || run != nil {
		t.Errorf("redelivery enqueued a run: %+v, %v", run, err)
	}
}
