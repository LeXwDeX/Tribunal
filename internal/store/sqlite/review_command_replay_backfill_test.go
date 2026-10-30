package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

// openLegacyCommandReplayDB builds a database with every migration before
// 0020 applied, ready for pre-upgrade rows.
func openLegacyCommandReplayDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy-command.db")
	db, err := sql.Open("sqlite", withPragmas(path))
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	for _, name := range migrationNamesBefore(t, 20) {
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			migrationVersionForTest(t, name), name, "2026-10-22T12:00:00.000000000Z"); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return path, db
}

func migrationNamesBefore(t *testing.T, limit int) []string {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	var before []string
	for _, name := range names {
		if migrationVersionForTest(t, name) < limit {
			before = append(before, name)
		}
	}
	return before
}

// applyCommandReplaySchema applies 0020 without running the backfill, so a
// test can start two Open calls or backfill calls at the same first-run state.
func applyCommandReplaySchema(t *testing.T, db *sql.DB) {
	t.Helper()
	body, err := migrations.FS.ReadFile("0020_review_command_replay.sql")
	if err != nil {
		t.Fatalf("read 0020: %v", err)
	}
	if _, err := db.Exec(string(body)); err != nil {
		t.Fatalf("apply 0020 schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (20, '0020_review_command_replay.sql', ?)`,
		"2026-10-22T12:00:00.000000000Z"); err != nil {
		t.Fatalf("record 0020 schema: %v", err)
	}
}

func insertLegacyReplayProject(t *testing.T, db *sql.DB, projectID int64) {
	t.Helper()
	stamp := "2026-10-22T12:00:00.000000000Z"
	if _, err := db.Exec(`INSERT INTO projects
		(id, path_with_namespace, web_url, default_branch, token_enc, webhook_secret_enc, created_at, updated_at)
		VALUES (?, ?, ?, 'main', x'01', x'02', ?, ?)`,
		projectID, fmt.Sprintf("group/project-%d", projectID),
		fmt.Sprintf("https://gitlab.example.com/group/project-%d", projectID), stamp, stamp); err != nil {
		t.Fatalf("insert legacy project: %v", err)
	}
}

func insertLegacyReplayJob(t *testing.T, db *sql.DB, id, kind, key, payload, state string) {
	t.Helper()
	stamp := "2026-10-22T12:00:00.000000000Z"
	if _, err := db.Exec(`INSERT INTO jobs
		(id, kind, key, payload, state, attempts, max_attempts, run_after,
		 lease_owner, lease_until, last_error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 1, 3, ?, NULL, NULL, NULL, ?, ?)`,
		id, kind, key, payload, state, stamp, stamp, stamp); err != nil {
		t.Fatalf("insert legacy job %s: %v", id, err)
	}
}

func insertLegacyReplayJobNullKey(t *testing.T, db *sql.DB, id, kind, payload, state string) {
	t.Helper()
	stamp := "2026-10-22T12:00:00.000000000Z"
	if _, err := db.Exec(`INSERT INTO jobs
		(id, kind, key, payload, state, attempts, max_attempts, run_after,
		 lease_owner, lease_until, last_error, created_at, updated_at)
		VALUES (?, ?, NULL, ?, ?, 1, 3, ?, NULL, NULL, NULL, ?, ?)`,
		id, kind, payload, state, stamp, stamp, stamp); err != nil {
		t.Fatalf("insert legacy job %s: %v", id, err)
	}
}

func insertLegacyReplayControl(t *testing.T, db *sql.DB, projectID, mrIID, generation int64, head string, cancelled bool) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO review_controls
		(project_id, mr_iid, generation, head_sha, cancelled, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		projectID, mrIID, generation, head, cancelled, "2026-10-22T12:00:00.000000000Z"); err != nil {
		t.Fatalf("insert legacy control: %v", err)
	}
}

