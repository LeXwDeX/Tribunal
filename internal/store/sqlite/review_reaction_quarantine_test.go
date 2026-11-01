package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestLegacyReactionQuarantineDoesNotInferAcceptance(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	head := reviewHead("a")
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayProject(t, legacy, 43)
	insertLegacyReplayControl(t, legacy, 42, 7, 2, head, true)
	// The command and current intent agree, but the reaction could have come
	// from a different, already-pruned command on the same note.
	insertLegacyReplayReaction(t, legacy, 42, 7, 555, 2)
	insertLegacyReplayJob(t, legacy, "unproven-cancel", "review.command", "review.command:42:555", legacyCommandPayload(42, 7, 555, "cancel"), "running")
	insertLegacyReplayReaction(t, legacy, 42, 7, 560, 1)
	insertLegacyReplayJob(t, legacy, "proven-run", "review.run", "review:42:7", legacyReviewPayload(42, 7, 560, 1, head), "done")
	for _, note := range []int64{561, 1 << 53, math.MaxInt64} {
		insertLegacyReplayReaction(t, legacy, 42, 7, note, 1)
	}
	insertLegacyReplayReaction(t, legacy, 43, 7, 999, 1)
	if _, err := legacy.Exec(`UPDATE review_reactions SET desired = 'none' WHERE project_id = 42 AND note_id = 561`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	beforeControl, _ := s.GetReviewControl(ctx, 42, 7)
	beforeStatus, statusErr := s.GetReviewStatus(ctx, 42, 7)
	beforeReaction, _ := s.GetReviewReaction(ctx, 42, 7, 561)
	for _, note := range []int64{555, 561, 1 << 53, math.MaxInt64} {
		for _, kind := range []string{"run", "cancel"} {
			commandHead := head
			if kind == "cancel" {
				commandHead = ""
			}
			result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, note, kind, commandHead))
			if !errors.Is(err, store.ErrLegacyCommandUncertain) || result.Duplicate || result.Generation != 0 {
				t.Errorf("note %d/%s = %+v, %v; want uncertain, not acceptance", note, kind, result, err)
			}
		}
	}
	if got := commandReplayIdentityRows(t, s, 42, 7, 555, "cancel"); got != 0 {
		t.Errorf("reaction falsely proved a retained command: %d", got)
	}
	proven, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 560, "run", head))
	if err != nil || !proven.Duplicate || proven.JobID != "proven-run" {
		t.Errorf("independently proven command = %+v, %v", proven, err)
	}
	opposite, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 560, "cancel", ""))
	if !errors.Is(err, store.ErrLegacyCommandUncertain) || opposite.Duplicate {
		t.Errorf("unproven other kind = %+v, %v", opposite, err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_quarantine WHERE project_id = 42 AND mr_iid = 7 AND note_id = 9223372036854775807 AND command = ''`); got != 1 {
		t.Errorf("maximum int64 reaction quarantine = %d", got)
	}
	delivery := store.Delivery{ProjectID: 42, UUID: "reaction-uncertain", Event: "Note Hook", Outcome: store.DeliveryEnqueued}
	cmd := commandReplay(42, 7, 561, "run", head)
	if result, err := s.RecordReviewDelivery(ctx, delivery, &cmd); !errors.Is(err, store.ErrLegacyCommandUncertain) || result.Duplicate {
		t.Errorf("direct uncertain delivery = %+v, %v", result, err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM webhook_deliveries WHERE uuid = 'reaction-uncertain'`); got != 0 {
		t.Errorf("uncertain delivery committed %d rows", got)
	}
	afterControl, _ := s.GetReviewControl(ctx, 42, 7)
	afterStatus, afterStatusErr := s.GetReviewStatus(ctx, 42, 7)
	afterReaction, _ := s.GetReviewReaction(ctx, 42, 7, 561)
	if !reflect.DeepEqual(beforeControl, afterControl) || !reflect.DeepEqual(beforeStatus, afterStatus) ||
		!errors.Is(statusErr, store.ErrNotFound) || !errors.Is(afterStatusErr, store.ErrNotFound) || !reflect.DeepEqual(beforeReaction, afterReaction) {
		t.Error("unproven replay changed control, status or reaction")
	}
	for _, fresh := range []store.ReviewCommand{
		commandReplay(42, 7, 777, "run", head),
		commandReplay(42, 7, (1<<53)+1, "run", head),
		commandReplay(42, 8, 555, "run", head),
		commandReplay(43, 7, 555, "run", head),
		commandReplay(42, 9, math.MaxInt64-1, "run", head),
	} {
		if result, err := s.EnqueueReviewLatest(ctx, fresh); err != nil || result.Duplicate || result.Generation == 0 {
			t.Errorf("new or isolated note = %+v, %v", result, err)
		}
	}
	if _, err := s.DeleteTerminalJobsBefore(ctx, newFakeClock().Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if reaction, err := s.GetReviewReaction(ctx, 42, 7, 561); err != nil || !reflect.DeepEqual(beforeReaction, reaction) {
		t.Fatalf("reaction changed during retention: %+v, %v", reaction, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := s.EnqueueReviewLatest(ctx, cmd); !errors.Is(err, store.ErrLegacyCommandUncertain) || result.Duplicate {
		t.Errorf("reaction quarantine after restart = %+v, %v", result, err)
	}
	if result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 777, "run", head)); err != nil || !result.Duplicate {
		t.Errorf("new note after restart = %+v, %v", result, err)
	}
	if err := s.DeleteProject(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_quarantine WHERE project_id = 42`); got != 0 {
		t.Errorf("deleted project retains %d quarantine rows", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_quarantine WHERE project_id = 43 AND note_id = 999`); got != 1 {
		t.Errorf("other project's reaction quarantine = %d", got)
	}
}

func TestReactionQuarantineBackfillRollbackAndConcurrentFirstRun(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayReaction(t, legacy, 42, 7, 555, 1)
	insertLegacyReplayJob(t, legacy, "proven-run", "review.run", "review:42:7", legacyReviewPayload(42, 7, 556, 1, reviewHead("a")), "done")
	applyCommandReplaySchema(t, legacy)
	if _, err := legacy.Exec(`CREATE TRIGGER reject_reaction_quarantine BEFORE INSERT ON review_command_quarantine
		WHEN NEW.command = '' BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := backfillReviewCommandReplays(ctx, legacy, newFakeClock().Now()); err == nil {
		t.Fatal("reaction quarantine insertion failure was ignored")
	}
	for _, table := range []string{"review_command_replay_backfill", "review_command_replays", "review_command_quarantine"} {
		var n int
		if err := legacy.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("failed backfill left %d rows in %s: %v", n, table, err)
		}
	}
	if _, err := legacy.Exec(`DROP TRIGGER reject_reaction_quarantine`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	first, err := sql.Open("sqlite", withPragmas(path))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := sql.Open("sqlite", withPragmas(path))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
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
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM review_command_replay_backfill`:                                 1,
		`SELECT COUNT(*) FROM review_command_replays WHERE note_id = 556 AND command = 'run'`: 1,
		`SELECT COUNT(*) FROM review_command_replays WHERE note_id = 555`:                     0,
		`SELECT COUNT(*) FROM review_command_quarantine WHERE note_id = 555 AND command = ''`: 1,
	} {
		var got int
		if err := first.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %d, %v; want %d", query, got, err, want)
		}
	}
	insertLegacyReplayReaction(t, first, 42, 7, 777, 2)
	if err := backfillReviewCommandReplays(ctx, first, newFakeClock().Now()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := first.QueryRow(`SELECT COUNT(*) FROM review_command_quarantine WHERE note_id = 777`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("post-backfill reaction was quarantined: %d, %v", count, err)
	}
}

func TestReactionQuarantineRejectsOldUnpublishedMarker(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	applyCommandReplaySchema(t, legacy)
	if _, err := legacy.Exec(`DROP TABLE review_command_replay_backfill;
		CREATE TABLE review_command_replay_backfill (id INTEGER PRIMARY KEY CHECK (id = 1), completed_at TEXT NOT NULL);
		INSERT INTO review_command_replay_backfill VALUES (1, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err == nil {
		_ = s.Close()
		t.Fatal("unpublished marker predating reaction quarantine was silently accepted")
	}
	if !strings.Contains(err.Error(), "reaction_quarantine") {
		t.Fatalf("unexpected startup error: %v", err)
	}
}
