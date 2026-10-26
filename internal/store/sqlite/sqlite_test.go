package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 22, 12, 0, 0, 123456789, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func openTestStore(t *testing.T, clock *fakeClock) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "review.db")
	s, err := Open(context.Background(), path, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustProject(t *testing.T, s *Store, id int64) string {
	t.Helper()
	jobID, err := s.RegisterProject(context.Background(), store.Project{
		ID:                id,
		PathWithNamespace: fmt.Sprintf("group/project-%d", id),
		WebURL:            fmt.Sprintf("https://gitlab.example.com/group/project-%d", id),
		DefaultBranch:     "main",
		TokenEnc:          []byte{0x00, 0x01, 0xff, byte(id)},
		WebhookSecretEnc:  []byte{0xfe, 0xed, byte(id)},
		TokenScopes:       []string{"api"},
	})
	if err != nil {
		t.Fatalf("RegisterProject: %v", err)
	}
	return jobID
}

func rowCount(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := s.db.QueryRowContext(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	return count
}

func TestMigrationPragmasAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "review.db")
	clock := newFakeClock()

	s, err := Open(ctx, path, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	pragmas := map[string]string{
		"journal_mode": "wal",
		"foreign_keys": "1",
		"busy_timeout": "5000",
		"synchronous":  "1",
	}
	for pragma, want := range pragmas {
		var got string
		if err := s.db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", pragma, err)
		}
		if !strings.EqualFold(got, want) {
			t.Errorf("PRAGMA %s = %q, want %q", pragma, got, want)
		}
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM schema_migrations`); got != migrationCount(t) {
		t.Fatalf("schema_migrations = %d, want %d", got, migrationCount(t))
	}
	if _, err := s.db.Exec(`INSERT INTO workspaces (project_id, state, updated_at) VALUES (999, 'new', '2026-10-22T12:00:00.000000000Z')`); err == nil {
		t.Fatal("foreign_keys pragma was not enforced")
	}
	projectID := int64(42)
	if _, err := s.RegisterProject(ctx, store.Project{
		ID:                projectID,
		PathWithNamespace: "group/app",
		WebURL:            "https://gitlab.example.com/group/app",
		DefaultBranch:     "main",
		TokenEnc:          []byte("ciphertext"),
		WebhookSecretEnc:  []byte("secret-ciphertext"),
	}); err != nil {
		t.Fatalf("RegisterProject: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(ctx, path, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if got := rowCount(t, s, `SELECT COUNT(*) FROM schema_migrations`); got != migrationCount(t) {
		t.Fatalf("schema_migrations after reopen = %d, want %d", got, migrationCount(t))
	}
	project, err := s.GetProject(ctx, projectID)
	if err != nil {
		t.Fatalf("GetProject after reopen: %v", err)
	}
	if project.PathWithNamespace != "group/app" || !bytes.Equal(project.TokenEnc, []byte("ciphertext")) {
		t.Fatalf("project after reopen = %+v", project)
	}
}

func TestProjectPersistsEncryptedBytesAndRedactsJSON(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	token := []byte{0x00, 0xff, 0x10, 0x7f, 0x80, 'n', 'o', 't', '-', 'j', 's', 'o', 'n'}
	secret := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01}
	projectID := int64(7)
	expires := clock.Now().Add(24 * time.Hour)
	hookID := int64(999)
	_, err := s.RegisterProject(ctx, store.Project{
		ID:                projectID,
		PathWithNamespace: "group/repo",
		WebURL:            "https://gitlab.example.com/group/repo",
		DefaultBranch:     "main",
		TokenEnc:          token,
		WebhookSecretEnc:  secret,
		TokenExpiresAt:    &expires,
		TokenScopes:       []string{"api", "read_repository"},
		HookID:            &hookID,
		Profile:           "deep",
		Settings:          json.RawMessage(`{"skip_draft":true}`),
	})
	if err != nil {
		t.Fatalf("RegisterProject: %v", err)
	}

	got, err := s.GetProject(ctx, projectID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if !bytes.Equal(got.TokenEnc, token) {
		t.Errorf("TokenEnc = %x, want %x", got.TokenEnc, token)
	}
	if !bytes.Equal(got.WebhookSecretEnc, secret) {
		t.Errorf("WebhookSecretEnc = %x, want %x", got.WebhookSecretEnc, secret)
	}
	if !got.TokenExpiresAt.Equal(expires) {
		t.Errorf("TokenExpiresAt = %v, want %v", got.TokenExpiresAt, expires)
	}
	if got.HookID == nil || *got.HookID != hookID {
		t.Errorf("HookID = %v, want %d", got.HookID, hookID)
	}
	if string(got.Settings) != `{"skip_draft":true}` {
		t.Errorf("Settings = %s", got.Settings)
	}
	if got.Profile != "deep" || !got.Enabled {
		t.Errorf("Profile/Enabled = %q/%v", got.Profile, got.Enabled)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, forbidden := range []string{"token_enc", "webhook_secret_enc", "ciphertext", "TokenEnc", "WebhookSecretEnc"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("marshalled project contains %q: %s", forbidden, encoded)
		}
	}

	if got := rowCount(t, s, `SELECT COUNT(*) FROM workspaces WHERE project_id = ? AND state = 'new'`, projectID); got != 1 {
		t.Errorf("new workspace count = %d, want 1", got)
	}
	var (
		kind, key, state, payload string
		maxAttempts               int
	)
	err = s.db.QueryRowContext(ctx, `SELECT kind, "key", state, payload, max_attempts FROM jobs WHERE "key" = ?`,
		fmt.Sprintf("workspace.onboard:%d", projectID)).Scan(&kind, &key, &state, &payload, &maxAttempts)
	if err != nil {
		t.Fatalf("onboarding job: %v", err)
	}
	if kind != onboardingKind || state != store.JobQueued || maxAttempts != defaultMaxRetry {
		t.Errorf("onboarding job = %s %s %s %d", kind, state, payload, maxAttempts)
	}
	if payload != fmt.Sprintf(`{"project_id":%d}`, projectID) {
		t.Errorf("onboarding payload = %s", payload)
	}
}

func TestRegisterProjectConflictDoesNotOverwrite(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	jobID := mustProject(t, s, 11)
	if jobID == "" {
		t.Fatal("empty onboarding job id")
	}
	_, err := s.RegisterProject(ctx, store.Project{
		ID:                11,
		PathWithNamespace: "group/other",
		WebURL:            "https://gitlab.example.com/group/other",
		DefaultBranch:     "trunk",
		TokenEnc:          []byte("other"),
		WebhookSecretEnc:  []byte("other"),
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate RegisterProject error = %v, want ErrConflict", err)
	}
	project, err := s.GetProject(ctx, 11)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if project.PathWithNamespace != "group/project-11" || project.DefaultBranch != "main" {
		t.Fatalf("project was overwritten: %+v", project)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM workspaces WHERE project_id = 11`); got != 1 {
		t.Errorf("workspace count = %d, want 1", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE "key" = 'workspace.onboard:11'`); got != 1 {
		t.Errorf("onboarding job count = %d, want 1", got)
	}
	if _, err := s.GetProject(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetProject missing error = %v, want ErrNotFound", err)
	}
	list, err := s.ListProjects(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListProjects = %v, %v", list, err)
	}
}

func TestConcurrentEnqueueDedupesQueuedKey(t *testing.T) {
	s := openTestStore(t, newFakeClock())
	ctx := context.Background()
	const workers = 24
	ids := make([]string, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ids[i], _, errs[i] = s.Enqueue(ctx, store.Job{
				Kind:    "review.run",
				Key:     "review:1:1",
				Payload: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)),
			})
		}(i)
	}
	close(start)
	wg.Wait()
	first := ""
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
		if ids[i] == "" {
			t.Fatalf("Enqueue %d returned empty id", i)
		}
		if first == "" {
			first = ids[i]
		} else if ids[i] != first {
			t.Fatalf("ids[%d]=%s differs from %s", i, ids[i], first)
		}
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE "key" = ? AND state = 'queued'`, "review:1:1"); got != 1 {
		t.Fatalf("queued jobs with key = %d, want 1", got)
	}
}

func TestRecordDeliveryAtomicAndReplay(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 1)
	mustProject(t, s, 2)

	delivery := store.Delivery{UUID: "uuid-a", ProjectID: 1, Event: "Merge Request Hook", Outcome: store.DeliveryEnqueued}
	job := store.Job{Kind: "review.run", Key: "review:1:1", Payload: json.RawMessage(`{"head":"a"}`)}
	jobID, duplicate, err := s.RecordDelivery(ctx, delivery, &job)
	if err != nil {
		t.Fatalf("RecordDelivery: %v", err)
	}
	if duplicate || jobID == "" {
		t.Fatalf("first delivery = %q dup=%v", jobID, duplicate)
	}

	replayJob := store.Job{Kind: "review.run", Key: "review:1:1", Payload: json.RawMessage(`{"head":"b"}`)}
	replayedID, duplicate, err := s.RecordDelivery(ctx, delivery, &replayJob)
	if err != nil {
		t.Fatalf("replay RecordDelivery: %v", err)
	}
	if !duplicate || replayedID != jobID {
		t.Fatalf("replay = %q dup=%v, want %q dup=true", replayedID, duplicate, jobID)
	}
	stored, err := s.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if string(stored.Payload) != `{"head":"a"}` {
		t.Errorf("replayed delivery replaced payload: %s", stored.Payload)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE "key" = 'review:1:1'`); got != 1 {
		t.Errorf("jobs after replay = %d, want 1", got)
	}

	// The UUID is scoped to the project, so another project may reuse it.
	otherID, duplicate, err := s.RecordDelivery(ctx,
		store.Delivery{UUID: "uuid-a", ProjectID: 2, Event: "Push Hook", Outcome: store.DeliveryEnqueued},
		&store.Job{Kind: "workspace.refresh", Key: "workspace.refresh:2", Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("other project delivery: %v", err)
	}
	if duplicate || otherID == "" || otherID == jobID {
		t.Fatalf("other project delivery = %q dup=%v", otherID, duplicate)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM webhook_deliveries`); got != 2 {
		t.Errorf("delivery count = %d, want 2", got)
	}

	// A failed enqueue must roll back the delivery insert.
	if _, _, err := s.Enqueue(ctx, store.Job{Kind: "review.cancel", Key: "review:3:3"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	failing := store.Job{Kind: "review.run", Key: "review:3:3"}
	if _, _, err := s.RecordDelivery(ctx,
		store.Delivery{UUID: "uuid-rollback", ProjectID: 1, Event: "Merge Request Hook", Outcome: store.DeliveryEnqueued},
		&failing); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("conflicting delivery error = %v, want ErrConflict", err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM webhook_deliveries WHERE uuid = 'uuid-rollback'`); got != 0 {
		t.Fatalf("rolled back delivery persisted: %d", got)
	}
	// Reusing the same UUID after the failed attempt must succeed.
	okJob := store.Job{Kind: "review.cancel", Key: "review:3:3", Payload: json.RawMessage(`{"retry":true}`)}
	if _, _, err := s.RecordDelivery(ctx,
		store.Delivery{UUID: "uuid-rollback", ProjectID: 1, Event: "Merge Request Hook", Outcome: store.DeliveryEnqueued},
		&okJob); err != nil {
		t.Fatalf("retry delivery: %v", err)
	}
}

func TestRecordDeliveryReplacesOnlyLatestQueued(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 1)

	first := store.Job{Kind: "review.run", Key: "review:1:9", Payload: json.RawMessage(`{"head":"a"}`)}
	id, _, err := s.RecordDelivery(ctx,
		store.Delivery{UUID: "u1", ProjectID: 1, Event: "Merge Request Hook", Outcome: store.DeliveryEnqueued}, &first)
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	second := store.Job{Kind: "review.run", Key: "review:1:9", Payload: json.RawMessage(`{"head":"b"}`)}
	replacedID, duplicate, err := s.RecordDelivery(ctx,
		store.Delivery{UUID: "u2", ProjectID: 1, Event: "Merge Request Hook", Outcome: store.DeliveryEnqueued}, &second)
	if err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if duplicate || replacedID != id {
		t.Fatalf("replacement id = %q dup=%v, want %q dup=false", replacedID, duplicate, id)
	}
	stored, err := s.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if stored.State != store.JobQueued || string(stored.Payload) != `{"head":"b"}` {
		t.Fatalf("queued replacement = state %s payload %s", stored.State, stored.Payload)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE "key" = 'review:1:9'`); got != 1 {
		t.Fatalf("job count after replacement = %d, want 1", got)
	}
}

func TestRunningKeyGetsSuccessorNotReplaced(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 1)

	first := store.Job{Kind: "review.run", Key: "review:1:5", Payload: json.RawMessage(`{"head":"a"}`)}
	if _, _, err := s.Enqueue(ctx, first); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	running, err := s.Claim(ctx, "w1", []string{"review.run"}, time.Hour)
	if err != nil || running == nil {
		t.Fatalf("Claim = %v, %v", running, err)
	}
	if running.Attempts != 1 || string(running.Payload) != `{"head":"a"}` {
		t.Fatalf("claimed job = %+v", running)
	}

	successor := store.Job{Kind: "review.run", Key: "review:1:5", Payload: json.RawMessage(`{"head":"b"}`)}
	successorID, duplicate, err := s.RecordDelivery(ctx,
		store.Delivery{UUID: "u-successor", ProjectID: 1, Event: "Merge Request Hook", Outcome: store.DeliveryEnqueued},
		&successor)
	if err != nil {
		t.Fatalf("successor delivery: %v", err)
	}
	if duplicate || successorID == running.ID {
		t.Fatalf("successor = %q dup=%v, running=%q", successorID, duplicate, running.ID)
	}
	stored, err := s.GetJob(ctx, running.ID)
	if err != nil {
		t.Fatalf("GetJob running: %v", err)
	}
	if stored.State != store.JobRunning || string(stored.Payload) != `{"head":"a"}` {
		t.Fatalf("running job was changed: state=%s payload=%s", stored.State, stored.Payload)
	}

	// A newer pending event replaces the queued successor, never the running job.
	latest := store.Job{Kind: "review.run", Key: "review:1:5", Payload: json.RawMessage(`{"head":"c"}`)}
	latestID, duplicate, err := s.RecordDelivery(ctx,
		store.Delivery{UUID: "u-latest", ProjectID: 1, Event: "Merge Request Hook", Outcome: store.DeliveryEnqueued},
		&latest)
	if err != nil {
		t.Fatalf("latest delivery: %v", err)
	}
	if duplicate || latestID != successorID {
		t.Fatalf("latest replacement = %q dup=%v, want %q", latestID, duplicate, successorID)
	}
	stored, err = s.GetJob(ctx, running.ID)
	if err != nil {
		t.Fatalf("GetJob running after replacement: %v", err)
	}
	if stored.State != store.JobRunning || string(stored.Payload) != `{"head":"a"}` {
		t.Fatalf("replacement changed running job: state=%s payload=%s", stored.State, stored.Payload)
	}

	if blocked, err := s.Claim(ctx, "w2", []string{"review.run"}, time.Hour); err != nil || blocked != nil {
		t.Fatalf("claim during running = %v, %v; want nil", blocked, err)
	}
	if err := s.Complete(ctx, *running); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	next, err := s.Claim(ctx, "w2", []string{"review.run"}, time.Hour)
	if err != nil || next == nil {
		t.Fatalf("successor Claim = %v, %v", next, err)
	}
	if next.ID != successorID || string(next.Payload) != `{"head":"c"}` || next.Attempts != 1 {
		t.Fatalf("claimed successor = %+v", next)
	}
}

func TestClaimFiltersAndSerializesKeys(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())

	// Same key: running A + queued successor B.
	if _, _, err := s.Enqueue(ctx, store.Job{Kind: "review.run", Key: "review:1:1", Payload: json.RawMessage(`{"a":1}`)}); err != nil {
		t.Fatalf("Enqueue A: %v", err)
	}
	running, err := s.Claim(ctx, "w", []string{"review.run"}, time.Hour)
	if err != nil || running == nil {
		t.Fatalf("Claim A: %v, %v", running, err)
	}
	if _, _, err := s.Enqueue(ctx, store.Job{Kind: "review.run", Key: "review:1:1", Payload: json.RawMessage(`{"b":1}`)}); err != nil {
		t.Fatalf("Enqueue B: %v", err)
	}
	// Another key must remain claimable.
	otherID, _, err := s.Enqueue(ctx, store.Job{Kind: "review.run", Key: "review:2:2", Payload: json.RawMessage(`{"c":1}`)})
	if err != nil {
		t.Fatalf("Enqueue C: %v", err)
	}
	other, err := s.Claim(ctx, "w", []string{"review.run"}, time.Hour)
	if err != nil || other == nil || other.ID != otherID {
		t.Fatalf("other key claim = %+v, %v; want %s", other, err, otherID)
	}
	if blocked, err := s.Claim(ctx, "w", []string{"review.run"}, time.Hour); err != nil || blocked != nil {
		t.Fatalf("successor claim before clearing running = %+v, %v", blocked, err)
	}
	if job, err := s.Claim(ctx, "w", nil, time.Hour); err != nil || job != nil {
		t.Fatalf("empty kinds claim = %+v, %v; want nil", job, err)
	}
	unknownID, _, err := s.Enqueue(ctx, store.Job{Kind: "not.implemented", Key: "unknown:1", Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("Enqueue unknown: %v", err)
	}
	if job, err := s.Claim(ctx, "w", []string{"workspace.onboard"}, time.Hour); err != nil || job != nil {
		t.Fatalf("unknown kind claim = %+v, %v; want nil", job, err)
	}
	unknown, err := s.GetJob(ctx, unknownID)
	if err != nil || unknown.State != store.JobQueued {
		t.Fatalf("unknown job after claim = %+v, %v", unknown, err)
	}
	if err := s.Complete(ctx, *running); err != nil {
		t.Fatalf("Complete A: %v", err)
	}
	successor, err := s.Claim(ctx, "w", []string{"review.run"}, time.Hour)
	if err != nil || successor == nil || successor.Key != "review:1:1" {
		t.Fatalf("successor = %+v, %v", successor, err)
	}

	// Concurrent claims of distinct keys never double-lease.
	for i := 0; i < 8; i++ {
		if _, _, err := s.Enqueue(ctx, store.Job{Kind: "parallel", Key: fmt.Sprintf("p:%d", i)}); err != nil {
			t.Fatalf("Enqueue parallel: %v", err)
		}
	}
	var wg sync.WaitGroup
	claimed := make(chan string, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, err := s.Claim(ctx, "concurrent", []string{"parallel"}, time.Minute)
			if err != nil {
				errs <- err
				return
			}
			if job != nil {
				claimed <- job.ID
			}
		}()
	}
	wg.Wait()
	close(claimed)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Claim: %v", err)
	}
	seen := make(map[string]bool)
	for id := range claimed {
		if seen[id] {
			t.Fatalf("job %s claimed twice", id)
		}
		seen[id] = true
	}
	if len(seen) != 8 {
		t.Fatalf("claimed %d jobs, want 8", len(seen))
	}
}

