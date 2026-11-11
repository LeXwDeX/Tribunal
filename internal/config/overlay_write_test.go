package config

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

const overlayTestBase = `
gitlab:
  url: "https://gitlab.example.com"
storage:
  driver: sqlite
  dsn: "file:data/review.db"
secrets:
  master_key_env: REVIEW_MASTER_KEY
auth:
  bootstrap_token_env: REVIEW_ADMIN_TOKEN
providers:
  primary:
    protocol: anthropic
    model: claude-sonnet
    api_key_env: PRIMARY_KEY
    max_tokens: 16000
  claude-sonnet:
    protocol: openai-chat
    model: claude-sonnet
    api_key_env: PRIMARY_KEY
`

const overlayTestDocument = `# console overlay
providers:
  primary:
    model: &model "claude-sonnet" # keep model comment
    base_url: ${CANARY_BASE_URL}
profiles:
  default:
    provider: *model
    lanes:
      review:
        provider: claude-sonnet

# trailing comment
`

type overlayHarness struct {
	t       *testing.T
	base    string
	overlay string
	writer  *OverlayWriter
}

func newOverlayHarness(t *testing.T, baseBody, overlayBody string) *overlayHarness {
	t.Helper()
	setRequiredEnv(t)
	t.Setenv("PRIMARY_KEY", "primary-key-value")
	t.Setenv("SECONDARY_KEY", "secondary-key-value")
	t.Setenv("CANARY_BASE_URL", "https://canary.example.com/expanded-secret")
	base := writeConfig(t, "server.yaml", baseBody)
	overlay := filepath.Join(t.TempDir(), "server.overlay.yaml")
	if overlayBody != "" {
		if err := os.WriteFile(overlay, []byte(overlayBody), 0o600); err != nil {
			t.Fatalf("write overlay: %v", err)
		}
	}
	writer, err := NewOverlayWriter(OverlayWriterOptions{
		BasePaths:   []string{base},
		OverlayPath: overlay,
	})
	if err != nil {
		t.Fatalf("NewOverlayWriter: %v", err)
	}
	return &overlayHarness{t: t, base: base, overlay: overlay, writer: writer}
}

// request builds a request whose revisions match the files on disk.
func (h *overlayHarness) request(changes ...OverlayChange) OverlayWriteRequest {
	h.t.Helper()
	snapshot, err := LoadSnapshot([]string{h.base}, h.overlay)
	if err != nil {
		h.t.Fatalf("LoadSnapshot() error: %v", err)
	}
	return OverlayWriteRequest{
		BaseRevision:    snapshot.BaseRevision,
		OverlayRevision: snapshot.OverlayRevision,
		Changes:         changes,
	}
}

func (h *overlayHarness) apply(req OverlayWriteRequest) (*OverlayWriteResult, error) {
	h.t.Helper()
	return h.writer.Apply(context.Background(), req)
}

func (h *overlayHarness) read() []byte {
	h.t.Helper()
	data, err := os.ReadFile(h.overlay)
	if err != nil {
		h.t.Fatalf("read overlay: %v", err)
	}
	return data
}

func (h *overlayHarness) readBase() []byte {
	h.t.Helper()
	data, err := os.ReadFile(h.base)
	if err != nil {
		h.t.Fatalf("read base: %v", err)
	}
	return data
}

func requireOverlayError(t *testing.T, err error, code string) *OverlayWriteError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error code %q, got nil", code)
	}
	var writeErr *OverlayWriteError
	if !errors.As(err, &writeErr) {
		t.Fatalf("error %v is not an OverlayWriteError", err)
	}
	if writeErr.Code != code {
		t.Fatalf("error code = %q (%v, details=%v), want %q", writeErr.Code, err, writeErr.Details, code)
	}
	return writeErr
}

func requireOverlayNoError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	var writeErr *OverlayWriteError
	if errors.As(err, &writeErr) {
		t.Fatalf("unexpected error %q: %s (details=%v)", writeErr.Code, writeErr.Message, writeErr.Details)
	}
	t.Fatalf("unexpected error: %v", err)
}

func requireNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestOverlayWriterPreservesCommentsAnchorsAndPlaceholders(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	baseBefore := harness.readBase()
	before := harness.read()

	result, err := harness.apply(harness.request(
		OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 4096},
	))
	if err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	if !result.Applied || !result.Changed {
		t.Fatalf("applied/changed = %v/%v", result.Applied, result.Changed)
	}
	if result.PreviousOverlayRevision == result.OverlayRevision {
		t.Fatal("overlay revision did not change")
	}

	after := string(harness.read())
	for _, want := range []string{
		"# console overlay", "# keep model comment", "# trailing comment",
		"&model", "*model", "${CANARY_BASE_URL}", "max_tokens: 4096",
	} {
		if !strings.Contains(after, want) {
			t.Errorf("overlay lost %q:\n%s", want, after)
		}
	}
	if strings.Contains(after, "expanded-secret") {
		t.Fatalf("overlay contains the expanded environment value:\n%s", after)
	}
	// Key order is preserved and new leaves are appended.
	modelAt := strings.Index(after, "model:")
	baseURLAt := strings.Index(after, "base_url:")
	maxTokensAt := strings.Index(after, "max_tokens:")
	if !(modelAt < baseURLAt && baseURLAt < maxTokensAt) {
		t.Fatalf("key order changed: model=%d base_url=%d max_tokens=%d\n%s", modelAt, baseURLAt, maxTokensAt, after)
	}
	if !bytes.Equal(baseBefore, harness.readBase()) {
		t.Fatal("base configuration was modified")
	}

	snapshot, err := LoadSnapshot([]string{harness.base}, harness.overlay)
	if err != nil {
		t.Fatalf("LoadSnapshot() after write: %v", err)
	}
	if snapshot.Config.Providers["primary"].MaxTokens != 4096 {
		t.Fatalf("max_tokens = %d, want 4096", snapshot.Config.Providers["primary"].MaxTokens)
	}
	if snapshot.OverlayRevision != result.OverlayRevision {
		t.Fatalf("revision = %q, want %q", snapshot.OverlayRevision, result.OverlayRevision)
	}
	requireNoTempFiles(t, filepath.Dir(harness.overlay))
	if len(before) == 0 {
		t.Fatal("test overlay is empty")
	}
}

func TestOverlayWriterRejectsSensitiveAndIgnoredPaths(t *testing.T) {
	paths := [][]string{
		{"providers"},
		{"providers", "primary", "price"},
		{"providers", "primary", "price", "input_per_mtok"},
		{"providers", "primary", "thinking"},
		{"providers", "primary", "unknown"},
		{"storage", "dsn"},
		{"secrets", "master_key_env"},
		{"auth", "bootstrap_token_env"},
		{"auth", "oauth", "client_id_env"},
		{"mcp", "config"},
		{"workspace", "root"},
		{"prompts_dir"},
		{"profiles", "default", "reasoner", "when"},
		{"profiles", "default", "consensus", "min_votes"},
		{"profiles", "default", "plan", "mode"},
		{"profiles", "default", "challenge", "fallback"},
		{"profiles", "default", "synthesize", "mode"},
		{"profiles", "default", "budget", "max_extends"},
		{"profiles", "default", "propose"},
		{"profiles", "onboarding", "provider"},
		{"unknown", "field"},
	}
	for _, path := range paths {
		t.Run(strings.Join(path, "."), func(t *testing.T) {
			harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
			before := harness.read()
			_, err := harness.apply(harness.request(OverlayChange{Path: path, Value: "x"}))
			requireOverlayError(t, err, OverlayErrorInvalidChange)
			if !bytes.Equal(before, harness.read()) {
				t.Fatal("overlay changed after a rejected path")
			}
		})
	}
}

func TestOverlayWriterRejectsInvalidValueTypes(t *testing.T) {
	tests := []struct {
		name  string
		path  []string
		value any
	}{
		{"model number", []string{"providers", "primary", "model"}, 7},
		{"max tokens string", []string{"providers", "primary", "max_tokens"}, "big"},
		{"bool string", []string{"providers", "primary", "strict_role_alternation"}, "yes"},
		{"temperature string", []string{"providers", "primary", "temperature"}, "hot"},
		{"null string", []string{"providers", "primary", "model"}, nil},
		{"api key env invalid", []string{"providers", "primary", "api_key_env"}, "BAD-NAME"},
		{"api key env placeholder", []string{"providers", "primary", "api_key_env"}, "${SECRET}"},
		{"ignore string", []string{"review_defaults", "ignore"}, "**/*.lock"},
		{"profile by label list", []string{"review_defaults", "profile_by_label"}, []any{"default"}},
		{"deadline invalid", []string{"profiles", "default", "budget", "deadline"}, "soon"},
		{"max files float", []string{"review_defaults", "max_files"}, 1.5},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
			before := harness.read()
			_, err := harness.apply(harness.request(OverlayChange{Path: test.path, Value: test.value}))
			requireOverlayError(t, err, OverlayErrorInvalidChange)
			if !bytes.Equal(before, harness.read()) {
				t.Fatal("overlay changed after a rejected value")
			}
		})
	}
}

