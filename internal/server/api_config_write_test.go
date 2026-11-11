package server

import (
	"context"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
)

const (
	overlayAPIBase = `
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

	overlayAPIDocument = `# console overlay
providers:
  primary:
    model: "claude-sonnet" # model comment
    base_url: ${CANARY_BASE_URL}
  extra:
    protocol: openai-chat
    model: gpt-test
    api_key_env: SECONDARY_KEY
profiles:
  default:
    lanes:
      review:
        provider: claude-sonnet
`
)

type overlayAPIHarness struct {
	t       *testing.T
	env     *m6Env
	base    string
	overlay string
	logs    *lockedBuffer
}

func newOverlayAPIHarness(t *testing.T, baseBody, overlayBody string) *overlayAPIHarness {
	t.Helper()
	t.Setenv("REVIEW_MASTER_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	t.Setenv("REVIEW_ADMIN_TOKEN", "abcdefghijklmnopqrstuvwxyz012345")
	t.Setenv("PRIMARY_KEY", "primary-key-value")
	t.Setenv("SECONDARY_KEY", "secondary-key-value")
	t.Setenv("CANARY_BASE_URL", "https://canary.example.com/expanded-secret")
	t.Setenv("CANARY_PROTOCOL", "expanded-secret-protocol")
	t.Setenv("CANARY_SECRET", "expanded-canary-value")

	dir := t.TempDir()
	base := filepath.Join(dir, "server.yaml")
	overlay := filepath.Join(dir, "server.overlay.yaml")
	if err := os.WriteFile(base, []byte(baseBody), 0o600); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(overlay, []byte(overlayBody), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	snapshot, err := config.LoadSnapshot([]string{base}, overlay)
	if err != nil {
		t.Fatalf("LoadSnapshot() error: %v", err)
	}
	logs := &lockedBuffer{}
	writer, err := config.NewOverlayWriter(config.OverlayWriterOptions{
		BasePaths:   []string{base},
		OverlayPath: overlay,
	})
	if err != nil {
		t.Fatalf("NewOverlayWriter: %v", err)
	}
	env := newM6Env(t, m6Options{
		platform:         snapshot.Config,
		overlayWriter:    writer,
		logWriter:        logs,
		profileOverrides: true,
	})
	env.api.SetConfigState(&config.State{Snapshot: snapshot, UpdatedAt: time.Now()})
	return &overlayAPIHarness{t: t, env: env, base: base, overlay: overlay, logs: logs}
}

func (h *overlayAPIHarness) requestBody(changes ...map[string]any) map[string]any {
	h.t.Helper()
	snapshot, err := config.LoadSnapshot([]string{h.base}, h.overlay)
	if err != nil {
		h.t.Fatalf("LoadSnapshot() error: %v", err)
	}
	items := make([]any, 0, len(changes))
	for _, change := range changes {
		items = append(items, change)
	}
	return map[string]any{
		"base_revision":    snapshot.BaseRevision,
		"overlay_revision": snapshot.OverlayRevision,
		"changes":          items,
	}
}

func (h *overlayAPIHarness) patch(client *m6Client, body map[string]any) apiResponse {
	h.t.Helper()
	return client.json(http.MethodPatch, "/api/v1/config/overlay", body)
}

func setChange(path []string, value any) map[string]any {
	return map[string]any{"path": path, "value": value}
}

func deleteChange(path []string) map[string]any {
	return map[string]any{"path": path, "delete": true}
}

func (h *overlayAPIHarness) readOverlay() string {
	h.t.Helper()
	data, err := os.ReadFile(h.overlay)
	if err != nil {
		h.t.Fatalf("read overlay: %v", err)
	}
	return string(data)
}

func (h *overlayAPIHarness) readBase() string {
	h.t.Helper()
	data, err := os.ReadFile(h.base)
	if err != nil {
		h.t.Fatalf("read base: %v", err)
	}
	return string(data)
}

func errorCode(t *testing.T, body map[string]any) string {
	t.Helper()
	envelope, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error envelope: %v", body)
	}
	code, _ := envelope["code"].(string)
	return code
}

func TestConfigOverlayPatchRequiresPlatformAdminAndCSRF(t *testing.T) {
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlayAPIDocument)
	body := harness.requestBody(setChange([]string{"providers", "primary", "max_tokens"}, 8192))

	assertStatus(t, harness.patch(harness.env.anonymousClient(), body), http.StatusUnauthorized)
	viewer := harness.env.loginAs("code-viewer")
	assertStatus(t, harness.patch(viewer, body), http.StatusForbidden)
	if logs := harness.logs.String(); !strings.Contains(logs, `"code":"forbidden"`) || !strings.Contains(logs, `"actor_id":8`) {
		t.Fatalf("viewer rejection audit lacks actor/code:\n%s", logs)
	}
	admin := harness.env.loginAs("code-admin")
	assertStatus(t, harness.patch(admin.withoutCSRF(), body), http.StatusForbidden)
	logs := harness.logs.String()
	for _, want := range []string{`"code":"forbidden"`, `"actor_id":7`, `"event":"platform_audit"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("CSRF rejection audit lacks %s:\n%s", want, logs)
		}
	}
	assertStatus(t, harness.patch(admin, body), http.StatusOK)
}