func insertLegacyReplayReaction(t *testing.T, db *sql.DB, projectID, mrIID, noteID, generation int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO review_reactions
		(project_id, mr_iid, note_id, generation, desired, version, applied_version, last_error, updated_at)
		VALUES (?, ?, ?, ?, 'eyes', 1, 0, '', ?)`,
		projectID, mrIID, noteID, generation, "2026-10-22T12:00:00.000000000Z"); err != nil {
		t.Fatalf("insert legacy reaction: %v", err)
	}
}

func legacyCommandPayload(projectID, mrIID, noteID int64, command string) string {
	return fmt.Sprintf(`{"project_id":%d,"mr_iid":%d,"note_id":%d,"author_id":12,"command":%q,"trigger":"command"}`,
		projectID, mrIID, noteID, command)
}

func legacyReviewPayload(projectID, mrIID, noteID, generation int64, head string) string {
	return fmt.Sprintf(`{"project_id":%d,"mr_iid":%d,"note_id":%d,"generation":%d,"head_sha":%q,"trigger":"command"}`,
		projectID, mrIID, noteID, generation, head)
}

func commandReplayIdentityRows(t *testing.T, s *Store, projectID, mrIID, noteID int64, kind string) int {
	t.Helper()
	return rowCount(t, s, `SELECT COUNT(*) FROM review_command_replays
		WHERE project_id = ? AND mr_iid = ? AND note_id = ? AND command = ?`,
		projectID, mrIID, noteID, kind)
}

func TestCommandReplayBackfillRestoresCommittedLegacyCommands(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	head := reviewHead("a")
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayControl(t, legacy, 42, 7, 2, head, true)
	// A run accepted before the upgrade, then superseded by /cancel. The
	// reaction proves the run enqueue committed; the command payload carries
	// the kind.
	insertLegacyReplayReaction(t, legacy, 42, 7, 555, 1)
	insertLegacyReplayJob(t, legacy, "legacy-command-555", "review.command",
		"review.command:42:555", legacyCommandPayload(42, 7, 555, "run"), "running")
	insertLegacyReplayJob(t, legacy, "legacy-run-555", "review.run",
		"review:42:7", legacyReviewPayload(42, 7, 555, 1, head), "queued")
	// The cancel has no reaction left, but its review.cancel job proves the
	// enqueue committed and supplies the downstream review job ID.
	insertLegacyReplayJob(t, legacy, "legacy-command-556", "review.command",
		"review.command:42:556", legacyCommandPayload(42, 7, 556, "cancel"), "running")
	insertLegacyReplayJob(t, legacy, "legacy-cancel-556", "review.cancel",
		"review.cancel:42:7", legacyReviewPayload(42, 7, 556, 2, ""), "queued")
	// A review.run job alone is acceptance evidence for a run without a
	// reaction and supplies its ID.
	insertLegacyReplayJob(t, legacy, "legacy-command-561", "review.command",
		"review.command:42:561", legacyCommandPayload(42, 7, 561, "run"), "running")
	insertLegacyReplayJob(t, legacy, "legacy-run-561", "review.run",
		"review:42:7", legacyReviewPayload(42, 7, 561, 1, head), "done")
	// A run whose review job was replaced still has a reaction and command job.
	// The downstream ID is unknown, so the backfilled ID is empty.
	insertLegacyReplayReaction(t, legacy, 42, 7, 557, 1)
	insertLegacyReplayJob(t, legacy, "legacy-command-557", "review.command",
		"review.command:42:557", legacyCommandPayload(42, 7, 557, "run"), "queued")
	// Never executed: no reaction and no review job, so there is no proof.
	insertLegacyReplayJob(t, legacy, "legacy-command-558", "review.command",
		"review.command:42:558", legacyCommandPayload(42, 7, 558, "run"), "queued")
	// A crashed command that exhausted its attempts after the enqueue
	// committed is still replayable evidence when a reaction is present.
	insertLegacyReplayReaction(t, legacy, 42, 7, 562, 1)
	insertLegacyReplayJob(t, legacy, "legacy-command-562", "review.command",
		"review.command:42:562", legacyCommandPayload(42, 7, 562, "run"), "failed")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	legacyRunBefore, err := s.GetJob(ctx, "legacy-run-555")
	if err != nil {
		t.Fatal(err)
	}

	replays := []struct {
		noteID  int64
		command string
		head    string
		jobID   string
	}{
		{555, "run", head, "legacy-run-555"},
		{556, "cancel", "", "legacy-cancel-556"},
		{561, "run", head, "legacy-run-561"},
	}
	for _, replay := range replays {
		result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, replay.noteID, replay.command, replay.head))
		if err != nil {
			t.Fatalf("replay note %d: %v", replay.noteID, err)
		}
		if !result.Duplicate || result.Generation != 0 || result.JobID != replay.jobID {
			t.Fatalf("replay note %d = %+v, want duplicate of %q", replay.noteID, result, replay.jobID)
		}
	}
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if control.Generation != 2 || !control.Cancelled || control.HeadSHA != head {
		t.Fatalf("control after legacy replays = %+v", control)
	}
	legacyRunAfter, err := s.GetJob(ctx, "legacy-run-555")
	if err != nil {
		t.Fatal(err)
	}
	if legacyRunAfter.State != legacyRunBefore.State || string(legacyRunAfter.Payload) != string(legacyRunBefore.Payload) ||
		!legacyRunAfter.UpdatedAt.Equal(legacyRunBefore.UpdatedAt) {
		t.Fatalf("legacy replay changed the review job: before=%+v after=%+v", legacyRunBefore, legacyRunAfter)
	}

	// These commands never had a review job, and their reaction is older than
	// the control. A reaction from an older generation cannot prove the kind.
	for i, noteID := range []int64{557, 562} {
		result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, noteID, "run", head))
		if err != nil || result.Duplicate || result.Generation != int64(i+3) {
			t.Fatalf("older-reaction note %d = %+v, %v", noteID, result, err)
		}
	}
	// The queued command that never executed must still enqueue normally.
	pending, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 558, "run", head))
	if err != nil || pending.Duplicate || pending.Generation != 5 {
		t.Fatalf("never-executed legacy command = %+v, %v", pending, err)
	}
}

func TestCommandReplayBackfillSkipsUnprovenLegacyState(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	head := reviewHead("a")
	insertLegacyReplayProject(t, legacy, 42)
	// The control points at a cancel; a reaction-only cancel proof at the
	// current generation is consistent with the latest intent.
	insertLegacyReplayControl(t, legacy, 42, 7, 1, head, true)
	// No reaction and no review job: the worker never committed this command.
	insertLegacyReplayJob(t, legacy, "unproven-601", "review.command",
		"review.command:42:601", legacyCommandPayload(42, 7, 601, "run"), "running")
	// Malformed JSON and a mismatched job key are ignored.
	insertLegacyReplayJob(t, legacy, "malformed-review", "review.run",
		"review:42:7", `{"kind":`, "done")
	insertLegacyReplayJob(t, legacy, "malformed-602", "review.command",
		"review.command:42:602", `{"project_id":42,`, "running")
	insertLegacyReplayJob(t, legacy, "mismatch-603", "review.command",
		"review.command:99:603", legacyCommandPayload(42, 7, 603, "run"), "running")
	// Terminal jobs are not replayable and are not backfilled.
	insertLegacyReplayReaction(t, legacy, 42, 7, 604, 1)
	insertLegacyReplayJob(t, legacy, "terminal-604", "review.command",
		"review.command:42:604", legacyCommandPayload(42, 7, 604, "run"), "done")
	// A command plus its reaction, with no review job left, is proven.
	insertLegacyReplayReaction(t, legacy, 42, 7, 605, 1)
	insertLegacyReplayJob(t, legacy, "proven-605", "review.command",
		"review.command:42:605", legacyCommandPayload(42, 7, 605, "cancel"), "queued")
	// Malformed review JSON is ignored.
	insertLegacyReplayJob(t, legacy, "bad-review-605", "review.run",
		"review:42:7", `{"note_id":`, "done")
	// A review job with a non-command trigger is not proof.
	insertLegacyReplayJob(t, legacy, "push-606", "review.command",
		"review.command:42:606", legacyCommandPayload(42, 7, 606, "run"), "queued")
	insertLegacyReplayJob(t, legacy, "review-606", "review.run",
		"review:42:7", `{"project_id":42,"mr_iid":7,"note_id":606,"trigger":"mr_update"}`, "done")
	// A review job without its command job is not replayable; no identity is
	// fabricated from it.
	insertLegacyReplayJob(t, legacy, "review-607", "review.run",
		"review:42:7", legacyReviewPayload(42, 7, 607, 1, head), "queued")
	// A review job whose key does not match its payload project/MR is not
	// acceptance evidence.
	insertLegacyReplayJob(t, legacy, "mismatched-review-609", "review.run",
		"review:99:7", legacyReviewPayload(42, 7, 609, 1, head), "queued")
	insertLegacyReplayJob(t, legacy, "unproven-609", "review.command",
		"review.command:42:609", legacyCommandPayload(42, 7, 609, "run"), "queued")
	// One reaction cannot prove both kinds. With running run and queued cancel
	// command jobs on the same note and no kind-specific review job, neither
	// is backfilled.
	insertLegacyReplayReaction(t, legacy, 42, 7, 608, 1)
	insertLegacyReplayJob(t, legacy, "ambiguous-608-run", "review.command",
		"review.command:42:608", legacyCommandPayload(42, 7, 608, "run"), "running")
	insertLegacyReplayJob(t, legacy, "ambiguous-608-cancel", "review.command",
		"review.command:42:608", legacyCommandPayload(42, 7, 608, "cancel"), "queued")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_replays`); got != 1 {
		t.Fatalf("backfilled identities = %d, want only the proven cancel", got)
	}

	for i, noteID := range []int64{601, 602, 603, 604, 606, 607, 609} {
		result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, noteID, "run", head))
		if err != nil || result.Duplicate || result.Generation != int64(i+2) {
			t.Fatalf("unproven note %d = %+v, %v", noteID, result, err)
		}
	}
	// Different command kind on the same note as the proven cancel is new.
	runResult, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 605, "run", head))
	if err != nil || runResult.Duplicate || runResult.Generation != 9 {
		t.Fatalf("new run on cancel note = %+v, %v", runResult, err)
	}
	cancelResult, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 605, "cancel", ""))
	if err != nil || !cancelResult.Duplicate || cancelResult.Generation != 0 {
		t.Fatalf("proven cancel replay = %+v, %v", cancelResult, err)
	}
	// The ambiguous note is not narrowed by the shared reaction: both commands
	// still run normally.
	ambiguousRun, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 608, "run", head))
	if err != nil || ambiguousRun.Duplicate || ambiguousRun.Generation != 10 {
		t.Fatalf("ambiguous run = %+v, %v", ambiguousRun, err)
	}
	ambiguousCancel, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 608, "cancel", ""))
	if err != nil || ambiguousCancel.Duplicate || ambiguousCancel.Generation != 11 {
		t.Fatalf("ambiguous cancel = %+v, %v", ambiguousCancel, err)
	}
}

