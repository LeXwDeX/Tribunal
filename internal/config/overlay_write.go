package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	// MaxOverlayBytes bounds the overlay file and any replacement. The console
	// only patches individual leaves, so a larger file is a deployment error.
	MaxOverlayBytes = 1 << 20

	// Stable error codes returned by the overlay writer. The HTTP layer maps
	// them to status codes without inspecting the underlying error.
	OverlayErrorNotConfigured = "overlay_not_configured"
	OverlayErrorReadOnly      = "config_read_only"
	OverlayErrorSymlink       = "config_symlink"
	OverlayErrorTooLarge      = "config_too_large"
	OverlayErrorStaleConfig   = "stale_config"
	OverlayErrorInvalidChange = "invalid_change"
	OverlayErrorInvalidConfig = "invalid_config"
	OverlayErrorProviderInUse = "provider_in_use"
	OverlayErrorWriteFailed   = "config_write_failed"
	OverlayErrorPathConflict  = "overlay_conflicts_with_base"
	OverlayErrorBusy          = "config_busy"
)

// OverlayWriteError is a classified overlay write failure. Details carry field
// paths or referrer names, never configuration values.
type OverlayWriteError struct {
	Code    string
	Message string
	Details []string
	Err     error
}

func (e *OverlayWriteError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil && !strings.Contains(e.Message, e.Err.Error()) {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *OverlayWriteError) Unwrap() error { return e.Err }

// OverlayChange replaces or deletes one allowlisted leaf. Value uses the types
// produced by encoding/json with UseNumber: nil, bool, string, json.Number,
// []any and map[string]any. Tests may also use native int and string slices.
type OverlayChange struct {
	Path   []string
	Value  any
	Delete bool
}

// OverlayWriteRequest is one atomic patch of the overlay file.
type OverlayWriteRequest struct {
	BaseRevision    string
	OverlayRevision string
	Changes         []OverlayChange
	DryRun          bool
	// CheckRemovedProviders, when set, receives the providers the patch
	// removes from the effective configuration before anything is written.
	// It returns the stored referrers it found; the writer aggregates them
	// with configuration profile references into one provider_in_use error.
	// Returning an error aborts the write and the error is returned unchanged.
	CheckRemovedProviders func(context.Context, []string) ([]string, error)
}

// OverlayWriteResult describes an applied or previewed patch.
type OverlayWriteResult struct {
	DryRun                  bool
	Applied                 bool
	Changed                 bool
	BaseRevision            string
	PreviousOverlayRevision string
	OverlayRevision         string
	// Candidate is the merged, validated configuration after the patch. It is
	// display data only; the running process keeps its startup configuration.
	Candidate *Snapshot
	// OverlayReviewDefaults is the patched overlay's review_defaults subtree
	// with ${ENV} placeholders unexpanded, or nil when the overlay does not set
	// it. It never contains expanded environment values.
	OverlayReviewDefaults map[string]any
	// EffectiveIgnore is the merged review_defaults.ignore list with
	// placeholders unexpanded, so the console can show inheritance without
	// reading environment values.
	EffectiveIgnore  []string
	RemovedProviders []string
}

// OverlayWriterOptions configures an OverlayWriter.
type OverlayWriterOptions struct {
	// BasePaths are the --config files in load order.
	BasePaths []string
	// OverlayPath is the --overlay file. Empty disables writes.
	OverlayPath string
}

// OverlayWriter serializes and atomically applies console patches to the
// overlay file. It never rewrites the base configuration files.
type OverlayWriter struct {
	basePaths   []string
	overlayPath string
	// sem serializes writes in this process. A channel is used instead of a
	// mutex so waiting for another write honours the request context.
	sem chan struct{}
}

// NewOverlayWriter builds a writer. The returned writer is safe for concurrent
// use; writes within one process are serialized by a semaphore and across
// processes by a sidecar flock. It refuses an overlay path that names one of
// the base configuration files, because writing it would rewrite a base file.
func NewOverlayWriter(opts OverlayWriterOptions) (*OverlayWriter, error) {
	if opts.OverlayPath != "" {
		for _, basePath := range opts.BasePaths {
			if sameConfigFile(basePath, opts.OverlayPath) {
				return nil, &OverlayWriteError{
					Code:    OverlayErrorPathConflict,
					Message: "configuration overlay must not be one of the base configuration files",
				}
			}
		}
	}
	return &OverlayWriter{
		basePaths:   append([]string(nil), opts.BasePaths...),
		overlayPath: opts.OverlayPath,
		sem:         make(chan struct{}, 1),
	}, nil
}

// sameConfigFile reports whether two paths name the same file. Canonical paths
// cover a missing overlay; Stat plus SameFile cover symlinks and hard links.
func sameConfigFile(left, right string) bool {
	if canonicalConfigPath(left) == canonicalConfigPath(right) {
		return true
	}
	leftInfo, err := os.Stat(left)
	if err != nil {
		return false
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		return false
	}
	return os.SameFile(leftInfo, rightInfo)
}

func canonicalConfigPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(absolute)); err == nil {
		return filepath.Join(dir, filepath.Base(absolute))
	}
	return filepath.Clean(absolute)
}

// Configured reports whether the writer has an overlay path.
func (w *OverlayWriter) Configured() bool {
	return w != nil && w.overlayPath != ""
}

