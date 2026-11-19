package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// #588: the documented behavior is that a watcher reload logs a WARNING for
// newly normalized review_defaults values, not only the Info reload line.
func TestWatcherReloadLogsNewNormalizationWarnings(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	harness := newWatcherHarnessWithLogger(t, watcherBase+"server:\n  listen: \":8080\"\n", "", logger)
	harness.watcher.poll()

	harness.rewrite(t, harness.overlay, "review_defaults:\n  max_files: 300\n")
	harness.watcher.poll()
	harness.watcher.poll()
	state := harness.requireState(t)
	if got := state.Snapshot.Config.ReviewDefaults.MaxFiles; got != 200 {
		t.Fatalf("reloaded max_files = %d, want 200", got)
	}
	output := logs.String()
	if !strings.Contains(output, "level=WARN") || !strings.Contains(output, "review_defaults.max_files") {
		t.Fatalf("reload did not log the normalization warning: %q", output)
	}
}

// Warnings already present in the startup snapshot were logged by the startup
// path; a later reload must not repeat them as WARNING (#588).
func TestWatcherReloadDoesNotRepeatStartupWarnings(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	base := watcherBase + "review_defaults:\n  max_files: 300\n"
	harness := newWatcherHarnessWithLogger(t, base, "", logger)
	harness.watcher.poll()

	harness.rewrite(t, harness.overlay, "server:\n  write_timeout: 30s\n")
	harness.watcher.poll()
	harness.watcher.poll()
	state := harness.requireState(t)
	if !strings.Contains(strings.Join(state.Snapshot.Warnings, "\n"), "review_defaults.max_files") {
		t.Fatalf("reloaded snapshot lost the startup warning: %v", state.Snapshot.Warnings)
	}
	if output := logs.String(); strings.Contains(output, "level=WARN") {
		t.Fatalf("reload repeated a startup warning: %q", output)
	}
}