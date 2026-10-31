package config

import (
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const (
	composePath    = "../../compose.yaml"
	envExamplePath = "../../.env.example"
)

var secondaryEnvNames = []string{
	"LLM_SECONDARY_PROTOCOL",
	"LLM_SECONDARY_BASE_URL",
	"LLM_SECONDARY_MODEL",
	"LLM_SECONDARY_API_KEY",
}

func unsetSecondaryEnv(t *testing.T) {
	t.Helper()
	for _, name := range secondaryEnvNames {
		unsetEnv(t, name)
	}
}

func setSecondaryExampleEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LLM_SECONDARY_PROTOCOL", "anthropic")
	t.Setenv("LLM_SECONDARY_BASE_URL", "https://anthropic.example.test")
	t.Setenv("LLM_SECONDARY_MODEL", "claude-example")
	t.Setenv("LLM_SECONDARY_API_KEY", "test-secondary-key")
}

func readDockerTemplate(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(dockerConfigPath)
	if err != nil {
		t.Fatalf("read %s: %v", dockerConfigPath, err)
	}
	return string(data)
}

// enableSecondaryExample applies the documented opt-in steps to the real
// template: uncomment providers.secondary, add its propose entry, and switch
// the challenger to secondary.
func enableSecondaryExample(t *testing.T, body string) string {
	t.Helper()
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
	return body
}

func TestDockerConfigSecondaryDisabledByDefault(t *testing.T) {
	setDockerConfigEnv(t)
	unsetSecondaryEnv(t)

	cfg := loadConfig(t, dockerConfigPath)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(%s) = %v", dockerConfigPath, err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %v, want only primary", cfg.Providers)
	}
	if _, ok := cfg.Providers["secondary"]; ok {
		t.Fatal("providers.secondary must stay commented out by default")
	}
	profile := cfg.Profiles["default"]
	if len(profile.Propose) != 1 || profile.Propose[0].Provider != "primary" {
		t.Errorf("default propose = %+v, want only primary", profile.Propose)
	}
	if profile.Challenge.Provider != "primary" {
		t.Errorf("default challenge.provider = %q, want primary", profile.Challenge.Provider)
	}
	if onboarding := cfg.Profiles["onboarding"]; onboarding.Agent == nil || onboarding.Agent.Provider != "primary" {
		t.Errorf("onboarding agent = %+v, want primary", onboarding.Agent)
	}

	body := readDockerTemplate(t)
	for _, want := range []string{
		"  # secondary:",
		`  #   protocol: "${LLM_SECONDARY_PROTOCOL}"`,
		`  #   base_url: "${LLM_SECONDARY_BASE_URL}"`,
		`  #   model: "${LLM_SECONDARY_MODEL}"`,
		"  #   api_key_env: LLM_SECONDARY_API_KEY",
		"  #   max_concurrency: 4",
		"  #   max_tokens: 16000",
		"      # - block: code-review",
		"      #   provider: secondary",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s lost secondary template line %q", dockerConfigPath, want)
		}
	}
	for _, want := range []string{"Anthropic", "API root", "/v1"} {
		if !strings.Contains(body, want) {
			t.Errorf("%s secondary comment does not explain %q", dockerConfigPath, want)
		}
	}
}