// Apply validates, patches, validates again and optionally writes the overlay.
// The file is read again under the lock, and both revisions must match the
// request before anything is mutated.
func (w *OverlayWriter) Apply(ctx context.Context, req OverlayWriteRequest) (*OverlayWriteResult, error) {
	if !w.Configured() {
		return nil, &OverlayWriteError{
			Code:    OverlayErrorNotConfigured,
			Message: "configuration overlay is not configured",
		}
	}
	if err := validateOverlayRequest(req); err != nil {
		return nil, err
	}
	if err := probeOverlayPath(w.overlayPath); err != nil {
		return nil, err
	}

	select {
	case w.sem <- struct{}{}:
		defer func() { <-w.sem }()
	case <-ctx.Done():
		return nil, &OverlayWriteError{
			Code:    OverlayErrorBusy,
			Message: "timed out waiting for the configuration overlay writer",
			Err:     ctx.Err(),
		}
	}

	unlock, err := lockOverlayPath(ctx, w.overlayPath)
	if err != nil {
		return nil, err
	}
	defer unlock()
	cleanupOverlayTemps(w.overlayPath)

	current, err := w.readCurrent()
	if err != nil {
		return nil, err
	}
	if req.BaseRevision != current.baseRevision {
		return nil, &OverlayWriteError{
			Code:    OverlayErrorStaleConfig,
			Message: "the base configuration changed; reload and retry",
		}
	}
	if req.OverlayRevision != current.overlayRevision {
		return nil, &OverlayWriteError{
			Code:    OverlayErrorStaleConfig,
			Message: "the overlay changed; reload and retry",
		}
	}

	doc, err := overlayDocument(current.data, w.overlayPath)
	if err != nil {
		return nil, w.invalidConfig(err, nil, nil)
	}
	root := doc.Content[0]
	beforeRoot := cloneNode(root)
	modified := false
	for _, change := range req.Changes {
		changed, err := applyOverlayChange(doc, root, change)
		if err != nil {
			return nil, err
		}
		if changed {
			modified = true
		}
	}

	if modified {
		encoded, err := marshalYAMLDocument(doc)
		if err != nil {
			return nil, &OverlayWriteError{Code: OverlayErrorWriteFailed, Message: "cannot encode the configuration overlay", Err: err}
		}
		current.candidate = encoded
	}
	changed := current.candidate != nil && !bytes.Equal(current.candidate, current.data)
	if !modified {
		current.candidate = current.data
	}
	if len(current.candidate) > MaxOverlayBytes {
		return nil, &OverlayWriteError{Code: OverlayErrorTooLarge, Message: "configuration overlay would exceed the size limit"}
	}

	// Redaction context: the current files plus the candidate overlay. Errors
	// echoed by validation are sanitized against both. The base sources also
	// feed the raw (unexpanded) review_defaults view returned to the console.
	redaction, _ := LoadSnapshot(w.basePaths, w.overlayPath)
	var baseSources []Source
	sources := make([]Source, 0, 2)
	if redaction != nil {
		for _, source := range redaction.Sources {
			if source.Origin == OriginBase {
				baseSources = append(baseSources, source)
			}
		}
		sources = append(sources, redaction.Sources...)
	} else {
		// A broken overlay fails the full load; keep the base scalars and
		// environment names available for redaction anyway.
		if readSources, _, err := readBaseSources(w.basePaths); err == nil {
			baseSources = readSources
			sources = append(sources, readSources...)
		}
	}
	candidateRevision := ""
	if current.present || modified {
		candidateRevision = revisionOf(current.candidate)
	}
	candidateSource := Source{Origin: OriginOverlay, Path: w.overlayPath, Revision: candidateRevision, Root: root}
	if !modified && !current.present {
		candidateSource.Root = nil
		candidateSource.Missing = true
	}
	sources = append(sources, candidateSource)

	candidate, err := loadSnapshot(w.basePaths, &candidateSource)
	if err != nil {
		return nil, w.invalidConfig(err, nil, sources)
	}
	if candidate.BaseRevision != current.baseRevision {
		return nil, &OverlayWriteError{
			Code:    OverlayErrorStaleConfig,
			Message: "the base configuration changed during the write; retry",
		}
	}
	if details := validateReviewDefaultsPolicy(candidate.Config); len(details) > 0 {
		return nil, &OverlayWriteError{
			Code:    OverlayErrorInvalidConfig,
			Message: "candidate review_defaults is invalid",
			Details: details,
		}
	}

	// Compare the effective provider sets before and after the patch. Fall back
	// to raw merged names when the current configuration is too broken to load.
	beforeProviders := mergedProviderNames(baseSources, beforeRoot)
	if redaction != nil && redaction.Config != nil {
		beforeProviders = providerNameSet(redaction.Config.Providers)
	}
	afterProviders := mergedProviderNames(baseSources, root)
	if candidate.Config != nil {
		afterProviders = providerNameSet(candidate.Config.Providers)
	}
	removed := removedProviders(req.Changes, beforeProviders, afterProviders)
	if len(removed) > 0 {
		references := ProviderReferences(candidate.Config)
		var details []string
		for _, name := range removed {
			for _, ref := range references[name] {
				details = append(details, ref+" refers to providers."+name)
			}
		}
		// Configuration profiles and stored profile overrides are separate
		// referrer sets. Collect both before rejecting so a single 409 lists
		// every reference instead of forcing a second attempt.
		if req.CheckRemovedProviders != nil {
			stored, err := req.CheckRemovedProviders(ctx, removed)
			if err != nil {
				return nil, err
			}
			details = append(details, stored...)
		}
		if len(details) > 0 {
			return nil, &OverlayWriteError{
				Code:    OverlayErrorProviderInUse,
				Message: "the provider is still referenced",
				Details: details,
			}
		}
	}
	if err := candidate.Config.Validate(); err != nil {
		return nil, w.invalidConfig(err, candidate.Config, sources)
	}

	result := &OverlayWriteResult{
		DryRun:                  req.DryRun,
		Changed:                 changed,
		BaseRevision:            current.baseRevision,
		PreviousOverlayRevision: current.overlayRevision,
		OverlayRevision:         current.overlayRevision,
		Candidate:               candidate,
		OverlayReviewDefaults:   rawReviewDefaults(root),
		EffectiveIgnore:         rawEffectiveIgnore(baseSources, &candidateSource),
		RemovedProviders:        removed,
	}
	if changed {
		result.OverlayRevision = candidateRevision
	}
	if req.DryRun {
		return result, nil
	}
	result.Applied = true
	if !changed {
		return result, nil
	}

	if current.present {
		// The backup is not a configuration source; keep it private regardless
		// of the overlay's mode.
		if err := writeOverlayFile(w.overlayPath+".bak", current.data, 0o600); err != nil {
			return nil, overlayFileError("backup the configuration overlay", err)
		}
	}
	mode := fs.FileMode(0o600)
	if current.present {
		mode = current.mode
	}
	if err := writeOverlayFile(w.overlayPath, current.candidate, mode); err != nil {
		return nil, overlayFileError("write the configuration overlay", err)
	}
	return result, nil
}

