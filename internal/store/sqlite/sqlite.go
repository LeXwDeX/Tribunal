// Package sqlite implements the M0 store on SQLite.
package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	sqlitedrv "modernc.org/sqlite"

	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

const (
	timeLayout       = "2006-01-02T15:04:05.000000000Z"
	onboardingKind   = "workspace.onboard"
	defaultMaxRetry  = 3
	pragmaParameters = "_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
)

// MaxDeferWindow bounds how long repeated ownership contention may defer one
// job. Once the first deferral is older than this window, Defer fails the job
// instead of extending its attempt budget again.
const MaxDeferWindow = 15 * time.Minute

// Store is a SQLite-backed store. One connection serializes all reads and
// writes; this is sufficient for the single-process M0 deployment.
type Store struct {
	db        *sql.DB
	now       func() time.Time
	mcpCipher atomic.Pointer[secret.Cipher]
}

// Option customizes Open.
type Option func(*Store)

// WithClock replaces the store clock, mainly for tests.
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// Open opens dsn, applies the required pragmas on every connection, runs the
// embedded migrations, and returns a ready store. The caller owns Close.
func Open(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("sqlite: empty dsn")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", withPragmas(dsn))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{db: db, now: time.Now}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: ping: %w", err)
	}
	if err := migrate(ctx, db, s.now); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func withPragmas(dsn string) string {
	// DSN pragmas apply to every new connection, including reconnects.
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + pragmaParameters
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Ping checks that the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func migrate(ctx context.Context, db *sql.DB, now func() time.Time) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("sqlite: create schema_migrations: %w", err)
	}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("sqlite: read migrations: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	applied := make(map[int]bool)
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("sqlite: list migrations: %w", err)
	}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			rows.Close()
			return fmt.Errorf("sqlite: scan migration: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("sqlite: close migration rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: iterate migrations: %w", err)
	}

	seen := make(map[int]string)
	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			return err
		}
		if previous, ok := seen[version]; ok {
			return fmt.Errorf("sqlite: duplicate migration version %d: %s and %s", version, previous, name)
		}
		seen[version] = name
		if applied[version] {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("sqlite: read migration %s: %w", name, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite: begin migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("sqlite: apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			version, name, formatTime(now())); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("sqlite: record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqlite: commit migration %s: %w", name, err)
		}
	}
	return nil
}

func migrationVersion(name string) (int, error) {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, fmt.Errorf("sqlite: migration %q has no version prefix", name)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("sqlite: migration %q has invalid version", name)
	}
	return version, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("sqlite: parse time %q: %w", value, err)
	}
	return t.UTC(), nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func nullableInt(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func newID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("sqlite: generate id: %w", err)
	}
	return id.String(), nil
}

func isConstraintErr(err error) bool {
	var sqliteErr *sqlitedrv.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code()&0xff == 19 // SQLITE_CONSTRAINT
	}
	return strings.Contains(err.Error(), "constraint failed")
}

func (s *Store) prepareJob(j *store.Job, now time.Time) error {
	if j == nil {
		return errors.New("sqlite: nil job")
	}
	if strings.TrimSpace(j.Kind) == "" {
		return errors.New("sqlite: job kind is required")
	}
	if j.MaxAttempts == 0 {
		j.MaxAttempts = defaultMaxRetry
	}
	if j.MaxAttempts <= 0 {
		return errors.New("sqlite: max attempts must be positive")
	}
	if j.ID == "" {
		id, err := newID()
		if err != nil {
			return err
		}
		j.ID = id
	}
	if len(bytes.TrimSpace(j.Payload)) == 0 {
		j.Payload = json.RawMessage(`{}`)
	} else if !json.Valid(j.Payload) {
		return errors.New("sqlite: job payload is not valid JSON")
	}
	if j.RunAfter.IsZero() {
		j.RunAfter = now
	}
	j.RunAfter = j.RunAfter.UTC()
	j.State = store.JobQueued
	j.Attempts = 0
	j.LeaseOwner = ""
	j.LeaseUntil = nil
	j.LastError = ""
	j.CreatedAt = now
	j.UpdatedAt = now
	return nil
}