func TestConfigOverlayPatchRejectsStaticBootstrapBearer(t *testing.T) {
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlayAPIDocument)
	body := harness.requestBody(setChange([]string{"providers", "primary", "max_tokens"}, 8192))
	bearer := harness.env.bearerClient()
	response := harness.patch(bearer, body)
	assertStatus(t, response, http.StatusForbidden)
	if strings.Contains(string(response.body), "expanded-secret") {
		t.Fatal("bootstrap rejection leaked a value")
	}
}

func TestConfigOverlayPatchAllowsAPITokenBearer(t *testing.T) {
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlayAPIDocument)
	admin := harness.env.loginAs("code-admin")
	created := admin.json(http.MethodPost, "/api/v1/api-tokens", map[string]any{"name": "overlay-writer"})
	assertStatus(t, created, http.StatusCreated)
	token, _ := harness.env.decode(t, created)["token"].(string)
	if token == "" {
		t.Fatal("API token response has no token")
	}
	client := &m6Client{t: t, base: harness.env.server.URL, client: &http.Client{}, bearer: token}
	response := harness.patch(client, harness.requestBody(setChange([]string{"providers", "primary", "max_tokens"}, 8192)))
	assertStatus(t, response, http.StatusOK)
}

func TestConfigOverlayPatchWritesAndAuditsWithoutValues(t *testing.T) {
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlayAPIDocument)
	baseBefore := harness.readBase()
	const canary = "claude-opus-audit-canary"
	admin := harness.env.loginAs("code-admin")
	response := harness.patch(admin, harness.requestBody(
		setChange([]string{"providers", "primary", "model"}, canary),
	))
	assertStatus(t, response, http.StatusOK)
	if strings.Contains(string(response.body), "expanded-secret") {
		t.Fatalf("response leaked the expanded environment value: %s", response.body)
	}
	body := harness.env.decode(t, response)
	if body["applied"] != true || body["changed"] != true || body["restart_required"] != true {
		t.Fatalf("applied/changed/restart_required = %v/%v/%v", body["applied"], body["changed"], body["restart_required"])
	}
	if body["previous_overlay_revision"] == body["overlay_revision"] {
		t.Fatalf("revisions did not change: %v", body["previous_overlay_revision"])
	}
	if notice, _ := body["notice"].(string); !strings.Contains(notice, "cancel") {
		t.Fatalf("notice does not mention cancelled reviews: %q", notice)
	}

	after := harness.readOverlay()
	if !strings.Contains(after, canary) || !strings.Contains(after, "# model comment") {
		t.Fatalf("overlay was not patched in place:\n%s", after)
	}
	if harness.readBase() != baseBefore {
		t.Fatal("base configuration was modified")
	}

	logs := harness.logs.String()
	for _, want := range []string{
		`"event":"platform_audit"`, `"actor_id":7`, `"paths":["providers.primary.model"]`,
		`"base_revision"`, `"previous_overlay_revision"`, `"overlay_revision"`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("audit log lacks %s:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, canary) {
		t.Fatalf("audit log contains the written value:\n%s", logs)
	}
}

