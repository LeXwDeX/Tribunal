package app

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

func ledgerRows(t *testing.T, st *sqlite.Store) map[string]store.ExtensionDefault {
	t.Helper()
	rows, err := st.ListExtensionDefaults(context.Background())
	if err != nil {
		t.Fatalf("ListExtensionDefaults: %v", err)
	}
	out := make(map[string]store.ExtensionDefault, len(rows))
	for _, row := range rows {
		out[row.Key] = row
	}
	return out
}

// A block the configuration file declares after the defaults bound it wins
// while the stored row is still exactly what the defaults wrote.
func TestFileBlockReplacesDefaultOwnedBinding(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	if _, ok := platformBindings(t, st)["review"]; !ok {
		t.Fatal("defaults did not bind the review block")
	}

	fileBlocks := map[string]config.BlockBinding{"review": {Skills: []string{"coding-standards"}}}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	for i := 0; i < 2; i++ {
		defaults, err := service.SkillDefaults(defaultCodeReview)
		if err != nil {
			t.Fatal(err)
		}
		if err := ApplyExtensionDefaults(ctx, st, fileBlocks, defaults, logger); err != nil {
			t.Fatalf("ApplyExtensionDefaults: %v", err)
		}
	}
	bindings := platformBindings(t, st)
	if row, ok := bindings["review"]; ok {
		t.Fatalf("default-owned review row still shadows the file block: %+v", row)
	}
	if got := bindings["code-review"].Skills; !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("code-review skills = %v, want default kept", got)
	}
	if n := strings.Count(logs.String(), "file configuration replaces default block binding"); n != 1 {
		t.Fatalf("release logged %d times, want once:\n%s", n, logs.String())
	}
	blocks, _, _, err := loadStoredPlatformExtensions(ctx, st, fileBlocks, mcp.Config{}, nil)
	if err != nil {
		t.Fatalf("loadStoredPlatformExtensions: %v", err)
	}
	if got := blocks["review"].Skills; !reflect.DeepEqual(got, []string{"coding-standards"}) {
		t.Fatalf("effective review skills = %v, want the file binding", got)
	}
}

// The release also happens when no defaults are configured any more.
func TestFileBlockReplacesDefaultOwnedBindingWithoutDefaults(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	cfg := &config.Config{
		Workspace: config.WorkspaceConfig{Root: t.TempDir()},
		Blocks:    map[string]config.BlockBinding{"review": {Skills: []string{"coding-standards"}}},
	}
	if err := seedDefaultExtensions(ctx, cfg, st, nil, time.Now); err != nil {
		t.Fatalf("seedDefaultExtensions: %v", err)
	}
	if row, ok := platformBindings(t, st)["review"]; ok {
		t.Fatalf("default-owned review row kept: %+v", row)
	}
}

// An administrator's change to the block is never overwritten, even when the
// configuration file declares the same block.
func TestFileBlockKeepsAdministratorBinding(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	custom := []string{"mcp__codebase-memory__search_graph"}
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: store.ScopePlatform, Block: "review", MCPTools: custom, Skills: []string{"code-review"},
	}); err != nil {
		t.Fatal(err)
	}
	fileBlocks := map[string]config.BlockBinding{"review": {Skills: []string{"coding-standards"}}}
	applySkillDefaults(t, service, st, fileBlocks, defaultCodeReview)

	got, ok := platformBindings(t, st)["review"]
	if !ok || !reflect.DeepEqual(got.MCPTools, custom) {
		t.Fatalf("administrator review binding changed: %+v (present %v)", got, ok)
	}
}

// A failed ledger write must not leave a binding behind that a later start
// mistakes for an administrator's change.
func TestDefaultBindingAndLedgerAreAtomic(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "review.db")
	st, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	service, err := NewExtensionService(ExtensionServiceConfig{Store: st, Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER fail_review_snapshot BEFORE INSERT ON extension_defaults
		WHEN NEW.key = 'block:review' BEGIN SELECT RAISE(ABORT, 'injected ledger failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	defaults, err := service.SkillDefaults(defaultCodeReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyExtensionDefaults(ctx, st, nil, defaults, nil); err == nil {
		t.Fatal("ledger failure was swallowed")
	}
	if row, ok := platformBindings(t, st)["review"]; ok {
		t.Fatalf("review binding written without its ledger snapshot: %+v", row)
	}

	if _, err := raw.ExecContext(ctx, `DROP TRIGGER fail_review_snapshot`); err != nil {
		t.Fatal(err)
	}
	applySkillDefaults(t, service, st, nil, defaultCodeReview)
	if got := ledgerRows(t, st)["skill:code-review@review"]; got.Outcome != store.ExtensionDefaultApplied {
		t.Fatalf("review grant after retry = %+v, want applied", got)
	}
	if got := platformBindings(t, st)["review"].Skills; !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("review skills after retry = %v", got)
	}
}

// A built-in skill that cannot be refreshed, for example because
// workspace.root moved, is logged and does not stop the service.
func TestRuntimeStartsWhenBuiltinSkillRefreshFails(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	cfg := &config.Config{
		Workspace: config.WorkspaceConfig{Root: t.TempDir(), RunCleanupAfter: time.Hour},
		Storage: config.StorageConfig{
			LLMCallRetention: time.Hour, DeliveryRetention: time.Hour, JobRetention: time.Hour, ContextVersions: 1,
		},
		Engine:            config.EngineConfig{MaxConcurrentNodes: 1, DefaultNodeTimeout: time.Minute},
		Profiles:          testProfiles(),
		DefaultExtensions: config.DefaultExtensionsConfig{Skills: defaultCodeReview},
	}
	var logs bytes.Buffer
	deps := RuntimeDeps{
		Store: st, Cipher: cipher, GitLab: newRuntimeTestGitLab(), LLM: runtimeTestClients{}, Git: repoGit{git: &repo.Git{}},
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	first, err := NewRuntime(ctx, cfg, deps)
	if err != nil {
		t.Fatalf("first NewRuntime: %v", err)
	}
	_ = first.Close()

	cfg.Workspace.Root = t.TempDir()
	runtime, err := NewRuntime(ctx, cfg, deps)
	if err != nil {
		t.Fatalf("NewRuntime after workspace.root changed: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "built-in skill refresh failed") {
		t.Fatalf("refresh failure not logged as a warning:\n%s", logs.String())
	}
}
