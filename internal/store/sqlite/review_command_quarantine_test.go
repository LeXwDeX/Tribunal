package sqlite

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestLegacyUncertainCommandsCannotReplay(t *testing.T) {
	for _, history := range []string{"accepted-and-pruned", "never-executed"} {
		t.Run(history, func(t *testing.T) {
			ctx := context.Background()
			path, legacy := openLegacyCommandReplayDB(t)
			head := reviewHead("a")
			insertLegacyReplayProject(t, legacy, 42)
			insertLegacyReplayControl(t, legacy, 42, 7, 2, head, true)
			insertLegacyReplayReaction(t, legacy, 42, 7, 555, 1)
			if _, err := legacy.Exec(`UPDATE review_reactions SET desired = 'none' WHERE note_id = 555`); err != nil {
				t.Fatal(err)
			}
			if _, err := legacy.Exec(`INSERT INTO review_status (project_id, mr_iid, generation, head_sha, state, updated_at)
				VALUES (42, 7, 2, ?, 'cancelled', ?)`, head, "2026-10-22T12:00:00.000000000Z"); err != nil {
				t.Fatal(err)
			}
			insertLegacyReplayJob(t, legacy, "legacy-command", "review.command", "review.command:42:555",
				legacyCommandPayload(42, 7, 555, "run"), "running")
			if history == "accepted-and-pruned" {
				insertLegacyReplayJob(t, legacy, "legacy-run", "review.run", "review:42:7",
					legacyReviewPayload(42, 7, 555, 1, head), "done")
				deleted, err := (&Store{db: legacy}).DeleteTerminalJobsBefore(ctx, newFakeClock().Now().Add(time.Hour))
				if err != nil || deleted != 1 {
					t.Fatalf("retention deleted %d jobs: %v", deleted, err)
				}
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(ctx, path, WithClock(newFakeClock().Now))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_quarantine
				WHERE project_id = 42 AND mr_iid = 7 AND note_id = 555 AND command = 'run'
				AND reason = 'legacy_acceptance_unproven'`); got != 1 {
				t.Fatalf("durable uncertainty rows = %d", got)
			}
			if got := commandReplayIdentityRows(t, s, 42, 7, 555, "run"); got != 0 {
				t.Fatalf("unproven acceptance rows = %d", got)
			}
			control, _ := s.GetReviewControl(ctx, 42, 7)
			status, _ := s.GetReviewStatus(ctx, 42, 7)
			reaction, _ := s.GetReviewReaction(ctx, 42, 7, 555)
			job, _ := s.GetJob(ctx, "legacy-command")
			result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 555, "run", head))
			if !errors.Is(err, store.ErrLegacyCommandUncertain) || !strings.Contains(err.Error(), "uncertain") || result.Duplicate {
				t.Errorf("indistinguishable legacy command = %+v, %v; want an explicit uncertainty, not acceptance or duplicate", result, err)
			}
			controlAfter, _ := s.GetReviewControl(ctx, 42, 7)
			statusAfter, _ := s.GetReviewStatus(ctx, 42, 7)
			reactionAfter, _ := s.GetReviewReaction(ctx, 42, 7, 555)
			jobAfter, _ := s.GetJob(ctx, "legacy-command")
			if !reflect.DeepEqual(control, controlAfter) || !reflect.DeepEqual(status, statusAfter) ||
				!reflect.DeepEqual(reaction, reactionAfter) || !reflect.DeepEqual(job, jobAfter) ||
				rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE kind IN ('review.run', 'review.cancel')`) != 0 {
				t.Error("uncertain replay changed review state or jobs")
			}
		})
	}
}