func enqueueTx(ctx context.Context, tx *sql.Tx, j *store.Job, replaceQueued bool) (string, bool, error) {
	if j.Key != "" {
		var (
			queuedID   string
			queuedKind string
		)
		err := tx.QueryRowContext(ctx,
			`SELECT id, kind FROM jobs WHERE "key" = ? AND state = ?`,
			j.Key, store.JobQueued).Scan(&queuedID, &queuedKind)
		switch {
		case err == nil:
			if queuedKind != j.Kind {
				return "", false, fmt.Errorf("%w: queued job %s has kind %s", store.ErrConflict, queuedID, queuedKind)
			}
			if !replaceQueued {
				return queuedID, true, nil
			}
			// Keep attempts standing as the fence for this retained job ID.
			// MaxAttempts is the absolute ceiling for the record, so grant the
			// replacement payload its bounded budget after the current attempt.
			res, err := tx.ExecContext(ctx, `UPDATE jobs
				SET payload = ?, max_attempts = attempts + ?, defer_since = NULL, run_after = ?,
				    lease_owner = NULL, lease_until = NULL, last_error = NULL, updated_at = ?
				WHERE id = ? AND state = ?`,
				string(j.Payload), j.MaxAttempts, formatTime(j.RunAfter), formatTime(j.UpdatedAt),
				queuedID, store.JobQueued)
			if err != nil {
				return "", false, fmt.Errorf("sqlite: replace queued job: %w", err)
			}
			if n, err := res.RowsAffected(); err != nil || n != 1 {
				if err != nil {
					return "", false, fmt.Errorf("sqlite: replace queued job: %w", err)
				}
				return "", false, fmt.Errorf("%w: queued job %s changed", store.ErrConflict, queuedID)
			}
			return queuedID, false, nil
		case errors.Is(err, sql.ErrNoRows):
		default:
			return "", false, fmt.Errorf("sqlite: look up queued job: %w", err)
		}
	}

	_, err := tx.ExecContext(ctx, `INSERT INTO jobs
		(id, kind, "key", payload, state, attempts, max_attempts, run_after,
		 lease_owner, lease_until, last_error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL, ?, ?)`,
		j.ID, j.Kind, nullString(j.Key), string(j.Payload), store.JobQueued, 0,
		j.MaxAttempts, formatTime(j.RunAfter), formatTime(j.CreatedAt), formatTime(j.UpdatedAt))
	if err != nil {
		if isConstraintErr(err) {
			return "", false, fmt.Errorf("%w: job id or active key already exists", store.ErrConflict)
		}
		return "", false, fmt.Errorf("sqlite: insert job: %w", err)
	}
	return j.ID, false, nil
}

// Enqueue inserts a queued job. If a queued job with the same non-empty key
// already exists, it returns that job's ID with dup=true. A running job with
// the same key does not block a queued successor.
func (s *Store) Enqueue(ctx context.Context, j store.Job) (string, bool, error) {
	return s.enqueue(ctx, j, false)
}

// EnqueueLatest inserts a queued job or replaces the payload of an existing
// queued job with the same key and kind. A running job is never modified, and
// a queued successor is preserved when the original is running. A replacement
// keeps the retained job's attempt fence and grants MaxAttempts further
// attempts beyond the current attempt count. The returned dup value is always
// false: a replaced queued job is not a duplicate.
func (s *Store) EnqueueLatest(ctx context.Context, j store.Job) (string, bool, error) {
	return s.enqueue(ctx, j, true)
}

func (s *Store) enqueue(ctx context.Context, j store.Job, replaceQueued bool) (string, bool, error) {
	now := s.now().UTC()
	if err := s.prepareJob(&j, now); err != nil {
		return "", false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("sqlite: begin enqueue: %w", err)
	}
	defer tx.Rollback()

	id, dup, err := enqueueTx(ctx, tx, &j, replaceQueued)
	if err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("sqlite: commit enqueue: %w", err)
	}
	return id, dup, nil
}