func TestCommandReplayBackfillIgnoresOppositeTerminalCommand(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	head := reviewHead("a")
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayControl(t, legacy, 42, 7, 2, head, true)
	// Note 902: run completed and its review job was pruned; only the reaction
	// and the terminal command job prove the run. A queued cancel must not be
	// mistaken for that accepted run.
	insertLegacyReplayReaction(t, legacy, 42, 7, 902, 1)
	insertLegacyReplayJob(t, legacy, "done-run-902", "review.command",
		"review.command:42:902", legacyCommandPayload(42, 7, 902, "run"), "done")
	insertLegacyReplayJob(t, legacy, "queued-cancel-902", "review.command",
		"review.command:42:902", legacyCommandPayload(42, 7, 902, "cancel"), "queued")
	// Note 903: the inverse. A completed cancel must not consume the queued
	// run.
	insertLegacyReplayReaction(t, legacy, 42, 7, 903, 1)
	insertLegacyReplayJob(t, legacy, "done-cancel-903", "review.command",
		"review.command:42:903", legacyCommandPayload(42, 7, 903, "cancel"), "done")
	insertLegacyReplayJob(t, legacy, "queued-run-903", "review.command",
		"review.command:42:903", legacyCommandPayload(42, 7, 903, "run"), "queued")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, tt := range []struct {
		noteID  int64
		command string
		head    string
	}{
		{902, "cancel", ""},
		{903, "run", head},
	} {
		if got := commandReplayIdentityRows(t, s, 42, 7, tt.noteID, tt.command); got != 0 {
			t.Fatalf("note %d %s was backfilled from a reaction shared with the opposite terminal command", tt.noteID, tt.command)
		}
	}
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if control.Generation != 2 || !control.Cancelled {
		t.Fatalf("backfill changed the control: %+v", control)
	}
}