func TestLeaseRecoveryExpiryAndRetryExhaustion(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)

	jobID, _, err := s.Enqueue(ctx, store.Job{Kind: "retry", Key: "retry:1", MaxAttempts: 2})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	first, err := s.Claim(ctx, "worker-a", []string{"retry"}, 10*time.Second)
	if err != nil || first == nil {
		t.Fatalf("first Claim: %v, %v", first, err)
	}
	if first.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", first.Attempts)
	}
	clock.Advance(11 * time.Second)
	n, err := s.Recover(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Recover = %d, %v; want 1", n, err)
	}
	stored, err := s.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if stored.State != store.JobQueued || stored.Attempts != 1 || stored.LeaseOwner != "" || stored.LeaseUntil != nil {
		t.Fatalf("recovered job = %+v", stored)
	}
	second, err := s.Claim(ctx, "worker-a", []string{"retry"}, 10*time.Second)
	if err != nil || second == nil {
		t.Fatalf("second Claim: %v, %v", second, err)
	}
	if second.Attempts != 2 {
		t.Fatalf("second attempts = %d, want 2", second.Attempts)
	}
	clock.Advance(11 * time.Second)
	if n, err := s.Recover(ctx); err != nil || n != 1 {
		t.Fatalf("second Recover = %d, %v; want 1", n, err)
	}
	stored, err = s.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if stored.State != store.JobFailed || stored.LeaseUntil != nil {
		t.Fatalf("exhausted job = %+v", stored)
	}
	if job, err := s.Claim(ctx, "worker-a", []string{"retry"}, time.Minute); err != nil || job != nil {
		t.Fatalf("claim exhausted = %+v, %v; want nil", job, err)
	}
}