// Claim leases the next runnable job of one of kinds. An empty kinds list
// claims nothing. A queued successor is not claimable while a job with the
// same key is running. Claim also recovers expired leases before selecting.
func (s *Store) Claim(ctx context.Context, worker string, kinds []string, lease time.Duration) (*store.Job, error) {
	if strings.TrimSpace(worker) == "" {
		return nil, errors.New("sqlite: worker is required")
	}
	if lease <= 0 {
		return nil, errors.New("sqlite: lease must be positive")
	}
	if len(kinds) == 0 {
		return nil, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin claim: %w", err)
	}
	defer tx.Rollback()

	now := s.now().UTC()
	if _, err := recoverTx(ctx, tx, now); err != nil {
		return nil, err
	}

	placeholders := make([]string, len(kinds))
	args := make([]any, 0, len(kinds)+3)
	args = append(args, store.JobQueued, formatTime(now))
	for i, kind := range kinds {
		placeholders[i] = "?"
		args = append(args, kind)
	}
	args = append(args, store.JobRunning)
	query := `SELECT id FROM jobs
		WHERE state = ? AND attempts < max_attempts AND run_after <= ?
		  AND kind IN (` + strings.Join(placeholders, ", ") + `)
		  AND ("key" IS NULL OR NOT EXISTS (
		      SELECT 1 FROM jobs r WHERE r."key" = jobs."key" AND r.state = ?
		  ))
		ORDER BY run_after, created_at, id LIMIT 1`

	var id string
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("sqlite: select claim: %w", err)
	}

	row := tx.QueryRowContext(ctx, `UPDATE jobs
		SET state = ?, attempts = attempts + 1, lease_owner = ?, lease_until = ?, updated_at = ?
		WHERE id = ? AND state = ?
		RETURNING `+jobColumns,
		store.JobRunning, worker, formatTime(now.Add(lease)), formatTime(now),
		id, store.JobQueued)
	job, err := scanJob(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("sqlite: lease job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit claim: %w", err)
	}
	return job, nil
}

func recoverTx(ctx context.Context, tx *sql.Tx, now time.Time) (int, error) {
	// Attempts increment only on Claim, so recovery requeues without
	// double-counting an attempt that was already reserved.
	nowText := formatTime(now)
	count := 0
	// A stale running job with a queued successor must not block or overwrite
	// the successor.
	res, err := tx.ExecContext(ctx, `UPDATE jobs
		SET state = ?, lease_owner = NULL, lease_until = NULL, defer_since = NULL,
		    last_error = 'superseded by queued job', updated_at = ?
		WHERE state = ? AND (lease_until IS NULL OR lease_until < ?)
		  AND "key" IS NOT NULL
		  AND EXISTS (SELECT 1 FROM jobs q WHERE q."key" = jobs."key" AND q.state = ?)`,
		store.JobCancelled, nowText, store.JobRunning, nowText, store.JobQueued)
	if err != nil {
		return 0, fmt.Errorf("sqlite: cancel stale leases: %w", err)
	}
	n, _ := res.RowsAffected()
	count += int(n)

	res, err = tx.ExecContext(ctx, `UPDATE jobs
		SET state = ?, lease_owner = NULL, lease_until = NULL, defer_since = NULL,
		    last_error = 'lease expired after maximum attempts', updated_at = ?
		WHERE state = ? AND (lease_until IS NULL OR lease_until < ?)
		  AND attempts >= max_attempts`,
		store.JobFailed, nowText, store.JobRunning, nowText)
	if err != nil {
		return 0, fmt.Errorf("sqlite: fail expired leases: %w", err)
	}
	n, _ = res.RowsAffected()
	count += int(n)

	res, err = tx.ExecContext(ctx, `UPDATE jobs
		SET state = ?, run_after = ?, lease_owner = NULL, lease_until = NULL,
		    defer_since = NULL, last_error = 'lease expired; retry scheduled', updated_at = ?
		WHERE state = ? AND (lease_until IS NULL OR lease_until < ?)`,
		store.JobQueued, nowText, nowText, store.JobRunning, nowText)
	if err != nil {
		return 0, fmt.Errorf("sqlite: requeue expired leases: %w", err)
	}
	n, _ = res.RowsAffected()
	count += int(n)
	return count, nil
}

