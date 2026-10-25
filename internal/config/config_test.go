package config

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const validGitLab = `gitlab:
  url: "https://gitlab.example.com"
`

func TestValidateDefaultConfigWithoutProviders(t *testing.T) {
	setRequiredEnv(t)
	path := writeConfig(t, "config.yaml", validGitLab)
	cfg := loadConfig(t, path)

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if cfg.Profiles["default"].Plan.Mode != "code" {
		t.Errorf("default profile not usable: %+v", cfg.Profiles["default"])
	}
}

func TestValidateStorageRetention(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "llm call retention",
			body: "storage:\n  llm_call_retention: 0s\n",
			want: "storage.llm_call_retention",
		},
		{
			name: "delivery retention",
			body: "storage:\n  delivery_retention: -1h\n",
			want: "storage.delivery_retention",
		},
		{
			name: "job retention",
			body: "storage:\n  job_retention: 0s\n",
			want: "storage.job_retention",
		},
		{
			name: "context versions",
			body: "storage:\n  context_versions: 0\n",
			want: "storage.context_versions",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			cfg := loadConfig(t, writeConfig(t, "config.yaml", validGitLab+tt.body))
			requireErrorContains(t, cfg.Validate(), tt.want, "must be positive")
		})
	}
}

func TestValidateAggregatesErrorsWithoutLeakingValues(t *testing.T) {
	const (
		providerEnv = "CFG_TEST_PROVIDER_KEY"
		masterValue = "not-base64-super-secret-master"
		tokenValue  = "short-secret-token"
	)
	unsetEnv(t, providerEnv)
	t.Setenv("REVIEW_MASTER_KEY", masterValue)
	t.Setenv("REVIEW_ADMIN_TOKEN", tokenValue)

	path := writeConfig(t, "config.yaml", validGitLab+`
providers:
  alpha:
    protocol: anthropic
    api_key_env: `+providerEnv+`
profiles:
  default:
    plan:
      mode: agent
      provider: missing-plan
    propose:
      - block: code-review
        provider: missing-propose
    reasoner:
      provider: missing-reasoner
    challenge:
      provider: cross
      fallback: missing-fallback
    on_uncertain: judge
    synthesize:
      mode: code
      provider: missing-synth
    agent:
      provider: missing-agent
    budget:
      max_nodes: 101
      max_extends: 3
      deadline: 20m
      max_tokens: 100
`)
	cfg := loadConfig(t, path)
	err := cfg.Validate()
	requireErrorContains(t, err,
		"secrets.master_key_env", "REVIEW_MASTER_KEY", "base64",
		"auth.bootstrap_token_env", "REVIEW_ADMIN_TOKEN", "at least 32 bytes",
		"providers.alpha.api_key_env", providerEnv,
		"missing-plan", "missing-propose", "missing-reasoner", "missing-fallback",
		"missing-synth", "missing-agent",
		"profiles.default.on_uncertain", "profiles.default.budget.max_nodes",
		"profiles.default.budget.max_extends",
	)
	requireErrorNotContains(t, err, masterValue, tokenValue)
}

func TestValidateBootstrapTokenLength(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{name: "31 bytes", token: strings.Repeat("a", 31), wantErr: true},
		{name: "32 bytes", token: strings.Repeat("a", 32), wantErr: false},
		{name: "empty", token: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("REVIEW_MASTER_KEY", base64.StdEncoding.EncodeToString([]byte(testMasterKey)))
			t.Setenv("REVIEW_ADMIN_TOKEN", tt.token)
			cfg := loadConfig(t, writeConfig(t, "config.yaml", validGitLab))
			err := cfg.Validate()
			if tt.wantErr {
				requireErrorContains(t, err, "auth.bootstrap_token_env")
				if tt.token != "" {
					requireErrorNotContains(t, err, tt.token)
				}
			} else if err != nil {
				t.Fatalf("Validate() = %v", err)
			}
		})
	}
}

func TestValidateMasterKey(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{
			name:  "valid base64",
			value: base64.StdEncoding.EncodeToString([]byte(testMasterKey)),
		},
		{
			name:    "invalid base64",
			value:   "not-base64-secret",
			wantErr: true,
		},
		{
			name:    "decoded 31 bytes",
			value:   base64.StdEncoding.EncodeToString([]byte(testMasterKey[:31])),
			wantErr: true,
		},
		{
			name:    "empty",
			value:   "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("REVIEW_MASTER_KEY", tt.value)
			t.Setenv("REVIEW_ADMIN_TOKEN", testBootstrapKey)
			cfg := loadConfig(t, writeConfig(t, "config.yaml", validGitLab))
			err := cfg.Validate()
			if tt.wantErr {
				requireErrorContains(t, err, "secrets.master_key_env")
				if tt.value != "" {
					requireErrorNotContains(t, err, tt.value)
				}
			} else if err != nil {
				t.Fatalf("Validate() = %v", err)
			}
		})
	}
}

