package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// DefaultWatchInterval is the production polling interval for configuration
// file changes. A change is applied after two consecutive identical polls.
const DefaultWatchInterval = 2 * time.Second

// WatcherOptions configures the polling configuration watcher.
type WatcherOptions struct {
	// BasePaths are the --config files in load order.
	BasePaths []string
	// OverlayPath is the --overlay file. Empty disables the overlay; a missing
	// file is an empty overlay.
	OverlayPath string
	// Interval defaults to DefaultWatchInterval.
	Interval time.Duration
	// Initial is the snapshot loaded at startup.
	Initial *Snapshot
	Logger  *slog.Logger
	// OnState receives the display state after every applied reload or new
	// reload error. It is called from the watcher goroutine.
	OnState func(State)
	// Now is injectable for tests.
	Now func() time.Time
}

// Watcher polls the configured files, reloads the merged snapshot when their
// contents stabilize, and keeps the last-good snapshot when a reload fails.
// It never stops the server or closes listeners on a configuration error.
type Watcher struct {
	opts WatcherOptions
	now  func() time.Time

	state      State
	applied    observation
	appliedSet bool
	pending    observation
	stable     int

	published       bool
	lastPublished   State
	lastLoggedError string
}

// NewWatcher builds a watcher that starts from opts.Initial. It observes the
// files once so an unchanged startup does not trigger an immediate reload.
func NewWatcher(opts WatcherOptions) *Watcher {
	if opts.Interval <= 0 {
		opts.Interval = DefaultWatchInterval
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	watcher := &Watcher{opts: opts, now: now}
	if opts.Initial != nil {
		watcher.state = State{Snapshot: opts.Initial, UpdatedAt: now()}
	}
	observed := observeFiles(opts.BasePaths, opts.OverlayPath)
	if opts.Initial != nil &&
		observed.baseRevision == opts.Initial.BaseRevision &&
		observed.overlayRevision == opts.Initial.OverlayRevision {
		watcher.applied = observed
		watcher.appliedSet = true
	}
	return watcher
}

// Run polls until ctx is canceled. It never returns a configuration error:
// failures are published through OnState and the last-good snapshot stays in
// place.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.poll()
		}
	}
}

type observation struct {
	// key covers every file's size, mtime, and content hash so a metadata-only
	// touch still passes through the two-poll stability gate.
	key string
	// baseRevision and overlayRevision match Snapshot revisions and are used
	// to reject a load that raced with another file write.
	baseRevision    string
	overlayRevision string
}

func (w *Watcher) poll() {
	observed := observeFiles(w.opts.BasePaths, w.opts.OverlayPath)
	if w.appliedSet && observed.key == w.applied.key {
		w.clearError()
		w.pending = observation{}
		w.stable = 0
		return
	}
	if observed.key == w.pending.key {
		w.stable++
	} else {
		w.pending = observed
		w.stable = 1
	}
	if w.stable < 2 {
		return
	}
	if w.appliedSet &&
		observed.baseRevision == w.applied.baseRevision &&
		observed.overlayRevision == w.applied.overlayRevision {
		// The file was touched but its effective contents did not change.
		// Returning to the last-good contents also recovers a failed reload.
		w.applied = observed
		w.pending = observation{}
		w.stable = 0
		w.clearError()
		return
	}

	snapshot, err := LoadSnapshot(w.opts.BasePaths, w.opts.OverlayPath)
	if err == nil {
		err = snapshot.Config.Validate()
	}
	if err != nil {
		w.fail(err, snapshot)
		return
	}
	if snapshot.BaseRevision != observed.baseRevision || snapshot.OverlayRevision != observed.overlayRevision {
		// The file changed between hashing and loading. Wait for the next
		// stable observation instead of publishing mixed content.
		w.pending = observation{}
		w.stable = 0
		return
	}

	w.applied = observed
	w.appliedSet = true
	w.pending = observation{}
	w.stable = 0
	w.succeed(snapshot)
}

func (w *Watcher) succeed(snapshot *Snapshot) {
	w.state.Snapshot = snapshot
	w.state.LastError = ""
	w.state.LastErrorAt = time.Time{}
	w.state.UpdatedAt = w.now()
	w.lastLoggedError = ""
	if w.opts.Logger != nil {
		w.opts.Logger.Info("configuration files changed; display snapshot reloaded",
			"base_revision", shortRevision(snapshot.BaseRevision),
			"overlay_present", snapshot.OverlayPresent)
	}
	w.publish()
}

// clearError publishes recovery when the files return to the last-good
// contents without replacing the display snapshot.
func (w *Watcher) clearError() {
	if w.state.LastError == "" {
		return
	}
	w.state.LastError = ""
	w.state.LastErrorAt = time.Time{}
	w.lastLoggedError = ""
	if w.opts.Logger != nil {
		w.opts.Logger.Info("configuration files returned to the last-good contents; reload error cleared")
	}
	w.publish()
}

