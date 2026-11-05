package secret

import "testing"

func TestModelCredentialAAD(t *testing.T) {
	if got := string(ModelCredentialAAD("user:first", "api_key")); got != "tribunal/llm-model/v1/10:user:first/api_key" {
		t.Fatalf("unexpected AAD: %q", got)
	}
	cipher, err := New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := cipher.Seal([]byte("test credential"), ModelCredentialAAD("user:first", "api_key"))
	if err != nil {
		t.Fatal(err)
	}
	for _, aad := range [][]byte{ModelCredentialAAD("user:second", "api_key"), ModelCredentialAAD("user:first", "other")} {
		if _, err := cipher.Open(sealed, aad); err == nil {
			t.Fatal("opened with wrong identity")
		}
	}
}
