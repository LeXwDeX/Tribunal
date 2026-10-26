package sqlite

import (
	"bytes"
	"context"
	"testing"
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
	if _, err := s.RewrapProjectSecrets(ctx, 1, after.TokenEnc, nil, after.WebhookSecretEnc, nil); err == nil {
		t.Fatal("empty token rewrap succeeded")
	}
}