func TestDockerConfigSecondaryExampleEnabled(t *testing.T) {
	setDockerConfigEnv(t)
	setSecondaryExampleEnv(t)

	body := enableSecondaryExample(t, readDockerTemplate(t))
	cfg := loadConfig(t, writeConfig(t, "server.docker.yaml", body))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(secondary opt-in) = %v", err)
	}

	if len(cfg.Providers) != 2 {
		t.Fatalf("providers = %v, want primary and secondary", cfg.Providers)
	}
	primary := cfg.Providers["primary"]
	if primary.Protocol != "openai-responses" || primary.APIKeyEnv != "LLM_API_KEY" ||
		primary.Model != "example-model" {
		t.Errorf("providers.primary changed: %+v", primary)
	}
	secondary, ok := cfg.Providers["secondary"]
	if !ok {
		t.Fatal("providers.secondary is missing")
	}
	if secondary.Protocol != "anthropic" ||
		secondary.BaseURL != "https://anthropic.example.test" ||
		secondary.Model != "claude-example" ||
		secondary.APIKeyEnv != "LLM_SECONDARY_API_KEY" ||
		secondary.MaxConcurrency != 4 ||
		secondary.MaxTokens != 16000 {
		t.Errorf("providers.secondary = %+v", secondary)
	}

	profile := cfg.Profiles["default"]
	if len(profile.Propose) != 2 ||
		profile.Propose[0].Block != "code-review" || profile.Propose[0].Provider != "primary" ||
		profile.Propose[1].Block != "code-review" || profile.Propose[1].Provider != "secondary" {
		t.Errorf("default propose = %+v, want primary and secondary", profile.Propose)
	}
	if profile.Challenge.Provider != "secondary" {
		t.Errorf("default challenge.provider = %q, want secondary", profile.Challenge.Provider)
	}
	if onboarding := cfg.Profiles["onboarding"]; onboarding.Agent == nil || onboarding.Agent.Provider != "primary" {
		t.Errorf("onboarding agent = %+v, want primary", onboarding.Agent)
	}
}

func TestDockerConfigSecondaryMissingKey(t *testing.T) {
	tests := []struct {
		name  string
		unset bool
		empty bool
	}{
		{name: "unset", unset: true},
		{name: "empty", empty: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setDockerConfigEnv(t)
			setSecondaryExampleEnv(t)
			if tt.unset {
				unsetEnv(t, "LLM_SECONDARY_API_KEY")
			} else {
				t.Setenv("LLM_SECONDARY_API_KEY", "")
			}

			body := enableSecondaryExample(t, readDockerTemplate(t))
			cfg := loadConfig(t, writeConfig(t, "server.docker.yaml", body))
			err := cfg.Validate()
			requireErrorContains(t, err, "providers.secondary.api_key_env", "LLM_SECONDARY_API_KEY")
			requireErrorNotContains(t, err, "test-provider-key", "test-secondary-key")
		})
	}
}

func TestComposeSecondaryEnvForwardedAsOptional(t *testing.T) {
	data, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read %s: %v", composePath, err)
	}
	var doc struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", composePath, err)
	}
	env := doc.Services["review"].Environment
	for name, want := range map[string]string{
		"LLM_SECONDARY_PROTOCOL": "${LLM_SECONDARY_PROTOCOL:-}",
		"LLM_SECONDARY_BASE_URL": "${LLM_SECONDARY_BASE_URL:-}",
		"LLM_SECONDARY_MODEL":    "${LLM_SECONDARY_MODEL:-}",
		"LLM_SECONDARY_API_KEY":  "${LLM_SECONDARY_API_KEY:-}",
	} {
		got, ok := env[name]
		if !ok {
			t.Errorf("%s environment does not forward %s", composePath, name)
			continue
		}
		if got != want {
			t.Errorf("%s environment[%s] = %q, want %q", composePath, name, got, want)
		}
	}
}

func TestEnvExampleDocumentsSecondaryOptions(t *testing.T) {
	data, err := os.ReadFile(envExamplePath)
	if err != nil {
		t.Fatalf("read %s: %v", envExamplePath, err)
	}
	body := string(data)
	for _, name := range secondaryEnvNames {
		if !strings.Contains(body, "\n"+name+"=\n") {
			t.Errorf("%s does not set %s to an empty default", envExamplePath, name)
		}
	}
	for _, want := range []string{"providers.secondary", "independent", "/v1"} {
		if !strings.Contains(body, want) {
			t.Errorf("%s secondary comment does not mention %q", envExamplePath, want)
		}
	}
}