func TestCommandReplayBackfillIgnoresRetainedDownstreamOpposite(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	head := reviewHead("a")
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayControl(t, legacy, 42, 7, 2, head, true)
	// The queued cancel never executed. A retained review.run job proves an
	// earlier run from the same note, so the shared reaction cannot select the
	// cancel.
	insertLegacyReplayReaction(t, legacy, 42, 7, 904, 1)
	insertLegacyReplayJob(t, legacy, "queued-cancel-904", "review.command",
		"review.command:42:904", legacyCommandPayload(42, 7, 904, "cancel"), "queued")
	insertLegacyReplayJob(t, legacy, "retained-run-904", "review.run",
		"review:42:7", legacyReviewPayload(42, 7, 904, 1, head), "done")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := commandReplayIdentityRows(t, s, 42, 7, 904, "cancel"); got != 0 {
		t.Fatal("unexecuted cancel was backfilled because a retained run used the same reaction")
	}
	if got := commandReplayIdentityRows(t, s, 42, 7, 904, "run"); got != 0 {
		t.Fatal("run without a retained command job was backfilled")
	}
}

func TestCommandReplayBackfillDoesNotTrustReactionAtCurrentControl(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	head := reviewHead("a")
	insertLegacyReplayProject(t, legacy, 42)
	// The control is a run at generation 1, but a queued cancel at the same
	// generation must still execute. The reaction cannot prove the cancel.
	insertLegacyReplayControl(t, legacy, 42, 7, 1, head, false)
	insertLegacyReplayReaction(t, legacy, 42, 7, 905, 1)
	insertLegacyReplayJob(t, legacy, "queued-cancel-905", "review.command",
		"review.command:42:905", legacyCommandPayload(42, 7, 905, "cancel"), "queued")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := commandReplayIdentityRows(t, s, 42, 7, 905, "cancel"); got != 0 {
		t.Fatal("reaction at the current run generation suppressed an unexecuted cancel")
	}
}

