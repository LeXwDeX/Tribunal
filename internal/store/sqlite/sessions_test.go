package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

func sessionHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func testSession(token string, now time.Time, ttl time.Duration) store.Session {
	return store.Session{
		IDHash:    sessionHash(token),
		UserID:    7,
		Username:  "alice",
		Name:      "Alice Admin",
		Role:      "admin",
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
}

func TestSessionStoresOnlyTheTokenHash(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)

	if err := s.CreateSession(ctx, testSession("raw-cookie-token", clock.Now(), time.Hour), store.SessionLimits{}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT id FROM sessions`).Scan(&stored); err != nil {
		t.Fatalf("read stored id: %v", err)
	}
	if stored == "raw-cookie-token" || strings.Contains(stored, "raw-cookie-token") {
		t.Fatalf("session table stores the raw token: %q", stored)
	}
	if stored != hex.EncodeToString(sessionHash("raw-cookie-token")) {
		t.Fatalf("stored id = %q, want the sha256 hex digest", stored)
	}

	got, err := s.GetSession(ctx, sessionHash("raw-cookie-token"), clock.Now())
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.UserID != 7 || got.Username != "alice" || got.Name != "Alice Admin" || got.Role != "admin" {
		t.Fatalf("session user = %+v", got)
	}
	if !got.ExpiresAt.Equal(clock.Now().Add(time.Hour)) {
		t.Fatalf("expires = %v", got.ExpiresAt)
	}
}

func TestSessionExpiryBoundaryDeletesRow(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	created := clock.Now()
	if err := s.CreateSession(ctx, testSession("expiring", created, time.Hour), store.SessionLimits{}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	almost := created.Add(time.Hour - time.Nanosecond)
	if _, err := s.GetSession(ctx, sessionHash("expiring"), almost); err != nil {
		t.Fatalf("session before expiry: %v", err)
	}
	if _, err := s.GetSession(ctx, sessionHash("expiring"), created.Add(time.Hour)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session at expiry = %v", err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM sessions`); got != 0 {
		t.Fatalf("expired session row remains: %d", got)
	}
}

func sessionFor(token string, userID int64, username string, now time.Time) store.Session {
	session := testSession(token, now, time.Hour)
	session.UserID = userID
	session.Username = username
	session.Role = "viewer"
	return session
}

func TestSessionPerUserLimitEvictsOnlyThatUsersOldest(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	limits := store.SessionLimits{PerUser: 2, Total: 100}
	if err := s.CreateSession(ctx, sessionFor("bob-1", 8, "bob", clock.Now()), limits); err != nil {
		t.Fatalf("CreateSession(bob-1): %v", err)
	}
	// One user logging in repeatedly must only evict their own sessions.
	for _, token := range []string{"mallory-1", "mallory-2", "mallory-3", "mallory-4"} {
		clock.Advance(time.Second)
		if err := s.CreateSession(ctx, sessionFor(token, 99, "mallory", clock.Now()), limits); err != nil {
			t.Fatalf("CreateSession(%s): %v", token, err)
		}
	}

	if got := rowCount(t, s, `SELECT COUNT(*) FROM sessions WHERE user_id = 99`); got != 2 {
		t.Fatalf("mallory session count = %d, want 2", got)
	}
	if _, err := s.GetSession(ctx, sessionHash("bob-1"), clock.Now()); err != nil {
		t.Fatalf("another user's session was evicted: %v", err)
	}
	for _, token := range []string{"mallory-1", "mallory-2"} {
		if _, err := s.GetSession(ctx, sessionHash(token), clock.Now()); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("old session %s was not evicted: %v", token, err)
		}
	}
	for _, token := range []string{"mallory-3", "mallory-4"} {
		if _, err := s.GetSession(ctx, sessionHash(token), clock.Now()); err != nil {
			t.Fatalf("session %s: %v", token, err)
		}
	}
}

func TestSessionTotalLimitIsBackstop(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	limits := store.SessionLimits{PerUser: 10, Total: 2}
	users := []struct {
		token string
		id    int64
	}{{"first", 7}, {"second", 8}, {"third", 9}}
	for _, user := range users {
		if err := s.CreateSession(ctx, sessionFor(user.token, user.id, "user"+user.token, clock.Now()), limits); err != nil {
			t.Fatalf("CreateSession(%s): %v", user.token, err)
		}
		clock.Advance(time.Second)
	}

	if got := rowCount(t, s, `SELECT COUNT(*) FROM sessions`); got != 2 {
		t.Fatalf("session count = %d, want 2", got)
	}
	if _, err := s.GetSession(ctx, sessionHash("first"), clock.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("oldest session was not evicted: %v", err)
	}
}

