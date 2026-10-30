package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkCommandReplayBackfillManyMRs measures one first-time backfill with
// many candidate merge requests and unrelated retained review jobs. It is a
// benchmark, not a regular test, so it has no timing threshold in CI.
func BenchmarkCommandReplayBackfillManyMRs(b *testing.B) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(b.TempDir(), "bench.db"), WithClock(newFakeClock().Now))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	stamp := "2026-10-22T12:00:00.000000000Z"
	if _, err := s.db.Exec(`INSERT INTO projects
		(id, path_with_namespace, web_url, default_branch, token_enc, webhook_secret_enc, created_at, updated_at)
		VALUES (42, 'group/bench', 'https://example.test/group/bench', 'main', x'01', x'02', ?, ?)`,
		stamp, stamp); err != nil {
		b.Fatal(err)
	}

	const candidates = 100
	const unrelated = 2000
	tx, err := s.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	insert := func(id, kind, key, payload, state string) {
		if _, err := tx.Exec(`INSERT INTO jobs
			(id, kind, key, payload, state, attempts, max_attempts, run_after, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 0, 3, ?, ?, ?)`,
			id, kind, key, payload, state, stamp, stamp, stamp); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < candidates; i++ {
		mrIID := int64(i + 1)
		noteID := int64(1_000_000 + i)
		insert(fmt.Sprintf("candidate-%d", i), "review.command",
			fmt.Sprintf("review.command:42:%d", noteID),
			fmt.Sprintf(`{"project_id":42,"mr_iid":%d,"note_id":%d,"command":"run","trigger":"command"}`, mrIID, noteID),
			"queued")
	}
	for i := 0; i < unrelated; i++ {
		mrIID := int64(100_000 + i)
		noteID := int64(2_000_000 + i)
		payload := fmt.Sprintf(`{"project_id":42,"mr_iid":%d,"note_id":%d,"trigger":"command"}`, mrIID, noteID)
		insert(fmt.Sprintf("run-%d", i), "review.run", fmt.Sprintf("review:42:%d", mrIID), payload, "queued")
		insert(fmt.Sprintf("cancel-%d", i), "review.cancel", fmt.Sprintf("review.cancel:42:%d", mrIID), payload, "queued")
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if _, err := s.db.Exec(`DELETE FROM review_command_replay_backfill`); err != nil {
			b.Fatal(err)
		}
		if _, err := s.db.Exec(`DELETE FROM review_command_replays`); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := backfillReviewCommandReplays(ctx, s.db, time.Now()); err != nil {
			b.Fatal(err)
		}
	}
}