func TestCommandReplayBackfillStrictReactionGeneration(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	head := reviewHead("a")
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayControl(t, legacy, 42, 7, 3, head, true)
	// Note 700: an older cancel left a reaction, but every command/review job
	// for it was pruned. An unrelated run advanced the control to generation 2.
	// A never-executed queued run for note 700 must still run.
	insertLegacyReplayReaction(t, legacy, 42, 7, 700, 1)
	insertLegacyReplayJob(t, legacy, "queued-run-700", "review.command",
		"review.command:42:700", legacyCommandPayload(42, 7, 700, "run"), "queued")
	// Note 701: inverse. An older run left a reaction; an unrelated cancel
	// advanced the control. The never-executed queued cancel must still run.
	insertLegacyReplayReaction(t, legacy, 42, 7, 701, 1)
	insertLegacyReplayJob(t, legacy, "queued-cancel-701", "review.command",
		"review.command:42:701", legacyCommandPayload(42, 7, 701, "cancel"), "queued")
	// Note 702 on MR 9: equality with a run control proves the run.
	insertLegacyReplayControl(t, legacy, 42, 9, 3, head, false)
	insertLegacyReplayReaction(t, legacy, 42, 9, 702, 3)
	insertLegacyReplayJob(t, legacy, "queued-run-702", "review.command",
		"review.command:42:702", legacyCommandPayload(42, 9, 702, "run"), "queued")
	// Note 703 on MR 8: equality with a cancel control proves the cancel.
	insertLegacyReplayControl(t, legacy, 42, 8, 3, head, true)
	insertLegacyReplayReaction(t, legacy, 42, 8, 703, 3)
	insertLegacyReplayJob(t, legacy, "queued-cancel-703", "review.command",
		"review.command:42:703", legacyCommandPayload(42, 8, 703, "cancel"), "queued")
	// Note 704: an older reaction cannot prove the kind, but a kind-specific
	// review job still can.
	insertLegacyReplayReaction(t, legacy, 42, 7, 704, 1)
	insertLegacyReplayJob(t, legacy, "queued-run-704", "review.command",
		"review.command:42:704", legacyCommandPayload(42, 7, 704, "run"), "queued")
	insertLegacyReplayJob(t, legacy, "review-704", "review.run",
		"review:42:7", legacyReviewPayload(42, 7, 704, 2, head), "done")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, tt := range []struct {
		mrIID   int64
		noteID  int64
		command string
	}{
		{7, 700, "run"},
		{7, 701, "cancel"},
	} {
		if got := commandReplayIdentityRows(t, s, 42, tt.mrIID, tt.noteID, tt.command); got != 0 {
			t.Fatalf("older reaction backfilled note %d %s", tt.noteID, tt.command)
		}
	}
	for _, tt := range []struct {
		mrIID   int64
		noteID  int64
		command string
		head    string
		jobID   string
	}{
		{9, 702, "run", head, ""},
		{8, 703, "cancel", "", ""},
		{7, 704, "run", head, "review-704"},
	} {
		result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, tt.mrIID, tt.noteID, tt.command, tt.head))
		if err != nil || !result.Duplicate || result.Generation != 0 || result.JobID != tt.jobID {
			t.Fatalf("strict proof note %d = %+v, %v; want duplicate of %q", tt.noteID, result, err, tt.jobID)
		}
	}
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil || control.Generation != 3 || !control.Cancelled {
		t.Fatalf("control after strict proofs = %+v, %v", control, err)
	}
}

