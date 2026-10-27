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
	t.Setenv("TRIBUNAL_PUBLIC_URL", "https://review.example.test")
	t.Setenv("CRC_PUBLIC_URL", "")
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

func TestDockerConfigPublicURLEnvPrecedence(t *testing.T) {
	const (
		legacyURL   = "https://legacy.example.test"
		tribunalURL = "https://tribunal.example.test"
	)
	value := func(v string) *string { return &v }
	tests := []struct {
		name     string
		legacy   *string
		tribunal *string
		want     string
		wantErr  bool
	}{
		{name: "legacy only", legacy: value(legacyURL), want: legacyURL},
		{name: "tribunal only", tribunal: value(tribunalURL), want: tribunalURL},
		{name: "empty legacy prefers tribunal", legacy: value(""), tribunal: value(tribunalURL), want: tribunalURL},
		{name: "both prefer tribunal", legacy: value(legacyURL), tribunal: value(tribunalURL), want: tribunalURL},
		{name: "whitespace tribunal falls back", legacy: value(legacyURL), tribunal: value("  "), want: legacyURL},
		{name: "missing both", wantErr: true},
		{name: "whitespace only", tribunal: value("  "), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setDockerConfigEnv(t)
			if tt.legacy == nil {
				unsetEnv(t, "CRC_PUBLIC_URL")
			} else {
				t.Setenv("CRC_PUBLIC_URL", *tt.legacy)
			}
			if tt.tribunal == nil {
				unsetEnv(t, "TRIBUNAL_PUBLIC_URL")
			} else {
				t.Setenv("TRIBUNAL_PUBLIC_URL", *tt.tribunal)
			}

			cfg, err := Load(dockerConfigPath)
			if tt.wantErr {
				requireErrorContains(t, err, "TRIBUNAL_PUBLIC_URL", "not set")
				return
			}
			if err != nil {
				t.Fatalf("Load(%s) = %v", dockerConfigPath, err)
			}
			if cfg.Server.PublicURL != tt.want {
				t.Errorf("server.public_url = %q, want %q", cfg.Server.PublicURL, tt.want)
			}
		})
	}
}

func TestDockerConfigUsesTribunalPublicURLPlaceholder(t *testing.T) {
	data, err := os.ReadFile(dockerConfigPath)
	if err != nil {
		t.Fatalf("read %s: %v", dockerConfigPath, err)
	}
	body := string(data)
	if strings.Contains(body, "${CRC_PUBLIC_URL}") {
		t.Errorf("%s still contains the legacy ${CRC_PUBLIC_URL} placeholder", dockerConfigPath)
	}
	if !strings.Contains(body, `public_url: "${TRIBUNAL_PUBLIC_URL}"`) {
		t.Errorf("%s active public_url does not use ${TRIBUNAL_PUBLIC_URL}", dockerConfigPath)
	}
	if !strings.Contains(body, "Redirect URI: ${TRIBUNAL_PUBLIC_URL}/auth/callback") {
		t.Errorf("%s OAuth callback comment does not use ${TRIBUNAL_PUBLIC_URL}", dockerConfigPath)
	}
}

func TestPublicURLEnvAliasLookup(t *testing.T) {
	const (
		legacyURL   = "https://legacy.example.test"
		tribunalURL = "https://tribunal.example.test"
	)
	value := func(v string) *string { return &v }
	tests := []struct {
		name        string
		placeholder string
		legacy      *string
		tribunal    *string
		want        string
		wantErr     string
	}{
		{name: "new placeholder old env only", placeholder: "TRIBUNAL_PUBLIC_URL", legacy: value(legacyURL), want: legacyURL},
		{name: "new placeholder new env only", placeholder: "TRIBUNAL_PUBLIC_URL", tribunal: value(tribunalURL), want: tribunalURL},
		{name: "new placeholder both prefer new", placeholder: "TRIBUNAL_PUBLIC_URL", legacy: value(legacyURL), tribunal: value(tribunalURL), want: tribunalURL},
		{name: "old yaml old env only", placeholder: "CRC_PUBLIC_URL", legacy: value(legacyURL), want: legacyURL},
		{name: "old yaml new env only", placeholder: "CRC_PUBLIC_URL", tribunal: value(tribunalURL), want: tribunalURL},
		{name: "old yaml both prefer new", placeholder: "CRC_PUBLIC_URL", legacy: value(legacyURL), tribunal: value(tribunalURL), want: tribunalURL},
		{name: "old yaml whitespace new falls back", placeholder: "CRC_PUBLIC_URL", legacy: value(legacyURL), tribunal: value("  "), want: legacyURL},
		{name: "new placeholder missing both", placeholder: "TRIBUNAL_PUBLIC_URL", wantErr: "TRIBUNAL_PUBLIC_URL"},
		{name: "old yaml missing both", placeholder: "CRC_PUBLIC_URL", wantErr: "CRC_PUBLIC_URL"},
		{name: "new placeholder whitespace only", placeholder: "TRIBUNAL_PUBLIC_URL", tribunal: value("  "), wantErr: "TRIBUNAL_PUBLIC_URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			if tt.legacy == nil {
				unsetEnv(t, "CRC_PUBLIC_URL")
			} else {
				t.Setenv("CRC_PUBLIC_URL", *tt.legacy)
			}
			if tt.tribunal == nil {
				unsetEnv(t, "TRIBUNAL_PUBLIC_URL")
			} else {
				t.Setenv("TRIBUNAL_PUBLIC_URL", *tt.tribunal)
			}
			path := writeConfig(t, "server.yaml", "server:\n  public_url: \"${"+tt.placeholder+"}\"\n")

			cfg, err := Load(path)
			if tt.wantErr != "" {
				requireErrorContains(t, err, tt.wantErr, "not set")
				return
			}
			if err != nil {
				t.Fatalf("Load() = %v", err)
			}
			if cfg.Server.PublicURL != tt.want {
				t.Errorf("server.public_url = %q, want %q", cfg.Server.PublicURL, tt.want)
			}
		})
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
	t.Setenv("GITLAB_ADMIN_USERNAME", "id:42")

	cfg := loadConfig(t, writeConfig(t, "server.docker.yaml", body))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(oauth opt-in) = %v", err)
	}
	if cfg.Auth.OAuth.ClientIDEnv != "GITLAB_OAUTH_CLIENT_ID" ||
		cfg.Auth.OAuth.ClientSecretEnv != "GITLAB_OAUTH_CLIENT_SECRET" {
		t.Errorf("auth.oauth = %+v", cfg.Auth.OAuth)
	}
	if len(cfg.Auth.BootstrapAdmins) != 1 || cfg.Auth.BootstrapAdmins[0] != "id:42" {
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