func TestConfigOverlayPatchDryRunDoesNotWrite(t *testing.T) {
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlayAPIDocument)
	before := harness.readOverlay()
	body := harness.requestBody(setChange([]string{"review_defaults", "language"}, "en-US"))
	body["dry_run"] = true
	admin := harness.env.loginAs("code-admin")
	response := harness.patch(admin, body)
	assertStatus(t, response, http.StatusOK)
	decoded := harness.env.decode(t, response)
	if decoded["dry_run"] != true || decoded["applied"] != false || decoded["changed"] != true {
		t.Fatalf("dry run response = %v", decoded)
	}
	if harness.readOverlay() != before {
		t.Fatal("dry run modified the overlay")
	}
}

func TestConfigOverlayPatchStaleRevision(t *testing.T) {
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlayAPIDocument)
	body := harness.requestBody(setChange([]string{"providers", "primary", "max_tokens"}, 8192))
	body["overlay_revision"] = "deadbeef"
	admin := harness.env.loginAs("code-admin")
	response := harness.patch(admin, body)
	assertStatus(t, response, http.StatusConflict)
	if code := errorCode(t, harness.env.decode(t, response)); code != config.OverlayErrorStaleConfig {
		t.Fatalf("error code = %q, want %q", code, config.OverlayErrorStaleConfig)
	}
}

func TestConfigOverlayPatchRejectsSensitivePath(t *testing.T) {
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlayAPIDocument)
	before := harness.readOverlay()
	admin := harness.env.loginAs("code-admin")
	response := harness.patch(admin, harness.requestBody(
		setChange([]string{"storage", "dsn"}, "file:elsewhere.db"),
	))
	assertStatus(t, response, http.StatusBadRequest)
	if code := errorCode(t, harness.env.decode(t, response)); code != config.OverlayErrorInvalidChange {
		t.Fatalf("error code = %q, want %q", code, config.OverlayErrorInvalidChange)
	}
	if harness.readOverlay() != before {
		t.Fatal("sensitive path modified the overlay")
	}
}

func TestConfigOverlayPatchNotConfigured(t *testing.T) {
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlayAPIDocument)
	admin := harness.env.loginAs("code-admin")
	body := harness.requestBody(setChange([]string{"review_defaults", "language"}, "en-US"))
	// Replace the wired writer with one that has no overlay path.
	writer, err := config.NewOverlayWriter(config.OverlayWriterOptions{})
	if err != nil {
		t.Fatalf("NewOverlayWriter: %v", err)
	}
	harness.env.api.overlayWriter = writer
	response := harness.patch(admin, body)
	assertStatus(t, response, http.StatusConflict)
	if code := errorCode(t, harness.env.decode(t, response)); code != config.OverlayErrorNotConfigured {
		t.Fatalf("error code = %q, want %q", code, config.OverlayErrorNotConfigured)
	}
}

func TestConfigOverlayPatchDeletesReferencedProvider(t *testing.T) {
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlayAPIDocument)
	ctx := context.Background()
	const overrides = `{"lanes":{"review":{"model":"config:extra"}}}`
	if _, err := harness.env.sqlite.PutProfileOverrides(ctx, "default", overrides, 0, 7); err != nil {
		t.Fatalf("PutProfileOverrides: %v", err)
	}
	before := harness.readOverlay()
	admin := harness.env.loginAs("code-admin")
	response := harness.patch(admin, harness.requestBody(deleteChange([]string{"providers", "extra"})))
	assertStatus(t, response, http.StatusConflict)
	body := harness.env.decode(t, response)
	if code := errorCode(t, body); code != config.OverlayErrorProviderInUse {
		t.Fatalf("error code = %q, want %q", code, config.OverlayErrorProviderInUse)
	}
	details := body["error"].(map[string]any)["details"].([]any)
	joined := ""
	for _, detail := range details {
		joined += detail.(map[string]any)["message"].(string) + "\n"
	}
	if !strings.Contains(joined, "overrides.lanes.review.model") {
		t.Fatalf("details do not list the stored referrer: %v", details)
	}
	if harness.readOverlay() != before {
		t.Fatal("referenced provider delete modified the overlay")
	}
	stored, err := harness.env.sqlite.GetProfileOverrides(ctx, "default")
	if err != nil || stored.OverridesJSON != overrides {
		t.Fatalf("stored overrides changed: %+v, %v", stored, err)
	}

	// Removing the override first releases the provider.
	if _, err := harness.env.sqlite.DeleteProfileOverrides(ctx, "default", 1, 7); err != nil {
		t.Fatalf("DeleteProfileOverrides: %v", err)
	}
	response = harness.patch(admin, harness.requestBody(deleteChange([]string{"providers", "extra"})))
	assertStatus(t, response, http.StatusOK)
	if strings.Contains(harness.readOverlay(), "extra:") {
		t.Fatalf("provider extra was not deleted:\n%s", harness.readOverlay())
	}
}