func TestValidateURLsAndStorage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "gitlab ftp",
			body: "gitlab:\n  url: \"ftp://gitlab.example.com\"\n",
			want: "gitlab.url",
		},
		{
			name: "gitlab credentials",
			body: "gitlab:\n  url: \"https://user:pass@gitlab.example.com\"\n",
			want: "gitlab.url",
		},
		{
			name: "gitlab query",
			body: "gitlab:\n  url: \"https://gitlab.example.com?x=1\"\n",
			want: "gitlab.url",
		},
		{
			name: "gitlab fragment",
			body: "gitlab:\n  url: \"https://gitlab.example.com#frag\"\n",
			want: "gitlab.url",
		},
		{
			name: "gitlab empty fragment marker",
			body: "gitlab:\n  url: \"https://gitlab.example.com/#\"\n",
			want: "gitlab.url",
		},
		{
			name: "gitlab missing",
			body: "server:\n  listen: \":8080\"\n",
			want: "gitlab.url is required",
		},
		{
			name: "server public url",
			body: validGitLab + "server:\n  public_url: \"ftp://review.example.com\"\n",
			want: "server.public_url",
		},
		{
			name: "provider base url",
			body: validGitLab + `
providers:
  p:
    protocol: openai-chat
    api_key_env: CFG_TEST_PROVIDER_KEY
    base_url: "https://user:pass@api.example.com"
`,
			want: "providers.p.base_url",
		},
		{
			name: "postgres",
			body: validGitLab + "storage:\n  driver: postgres\n",
			want: "not supported",
		},
		{
			name: "unknown driver",
			body: validGitLab + "storage:\n  driver: mysql\n",
			want: "storage.driver",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("CFG_TEST_PROVIDER_KEY", "provider-secret")
			cfg := loadConfig(t, writeConfig(t, "config.yaml", tt.body))
			err := cfg.Validate()
			requireErrorContains(t, err, tt.want)
		})
	}
}

func TestValidateProviderProtocolAndEnv(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "invalid protocol",
			body: validGitLab + `
providers:
  p:
    protocol: grpc
    api_key_env: CFG_TEST_PROVIDER_KEY
`,
			want: "providers.p.protocol",
		},
		{
			name: "missing api_key_env",
			body: validGitLab + `
providers:
  p:
    protocol: anthropic
`,
			want: "providers.p.api_key_env",
		},
		{
			name: "unset api_key_env",
			body: validGitLab + `
providers:
  p:
    protocol: anthropic
    api_key_env: CFG_TEST_PROVIDER_KEY
`,
			want: "CFG_TEST_PROVIDER_KEY",
		},
		{
			name: "negative concurrency",
			body: validGitLab + `
providers:
  p:
    protocol: anthropic
    api_key_env: CFG_TEST_PROVIDER_KEY
    max_concurrency: -1
`,
			want: "providers.p.max_concurrency",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			unsetEnv(t, "CFG_TEST_PROVIDER_KEY")
			cfg := loadConfig(t, writeConfig(t, "config.yaml", tt.body))
			err := cfg.Validate()
			requireErrorContains(t, err, tt.want)
		})
	}
}