func TestOverlayWriterRejectsDuplicateAndConflictingPaths(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	_, err := harness.apply(harness.request(
		OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 1},
		OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 2},
	))
	requireOverlayError(t, err, OverlayErrorInvalidChange)

	_, err = harness.apply(harness.request(
		OverlayChange{Path: []string{"providers", "primary"}, Delete: true},
		OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 2},
	))
	requireOverlayError(t, err, OverlayErrorInvalidChange)

	_, err = harness.apply(overlayHarnessRequest(harness, nil))
	requireOverlayError(t, err, OverlayErrorInvalidChange)
}

// overlayHarnessRequest returns a request with no changes.
func overlayHarnessRequest(harness *overlayHarness, changes []OverlayChange) OverlayWriteRequest {
	req := harness.request()
	req.Changes = changes
	return req
}

func TestOverlayWriterRejectsModifyingAliasedNode(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	before := harness.read()
	_, err := harness.apply(harness.request(
		OverlayChange{Path: []string{"providers", "primary", "model"}, Value: "claude-opus"},
	))
	writeErr := requireOverlayError(t, err, OverlayErrorInvalidChange)
	if !strings.Contains(writeErr.Error(), "anchor") && !strings.Contains(writeErr.Error(), "alias") {
		t.Fatalf("error does not explain the anchor conflict: %v", writeErr)
	}
	if !bytes.Equal(before, harness.read()) {
		t.Fatal("overlay changed after a rejected anchored edit")
	}
}

func TestOverlayWriterStaleRevision(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	valid := harness.request(OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 1})

	baseOnly := valid
	baseOnly.BaseRevision = "deadbeef"
	_, err := harness.apply(baseOnly)
	requireOverlayError(t, err, OverlayErrorStaleConfig)

	overlayOnly := valid
	overlayOnly.OverlayRevision = "deadbeef"
	_, err = harness.apply(overlayOnly)
	requireOverlayError(t, err, OverlayErrorStaleConfig)

	missing := valid
	missing.OverlayRevision = ""
	_, err = harness.apply(missing)
	requireOverlayError(t, err, OverlayErrorStaleConfig)

	// A missing overlay accepts an empty overlay revision.
	noOverlay := newOverlayHarness(t, overlayTestBase, "")
	req := noOverlay.request(OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 1})
	if req.OverlayRevision != "" {
		t.Fatalf("missing overlay revision = %q, want empty", req.OverlayRevision)
	}
	if _, err := noOverlay.apply(req); err != nil {
		t.Fatalf("apply on missing overlay: %v", err)
	}
	notMissing := req
	notMissing.OverlayRevision = "deadbeef"
	_, err = noOverlay.apply(notMissing)
	requireOverlayError(t, err, OverlayErrorStaleConfig)
}

func TestOverlayWriterCandidateValidationFailureLeavesFileUnchanged(t *testing.T) {
	tests := []struct {
		name   string
		change OverlayChange
	}{
		{"negative max tokens", OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: -1}},
		{"invalid protocol", OverlayChange{Path: []string{"providers", "primary", "protocol"}, Value: "grpc"}},
		{"max files zero", OverlayChange{Path: []string{"review_defaults", "max_files"}, Value: 0}},
		{"empty language", OverlayChange{Path: []string{"review_defaults", "language"}, Value: "  "}},
		{"invalid branch glob", OverlayChange{Path: []string{"review_defaults", "target_branches"}, Value: []any{"["}}},
		{"empty custom rule", OverlayChange{Path: []string{"review_defaults", "custom_rules"}, Value: []any{""}}},
		{"unknown profile label", OverlayChange{Path: []string{"review_defaults", "profile_by_label"}, Value: map[string]any{"bug": "removed"}}},
		{"missing provider", OverlayChange{Path: []string{"profiles", "default", "lanes", "review", "provider"}, Value: "missing"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
			before := harness.read()
			_, err := harness.apply(harness.request(test.change))
			writeErr := requireOverlayError(t, err, OverlayErrorInvalidConfig)
			if writeErr.Message == "" {
				t.Fatal("invalid_config error has no message")
			}
			if !bytes.Equal(before, harness.read()) {
				t.Fatal("overlay changed after a failed candidate validation")
			}
			requireNoTempFiles(t, filepath.Dir(harness.overlay))
		})
	}
}