func removeSecondaryProviderBlock(t *testing.T, body string) string {
	t.Helper()
	block := "  secondary:\n" +
		"    protocol: \"${LLM_SECONDARY_PROTOCOL}\"\n" +
		"    base_url: \"${LLM_SECONDARY_BASE_URL}\"\n" +
		"    model: \"${LLM_SECONDARY_MODEL}\"\n" +
		"    api_key_env: LLM_SECONDARY_API_KEY\n" +
		"    max_concurrency: 4\n" +
		"    max_tokens: 16000\n"
	if !strings.Contains(body, block) {
		t.Fatal("docker config lost the enabled secondary provider block")
	}
	return strings.Replace(body, block, "", 1)
}

const defaultProposeWithSecondary = "      - block: code-review\n" +
	"        provider: primary\n" +
	"      - block: code-review\n" +
	"        provider: secondary\n"

func removeSecondaryDefaultPropose(t *testing.T, body string) string {
	t.Helper()
	if !strings.Contains(body, defaultProposeWithSecondary) {
		t.Fatal("docker config lost the enabled secondary propose entry")
	}
	return strings.Replace(body, defaultProposeWithSecondary, "      - block: code-review\n        provider: primary\n", 1)
}

func setChallenge(t *testing.T, body, provider, fallback string) string {
	t.Helper()
	old := "    challenge:\n      provider: secondary\n"
	if !strings.Contains(body, old) {
		t.Fatal("docker config lost the enabled secondary challenge")
	}
	replacement := "    challenge:\n      provider: " + provider + "\n"
	if fallback != "" {
		replacement += "      fallback: " + fallback + "\n"
	}
	return strings.Replace(body, old, replacement, 1)
}

// addResidualSecondaryReferences spreads secondary references across the
// default profile, a second profile, and onboarding so removing the provider
// must fail until every reference is cleaned up.
func addResidualSecondaryReferences(t *testing.T, body string) string {
	t.Helper()
	replacements := []struct{ old, new string }{
		{
			"profiles:\n  onboarding:\n    agent:\n      provider: primary\n",
			"profiles:\n  deep:\n    propose:\n      - block: code-review\n        provider: secondary\n    challenge:\n      provider: primary\n      fallback: secondary\n  onboarding:\n    agent:\n      provider: secondary\n",
		},
		{
			"  default:\n    propose:\n",
			"  default:\n    plan:\n      provider: secondary\n    reasoner:\n      provider: secondary\n      when: never\n    propose:\n",
		},
		{
			"    challenge:\n      provider: secondary\n",
			"    synthesize:\n      provider: secondary\n    agent:\n      provider: secondary\n    challenge:\n      provider: primary\n      fallback: secondary\n",
		},
	}
	for _, replacement := range replacements {
		if !strings.Contains(body, replacement.old) {
			t.Fatalf("docker config lost profile fragment %q", replacement.old)
		}
		body = strings.Replace(body, replacement.old, replacement.new, 1)
	}
	return body
}

func TestDockerConfigSecondaryCrossChallengeValid(t *testing.T) {
	setDockerConfigEnv(t)
	setSecondaryExampleEnv(t)
	body := setChallenge(t, enableSecondaryExample(t, readDockerTemplate(t)), "cross", "secondary")

	cfg := loadConfig(t, writeConfig(t, "server.docker.yaml", body))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(cross challenge with secondary fallback) = %v", err)
	}
	profile := cfg.Profiles["default"]
	if profile.Challenge.Provider != "cross" || profile.Challenge.Fallback != "secondary" {
		t.Errorf("challenge = %+v, want cross challenge with secondary fallback", profile.Challenge)
	}
	if len(profile.Propose) != 2 ||
		profile.Propose[0].Provider != "primary" || profile.Propose[1].Provider != "secondary" {
		t.Errorf("propose = %+v, want primary and secondary", profile.Propose)
	}
}