func TestValidateProviderWithAPIKeyEnvPasses(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("CFG_TEST_PROVIDER_KEY", "provider-secret")
	path := writeConfig(t, "config.yaml", validGitLab+`
providers:
  p:
    protocol: openai-responses
    base_url: "https://api.example.com/v1"
    api_key_env: CFG_TEST_PROVIDER_KEY
`)
	cfg := loadConfig(t, path)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestValidateProfileReferences(t *testing.T) {
	setRequiredEnv(t)
	path := writeConfig(t, "config.yaml", validGitLab+`
providers:
  p:
    protocol: anthropic
    api_key_env: CFG_TEST_PROVIDER_KEY
profiles:
  default:
    plan:
      provider: missing-plan
    propose:
      - provider: missing-propose
    reasoner:
      provider: missing-reasoner
    challenge:
      provider: p
      fallback: missing-fallback
    synthesize:
      provider: missing-synth
    agent:
      provider: p
    budget:
      max_nodes: 100
      max_extends: 2
      deadline: 1m
`)
	t.Setenv("CFG_TEST_PROVIDER_KEY", "x")
	cfg := loadConfig(t, path)
	err := cfg.Validate()
	requireErrorContains(t, err,
		"missing-plan", "missing-propose", "missing-reasoner", "missing-fallback", "missing-synth")
	requireErrorNotContains(t, err, "profiles.default.agent")
}

func TestValidateChallengeCrossRequiresFallback(t *testing.T) {
	setRequiredEnv(t)
	path := writeConfig(t, "config.yaml", validGitLab+`
profiles:
  default:
    challenge:
      provider: cross
`)
	cfg := loadConfig(t, path)
	requireErrorContains(t, cfg.Validate(), "profiles.default.challenge.fallback")
}

func TestValidateJudgeRequiresAgentSynthesize(t *testing.T) {
	setRequiredEnv(t)
	for _, mode := range []string{"code", "agent"} {
		t.Run(mode, func(t *testing.T) {
			path := writeConfig(t, "config.yaml", validGitLab+`
profiles:
  default:
    on_uncertain: judge
    synthesize:
      mode: `+mode+`
`)
			cfg := loadConfig(t, path)
			err := cfg.Validate()
			if mode == "agent" {
				if err != nil {
					t.Fatalf("Validate() = %v", err)
				}
			} else {
				requireErrorContains(t, err, "profiles.default.on_uncertain", "agent")
			}
		})
	}
}

func TestValidateOAuthOptional(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		setEnv  bool
		wantErr string
	}{
		{
			name: "not configured",
			body: validGitLab,
		},
		{
			name: "only client id configured",
			body: validGitLab + `
auth:
  oauth:
    client_id_env: CFG_TEST_OAUTH_ID
`,
			setEnv:  true,
			wantErr: "set together",
		},
		{
			name: "missing client secret value",
			body: validGitLab + `
auth:
  oauth:
    client_id_env: CFG_TEST_OAUTH_ID
    client_secret_env: CFG_TEST_OAUTH_SECRET
`,
			wantErr: "CFG_TEST_OAUTH_SECRET",
		},
		{
			name: "both configured",
			body: validGitLab + `
server:
  public_url: "https://review.example.com"
auth:
  oauth:
    client_id_env: CFG_TEST_OAUTH_ID
    client_secret_env: CFG_TEST_OAUTH_SECRET
`,
			setEnv: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			unsetEnv(t, "CFG_TEST_OAUTH_ID")
			unsetEnv(t, "CFG_TEST_OAUTH_SECRET")
			if tt.setEnv {
				t.Setenv("CFG_TEST_OAUTH_ID", "client-id")
				t.Setenv("CFG_TEST_OAUTH_SECRET", "client-secret")
			}
			cfg := loadConfig(t, writeConfig(t, "config.yaml", tt.body))
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v", err)
				}
				return
			}
			requireErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestValidateMCPBindings(t *testing.T) {
	setRequiredEnv(t)
	mcpPath := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(mcpPath, []byte(`{"mcpServers":{"known":{"command":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, "config.yaml", validGitLab+`
mcp:
  config: `+mcpPath+`
blocks:
  review:
    mcp_tools:
      - mcp__known__search_graph
      - mcp__missing__find
      - bad
`)
	cfg := loadConfig(t, path)
	err := cfg.Validate()
	requireErrorContains(t, err, "mcp__missing__find", "bad")
	requireErrorNotContains(t, err, "mcp__known__search_graph")
}

func TestValidateMCPBindingsNeedConfig(t *testing.T) {
	setRequiredEnv(t)
	path := writeConfig(t, "config.yaml", validGitLab+`
blocks:
  review:
    mcp_tools: [mcp__known__tool]
`)
	cfg := loadConfig(t, path)
	requireErrorContains(t, cfg.Validate(), "mcp.config")
}

func TestValidateMCPConfigIgnoredWithoutBindings(t *testing.T) {
	setRequiredEnv(t)
	path := writeConfig(t, "config.yaml", validGitLab+`
mcp:
  config: /definitely/not/read.json
blocks:
  review:
    skills: [coding-standards]
`)
	cfg := loadConfig(t, path)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestReviewDefaultsExplicitFalseAndJSONOverlay(t *testing.T) {
	setRequiredEnv(t)
	cfg := loadConfig(t, writeConfig(t, "config.yaml", validGitLab+`
review_defaults:
  skip_draft: false
  post_failure_note: false
  allow_repo_config: false
`))
	if cfg.ReviewDefaults.SkipDraft || cfg.ReviewDefaults.PostFailureNote || cfg.ReviewDefaults.AllowRepoConfig {
		t.Fatalf("explicit false was lost: %+v", cfg.ReviewDefaults)
	}

	settings := loadConfig(t).ReviewDefaults
	if !settings.SkipDraft || !settings.PostFailureNote {
		t.Fatalf("unexpected defaults: %+v", settings)
	}
	if err := json.Unmarshal([]byte(`{"skip_draft":false,"max_files":12}`), &settings); err != nil {
		t.Fatal(err)
	}
	if settings.SkipDraft {
		t.Error("JSON overlay with explicit false did not override true")
	}
	if !settings.PostFailureNote || settings.MaxFiles != 12 {
		t.Errorf("JSON overlay changed absent fields: %+v", settings)
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"skip_draft":false`) {
		t.Errorf("serialized settings drop explicit false: %s", encoded)
	}
}

func TestConfigSerializationHasNoSecretValues(t *testing.T) {
	const masterValue = "c2VjcmV0LW1hc3Rlci1rZXktdmFsdWU="
	const tokenValue = "bootstrap-token-value-that-is-long"
	t.Setenv("REVIEW_MASTER_KEY", masterValue)
	t.Setenv("REVIEW_ADMIN_TOKEN", tokenValue)
	cfg := loadConfig(t, writeConfig(t, "config.yaml", validGitLab))

	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), masterValue) || strings.Contains(string(encoded), tokenValue) {
		t.Fatalf("serialized config contains secret values: %s", encoded)
	}
}