func TestOverlayWriterConcurrentSameRevisionOnlyOneSucceeds(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	req := harness.request(OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 2048})

	const workers = 2
	results := make(chan error, workers)
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < workers; i++ {
		go func() {
			start.Wait()
			_, err := harness.writer.Apply(context.Background(), req)
			results <- err
		}()
	}
	start.Done()
	var succeeded, stale int
	for i := 0; i < workers; i++ {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		default:
			requireOverlayError(t, err, OverlayErrorStaleConfig)
			stale++
		}
	}
	if succeeded != 1 || stale != 1 {
		t.Fatalf("succeeded=%d stale=%d, want 1/1", succeeded, stale)
	}
}

func TestOverlayWriterDryRunDoesNotWrite(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	before := harness.read()
	req := harness.request(OverlayChange{Path: []string{"review_defaults", "ignore"}, Value: []any{"custom/**"}})
	req.DryRun = true
	result, err := harness.apply(req)
	requireOverlayNoError(t, err)
	if !result.DryRun || result.Applied || !result.Changed {
		t.Fatalf("dry run result = %+v", result)
	}
	if !bytes.Equal(before, harness.read()) {
		t.Fatal("dry run modified the overlay")
	}
	if result.Candidate == nil || result.Candidate.Config.ReviewDefaults.Ignore[0] != "custom/**" {
		t.Fatalf("dry run candidate = %+v", result.Candidate)
	}
	if result.OverlayRevision == req.OverlayRevision {
		t.Fatal("dry run did not report the proposed revision")
	}
}

func TestOverlayWriterReviewDefaultsIgnoreInheritanceAndReplacement(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	builtin := DefaultReviewIgnorePatterns()
	if len(builtin) != 6 {
		t.Fatalf("builtin ignore patterns = %v, want 6 entries", builtin)
	}

	result, err := harness.apply(harness.request(
		OverlayChange{Path: []string{"review_defaults", "ignore"}, Value: []any{"custom/**", "**/*.lock"}},
		OverlayChange{Path: []string{"review_defaults", "max_files"}, Value: 150},
	))
	requireOverlayNoError(t, err)
	if got := result.Candidate.Config.ReviewDefaults.Ignore; len(got) != 2 || got[0] != "custom/**" {
		t.Fatalf("effective ignore = %v", got)
	}
	if origin := result.Candidate.Origins["review_defaults.ignore"]; origin != OriginOverlay {
		t.Fatalf("ignore origin = %q, want overlay", origin)
	}
	rawIgnore, ok := result.OverlayReviewDefaults["ignore"].([]any)
	if !ok || len(rawIgnore) != 2 || rawIgnore[0] != "custom/**" {
		t.Fatalf("raw overlay ignore = %v", result.OverlayReviewDefaults)
	}
	if len(result.EffectiveIgnore) != 2 || result.EffectiveIgnore[0] != "custom/**" {
		t.Fatalf("effective ignore = %v", result.EffectiveIgnore)
	}

	// Deleting the overlay key falls back to the built-in list.
	result, err = harness.apply(harness.request(
		OverlayChange{Path: []string{"review_defaults", "ignore"}, Delete: true},
	))
	if err != nil {
		t.Fatalf("delete Apply() error: %v", err)
	}
	got := result.Candidate.Config.ReviewDefaults.Ignore
	if len(got) != len(builtin) {
		t.Fatalf("restored ignore = %v, want %v", got, builtin)
	}
	for i := range builtin {
		if got[i] != builtin[i] {
			t.Fatalf("restored ignore[%d] = %q, want %q", i, got[i], builtin[i])
		}
	}
	if origin := result.Candidate.Origins["review_defaults.ignore"]; origin != OriginDefault {
		t.Fatalf("restored ignore origin = %q, want default", origin)
	}
	if len(result.EffectiveIgnore) != len(builtin) || result.EffectiveIgnore[0] != builtin[0] {
		t.Fatalf("restored effective ignore = %v, want %v", result.EffectiveIgnore, builtin)
	}
}