func TestExpiredLeaseRecoveryAfterReopen(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	path := filepath.Join(t.TempDir(), "reopen.db")
	s, err := Open(ctx, path, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	jobID, _, err := s.Enqueue(ctx, store.Job{Kind: "reopen", Key: "reopen:1"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if job, err := s.Claim(ctx, "before-close", []string{"reopen"}, 10*time.Second); err != nil || job == nil {
		t.Fatalf("Claim: %v, %v", job, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	clock.Advance(11 * time.Second)
	s, err = Open(ctx, path, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if n, err := s.Recover(ctx); err != nil || n != 1 {
		t.Fatalf("Recover after reopen = %d, %v; want 1", n, err)
	}
	stored, err := s.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if stored.State != store.JobQueued || stored.Attempts != 1 {
		t.Fatalf("reopened job = %+v", stored)
	}
}

func TestExpiredLeaseWithQueuedSuccessorCancelsStale(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)

	if _, _, err := s.Enqueue(ctx, store.Job{Kind: "review.run", Key: "review:1:2", Payload: json.RawMessage(`{"head":"old"}`)}); err != nil {
		t.Fatalf("Enqueue old: %v", err)
	}
	stale, err := s.Claim(ctx, "w1", []string{"review.run"}, 10*time.Second)
	if err != nil || stale == nil {
		t.Fatalf("Claim old: %v, %v", stale, err)
	}
	newID, _, err := s.Enqueue(ctx, store.Job{Kind: "review.run", Key: "review:1:2", Payload: json.RawMessage(`{"head":"new"}`)})
	if err != nil {
		t.Fatalf("Enqueue new: %v", err)
	}
	clock.Advance(11 * time.Second)
	fresh, err := s.Claim(ctx, "w2", []string{"review.run"}, time.Minute)
	if err != nil || fresh == nil {
		t.Fatalf("Claim new: %v, %v", fresh, err)
	}
	if fresh.ID != newID || string(fresh.Payload) != `{"head":"new"}` {
		t.Fatalf("fresh claim = %+v", fresh)
	}
	staleAfter, err := s.GetJob(ctx, stale.ID)
	if err != nil {
		t.Fatalf("GetJob stale: %v", err)
	}
	if staleAfter.State != store.JobCancelled {
		t.Fatalf("stale state = %s, want cancelled", staleAfter.State)
	}
	if err := s.Complete(ctx, *stale); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("late Complete error = %v, want ErrLeaseLost", err)
	}
	if err := s.Complete(ctx, *fresh); err != nil {
		t.Fatalf("Complete fresh: %v", err)
	}
}

func TestStaleAttemptRejectedEvenSameWorker(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)

	if _, _, err := s.Enqueue(ctx, store.Job{Kind: "stale", Key: "stale:1", MaxAttempts: 3}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	old, err := s.Claim(ctx, "same-worker", []string{"stale"}, 10*time.Second)
	if err != nil || old == nil {
		t.Fatalf("first Claim: %v, %v", old, err)
	}
	clock.Advance(11 * time.Second)
	if _, err := s.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	current, err := s.Claim(ctx, "same-worker", []string{"stale"}, time.Minute)
	if err != nil || current == nil {
		t.Fatalf("second Claim: %v, %v", current, err)
	}
	if current.Attempts != old.Attempts+1 {
		t.Fatalf("attempts = %d, want %d", current.Attempts, old.Attempts+1)
	}
	if err := s.Complete(ctx, *old); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("old Complete error = %v, want ErrLeaseLost", err)
	}
	if err := s.Heartbeat(ctx, *old, time.Minute); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("old Heartbeat error = %v, want ErrLeaseLost", err)
	}
	if err := s.Fail(ctx, *old, nil, nil); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("old Fail error = %v, want ErrLeaseLost", err)
	}
	if err := s.Complete(ctx, *current); err != nil {
		t.Fatalf("current Complete: %v", err)
	}
}

func TestHeartbeatExtendsLeaseAndRejectsExpiry(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)

	if _, _, err := s.Enqueue(ctx, store.Job{Kind: "beat", Key: "beat:1"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	job, err := s.Claim(ctx, "w", []string{"beat"}, 10*time.Second)
	if err != nil || job == nil {
		t.Fatalf("Claim: %v, %v", job, err)
	}
	clock.Advance(6 * time.Second)
	if err := s.Heartbeat(ctx, *job, 10*time.Second); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	clock.Advance(5 * time.Second)
	if err := s.Complete(ctx, *job); err != nil {
		t.Fatalf("Complete after heartbeat: %v", err)
	}

	if _, _, err := s.Enqueue(ctx, store.Job{Kind: "beat", Key: "beat:2"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	expired, err := s.Claim(ctx, "w", []string{"beat"}, 10*time.Second)
	if err != nil || expired == nil {
		t.Fatalf("Claim expired: %v, %v", expired, err)
	}
	clock.Advance(11 * time.Second)
	if err := s.Heartbeat(ctx, *expired, time.Minute); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("expired Heartbeat error = %v, want ErrLeaseLost", err)
	}
	if err := s.Complete(ctx, *expired); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("expired Complete error = %v, want ErrLeaseLost", err)
	}
}

func TestCancelByKeyIsExactAndQueuedOnly(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())

	keys := []string{"review:1:1", "review:1:10", "review:1:1:extra", "review:1:%"}
	for _, key := range keys {
		if _, _, err := s.Enqueue(ctx, store.Job{Kind: "review.run", Key: key}); err != nil {
			t.Fatalf("Enqueue %s: %v", key, err)
		}
	}
	if n, err := s.CancelByKey(ctx, "review:1:1"); err != nil || n != 1 {
		t.Fatalf("CancelByKey exact = %d, %v; want 1", n, err)
	}
	for _, key := range keys[1:] {
		if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE "key" = ? AND state = 'queued'`, key); got != 1 {
			t.Errorf("key %s queued count = %d, want 1 (prefix must not match)", key, got)
		}
	}
	if n, err := s.CancelByKey(ctx, "review:1:%"); err != nil || n != 1 {
		t.Fatalf("CancelByKey literal wildcard = %d, %v; want 1", n, err)
	}
	if n, err := s.CancelByKey(ctx, "review:1:%"); err != nil || n != 0 {
		t.Fatalf("second CancelByKey literal wildcard = %d, %v; want 0", n, err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE "key" = 'review:1:10' AND state = 'queued'`); got != 1 {
		t.Errorf("wildcard cancel leaked into review:1:10")
	}

	running, err := s.Claim(ctx, "w", []string{"review.run"}, time.Hour)
	if err != nil || running == nil {
		t.Fatalf("Claim: %v, %v", running, err)
	}
	successorKey := running.Key
	if _, _, err := s.Enqueue(ctx, store.Job{Kind: "review.run", Key: successorKey}); err != nil {
		t.Fatalf("Enqueue successor: %v", err)
	}
	if n, err := s.CancelByKey(ctx, successorKey); err != nil || n != 1 {
		t.Fatalf("CancelByKey running key = %d, %v; want only successor", n, err)
	}
	stored, err := s.GetJob(ctx, running.ID)
	if err != nil || stored.State != store.JobRunning {
		t.Fatalf("running job after CancelByKey = %+v, %v", stored, err)
	}
	if err := s.Complete(ctx, *running); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestFailRetriesWithSanitizedError(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)

	jobID, _, err := s.Enqueue(ctx, store.Job{Kind: "fail", Key: "fail:1", MaxAttempts: 2})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	first, err := s.Claim(ctx, "w", []string{"fail"}, 10*time.Second)
	if err != nil || first == nil {
		t.Fatalf("first Claim: %v, %v", first, err)
	}
	retryAt := clock.Now().Add(5 * time.Second)
	secretErr := errors.New("token=SECRET-TOKEN payload=RAW-BODY")
	if err := s.Fail(ctx, *first, secretErr, &retryAt); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	stored, err := s.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if stored.State != store.JobQueued || stored.LastError == "" {
		t.Fatalf("retry job = %+v", stored)
	}
	if strings.Contains(stored.LastError, "SECRET") || strings.Contains(stored.LastError, "RAW") {
		t.Fatalf("handler error leaked: %q", stored.LastError)
	}
	if job, err := s.Claim(ctx, "w", []string{"fail"}, time.Minute); err != nil || job != nil {
		t.Fatalf("claim before retryAt = %+v, %v; want nil", job, err)
	}
	clock.Advance(5 * time.Second)
	second, err := s.Claim(ctx, "w", []string{"fail"}, 10*time.Second)
	if err != nil || second == nil {
		t.Fatalf("second Claim: %v, %v", second, err)
	}
	if second.Attempts != 2 {
		t.Fatalf("second attempts = %d, want 2", second.Attempts)
	}
	if err := s.Fail(ctx, *second, errors.New("another SECRET"), nil); err != nil {
		t.Fatalf("second Fail: %v", err)
	}
	stored, err = s.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if stored.State != store.JobFailed || strings.Contains(stored.LastError, "SECRET") {
		t.Fatalf("exhausted job = %+v", stored)
	}
	if err := s.Heartbeat(ctx, *second, time.Minute); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("Heartbeat after Fail error = %v, want ErrLeaseLost", err)
	}
}