func TestConfigOverlayPatchReviewDefaultsIgnoreDisplay(t *testing.T) {
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlayAPIDocument)
	admin := harness.env.loginAs("code-admin")
	response := harness.patch(admin, harness.requestBody(
		setChange([]string{"review_defaults", "ignore"}, []string{"custom/**"}),
	))
	assertStatus(t, response, http.StatusOK)
	body := harness.env.decode(t, response)
	defaults := body["review_defaults"].(map[string]any)
	overlay := defaults["overlay"].(map[string]any)
	overlayIgnore := overlay["ignore"].([]any)
	if len(overlayIgnore) != 1 || overlayIgnore[0] != "custom/**" {
		t.Fatalf("overlay ignore = %v", overlayIgnore)
	}
	effective := defaults["effective_ignore"].([]any)
	if len(effective) != 1 || effective[0] != "custom/**" {
		t.Fatalf("effective ignore = %v", effective)
	}
	if defaults["ignore_origin"] != "overlay" {
		t.Fatalf("ignore origin = %v", defaults["ignore_origin"])
	}
	builtin := defaults["builtin_ignore"].([]any)
	if len(builtin) != 6 {
		t.Fatalf("builtin ignore = %v", builtin)
	}

	response = harness.patch(admin, harness.requestBody(
		deleteChange([]string{"review_defaults", "ignore"}),
	))
	assertStatus(t, response, http.StatusOK)
	body = harness.env.decode(t, response)
	defaults = body["review_defaults"].(map[string]any)
	if defaults["ignore_origin"] != "default" {
		t.Fatalf("restored ignore origin = %v", defaults["ignore_origin"])
	}
	restored := defaults["effective_ignore"].([]any)
	if len(restored) != len(builtin) {
		t.Fatalf("restored ignore = %v, want the built-in list", restored)
	}
	overlay = defaults["overlay"].(map[string]any)
	if _, ok := overlay["ignore"]; ok {
		t.Fatalf("deleted ignore is still in the raw overlay view: %v", overlay)
	}
}

func TestConfigOverlayPatchErrorHidesExpandedEnvValues(t *testing.T) {
	const base = `
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
    protocol: ${CANARY_PROTOCOL}
    model: claude-sonnet
    api_key_env: PRIMARY_KEY
`
	harness := newOverlayAPIHarness(t, base, overlayAPIDocument)
	before := harness.readOverlay()
	admin := harness.env.loginAs("code-admin")
	response := harness.patch(admin, harness.requestBody(
		setChange([]string{"providers", "primary", "max_tokens"}, 8192),
	))
	assertStatus(t, response, http.StatusBadRequest)
	if code := errorCode(t, harness.env.decode(t, response)); code != config.OverlayErrorInvalidConfig {
		t.Fatalf("error code = %q, want %q", code, config.OverlayErrorInvalidConfig)
	}
	if strings.Contains(string(response.body), "expanded-secret-protocol") {
		t.Fatalf("error response leaked the expanded environment value: %s", response.body)
	}
	if strings.Contains(harness.logs.String(), "expanded-secret-protocol") {
		t.Fatalf("logs leaked the expanded environment value:\n%s", harness.logs.String())
	}
	if harness.readOverlay() != before {
		t.Fatal("failed write modified the overlay")
	}
}