type overlayCurrent struct {
	data            []byte
	present         bool
	mode            fs.FileMode
	baseRevision    string
	overlayRevision string
	// candidate is the encoded replacement, nil when no change was applied.
	candidate []byte
}

// readCurrent reads the overlay and base files under the writer lock.
func (w *OverlayWriter) readCurrent() (*overlayCurrent, error) {
	current := &overlayCurrent{}
	info, err := os.Lstat(w.overlayPath)
	switch {
	case err == nil:
		if err := checkOverlayInfo(info); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(w.overlayPath)
		if err != nil {
			return nil, overlayFileError("read the configuration overlay", err)
		}
		if len(data) > MaxOverlayBytes {
			return nil, &OverlayWriteError{Code: OverlayErrorTooLarge, Message: "configuration overlay exceeds the size limit"}
		}
		current.data = data
		current.present = true
		current.mode = info.Mode().Perm()
		current.overlayRevision = revisionOf(data)
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, overlayFileError("inspect the configuration overlay", err)
	}

	dir := filepath.Dir(w.overlayPath)
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return nil, overlayFileError("inspect the configuration overlay directory", err)
	}
	if !dirInfo.IsDir() {
		return nil, &OverlayWriteError{Code: OverlayErrorWriteFailed, Message: "configuration overlay directory is not a directory"}
	}
	if dirInfo.Mode().Perm()&0o222 == 0 {
		return nil, &OverlayWriteError{Code: OverlayErrorReadOnly, Message: "configuration overlay directory is read-only"}
	}

	baseRevision, err := baseRevisionOf(w.basePaths)
	if err != nil {
		return nil, err
	}
	current.baseRevision = baseRevision
	return current, nil
}

// probeOverlayPath rejects a symlink, a non-regular file, an oversized file or
// a read-only directory before a lock file is created next to the overlay.
func probeOverlayPath(overlayPath string) error {
	info, err := os.Lstat(overlayPath)
	switch {
	case err == nil:
		if err := checkOverlayInfo(info); err != nil {
			return err
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return overlayFileError("inspect the configuration overlay", err)
	}
	dir := filepath.Dir(overlayPath)
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return overlayFileError("inspect the configuration overlay directory", err)
	}
	if !dirInfo.IsDir() {
		return &OverlayWriteError{Code: OverlayErrorWriteFailed, Message: "configuration overlay directory is not a directory"}
	}
	if dirInfo.Mode().Perm()&0o222 == 0 {
		return &OverlayWriteError{Code: OverlayErrorReadOnly, Message: "configuration overlay directory is read-only"}
	}
	return nil
}

func checkOverlayInfo(info fs.FileInfo) error {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return &OverlayWriteError{Code: OverlayErrorSymlink, Message: "configuration overlay must not be a symlink"}
	case !info.Mode().IsRegular():
		return &OverlayWriteError{Code: OverlayErrorReadOnly, Message: "configuration overlay is not a regular file"}
	case info.Size() > MaxOverlayBytes:
		return &OverlayWriteError{Code: OverlayErrorTooLarge, Message: "configuration overlay exceeds the size limit"}
	case info.Mode().Perm()&0o222 == 0:
		return &OverlayWriteError{Code: OverlayErrorReadOnly, Message: "configuration overlay is read-only"}
	}
	return nil
}

func baseRevisionOf(paths []string) (string, error) {
	revisions := make([]string, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", overlayFileError("read the base configuration", err)
		}
		revisions = append(revisions, revisionOf(data))
	}
	return combineRevisions(revisions), nil
}

// lockOverlayPath takes the sidecar flock. Closing the file releases it. The
// lock file must be a regular file, never a symlink, and waiting honours the
// request context.
func lockOverlayPath(ctx context.Context, overlayPath string) (func(), error) {
	lockPath := overlayPath + ".lock"
	if info, err := os.Lstat(lockPath); err == nil {
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return nil, &OverlayWriteError{Code: OverlayErrorSymlink, Message: "configuration overlay lock must not be a symlink"}
		case !info.Mode().IsRegular():
			return nil, &OverlayWriteError{Code: OverlayErrorReadOnly, Message: "configuration overlay lock is not a regular file"}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, overlayFileError("inspect the configuration overlay lock", err)
	}
	file, err := openOverlayLock(lockPath)
	if err != nil {
		if overlayLockSymlinkError(err) {
			return nil, &OverlayWriteError{Code: OverlayErrorSymlink, Message: "configuration overlay lock must not be a symlink"}
		}
		return nil, overlayFileError("create the configuration overlay lock", err)
	}
	if err := lockOverlayFlock(ctx, file); err != nil {
		_ = file.Close()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, &OverlayWriteError{
				Code:    OverlayErrorBusy,
				Message: "timed out waiting for the configuration overlay lock",
				Err:     err,
			}
		}
		return nil, overlayFileError("lock the configuration overlay", err)
	}
	return func() { _ = file.Close() }, nil
}

// cleanupOverlayTemps removes temp files left by an interrupted writer. The
// caller holds the overlay lock, so a matching file is stale.
func cleanupOverlayTemps(overlayPath string) {
	dir := filepath.Dir(overlayPath)
	base := filepath.Base(overlayPath)
	for _, pattern := range []string{
		"." + base + ".tmp-*",
		"." + base + ".bak.tmp-*",
	} {
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			continue
		}
		for _, match := range matches {
			info, err := os.Lstat(match)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			_ = os.Remove(match)
		}
	}
}

func overlayFileError(action string, err error) *OverlayWriteError {
	if errors.Is(err, fs.ErrPermission) || overlayWriteUnavailable(err) {
		return &OverlayWriteError{Code: OverlayErrorReadOnly, Message: "configuration files are read-only", Err: err}
	}
	if errors.Is(err, fs.ErrNotExist) {
		return &OverlayWriteError{Code: OverlayErrorWriteFailed, Message: action + ": a configuration file or directory does not exist", Err: err}
	}
	return &OverlayWriteError{Code: OverlayErrorWriteFailed, Message: action + " failed", Err: err}
}