func TestOverlayWriterRawReviewDefaultsKeepsPlaceholders(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	t.Setenv("CANARY_RULE", "expanded-rule-value")
	result, err := harness.apply(harness.request(
		OverlayChange{Path: []string{"review_defaults", "custom_rules"}, Value: []any{"${CANARY_RULE}"}},
	))
	requireOverlayNoError(t, err)
	rules, ok := result.OverlayReviewDefaults["custom_rules"].([]any)
	if !ok || len(rules) != 1 || rules[0] != "${CANARY_RULE}" {
		t.Fatalf("raw overlay custom_rules = %v", result.OverlayReviewDefaults)
	}
	if strings.Contains(string(harness.read()), "expanded-rule-value") {
		t.Fatalf("overlay contains the expanded value:\n%s", harness.read())
	}
}

func TestOverlayWriterDeleteProviderAndReferrers(t *testing.T) {
	const base = overlayTestBase + `profiles:
  default:
    lanes:
      review:
        provider: primary
`
	const overlay = `providers:
  extra:
    protocol: openai-chat
    model: gpt-test
    api_key_env: SECONDARY_KEY
profiles:
  default:
    lanes:
      review:
        provider: extra
`
	harness := newOverlayHarness(t, base, overlay)
	before := harness.read()

	_, err := harness.apply(harness.request(
		OverlayChange{Path: []string{"providers", "extra"}, Delete: true},
	))
	writeErr := requireOverlayError(t, err, OverlayErrorProviderInUse)
	if len(writeErr.Details) == 0 || !strings.Contains(strings.Join(writeErr.Details, ","), "profiles.default.lanes.review.provider") {
		t.Fatalf("referrer details = %v", writeErr.Details)
	}
	if !bytes.Equal(before, harness.read()) {
		t.Fatal("overlay changed after a referenced provider delete")
	}

	// Deleting the override and its only reference in one request succeeds.
	if _, err := harness.apply(harness.request(
		OverlayChange{Path: []string{"providers", "extra"}, Delete: true},
		OverlayChange{Path: []string{"profiles", "default", "lanes", "review", "provider"}, Delete: true},
	)); err != nil {
		t.Fatalf("delete provider and reference: %v", err)
	}
	if strings.Contains(string(harness.read()), "extra:") {
		t.Fatalf("provider extra still present:\n%s", harness.read())
	}

	// Deleting a provider the overlay does not define is a no-op and the base
	// provider stays effective.
	noop := newOverlayHarness(t, overlayTestBase, "review_defaults:\n  max_files: 100\n")
	result, err := noop.apply(noop.request(
		OverlayChange{Path: []string{"providers", "primary"}, Delete: true},
	))
	requireOverlayNoError(t, err)
	if !result.Applied || result.Changed {
		t.Fatalf("no-op result = %+v", result)
	}
}

func TestOverlayWriterCreatesProviderFromLeaves(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	changes := []OverlayChange{
		{Path: []string{"providers", "secondary", "protocol"}, Value: "openai-chat"},
		{Path: []string{"providers", "secondary", "model"}, Value: "gpt-test"},
		{Path: []string{"providers", "secondary", "api_key_env"}, Value: "SECONDARY_KEY"},
		{Path: []string{"providers", "secondary", "max_tokens"}, Value: 1000},
		{Path: []string{"providers", "secondary", "temperature"}, Value: 0.7},
		{Path: []string{"providers", "secondary", "thinking", "type"}, Value: "disabled"},
		{Path: []string{"providers", "secondary", "reasoning", "effort"}, Value: "high"},
		{Path: []string{"providers", "secondary", "strict_role_alternation"}, Value: true},
	}
	_, err := harness.apply(harness.request(changes...))
	requireOverlayNoError(t, err)
	snapshot, err := LoadSnapshot([]string{harness.base}, harness.overlay)
	if err != nil {
		t.Fatalf("LoadSnapshot() after write: %v", err)
	}
	provider, ok := snapshot.Config.Providers["secondary"]
	if !ok {
		t.Fatalf("provider secondary missing: %+v", snapshot.Config.Providers)
	}
	if provider.Protocol != "openai-chat" || provider.Model != "gpt-test" || provider.APIKeyEnv != "SECONDARY_KEY" ||
		provider.MaxTokens != 1000 || provider.Temperature == nil || *provider.Temperature != 0.7 ||
		provider.Thinking == nil || provider.Thinking.Type != "disabled" ||
		provider.Reasoning == nil || provider.Reasoning.Effort != "high" || !provider.StrictRoleAlternation {
		t.Fatalf("provider = %+v", provider)
	}
	// The environment variable name, not its value, is written.
	if strings.Contains(string(harness.read()), "secondary-key-value") {
		t.Fatalf("overlay contains the resolved key:\n%s", harness.read())
	}
}

