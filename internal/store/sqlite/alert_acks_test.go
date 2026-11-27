package sqlite

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// alertAckUser creates the users row an alert ack references.
func alertAckUser(t *testing.T, s *Store, id int64, username string, now time.Time) {
	t.Helper()
	session := testSession("alert-ack-"+username, now, time.Hour)
	session.UserID = id
	session.Username = username
	session.Name = username
	if err := s.CreateSession(context.Background(), session, store.SessionLimits{}); err != nil {
		t.Fatalf("CreateSession(%d): %v", id, err)
	}
}

func ackMap(t *testing.T, s *Store, userID int64) map[string]store.AlertAck {
	t.Helper()
	acks, err := s.ListAlertAcks(context.Background(), userID)
	if err != nil {
		t.Fatalf("ListAlertAcks(%d): %v", userID, err)
	}
	out := make(map[string]store.AlertAck, len(acks))
	for _, ack := range acks {
		out[ack.Key] = ack
	}
	return out
}

// seedAlertAcks inserts rows directly so cap tests do not pay for a
// thousand separate write transactions.
func seedAlertAcks(t *testing.T, s *Store, userID int64, prefix string, n int, base time.Time, step time.Duration) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(`INSERT INTO alert_acks (user_id, alert_key, fingerprint, acked_at) VALUES (?, ?, 'f', ?)`,
			userID, fmt.Sprintf("%s-%04d", prefix, i), formatTime(base.Add(time.Duration(i)*step))); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestAlertAcksAreIsolatedPerUser(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	alertAckUser(t, s, 7, "alice", clock.Now())
	alertAckUser(t, s, 8, "bob", clock.Now())

	at := clock.Now()
	saved, err := s.PutAlertAck(ctx, 7, store.AlertAck{Key: "workspace-42", Fingerprint: "a|b|c", AckedAt: at})
	if err != nil {
		t.Fatalf("PutAlertAck: %v", err)
	}
	if saved.Key != "workspace-42" || saved.Fingerprint != "a|b|c" || !saved.AckedAt.Equal(at) {
		t.Fatalf("saved = %+v", saved)
	}
	if got := ackMap(t, s, 7); len(got) != 1 || got["workspace-42"].Fingerprint != "a|b|c" {
		t.Fatalf("alice acks = %+v", got)
	}
	if got := ackMap(t, s, 8); len(got) != 0 {
		t.Fatalf("bob sees alice's acks: %+v", got)
	}
	// Deleting another user's key is a no-op for the owner.
	if err := s.DeleteAlertAck(ctx, 8, "workspace-42"); err != nil {
		t.Fatalf("DeleteAlertAck(bob): %v", err)
	}
	if got := ackMap(t, s, 7); len(got) != 1 {
		t.Fatalf("bob's delete removed alice's ack: %+v", got)
	}
	if err := s.DeleteAlertAck(ctx, 7, "workspace-42"); err != nil {
		t.Fatalf("DeleteAlertAck(alice): %v", err)
	}
	if got := ackMap(t, s, 7); len(got) != 0 {
		t.Fatalf("alice acks after delete = %+v", got)
	}
	// Deleting a missing key succeeds.
	if err := s.DeleteAlertAck(ctx, 7, "missing"); err != nil {
		t.Fatalf("DeleteAlertAck(missing): %v", err)
	}
}

func TestAlertAckPutOverwritesFingerprintAndTime(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	alertAckUser(t, s, 7, "alice", clock.Now())

	first := clock.Now()
	if _, err := s.PutAlertAck(ctx, 7, store.AlertAck{Key: "reviews-incomplete", Fingerprint: "2026-10-01T00:00:00Z", AckedAt: first}); err != nil {
		t.Fatal(err)
	}
	second := first.Add(time.Minute)
	if _, err := s.PutAlertAck(ctx, 7, store.AlertAck{Key: "reviews-incomplete", Fingerprint: "2026-10-02T00:00:00Z", AckedAt: second}); err != nil {
		t.Fatal(err)
	}
	got := ackMap(t, s, 7)
	if len(got) != 1 {
		t.Fatalf("acks = %+v, want one row", got)
	}
	if ack := got["reviews-incomplete"]; ack.Fingerprint != "2026-10-02T00:00:00Z" || !ack.AckedAt.Equal(second) {
		t.Fatalf("ack = %+v, want overwritten fingerprint and time", ack)
	}
}

