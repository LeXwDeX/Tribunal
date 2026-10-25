package config

import (
	"os"
	"strings"
	"testing"
)

const dockerConfigPath = "../../config/server.docker.yaml"

func setDockerConfigEnv(t *testing.T) {
	t.Helper()
	setRequiredEnv(t)
	t.Setenv("CRC_PUBLIC_URL", "https://review.example.test")
	t.Setenv("GITLAB_URL", "https://gitlab.example.test")
	t.Setenv("LLM_PROTOCOL", "openai-responses")
	t.Setenv("LLM_BASE_URL", "https://llm.example.test/v1")
	t.Setenv("LLM_MODEL", "example-model")
	t.Setenv("LLM_API_KEY", "test-provider-key")
}

func TestDockerConfigArtifact(t *testing.T) {
	setDockerConfigEnv(t)
	for _, name := range []string{
		"GITLAB_OAUTH_CLIENT_ID",
		"GITLAB_OAUTH_CLIENT_SECRET",
		"GITLAB_ADMIN_USERNAME",
	} {
		unsetEnv(t, name)
	}

	cfg := loadConfig(t, dockerConfigPath)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(%s) = %v", dockerConfigPath, err)
	}

	if cfg.Server.Listen != ":8080" {
		t.Errorf("server.listen = %q, want :8080", cfg.Server.Listen)
	}
	if cfg.Server.PublicURL != "https://review.example.test" {
		t.Errorf("server.public_url = %q", cfg.Server.PublicURL)
	}
	if cfg.GitLab.URL != "https://gitlab.example.test" {
		t.Errorf("gitlab.url = %q", cfg.GitLab.URL)
	}
	if cfg.Auth.BootstrapTokenEnv != "REVIEW_ADMIN_TOKEN" {
		t.Errorf("auth.bootstrap_token_env = %q", cfg.Auth.BootstrapTokenEnv)
	}
	if cfg.Auth.OAuth != (OAuthConfig{}) {
		t.Errorf("auth.oauth must be off by default: %+v", cfg.Auth.OAuth)
	}
	if len(cfg.Auth.BootstrapAdmins) != 0 {
		t.Errorf("auth.bootstrap_admins must be commented out by default: %v", cfg.Auth.BootstrapAdmins)
	}
	if cfg.Secrets.MasterKeyEnv != "REVIEW_MASTER_KEY" {
		t.Errorf("secrets.master_key_env = %q", cfg.Secrets.MasterKeyEnv)
	}
	if cfg.Storage.Driver != "sqlite" {
		t.Errorf("storage.driver = %q", cfg.Storage.Driver)
	}
	if cfg.Storage.DSN != "file:/app/data/review.db" {
		t.Errorf("storage.dsn = %q, want file:/app/data/review.db", cfg.Storage.DSN)
	}
	if cfg.Workspace.Root != "/app/data/workspaces" {
		t.Errorf("workspace.root = %q, want /app/data/workspaces", cfg.Workspace.Root)
	}

	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %v, want only primary", cfg.Providers)
	}
	provider, ok := cfg.Providers["primary"]
	if !ok {
		t.Fatal("providers.primary is missing")
	}
	if provider.Protocol != "openai-responses" ||
		provider.BaseURL != "https://llm.example.test/v1" ||
		provider.Model != "example-model" ||
		provider.APIKeyEnv != "LLM_API_KEY" ||
		provider.MaxConcurrency != 4 {
		t.Errorf("providers.primary = %+v", provider)
	}

	profile, ok := cfg.Profiles["default"]
	if !ok {
		t.Fatal("profiles.default is missing")
	}
	if profile.Plan.Mode != "code" || profile.Synthesize.Mode != "code" {
		t.Errorf("default profile code modes changed: plan=%q synthesize=%q",
			profile.Plan.Mode, profile.Synthesize.Mode)
	}
	if len(profile.Propose) != 1 ||
		profile.Propose[0].Block != "code-review" ||
		profile.Propose[0].Provider != "primary" {
		t.Errorf("default propose = %+v", profile.Propose)
	}
	onboarding, ok := cfg.Profiles["onboarding"]
	if !ok || onboarding.Agent == nil || onboarding.Agent.Provider != "primary" {
		t.Fatal("onboarding profile must bind its agent to primary for project registration")
	}
}

func TestDockerConfigOAuthTemplateOptIn(t *testing.T) {
	setDockerConfigEnv(t)
	data, err := os.ReadFile(dockerConfigPath)
	if err != nil {
		t.Fatalf("read %s: %v", dockerConfigPath, err)
	}
	body := string(data)
	for old, replacement := range map[string]string{
		"  # oauth:": "  oauth:",
		"  #   client_id_env: GITLAB_OAUTH_CLIENT_ID":         "    client_id_env: GITLAB_OAUTH_CLIENT_ID",
		"  #   client_secret_env: GITLAB_OAUTH_CLIENT_SECRET": "    client_secret_env: GITLAB_OAUTH_CLIENT_SECRET",
		`  # bootstrap_admins: ["${GITLAB_ADMIN_USERNAME}"]`:  `  bootstrap_admins: ["${GITLAB_ADMIN_USERNAME}"]`,
	} {
		if !strings.Contains(body, old) {
			t.Fatalf("docker config lost OAuth template line %q", old)
		}
		body = strings.ReplaceAll(body, old, replacement)
	}
	t.Setenv("GITLAB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("GITLAB_OAUTH_CLIENT_SECRET", "client-secret")
	t.Setenv("GITLAB_ADMIN_USERNAME", "docker-admin")

	cfg := loadConfig(t, writeConfig(t, "server.docker.yaml", body))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(oauth opt-in) = %v", err)
	}
	if cfg.Auth.OAuth.ClientIDEnv != "GITLAB_OAUTH_CLIENT_ID" ||
		cfg.Auth.OAuth.ClientSecretEnv != "GITLAB_OAUTH_CLIENT_SECRET" {
		t.Errorf("auth.oauth = %+v", cfg.Auth.OAuth)
	}
	if len(cfg.Auth.BootstrapAdmins) != 1 || cfg.Auth.BootstrapAdmins[0] != "docker-admin" {
		t.Errorf("auth.bootstrap_admins = %v", cfg.Auth.BootstrapAdmins)
	}
}

func TestDockerConfigRequiresInterpolatedEnv(t *testing.T) {
	setDockerConfigEnv(t)
	unsetEnv(t, "LLM_BASE_URL")

	_, err := Load(dockerConfigPath)
	requireErrorContains(t, err, "LLM_BASE_URL", "not set")
}

func TestDockerConfigRequiresProviderAPIKey(t *testing.T) {
	setDockerConfigEnv(t)
	unsetEnv(t, "LLM_API_KEY")

	cfg := loadConfig(t, dockerConfigPath)
	requireErrorContains(t, cfg.Validate(), "providers.primary.api_key_env", "LLM_API_KEY")
}