func TestOverlayWriterReadOnlySymlinkAndOversized(t *testing.T) {
	t.Run("read-only directory", func(t *testing.T) {
		harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
		dir := filepath.Dir(harness.overlay)
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		before := harness.read()
		_, err := harness.apply(harness.request(
			OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 1},
		))
		requireOverlayError(t, err, OverlayErrorReadOnly)
		if !bytes.Equal(before, harness.read()) {
			t.Fatal("overlay changed in a read-only directory")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		base := writeConfig(t, "server.yaml", overlayTestBase)
		real := filepath.Join(dir, "real.yaml")
		if err := os.WriteFile(real, []byte(overlayTestDocument), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "server.overlay.yaml")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		writer, err := NewOverlayWriter(OverlayWriterOptions{BasePaths: []string{base}, OverlayPath: link})
		if err != nil {
			t.Fatalf("NewOverlayWriter: %v", err)
		}
		before, err := os.ReadFile(real)
		if err != nil {
			t.Fatal(err)
		}
		_, err = writer.Apply(context.Background(), OverlayWriteRequest{Changes: []OverlayChange{
			{Path: []string{"providers", "primary", "max_tokens"}, Value: 1},
		}})
		requireOverlayError(t, err, OverlayErrorSymlink)
		after, _ := os.ReadFile(real)
		if !bytes.Equal(before, after) {
			t.Fatal("symlink target changed")
		}
	})

	t.Run("oversized", func(t *testing.T) {
		harness := newOverlayHarness(t, overlayTestBase, "")
		if err := os.WriteFile(harness.overlay, []byte(strings.Repeat("a", MaxOverlayBytes+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := harness.apply(OverlayWriteRequest{Changes: []OverlayChange{
			{Path: []string{"providers", "primary", "max_tokens"}, Value: 1},
		}})
		requireOverlayError(t, err, OverlayErrorTooLarge)
	})

	t.Run("replacement exceeds the limit", func(t *testing.T) {
		padding := strings.Repeat("#", MaxOverlayBytes-len("providers:\n  primary:\n    max_tokens: 1\n")-16)
		body := "providers:\n  primary:\n    max_tokens: 1\n" + padding + "\n"
		harness := newOverlayHarness(t, overlayTestBase, body)
		before := harness.read()
		_, err := harness.apply(harness.request(
			OverlayChange{Path: []string{"providers", "primary", "model"}, Value: strings.Repeat("m", 256)},
		))
		requireOverlayError(t, err, OverlayErrorTooLarge)
		if !bytes.Equal(before, harness.read()) {
			t.Fatal("overlay changed after a rejected oversized replacement")
		}
	})
}

func TestOverlayWriterBackupFailureLeavesOriginal(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	before := harness.read()
	if err := os.Mkdir(harness.overlay+".bak", 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := harness.apply(harness.request(
		OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 1},
	))
	requireOverlayError(t, err, OverlayErrorWriteFailed)
	if !bytes.Equal(before, harness.read()) {
		t.Fatal("overlay changed after a backup failure")
	}
	requireNoTempFiles(t, filepath.Dir(harness.overlay))
}

func TestDefaultReviewIgnorePatternsMatchDefaults(t *testing.T) {
	root, err := parseYAML([]byte(defaultConfigYAML), "<defaults>")
	if err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	ignore := mappingValue(mappingValue(root, "review_defaults"), "ignore")
	if ignore == nil || ignore.Kind != yaml.SequenceNode {
		t.Fatalf("default review_defaults.ignore = %+v", ignore)
	}
	builtin := DefaultReviewIgnorePatterns()
	if len(ignore.Content) != len(builtin) {
		t.Fatalf("defaults have %d ignore patterns, helper has %d", len(ignore.Content), len(builtin))
	}
	for i, node := range ignore.Content {
		if node.Value != builtin[i] {
			t.Fatalf("ignore[%d] = %q, helper has %q", i, node.Value, builtin[i])
		}
	}
}

func TestOverlayWriterProfileLeafChanges(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	_, err := harness.apply(harness.request(
		OverlayChange{Path: []string{"profiles", "default", "provider"}, Value: "primary"},
		OverlayChange{Path: []string{"profiles", "default", "lanes", "review", "provider"}, Value: "claude-sonnet"},
		OverlayChange{Path: []string{"profiles", "default", "plan", "max_packages"}, Value: 2},
		OverlayChange{Path: []string{"profiles", "default", "budget", "deadline"}, Value: "45m"},
		OverlayChange{Path: []string{"review_defaults", "language"}, Value: "en-US"},
	))
	requireOverlayNoError(t, err)
	snapshot, err := LoadSnapshot([]string{harness.base}, harness.overlay)
	if err != nil {
		t.Fatalf("LoadSnapshot() after write: %v", err)
	}
	profile := snapshot.Config.Profiles["default"]
	if profile.Provider != "primary" || profile.Lanes == nil || profile.Lanes.Review.Provider != "claude-sonnet" {
		t.Fatalf("profile = %+v", profile)
	}
	if profile.Plan.MaxPackages != 2 || profile.Budget.Deadline != 45*time.Minute {
		t.Fatalf("profile plan/budget = %+v/%+v", profile.Plan, profile.Budget)
	}
	if snapshot.Config.ReviewDefaults.Language != "en-US" {
		t.Fatalf("language = %q", snapshot.Config.ReviewDefaults.Language)
	}
}

func TestOverlayWriterChangeIsReloadedByWatcher(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	initial, err := LoadSnapshot([]string{harness.base}, harness.overlay)
	if err != nil {
		t.Fatalf("initial LoadSnapshot() error: %v", err)
	}
	watcher := NewWatcher(WatcherOptions{
		BasePaths:   []string{harness.base},
		OverlayPath: harness.overlay,
		Initial:     initial,
	})
	_, err = harness.apply(harness.request(
		OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 4096},
	))
	requireOverlayNoError(t, err)
	watcher.poll()
	watcher.poll()
	if watcher.state.LastError != "" {
		t.Fatalf("watcher reload error: %s", watcher.state.LastError)
	}
	if got := watcher.state.Snapshot.Config.Providers["primary"].MaxTokens; got != 4096 {
		t.Fatalf("reloaded max_tokens = %d, want 4096", got)
	}
	if !watcher.state.Snapshot.OverlayPresent {
		t.Fatal("reloaded snapshot does not report the overlay as present")
	}
}

func TestOverlayFileErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{"permission", fs.ErrPermission, OverlayErrorReadOnly},
		{"read-only filesystem", syscall.EROFS, OverlayErrorReadOnly},
		{"busy", syscall.EBUSY, OverlayErrorReadOnly},
		{"no space", syscall.ENOSPC, OverlayErrorWriteFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writeErr := overlayFileError("write", test.err)
			if writeErr.Code != test.code {
				t.Fatalf("code = %q, want %q", writeErr.Code, test.code)
			}
		})
	}
}

