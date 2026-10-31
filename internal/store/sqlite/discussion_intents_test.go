package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
	sqliteDriver "modernc.org/sqlite"
)

func TestDiscussionIntentSurvivesRestartAndProjectDeletion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "intent.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	mustProject(t, s, 42)
	if created, id, err := s.ReserveDiscussionIntent(ctx, 42, 7, "key"); err != nil || !created || id != "" {
		t.Fatalf("reserve = %v, %q, %v", created, id, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if created, id, err := s.ReserveDiscussionIntent(ctx, 42, 7, "key"); err != nil || created || id != "" {
		t.Fatalf("crash-before-send reserve = %v, %q, %v", created, id, err)
	}
	for _, scope := range [][2]int64{{42, 8}, {43, 7}} {
		if created, _, err := s.ReserveDiscussionIntent(ctx, scope[0], scope[1], "key"); err != nil || !created {
			t.Fatalf("different project/MR %v blocked: %v", scope, err)
		}
	}
	if err := s.ConfirmDiscussionIntent(ctx, 42, 7, "key", "remote-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmDiscussionIntent(ctx, 42, 7, "key", "remote-2"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed receipt = %v", err)
	}
	if err := s.RejectDiscussionIntent(ctx, 42, 7, "key"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("rejected a confirmed intent: %v", err)
	}
	if created, _, err := s.ReserveDiscussionIntent(ctx, 42, 7, "unknown-key"); err != nil || !created {
		t.Fatalf("unknown reservation = %v, %v", created, err)
	}
	if err := s.DeleteProject(ctx, 42); err != nil {
		t.Fatal(err)
	}
	mustProject(t, s, 42)
	if created, id, err := s.ReserveDiscussionIntent(ctx, 42, 7, "key"); err != nil || created || id != "" {
		t.Fatalf("reregistered project exposed an unverified old receipt: %v, %q, %v", created, id, err)
	}
	if pubs, err := s.ListPublications(ctx, "new-workflow"); err != nil || len(pubs) != 0 {
		t.Fatalf("new registration exposed publications: %+v, %v", pubs, err)
	}
	if err := s.ConfirmDiscussionIntent(ctx, 42, 7, "key", "different-instance-id"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("different instance ID replaced old evidence: %v", err)
	}
	if err := s.ConfirmDiscussionIntent(ctx, 42, 7, "key", "remote-1"); err != nil {
		t.Fatalf("confirm matching current marker: %v", err)
	}
	if created, id, err := s.ReserveDiscussionIntent(ctx, 42, 7, "key"); err != nil || created || id != "remote-1" {
		t.Fatalf("verified receipt = %v, %q, %v", created, id, err)
	}
	if created, id, err := s.ReserveDiscussionIntent(ctx, 42, 7, "unknown-key"); err != nil || created || id != "" {
		t.Fatalf("reregistered project lost unknown intent: %v, %q, %v", created, id, err)
	}
}