func TestValidateBoundariesPass(t *testing.T) {
	setRequiredEnv(t)
	path := writeConfig(t, "config.yaml", validGitLab+`
profiles:
  default:
    budget:
      max_nodes: 100
      max_extends: 2
      deadline: 1s
`)
	if err := loadConfig(t, path).Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestValidatePositiveTimeoutsAndConcurrency(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "server read timeout",
			body: validGitLab + "server:\n  read_timeout: 0s\n",
			want: "server.read_timeout",
		},
		{
			name: "gitlab concurrency",
			body: "gitlab:\n  url: \"https://gitlab.example.com\"\n  max_concurrency: 0\n",
			want: "gitlab.max_concurrency",
		},
		{
			name: "jobs workers",
			body: validGitLab + "jobs:\n  workers: 0\n",
			want: "jobs.workers",
		},
		{
			name: "engine timeout",
			body: validGitLab + "engine:\n  default_node_timeout: 0s\n",
			want: "engine.default_node_timeout",
		},
		{
			name: "budget deadline",
			body: validGitLab + "profiles:\n  default:\n    budget:\n      deadline: 0s\n",
			want: "profiles.default.budget.deadline",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			cfg := loadConfig(t, writeConfig(t, "config.yaml", tt.body))
			requireErrorContains(t, cfg.Validate(), tt.want)
		})
	}
}

func TestValidateOAuthRequiresPublicURL(t *testing.T) {
	const oauthEnv = `
auth:
  oauth:
    client_id_env: CFG_TEST_OAUTH_ID
    client_secret_env: CFG_TEST_OAUTH_SECRET
`
	tests := []struct {
		name    string
		body    string
		oauth   bool
		wantErr string
	}{
		{
			name:    "missing public url",
			body:    validGitLab + oauthEnv,
			oauth:   true,
			wantErr: "server.public_url is required",
		},
		{
			name:    "relative public url",
			body:    validGitLab + oauthEnv + "server:\n  public_url: \"/review\"\n",
			oauth:   true,
			wantErr: "server.public_url",
		},
		{
			name:  "valid public url",
			body:  validGitLab + oauthEnv + "server:\n  public_url: \"https://review.example.com\"\n",
			oauth: true,
		},
		{
			name: "no oauth without public url",
			body: validGitLab,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			unsetEnv(t, "CFG_TEST_OAUTH_ID")
			unsetEnv(t, "CFG_TEST_OAUTH_SECRET")
			if tt.oauth {
				t.Setenv("CFG_TEST_OAUTH_ID", "client-id")
				t.Setenv("CFG_TEST_OAUTH_SECRET", "client-secret")
			}
			cfg := loadConfig(t, writeConfig(t, "config.yaml", tt.body))
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v", err)
				}
				return
			}
			requireErrorContains(t, err, tt.wantErr)
		})
	}
}