func TestCommandReplayBackfillDoesNotSuppressCancelAfterNewerRun(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	head := reviewHead("a")
	insertLegacyReplayProject(t, legacy, 42)
	// The reaction is from an older run. A newer automatic run advanced the
	// control past it, and the queued cancel has not executed. With both the
	// run command and review jobs pruned, the reaction cannot prove the cancel.
	insertLegacyReplayControl(t, legacy, 42, 7, 2, head, false)
	insertLegacyReplayReaction(t, legacy, 42, 7, 907, 1)
	insertLegacyReplayJob(t, legacy, "queued-cancel-907", "review.command",
		"review.command:42:907", legacyCommandPayload(42, 7, 907, "cancel"), "queued")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := commandReplayIdentityRows(t, s, 42, 7, 907, "cancel"); got != 0 {
		t.Fatal("reaction from an older run suppressed an unexecuted cancel")
	}
	result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 907, "cancel", ""))
	if err != nil || result.Duplicate || result.Generation != 3 {
		t.Fatalf("unexecuted cancel = %+v, %v", result, err)
	}
}

func TestCommandReplayBackfillRequiresReviewKeyKindMatch(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	head := reviewHead("a")
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayControl(t, legacy, 42, 7, 2, head, false)
	// A review.run row keyed as a cancel must not prove a run. The prior
	// per-kind key lookup could not match this malformed row.
	insertLegacyReplayJob(t, legacy, "mismatched-review-kind", "review.run",
		"review.cancel:42:7", legacyReviewPayload(42, 7, 910, 1, head), "done")
	insertLegacyReplayJob(t, legacy, "queued-run-910", "review.command",
		"review.command:42:910", legacyCommandPayload(42, 7, 910, "run"), "queued")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := commandReplayIdentityRows(t, s, 42, 7, 910, "run"); got != 0 {
		t.Fatal("review.run row with a cancel key was accepted as run proof")
	}
	result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 910, "run", head))
	if err != nil || result.Duplicate || result.Generation != 3 {
		t.Fatalf("queued run = %+v, %v", result, err)
	}
}