func TestDiscussionIntentProjectDeleteRollbackKeepsReceipt(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	if created, _, err := s.ReserveDiscussionIntent(ctx, 42, 7, "key"); err != nil || !created {
		t.Fatalf("reserve = %v, %v", created, err)
	}
	if err := s.ConfirmDiscussionIntent(ctx, 42, 7, "key", "remote"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_project_delete BEFORE DELETE ON projects BEGIN SELECT RAISE(ABORT, 'delete blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, 42); err == nil {
		t.Fatal("delete unexpectedly succeeded")
	}
	if created, id, err := s.ReserveDiscussionIntent(ctx, 42, 7, "key"); err != nil || created || id != "remote" {
		t.Fatalf("failed delete changed receipt: %v, %q, %v", created, id, err)
	}
}

func TestDiscussionIntentReservationIsAtomic(t *testing.T) {
	s := openTestStore(t, newFakeClock())
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			created, _, err := s.ReserveDiscussionIntent(context.Background(), 42, 7, "key")
			results <- created
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	winners := 0
	for created := range results {
		if created {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("reservation winners = %d", winners)
	}
}

func TestDiscussionIntentReservationUsesFullSyncAndRestoresConnection(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE intent_sync_observed (level INTEGER);
		CREATE TRIGGER observe_intent_sync BEFORE INSERT ON discussion_intents BEGIN
		  INSERT INTO intent_sync_observed SELECT synchronous FROM pragma_synchronous;
		END;`); err != nil {
		t.Fatal(err)
	}
	if created, _, err := s.ReserveDiscussionIntent(ctx, 42, 7, "full-sync"); err != nil || !created {
		t.Fatalf("reservation = %v, %v", created, err)
	}
	var during, after int
	if err := s.db.QueryRowContext(ctx, `SELECT level FROM intent_sync_observed`).Scan(&during); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if during != 2 || after != 1 {
		t.Fatalf("synchronous during/after = %d/%d, want FULL/NORMAL", during, after)
	}
}

func TestDiscussionIntentFullSyncIsConnectionLocalAndExclusive(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "scope.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	other, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Raw(func(raw any) error {
		return raw.(sqliteDriver.AuthorizerRegisterer).RegisterAuthorizer(func(action sqliteDriver.AuthorizerActionCode, arg1, _, _, _ string) sqliteDriver.AuthorizerReturnCode {
			if action == sqliteDriver.AuthInsert && arg1 == "discussion_intents" {
				close(entered)
				<-release
			}
			return sqliteDriver.AuthorizerOK
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		created, _, err := s.ReserveDiscussionIntent(ctx, 42, 7, "exclusive")
		if err == nil && !created {
			err = errors.New("reservation was not created")
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("reservation did not reach insert")
	}
	var otherSync int
	if err := other.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&otherSync); err != nil || otherSync != 1 {
		t.Fatalf("other connection synchronous = %d, %v", otherSync, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	var blocked int
	if err := s.db.QueryRowContext(waitCtx, `PRAGMA synchronous`).Scan(&blocked); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("same pool bypassed exclusive connection: %d, %v", blocked, err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var restored int
	if err := s.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&restored); err != nil || restored != 1 {
		t.Fatalf("restored synchronous = %d, %v", restored, err)
	}
}

func TestDiscussionIntentRejectsNonDurableMemoryDatabase(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if created, _, err := s.ReserveDiscussionIntent(context.Background(), 42, 7, "key"); err == nil || created {
		t.Fatalf("non-durable database allowed a send: %v, %v", created, err)
	}
}

func TestDiscussionIntentDiscardsConnectionWhenSyncRestoreFails(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var denied atomic.Int32
	if err := conn.Raw(func(raw any) error {
		return raw.(sqliteDriver.AuthorizerRegisterer).RegisterAuthorizer(func(action sqliteDriver.AuthorizerActionCode, arg1, arg2, _, _ string) sqliteDriver.AuthorizerReturnCode {
			if action == sqliteDriver.AuthPragma && arg1 == "synchronous" && arg2 == "1" {
				denied.Add(1)
				return sqliteDriver.AuthorizerDeny
			}
			return sqliteDriver.AuthorizerOK
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if created, _, err := s.ReserveDiscussionIntent(ctx, 42, 7, "restore-failure"); err == nil || created {
		t.Fatalf("failed cleanup allowed send: %v, %v", created, err)
	}
	if denied.Load() != 1 {
		t.Fatalf("restore attempts denied = %d", denied.Load())
	}
	var after int
	if err := s.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&after); err != nil || after != 1 {
		t.Fatalf("replacement connection synchronous = %d, %v", after, err)
	}
	if created, _, err := s.ReserveDiscussionIntent(ctx, 42, 7, "restore-failure"); err != nil || created {
		t.Fatalf("durable unknown intent was not retained: %v, %v", created, err)
	}
}

func TestDiscussionIntentSyncSetupFailureDoesNotReserve(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Raw(func(raw any) error {
		return raw.(sqliteDriver.AuthorizerRegisterer).RegisterAuthorizer(func(action sqliteDriver.AuthorizerActionCode, arg1, arg2, _, _ string) sqliteDriver.AuthorizerReturnCode {
			if action == sqliteDriver.AuthPragma && arg1 == "synchronous" && arg2 == "FULL" {
				return sqliteDriver.AuthorizerDeny
			}
			return sqliteDriver.AuthorizerOK
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if created, _, err := s.ReserveDiscussionIntent(ctx, 42, 7, "setup-failure"); err == nil || created {
		t.Fatalf("failed sync setup allowed send: %v, %v", created, err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM discussion_intents WHERE publish_key = ?`, "setup-failure"); got != 0 {
		t.Fatalf("failed sync setup persisted %d intents", got)
	}
	var after int
	if err := s.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&after); err != nil || after != 1 {
		t.Fatalf("synchronous after failure = %d, %v", after, err)
	}
}

