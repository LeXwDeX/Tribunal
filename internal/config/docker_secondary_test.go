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
// template: uncomment providers.secondary, move the review lane to it, and
// switch the challenger to secondary.
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
		{"    # lanes:\n    #   review: { provider: secondary }\n", "    lanes:\n      review: { provider: secondary }\n"},
		{"    # challenge: { provider: secondary }\n", "    challenge: { provider: secondary }\n"},
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
	if lanes := laneProviders(profile); lanes != "primary,primary,primary,primary,primary" {
		t.Errorf("default explore and lanes = %s, want all primary", lanes)
	}
	if profile.Challenge.Provider != "primary" {
		t.Errorf("default challenge.provider = %q, want primary", profile.Challenge.Provider)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("shipped config must load without migration warnings: %v", cfg.Warnings)
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
		"    #   review: { provider: secondary }",
		"    # challenge: { provider: secondary }",
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
	if lanes := laneProviders(profile); lanes != "primary,primary,secondary,primary,primary" {
		t.Errorf("default explore,code_review,review,reasoner,validate = %s, want the review lane on secondary", lanes)
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

func TestDockerConfigSecondaryRequiresModel(t *testing.T) {
	for _, model := range []string{"", "   "} {
		t.Run("model="+model, func(t *testing.T) {
			setDockerConfigEnv(t)
			setSecondaryExampleEnv(t)
			t.Setenv("LLM_SECONDARY_MODEL", model)
			body := enableSecondaryExample(t, readDockerTemplate(t))
			cfg := loadConfig(t, writeConfig(t, "server.docker.yaml", body))
			err := cfg.Validate()
			requireErrorContains(t, err, "providers.secondary.model")
			requireErrorNotContains(t, err, "test-provider-key", "test-secondary-key")
		})
	}
}

func TestDockerConfigSecondaryAllowsDefaultBaseURL(t *testing.T) {
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			setDockerConfigEnv(t)
			setSecondaryExampleEnv(t)
			t.Setenv("LLM_SECONDARY_PROTOCOL", protocol)
			t.Setenv("LLM_SECONDARY_BASE_URL", "")
			body := enableSecondaryExample(t, readDockerTemplate(t))
			cfg := loadConfig(t, writeConfig(t, "server.docker.yaml", body))
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
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

// laneProviders lists explore, code_review, review, reasoner and validate.
func laneProviders(profile ProfileConfig) string {
	if profile.Explore == nil || profile.Lanes == nil {
		return "missing"
	}
	return strings.Join([]string{
		profile.Explore.Provider, profile.Lanes.CodeReview.Provider, profile.Lanes.Review.Provider,
		profile.Lanes.Reasoner.Provider, profile.Lanes.Validate.Provider,
	}, ",")
}

// addLegacySecondaryReferences writes an earlier release's profile shape: a
// second review profile and legacy default-profile fields, all on secondary.
func addLegacySecondaryReferences(t *testing.T, body string) string {
	t.Helper()
	replacements := []struct{ old, new string }{
		{
			"profiles:\n  onboarding:\n    agent:\n      provider: primary\n",
			"profiles:\n  deep:\n    propose:\n      - block: code-review\n        provider: secondary\n    challenge:\n      provider: primary\n      fallback: secondary\n  onboarding:\n    agent:\n      provider: secondary\n",
		},
		{
			"  default:\n    provider: primary\n",
			"  default:\n    provider: primary\n    plan:\n      mode: agent\n      provider: secondary\n    reasoner:\n      provider: secondary\n      when: auto\n    synthesize:\n      mode: agent\n      provider: secondary\n    agent:\n      provider: secondary\n",
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

func TestDockerConfigLegacyCrossChallengeMigrates(t *testing.T) {
	setDockerConfigEnv(t)
	setSecondaryExampleEnv(t)
	body := enableSecondaryExample(t, readDockerTemplate(t))
	body = strings.Replace(body, "    challenge: { provider: secondary }\n", "    challenge: { provider: cross, fallback: secondary }\n", 1)

	cfg := loadConfig(t, writeConfig(t, "server.docker.yaml", body))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(legacy cross challenge) = %v", err)
	}
	if got := cfg.Profiles["default"].Challenge; got.Provider != "secondary" || got.Fallback != "" {
		t.Errorf("challenge = %+v, want the fallback as the challenge provider", got)
	}
	requireWarning(t, cfg.Warnings, "challenge.provider cross")
}

func TestDockerConfigSecondaryResidualChallengeRejected(t *testing.T) {
	setDockerConfigEnv(t)
	setSecondaryExampleEnv(t)
	body := enableSecondaryExample(t, readDockerTemplate(t))
	body = removeSecondaryProviderBlock(t, body)
	body = strings.Replace(body, "      review: { provider: secondary }\n", "      review: { provider: primary }\n", 1)

	cfg := loadConfig(t, writeConfig(t, "server.docker.yaml", body))
	err := cfg.Validate()
	requireErrorContains(t, err, `profiles.default.challenge.provider "secondary" is not defined`)
	requireErrorNotContains(t, err, "test-secondary-key", "test-provider-key", "profiles.default.lanes", "profiles.onboarding")
}

func TestDockerConfigLegacyProfilesLoadWithWarnings(t *testing.T) {
	setDockerConfigEnv(t)
	setSecondaryExampleEnv(t)
	body := addLegacySecondaryReferences(t, enableSecondaryExample(t, readDockerTemplate(t)))
	body = removeSecondaryProviderBlock(t, body)

	cfg := loadConfig(t, writeConfig(t, "server.docker.yaml", body))
	err := cfg.Validate()
	// Migrated provider choices are still checked; ignored flow choices and
	// the removed profile are not.
	requireErrorContains(t, err,
		`profiles.default.lanes.review.provider "secondary" is not defined`,
		`profiles.default.lanes.reasoner.provider "secondary" is not defined`,
		`profiles.default.challenge.provider "secondary" is not defined`,
		`profiles.onboarding.agent.provider "secondary" is not defined`,
	)
	requireErrorNotContains(t, err, "test-secondary-key", "test-provider-key",
		"profiles.deep", "profiles.default.plan", "profiles.default.synthesize", "profiles.default.agent")
	for _, want := range []string{
		"profiles.deep was removed", "profiles.default.plan.mode was ignored", "profiles.default.plan.provider was ignored",
		"profiles.default.reasoner is deprecated", "profiles.default.synthesize was ignored", "profiles.default.agent was ignored",
	} {
		requireWarning(t, cfg.Warnings, want)
	}
	if _, ok := cfg.Profiles["deep"]; ok {
		t.Error("a removed review profile must not stay selectable")
	}
}

func TestDockerConfigLegacyReferencesRemovedThenValid(t *testing.T) {
	setDockerConfigEnv(t)
	setSecondaryExampleEnv(t)
	body := addLegacySecondaryReferences(t, enableSecondaryExample(t, readDockerTemplate(t)))
	body = removeSecondaryProviderBlock(t, body)
	body = strings.ReplaceAll(body, "provider: secondary", "provider: primary")
	body = strings.ReplaceAll(body, "      fallback: secondary\n", "")

	cfg := loadConfig(t, writeConfig(t, "server.docker.yaml", body))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(primary-only after cleanup) = %v", err)
	}
	profile := cfg.Profiles["default"]
	if profile.Flow != ReviewFlow || profile.Plan.Mode != "code" || profile.Plan.Provider != "" ||
		profile.Reasoner != nil || profile.Synthesize.Mode != "code" || profile.Synthesize.Provider != "" ||
		profile.Agent != nil || len(profile.Propose) != 1 {
		t.Errorf("default profile keeps legacy flow choices: %+v", profile)
	}
	if lanes := laneProviders(profile); lanes != "primary,primary,primary,primary,primary" {
		t.Errorf("lanes = %s", lanes)
	}
	if onboarding := cfg.Profiles["onboarding"]; onboarding.Agent == nil || onboarding.Agent.Provider != "primary" {
		t.Errorf("onboarding agent = %+v, want primary", onboarding.Agent)
	}
}

func requireWarning(t *testing.T, warnings []string, want string) {
	t.Helper()
	for _, warning := range warnings {
		if strings.Contains(warning, want) {
			return
		}
	}
	t.Errorf("warnings %q do not mention %q", warnings, want)
}
