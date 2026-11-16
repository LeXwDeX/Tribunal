package sqlite

import (
	"bytes"
	"context"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestRewrapProjectSecretsComparesStoredValues(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 1)
	before, err := s.GetProject(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	// A stale expected value must not overwrite the stored one.
	ok, err := s.RewrapProjectSecrets(ctx, 1, []byte("stale"), []byte("new-token"), before.WebhookSecretEnc, []byte("new-secret"))
	if err != nil || ok {
		t.Fatalf("stale rewrap = %v, %v; want false", ok, err)
	}
	ok, err = s.RewrapProjectSecrets(ctx, 1, before.TokenEnc, []byte("new-token"), before.WebhookSecretEnc, []byte("new-secret"))
	if err != nil || !ok {
		t.Fatalf("rewrap = %v, %v; want true", ok, err)
	}
	after, err := s.GetProject(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after.TokenEnc, []byte("new-token")) || !bytes.Equal(after.WebhookSecretEnc, []byte("new-secret")) {
		t.Fatalf("stored = %q, %q", after.TokenEnc, after.WebhookSecretEnc)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("rewrap changed updated_at: %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}
	if ok, err := s.RewrapProjectSecrets(ctx, 99, nil, []byte("x"), nil, nil); err != nil || ok {
		t.Fatalf("missing project rewrap = %v, %v", ok, err)
	}
	// An empty token is the shared-credential sentinel, not a corrupt value.
	if ok, err := s.RewrapProjectSecrets(ctx, 1, after.TokenEnc, nil, after.WebhookSecretEnc, nil); err != nil || !ok {
		t.Fatalf("sentinel rewrap = %v, %v; want true", ok, err)
	}
	sentinel, err := s.GetProject(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(sentinel.TokenEnc) != 0 {
		t.Fatalf("sentinel token = %q, want zero length", sentinel.TokenEnc)
	}
}

func TestRewrapProjectSecretsPreservesSharedSentinel(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	if _, err := s.RegisterProject(ctx, store.Project{
		ID: 42, PathWithNamespace: "group/shared", WebURL: "https://gitlab.example.com/group/shared",
		DefaultBranch: "main", TokenEnc: []byte{}, WebhookSecretEnc: []byte("legacy-secret"),
		Profile: "default", Settings: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.TokenEnc) != 0 {
		t.Fatalf("seed token = %q, want sentinel", before.TokenEnc)
	}
	ok, err := s.RewrapProjectSecrets(ctx, 42, before.TokenEnc, []byte{}, before.WebhookSecretEnc, []byte("sealed-secret"))
	if err != nil || !ok {
		t.Fatalf("sentinel rewrap = %v, %v; want true", ok, err)
	}
	after, err := s.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.TokenEnc) != 0 || !bytes.Equal(after.WebhookSecretEnc, []byte("sealed-secret")) {
		t.Fatalf("rewrapped project = %+v", after)
	}
}
