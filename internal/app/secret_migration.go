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
}

// MigrateProjectSecrets rewrites legacy project ciphertexts in the sealed
// format bound to their project and field. It is idempotent and runs before
// the services start. Each field migrates independently: a field that cannot
// be decrypted is logged and left unchanged, since using it would fail the
// same way before the migration, and the other field still migrates. A row
// changed since it was listed is left alone. It returns the number of rewritten projects.
func MigrateProjectSecrets(ctx context.Context, st projectSecretStore, cipher *secret.Cipher, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	projects, err := st.ListProjects(ctx)
	if err != nil {
		return 0, fmt.Errorf("app: list projects for secret migration: %w", err)
	}
	migrated := 0
	for _, p := range projects {
		// Each field migrates on its own: a damaged webhook secret must not
		// keep a usable token in the legacy format, which Open rejects.
		token, tokenChanged, err := cipher.Rewrap(p.TokenEnc, secret.TokenAAD(p.ID))
		if err != nil {
			logger.Warn("stored project token cannot be decrypted; left unchanged", "project_id", p.ID)
			token, tokenChanged = p.TokenEnc, false
		}
		hook, hookChanged, err := cipher.Rewrap(p.WebhookSecretEnc, secret.WebhookSecretAAD(p.ID))
		if err != nil {
			logger.Warn("stored webhook secret cannot be decrypted; left unchanged", "project_id", p.ID)
			hook, hookChanged = p.WebhookSecretEnc, false
		}
		if !tokenChanged && !hookChanged {
			continue
		}
		ok, err := st.RewrapProjectSecrets(ctx, p.ID, p.TokenEnc, token, p.WebhookSecretEnc, hook)
		if err != nil {
			return migrated, fmt.Errorf("app: rewrap project %d secrets: %w", p.ID, err)
		}
		if !ok {
			logger.Warn("project secrets changed during migration; skipped", "project_id", p.ID)
			continue
		}
		migrated++
	}
	return migrated, nil
}
