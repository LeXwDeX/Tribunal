package config

import (
	"os"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestLoadDefaults(t *testing.T) {
	cfg := loadConfig(t)

	if cfg.Server.Listen != ":8080" {
		t.Errorf("server.listen = %q", cfg.Server.Listen)
	}
	if cfg.Server.ReadTimeout != 15*time.Second || cfg.Server.WriteTimeout != 60*time.Second {
		t.Errorf("server timeouts = %v/%v", cfg.Server.ReadTimeout, cfg.Server.WriteTimeout)
	}
	if cfg.GitLab.RequestTimeout != 30*time.Second || cfg.GitLab.MaxConcurrency != 16 {
		t.Errorf("gitlab defaults = %v/%v", cfg.GitLab.RequestTimeout, cfg.GitLab.MaxConcurrency)
	}
	if cfg.Storage.Driver != "sqlite" || cfg.Storage.DSN == "" {
		t.Errorf("storage defaults = %q/%q", cfg.Storage.Driver, cfg.Storage.DSN)
	}
	if cfg.Storage.LLMCallRetention != 720*time.Hour ||
		cfg.Storage.DeliveryRetention != 336*time.Hour ||
		cfg.Storage.JobRetention != 168*time.Hour ||
		cfg.Storage.ContextVersions != 5 {
		t.Errorf("storage retention defaults = %+v", cfg.Storage)
	}
	if cfg.Secrets.MasterKeyEnv != "REVIEW_MASTER_KEY" {
		t.Errorf("secrets.master_key_env = %q", cfg.Secrets.MasterKeyEnv)
	}
	if cfg.Auth.BootstrapTokenEnv != "REVIEW_ADMIN_TOKEN" {
		t.Errorf("auth.bootstrap_token_env = %q", cfg.Auth.BootstrapTokenEnv)
	}
	if cfg.Auth.SessionTTL != 168*time.Hour {
		t.Errorf("auth.session_ttl = %v", cfg.Auth.SessionTTL)
	}
	if cfg.Jobs.Workers != 4 || cfg.Jobs.Lease != 5*time.Minute {
		t.Errorf("jobs defaults = %v/%v", cfg.Jobs.Workers, cfg.Jobs.Lease)
	}
	if cfg.Engine.MaxConcurrentNodes != 8 || cfg.Engine.DefaultNodeTimeout != 10*time.Minute {
		t.Errorf("engine defaults = %v/%v", cfg.Engine.MaxConcurrentNodes, cfg.Engine.DefaultNodeTimeout)
	}

	profile, ok := cfg.Profiles["default"]
	if !ok {
		t.Fatal("default profile missing")
	}
	if profile.Plan.Mode != "code" || profile.Synthesize.Mode != "code" || profile.OnUncertain != "mark" {
		t.Errorf("default profile = %+v", profile)
	}
	if profile.Budget.MaxNodes != 30 || profile.Budget.MaxExtends != 0 ||
		profile.Budget.Deadline != 20*time.Minute || profile.Budget.MaxTokens != 1500000 {
		t.Errorf("default budget = %+v", profile.Budget)
	}
	if len(profile.Propose) != 1 || profile.Propose[0].Block != "code-review" || profile.Propose[0].Provider != "" {
		t.Errorf("default profile should contain one provider-free code-review block: %+v", profile.Propose)
	}

	if !cfg.ReviewDefaults.SkipDraft {
		t.Error("review_defaults.skip_draft default must be true")
	}
	if cfg.ReviewDefaults.Language != "zh-CN" || cfg.ReviewDefaults.MaxFiles != 200 || cfg.ReviewDefaults.MaxDiffLines != 8000 {
		t.Errorf("review defaults = %+v", cfg.ReviewDefaults)
	}
	if !cfg.ReviewDefaults.PostFailureNote || !cfg.ReviewDefaults.AllowRepoConfig || cfg.ReviewDefaults.AllowRepoSkills {
		t.Errorf("review default booleans = %+v", cfg.ReviewDefaults)
	}
	if len(cfg.Providers) != 0 {
		t.Errorf("providers should be optional in M0, got %v", cfg.Providers)
	}
}

func TestLoadMergesFiles(t *testing.T) {
	base := writeConfig(t, "base.yaml", `
server:
  listen: ":9000"
  trusted_proxies: ["10.0.0.0/8"]
gitlab:
  url: "https://gitlab.example.com"
review_defaults:
  ignore: ["base-only"]
profiles:
  default:
    challenge:
      only: [P0]
  deep:
    plan:
      mode: code
      package_tokens: 123
`)
	override := writeConfig(t, "override.yaml", `
server:
  write_timeout: 30s
gitlab:
  request_timeout: 10s
review_defaults:
  ignore: ["one", "two"]
profiles:
  default:
    challenge:
      only: [P1, P2]
  deep:
    plan:
      mode: agent
`)

	cfg := loadConfig(t, base, override)

	if cfg.Server.Listen != ":9000" {
		t.Errorf("base scalar was not preserved: %q", cfg.Server.Listen)
	}
	if cfg.Server.ReadTimeout != 15*time.Second || cfg.Server.WriteTimeout != 30*time.Second {
		t.Errorf("nested defaults/override = %v/%v", cfg.Server.ReadTimeout, cfg.Server.WriteTimeout)
	}
	if len(cfg.Server.TrustedProxies) != 1 || cfg.Server.TrustedProxies[0] != "10.0.0.0/8" {
		t.Errorf("base array = %v", cfg.Server.TrustedProxies)
	}
	if cfg.GitLab.URL != "https://gitlab.example.com" || cfg.GitLab.RequestTimeout != 10*time.Second {
		t.Errorf("gitlab merge = %+v", cfg.GitLab)
	}
	if got := cfg.ReviewDefaults.Ignore; len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("arrays must replace, got %v", got)
	}
	defaultProfile := cfg.Profiles["default"]
	if defaultProfile.Plan.PackageTokens != 60000 {
		t.Errorf("default profile lost defaults: %+v", defaultProfile)
	}
	if got := defaultProfile.Challenge.Only; len(got) != 2 || got[0] != "P1" || got[1] != "P2" {
		t.Errorf("nested arrays must replace, got %v", got)
	}
	deep := cfg.Profiles["deep"]
	if deep.Plan.Mode != "agent" || deep.Plan.PackageTokens != 123 {
		t.Errorf("map merge did not preserve nested values: %+v", deep.Plan)
	}
	if deep.Budget.MaxNodes != 30 || deep.Budget.Deadline != 20*time.Minute {
		t.Errorf("profile defaults not applied: %+v", deep.Budget)
	}
}