func TestCommandReplayBackfillRequiresCommandKeyPayloadMatch(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	head := reviewHead("c")
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayControl(t, legacy, 42, 7, 2, head, true)
	// Candidate A's key is reused by a terminal row whose payload names
	// candidate B with an opposite kind. It must not add a spurious known kind
	// to B.
	insertLegacyReplayJob(t, legacy, "candidate-a", "review.command",
		"review.command:42:911", legacyCommandPayload(42, 8, 911, "run"), "queued")
	insertLegacyReplayJob(t, legacy, "mismatched-command-payload", "review.command",
		"review.command:42:911", legacyCommandPayload(42, 7, 912, "run"), "done")
	insertLegacyReplayReaction(t, legacy, 42, 7, 912, 2)
	insertLegacyReplayJob(t, legacy, "candidate-b", "review.command",
		"review.command:42:912", legacyCommandPayload(42, 7, 912, "cancel"), "queued")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_replays`); got != 1 {
		t.Fatalf("backfilled identities = %d, want the proven cancel only", got)
	}
	if got := commandReplayIdentityRows(t, s, 42, 7, 912, "cancel"); got != 1 {
		t.Fatal("mismatched command key/payload added a spurious known kind to another candidate")
	}
	result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 912, "cancel", ""))
	if err != nil || !result.Duplicate || result.Generation != 0 {
		t.Fatalf("proven cancel replay = %+v, %v", result, err)
	}
}

func TestCommandReplayBackfillHandlesNullJobKeys(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayControl(t, legacy, 42, 7, 1, "", false)
	insertLegacyReplayReaction(t, legacy, 42, 7, 906, 1)
	insertLegacyReplayJobNullKey(t, legacy, "null-key-906", "review.command",
		legacyCommandPayload(42, 7, 906, "run"), "queued")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("a NULL job key blocked startup: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := commandReplayIdentityRows(t, s, 42, 7, 906, "run"); got != 0 {
		t.Fatal("NULL-key command job was backfilled without a matching key")
	}
}

func TestCommandReplayBackfillNoopAfterMarker(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	head := reviewHead("a")
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayControl(t, legacy, 42, 7, 1, head, false)
	insertLegacyReplayReaction(t, legacy, 42, 7, 701, 1)
	insertLegacyReplayJob(t, legacy, "proven-701", "review.command",
		"review.command:42:701", legacyCommandPayload(42, 7, 701, "run"), "running")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	before := rowCount(t, s, `SELECT COUNT(*) FROM review_command_replays`)
	if before != 1 {
		t.Fatalf("backfilled identities = %d", before)
	}

	const workers = 8
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- backfillReviewCommandReplays(ctx, s.db, s.now())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent backfill: %v", err)
		}
	}
	if after := rowCount(t, s, `SELECT COUNT(*) FROM review_command_replays`); after != before {
		t.Fatalf("identities after concurrent no-op backfill = %d, want %d", after, before)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_replay_backfill`); got != 1 {
		t.Fatalf("backfill markers = %d", got)
	}
}