func TestOverlayWriterNotConfigured(t *testing.T) {
	writer, err := NewOverlayWriter(OverlayWriterOptions{})
	if err != nil {
		t.Fatalf("NewOverlayWriter: %v", err)
	}
	_, err = writer.Apply(context.Background(), OverlayWriteRequest{Changes: []OverlayChange{
		{Path: []string{"providers", "primary", "max_tokens"}, Value: 1},
	}})
	requireOverlayError(t, err, OverlayErrorNotConfigured)
	if writer.Configured() {
		t.Fatal("writer without an overlay path reports configured")
	}
}

func TestOverlayWriterNoopDeleteDoesNotRewrite(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	before := harness.read()
	result, err := harness.apply(harness.request(
		OverlayChange{Path: []string{"providers", "missing"}, Delete: true},
	))
	requireOverlayNoError(t, err)
	if !result.Applied || result.Changed {
		t.Fatalf("no-op result = %+v", result)
	}
	if !bytes.Equal(before, harness.read()) {
		t.Fatal("no-op delete rewrote the overlay")
	}
}

func TestNewOverlayWriterRejectsBaseOverlayConflict(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "server.yaml")
	if err := os.WriteFile(base, []byte(overlayTestBase), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, overlay := range []string{base, filepath.Join(dir, ".", "server.yaml")} {
		_, err := NewOverlayWriter(OverlayWriterOptions{BasePaths: []string{base}, OverlayPath: overlay})
		requireOverlayError(t, err, OverlayErrorPathConflict)
	}

	link := filepath.Join(dir, "overlay-link.yaml")
	if err := os.Symlink(base, link); err != nil {
		t.Fatal(err)
	}
	_, err := NewOverlayWriter(OverlayWriterOptions{BasePaths: []string{base}, OverlayPath: link})
	requireOverlayError(t, err, OverlayErrorPathConflict)

	hard := filepath.Join(dir, "overlay-hard.yaml")
	if err := os.Link(base, hard); err != nil {
		t.Fatal(err)
	}
	_, err = NewOverlayWriter(OverlayWriterOptions{BasePaths: []string{base}, OverlayPath: hard})
	requireOverlayError(t, err, OverlayErrorPathConflict)

	distinct := filepath.Join(dir, "server.overlay.yaml")
	writer, err := NewOverlayWriter(OverlayWriterOptions{BasePaths: []string{base}, OverlayPath: distinct})
	requireOverlayNoError(t, err)
	if writer == nil || !writer.Configured() {
		t.Fatalf("writer = %+v", writer)
	}
}

