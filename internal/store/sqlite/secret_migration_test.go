package sqlite

import (
	"context"
	"testing"
)

func TestSecretMigrationMarkerIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	if done, err := s.SecretMigrationComplete(ctx); err != nil || done {
		t.Fatalf("new store complete = %v, %v; want false", done, err)
	}
	for i := 0; i < 2; i++ {
		if err := s.MarkSecretMigrationComplete(ctx); err != nil {
			t.Fatalf("mark %d: %v", i, err)
		}
	}
	if done, err := s.SecretMigrationComplete(ctx); err != nil || !done {
		t.Fatalf("marked store complete = %v, %v; want true", done, err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM secret_migration`); got != 1 {
		t.Fatalf("marker rows = %d, want 1", got)
	}
}
