package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

func seedLegacyProject(t *testing.T, st *sqlite.Store, cipher *secret.Cipher, id int64, token, hook string) {
	t.Helper()
	tokenEnc, err := cipher.Encrypt([]byte(token))
	if err != nil {
		t.Fatal(err)
	}
	var hookEnc []byte
	if hook != "" {
		if hookEnc, err = cipher.Encrypt([]byte(hook)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RegisterProject(context.Background(), store.Project{
		ID: id, PathWithNamespace: "group/legacy", WebURL: "https://gitlab.example.com/group/legacy",
		DefaultBranch: "main", TokenEnc: tokenEnc, TokenScopes: []string{"api"},
		WebhookSecretEnc: hookEnc, Profile: "default", Settings: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateProjectSecretsUpgradesLegacyOnce(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	seedLegacyProject(t, st, cipher, 10, "glpat-legacy", "whsec-legacy")
	seedLegacyProject(t, st, cipher, 11, "glpat-no-hook", "")
	seedProject(t, st, cipher, 12, "whsec-sealed", `{}`, "default")

	migrated, err := MigrateProjectSecrets(ctx, st, cipher, nil)
	if err != nil || migrated != 2 {
		t.Fatalf("first migration = %d, %v; want 2", migrated, err)
	}
	p, err := st.GetProject(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := cipher.Open(p.TokenEnc, secret.TokenAAD(10)); err != nil || string(got) != "glpat-legacy" {
		t.Fatalf("token after migration = %q, %v", got, err)
	}
	if got, err := cipher.Open(p.WebhookSecretEnc, secret.WebhookSecretAAD(10)); err != nil || string(got) != "whsec-legacy" {
		t.Fatalf("webhook secret after migration = %q, %v", got, err)
	}
	if p, err := st.GetProject(ctx, 11); err != nil || len(p.WebhookSecretEnc) != 0 {
		t.Fatalf("empty webhook secret changed: %+v, %v", p, err)
	}

	again, err := MigrateProjectSecrets(ctx, st, cipher, nil)
	if err != nil || again != 0 {
		t.Fatalf("second migration = %d, %v; want 0", again, err)
	}
}

// staleProjectStore lists a snapshot taken before a concurrent rotation.
type staleProjectStore struct {
	*sqlite.Store
	snapshot []store.Project
}

func (s *staleProjectStore) ListProjects(context.Context) ([]store.Project, error) {
	return s.snapshot, nil
}

func TestMigrateProjectSecretsSkipsConcurrentChangeAndBadRows(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	seedLegacyProject(t, st, cipher, 20, "glpat-old", "whsec")
	seedLegacyProject(t, st, cipher, 21, "glpat-other", "whsec")
	snapshot, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Project 20 is rotated after the snapshot; project 21 is corrupted.
	rotated, err := cipher.Seal([]byte("glpat-rotated"), secret.TokenAAD(20))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateProjectToken(ctx, 20, store.ProjectTokenUpdate{TokenEnc: rotated, Scopes: []string{"api"}}); err != nil {
		t.Fatal(err)
	}
	for i := range snapshot {
		if snapshot[i].ID == 21 {
			snapshot[i].TokenEnc = []byte("not a ciphertext")
		}
	}

	migrated, err := MigrateProjectSecrets(ctx, &staleProjectStore{Store: st, snapshot: snapshot}, cipher, nil)
	if err != nil || migrated != 0 {
		t.Fatalf("migration = %d, %v; want 0", migrated, err)
	}
	p, err := st.GetProject(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := cipher.Open(p.TokenEnc, secret.TokenAAD(20)); err != nil || string(got) != "glpat-rotated" {
		t.Fatalf("concurrent rotation was overwritten: %q, %v", got, err)
	}
}

func TestMigrateProjectSecretsUpgradesUsableFieldBesideDamagedOne(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	tokenEnc, err := cipher.Encrypt([]byte("glpat-usable"))
	if err != nil {
		t.Fatal(err)
	}
	damaged := []byte("damaged webhook secret")
	if _, err := st.RegisterProject(ctx, store.Project{
		ID: 30, PathWithNamespace: "group/half", WebURL: "https://gitlab.example.com/group/half",
		DefaultBranch: "main", TokenEnc: tokenEnc, TokenScopes: []string{"api"},
		WebhookSecretEnc: damaged, Profile: "default", Settings: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	migrated, err := MigrateProjectSecrets(ctx, st, cipher, nil)
	if err != nil || migrated != 1 {
		t.Fatalf("migration = %d, %v; want 1", migrated, err)
	}
	p, err := st.GetProject(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}
	// The usable token keeps working; the damaged secret is left for a rebuild.
	if got, err := cipher.Open(p.TokenEnc, secret.TokenAAD(30)); err != nil || string(got) != "glpat-usable" {
		t.Fatalf("token after migration = %q, %v", got, err)
	}
	if string(p.WebhookSecretEnc) != string(damaged) {
		t.Fatalf("damaged webhook secret was rewritten: %q", p.WebhookSecretEnc)
	}
	if again, err := MigrateProjectSecrets(ctx, st, cipher, nil); err != nil || again != 0 {
		t.Fatalf("second migration = %d, %v; want 0", again, err)
	}
	// A damaged legacy value is still not in the sealed format, so the
	// migration is not complete and a later start keeps trying.
	if done, err := st.SecretMigrationComplete(ctx); err != nil || done {
		t.Fatalf("migration complete = %v, %v; want false", done, err)
	}
}

func TestMigrateProjectSecretsMarksFreshInstallComplete(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	if done, err := st.SecretMigrationComplete(ctx); err != nil || done {
		t.Fatalf("fresh store complete = %v, %v; want false", done, err)
	}
	if migrated, err := MigrateProjectSecrets(ctx, st, cipher, nil); err != nil || migrated != 0 {
		t.Fatalf("migration = %d, %v; want 0", migrated, err)
	}
	if done, err := st.SecretMigrationComplete(ctx); err != nil || !done {
		t.Fatalf("fresh install complete = %v, %v; want true", done, err)
	}
}

// Once every legacy value is migrated, a legacy value written later (for
// example one copied from another project) is never accepted again.
func TestMigrateProjectSecretsRejectsLegacyAfterCompletion(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	seedLegacyProject(t, st, cipher, 40, "glpat-forty", "whsec-forty")
	seedProject(t, st, cipher, 41, "whsec-sealed", `{}`, "default")
	// Keep project 40's legacy ciphertexts before the first migration.
	before, err := st.GetProject(ctx, 40)
	if err != nil {
		t.Fatal(err)
	}

	if migrated, err := MigrateProjectSecrets(ctx, st, cipher, nil); err != nil || migrated != 1 {
		t.Fatalf("first migration = %d, %v; want 1", migrated, err)
	}
	if done, err := st.SecretMigrationComplete(ctx); err != nil || !done {
		t.Fatalf("migration complete = %v, %v; want true", done, err)
	}

	// Replay project 40's old ciphertexts into project 41.
	target, err := st.GetProject(ctx, 41)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.RewrapProjectSecrets(ctx, 41, target.TokenEnc, before.TokenEnc,
		target.WebhookSecretEnc, before.WebhookSecretEnc); err != nil || !ok {
		t.Fatalf("plant legacy values = %v, %v", ok, err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	if migrated, err := MigrateProjectSecrets(ctx, st, cipher, logger); err != nil || migrated != 0 {
		t.Fatalf("migration after completion = %d, %v; want 0", migrated, err)
	}
	after, err := st.GetProject(ctx, 41)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after.TokenEnc, before.TokenEnc) || !bytes.Equal(after.WebhookSecretEnc, before.WebhookSecretEnc) {
		t.Fatal("legacy values were rewritten after the migration completed")
	}
	if _, err := cipher.Open(after.TokenEnc, secret.TokenAAD(41)); err == nil {
		t.Fatal("replayed legacy token opens for project 41")
	}
	out := logs.String()
	for _, want := range []string{"level=WARN", "project_id=41", "field=token", "field=webhook_secret"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log %q missing %q", out, want)
		}
	}
	for _, secretText := range []string{"glpat-forty", "whsec-forty", string(before.TokenEnc)} {
		if strings.Contains(out, secretText) {
			t.Fatalf("log leaks secret material: %q", out)
		}
	}
}