// Recover returns expired running jobs to the queue, or terminates them when
// their attempt budget is exhausted or a newer queued successor exists.
func (s *Store) Recover(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("sqlite: begin recover: %w", err)
	}
	defer tx.Rollback()

	count, err := recoverTx(ctx, tx, s.now().UTC())
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("sqlite: commit recover: %w", err)
	}
	return count, nil
}

// Heartbeat extends the lease of a running job. It rejects a stale owner,
// attempt, or lease.
func (s *Store) Heartbeat(ctx context.Context, j store.Job, lease time.Duration) error {
	if lease <= 0 {
		return errors.New("sqlite: lease must be positive")
	}
	now := s.now().UTC()
	res, err := s.db.ExecContext(ctx, `UPDATE jobs
		SET lease_until = ?, updated_at = ?
		WHERE id = ? AND state = ? AND lease_owner = ? AND attempts = ?
		  AND lease_until IS NOT NULL AND lease_until >= ?`,
		formatTime(now.Add(lease)), formatTime(now),
		j.ID, store.JobRunning, j.LeaseOwner, j.Attempts, formatTime(now))
	if err != nil {
		return fmt.Errorf("sqlite: heartbeat: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sqlite: heartbeat: %w", err)
	} else if n != 1 {
		return store.ErrLeaseLost
	}
	return nil
}

// Complete marks a running job done. It rejects a stale owner, attempt, or
// expired lease.
func (s *Store) Complete(ctx context.Context, j store.Job) error {
	now := s.now().UTC()
	res, err := s.db.ExecContext(ctx, `UPDATE jobs
		SET state = ?, lease_owner = NULL, lease_until = NULL, defer_since = NULL,
		    last_error = NULL, updated_at = ?
		WHERE id = ? AND state = ? AND lease_owner = ? AND attempts = ?
		  AND lease_until IS NOT NULL AND lease_until >= ?`,
		store.JobDone, formatTime(now),
		j.ID, store.JobRunning, j.LeaseOwner, j.Attempts, formatTime(now))
	if err != nil {
		return fmt.Errorf("sqlite: complete: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sqlite: complete: %w", err)
	} else if n != 1 {
		return store.ErrLeaseLost
	}
	return nil
}