func TestSessionLimitsDefaults(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	for i := 0; i < store.DefaultSessionsPerUser+2; i++ {
		token := "token-" + strings.Repeat("x", i)
		if err := s.CreateSession(ctx, testSession(token, clock.Now(), time.Hour), store.SessionLimits{}); err != nil {
			t.Fatalf("CreateSession(%d): %v", i, err)
		}
		clock.Advance(time.Second)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM sessions`); got != store.DefaultSessionsPerUser {
		t.Fatalf("session count = %d, want %d", got, store.DefaultSessionsPerUser)
	}
}

func TestCreateSessionPurgesExpiredRows(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	if err := s.CreateSession(ctx, testSession("old", clock.Now(), time.Minute), store.SessionLimits{}); err != nil {
		t.Fatalf("CreateSession(old): %v", err)
	}
	clock.Advance(2 * time.Minute)
	if err := s.CreateSession(ctx, testSession("new", clock.Now(), time.Hour), store.SessionLimits{}); err != nil {
		t.Fatalf("CreateSession(new): %v", err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM sessions`); got != 1 {
		t.Fatalf("expired rows not purged: %d", got)
	}
}

func TestDeleteSessionIsIdempotent(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	if err := s.CreateSession(ctx, testSession("token", clock.Now(), time.Hour), store.SessionLimits{}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.DeleteSession(ctx, sessionHash("token")); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if err := s.DeleteSession(ctx, sessionHash("token")); err != nil {
		t.Fatalf("second DeleteSession: %v", err)
	}
	if _, err := s.GetSession(ctx, sessionHash("token"), clock.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted session = %v", err)
	}
}

func TestSessionValidation(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	valid := testSession("token", clock.Now(), time.Hour)

	cases := map[string]store.Session{
		"short hash":    {IDHash: []byte{1}, UserID: 7, Username: "alice", Role: "admin", ExpiresAt: valid.ExpiresAt},
		"missing user":  {IDHash: valid.IDHash, Username: "alice", Role: "admin", ExpiresAt: valid.ExpiresAt},
		"missing name":  {IDHash: valid.IDHash, UserID: 7, Role: "admin", ExpiresAt: valid.ExpiresAt},
		"invalid role":  {IDHash: valid.IDHash, UserID: 7, Username: "alice", Role: "owner", ExpiresAt: valid.ExpiresAt},
		"expired entry": {IDHash: valid.IDHash, UserID: 7, Username: "alice", Role: "admin", CreatedAt: clock.Now(), ExpiresAt: clock.Now()},
	}
	for name, session := range cases {
		if err := s.CreateSession(ctx, session, store.SessionLimits{}); err == nil {
			t.Errorf("%s: CreateSession accepted an invalid session", name)
		}
	}
}

func TestLegacyDatabaseKeepsOldSessionRowsUntrusted(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := sql.Open("sqlite", withPragmas(path))
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	applied := []string{"0001_init.sql", "0003_review_state.sql", "0004_context_revisions.sql"}
	for _, name := range applied {
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := legacy.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	stamp := "2026-10-20T12:00:00.000000000Z"
	if _, err := legacy.Exec(`INSERT INTO users (id, username, name, role, last_login_at)
		VALUES (7, 'alice', 'Alice Admin', 'admin', ?)`, stamp); err != nil {
		t.Fatalf("insert legacy user: %v", err)
	}
	// A row written by an older build used a plaintext session id.
	if _, err := legacy.Exec(`INSERT INTO sessions (id, user_id, expires_at, created_at)
		VALUES ('legacy-plaintext-id', 7, '2026-10-20T13:00:00.000000000Z', ?)`, stamp); err != nil {
		t.Fatalf("insert legacy session: %v", err)
	}
	for _, name := range applied {
		if _, err := legacy.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			migrationVersionForTest(t, name), name, stamp); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("Open legacy db: %v", err)
	}
	defer s.Close()
	clock := newFakeClock()
	if err := s.CreateSession(ctx, testSession("fresh-token", clock.Now(), time.Hour), store.SessionLimits{}); err != nil {
		t.Fatalf("CreateSession on legacy schema: %v", err)
	}
	if _, err := s.GetSession(ctx, sessionHash("fresh-token"), clock.Now()); err != nil {
		t.Fatalf("GetSession on legacy schema: %v", err)
	}
	if _, err := s.GetSession(ctx, sessionHash("legacy-plaintext-id"), clock.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("legacy plaintext session id was accepted: %v", err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM schema_migrations`); got != migrationCount(t) {
		// Open applies every newer migration on upgrade.
		t.Fatalf("schema_migrations = %d, want %d", got, migrationCount(t))
	}
}

func migrationVersionForTest(t *testing.T, name string) int {
	t.Helper()
	version, err := migrationVersion(name)
	if err != nil {
		t.Fatalf("migrationVersion(%s): %v", name, err)
	}
	return version
}
