package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const watcherBase = "gitlab:\n  url: \"https://gitlab.example.com\"\n"

type watcherHarness struct {
	watcher *Watcher
	states  chan State
	base    string
	overlay string
}

func newWatcherHarness(t *testing.T, baseBody string, overlayBody string) *watcherHarness {
	t.Helper()
	setRequiredEnv(t)
	base := writeConfig(t, "server.yaml", baseBody)
	overlay := filepath.Join(t.TempDir(), "server.overlay.yaml")
	if overlayBody != "" {
		if err := os.WriteFile(overlay, []byte(overlayBody), 0o600); err != nil {
			t.Fatalf("write overlay: %v", err)
		}
	}
	snapshot, err := LoadSnapshot([]string{base}, overlay)
	if err != nil {
		t.Fatalf("initial LoadSnapshot() error: %v", err)
	}
	states := make(chan State, 16)
	watcher := NewWatcher(WatcherOptions{
		BasePaths:   []string{base},
		OverlayPath: overlay,
		Interval:    time.Millisecond,
		Initial:     snapshot,
		OnState:     func(state State) { states <- state },
	})
	return &watcherHarness{watcher: watcher, states: states, base: base, overlay: overlay}
}

func (h *watcherHarness) rewrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("rewrite %s: %v", filepath.Base(path), err)
	}
}

func (h *watcherHarness) requireState(t *testing.T) State {
	t.Helper()
	select {
	case state := <-h.states:
		return state
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not publish a state")
		return State{}
	}
}

func (h *watcherHarness) requireNoState(t *testing.T) {
	t.Helper()
	select {
	case state := <-h.states:
		t.Fatalf("unexpected watcher state: %+v", state)
	default:
	}
}

func TestWatcherAppliesOnlyAfterTwoStableObservations(t *testing.T) {
	harness := newWatcherHarness(t, watcherBase+"server:\n  listen: \":8080\"\n", "")
	harness.watcher.poll()
	harness.requireNoState(t)

	harness.rewrite(t, harness.base, watcherBase+"server:\n  listen: \":9090\"\n")
	harness.watcher.poll()
	harness.requireNoState(t)
	harness.watcher.poll()
	state := harness.requireState(t)
	if got := state.Snapshot.Config.Server.Listen; got != ":9090" {
		t.Fatalf("applied listen = %q, want :9090 (last_error=%q)", got, state.LastError)
	}
	if state.LastError != "" {
		t.Fatalf("last error = %q", state.LastError)
	}
	if state.Snapshot.Origins["server.listen"] != OriginBase {
		t.Fatalf("origin = %q", state.Snapshot.Origins["server.listen"])
	}

	harness.watcher.poll()
	harness.requireNoState(t)
}

func TestWatcherKeepsLastGoodSnapshotOnInvalidConfig(t *testing.T) {
	harness := newWatcherHarness(t, watcherBase+"server:\n  listen: \":8080\"\n", "")
	harness.watcher.poll()

	harness.rewrite(t, harness.base, watcherBase+"server:\n  listen: [\n")
	harness.watcher.poll()
	harness.requireNoState(t)
	harness.watcher.poll()
	state := harness.requireState(t)
	if state.LastError == "" {
		t.Fatal("invalid config did not produce a last_error")
	}
	if got := state.Snapshot.Config.Server.Listen; got != ":8080" {
		t.Fatalf("last-good listen = %q, want :8080", got)
	}
	if state.LastErrorAt.IsZero() {
		t.Fatal("last_error_at must be set")
	}

	// A repeated failure with the same message is not republished.
	harness.watcher.poll()
	harness.requireNoState(t)

	harness.rewrite(t, harness.base, watcherBase+"server:\n  listen: \":9090\"\n")
	harness.watcher.poll()
	harness.watcher.poll()
	state = harness.requireState(t)
	if got := state.Snapshot.Config.Server.Listen; got != ":9090" {
		t.Fatalf("recovered listen = %q, want :9090", got)
	}
	if state.LastError != "" {
		t.Fatalf("last error was not cleared: %q", state.LastError)
	}
}