func TestAlertAckPutEvictsOldestBeyondCap(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	alertAckUser(t, s, 7, "alice", clock.Now())
	alertAckUser(t, s, 8, "bob", clock.Now())
	if _, err := s.PutAlertAck(ctx, 8, store.AlertAck{Key: "bob-key", Fingerprint: "x", AckedAt: clock.Now()}); err != nil {
		t.Fatal(err)
	}

	base := clock.Now()
	seedAlertAcks(t, s, 7, "review", store.MaxAlertAcksPerUser-1, base, time.Second)
	last := fmt.Sprintf("review-%04d", store.MaxAlertAcksPerUser-1)
	if _, err := s.PutAlertAck(ctx, 7, store.AlertAck{Key: last, Fingerprint: "failed", AckedAt: base.Add(time.Duration(store.MaxAlertAcksPerUser-1) * time.Second)}); err != nil {
		t.Fatalf("PutAlertAck(%s): %v", last, err)
	}
	if got := ackMap(t, s, 7); len(got) != store.MaxAlertAcksPerUser {
		t.Fatalf("acks = %d, want %d", len(got), store.MaxAlertAcksPerUser)
	}
	// Refreshing the oldest key moves it to the newest position, so the
	// second oldest is evicted by the next insert.
	if _, err := s.PutAlertAck(ctx, 7, store.AlertAck{Key: "review-0000", Fingerprint: "failed", AckedAt: base.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutAlertAck(ctx, 7, store.AlertAck{Key: "review-new", Fingerprint: "failed", AckedAt: base.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got := ackMap(t, s, 7)
	if len(got) != store.MaxAlertAcksPerUser {
		t.Fatalf("acks = %d, want %d after eviction", len(got), store.MaxAlertAcksPerUser)
	}
	if _, ok := got["review-0001"]; ok {
		t.Fatal("oldest ack review-0001 was not evicted")
	}
	for _, key := range []string{"review-0000", "review-0002", "review-new"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("ack %s was evicted, want kept", key)
		}
	}
	if got := ackMap(t, s, 8); len(got) != 1 {
		t.Fatalf("eviction touched another user's acks: %+v", got)
	}
}

func TestAlertAckConcurrentPutsKeepCap(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	alertAckUser(t, s, 7, "alice", clock.Now())
	base := clock.Now()
	seedAlertAcks(t, s, 7, "seed", store.MaxAlertAcksPerUser-5, base, 0)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.PutAlertAck(ctx, 7, store.AlertAck{Key: fmt.Sprintf("race-%02d", i), Fingerprint: "f", AckedAt: base.Add(time.Duration(i+1) * time.Second)})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent PutAlertAck: %v", err)
		}
	}
	if got := ackMap(t, s, 7); len(got) != store.MaxAlertAcksPerUser {
		t.Fatalf("acks = %d, want cap %d", len(got), store.MaxAlertAcksPerUser)
	}
}

func TestAlertAcksCascadeWithUserDelete(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	alertAckUser(t, s, 7, "alice", clock.Now())
	if _, err := s.PutAlertAck(ctx, 7, store.AlertAck{Key: "token-42", Fingerprint: "a|b|warn", AckedAt: clock.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = 7`); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_acks WHERE user_id = 7`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("alert_acks rows after user delete = %d, want 0", count)
	}
}

func TestAlertAckPutRejectsUnknownUser(t *testing.T) {
	clock := newFakeClock()
	s := openTestStore(t, clock)
	if _, err := s.PutAlertAck(context.Background(), 99, store.AlertAck{Key: "k", Fingerprint: "f", AckedAt: clock.Now()}); err == nil {
		t.Fatal("PutAlertAck for a user without a row succeeded")
	}
	if _, err := s.PutAlertAck(context.Background(), 0, store.AlertAck{Key: "k", Fingerprint: "f", AckedAt: clock.Now()}); err == nil {
		t.Fatal("PutAlertAck for user 0 succeeded")
	}
}