// writeOverlayFile writes data to a temporary file in the same directory,
// syncs it, renames it over the target and syncs the directory.
func writeOverlayFile(target string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(target)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, target); err != nil {
		return err
	}
	syncOverlayDir(dir)
	return nil
}

// syncOverlayDir makes the rename durable. The file contents are already
// synced; a directory sync failure cannot be rolled back, so it is best effort.
func syncOverlayDir(dir string) {
	file, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = file.Sync()
	_ = file.Close()
}

// overlayDocument parses the raw overlay and returns its document node so
// comments before and after the root mapping survive the round trip. A missing,
// empty, comment-only or null-root file becomes an empty mapping that keeps the
// original comment block.
func overlayDocument(data []byte, source string) (*yaml.Node, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return emptyOverlayDocument(nil, ""), nil
	}
	doc, err := parseYAMLDocument(data, source)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return emptyOverlayDocument(nil, leadingCommentBlock(data)), nil
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return emptyOverlayDocument(doc, leadingCommentBlock(data)), nil
	}
	return doc, nil
}

// emptyOverlayDocument builds a writable empty mapping. Comments on the parsed
// null document are carried over; a raw leading comment block covers
// comment-only files that the YAML decoder discards.
func emptyOverlayDocument(original *yaml.Node, rawPrefix string) *yaml.Node {
	mapping := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{mapping}}
	head, foot := rawPrefix, ""
	if original != nil {
		if len(original.Content) > 0 {
			root := original.Content[0]
			switch {
			case root.HeadComment != "":
				head = root.HeadComment
			case original.HeadComment != "":
				head = joinComment(head, original.HeadComment)
			}
			if root.LineComment != "" {
				head = joinComment(head, root.LineComment)
			}
			foot = root.FootComment
			if foot == "" {
				foot = original.FootComment
			}
		} else {
			head = joinComment(head, original.HeadComment)
			foot = original.FootComment
		}
	}
	head = strings.TrimRight(head, "\n")
	foot = strings.TrimRight(foot, "\n")
	if foot == head {
		// A comment block followed by an orphan "---" can end up as both the
		// document's footer and the raw leading block. Emit it once.
		foot = ""
	}
	mapping.HeadComment = head
	mapping.FootComment = foot
	return doc
}

func joinComment(left, right string) string {
	left = strings.TrimRight(left, "\n")
	right = strings.TrimSpace(right)
	switch {
	case left == "":
		return right
	case right == "":
		return left
	default:
		return left + "\n" + right
	}
}

// leadingCommentBlock returns the blank and comment lines before the first YAML
// token. comment-only documents have no nodes, so this text would otherwise be
// lost on the first patch.
func leadingCommentBlock(data []byte) string {
	lines := strings.Split(string(data), "\n")
	comments := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			if len(comments) > 0 {
				comments = append(comments, "")
			}
		case strings.HasPrefix(trimmed, "#"):
			comments = append(comments, strings.TrimRight(line, "\r"))
		default:
			for len(comments) > 0 && comments[len(comments)-1] == "" {
				comments = comments[:len(comments)-1]
			}
			return strings.Join(comments, "\n")
		}
	}
	for len(comments) > 0 && comments[len(comments)-1] == "" {
		comments = comments[:len(comments)-1]
	}
	return strings.Join(comments, "\n")
}

