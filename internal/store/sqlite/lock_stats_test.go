package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestStatsReportsConnectionWait(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		var n int
		done <- s.db.QueryRowContext(ctx, "SELECT 1").Scan(&n)
	}()
	time.Sleep(20 * time.Millisecond)
	if err := conn.Close(); err != nil {
		t.Fatalf("close held connection: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("query: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("query did not finish")
	}

	stats := s.DBStats()
	if stats.WaitCount < 1 {
		t.Fatalf("WaitCount = %d, want at least 1", stats.WaitCount)
	}
	if stats.WaitDuration <= 0 {
		t.Fatalf("WaitDuration = %s, want a positive duration", stats.WaitDuration)
	}
	if stats.MaxOpenConnections != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1", stats.MaxOpenConnections)
	}
}
