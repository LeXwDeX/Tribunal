package sqlite

import (
	"context"
	"fmt"
)

// SecretMigrationComplete reports whether the legacy secret migration has
// finished, after which legacy ciphertexts are no longer accepted.
func (s *Store) SecretMigrationComplete(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM secret_migration WHERE id = 1`).Scan(&n); err != nil {
		return false, fmt.Errorf("sqlite: read secret migration state: %w", err)
	}
	return n == 1, nil
}

// MarkSecretMigrationComplete records that no legacy ciphertext remains. It is
// idempotent and keeps the first completion time.
func (s *Store) MarkSecretMigrationComplete(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO secret_migration (id, completed_at) VALUES (1, ?) ON CONFLICT (id) DO NOTHING`,
		formatTime(s.now())); err != nil {
		return fmt.Errorf("sqlite: mark secret migration complete: %w", err)
	}
	return nil
}