func TestDiscussionIntentInsertFailureRestoresSync(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_intent BEFORE INSERT ON discussion_intents WHEN NEW.publish_key = 'fail' BEGIN SELECT RAISE(ABORT, 'insert blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if created, _, err := s.ReserveDiscussionIntent(ctx, 42, 7, "fail"); err == nil || created {
		t.Fatalf("failed insert allowed send: %v, %v", created, err)
	}
	var after int
	if err := s.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&after); err != nil || after != 1 {
		t.Fatalf("synchronous after failed insert = %d, %v", after, err)
	}
	if created, _, err := s.ReserveDiscussionIntent(ctx, 42, 7, "other"); err != nil || !created {
		t.Fatalf("later reservation = %v, %v", created, err)
	}
}

func TestDiscussionIntentCancelledTransactionRestoresSync(t *testing.T) {
	s := openTestStore(t, newFakeClock())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Raw(func(raw any) error {
		return raw.(sqliteDriver.AuthorizerRegisterer).RegisterAuthorizer(func(action sqliteDriver.AuthorizerActionCode, arg1, _, _, _ string) sqliteDriver.AuthorizerReturnCode {
			if action == sqliteDriver.AuthInsert && arg1 == "discussion_intents" {
				cancel()
			}
			return sqliteDriver.AuthorizerOK
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if created, _, err := s.ReserveDiscussionIntent(ctx, 42, 7, "cancelled"); err == nil || created {
		t.Fatalf("cancelled intent allowed send: %v, %v", created, err)
	}
	var after int
	if err := s.db.QueryRowContext(context.Background(), `PRAGMA synchronous`).Scan(&after); err != nil || after != 1 {
		t.Fatalf("synchronous after cancellation = %d, %v", after, err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM discussion_intents WHERE publish_key = ?`, "cancelled"); got != 0 {
		t.Fatalf("cancelled reservation persisted %d intents", got)
	}
}

func TestDiscussionIntentMigrationFencesLegacyFindings(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	mustProject(t, s, 42)
	mustCreateWorkflow(t, s, "legacy", reviewMeta(42, 7, "head"), newFakeClock().Now())
	position := true
	if err := s.ReplaceFindings(ctx, "legacy", []store.Finding{
		{ID: "f1", WorkflowID: "legacy", OriginNode: "review", Severity: "P2", Category: "logic", Title: "first", Body: "body", Decision: "publish", PositionOK: &position, PublishKey: "known-key"},
		{ID: "f2", WorkflowID: "legacy", OriginNode: "review", Severity: "P2", Category: "logic", Title: "second", Body: "body", Decision: "mark", PositionOK: &position},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertPublication(ctx, store.Publication{ProjectID: 42, MRIID: 7, Kind: "discussion", PublishKey: "confirmed-key", GitlabID: "legacy-id", WorkflowID: "legacy", Fingerprint: "fp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE discussion_intents; DELETE FROM schema_migrations WHERE version = 22`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, key := range []string{"known-key", "any-other-key"} {
		if created, id, err := s.ReserveDiscussionIntent(ctx, 42, 7, key); err != nil || created || id != "" {
			t.Fatalf("legacy key %q not fenced: %v, %q, %v", key, created, id, err)
		}
	}
	if created, id, err := s.ReserveDiscussionIntent(ctx, 42, 7, "confirmed-key"); err != nil || created || id != "legacy-id" {
		t.Fatalf("legacy receipt = %v, %q, %v", created, id, err)
	}
	if created, _, err := s.ReserveDiscussionIntent(ctx, 42, 8, "known-key"); err != nil || !created {
		t.Fatalf("different MR blocked: %v", err)
	}
}