// Fail records a failed attempt. Handler error text is deliberately not
// stored; only a fixed reason and the sanitized error class are persisted. If
// a queued successor exists, the stale running job is cancelled instead of
// overwriting it.
func (s *Store) Fail(ctx context.Context, j store.Job, cause error, retryAt *time.Time) error {

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin fail: %w", err)
	}
	defer tx.Rollback()

	now := s.now().UTC()
	var maxAttempts int
	err = tx.QueryRowContext(ctx, `SELECT max_attempts FROM jobs
		WHERE id = ? AND state = ? AND lease_owner = ? AND attempts = ?
		  AND lease_until IS NOT NULL AND lease_until >= ?`,
		j.ID, store.JobRunning, j.LeaseOwner, j.Attempts, formatTime(now)).Scan(&maxAttempts)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("sqlite: locate running job: %w", err)
	}

	var successors int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs q
		JOIN jobs r ON q."key" = r."key"
		WHERE r.id = ? AND q.state = ?`, j.ID, store.JobQueued).Scan(&successors); err != nil {
		return fmt.Errorf("sqlite: count queued successors: %w", err)
	}

	state := store.JobQueued
	lastError := "job failed; retry scheduled"
	runAfter := now
	if successors > 0 {
		state = store.JobCancelled
		lastError = "superseded by queued job"
	} else if j.Attempts >= maxAttempts {
		state = store.JobFailed
		lastError = "job failed after maximum attempts"
	} else if retryAt != nil {
		runAfter = retryAt.UTC()
	}
	if class := errclass.Of(cause); class != "" && successors == 0 {
		lastError += " (" + class + ")"
	}

	res, err := tx.ExecContext(ctx, `UPDATE jobs
		SET state = ?, run_after = ?, lease_owner = NULL, lease_until = NULL,
		    defer_since = NULL, last_error = ?, updated_at = ?
		WHERE id = ? AND state = ? AND lease_owner = ? AND attempts = ?
		  AND lease_until IS NOT NULL AND lease_until >= ?`,
		state, formatTime(runAfter), lastError, formatTime(now),
		j.ID, store.JobRunning, j.LeaseOwner, j.Attempts, formatTime(now))
	if err != nil {
		return fmt.Errorf("sqlite: fail job: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sqlite: fail job: %w", err)
	} else if n != 1 {
		return store.ErrLeaseLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit fail: %w", err)
	}
	return nil
}

// Defer releases a contended lease while preserving both the retry budget and
// the monotonic attempt fence used to reject stale owners. The first deferral
// time is retained: once the job has been deferred for longer than
// MaxDeferWindow, it fails instead of extending its attempt budget again.
func (s *Store) Defer(ctx context.Context, j store.Job, retryAt time.Time) error {
	now := s.now().UTC()
	cutoff := formatTime(now.Add(-MaxDeferWindow))
	res, err := s.db.ExecContext(ctx, `UPDATE jobs
		SET state = CASE
		    WHEN "key" IS NOT NULL AND EXISTS (
		        SELECT 1 FROM jobs q WHERE q."key" = jobs."key" AND q.state = ?
		    ) THEN ?
		    WHEN defer_since IS NOT NULL AND defer_since <= ? THEN ?
		    ELSE ? END,
		max_attempts = max_attempts + CASE
		    WHEN defer_since IS NOT NULL AND defer_since <= ? THEN 0
		    ELSE 1 END,
		defer_since = COALESCE(defer_since, ?),
		run_after = ?, lease_owner = NULL, lease_until = NULL,
		last_error = CASE
		    WHEN "key" IS NOT NULL AND EXISTS (
		        SELECT 1 FROM jobs q WHERE q."key" = jobs."key" AND q.state = ?
		    ) THEN 'superseded by queued job'
		    WHEN defer_since IS NOT NULL AND defer_since <= ? THEN 'execution deferral window exceeded'
		    ELSE 'execution deferred' END,
		updated_at = ?
		WHERE id = ? AND state = ? AND lease_owner = ? AND attempts = ?
		  AND lease_until IS NOT NULL AND lease_until >= ?`,
		store.JobQueued, store.JobCancelled, cutoff, store.JobFailed, store.JobQueued,
		cutoff, formatTime(now), formatTime(retryAt.UTC()), store.JobQueued, cutoff, formatTime(now),
		j.ID, store.JobRunning, j.LeaseOwner, j.Attempts, formatTime(now))
	if err != nil {
		return fmt.Errorf("sqlite: defer job: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sqlite: defer job: %w", err)
	} else if n != 1 {
		return store.ErrLeaseLost
	}
	return nil
}

// CancelByKey cancels queued jobs whose key exactly matches key. It never
// touches running jobs. The comparison is parameterized, so SQL wildcards in
// key are literal.
func (s *Store) CancelByKey(ctx context.Context, key string) (int, error) {
	if key == "" {
		return 0, errors.New("sqlite: key is required")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE jobs
		SET state = ?, lease_owner = NULL, lease_until = NULL,
		    last_error = 'cancelled by key', updated_at = ?
		WHERE state = ? AND "key" = ?`,
		store.JobCancelled, formatTime(s.now()), store.JobQueued, key)
	if err != nil {
		return 0, fmt.Errorf("sqlite: cancel by key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: cancel by key: %w", err)
	}
	return int(n), nil
}

