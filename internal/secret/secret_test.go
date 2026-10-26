package secret

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return key
}

func TestNewRejectsWrongKeyLength(t *testing.T) {
	for _, n := range []int{0, 1, 16, 31, 33, 64} {
		if _, err := New(make([]byte, n)); err == nil {
			t.Errorf("New(%d-byte key) succeeded, want error", n)
		}
	}
	if _, err := New(make([]byte, 32)); err != nil {
		t.Fatalf("New(32-byte key): %v", err)
	}
}

func TestRoundTrip(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}

	cases := [][]byte{
		nil,
		{},
		[]byte("project token"),
		bytes.Repeat([]byte("x"), 4096),
	}
	for _, plaintext := range cases {
		ciphertext, err := c.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt(%d bytes): %v", len(plaintext), err)
		}
		got, err := c.Decrypt(ciphertext)
		if err != nil {
			t.Fatalf("Decrypt(%d bytes): %v", len(plaintext), err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("round trip mismatch: got %q want %q", got, plaintext)
		}
	}
}

func TestCiphertextLayoutAndNonceRandomness(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}

	plaintext := []byte("same plaintext")
	first, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}

	if len(first) != c.aead.NonceSize()+len(plaintext)+c.aead.Overhead() {
		t.Fatalf("ciphertext length = %d, want nonce+plaintext+tag", len(first))
	}
	if bytes.Equal(first, second) {
		t.Fatal("two encryptions produced identical ciphertext; nonce is not random")
	}
	if bytes.Equal(first[:c.aead.NonceSize()], second[:c.aead.NonceSize()]) {
		t.Fatal("two encryptions produced identical nonce")
	}
}

func TestDecryptRejectsTampering(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := c.Encrypt([]byte("webhook secret"))
	if err != nil {
		t.Fatal(err)
	}

	for _, pos := range []int{0, c.aead.NonceSize(), len(ciphertext) - 1} {
		tampered := bytes.Clone(ciphertext)
		tampered[pos] ^= 0xff
		if _, err := c.Decrypt(tampered); err == nil {
			t.Errorf("Decrypt accepted tampered byte at position %d", pos)
		}
	}
}

func TestDecryptRejectsTruncatedCiphertext(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := c.Encrypt([]byte("webhook secret"))
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range []int{0, 1, c.aead.NonceSize() - 1, c.aead.NonceSize(), len(ciphertext) - 1} {
		if _, err := c.Decrypt(ciphertext[:n]); err == nil {
			t.Errorf("Decrypt accepted %d-byte truncated ciphertext", n)
		}
	}
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	writer, _ := New(testKey(t))
	reader, _ := New(testKey(t))
	ciphertext, err := writer.Encrypt([]byte("project token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Decrypt(ciphertext); err == nil {
		t.Fatal("Decrypt with a different key succeeded")
	}
}

func TestSealBindsProjectAndField(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.Seal([]byte("glpat-token"), TokenAAD(7))
	if err != nil {
		t.Fatal(err)
	}
	if !IsSealed(sealed) {
		t.Fatal("sealed value lacks the versioned prefix")
	}
	got, err := c.Open(sealed, TokenAAD(7))
	if err != nil || string(got) != "glpat-token" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	for name, aad := range map[string][]byte{
		"other project": TokenAAD(8),
		"other field":   WebhookSecretAAD(7),
		"nil":           nil,
	} {
		if _, err := c.Open(sealed, aad); err == nil {
			t.Fatalf("Open with %s AAD succeeded", name)
		}
	}
}

func TestOpenRejectsLegacyAndTamperedValues(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := c.Encrypt([]byte("legacy"))
	if err != nil {
		t.Fatal(err)
	}
	if IsSealed(legacy) && bytes.HasPrefix(legacy, []byte(sealedPrefix)) {
		t.Skip("random nonce collided with the prefix")
	}
	if _, err := c.Open(legacy, TokenAAD(1)); err == nil {
		t.Fatal("Open accepted a legacy value")
	}
	sealed, err := c.Seal([]byte("value"), TokenAAD(1))
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(sealed)-1] ^= 0xff
	if _, err := c.Open(sealed, TokenAAD(1)); err == nil {
		t.Fatal("Open accepted a tampered value")
	}
	if _, err := c.Open([]byte(sealedPrefix), TokenAAD(1)); err == nil {
		t.Fatal("Open accepted a truncated value")
	}
	var nilCipher *Cipher
	if _, err := nilCipher.Seal([]byte("x"), nil); err == nil {
		t.Fatal("nil cipher Seal succeeded")
	}
	if _, err := nilCipher.Open(sealed, nil); err == nil {
		t.Fatal("nil cipher Open succeeded")
	}
}

func TestRewrapUpgradesLegacyAndKeepsSealed(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := c.Encrypt([]byte("legacy-secret"))
	if err != nil {
		t.Fatal(err)
	}
	upgraded, changed, err := c.Rewrap(legacy, WebhookSecretAAD(3))
	if err != nil || !changed {
		t.Fatalf("Rewrap legacy = changed %v, %v", changed, err)
	}
	if got, err := c.Open(upgraded, WebhookSecretAAD(3)); err != nil || string(got) != "legacy-secret" {
		t.Fatalf("Open upgraded = %q, %v", got, err)
	}
	same, changed, err := c.Rewrap(upgraded, WebhookSecretAAD(3))
	if err != nil || changed || !bytes.Equal(same, upgraded) {
		t.Fatalf("Rewrap sealed = changed %v, %v", changed, err)
	}
	if _, _, err := c.Rewrap(upgraded, WebhookSecretAAD(4)); err == nil {
		t.Fatal("Rewrap accepted a value sealed for another project")
	}
	if out, changed, err := c.Rewrap(nil, TokenAAD(1)); err != nil || changed || out != nil {
		t.Fatalf("Rewrap empty = %v, %v, %v", out, changed, err)
	}
}