func TestWatcherOverlayChangesApply(t *testing.T) {
	harness := newWatcherHarness(t, watcherBase+"server:\n  listen: \":8080\"\n", "")
	if harness.watcher.state.Snapshot.OverlayPresent {
		t.Fatal("overlay must start absent")
	}
	harness.watcher.poll()
	harness.requireNoState(t)

	harness.rewrite(t, harness.overlay, "server:\n  write_timeout: 30s\n")
	harness.watcher.poll()
	harness.watcher.poll()
	state := harness.requireState(t)
	if !state.Snapshot.OverlayPresent {
		t.Fatal("created overlay was not reported present")
	}
	if got := state.Snapshot.Config.Server.WriteTimeout; got != 30*time.Second {
		t.Fatalf("write timeout = %v", got)
	}
	if state.Snapshot.Origins["server.write_timeout"] != OriginOverlay {
		t.Fatalf("origin = %q", state.Snapshot.Origins["server.write_timeout"])
	}
}

func TestWatcherLastErrorHidesPathsAndValues(t *testing.T) {
	harness := newWatcherHarness(t, watcherBase+"server:\n  listen: \":8080\"\n", "")
	harness.watcher.poll()

	const secret = "super-secret-value"
	harness.rewrite(t, harness.overlay, "server:\n  read_timeout: "+secret+"\n")
	harness.watcher.poll()
	harness.watcher.poll()
	state := harness.requireState(t)
	if !strings.Contains(state.LastError, "server.overlay.yaml") {
		t.Fatalf("last error does not name the source file: %q", state.LastError)
	}
	if strings.Contains(state.LastError, secret) {
		t.Fatalf("last error leaked a value: %q", state.LastError)
	}
	if strings.Contains(state.LastError, filepath.Dir(harness.overlay)) {
		t.Fatalf("last error leaked an absolute path: %q", state.LastError)
	}

	// A missing base file reports its base name without the directory.
	if err := os.Remove(harness.base); err != nil {
		t.Fatal(err)
	}
	harness.watcher.poll()
	harness.watcher.poll()
	state = harness.requireState(t)
	if !strings.Contains(state.LastError, "server.yaml") {
		t.Fatalf("missing base error does not name the file: %q", state.LastError)
	}
	if strings.Contains(state.LastError, filepath.Dir(harness.base)) {
		t.Fatalf("missing base error leaked an absolute path: %q", state.LastError)
	}
}

func TestWatcherIgnoresMetadataOnlyChanges(t *testing.T) {
	harness := newWatcherHarness(t, watcherBase+"server:\n  listen: \":8080\"\n", "")
	harness.watcher.poll()
	harness.requireNoState(t)

	stamp := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(harness.base, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	harness.watcher.poll()
	harness.watcher.poll()
	harness.requireNoState(t)
}

func TestWatcherStopsOnContextCancel(t *testing.T) {
	harness := newWatcherHarness(t, watcherBase+"server:\n  listen: \":8080\"\n", "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		harness.watcher.Run(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop after context cancellation")
	}
}

func TestWatcherLastErrorHidesEnvironmentValues(t *testing.T) {
	harness := newWatcherHarness(t, watcherBase+"server:\n  listen: \":8080\"\n", "")
	harness.watcher.poll()

	const secret = "super-secret-value"
	t.Setenv("CFG_WATCHER_DRIVER", secret)
	harness.rewrite(t, harness.base, watcherBase+"storage:\n  driver: ${CFG_WATCHER_DRIVER}\n")
	harness.watcher.poll()
	harness.watcher.poll()
	state := harness.requireState(t)
	if !strings.Contains(state.LastError, "storage.driver") {
		t.Fatalf("last error does not name the field: %q", state.LastError)
	}
	if strings.Contains(state.LastError, secret) {
		t.Fatalf("last error leaked an environment value: %q", state.LastError)
	}
}

