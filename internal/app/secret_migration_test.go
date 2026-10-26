package app

import (
	"context"
	"encoding/json"
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