func TestDockerConfigSecondaryResidualFallbackRejected(t *testing.T) {
	setDockerConfigEnv(t)
	setSecondaryExampleEnv(t)
	body := enableSecondaryExample(t, readDockerTemplate(t))
	body = removeSecondaryProviderBlock(t, body)
	body = removeSecondaryDefaultPropose(t, body)
	body = setChallenge(t, body, "primary", "secondary")

	cfg := loadConfig(t, writeConfig(t, "server.docker.yaml", body))
	err := cfg.Validate()
	requireErrorContains(t, err, `profiles.default.challenge.fallback "secondary" is not defined`)
	requireErrorNotContains(t, err,
		"test-secondary-key", "test-provider-key",
		"profiles.default.plan", "profiles.default.propose", "profiles.default.synthesize",
		"profiles.default.agent", "profiles.onboarding",
	)
}

func TestDockerConfigSecondaryResidualReferencesAcrossProfilesRejected(t *testing.T) {
	setDockerConfigEnv(t)
	setSecondaryExampleEnv(t)
	body := addResidualSecondaryReferences(t, enableSecondaryExample(t, readDockerTemplate(t)))
	body = removeSecondaryProviderBlock(t, body)

	cfg := loadConfig(t, writeConfig(t, "server.docker.yaml", body))
	err := cfg.Validate()
	requireErrorContains(t, err,
		`profiles.default.plan.provider "secondary" is not defined`,
		`profiles.default.reasoner.provider "secondary" is not defined`,
		`profiles.default.propose[1].provider "secondary" is not defined`,
		`profiles.default.synthesize.provider "secondary" is not defined`,
		`profiles.default.agent.provider "secondary" is not defined`,
		`profiles.default.challenge.fallback "secondary" is not defined`,
		`profiles.deep.propose[0].provider "secondary" is not defined`,
		`profiles.deep.challenge.fallback "secondary" is not defined`,
		`profiles.onboarding.agent.provider "secondary" is not defined`,
	)
	requireErrorNotContains(t, err, "test-secondary-key", "test-provider-key")
}

func TestDockerConfigSecondaryAllReferencesRemovedThenValid(t *testing.T) {
	setDockerConfigEnv(t)
	setSecondaryExampleEnv(t)
	body := addResidualSecondaryReferences(t, enableSecondaryExample(t, readDockerTemplate(t)))
	body = removeSecondaryProviderBlock(t, body)
	body = removeSecondaryDefaultPropose(t, body)
	body = strings.ReplaceAll(body, "provider: secondary", "provider: primary")
	body = strings.ReplaceAll(body, "      fallback: secondary\n", "")

	cfg := loadConfig(t, writeConfig(t, "server.docker.yaml", body))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(primary-only after cleanup) = %v", err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %v, want only primary", cfg.Providers)
	}
	profile := cfg.Profiles["default"]
	if profile.Plan.Provider != "primary" ||
		profile.Reasoner == nil || profile.Reasoner.Provider != "primary" ||
		profile.Synthesize.Provider != "primary" ||
		profile.Agent == nil || profile.Agent.Provider != "primary" ||
		profile.Challenge.Provider != "primary" || profile.Challenge.Fallback != "" {
		t.Errorf("default profile still carries secondary references: %+v", profile)
	}
	if len(profile.Propose) != 1 || profile.Propose[0].Provider != "primary" {
		t.Errorf("default propose = %+v, want only primary", profile.Propose)
	}
	deep := cfg.Profiles["deep"]
	if len(deep.Propose) != 1 || deep.Propose[0].Provider != "primary" ||
		deep.Challenge.Provider != "primary" || deep.Challenge.Fallback != "" {
		t.Errorf("deep profile still carries secondary references: %+v", deep)
	}
	onboarding := cfg.Profiles["onboarding"]
	if onboarding.Agent == nil || onboarding.Agent.Provider != "primary" {
		t.Errorf("onboarding agent = %+v, want primary", onboarding.Agent)
	}
}