func TestRecordDeliveryValidationAndIgnored(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 1)

	ignored := store.Delivery{UUID: "ignored-1", ProjectID: 1, Event: "Push Hook", Outcome: store.DeliveryIgnored, Reason: "branch_not_target"}
	jobID, duplicate, err := s.RecordDelivery(ctx, ignored, nil)
	if err != nil {
		t.Fatalf("ignored delivery: %v", err)
	}
	if jobID != "" || duplicate {
		t.Fatalf("ignored delivery = %q dup=%v", jobID, duplicate)
	}
	replayedID, duplicate, err := s.RecordDelivery(ctx, ignored, nil)
	if err != nil || !duplicate || replayedID != "" {
		t.Fatalf("ignored replay = %q dup=%v err=%v", replayedID, duplicate, err)
	}
	if _, _, err := s.RecordDelivery(ctx,
		store.Delivery{UUID: "bad-1", ProjectID: 1, Event: "Push Hook", Outcome: store.DeliveryEnqueued}, nil); err == nil {
		t.Fatal("enqueued delivery without job succeeded")
	}
	if _, _, err := s.RecordDelivery(ctx,
		store.Delivery{UUID: "bad-2", ProjectID: 1, Event: "Push Hook", Outcome: "unknown"}, nil); err == nil {
		t.Fatal("unknown outcome succeeded")
	}
	if _, _, err := s.RecordDelivery(ctx,
		store.Delivery{UUID: "", ProjectID: 1, Event: "Push Hook", Outcome: store.DeliveryIgnored}, nil); err == nil {
		t.Fatal("empty uuid succeeded")
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM webhook_deliveries`); got != 1 {
		t.Fatalf("delivery rows = %d, want 1", got)
	}
}

func TestFailWithQueuedSuccessorCancelsStale(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())

	if _, _, err := s.Enqueue(ctx, store.Job{Kind: "review.run", Key: "review:4:4", Payload: json.RawMessage(`{"head":"old"}`)}); err != nil {
		t.Fatalf("Enqueue old: %v", err)
	}
	stale, err := s.Claim(ctx, "w1", []string{"review.run"}, time.Hour)
	if err != nil || stale == nil {
		t.Fatalf("Claim old: %v, %v", stale, err)
	}
	successorID, _, err := s.Enqueue(ctx, store.Job{Kind: "review.run", Key: "review:4:4", Payload: json.RawMessage(`{"head":"new"}`)})
	if err != nil {
		t.Fatalf("Enqueue successor: %v", err)
	}
	if err := s.Fail(ctx, *stale, errors.New("boom"), nil); err != nil {
		t.Fatalf("Fail stale: %v", err)
	}
	stored, err := s.GetJob(ctx, stale.ID)
	if err != nil {
		t.Fatalf("GetJob stale: %v", err)
	}
	if stored.State != store.JobCancelled {
		t.Fatalf("stale state = %s, want cancelled", stored.State)
	}
	fresh, err := s.Claim(ctx, "w2", []string{"review.run"}, time.Hour)
	if err != nil || fresh == nil || fresh.ID != successorID {
		t.Fatalf("successor claim = %+v, %v; want %s", fresh, err, successorID)
	}
	if err := s.Complete(ctx, *stale); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("late Complete error = %v, want ErrLeaseLost", err)
	}
}

func TestOpenMemoryDSN(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	defer s.Close()
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM schema_migrations`); got != migrationCount(t) {
		t.Fatalf("schema_migrations = %d, want %d", got, migrationCount(t))
	}
}