func marshalYAMLDocument(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type leafKind int

const (
	kindString leafKind = iota
	kindInt
	kindNumber
	kindBool
	kindStringList
	kindStringMap
	kindDuration
	kindEnvName
)

var providerLeafKinds = map[string]leafKind{
	"protocol":                    kindString,
	"base_url":                    kindString,
	"model":                       kindString,
	"max_tokens":                  kindInt,
	"context_window":              kindInt,
	"temperature":                 kindNumber,
	"max_concurrency":             kindInt,
	"rpm":                         kindInt,
	"forced_tool_choice":          kindString,
	"strict_role_alternation":     kindBool,
	"preserve_reasoning_content":  kindBool,
	"allow_reasoning_effort_none": kindBool,
	"output_config_effort":        kindBool,
	"responses_params":            kindString,
	"api_key_env":                 kindEnvName,
}

var profileLeafKinds = map[string]leafKind{
	"provider":                   kindString,
	"explore.provider":           kindString,
	"lanes.code_review.provider": kindString,
	"lanes.review.provider":      kindString,
	"lanes.reasoner.provider":    kindString,
	"lanes.validate.provider":    kindString,
	"plan.package_tokens":        kindInt,
	"plan.max_packages":          kindInt,
	"budget.max_nodes":           kindInt,
	"budget.max_tokens":          kindInt,
	"budget.max_turns":           kindInt,
	"budget.deadline":            kindDuration,
}

var reviewDefaultsLeafKinds = map[string]leafKind{
	"language":               kindString,
	"skip_draft":             kindBool,
	"target_branches":        kindStringList,
	"labels_include":         kindStringList,
	"labels_exclude":         kindStringList,
	"ignore":                 kindStringList,
	"max_files":              kindInt,
	"max_diff_lines":         kindInt,
	"commit_status":          kindBool,
	"post_failure_note":      kindBool,
	"show_model_attribution": kindBool,
	"allow_repo_config":      kindBool,
	"allow_repo_skills":      kindBool,
	"profile_by_label":       kindStringMap,
	"custom_rules":           kindStringList,
}

type overlayPathClass int

const (
	overlayPathUnknown overlayPathClass = iota
	overlayPathLeaf
	overlayPathSubtree
)

func classifyOverlayPath(segments []string) (leafKind, overlayPathClass) {
	switch {
	case len(segments) >= 2 && segments[0] == "providers":
		if !validProviderName(segments[1]) {
			return 0, overlayPathUnknown
		}
		switch len(segments) {
		case 2:
			return 0, overlayPathSubtree
		case 3:
			if kind, ok := providerLeafKinds[segments[2]]; ok {
				return kind, overlayPathLeaf
			}
			if segments[2] == "thinking" || segments[2] == "reasoning" {
				return 0, overlayPathSubtree
			}
		case 4:
			if segments[2] == "thinking" {
				switch segments[3] {
				case "budget_tokens":
					return kindInt, overlayPathLeaf
				case "type":
					return kindString, overlayPathLeaf
				}
			}
			if segments[2] == "reasoning" && segments[3] == "effort" {
				return kindString, overlayPathLeaf
			}
		}
	case len(segments) >= 2 && segments[0] == "profiles" && segments[1] == "default":
		if len(segments) == 2 {
			return 0, overlayPathSubtree
		}
		key := strings.Join(segments[2:], ".")
		if kind, ok := profileLeafKinds[key]; ok {
			return kind, overlayPathLeaf
		}
		if profilePathSubtree(key) {
			return 0, overlayPathSubtree
		}
	case len(segments) >= 1 && segments[0] == "review_defaults":
		if len(segments) == 1 {
			return 0, overlayPathSubtree
		}
		if len(segments) == 2 {
			if kind, ok := reviewDefaultsLeafKinds[segments[1]]; ok {
				return kind, overlayPathLeaf
			}
		}
	}
	return 0, overlayPathUnknown
}

func profilePathSubtree(key string) bool {
	for leaf := range profileLeafKinds {
		if strings.HasPrefix(leaf, key+".") {
			return true
		}
	}
	return false
}

func validProviderName(name string) bool {
	return name != "" && len(name) <= 128 && !strings.ContainsAny(name, ":\x00\n\r\t")
}

func validateOverlayRequest(req OverlayWriteRequest) error {
	if len(req.Changes) == 0 {
		return &OverlayWriteError{Code: OverlayErrorInvalidChange, Message: "at least one change is required"}
	}
	for i, change := range req.Changes {
		if err := validateOverlayChange(i, change); err != nil {
			return err
		}
	}
	for i := 0; i < len(req.Changes); i++ {
		for j := i + 1; j < len(req.Changes); j++ {
			left, right := req.Changes[i].Path, req.Changes[j].Path
			if pathPrefix(left, right) || pathPrefix(right, left) {
				return &OverlayWriteError{
					Code:    OverlayErrorInvalidChange,
					Message: fmt.Sprintf("changes[%d] and changes[%d] target the same or a parent path", i, j),
				}
			}
		}
	}
	return nil
}

func validateOverlayChange(index int, change OverlayChange) error {
	invalid := func(format string, args ...any) error {
		return &OverlayWriteError{
			Code:    OverlayErrorInvalidChange,
			Message: fmt.Sprintf("changes[%d]: ", index) + fmt.Sprintf(format, args...),
		}
	}
	if len(change.Path) == 0 || len(change.Path) > 8 {
		return invalid("path must have between 1 and 8 segments")
	}
	for _, segment := range change.Path {
		if segment == "" || len(segment) > 128 || strings.ContainsAny(segment, "\x00\n\r\t") {
			return invalid("path segments must be non-empty and free of control characters")
		}
	}
	if !change.Delete {
		for _, segment := range change.Path {
			if envRefRE.MatchString(segment) {
				// Do not quote the segment: it may be the rejected value.
				return invalid("path must not contain an ${ENV} placeholder")
			}
		}
	}
	kind, class := classifyOverlayPath(change.Path)
	joined := strings.Join(change.Path, ".")
	switch class {
	case overlayPathUnknown:
		return invalid("path %s is not writable", joined)
	case overlayPathSubtree:
		if !change.Delete {
			return invalid("path %s is not a settable leaf", joined)
		}
	case overlayPathLeaf:
		if !change.Delete {
			if err := validateOverlayValue(kind, change.Value); err != nil {
				return invalid("value for %s %s", joined, err)
			}
		}
	}
	return nil
}

func pathPrefix(left, right []string) bool {
	if len(left) > len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func validateOverlayValue(kind leafKind, value any) error {
	invalidType := func(want string) error { return fmt.Errorf("must be %s", want) }
	rejectPlaceholder := func(text string) error {
		if envRefRE.MatchString(text) {
			// Echoing the value would defeat the purpose of the check.
			return errors.New("must not contain an ${ENV} placeholder")
		}
		return nil
	}
	switch kind {
	case kindString:
		text, ok := value.(string)
		if !ok {
			return invalidType("a string")
		}
		if err := rejectPlaceholder(text); err != nil {
			return err
		}
	case kindEnvName:
		name, ok := value.(string)
		if !ok {
			return invalidType("an environment variable name")
		}
		if name == "" || !envNameRE.MatchString(name) {
			return invalidType("a valid environment variable name")
		}
	case kindInt:
		if _, ok := overlayInt64(value); !ok {
			return invalidType("an integer")
		}
	case kindNumber:
		if value != nil {
			if _, ok := overlayFloat64(value); !ok {
				return invalidType("a number or null")
			}
		}
	case kindBool:
		if _, ok := value.(bool); !ok {
			return invalidType("a boolean")
		}
	case kindStringList:
		list, ok := overlayStringList(value)
		if !ok {
			return invalidType("a list of strings")
		}
		for _, item := range list {
			if err := rejectPlaceholder(item); err != nil {
				return err
			}
		}
	case kindStringMap:
		object, ok := overlayStringMap(value)
		if !ok {
			return invalidType("a map of strings")
		}
		for key, item := range object {
			if err := rejectPlaceholder(key); err != nil {
				return err
			}
			if err := rejectPlaceholder(item); err != nil {
				return err
			}
		}
	case kindDuration:
		text, ok := value.(string)
		if !ok {
			return invalidType("a duration string")
		}
		if _, err := time.ParseDuration(text); err != nil {
			return invalidType("a duration string such as 30m")
		}
	}
	return nil
}

func overlayInt64(value any) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		if parsed, err := strconv.ParseInt(number.String(), 10, 64); err == nil {
			return parsed, true
		}
		parsed, err := strconv.ParseFloat(number.String(), 64)
		if err != nil || math.IsInf(parsed, 0) || parsed != math.Trunc(parsed) ||
			parsed < math.MinInt64 || parsed > math.MaxInt64 {
			return 0, false
		}
		return int64(parsed), true
	case int:
		return int64(number), true
	case int64:
		return number, true
	case float64:
		if math.IsInf(number, 0) || number != math.Trunc(number) ||
			number < math.MinInt64 || number > math.MaxInt64 {
			return 0, false
		}
		return int64(number), true
	}
	return 0, false
}

