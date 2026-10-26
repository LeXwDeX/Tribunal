package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// projectSecretStore is the store surface the secret migration needs.
type projectSecretStore interface {
	ListProjects(ctx context.Context) ([]store.Project, error)
	RewrapProjectSecrets(ctx context.Context, id int64, oldToken, newToken, oldSecret, newSecret []byte) (bool, error)
	SecretMigrationComplete(ctx context.Context) (bool, error)
	MarkSecretMigrationComplete(ctx context.Context) error
}

// MigrateProjectSecrets rewrites legacy project ciphertexts in the sealed
// format bound to their project and field. It is idempotent and runs before
// the services start. Each field migrates independently: a field that cannot
// be decrypted is logged and left unchanged, since using it would fail the
// same way before the migration, and the other field still migrates. A row
// changed since it was listed is left alone.
//
// When a run leaves no legacy value behind, it records that the migration is
// complete. From then on legacy values are never decrypted: they are logged
// and left unchanged, so a legacy ciphertext copied into another row cannot be
// bound to that row. It returns the number of rewritten projects.
func MigrateProjectSecrets(ctx context.Context, st projectSecretStore, cipher *secret.Cipher, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	complete, err := st.SecretMigrationComplete(ctx)
	if err != nil {
		return 0, fmt.Errorf("app: read secret migration state: %w", err)
	}
	projects, err := st.ListProjects(ctx)
	if err != nil {
		return 0, fmt.Errorf("app: list projects for secret migration: %w", err)
	}
	if complete {
		rejectLegacyProjectSecrets(projects, logger)
		return 0, nil
	}
	migrated := 0
	pending := false
	for _, p := range projects {
		// Each field migrates on its own: a damaged webhook secret must not
		// keep a usable token in the legacy format, which Open rejects.
		token, tokenChanged, err := cipher.Rewrap(p.TokenEnc, secret.TokenAAD(p.ID))
		if err != nil {
			logger.Warn("stored project token cannot be decrypted; left unchanged", "project_id", p.ID, "field", "token")
			token, tokenChanged = p.TokenEnc, false
			pending = pending || isLegacySecret(p.TokenEnc)
		}
		hook, hookChanged, err := cipher.Rewrap(p.WebhookSecretEnc, secret.WebhookSecretAAD(p.ID))
		if err != nil {
			logger.Warn("stored webhook secret cannot be decrypted; left unchanged", "project_id", p.ID, "field", "webhook_secret")
			hook, hookChanged = p.WebhookSecretEnc, false
			pending = pending || isLegacySecret(p.WebhookSecretEnc)
		}
		if !tokenChanged && !hookChanged {
			continue
		}
		ok, err := st.RewrapProjectSecrets(ctx, p.ID, p.TokenEnc, token, p.WebhookSecretEnc, hook)
		if err != nil {
			return migrated, fmt.Errorf("app: rewrap project %d secrets: %w", p.ID, err)
		}
		if !ok {
			// The row's current values are unknown; check again next start.
			logger.Warn("project secrets changed during migration; skipped", "project_id", p.ID)
			pending = true
			continue
		}
		migrated++
	}
	if !pending {
		if err := st.MarkSecretMigrationComplete(ctx); err != nil {
			return migrated, fmt.Errorf("app: mark secret migration complete: %w", err)
		}
	}
	return migrated, nil
}

// rejectLegacyProjectSecrets reports legacy values found after the migration
// completed. They are left unchanged; Open rejects them, so the token or
// webhook secret must be set again. Only the project and field are logged.
func rejectLegacyProjectSecrets(projects []store.Project, logger *slog.Logger) {
	for _, p := range projects {
		if isLegacySecret(p.TokenEnc) {
			logger.Warn("legacy project secret rejected after migration completed; left unchanged", "project_id", p.ID, "field", "token")
		}
		if isLegacySecret(p.WebhookSecretEnc) {
			logger.Warn("legacy project secret rejected after migration completed; left unchanged", "project_id", p.ID, "field", "webhook_secret")
		}
	}
}

// isLegacySecret reports whether a stored value is a non-empty value outside
// the sealed format.
func isLegacySecret(value []byte) bool {
	return len(value) > 0 && !secret.IsSealed(value)
}