func TestLoadExpandsEnvironmentOnlyInStringScalars(t *testing.T) {
	const url = "https://gitlab.example.com"
	const dsn = "file:data/review.db\nproviders:\n  injected: {}\n"
	t.Setenv("CFG_TEST_GITLAB_URL", url)
	t.Setenv("CFG_TEST_DSN", dsn)

	path := writeConfig(t, "config.yaml", `
gitlab:
  url: ${CFG_TEST_GITLAB_URL}
storage:
  dsn: ${CFG_TEST_DSN}
`)
	cfg := loadConfig(t, path)

	if cfg.GitLab.URL != url {
		t.Errorf("gitlab.url = %q, want %q", cfg.GitLab.URL, url)
	}
	if cfg.Storage.DSN != dsn {
		t.Errorf("storage.dsn = %q, want %q", cfg.Storage.DSN, dsn)
	}
	if len(cfg.Providers) != 0 {
		t.Errorf("environment value was parsed as YAML and changed providers: %v", cfg.Providers)
	}
}

func TestLoadRejectsMissingInterpolationVariable(t *testing.T) {
	const name = "CFG_TEST_DEFINITELY_UNSET"
	unsetEnv(t, name)
	path := writeConfig(t, "config.yaml", "gitlab:\n  url: \"${"+name+"}\"\n")

	_, err := Load(path)
	requireErrorContains(t, err, name, "not set")
}

func TestLoadStrictErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown field",
			body: "server:\n  listen: \":8080\"\n  nope: 1\n",
			want: "field nope not found",
		},
		{
			name: "inline provider secret field",
			body: "providers:\n  p:\n    protocol: anthropic\n    api_key: sk-secret\n",
			want: "field api_key not found",
		},
		{
			name: "duplicate key",
			body: "server:\n  listen: \":1\"\n  listen: \":2\"\n",
			want: "already defined",
		},
		{
			name: "multiple documents",
			body: "server:\n  listen: \":1\"\n---\nserver:\n  listen: \":2\"\n",
			want: "multiple YAML documents",
		},
		{
			name: "trailing document separator",
			body: "server:\n  listen: \":1\"\n---\n",
			want: "multiple YAML documents",
		},
		{
			name: "top-level sequence",
			body: "- server\n",
			want: "top-level value must be a mapping",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, "config.yaml", tt.body)
			_, err := Load(path)
			requireErrorContains(t, err, tt.want)
		})
	}
}

func TestLoadErrorsDoNotEchoValues(t *testing.T) {
	const secret = "super-secret-value"
	path := writeConfig(t, "config.yaml", "server:\n  read_timeout: "+secret+"\n")

	_, err := Load(path)
	requireErrorContains(t, err, "cannot unmarshal", "<redacted>")
	requireErrorNotContains(t, err, secret)
}

func TestLoadEmptyFileIsAllowed(t *testing.T) {
	path := writeConfig(t, "empty.yaml", "")
	cfg := loadConfig(t, path)
	if cfg.Server.Listen != ":8080" {
		t.Errorf("defaults not applied for empty file: %+v", cfg.Server)
	}
}

func TestLoadDoesNotMutateInput(t *testing.T) {
	path := writeConfig(t, "config.yaml", "server:\n  listen: \":9000\"\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loadConfig(t, path)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("Load modified its input file")
	}
	if strings.Contains(string(after), "${") {
		t.Fatal("unexpected interpolation in source file")
	}
}

func TestServerExampleConfig(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load("../../config/server.example.yaml")
	if err != nil {
		t.Fatalf("Load(example): %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(example) = %v", err)
	}
	if cfg.Server.Listen != ":8080" || cfg.Server.PublicURL != "http://localhost:8080" {
		t.Errorf("example server = %+v", cfg.Server)
	}
	if cfg.Storage.Driver != "sqlite" {
		t.Errorf("example storage driver = %q", cfg.Storage.Driver)
	}
	if len(cfg.Providers) != 0 || len(cfg.Blocks) != 0 {
		t.Errorf("example unexpectedly needs providers or MCP: %v/%v", cfg.Providers, cfg.Blocks)
	}
}

func TestYAMLTimeDurationDecodes(t *testing.T) {
	// Guards the transparent time.Duration choice for main.
	path := writeConfig(t, "config.yaml", "server:\n  read_timeout: 1m30s\n")
	cfg := loadConfig(t, path)
	if cfg.Server.ReadTimeout != 90*time.Second {
		t.Errorf("read_timeout = %v", cfg.Server.ReadTimeout)
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("budget:\n  deadline: 20m\n"), &node); err != nil {
		t.Fatalf("yaml node sanity check: %v", err)
	}
}