func overlayFloat64(value any) (float64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseFloat(number.String(), 64)
		return parsed, err == nil && !math.IsInf(parsed, 0) && !math.IsNaN(parsed)
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case float64:
		return number, !math.IsInf(number, 0) && !math.IsNaN(number)
	}
	return 0, false
}

func overlayStringList(value any) ([]string, bool) {
	switch list := value.(type) {
	case []string:
		return append([]string(nil), list...), true
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, text)
		}
		return out, true
	}
	return nil, false
}

func overlayStringMap(value any) (map[string]string, bool) {
	switch object := value.(type) {
	case map[string]string:
		out := make(map[string]string, len(object))
		for key, entry := range object {
			out[key] = entry
		}
		return out, true
	case map[string]any:
		out := make(map[string]string, len(object))
		for key, entry := range object {
			text, ok := entry.(string)
			if !ok {
				return nil, false
			}
			out[key] = text
		}
		return out, true
	}
	return nil, false
}

// applyOverlayChange patches one node. It reports whether the document changed
// and never writes expanded values: only the nodes named by the allowlist are
// replaced.
func applyOverlayChange(doc *yaml.Node, root *yaml.Node, change OverlayChange) (bool, error) {
	if change.Delete {
		return deleteOverlayPath(doc, root, change.Path)
	}
	return setOverlayPath(doc, root, change.Path, change.Value)
}

func setOverlayPath(doc, root *yaml.Node, segments []string, value any) (bool, error) {
	node := root
	for _, segment := range segments[:len(segments)-1] {
		if err := requireMutableMapping(doc, node); err != nil {
			return false, err
		}
		next := mappingValue(node, segment)
		if next == nil {
			child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			node.Content = append(node.Content, scalarNode("!!str", segment), child)
			node = child
			continue
		}
		if next.Kind == yaml.AliasNode {
			return false, overlayAliasError(segments)
		}
		if next.Kind != yaml.MappingNode {
			return false, &OverlayWriteError{
				Code:    OverlayErrorInvalidChange,
				Message: "path " + strings.Join(segments, ".") + " collides with a non-mapping value",
			}
		}
		node = next
	}
	if err := requireMutableMapping(doc, node); err != nil {
		return false, err
	}
	last := segments[len(segments)-1]
	index := mappingIndex(node, last)
	if index < 0 {
		valueNode, err := overlayValueNode(value)
		if err != nil {
			return false, &OverlayWriteError{Code: OverlayErrorInvalidChange, Message: err.Error()}
		}
		node.Content = append(node.Content, scalarNode("!!str", last), valueNode)
		return true, nil
	}
	old := node.Content[index+1]
	if old.Kind != yaml.AliasNode && (old.Anchor != "" || nodeIsAliased(doc, old)) {
		return false, overlayAliasError(segments)
	}
	if subtreeAliasEscape(doc, old) {
		return false, overlayAliasError(segments)
	}
	valueNode, err := overlayValueNode(value)
	if err != nil {
		return false, &OverlayWriteError{Code: OverlayErrorInvalidChange, Message: err.Error()}
	}
	valueNode.HeadComment = old.HeadComment
	valueNode.LineComment = old.LineComment
	valueNode.FootComment = old.FootComment
	if old.Kind == yaml.ScalarNode && old.Tag == "!!str" && valueNode.Kind == yaml.ScalarNode && valueNode.Tag == "!!str" {
		valueNode.Style = old.Style
	}
	node.Content[index+1] = valueNode
	return true, nil
}

func deleteOverlayPath(doc, root *yaml.Node, segments []string) (bool, error) {
	node := root
	for _, segment := range segments[:len(segments)-1] {
		if err := requireMutableMapping(doc, node); err != nil {
			return false, err
		}
		next := mappingValue(node, segment)
		if next == nil {
			return false, nil
		}
		if next.Kind == yaml.AliasNode {
			return false, overlayAliasError(segments)
		}
		if next.Kind != yaml.MappingNode {
			return false, nil
		}
		node = next
	}
	if err := requireMutableMapping(doc, node); err != nil {
		return false, err
	}
	index := mappingIndex(node, segments[len(segments)-1])
	if index < 0 {
		return false, nil
	}
	old := node.Content[index+1]
	if old.Kind != yaml.AliasNode && (old.Anchor != "" || nodeIsAliased(doc, old)) {
		return false, overlayAliasError(segments)
	}
	if subtreeAliasEscape(doc, old) {
		return false, overlayAliasError(segments)
	}
	node.Content = append(node.Content[:index], node.Content[index+2:]...)
	return true, nil
}

func overlayAliasError(segments []string) error {
	return &OverlayWriteError{
		Code:    OverlayErrorInvalidChange,
		Message: "path " + strings.Join(segments, ".") + " is anchored or referenced by a YAML alias",
	}
}

// requireMutableMapping rejects a mapping that an alias shares, because adding,
// removing or replacing a child would change every alias target.
func requireMutableMapping(doc, node *yaml.Node) error {
	if nodeIsAliased(doc, node) {
		return &OverlayWriteError{
			Code:    OverlayErrorInvalidChange,
			Message: "path traverses a YAML anchor that is referenced by an alias",
		}
	}
	return nil
}

func nodeIsAliased(doc, target *yaml.Node) bool {
	seen := make(map[*yaml.Node]bool)
	var walk func(*yaml.Node) bool
	walk = func(node *yaml.Node) bool {
		if node == nil || seen[node] {
			return false
		}
		seen[node] = true
		if node.Kind == yaml.AliasNode {
			return node.Alias == target
		}
		for _, child := range node.Content {
			if walk(child) {
				return true
			}
		}
		return false
	}
	return walk(doc)
}

