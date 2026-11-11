package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestLoadSnapshotReportsOrigins(t *testing.T) {
	base := writeConfig(t, "base.yaml", `
server:
  listen: ":9000"
gitlab:
  url: "https://gitlab.example.com"
review_defaults:
  ignore: ["base-only"]
providers:
  primary:
    protocol: anthropic
    model: claude
    api_key_env: PRIMARY_KEY
`)
	overlay := writeConfig(t, "server.overlay.yaml", `
server:
  write_timeout: 30s
providers:
  primary:
    model: claude-opus
`)

	snapshot, err := LoadSnapshot([]string{base}, overlay)
	if err != nil {
		t.Fatalf("LoadSnapshot() error: %v", err)
	}
	if snapshot.Config.Server.Listen != ":9000" || snapshot.Config.Server.WriteTimeout != 30_000_000_000 {
		t.Fatalf("merged server = %+v", snapshot.Config.Server)
	}
	tests := []struct {
		path string
		want Origin
	}{
		{"server.listen", OriginBase},
		{"server.read_timeout", OriginDefault},
		{"server.write_timeout", OriginOverlay},
		{"gitlab.url", OriginBase},
		{"gitlab.request_timeout", OriginDefault},
		{"providers.primary.protocol", OriginBase},
		{"providers.primary.model", OriginOverlay},
		{"providers.primary.api_key_env", OriginBase},
		{"providers.primary.max_concurrency", OriginDefault},
		{"review_defaults.ignore", OriginBase},
		{"review_defaults.ignore[0]", OriginBase},
		{"review_defaults.max_files", OriginDefault},
		{"profiles.default.plan.package_tokens", OriginDefault},
	}
	for _, test := range tests {
		if got, ok := snapshot.Origins[test.path]; !ok || got != test.want {
			t.Errorf("origin[%s] = %q (present=%v), want %q", test.path, got, ok, test.want)
		}
	}
	if !snapshot.OverlayPresent {
		t.Error("overlay should be reported as present")
	}
	if snapshot.BaseRevision == "" || snapshot.OverlayRevision == "" {
		t.Errorf("revisions = %q/%q, want both set", snapshot.BaseRevision, snapshot.OverlayRevision)
	}
	if snapshot.Config.Server.Listen != ":9000" {
		t.Errorf("LoadSnapshot changed the merged value: %q", snapshot.Config.Server.Listen)
	}
}

func TestLoadSnapshotKeepsUnexpandedSourceCopies(t *testing.T) {
	const url = "https://expanded.example.com"
	t.Setenv("CFG_SNAPSHOT_TEST_URL", url)
	base := writeConfig(t, "base.yaml", "gitlab:\n  url: ${CFG_SNAPSHOT_TEST_URL}\n")

	snapshot, err := LoadSnapshot([]string{base}, "")
	if err != nil {
		t.Fatalf("LoadSnapshot() error: %v", err)
	}
	if snapshot.Config.GitLab.URL != url {
		t.Fatalf("expanded gitlab.url = %q, want %q", snapshot.Config.GitLab.URL, url)
	}
	var source *Source
	for i := range snapshot.Sources {
		if snapshot.Sources[i].Origin == OriginBase {
			source = &snapshot.Sources[i]
			break
		}
	}
	if source == nil || source.Root == nil {
		t.Fatal("base source with its raw document is missing")
	}
	raw, err := yaml.Marshal(source.Root)
	if err != nil {
		t.Fatalf("marshal source: %v", err)
	}
	if !strings.Contains(string(raw), "${CFG_SNAPSHOT_TEST_URL}") {
		t.Fatalf("source document lost the ${ENV} reference: %s", raw)
	}

	// A second load must see the same raw document; in-place expansion would
	// have replaced the reference on the first load.
	again, err := LoadSnapshot([]string{base}, "")
	if err != nil {
		t.Fatalf("second LoadSnapshot() error: %v", err)
	}
	rawAgain, err := yaml.Marshal(again.Sources[0].Root)
	if err != nil {
		t.Fatalf("marshal second source: %v", err)
	}
	if string(raw) != string(rawAgain) {
		t.Fatalf("source document changed between loads:\n%s\n%s", raw, rawAgain)
	}
}

func TestLoadSnapshotMissingOverlayIsEmptyDocument(t *testing.T) {
	base := writeConfig(t, "base.yaml", "server:\n  listen: \":9000\"\n")
	overlay := filepath.Join(t.TempDir(), "server.overlay.yaml")

	withMissing, err := LoadSnapshot([]string{base}, overlay)
	if err != nil {
		t.Fatalf("LoadSnapshot(missing overlay) error: %v", err)
	}
	withoutOverlay, err := LoadSnapshot([]string{base}, "")
	if err != nil {
		t.Fatalf("LoadSnapshot(no overlay) error: %v", err)
	}
	if withMissing.OverlayPresent {
		t.Error("missing overlay must not be reported as present")
	}
	if withMissing.OverlayRevision != "" || withoutOverlay.OverlayRevision != "" {
		t.Errorf("overlay revisions = %q/%q, want empty", withMissing.OverlayRevision, withoutOverlay.OverlayRevision)
	}
	if withMissing.BaseRevision != withoutOverlay.BaseRevision {
		t.Errorf("base revision changed with a missing overlay: %q/%q", withMissing.BaseRevision, withoutOverlay.BaseRevision)
	}
	if withMissing.Config.Server.Listen != withoutOverlay.Config.Server.Listen {
		t.Errorf("missing overlay changed the config: %q/%q", withMissing.Config.Server.Listen, withoutOverlay.Config.Server.Listen)
	}
	if len(withMissing.Sources) != 2 || !withMissing.Sources[1].Missing {
		t.Fatalf("missing overlay source = %+v", withMissing.Sources)
	}
}