func TestOverlayWriterPreservesCommentsOnEmptyOverlayRoot(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		wants []string
	}{
		{"comment only", "# header\n# second\n", []string{"# header", "# second"}},
		{"null root with header", "# header\n---\nnull\n", []string{"# header"}},
		{"null root with trailing comment", "# header\n---\nnull\n# trailer\n", []string{"# header", "# trailer"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newOverlayHarness(t, overlayTestBase, test.body)
			_, err := harness.apply(harness.request(
				OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 2048},
			))
			requireOverlayNoError(t, err)
			after := string(harness.read())
			for _, want := range test.wants {
				if !strings.Contains(after, want) {
					t.Fatalf("overlay lost %q:\n%s", want, after)
				}
			}
			snapshot, err := LoadSnapshot([]string{harness.base}, harness.overlay)
			if err != nil {
				t.Fatalf("LoadSnapshot() after write: %v", err)
			}
			if snapshot.Config.Providers["primary"].MaxTokens != 2048 {
				t.Fatalf("max_tokens = %d, want 2048", snapshot.Config.Providers["primary"].MaxTokens)
			}
		})
	}
}

func TestOverlayWriterRejectsSymlinkLock(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	target := filepath.Join(t.TempDir(), "lock-target")
	if err := os.WriteFile(target, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, harness.overlay+".lock"); err != nil {
		t.Fatal(err)
	}
	before := harness.read()
	_, err := harness.apply(harness.request(
		OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 1},
	))
	requireOverlayError(t, err, OverlayErrorSymlink)
	if !bytes.Equal(before, harness.read()) {
		t.Fatal("overlay changed with a symlink lock")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "do not touch" {
		t.Fatalf("symlink target changed: %q, %v", data, err)
	}
}

func TestOverlayWriterCleansStaleTempFiles(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	dir := filepath.Dir(harness.overlay)
	base := filepath.Base(harness.overlay)
	stale := []string{
		filepath.Join(dir, "."+base+".tmp-stale"),
		filepath.Join(dir, "."+base+".bak.tmp-stale"),
	}
	for _, path := range stale {
		if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(dir, ".other.tmp-stale")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := harness.apply(harness.request(
		OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 2048},
	))
	requireOverlayNoError(t, err)
	for _, path := range stale {
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stale temp file %s was not removed", filepath.Base(path))
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated temp file was removed: %v", err)
	}
}

func TestOverlayWriterBackupIsPrivate(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	before := harness.read()
	_, err := harness.apply(harness.request(
		OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 2048},
	))
	requireOverlayNoError(t, err)
	backup, err := os.ReadFile(harness.overlay + ".bak")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !bytes.Equal(before, backup) {
		t.Fatal("backup does not contain the pre-write overlay")
	}
	info, err := os.Stat(harness.overlay + ".bak")
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %o, want 600", info.Mode().Perm())
	}
}

func TestOverlayWriterRejectsReplacingAliasedContainer(t *testing.T) {
	const overlay = `review_defaults:
  profile_by_label:
    bug: &label default
  custom_rules: [*label]
`
	harness := newOverlayHarness(t, overlayTestBase, overlay)
	before := harness.read()
	_, err := harness.apply(harness.request(
		OverlayChange{Path: []string{"review_defaults", "profile_by_label"}, Value: map[string]any{"bug": "default"}},
	))
	requireOverlayError(t, err, OverlayErrorInvalidChange)
	if !bytes.Equal(before, harness.read()) {
		t.Fatal("overlay changed after replacing an aliased container")
	}
}