// subtreeAliasEscape reports whether deleting node would detach an anchor that
// an alias outside the deleted subtree still references. The resulting document
// would contain a dangling alias.
func subtreeAliasEscape(doc, node *yaml.Node) bool {
	anchored := make(map[*yaml.Node]bool)
	var collect func(*yaml.Node)
	collect = func(current *yaml.Node) {
		if current == nil {
			return
		}
		if current.Anchor != "" {
			anchored[current] = true
		}
		for _, child := range current.Content {
			collect(child)
		}
	}
	collect(node)
	if len(anchored) == 0 {
		return false
	}
	escaped := false
	seen := make(map[*yaml.Node]bool)
	var walk func(current *yaml.Node, inside bool)
	walk = func(current *yaml.Node, inside bool) {
		if current == nil || escaped || seen[current] {
			return
		}
		seen[current] = true
		if current.Kind == yaml.AliasNode {
			if anchored[current.Alias] && !inside {
				escaped = true
			}
			return
		}
		if current == node {
			inside = true
		}
		for _, child := range current.Content {
			walk(child, inside)
		}
	}
	walk(doc, false)
	return escaped
}

func mappingIndex(node *yaml.Node, key string) int {
	if node == nil || node.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func scalarNode(tag, value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
}

func overlayValueNode(value any) (*yaml.Node, error) {
	switch typed := value.(type) {
	case nil:
		return scalarNode("!!null", "null"), nil
	case string:
		return scalarNode("!!str", typed), nil
	case bool:
		if typed {
			return scalarNode("!!bool", "true"), nil
		}
		return scalarNode("!!bool", "false"), nil
	case json.Number:
		if integer, ok := overlayInt64(typed); ok {
			return scalarNode("!!int", strconv.FormatInt(integer, 10)), nil
		}
		number, ok := overlayFloat64(typed)
		if !ok {
			return nil, fmt.Errorf("unsupported number %q", typed.String())
		}
		return scalarNode("!!float", strconv.FormatFloat(number, 'g', -1, 64)), nil
	case int:
		return scalarNode("!!int", strconv.FormatInt(int64(typed), 10)), nil
	case int64:
		return scalarNode("!!int", strconv.FormatInt(typed, 10)), nil
	case float64:
		if math.IsInf(typed, 0) || math.IsNaN(typed) {
			return nil, errors.New("unsupported number")
		}
		return scalarNode("!!float", strconv.FormatFloat(typed, 'g', -1, 64)), nil
	case []string:
		return sequenceNode(typed)
	case []any:
		items := make([]*yaml.Node, 0, len(typed))
		for _, item := range typed {
			node, err := overlayValueNode(item)
			if err != nil {
				return nil, err
			}
			items = append(items, node)
		}
		return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: items}, nil
	case map[string]string:
		return mappingNodeFromStrings(typed)
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, key := range keys {
			item, err := overlayValueNode(typed[key])
			if err != nil {
				return nil, err
			}
			node.Content = append(node.Content, scalarNode("!!str", key), item)
		}
		return node, nil
	}
	return nil, fmt.Errorf("unsupported value type %T", value)
}

func sequenceNode(values []string) (*yaml.Node, error) {
	items := make([]*yaml.Node, 0, len(values))
	for _, value := range values {
		items = append(items, scalarNode("!!str", value))
	}
	return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: items}, nil
}

func mappingNodeFromStrings(values map[string]string) (*yaml.Node, error) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, key := range keys {
		node.Content = append(node.Content, scalarNode("!!str", key), scalarNode("!!str", values[key]))
	}
	return node, nil
}

// validateReviewDefaultsPolicy applies the project settings rules that the
// platform config loader does not enforce. It returns field-level messages.
func validateReviewDefaultsPolicy(cfg *Config) []string {
	defaults := cfg.ReviewDefaults
	var details []string
	add := func(format string, args ...any) {
		details = append(details, fmt.Sprintf(format, args...))
	}
	if defaults.MaxFiles <= 0 {
		add("review_defaults.max_files must be positive")
	}
	if defaults.MaxDiffLines <= 0 {
		add("review_defaults.max_diff_lines must be positive")
	}
	if strings.TrimSpace(defaults.Language) == "" {
		add("review_defaults.language must not be empty")
	}
	for i, pattern := range defaults.TargetBranches {
		if strings.TrimSpace(pattern) == "" {
			add("review_defaults.target_branches[%d] must not be empty", i)
			continue
		}
		if _, err := path.Match(pattern, "probe"); err != nil {
			add("review_defaults.target_branches[%d] is not a valid glob", i)
		}
	}
	for i, value := range defaults.LabelsInclude {
		if strings.TrimSpace(value) == "" {
			add("review_defaults.labels_include[%d] must not be empty", i)
		}
	}
	for i, value := range defaults.LabelsExclude {
		if strings.TrimSpace(value) == "" {
			add("review_defaults.labels_exclude[%d] must not be empty", i)
		}
	}
	for i, value := range defaults.Ignore {
		if strings.TrimSpace(value) == "" {
			add("review_defaults.ignore[%d] must not be empty", i)
		}
	}
	for i, value := range defaults.CustomRules {
		if strings.TrimSpace(value) == "" {
			add("review_defaults.custom_rules[%d] must not be empty", i)
		}
	}
	labels := make([]string, 0, len(defaults.ProfileByLabel))
	for label := range defaults.ProfileByLabel {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		if strings.TrimSpace(label) == "" {
			add("review_defaults.profile_by_label must not contain an empty label")
			continue
		}
		profile := defaults.ProfileByLabel[label]
		if strings.TrimSpace(profile) == "" {
			add("review_defaults.profile_by_label.%s must not be empty", label)
			continue
		}
		if profile != ReviewProfile {
			add("review_defaults.profile_by_label.%s refers to unknown review profile %q", label, profile)
		}
	}
	return details
}