func TestWatcherClearsErrorWhenLastGoodContentReturns(t *testing.T) {
	valid := watcherBase + "server:\n  listen: \":8080\"\n"
	harness := newWatcherHarness(t, valid, "")
	harness.watcher.poll()

	harness.rewrite(t, harness.base, "server:\n  listen: [\n")
	harness.watcher.poll()
	harness.watcher.poll()
	failed := harness.requireState(t)
	if failed.LastError == "" {
		t.Fatal("invalid config did not produce a last_error")
	}

	// Restoring the exact last-good contents does not replace the display
	// snapshot; the watcher must still clear the stale reload error.
	harness.rewrite(t, harness.base, valid)
	harness.watcher.poll()
	harness.watcher.poll()
	recovered := harness.requireState(t)
	if recovered.LastError != "" {
		t.Fatalf("last_error was not cleared: %q", recovered.LastError)
	}
	if !recovered.LastErrorAt.IsZero() {
		t.Fatalf("last_error_at was not cleared: %v", recovered.LastErrorAt)
	}
	if got := recovered.Snapshot.Config.Server.Listen; got != ":8080" {
		t.Fatalf("last-good listen = %q, want :8080", got)
	}
}

func TestWatcherClearsErrorWhenBadOverlayIsDeleted(t *testing.T) {
	harness := newWatcherHarness(t, watcherBase+"server:\n  listen: \":8080\"\n", "")
	harness.watcher.poll()

	harness.rewrite(t, harness.overlay, "server:\n  read_timeout: nope\n")
	harness.watcher.poll()
	harness.watcher.poll()
	failed := harness.requireState(t)
	if failed.LastError == "" {
		t.Fatal("invalid overlay did not produce a last_error")
	}

	if err := os.Remove(harness.overlay); err != nil {
		t.Fatal(err)
	}
	harness.watcher.poll()
	harness.watcher.poll()
	recovered := harness.requireState(t)
	if recovered.LastError != "" || !recovered.LastErrorAt.IsZero() {
		t.Fatalf("deleting the bad overlay did not clear the error: %q/%v", recovered.LastError, recovered.LastErrorAt)
	}
	if recovered.Snapshot.OverlayPresent {
		t.Fatal("deleted overlay is still reported as present")
	}
}

func TestWatcherLastErrorHidesShortEnvironmentValues(t *testing.T) {
	harness := newWatcherHarness(t, watcherBase+"server:\n  listen: \":8080\"\n", "")
	harness.watcher.poll()

	const secret = "mysql"
	t.Setenv("CFG_SHORT_DRIVER", secret)
	harness.rewrite(t, harness.base, watcherBase+"storage:\n  driver: ${CFG_SHORT_DRIVER}\n")
	harness.watcher.poll()
	harness.watcher.poll()
	state := harness.requireState(t)
	if !strings.Contains(state.LastError, "storage.driver") {
		t.Fatalf("last error does not name the field: %q", state.LastError)
	}
	if strings.Contains(state.LastError, secret) {
		t.Fatalf("last error leaked a short environment value: %q", state.LastError)
	}
}

func TestWatcherLastErrorHidesFileScalarValues(t *testing.T) {
	harness := newWatcherHarness(t, watcherBase+"server:\n  listen: \":8080\"\n", "")
	harness.watcher.poll()

	const secret = "hunter2-driver-value"
	harness.rewrite(t, harness.overlay, "storage:\n  driver: "+secret+"\n")
	harness.watcher.poll()
	harness.watcher.poll()
	state := harness.requireState(t)
	if !strings.Contains(state.LastError, "storage.driver") {
		t.Fatalf("last error does not name the field: %q", state.LastError)
	}
	if strings.Contains(state.LastError, secret) {
		t.Fatalf("last error leaked a source value: %q", state.LastError)
	}
}