func (w *Watcher) fail(err error, snapshot *Snapshot) {
	message := sanitizeReloadError(err, append(append([]string(nil), w.opts.BasePaths...), w.opts.OverlayPath), snapshot)
	if message == w.state.LastError {
		return
	}
	w.state.LastError = message
	w.state.LastErrorAt = w.now()
	if w.opts.Logger != nil && message != w.lastLoggedError {
		w.opts.Logger.Warn("configuration reload failed; keeping the last-good display snapshot", "detail", message)
		w.lastLoggedError = message
	}
	w.publish()
}

func (w *Watcher) publish() {
	if w.opts.OnState == nil {
		return
	}
	if w.published && statesEqual(w.lastPublished, w.state) {
		return
	}
	w.lastPublished = w.state
	w.published = true
	w.opts.OnState(w.state)
}

func statesEqual(left, right State) bool {
	return left.Snapshot == right.Snapshot &&
		left.LastError == right.LastError &&
		left.LastErrorAt.Equal(right.LastErrorAt) &&
		left.UpdatedAt.Equal(right.UpdatedAt)
}

// observeFiles reads every configured file and builds a stable observation key.
func observeFiles(basePaths []string, overlayPath string) observation {
	var key strings.Builder
	hashes := make([]string, 0, len(basePaths))
	baseOK := true
	for _, path := range basePaths {
		fileKey, hash, ok := observeFile(path)
		fmt.Fprintf(&key, "base:%s:%s;", path, fileKey)
		if ok {
			hashes = append(hashes, hash)
		} else {
			baseOK = false
		}
	}
	baseRevision := ""
	if baseOK {
		baseRevision = combineRevisions(hashes)
	}
	overlayRevision := ""
	if overlayPath != "" {
		fileKey, hash, ok := observeFile(overlayPath)
		fmt.Fprintf(&key, "overlay:%s:%s;", overlayPath, fileKey)
		if ok {
			overlayRevision = hash
		}
	}
	return observation{key: key.String(), baseRevision: baseRevision, overlayRevision: overlayRevision}
}

// observeFile returns a metadata plus content key. A missing or unreadable file
// returns ok=false and still produces a key that changes when it reappears.
func observeFile(path string) (string, string, bool) {
	info, statErr := os.Stat(path)
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if statErr == nil {
			return fmt.Sprintf("unreadable:%d:%d", info.Size(), info.ModTime().UnixNano()), "", false
		}
		return "unreadable", "", false
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	modTime := int64(0)
	if statErr == nil {
		modTime = info.ModTime().UnixNano()
	}
	return fmt.Sprintf("ok:%d:%d:%s", len(data), modTime, hash), hash, true
}

// sanitizeReloadError keeps the last_error useful without leaking absolute
// paths, configured path values, or environment values into the API or logs.
func sanitizeReloadError(err error, paths []string, snapshot *Snapshot) string {
	message := err.Error()
	for _, path := range paths {
		if path == "" {
			continue
		}
		message = strings.ReplaceAll(message, path, filepath.Base(path))
	}
	var values []string
	if snapshot != nil {
		values = append(values, configInternalPaths(snapshot.Config)...)
		values = append(values, sourceEnvValues(snapshot.Sources)...)
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, value := range values {
		// Only redact values that can look like a path or contain a
		// credential; short plain values such as the default git binary
		// would corrupt unrelated field names in the message.
		if value == "" || (!strings.ContainsAny(value, `/\:`) && len(value) < 8) {
			continue
		}
		message = strings.ReplaceAll(message, value, "<redacted>")
	}
	return redactYAMLValues(message)
}

// sourceEnvValues collects the values of environment variables referenced by
// the raw file documents. Validation errors may echo a field value, so a value
// that came from ${ENV} must not reach last_error.
func sourceEnvValues(sources []Source) []string {
	names := make(map[string]bool)
	for _, source := range sources {
		collectEnvNames(source.Root, names)
	}
	values := make([]string, 0, len(names))
	for name := range names {
		if value, ok := envValue(name); ok && value != "" {
			values = append(values, value)
		}
	}
	return values
}

func collectEnvNames(node *yaml.Node, names map[string]bool) {
	if node == nil {
		return
	}
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		for _, match := range envRefRE.FindAllStringSubmatch(node.Value, -1) {
			if len(match) > 1 && envNameRE.MatchString(match[1]) {
				names[match[1]] = true
			}
		}
	}
	for _, child := range node.Content {
		collectEnvNames(child, names)
	}
}

// configInternalPaths lists path-valued settings that must not appear in a
// sanitized reload error.
func configInternalPaths(cfg *Config) []string {
	values := []string{
		cfg.Storage.DSN,
		cfg.Workspace.Root,
		cfg.Workspace.GitBin,
		cfg.MCP.Config,
		cfg.PromptsDir,
	}
	values = append(values, cfg.Skills.Dirs...)
	return values
}

func shortRevision(revision string) string {
	if len(revision) <= 12 {
		return revision
	}
	return revision[:12]
}