func TestLoadSnapshotExpandsEnvInTypedOverlayFields(t *testing.T) {
	t.Setenv("CFG_OVERLAY_TIMEOUT", "45s")
	base := writeConfig(t, "base.yaml", "server:\n  listen: \":9000\"\n")
	overlay := writeConfig(t, "server.overlay.yaml", "server:\n  read_timeout: ${CFG_OVERLAY_TIMEOUT}\n")

	snapshot, err := LoadSnapshot([]string{base}, overlay)
	if err != nil {
		t.Fatalf("LoadSnapshot() error: %v", err)
	}
	if snapshot.Config.Server.ReadTimeout != 45*time.Second {
		t.Fatalf("read_timeout = %v, want 45s", snapshot.Config.Server.ReadTimeout)
	}
	if snapshot.Origins["server.read_timeout"] != OriginOverlay {
		t.Fatalf("origin = %q", snapshot.Origins["server.read_timeout"])
	}
	var overlaySource *Source
	for i := range snapshot.Sources {
		if snapshot.Sources[i].Origin == OriginOverlay {
			overlaySource = &snapshot.Sources[i]
		}
	}
	if overlaySource == nil || overlaySource.Root == nil {
		t.Fatal("overlay source missing")
	}
	raw, err := yaml.Marshal(overlaySource.Root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "${CFG_OVERLAY_TIMEOUT}") {
		t.Fatalf("overlay source was expanded in place: %s", raw)
	}
}

func TestLoadSnapshotRejectsInvalidOverlayWithSourceName(t *testing.T) {
	base := writeConfig(t, "base.yaml", "server:\n  listen: \":9000\"\n")
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "invalid yaml", body: "server: [", want: "server.overlay.yaml"},
		{name: "unknown field", body: "server:\n  nope: 1\n", want: "field nope not found"},
		{name: "invalid type", body: "server:\n  read_timeout: nope\n", want: "cannot unmarshal"},
		{name: "duplicate key", body: "server:\n  listen: \":1\"\n  listen: \":2\"\n", want: "already defined"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			overlay := writeConfig(t, "server.overlay.yaml", test.body)
			_, err := LoadSnapshot([]string{base}, overlay)
			requireErrorContains(t, err, test.want, "server.overlay.yaml")
		})
	}
}

func TestLoadSnapshotDoesNotEchoOverlayValues(t *testing.T) {
	const secret = "super-secret-value"
	base := writeConfig(t, "base.yaml", "server:\n  listen: \":9000\"\n")
	overlay := writeConfig(t, "server.overlay.yaml", "server:\n  read_timeout: "+secret+"\n")

	_, err := LoadSnapshot([]string{base}, overlay)
	requireErrorContains(t, err, "cannot unmarshal", "<redacted>", "server.overlay.yaml")
	requireErrorNotContains(t, err, secret)
}

func TestLoadSnapshotWarningsAreCopied(t *testing.T) {
	path := writeConfig(t, "config.yaml", `
profiles:
  deep:
    plan:
      mode: code
`)
	snapshot, err := LoadSnapshot([]string{path}, "")
	if err != nil {
		t.Fatalf("LoadSnapshot() error: %v", err)
	}
	if len(snapshot.Warnings) != 1 || !strings.Contains(snapshot.Warnings[0], "profiles.deep was removed") {
		t.Fatalf("warnings = %q", snapshot.Warnings)
	}
	if len(snapshot.Config.Warnings) != len(snapshot.Warnings) {
		t.Fatalf("config warnings = %q, snapshot warnings = %q", snapshot.Config.Warnings, snapshot.Warnings)
	}
}

func TestLoadSnapshotRevisionTracksContent(t *testing.T) {
	path := writeConfig(t, "config.yaml", "server:\n  listen: \":9000\"\n")
	first, err := LoadSnapshot([]string{path}, "")
	if err != nil {
		t.Fatalf("LoadSnapshot() error: %v", err)
	}
	second, err := LoadSnapshot([]string{path}, "")
	if err != nil {
		t.Fatalf("second LoadSnapshot() error: %v", err)
	}
	if first.BaseRevision == "" || first.BaseRevision != second.BaseRevision {
		t.Fatalf("base revision is not stable: %q/%q", first.BaseRevision, second.BaseRevision)
	}
	if err := os.WriteFile(path, []byte("server:\n  listen: \":9001\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := LoadSnapshot([]string{path}, "")
	if err != nil {
		t.Fatalf("third LoadSnapshot() error: %v", err)
	}
	if third.BaseRevision == first.BaseRevision {
		t.Fatalf("base revision did not change with content: %q", third.BaseRevision)
	}
}

func TestEqualRuntimeIgnoresDisplayWarnings(t *testing.T) {
	left := loadConfig(t)
	right := loadConfig(t)
	if !EqualRuntime(left, right) {
		t.Fatal("identical loads must be runtime-equal")
	}
	right.Warnings = append(right.Warnings, "display only")
	if !EqualRuntime(left, right) {
		t.Fatal("display-only warnings must not require a restart")
	}
	right.Server.Listen = ":9999"
	if EqualRuntime(left, right) {
		t.Fatal("a changed effective value must not be runtime-equal")
	}
}