func TestConfigOverlayPatchSuccessResponseKeepsPlaceholders(t *testing.T) {
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlayAPIDocument)
	const secret = "expanded-canary-value"
	admin := harness.env.loginAs("code-admin")
	response := harness.patch(admin, harness.requestBody(
		setChange([]string{"review_defaults", "custom_rules"}, []any{"${CANARY_SECRET}"}),
	))
	assertStatus(t, response, http.StatusOK)
	if !strings.Contains(string(response.body), "${CANARY_SECRET}") {
		t.Fatalf("response does not keep the placeholder: %s", response.body)
	}
	if strings.Contains(string(response.body), secret) {
		t.Fatalf("response leaked the expanded environment value: %s", response.body)
	}
	overlay := harness.readOverlay()
	if !strings.Contains(overlay, "${CANARY_SECRET}") {
		t.Fatalf("overlay does not keep the placeholder:\n%s", overlay)
	}
	if strings.Contains(overlay, secret) {
		t.Fatalf("overlay contains the expanded environment value:\n%s", overlay)
	}
	if strings.Contains(harness.logs.String(), secret) {
		t.Fatalf("logs leaked the expanded environment value:\n%s", harness.logs.String())
	}
}

func TestConfigOverlayPatchNoopDoesNotAskForRestart(t *testing.T) {
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlayAPIDocument)
	admin := harness.env.loginAs("code-admin")
	response := harness.patch(admin, harness.requestBody(deleteChange([]string{"providers", "missing"})))
	assertStatus(t, response, http.StatusOK)
	body := harness.env.decode(t, response)
	if body["applied"] != true || body["changed"] != false || body["restart_required"] != false {
		t.Fatalf("applied/changed/restart_required = %v/%v/%v", body["applied"], body["changed"], body["restart_required"])
	}
	if notice, ok := body["notice"].(string); ok && notice != "" {
		t.Fatalf("no-op response carries a save notice: %q", notice)
	}
	if !strings.Contains(harness.logs.String(), `"changed":false`) {
		t.Fatalf("audit does not mark the no-op write:\n%s", harness.logs.String())
	}
}

func TestConfigOverlayPatchRejectionIsAudited(t *testing.T) {
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlayAPIDocument)
	admin := harness.env.loginAs("code-admin")
	response := harness.patch(admin, harness.requestBody(
		setChange([]string{"storage", "dsn"}, "file:elsewhere.db"),
	))
	assertStatus(t, response, http.StatusBadRequest)
	logs := harness.logs.String()
	for _, want := range []string{
		`"event":"platform_audit"`, `"actor_id":7`, `"paths":["storage.dsn"]`, `"code":"invalid_change"`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("rejection audit lacks %s:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "elsewhere") {
		t.Fatalf("rejection audit leaked the rejected value:\n%s", logs)
	}
}

func TestConfigOverlayPatchDeleteMissingProviderIgnoresStaleOverride(t *testing.T) {
	harness := newOverlayAPIHarness(t, overlayAPIBase, overlayAPIDocument)
	ctx := context.Background()
	const overrides = `{"lanes":{"review":{"model":"config:ghost"}}}`
	if _, err := harness.env.sqlite.PutProfileOverrides(ctx, "default", overrides, 0, 7); err != nil {
		t.Fatalf("PutProfileOverrides: %v", err)
	}
	admin := harness.env.loginAs("code-admin")
	response := harness.patch(admin, harness.requestBody(deleteChange([]string{"providers", "ghost"})))
	assertStatus(t, response, http.StatusOK)
	body := harness.env.decode(t, response)
	if body["applied"] != true || body["changed"] != false {
		t.Fatalf("applied/changed = %v/%v", body["applied"], body["changed"])
	}
	stored, err := harness.env.sqlite.GetProfileOverrides(ctx, "default")
	if err != nil || stored.OverridesJSON != overrides {
		t.Fatalf("stored overrides changed: %+v, %v", stored, err)
	}
}
