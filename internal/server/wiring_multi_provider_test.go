package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/review"
)

const serverDockerConfigPath = "../../config/server.docker.yaml"

type providerUpstream struct {
	mu       sync.Mutex
	calls    int
	paths    []string
	models   []string
	authKeys []string
}

func (u *providerUpstream) record(r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	u.paths = append(u.paths, r.URL.Path)
	key := r.Header.Get("x-api-key")
	if key == "" {
		key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	u.authKeys = append(u.authKeys, key)
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	u.models = append(u.models, body.Model)
}

func (u *providerUpstream) snapshot() (calls int, paths, models, authKeys []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls, append([]string(nil), u.paths...), append([]string(nil), u.models...), append([]string(nil), u.authKeys...)
}

func newOpenAIUpstream(t *testing.T, model string) (*httptest.Server, *providerUpstream) {
	t.Helper()
	upstream := &providerUpstream{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion", "created": 1, "model": model,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "pong"},
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	t.Cleanup(server.Close)
	return server, upstream
}

func newAnthropicUpstream(t *testing.T, model string) (*httptest.Server, *providerUpstream) {
	t.Helper()
	upstream := &providerUpstream{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": model,
			"content":     []map[string]any{{"type": "text", "text": "pong"}},
			"stop_reason": "end_turn", "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	t.Cleanup(server.Close)
	return server, upstream
}

func writeSecondaryDockerConfig(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(serverDockerConfigPath)
	if err != nil {
		t.Fatalf("read %s: %v", serverDockerConfigPath, err)
	}
	body := string(data)
	replacements := []struct{ old, new string }{
		{"  # secondary:\n", "  secondary:\n"},
		{"  #   protocol: \"${LLM_SECONDARY_PROTOCOL}\"\n", "    protocol: \"${LLM_SECONDARY_PROTOCOL}\"\n"},
		{"  #   base_url: \"${LLM_SECONDARY_BASE_URL}\"\n", "    base_url: \"${LLM_SECONDARY_BASE_URL}\"\n"},
		{"  #   model: \"${LLM_SECONDARY_MODEL}\"\n", "    model: \"${LLM_SECONDARY_MODEL}\"\n"},
		{"  #   api_key_env: LLM_SECONDARY_API_KEY\n", "    api_key_env: LLM_SECONDARY_API_KEY\n"},
		{"  #   max_concurrency: 4\n", "    max_concurrency: 4\n"},
		{"  #   max_tokens: 16000\n", "    max_tokens: 16000\n"},
		{
			"      - block: code-review\n        provider: primary\n",
			"      - block: code-review\n        provider: primary\n      - block: code-review\n        provider: secondary\n",
		},
		{
			"    challenge:\n      # Replace primary with secondary to challenge a different model after\n      # enabling providers.secondary above.\n      provider: primary\n",
			"    challenge:\n      provider: secondary\n",
		},
	}
	for _, replacement := range replacements {
		if !strings.Contains(body, replacement.old) {
			t.Fatalf("docker config lost secondary template fragment %q", replacement.old)
		}
		body = strings.Replace(body, replacement.old, replacement.new, 1)
	}
	path := t.TempDir() + "/server.docker.yaml"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func loadSecondaryPlatform(t *testing.T, primaryURL, secondaryURL string) *config.Config {
	t.Helper()
	t.Setenv("TRIBUNAL_PUBLIC_URL", "https://review.example.test")
	t.Setenv("GITLAB_URL", "https://gitlab.example.test")
	t.Setenv("REVIEW_MASTER_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	t.Setenv("REVIEW_ADMIN_TOKEN", strings.Repeat("a", 32))
	t.Setenv("LLM_PROTOCOL", "openai-chat")
	t.Setenv("LLM_BASE_URL", primaryURL+"/v1")
	t.Setenv("LLM_MODEL", "primary-model")
	t.Setenv("LLM_API_KEY", "primary-secret-key")
	t.Setenv("LLM_SECONDARY_PROTOCOL", "anthropic")
	t.Setenv("LLM_SECONDARY_BASE_URL", secondaryURL)
	t.Setenv("LLM_SECONDARY_MODEL", "secondary-model")
	t.Setenv("LLM_SECONDARY_API_KEY", "secondary-secret-key")

	cfg, err := config.Load(writeSecondaryDockerConfig(t))
	if err != nil {
		t.Fatalf("Load(secondary config): %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(secondary config): %v", err)
	}
	return cfg
}

// newMultiProviderWiringEnv reuses newWiringEnv's full service and replaces
// only the provider registry and platform config with the secondary opt-in
// configuration. The HTTP handlers read these fields per request.
func newMultiProviderWiringEnv(t *testing.T, primaryURL, secondaryURL string) (*wiringEnv, *config.Config) {
	t.Helper()
	cfg := loadSecondaryPlatform(t, primaryURL, secondaryURL)
	env := newWiringEnv(t)

	registry, err := llm.NewRegistry(cfg.Providers)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	extensions, err := app.NewExtensionService(app.ExtensionServiceConfig{
		Store:     env.db,
		Clients:   registry,
		Workspace: env.manager,
		Logger:    newTestLogger(io.Discard),
	})
	if err != nil {
		t.Fatalf("NewExtensionService: %v", err)
	}
	env.service.llm = NewAppExtensions(extensions)
	env.service.platform = cfg
	return env, cfg
}

func TestWiringMultiProviderUsesConfiguredUpstreams(t *testing.T) {
	primaryUpstream, primary := newOpenAIUpstream(t, "primary-model")
	secondaryUpstream, secondary := newAnthropicUpstream(t, "secondary-model")
	env, cfg := newMultiProviderWiringEnv(t, primaryUpstream.URL, secondaryUpstream.URL)
	client := env.bearer()

	list := client.request(http.MethodGet, "/api/v1/providers", nil, nil)
	assertStatus(t, list, http.StatusOK)
	body := decodeMap(t, list.body)
	providers, _ := body["providers"].([]any)
	if len(providers) != 2 {
		t.Fatalf("providers = %v, want primary and secondary", body["providers"])
	}
	seen := make(map[string]map[string]any, len(providers))
	for _, item := range providers {
		provider := item.(map[string]any)
		seen[provider["name"].(string)] = provider
	}
	for _, want := range []struct{ name, protocol, model string }{
		{"primary", "openai-chat", "primary-model"},
		{"secondary", "anthropic", "secondary-model"},
	} {
		provider, ok := seen[want.name]
		if !ok {
			t.Fatalf("providers response lost %s: %s", want.name, list.body)
		}
		if provider["protocol"] != want.protocol || provider["model"] != want.model {
			t.Errorf("provider %s = %v, want protocol %q model %q", want.name, provider, want.protocol, want.model)
		}
	}
	assertNoProviderSecrets(t, "GET /providers", list.body)

	tests := []struct {
		name      string
		wantModel string
		wantPath  string
		wantAuth  string
		upstream  *providerUpstream
		other     *providerUpstream
	}{
		{"primary", "primary-model", "/v1/chat/completions", "primary-secret-key", primary, secondary},
		{"secondary", "secondary-model", "/v1/messages", "secondary-secret-key", secondary, primary},
	}
	for _, tt := range tests {
		beforeCalls, _, _, _ := tt.upstream.snapshot()
		otherBefore, _, _, _ := tt.other.snapshot()
		response := client.request(http.MethodPost, "/api/v1/providers/"+tt.name+"/test", nil, nil)
		assertStatus(t, response, http.StatusOK)
		testBody := decodeMap(t, response.body)
		if testBody["ok"] != true || testBody["model"] != tt.wantModel {
			t.Errorf("%s test = %s, want ok model %q", tt.name, response.body, tt.wantModel)
		}
		assertNoProviderSecrets(t, tt.name+" test", response.body)

		calls, paths, models, authKeys := tt.upstream.snapshot()
		if calls != beforeCalls+1 || len(paths) == 0 || paths[len(paths)-1] != tt.wantPath {
			t.Errorf("%s upstream calls = %d (before %d) paths = %v, want one new %s", tt.name, calls, beforeCalls, paths, tt.wantPath)
		}
		if len(models) == 0 || models[len(models)-1] != tt.wantModel {
			t.Errorf("%s upstream models = %v, want %q", tt.name, models, tt.wantModel)
		}
		if len(authKeys) == 0 || authKeys[len(authKeys)-1] != tt.wantAuth {
			t.Errorf("%s upstream auth key did not match the configured key", tt.name)
		}
		if otherAfter, _, _, _ := tt.other.snapshot(); otherAfter != otherBefore {
			t.Errorf("%s test reached the other upstream %d times", tt.name, otherAfter-otherBefore)
		}
	}

	spec, err := review.BuildGraph(
		review.Profile{Name: "default", Config: cfg.Profiles["default"]},
		&review.Dossier{},
		review.GraphConfig{AllowedProviders: []string{"primary", "secondary"}},
	)
	if err != nil {
		t.Fatalf("BuildGraph(default profile): %v", err)
	}
	if len(spec.Nodes) == 0 {
		t.Fatal("BuildGraph(default profile) returned no nodes")
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal graph: %v", err)
	}
	for _, provider := range []string{"primary", "secondary"} {
		if !strings.Contains(string(raw), `"`+provider+`"`) {
			t.Errorf("graph lost provider %q: %s", provider, raw)
		}
	}
}

func assertNoProviderSecrets(t *testing.T, context string, body []byte) {
	t.Helper()
	for _, secret := range []string{
		"primary-secret-key", "secondary-secret-key",
		"LLM_API_KEY", "LLM_SECONDARY_API_KEY",
	} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("%s leaked %q: %s", context, secret, body)
		}
	}
}