func TestEnqueueLatestReplacesQueuedPayload(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 1)

	first := store.Job{Kind: "review.run", Key: "review:1:7", Payload: json.RawMessage(`{"head":"old","full":false}`)}
	firstID, duplicate, err := s.Enqueue(ctx, first)
	if err != nil || duplicate {
		t.Fatalf("Enqueue = %q dup=%v err=%v", firstID, duplicate, err)
	}
	latest := store.Job{Kind: "review.run", Key: "review:1:7", Payload: json.RawMessage(`{"head":"new","full":true}`)}
	latestID, duplicate, err := s.EnqueueLatest(ctx, latest)
	if err != nil {
		t.Fatalf("EnqueueLatest: %v", err)
	}
	if duplicate || latestID != firstID {
		t.Fatalf("EnqueueLatest = %q dup=%v, want %q dup=false", latestID, duplicate, firstID)
	}
	stored, err := s.GetJob(ctx, firstID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if string(stored.Payload) != `{"head":"new","full":true}` || stored.State != store.JobQueued {
		t.Fatalf("stored = state %s payload %s", stored.State, stored.Payload)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE "key" = 'review:1:7'`); got != 1 {
		t.Fatalf("job count = %d, want 1", got)
	}
}

func TestEnqueueStillKeepsFirstQueuedPayload(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 1)

	firstID, _, err := s.Enqueue(ctx, store.Job{
		Kind: "review.run", Key: "review:1:8", Payload: json.RawMessage(`{"head":"first"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondID, duplicate, err := s.Enqueue(ctx, store.Job{
		Kind: "review.run", Key: "review:1:8", Payload: json.RawMessage(`{"head":"second"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate || secondID != firstID {
		t.Fatalf("Enqueue = %q dup=%v, want %q dup=true", secondID, duplicate, firstID)
	}
	stored, err := s.GetJob(ctx, firstID)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.Payload) != `{"head":"first"}` {
		t.Fatalf("ordinary Enqueue replaced payload: %s", stored.Payload)
	}
}

func TestEnqueueLatestRejectsKindConflict(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 1)

	firstID, _, err := s.Enqueue(ctx, store.Job{
		Kind: "review.run", Key: "shared:1", Payload: json.RawMessage(`{"head":"old"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnqueueLatest(ctx, store.Job{
		Kind: "review.cancel", Key: "shared:1", Payload: json.RawMessage(`{"head":"wrong-kind"}`),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("kind conflict error = %v, want ErrConflict", err)
	}
	stored, err := s.GetJob(ctx, firstID)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.Payload) != `{"head":"old"}` {
		t.Fatalf("kind conflict overwrote payload: %s", stored.Payload)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE "key" = 'shared:1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("job count = %d, want 1", count)
	}
}

func TestEnqueueLatestPreservesRunningAndQueuesSuccessor(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 1)

	firstID, _, err := s.Enqueue(ctx, store.Job{
		Kind: "review.run", Key: "review:1:9", Payload: json.RawMessage(`{"head":"old"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	running, err := s.Claim(ctx, "worker-1", []string{"review.run"}, time.Hour)
	if err != nil || running == nil {
		t.Fatalf("Claim = %+v, %v", running, err)
	}
	if running.ID != firstID {
		t.Fatalf("claimed %q, want %q", running.ID, firstID)
	}

	latestID, duplicate, err := s.EnqueueLatest(ctx, store.Job{
		Kind: "review.run", Key: "review:1:9", Payload: json.RawMessage(`{"head":"new"}`),
	})
	if err != nil {
		t.Fatalf("EnqueueLatest: %v", err)
	}
	if duplicate || latestID == firstID {
		t.Fatalf("successor = %q dup=%v, want a new id", latestID, duplicate)
	}
	stillRunning, err := s.GetJob(ctx, firstID)
	if err != nil {
		t.Fatal(err)
	}
	if stillRunning.State != store.JobRunning || string(stillRunning.Payload) != `{"head":"old"}` {
		t.Fatalf("running job changed: state %s payload %s", stillRunning.State, stillRunning.Payload)
	}
	successor, err := s.GetJob(ctx, latestID)
	if err != nil {
		t.Fatal(err)
	}
	if successor.State != store.JobQueued || string(successor.Payload) != `{"head":"new"}` {
		t.Fatalf("successor = state %s payload %s", successor.State, successor.Payload)
	}

	if err := s.Complete(ctx, *running); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	claimed, err := s.Claim(ctx, "worker-2", []string{"review.run"}, time.Hour)
	if err != nil || claimed == nil {
		t.Fatalf("second Claim = %+v, %v", claimed, err)
	}
	if claimed.ID != latestID || string(claimed.Payload) != `{"head":"new"}` {
		t.Fatalf("second claim = %+v", claimed)
	}
}

// A queued replacement must retain the job's attempt fence. Otherwise a stale
// owner with an old attempt can rule a freshly claimed payload because a reset
// attempts counter recreates the same (worker, attempt) pair.
func TestQueuedReplacementPreservesAttemptFenceAndBudget(t *testing.T) {
	ctx := context.Background()
	const worker = "worker-race"
	const kind = "review.run"
	const key = "review:1:1"
	oldPayload := json.RawMessage(`{"head":"old"}`)
	newPayload := json.RawMessage(`{"head":"new"}`)

	for _, replacement := range []string{"EnqueueLatest", "RecordDelivery"} {
		t.Run(replacement, func(t *testing.T) {
			clock := newFakeClock()
			s := openTestStore(t, clock)
			mustProject(t, s, 1)

			id, _, err := s.Enqueue(ctx, store.Job{
				Kind: kind, Key: key, Payload: oldPayload, MaxAttempts: 2,
			})
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			stale, err := s.Claim(ctx, worker, []string{kind}, 10*time.Second)
			if err != nil || stale == nil {
				t.Fatalf("first Claim = %+v, %v", stale, err)
			}
			if stale.ID != id || stale.Attempts != 1 {
				t.Fatalf("first claim = id %s attempts %d", stale.ID, stale.Attempts)
			}

			clock.Advance(11 * time.Second)
			if n, err := s.Recover(ctx); err != nil || n != 1 {
				t.Fatalf("Recover = %d, %v; want 1", n, err)
			}

			latest := store.Job{Kind: kind, Key: key, Payload: newPayload, MaxAttempts: 2}
			var replacedID string
			switch replacement {
			case "EnqueueLatest":
				replacedID, _, err = s.EnqueueLatest(ctx, latest)
			case "RecordDelivery":
				replacedID, _, err = s.RecordDelivery(ctx, store.Delivery{
					UUID: "uuid-fence", ProjectID: 1, Event: "Merge Request Hook",
					Outcome: store.DeliveryEnqueued,
				}, &latest)
			default:
				t.Fatalf("unknown replacement %q", replacement)
			}
			if err != nil {
				t.Fatalf("replace queued: %v", err)
			}
			if replacedID != id {
				t.Fatalf("replacement id = %s, want retained id %s", replacedID, id)
			}
			replaced, err := s.GetJob(ctx, id)
			if err != nil {
				t.Fatalf("GetJob after replacement: %v", err)
			}
			if replaced.Attempts != stale.Attempts {
				t.Fatalf("replacement attempts = %d, want retained %d", replaced.Attempts, stale.Attempts)
			}
			if replaced.MaxAttempts != stale.Attempts+2 {
				t.Fatalf("replacement max attempts = %d, want %d", replaced.MaxAttempts, stale.Attempts+2)
			}
			if replaced.State != store.JobQueued || string(replaced.Payload) != string(newPayload) {
				t.Fatalf("replacement = state %s payload %s", replaced.State, replaced.Payload)
			}

			fresh, err := s.Claim(ctx, worker, []string{kind}, 10*time.Second)
			if err != nil || fresh == nil {
				t.Fatalf("second Claim = %+v, %v", fresh, err)
			}
			if fresh.Attempts != stale.Attempts+1 {
				t.Fatalf("new attempt = %d, want %d (stale attempt recreated)", fresh.Attempts, stale.Attempts+1)
			}
			if fresh.ID != id || string(fresh.Payload) != string(newPayload) {
				t.Fatalf("fresh claim = id %s payload %s", fresh.ID, fresh.Payload)
			}

			assertFreshRunning := func(stage string) {
				t.Helper()
				current, err := s.GetJob(ctx, id)
				if err != nil {
					t.Fatalf("%s GetJob: %v", stage, err)
				}
				if current.State != store.JobRunning || current.Attempts != fresh.Attempts {
					t.Fatalf("%s changed fresh job: state=%s attempts=%d", stage, current.State, current.Attempts)
				}
			}
			if err := s.Heartbeat(ctx, *stale, 10*time.Second); !errors.Is(err, store.ErrLeaseLost) {
				t.Errorf("stale Heartbeat error = %v, want ErrLeaseLost", err)
			}
			assertFreshRunning("Heartbeat")
			if err := s.Complete(ctx, *stale); !errors.Is(err, store.ErrLeaseLost) {
				t.Errorf("stale Complete error = %v, want ErrLeaseLost", err)
			}
			assertFreshRunning("Complete")
			if err := s.Fail(ctx, *stale, errors.New("stale failure"), nil); !errors.Is(err, store.ErrLeaseLost) {
				t.Errorf("stale Fail error = %v, want ErrLeaseLost", err)
			}
			assertFreshRunning("Fail")

			// The replacement payload gets two fresh attempts, numbered after
			// the retained fence, rather than being exhausted immediately.
			if err := s.Fail(ctx, *fresh, errors.New("first fresh failure"), nil); err != nil {
				t.Fatalf("fresh Fail: %v", err)
			}
			retry, err := s.Claim(ctx, worker, []string{kind}, 10*time.Second)
			if err != nil || retry == nil {
				t.Fatalf("retry Claim = %+v, %v", retry, err)
			}
			if retry.Attempts != fresh.Attempts+1 || string(retry.Payload) != string(newPayload) {
				t.Fatalf("retry = attempts %d payload %s", retry.Attempts, retry.Payload)
			}
			if err := s.Fail(ctx, *retry, errors.New("second fresh failure"), nil); err != nil {
				t.Fatalf("retry Fail: %v", err)
			}
			final, err := s.GetJob(ctx, id)
			if err != nil {
				t.Fatalf("GetJob final: %v", err)
			}
			if final.State != store.JobFailed || final.Attempts != 3 || final.MaxAttempts != 3 {
				t.Fatalf("final = state %s attempts %d max %d", final.State, final.Attempts, final.MaxAttempts)
			}
		})
	}
}

// migrationCount is the number of embedded migrations a fully migrated
// database records.
func migrationCount(t *testing.T) int {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	return len(names)
}
