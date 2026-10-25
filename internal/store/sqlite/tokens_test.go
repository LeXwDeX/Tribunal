package sqlite

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func TestFindAPITokenByHash(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	owner := store.TokenUser{ID: 7, Username: "alice", Name: "Alice", Role: "admin"}
	plaintext := []byte("crc_test-token")
	sum := sha256.Sum256(plaintext)
	expires := clock.Now().Add(time.Hour)
	if _, err := s.CreateAPIToken(ctx, owner, store.APIToken{
		Name: "ci", TokenHash: sum[:], ExpiresAt: &expires,
	}); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	token, user, err := s.FindAPITokenByHash(ctx, sum[:], clock.Now())
	if err != nil {
		t.Fatalf("FindAPITokenByHash: %v", err)
	}
	if token.Name != "ci" || token.LastUsedAt == nil || !token.LastUsedAt.Equal(clock.Now()) {
		t.Fatalf("token = %+v", token)
	}
	if user == nil || user.ID != 7 || user.Username != "alice" || user.Role != "admin" {
		t.Fatalf("user = %+v", user)
	}

	unknown := sha256.Sum256([]byte("crc_other"))
	if _, _, err := s.FindAPITokenByHash(ctx, unknown[:], clock.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown token = %v", err)
	}

	clock.Advance(2 * time.Hour)
	if _, _, err := s.FindAPITokenByHash(ctx, sum[:], clock.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired token = %v", err)
	}
	tokens, err := s.ListAPITokens(ctx, 7)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("ListAPITokens = %+v, %v", tokens, err)
	}
	if tokens[0].LastUsedAt == nil || !tokens[0].LastUsedAt.Equal(expires.Add(-time.Hour)) {
		t.Fatalf("expired lookup changed last_used_at: %+v", tokens[0].LastUsedAt)
	}
	if _, _, err := s.FindAPITokenByHash(ctx, []byte{1, 2}, clock.Now()); err == nil {
		t.Fatal("short hash accepted")
	}
}

func TestListAPITokensPage(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	owner := store.TokenUser{ID: 7, Username: "alice", Name: "Alice", Role: "admin"}
	other := store.TokenUser{ID: 8, Username: "bob", Name: "Bob", Role: "viewer"}

	created := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("owner-token-%d", i)))
		token, err := s.CreateAPIToken(ctx, owner, store.APIToken{
			Name: fmt.Sprintf("owner-%d", i), TokenHash: sum[:],
		})
		if err != nil {
			t.Fatalf("CreateAPIToken %d: %v", i, err)
		}
		created = append(created, token.ID)
	}
	otherSum := sha256.Sum256([]byte("other-token"))
	otherToken, err := s.CreateAPIToken(ctx, other, store.APIToken{Name: "other", TokenHash: otherSum[:]})
	if err != nil {
		t.Fatalf("CreateAPIToken other: %v", err)
	}

	all, err := s.ListAPITokens(ctx, owner.ID)
	if err != nil || len(all) != 5 {
		t.Fatalf("ListAPITokens = %+v, %v", all, err)
	}
	for _, token := range all {
		if !token.CreatedAt.Equal(clock.Now()) {
			t.Fatalf("created_at = %v, want %v", token.CreatedAt, clock.Now())
		}
	}

	var paged []store.APIToken
	var firstCursor string
	cursor := ""
	for {
		page, next, err := s.ListAPITokensPage(ctx, owner.ID, cursor, 2)
		if err != nil {
			t.Fatalf("ListAPITokensPage: %v", err)
		}
		if firstCursor == "" {
			firstCursor = next
		}
		paged = append(paged, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(paged) != len(created) {
		t.Fatalf("paged = %d tokens, want %d", len(paged), len(created))
	}
	seen := map[string]bool{}
	for i, token := range paged {
		want := created[len(created)-1-i]
		if token.ID != want {
			t.Fatalf("paged[%d] = %s, want %s", i, token.ID, want)
		}
		if seen[token.ID] {
			t.Fatalf("duplicate token %s", token.ID)
		}
		seen[token.ID] = true
	}

	otherPage, _, err := s.ListAPITokensPage(ctx, other.ID, "", 10)
	if err != nil || len(otherPage) != 1 || otherPage[0].ID != otherToken.ID {
		t.Fatalf("other page = %+v, %v", otherPage, err)
	}
	if _, _, err := s.ListAPITokensPage(ctx, other.ID, firstCursor, 10); !errors.Is(err, store.ErrInvalidCursor) {
		t.Fatalf("cross-user cursor = %v", err)
	}

	head, cursor, err := s.ListAPITokensPage(ctx, owner.ID, "", 1)
	if err != nil || len(head) != 1 || head[0].ID != created[len(created)-1] || cursor == "" {
		t.Fatalf("head page = %+v, %q, %v", head, cursor, err)
	}
	if err := s.DeleteAPIToken(ctx, owner.ID, head[0].ID); err != nil {
		t.Fatalf("DeleteAPIToken: %v", err)
	}
	afterDelete, _, err := s.ListAPITokensPage(ctx, owner.ID, cursor, 1)
	if err != nil || len(afterDelete) != 1 || afterDelete[0].ID != created[len(created)-2] {
		t.Fatalf("page after delete = %+v, %v", afterDelete, err)
	}
}