// removedProviders reports providers that existed before the patch and no
// longer exist after it. A delete of a provider that never existed is a no-op,
// not a removal, so it must not fail the referenced-provider check.
func removedProviders(changes []OverlayChange, before, after map[string]bool) []string {
	if before == nil || after == nil {
		return nil
	}
	seen := make(map[string]bool)
	var removed []string
	for _, change := range changes {
		if !change.Delete || len(change.Path) != 2 || change.Path[0] != "providers" {
			continue
		}
		name := change.Path[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if before[name] && !after[name] {
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)
	return removed
}

func providerNameSet(providers map[string]ProviderConfig) map[string]bool {
	names := make(map[string]bool, len(providers))
	for name := range providers {
		names[name] = true
	}
	return names
}

// mergedProviderNames returns the provider keys visible after merging the
// defaults, base files and one overlay root. It reads raw nodes, so it works
// even when the current configuration does not pass type validation.
func mergedProviderNames(baseSources []Source, overlayRoot *yaml.Node) map[string]bool {
	defaultRoot, err := parseYAML([]byte(defaultConfigYAML), "<defaults>")
	if err != nil {
		return nil
	}
	merged := cloneNode(defaultRoot)
	for _, source := range baseSources {
		if source.Root != nil {
			merged = mergeNode(merged, cloneNode(source.Root))
		}
	}
	if overlayRoot != nil {
		merged = mergeNode(merged, cloneNode(overlayRoot))
	}
	providers := mappingValue(merged, "providers")
	if providers == nil || providers.Kind != yaml.MappingNode {
		return map[string]bool{}
	}
	names := make(map[string]bool, len(providers.Content)/2)
	for i := 0; i+1 < len(providers.Content); i += 2 {
		names[providers.Content[i].Value] = true
	}
	return names
}

// rawReviewDefaults returns the overlay's review_defaults mapping with
// placeholders unexpanded. It is nil when the overlay does not set the key.
func rawReviewDefaults(root *yaml.Node) map[string]any {
	node := mappingValue(root, "review_defaults")
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	values := make(map[string]any, len(node.Content)/2)
	if err := node.Decode(&values); err != nil {
		return nil
	}
	return values
}

// rawEffectiveIgnore merges the raw layers and extracts
// review_defaults.ignore without expanding ${ENV}, so the console can show
// inheritance or replacement without turning the API into an environment
// variable read oracle.
func rawEffectiveIgnore(baseSources []Source, overlay *Source) []string {
	defaultRoot, err := parseYAML([]byte(defaultConfigYAML), "<defaults>")
	if err != nil {
		return nil
	}
	merged := cloneNode(defaultRoot)
	for _, source := range baseSources {
		if source.Root != nil {
			merged = mergeNode(merged, cloneNode(source.Root))
		}
	}
	if overlay != nil && overlay.Root != nil {
		merged = mergeNode(merged, cloneNode(overlay.Root))
	}
	ignore := mappingValue(mappingValue(merged, "review_defaults"), "ignore")
	if ignore != nil && ignore.Kind == yaml.AliasNode {
		ignore = ignore.Alias
	}
	if ignore == nil || ignore.Kind != yaml.SequenceNode {
		return nil
	}
	values := make([]string, 0, len(ignore.Content))
	for _, item := range ignore.Content {
		if item.Kind == yaml.AliasNode {
			item = item.Alias
		}
		if item == nil || item.Kind != yaml.ScalarNode || item.Tag != "!!str" {
			continue
		}
		values = append(values, item.Value)
	}
	return values
}

// ProviderReferences maps each provider name to the configuration paths that
// refer to it. Raw profiles are preferred so the paths name the fields that
// were written; onboarding has no raw copy and uses the loaded profile.
func ProviderReferences(cfg *Config) map[string][]string {
	references := make(map[string][]string)
	seen := make(map[string]bool)
	add := func(field, name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := field + "\x00" + name
		if seen[key] {
			return
		}
		seen[key] = true
		references[name] = append(references[name], field)
	}
	for _, name := range sortedKeys(cfg.Profiles) {
		profile := cfg.Profiles[name]
		if raw, ok := cfg.RawProfiles[name]; ok {
			profile = raw
		}
		collectProfileReferences("profiles."+name, profile, add)
	}
	for name := range references {
		sort.Strings(references[name])
	}
	return references
}

func collectProfileReferences(field string, profile ProfileConfig, add func(string, string)) {
	add(field+".provider", profile.Provider)
	if profile.Explore != nil {
		add(field+".explore.provider", profile.Explore.Provider)
	}
	if profile.Lanes != nil {
		add(field+".lanes.code_review.provider", profile.Lanes.CodeReview.Provider)
		add(field+".lanes.review.provider", profile.Lanes.Review.Provider)
		add(field+".lanes.reasoner.provider", profile.Lanes.Reasoner.Provider)
		add(field+".lanes.validate.provider", profile.Lanes.Validate.Provider)
	}
	add(field+".plan.provider", profile.Plan.Provider)
	for i, propose := range profile.Propose {
		add(fmt.Sprintf("%s.propose[%d].provider", field, i), propose.Provider)
	}
	if profile.Reasoner != nil {
		add(field+".reasoner.provider", profile.Reasoner.Provider)
	}
	add(field+".challenge.provider", profile.Challenge.Provider)
	add(field+".challenge.fallback", profile.Challenge.Fallback)
	add(field+".synthesize.provider", profile.Synthesize.Provider)
	if profile.Agent != nil {
		add(field+".agent.provider", profile.Agent.Provider)
	}
}

// invalidConfig sanitizes a load or validation error against the current files
// and the candidate overlay, then returns it with stable details.
func (w *OverlayWriter) invalidConfig(err error, candidate *Config, sources []Source) *OverlayWriteError {
	if candidate == nil {
		candidate = &Config{}
	}
	redaction := &Snapshot{Config: candidate, Sources: sources}
	paths := append(append([]string(nil), w.basePaths...), w.overlayPath)
	details := make([]string, 0, 4)
	seen := make(map[string]bool)
	for _, message := range joinedErrorMessages(err) {
		sanitized := sanitizeReloadError(errors.New(message), paths, redaction)
		if sanitized == "" || seen[sanitized] {
			continue
		}
		seen[sanitized] = true
		details = append(details, sanitized)
	}
	return &OverlayWriteError{
		Code:    OverlayErrorInvalidConfig,
		Message: "candidate configuration is invalid",
		Details: details,
	}
}

func joinedErrorMessages(err error) []string {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		items := joined.Unwrap()
		messages := make([]string, 0, len(items))
		for _, item := range items {
			messages = append(messages, item.Error())
		}
		return messages
	}
	return []string{err.Error()}
}