func TestCommandReplayBackfillConcurrentFirstRun(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayControl(t, legacy, 42, 7, 1, "", false)
	insertLegacyReplayReaction(t, legacy, 42, 7, 702, 1)
	insertLegacyReplayJob(t, legacy, "proven-702", "review.command",
		"review.command:42:702", legacyCommandPayload(42, 7, 702, "run"), "running")
	applyCommandReplaySchema(t, legacy)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	// Two independent connections race on the first backfill, before either
	// has written the marker.
	first, err := sql.Open("sqlite", withPragmas(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := sql.Open("sqlite", withPragmas(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, db := range []*sql.DB{first, second} {
		wg.Add(1)
		go func(db *sql.DB) {
			defer wg.Done()
			errs <- backfillReviewCommandReplays(ctx, db, newFakeClock().Now())
		}(db)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent first backfill: %v", err)
		}
	}
	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("reopen migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_replay_backfill`); got != 1 {
		t.Fatalf("backfill markers = %d", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_replays`); got != 1 {
		t.Fatalf("identities = %d", got)
	}
}

func TestCommandReplayBackfillRollbackRetry(t *testing.T) {
	ctx := context.Background()
	_, legacy := openLegacyCommandReplayDB(t)
	rawCount := func(query string) int {
		t.Helper()
		var count int
		if err := legacy.QueryRow(query).Scan(&count); err != nil {
			t.Fatalf("count query: %v", err)
		}
		return count
	}
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayControl(t, legacy, 42, 7, 1, "", false)
	insertLegacyReplayReaction(t, legacy, 42, 7, 703, 1)
	insertLegacyReplayJob(t, legacy, "proven-703", "review.command",
		"review.command:42:703", legacyCommandPayload(42, 7, 703, "run"), "running")
	applyCommandReplaySchema(t, legacy)
	if _, err := legacy.Exec(`CREATE TRIGGER block_replay_backfill BEFORE INSERT ON review_command_replays
		BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := backfillReviewCommandReplays(ctx, legacy, newFakeClock().Now()); err == nil {
		t.Fatal("backfill ignored an insert failure")
	}
	if got := rawCount(`SELECT COUNT(*) FROM review_command_replay_backfill`); got != 0 {
		t.Fatalf("marker after failed backfill = %d", got)
	}
	if got := rawCount(`SELECT COUNT(*) FROM review_command_replays`); got != 0 {
		t.Fatalf("identities after failed backfill = %d", got)
	}
	if _, err := legacy.Exec(`DROP TRIGGER block_replay_backfill`); err != nil {
		t.Fatal(err)
	}
	if err := backfillReviewCommandReplays(ctx, legacy, newFakeClock().Now()); err != nil {
		t.Fatalf("retry backfill: %v", err)
	}
	if got := rawCount(`SELECT COUNT(*) FROM review_command_replay_backfill`); got != 1 {
		t.Fatalf("marker after retry = %d", got)
	}
	if got := rawCount(`SELECT COUNT(*) FROM review_command_replays`); got != 1 {
		t.Fatalf("identities after retry = %d", got)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBackfilledCommandReplayFollowsProjectLifecycle(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	head := reviewHead("a")
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayControl(t, legacy, 42, 7, 1, head, false)
	insertLegacyReplayReaction(t, legacy, 42, 7, 801, 1)
	insertLegacyReplayJob(t, legacy, "proven-801", "review.command",
		"review.command:42:801", legacyCommandPayload(42, 7, 801, "run"), "running")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_replays WHERE project_id = 42`); got != 1 {
		t.Fatalf("backfilled identities = %d", got)
	}
	if err := s.DeleteProject(ctx, 42); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_replays WHERE project_id = 42`); got != 0 {
		t.Fatalf("identities after deletion = %d", got)
	}
	mustProject(t, s, 42)
	result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 801, "run", head))
	if err != nil || result.Duplicate || result.Generation != 1 {
		t.Fatalf("re-registered project replay = %+v, %v", result, err)
	}
}

func TestEnqueueReviewLatestRejectsInconsistentCommandIdentity(t *testing.T) {
	ctx := context.Background()
	head := reviewHead("a")
	cases := []struct {
		name    string
		command store.ReviewCommand
		want    string
	}{
		{
			name:    "note without command",
			command: store.ReviewCommand{ProjectID: 42, MRIID: 7, NoteID: 555, HeadSHA: head},
			want:    "requires both note id and command",
		},
		{
			name:    "command without note",
			command: store.ReviewCommand{ProjectID: 42, MRIID: 7, Command: "run", HeadSHA: head},
			want:    "requires both note id and command",
		},
		{
			name:    "unknown command",
			command: store.ReviewCommand{ProjectID: 42, MRIID: 7, NoteID: 555, Command: "help", HeadSHA: head},
			want:    "unknown review command kind",
		},
		{
			name:    "run marked cancel",
			command: store.ReviewCommand{ProjectID: 42, MRIID: 7, NoteID: 555, Command: "run", Cancel: true},
			want:    "kind run conflicts with cancel",
		},
		{
			name:    "cancel marked run",
			command: store.ReviewCommand{ProjectID: 42, MRIID: 7, NoteID: 555, Command: "cancel", HeadSHA: head},
			want:    "kind cancel requires cancel",
		},
		{
			name:    "negative note without command",
			command: store.ReviewCommand{ProjectID: 42, MRIID: 7, NoteID: -1, HeadSHA: head},
			want:    "note id must not be negative",
		},
		{
			name: "payload note mismatch",
			command: store.ReviewCommand{
				ProjectID: 42, MRIID: 7, NoteID: 555, Command: "run", HeadSHA: head,
				Payload: json.RawMessage(`{"trigger":"command","note_id":556}`),
			},
			want: "payload note_id does not match",
		},
		{
			name: "payload note not an integer",
			command: store.ReviewCommand{
				ProjectID: 42, MRIID: 7, NoteID: 555, Command: "run", HeadSHA: head,
				Payload: json.RawMessage(`{"trigger":"command","note_id":"555"}`),
			},
			want: "payload note_id must be an integer",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t, newFakeClock())
			mustProject(t, s, 42)
			_, err := s.EnqueueReviewLatest(ctx, tt.command)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			for _, query := range []string{
				`SELECT COUNT(*) FROM review_command_replays`,
				`SELECT COUNT(*) FROM review_controls`,
				`SELECT COUNT(*) FROM jobs WHERE kind IN ('review.run', 'review.cancel', 'review.command')`,
			} {
				if got := rowCount(t, s, query); got != 0 {
					t.Fatalf("%s = %d, want 0", query, got)
				}
			}
		})
	}

	// A producer without a source-note identity keeps the old behavior,
	// including a payload note_id from a webhook command job.
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	result, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
		Payload: json.RawMessage(`{"trigger":"command","note_id":999}`),
	})
	if err != nil || result.Duplicate || result.Generation != 1 {
		t.Fatalf("identity-free enqueue = %+v, %v", result, err)
	}
}