func TestLegacyQuarantineRestartConcurrentAndIsolation(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	head := reviewHead("a")
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayProject(t, legacy, 43)
	insertLegacyReplayControl(t, legacy, 42, 7, 2, head, true)
	insertLegacyReplayJob(t, legacy, "uncertain-run", "review.command", "review.command:42:555",
		legacyCommandPayload(42, 7, 555, "run"), "running")
	insertLegacyReplayJob(t, legacy, "uncertain-cancel", "review.command", "review.command:42:556",
		legacyCommandPayload(42, 7, 556, "cancel"), "done")
	insertLegacyReplayJob(t, legacy, "other-project-uncertain", "review.command", "review.command:43:900",
		legacyCommandPayload(43, 7, 900, "run"), "done")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []store.ReviewCommand{
		commandReplay(42, 8, 555, "run", head),
		commandReplay(43, 7, 555, "run", head),
	} {
		result, err := s.EnqueueReviewLatest(ctx, cmd)
		if err != nil || result.Duplicate || result.Generation != 1 {
			t.Fatalf("other project/MR = %+v, %v", result, err)
		}
	}
	// A direct delivery cannot commit a misleading enqueued/duplicate record.
	delivery := store.Delivery{ProjectID: 42, UUID: "uncertain-delivery", Event: "Note Hook", Outcome: store.DeliveryEnqueued}
	old := commandReplay(42, 7, 555, "run", head)
	for i := 0; i < 2; i++ {
		result, err := s.RecordReviewDelivery(ctx, delivery, &old)
		if !errors.Is(err, store.ErrLegacyCommandUncertain) || result.Duplicate {
			t.Fatalf("direct delivery %d = %+v, %v", i, result, err)
		}
	}
	// An old opposite-kind command can also have been pruned for this note.
	opposite, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 555, "cancel", ""))
	if !errors.Is(err, store.ErrLegacyCommandUncertain) || opposite.Duplicate || opposite.Generation != 0 {
		t.Fatalf("unproven opposite kind on quarantined note = %+v, %v", opposite, err)
	}
	if count := rowCount(t, s, `SELECT COUNT(*) FROM webhook_deliveries WHERE uuid = 'uncertain-delivery'`); count != 0 {
		t.Fatalf("uncertain delivery committed %d rows", count)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i < 16 {
				cmd := old
				if i%2 == 1 {
					cmd = commandReplay(42, 7, 556, "cancel", "")
				}
				result, err := s.EnqueueReviewLatest(ctx, cmd)
				if !errors.Is(err, store.ErrLegacyCommandUncertain) || result.Duplicate || result.Generation != 0 {
					errs <- fmt.Errorf("quarantined concurrent command = %+v, %v", result, err)
				}
				return
			}
			result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 557, "run", head))
			if err != nil || (!result.Duplicate && result.Generation != 3) || (result.Duplicate && result.Generation != 0) {
				errs <- fmt.Errorf("new concurrent command = %+v, %v", result, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil || control.Cancelled || control.Generation != 3 {
		t.Fatalf("only the new command should change the control: %+v, %v", control, err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_replays WHERE project_id = 42 AND mr_iid = 7`); got != 1 {
		t.Fatalf("only new command has acceptance proof, got %d rows", got)
	}
	// A terminal command may be pruned, but its uncertainty must survive.
	if _, err := s.DeleteTerminalJobsBefore(ctx, newFakeClock().Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, cmd := range []store.ReviewCommand{old, commandReplay(42, 7, 556, "cancel", "")} {
		result, err := s.EnqueueReviewLatest(ctx, cmd)
		if !errors.Is(err, store.ErrLegacyCommandUncertain) || result.Duplicate || result.Generation != 0 {
			t.Fatalf("after restart and pruning = %+v, %v", result, err)
		}
	}
	if err := s.DeleteProject(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_quarantine WHERE project_id = 42`); got != 0 {
		t.Fatalf("quarantine rows after project deletion = %d", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_quarantine WHERE project_id = 43`); got != 1 {
		t.Fatalf("other project's quarantine rows after deletion = %d", got)
	}
}

func TestLegacyQuarantineUsesWorkerVisibleIdentityWithoutClaimingProof(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayJob(t, legacy, "wrong-key", "review.command", "review.command:99:601",
		legacyCommandPayload(42, 7, 601, "run"), "running")
	insertLegacyReplayJobNullKey(t, legacy, "null-key", "review.command",
		legacyCommandPayload(42, 7, 602, "cancel"), "queued")
	insertLegacyReplayJob(t, legacy, "trailing-json", "review.command", "review.command:42:603",
		legacyCommandPayload(42, 7, 603, "run")+` {}`, "running")
	insertLegacyReplayJob(t, legacy, "duplicate-note", "review.command", "review.command:42:604",
		`{"project_id":42,"mr_iid":7,"note_id":999,"note_id":604,"author_id":12,"command":"run"}`, "queued")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, tc := range []struct {
		note int64
		kind string
	}{{601, "run"}, {602, "cancel"}, {603, "run"}, {604, "run"}} {
		head := reviewHead("a")
		if tc.kind == "cancel" {
			head = ""
		}
		result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, tc.note, tc.kind, head))
		if !errors.Is(err, store.ErrLegacyCommandUncertain) || result.Duplicate || result.Generation != 0 {
			t.Errorf("worker-visible identity %d/%s = %+v, %v", tc.note, tc.kind, result, err)
		}
		if count := commandReplayIdentityRows(t, s, 42, 7, tc.note, tc.kind); count != 0 {
			t.Errorf("inconsistent identity %d/%s falsely proved acceptance", tc.note, tc.kind)
		}
	}
}

func TestLegacyQuarantineBackfillIsAtomicAndRetries(t *testing.T) {
	ctx := context.Background()
	_, legacy := openLegacyCommandReplayDB(t)
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayJob(t, legacy, "unproven", "review.command", "review.command:42:555",
		legacyCommandPayload(42, 7, 555, "run"), "running")
	insertLegacyReplayJob(t, legacy, "proven", "review.run", "review:42:7",
		legacyReviewPayload(42, 7, 556, 1, reviewHead("a")), "done")
	applyCommandReplaySchema(t, legacy)
	if _, err := legacy.Exec(`CREATE TRIGGER reject_quarantine BEFORE INSERT ON review_command_quarantine
		BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := backfillReviewCommandReplays(ctx, legacy, newFakeClock().Now()); err == nil {
		t.Fatal("backfill succeeded despite quarantine failure")
	}
	for _, table := range []string{"review_command_replay_backfill", "review_command_replays", "review_command_quarantine"} {
		var count int
		if err := legacy.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed backfill left %d rows in %s: %v", count, table, err)
		}
	}
	if _, err := legacy.Exec(`DROP TRIGGER reject_quarantine`); err != nil {
		t.Fatal(err)
	}
	if err := backfillReviewCommandReplays(ctx, legacy, newFakeClock().Now()); err != nil {
		t.Fatalf("retry backfill: %v", err)
	}
	for _, table := range []string{"review_command_replay_backfill", "review_command_replays", "review_command_quarantine"} {
		var count int
		if err := legacy.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("retried backfill left %d rows in %s: %v", count, table, err)
		}
	}
}

func TestLegacyQuarantineRejectsIncompleteUnpublishedSchema(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	applyCommandReplaySchema(t, legacy)
	if _, err := legacy.Exec(`INSERT INTO review_command_replay_backfill VALUES (1, '2026-01-01T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`DROP TABLE review_command_quarantine`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err == nil {
		_ = s.Close()
		t.Fatal("older unpublished schema must not start without the quarantine table")
	}
	if !strings.Contains(err.Error(), "verify legacy command quarantine schema") {
		t.Fatalf("startup error = %v", err)
	}
}