// RegisterProject atomically inserts a project, a new workspace, and its
// onboarding job. Duplicate project registration is a conflict and never
// overwrites the existing row.
func (s *Store) RegisterProject(ctx context.Context, p store.Project) (string, error) {
	if p.ID <= 0 {
		return "", errors.New("sqlite: project id is required")
	}
	if strings.TrimSpace(p.PathWithNamespace) == "" {
		return "", errors.New("sqlite: path with namespace is required")
	}
	if strings.TrimSpace(p.WebURL) == "" {
		return "", errors.New("sqlite: web url is required")
	}
	if strings.TrimSpace(p.DefaultBranch) == "" {
		return "", errors.New("sqlite: default branch is required")
	}
	if len(p.TokenEnc) == 0 {
		return "", errors.New("sqlite: encrypted token is required")
	}
	settings := p.Settings
	if len(settings) == 0 {
		settings = json.RawMessage(`{}`)
	} else if !json.Valid(settings) {
		return "", errors.New("sqlite: project settings is not valid JSON")
	}
	scopes := p.TokenScopes
	if scopes == nil {
		scopes = []string{}
	}
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		return "", fmt.Errorf("sqlite: encode token scopes: %w", err)
	}
	if p.Profile == "" {
		p.Profile = "default"
	}
	if p.WebhookSecretEnc == nil {
		p.WebhookSecretEnc = []byte{}
	}
	if p.WebhookState == "" {
		p.WebhookState = store.WebhookStateDisabled
		if p.HookID != nil {
			p.WebhookState = store.WebhookStateUnknown
		}
	}
	if p.WebhookEvents == nil {
		p.WebhookEvents = []string{}
	}
	eventsJSON, err := json.Marshal(p.WebhookEvents)
	if err != nil {
		return "", fmt.Errorf("sqlite: encode webhook events: %w", err)
	}

	now := s.now().UTC()
	if p.HookID != nil && p.WebhookManagedBy == "" {
		// Compatibility for pre-ownership callers: a stored hook ID can only
		// belong to a service-created hook.
		p.WebhookManagedBy = store.WebhookManagedByService
	}
	if p.WebhookManagedBy != "" && p.WebhookCreatedAt == nil {
		p.WebhookCreatedAt = &now
		p.WebhookUpdatedAt = &now
	}
	// New registrations are active; a later management API controls enabled.
	p.Enabled = true
	p.CreatedAt = now
	p.UpdatedAt = now

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("sqlite: begin register project: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `INSERT INTO projects
		(id, path_with_namespace, web_url, default_branch, token_enc, token_expires_at,
		 token_scopes, webhook_secret_enc, hook_id, webhook_state, webhook_events,
		 webhook_error, webhook_managed_by, webhook_created_at, webhook_updated_at,
		 profile, settings, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.PathWithNamespace, p.WebURL, p.DefaultBranch, p.TokenEnc,
		nullableTime(p.TokenExpiresAt), string(scopesJSON), p.WebhookSecretEnc,
		nullableInt(p.HookID), p.WebhookState, string(eventsJSON), p.WebhookError,
		p.WebhookManagedBy, nullableTime(p.WebhookCreatedAt), nullableTime(p.WebhookUpdatedAt),
		p.Profile, string(settings), 1,
		formatTime(p.CreatedAt), formatTime(p.UpdatedAt))
	if err != nil {
		if isConstraintErr(err) {
			return "", fmt.Errorf("%w: project %d already registered", store.ErrConflict, p.ID)
		}
		return "", fmt.Errorf("sqlite: insert project: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspaces
		(project_id, state, updated_at) VALUES (?, ?, ?)`,
		p.ID, "new", formatTime(now)); err != nil {
		if isConstraintErr(err) {
			return "", fmt.Errorf("%w: workspace for project %d already exists", store.ErrConflict, p.ID)
		}
		return "", fmt.Errorf("sqlite: insert workspace: %w", err)
	}

	job := store.Job{
		Kind:        onboardingKind,
		Key:         fmt.Sprintf("workspace.onboard:%d", p.ID),
		Payload:     json.RawMessage(fmt.Sprintf(`{"project_id":%d}`, p.ID)),
		MaxAttempts: defaultMaxRetry,
	}
	if err := s.prepareJob(&job, now); err != nil {
		return "", err
	}
	jobID, _, err := enqueueTx(ctx, tx, &job, false)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("sqlite: commit register project: %w", err)
	}
	return jobID, nil
}

const projectColumns = `id, path_with_namespace, web_url, default_branch, token_enc,
	token_expires_at, token_scopes, webhook_secret_enc, hook_id, webhook_state,
	webhook_events, webhook_error, webhook_managed_by, webhook_created_at,
	webhook_updated_at, profile, settings, enabled, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanProject(row rowScanner) (*store.Project, error) {
	var (
		p              store.Project
		tokenExpires   sql.NullString
		scopesText     string
		hookID         sql.NullInt64
		eventsText     string
		webhookCreated sql.NullString
		webhookUpdated sql.NullString
		settings       string
		enabled        int
		createdAt      string
		updatedAt      string
	)
	if err := row.Scan(&p.ID, &p.PathWithNamespace, &p.WebURL, &p.DefaultBranch,
		&p.TokenEnc, &tokenExpires, &scopesText, &p.WebhookSecretEnc, &hookID,
		&p.WebhookState, &eventsText, &p.WebhookError,
		&p.WebhookManagedBy, &webhookCreated, &webhookUpdated,
		&p.Profile, &settings, &enabled, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	if webhookCreated.Valid {
		t, err := parseTime(webhookCreated.String)
		if err != nil {
			return nil, err
		}
		p.WebhookCreatedAt = &t
	}
	if webhookUpdated.Valid {
		t, err := parseTime(webhookUpdated.String)
		if err != nil {
			return nil, err
		}
		p.WebhookUpdatedAt = &t
	}
	if tokenExpires.Valid {
		t, err := parseTime(tokenExpires.String)
		if err != nil {
			return nil, err
		}
		p.TokenExpiresAt = &t
	}
	if scopesText == "" {
		scopesText = "[]"
	}
	if err := json.Unmarshal([]byte(scopesText), &p.TokenScopes); err != nil {
		return nil, fmt.Errorf("sqlite: decode token scopes: %w", err)
	}
	if p.TokenScopes == nil {
		p.TokenScopes = []string{}
	}
	if eventsText == "" {
		eventsText = "[]"
	}
	if err := json.Unmarshal([]byte(eventsText), &p.WebhookEvents); err != nil {
		return nil, fmt.Errorf("sqlite: decode webhook events: %w", err)
	}
	if p.WebhookEvents == nil {
		p.WebhookEvents = []string{}
	}
	if hookID.Valid {
		value := hookID.Int64
		p.HookID = &value
	}
	if settings == "" {
		settings = "{}"
	}
	p.Settings = json.RawMessage(settings)
	p.Enabled = enabled != 0
	var err error
	if p.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if p.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetProject returns a registered project.
func (s *Store) GetProject(ctx context.Context, id int64) (*store.Project, error) {
	p, err := scanProject(s.db.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: project %d", store.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get project: %w", err)
	}
	return p, nil
}

// ListProjects returns all registered projects ordered by ID.
func (s *Store) ListProjects(ctx context.Context) ([]store.Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list projects: %w", err)
	}
	defer rows.Close()
	projects := make([]store.Project, 0)
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan project: %w", err)
		}
		projects = append(projects, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list projects: %w", err)
	}
	return projects, nil
}

const jobColumns = `id, kind, "key", payload, state, attempts, max_attempts, run_after,
	lease_owner, lease_until, last_error, created_at, updated_at`

func scanJob(row rowScanner) (*store.Job, error) {
	var (
		j          store.Job
		key        sql.NullString
		payload    string
		leaseOwner sql.NullString
		leaseUntil sql.NullString
		lastError  sql.NullString
		runAfter   string
		createdAt  string
		updatedAt  string
	)
	if err := row.Scan(&j.ID, &j.Kind, &key, &payload, &j.State, &j.Attempts,
		&j.MaxAttempts, &runAfter, &leaseOwner, &leaseUntil, &lastError,
		&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	j.Key = key.String
	j.Payload = json.RawMessage(payload)
	j.LeaseOwner = leaseOwner.String
	j.LastError = lastError.String
	if leaseUntil.Valid {
		t, err := parseTime(leaseUntil.String)
		if err != nil {
			return nil, err
		}
		j.LeaseUntil = &t
	}
	var err error
	if j.RunAfter, err = parseTime(runAfter); err != nil {
		return nil, err
	}
	if j.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if j.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &j, nil
}

// GetJob returns a job by ID.
func (s *Store) GetJob(ctx context.Context, id string) (*store.Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: job %s", store.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get job: %w", err)
	}
	return j, nil
}

// RecordDelivery atomically records a webhook delivery and, when j is not nil,
// enqueues its job. A replayed (project_id, uuid) returns the original job ID
// with duplicate=true and enqueues nothing. For an enqueued delivery with a
// queued job of the same key and kind, the queued payload is replaced with the
// latest one; a running job is never replaced or cancelled at ingress.
func (s *Store) RecordDelivery(ctx context.Context, d store.Delivery, j *store.Job) (string, bool, error) {
	if d.UUID == "" {
		return "", false, errors.New("sqlite: delivery uuid is required")
	}
	if d.ProjectID <= 0 {
		return "", false, errors.New("sqlite: delivery project id is required")
	}
	if d.Event == "" {
		return "", false, errors.New("sqlite: delivery event is required")
	}
	switch d.Outcome {
	case store.DeliveryEnqueued:
		if j == nil {
			return "", false, errors.New("sqlite: enqueued delivery requires a job")
		}
	case store.DeliveryIgnored, store.DeliveryRejected:
		if j != nil {
			return "", false, errors.New("sqlite: ignored or rejected delivery must not enqueue a job")
		}
	default:
		return "", false, fmt.Errorf("sqlite: invalid delivery outcome %q", d.Outcome)
	}

	now := s.now().UTC()
	if d.ReceivedAt.IsZero() {
		d.ReceivedAt = now
	} else {
		d.ReceivedAt = d.ReceivedAt.UTC()
	}
	if j != nil {
		if err := s.prepareJob(j, now); err != nil {
			return "", false, err
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("sqlite: begin delivery: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `INSERT INTO webhook_deliveries
		(project_id, uuid, event, outcome, reason, job_id, received_at)
		VALUES (?, ?, ?, ?, ?, NULL, ?)
		ON CONFLICT (project_id, uuid) DO NOTHING`,
		d.ProjectID, d.UUID, d.Event, d.Outcome, nullString(d.Reason), formatTime(d.ReceivedAt))
	if err != nil {
		return "", false, fmt.Errorf("sqlite: insert delivery: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return "", false, fmt.Errorf("sqlite: insert delivery: %w", err)
	}
	if inserted == 0 {
		var existing sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT job_id FROM webhook_deliveries
			WHERE project_id = ? AND uuid = ?`, d.ProjectID, d.UUID).Scan(&existing); err != nil {
			return "", false, fmt.Errorf("sqlite: read delivery: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return "", false, fmt.Errorf("sqlite: commit delivery: %w", err)
		}
		return existing.String, true, nil
	}

	jobID := ""
	if j != nil {
		jobID, _, err = enqueueTx(ctx, tx, j, true)
		if err != nil {
			return "", false, err
		}
		// A queued replacement keeps the existing job ID; reflect it for the caller.
		j.ID = jobID
		if _, err := tx.ExecContext(ctx, `UPDATE webhook_deliveries SET job_id = ?
			WHERE project_id = ? AND uuid = ?`, jobID, d.ProjectID, d.UUID); err != nil {
			return "", false, fmt.Errorf("sqlite: attach delivery job: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("sqlite: commit delivery: %w", err)
	}
	return jobID, false, nil
}
