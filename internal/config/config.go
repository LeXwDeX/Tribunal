// Package config loads and validates the platform configuration described in
// docs/design/06-config.md.
package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Config is the complete platform configuration.
type Config struct {
	Server         ServerConfig              `yaml:"server" json:"server"`
	GitLab         GitLabConfig              `yaml:"gitlab" json:"gitlab"`
	Auth           AuthConfig                `yaml:"auth" json:"auth"`
	Storage        StorageConfig             `yaml:"storage" json:"storage"`
	Secrets        SecretsConfig             `yaml:"secrets" json:"secrets"`
	Workspace      WorkspaceConfig           `yaml:"workspace" json:"workspace"`
	Jobs           JobsConfig                `yaml:"jobs" json:"jobs"`
	Engine         EngineConfig              `yaml:"engine" json:"engine"`
	Providers      map[string]ProviderConfig `yaml:"providers" json:"providers"`
	Profiles       map[string]ProfileConfig  `yaml:"profiles" json:"profiles"`
	MCP            MCPConfig                 `yaml:"mcp" json:"mcp"`
	Skills         SkillsConfig              `yaml:"skills" json:"skills"`
	Blocks         map[string]BlockBinding   `yaml:"blocks" json:"blocks"`
	ReviewDefaults ProjectSettings           `yaml:"review_defaults" json:"review_defaults"`
	PromptsDir     string                    `yaml:"prompts_dir" json:"prompts_dir"`
}

// ServerConfig controls the HTTP listener.
type ServerConfig struct {
	Listen         string        `yaml:"listen" json:"listen"`
	PublicURL      string        `yaml:"public_url" json:"public_url"`
	ReadTimeout    time.Duration `yaml:"read_timeout" json:"read_timeout"`
	WriteTimeout   time.Duration `yaml:"write_timeout" json:"write_timeout"`
	TrustedProxies []string      `yaml:"trusted_proxies" json:"trusted_proxies"`
}

// GitLabConfig controls access to the GitLab instance.
type GitLabConfig struct {
	URL            string        `yaml:"url" json:"url"`
	RequestTimeout time.Duration `yaml:"request_timeout" json:"request_timeout"`
	MaxConcurrency int           `yaml:"max_concurrency" json:"max_concurrency"`
	BotUsernames   []string      `yaml:"bot_usernames" json:"bot_usernames"`
}

// AuthConfig controls login and the M0 bootstrap token.
type AuthConfig struct {
	OAuth             OAuthConfig   `yaml:"oauth" json:"oauth"`
	BootstrapAdmins   []string      `yaml:"bootstrap_admins" json:"bootstrap_admins"`
	SessionTTL        time.Duration `yaml:"session_ttl" json:"session_ttl"`
	BootstrapTokenEnv string        `yaml:"bootstrap_token_env" json:"bootstrap_token_env"`
}

// OAuthConfig names the environment variables for an optional GitLab OAuth app.
type OAuthConfig struct {
	ClientIDEnv     string `yaml:"client_id_env" json:"client_id_env"`
	ClientSecretEnv string `yaml:"client_secret_env" json:"client_secret_env"`
}

// StorageConfig selects the persistence backend and retention windows. Only
// sqlite is supported at runtime in M0.
type StorageConfig struct {
	Driver            string        `yaml:"driver" json:"driver"`
	DSN               string        `yaml:"dsn" json:"dsn"`
	LLMCallRetention  time.Duration `yaml:"llm_call_retention" json:"llm_call_retention"`
	DeliveryRetention time.Duration `yaml:"delivery_retention" json:"delivery_retention"`
	JobRetention      time.Duration `yaml:"job_retention" json:"job_retention"`
	ContextVersions   int           `yaml:"context_versions" json:"context_versions"`
}

// SecretsConfig names the environment variable that holds the base64 master key.
type SecretsConfig struct {
	MasterKeyEnv string `yaml:"master_key_env" json:"master_key_env"`
}

// WorkspaceConfig controls repository checkouts and context refresh.
type WorkspaceConfig struct {
	Root            string               `yaml:"root" json:"root"`
	GitBin          string               `yaml:"git_bin" json:"git_bin"`
	ContextRefresh  ContextRefreshConfig `yaml:"context_refresh" json:"context_refresh"`
	RunCleanupAfter time.Duration        `yaml:"run_cleanup_after" json:"run_cleanup_after"`
}

// ContextRefreshConfig controls context document freshness.
type ContextRefreshConfig struct {
	ChangedFiles int           `yaml:"changed_files" json:"changed_files"`
	MaxAge       time.Duration `yaml:"max_age" json:"max_age"`
}

// JobsConfig controls the persistent worker pool.
type JobsConfig struct {
	Workers int           `yaml:"workers" json:"workers"`
	Lease   time.Duration `yaml:"lease" json:"lease"`
}

// EngineConfig controls workflow node concurrency and timeouts.
type EngineConfig struct {
	MaxConcurrentNodes int           `yaml:"max_concurrent_nodes" json:"max_concurrent_nodes"`
	DefaultNodeTimeout time.Duration `yaml:"default_node_timeout" json:"default_node_timeout"`
}

// ProviderConfig describes one LLM provider. Providers are optional in M0.
type ProviderConfig struct {
	Protocol       string           `yaml:"protocol" json:"protocol"`
	BaseURL        string           `yaml:"base_url" json:"base_url"`
	Model          string           `yaml:"model" json:"model"`
	APIKeyEnv      string           `yaml:"api_key_env" json:"api_key_env"`
	MaxConcurrency int              `yaml:"max_concurrency" json:"max_concurrency"`
	RPM            int              `yaml:"rpm" json:"rpm"`
	MaxTokens      int              `yaml:"max_tokens" json:"max_tokens"`
	Temperature    *float64         `yaml:"temperature" json:"temperature"`
	Thinking       *ThinkingConfig  `yaml:"thinking" json:"thinking"`
	Reasoning      *ReasoningConfig `yaml:"reasoning" json:"reasoning"`
	Price          *PriceConfig     `yaml:"price" json:"price"`
}

// ThinkingConfig carries provider-specific reasoning token settings.
type ThinkingConfig struct {
	BudgetTokens int `yaml:"budget_tokens" json:"budget_tokens"`
}

// ReasoningConfig carries provider-specific reasoning effort.
type ReasoningConfig struct {
	Effort string `yaml:"effort" json:"effort"`
}

// PriceConfig records per-million-token prices for cost statistics.
type PriceConfig struct {
	InputPerMTok  float64 `yaml:"input_per_mtok" json:"input_per_mtok"`
	OutputPerMTok float64 `yaml:"output_per_mtok" json:"output_per_mtok"`
}

// ProfileConfig describes a review profile.
type ProfileConfig struct {
	Plan        PlanConfig       `yaml:"plan" json:"plan"`
	Propose     []ProposeConfig  `yaml:"propose" json:"propose"`
	Reasoner    *ReasonerConfig  `yaml:"reasoner" json:"reasoner"`
	Consensus   *ConsensusConfig `yaml:"consensus" json:"consensus"`
	Challenge   ChallengeConfig  `yaml:"challenge" json:"challenge"`
	OnUncertain string           `yaml:"on_uncertain" json:"on_uncertain"`
	Synthesize  SynthesizeConfig `yaml:"synthesize" json:"synthesize"`
	Budget      BudgetConfig     `yaml:"budget" json:"budget"`
	Agent       *AgentConfig     `yaml:"agent" json:"agent"`
}

// PlanConfig selects the planning mode and optional provider.
type PlanConfig struct {
	Mode          string `yaml:"mode" json:"mode"`
	Provider      string `yaml:"provider" json:"provider"`
	PackageTokens int    `yaml:"package_tokens" json:"package_tokens"`
	MaxPackages   int    `yaml:"max_packages" json:"max_packages"`
}

// ProposeConfig binds a review block to an optional provider.
type ProposeConfig struct {
	Block    string `yaml:"block" json:"block"`
	Provider string `yaml:"provider" json:"provider"`
}

// ReasonerConfig selects the reasoner provider and trigger.
type ReasonerConfig struct {
	Provider string `yaml:"provider" json:"provider"`
	When     string `yaml:"when" json:"when"`
}

// ConsensusConfig controls multi-provider vote weighting.
type ConsensusConfig struct {
	MinVotes        int     `yaml:"min_votes" json:"min_votes"`
	SameModelWeight float64 `yaml:"same_model_weight" json:"same_model_weight"`
}

// ChallengeConfig selects the challenger and its fallback.
type ChallengeConfig struct {
	Provider string   `yaml:"provider" json:"provider"`
	Fallback string   `yaml:"fallback" json:"fallback"`
	Only     []string `yaml:"only" json:"only"`
}

// SynthesizeConfig selects the synthesis mode and optional provider.
type SynthesizeConfig struct {
	Mode     string `yaml:"mode" json:"mode"`
	Provider string `yaml:"provider" json:"provider"`
}

// BudgetConfig limits one workflow.
type BudgetConfig struct {
	MaxNodes   int           `yaml:"max_nodes" json:"max_nodes"`
	MaxExtends int           `yaml:"max_extends" json:"max_extends"`
	Deadline   time.Duration `yaml:"deadline" json:"deadline"`
	MaxTokens  int64         `yaml:"max_tokens" json:"max_tokens"`
}

// AgentConfig selects the provider for agent-mode nodes.
type AgentConfig struct {
	Provider string `yaml:"provider" json:"provider"`
}

// MCPConfig points at the Claude Code-compatible MCP server JSON file.
type MCPConfig struct {
	Config string `yaml:"config" json:"config"`
}

// SkillsConfig lists platform-level skill directories.
type SkillsConfig struct {
	Dirs []string `yaml:"dirs" json:"dirs"`
}

// BlockBinding grants MCP tools and skills to a review block.
type BlockBinding struct {
	MCPTools []string `yaml:"mcp_tools" json:"mcp_tools"`
	Skills   []string `yaml:"skills" json:"skills"`
}

// ProjectSettings holds per-project values and the platform review defaults.
// JSON tags intentionally omit omitempty on booleans so an explicit false
// survives a settings round trip.
type ProjectSettings struct {
	Language             string            `yaml:"language" json:"language"`
	SkipDraft            bool              `yaml:"skip_draft" json:"skip_draft"`
	TargetBranches       []string          `yaml:"target_branches" json:"target_branches"`
	LabelsInclude        []string          `yaml:"labels_include" json:"labels_include"`
	LabelsExclude        []string          `yaml:"labels_exclude" json:"labels_exclude"`
	Ignore               []string          `yaml:"ignore" json:"ignore"`
	MaxFiles             int               `yaml:"max_files" json:"max_files"`
	MaxDiffLines         int               `yaml:"max_diff_lines" json:"max_diff_lines"`
	CommitStatus         bool              `yaml:"commit_status" json:"commit_status"`
	PostFailureNote      bool              `yaml:"post_failure_note" json:"post_failure_note"`
	ShowModelAttribution bool              `yaml:"show_model_attribution" json:"show_model_attribution"`
	AllowRepoConfig      bool              `yaml:"allow_repo_config" json:"allow_repo_config"`
	AllowRepoSkills      bool              `yaml:"allow_repo_skills" json:"allow_repo_skills"`
	ProfileByLabel       map[string]string `yaml:"profile_by_label" json:"profile_by_label"`
	CustomRules          []string          `yaml:"custom_rules" json:"custom_rules"`
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type problems struct {
	errs []error
}

func (p *problems) add(format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf(format, args...))
}

func (p *problems) err() error {
	return errors.Join(p.errs...)
}

// Validate checks required values and environment-backed secrets. It returns
// all problems at once and never includes environment values in the error.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config: nil")
	}
	var p problems

	if c.Server.ReadTimeout <= 0 {
		p.add("server.read_timeout must be positive")
	}
	if c.Server.WriteTimeout <= 0 {
		p.add("server.write_timeout must be positive")
	}
	oauthEnabled := c.Auth.OAuth.ClientIDEnv != "" || c.Auth.OAuth.ClientSecretEnv != ""
	validateURL(&p, "server.public_url", c.Server.PublicURL, oauthEnabled)

	validateURL(&p, "gitlab.url", c.GitLab.URL, true)
	if c.GitLab.RequestTimeout <= 0 {
		p.add("gitlab.request_timeout must be positive")
	}
	if c.GitLab.MaxConcurrency <= 0 {
		p.add("gitlab.max_concurrency must be positive")
	}

	if c.Auth.SessionTTL <= 0 {
		p.add("auth.session_ttl must be positive")
	}
	bootstrap, ok := p.env("auth.bootstrap_token_env", c.Auth.BootstrapTokenEnv)
	if ok && len(bootstrap) < 32 {
		p.add("auth.bootstrap_token_env: environment variable %s must be at least 32 bytes", c.Auth.BootstrapTokenEnv)
	}
	validateOAuth(&p, c.Auth.OAuth)

	switch c.Storage.Driver {
	case "sqlite":
	case "postgres":
		p.add(`storage.driver "postgres" is not supported at runtime; use sqlite`)
	case "":
		p.add("storage.driver must be sqlite")
	default:
		p.add("storage.driver %q is not supported; use sqlite", c.Storage.Driver)
	}
	if c.Storage.DSN == "" {
		p.add("storage.dsn must not be empty")
	}
	if c.Storage.LLMCallRetention <= 0 {
		p.add("storage.llm_call_retention must be positive")
	}
	if c.Storage.DeliveryRetention <= 0 {
		p.add("storage.delivery_retention must be positive")
	}
	if c.Storage.JobRetention <= 0 {
		p.add("storage.job_retention must be positive")
	}
	if c.Storage.ContextVersions <= 0 {
		p.add("storage.context_versions must be positive")
	}

	masterKey, ok := p.env("secrets.master_key_env", c.Secrets.MasterKeyEnv)
	if ok {
		raw, err := base64.StdEncoding.DecodeString(masterKey)
		if err != nil {
			raw, err = base64.RawStdEncoding.DecodeString(masterKey)
		}
		if err != nil || len(raw) != 32 {
			p.add("secrets.master_key_env: environment variable %s must be base64 of exactly 32 bytes", c.Secrets.MasterKeyEnv)
		}
	}

	if c.Workspace.ContextRefresh.MaxAge <= 0 {
		p.add("workspace.context_refresh.max_age must be positive")
	}
	if c.Workspace.RunCleanupAfter <= 0 {
		p.add("workspace.run_cleanup_after must be positive")
	}
	if c.Jobs.Workers <= 0 {
		p.add("jobs.workers must be positive")
	}
	if c.Jobs.Lease <= 0 {
		p.add("jobs.lease must be positive")
	}
	if c.Engine.MaxConcurrentNodes <= 0 {
		p.add("engine.max_concurrent_nodes must be positive")
	}
	if c.Engine.DefaultNodeTimeout <= 0 {
		p.add("engine.default_node_timeout must be positive")
	}

	for _, name := range sortedKeys(c.Providers) {
		provider := c.Providers[name]
		field := "providers." + name
		if !isProviderProtocol(provider.Protocol) {
			p.add("%s.protocol %q is not supported", field, provider.Protocol)
		}
		validateURL(&p, field+".base_url", provider.BaseURL, false)
		if provider.MaxConcurrency <= 0 {
			p.add("%s.max_concurrency must be positive", field)
		}
		if provider.APIKeyEnv == "" {
			p.add("%s.api_key_env must be set", field)
		} else {
			p.env(field+".api_key_env", provider.APIKeyEnv)
		}
	}

	for _, name := range sortedKeys(c.Profiles) {
		validateProfile(&p, c.Providers, "profiles."+name, c.Profiles[name])
	}
	validateMCP(&p, c)

	return p.err()
}

func validateOAuth(p *problems, oauth OAuthConfig) {
	if oauth.ClientIDEnv == "" && oauth.ClientSecretEnv == "" {
		return
	}
	if oauth.ClientIDEnv == "" || oauth.ClientSecretEnv == "" {
		p.add("auth.oauth.client_id_env and client_secret_env must be set together")
		return
	}
	p.env("auth.oauth.client_id_env", oauth.ClientIDEnv)
	p.env("auth.oauth.client_secret_env", oauth.ClientSecretEnv)
}

func validateProfile(p *problems, providers map[string]ProviderConfig, field string, profile ProfileConfig) {
	checkProvider := func(refField, name string) {
		if name == "" {
			return
		}
		if _, ok := providers[name]; !ok {
			p.add("%s %q is not defined", refField, name)
		}
	}

	checkProvider(field+".plan.provider", profile.Plan.Provider)
	for i, propose := range profile.Propose {
		checkProvider(fmt.Sprintf("%s.propose[%d].provider", field, i), propose.Provider)
	}
	if profile.Reasoner != nil {
		checkProvider(field+".reasoner.provider", profile.Reasoner.Provider)
	}
	if profile.Challenge.Provider == "cross" {
		if profile.Challenge.Fallback == "" {
			p.add("%s.challenge.fallback is required when challenge.provider is cross", field)
		}
	} else {
		checkProvider(field+".challenge.provider", profile.Challenge.Provider)
	}
	checkProvider(field+".challenge.fallback", profile.Challenge.Fallback)
	checkProvider(field+".synthesize.provider", profile.Synthesize.Provider)
	if profile.Agent != nil {
		checkProvider(field+".agent.provider", profile.Agent.Provider)
	}

	if !isUncertainPolicy(profile.OnUncertain) {
		p.add("%s.on_uncertain must be one of drop, mark, judge", field)
	}
	if profile.OnUncertain == "judge" && profile.Synthesize.Mode != "agent" {
		p.add("%s.on_uncertain %q requires synthesize.mode %q", field, "judge", "agent")
	}
	if profile.Budget.MaxNodes < 1 || profile.Budget.MaxNodes > 100 {
		p.add("%s.budget.max_nodes must be between 1 and 100", field)
	}
	if profile.Budget.MaxExtends < 0 || profile.Budget.MaxExtends > 2 {
		p.add("%s.budget.max_extends must be between 0 and 2", field)
	}
	if profile.Budget.Deadline <= 0 {
		p.add("%s.budget.deadline must be positive", field)
	}
}

func validateURL(p *problems, field, raw string, required bool) {
	if raw == "" {
		if required {
			p.add("%s is required", field)
		}
		return
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		p.add("%s must be an absolute http(s) URL without credentials, query, or fragment", field)
		return
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || strings.Contains(raw, "#") {
		p.add("%s must be an absolute http(s) URL without credentials, query, or fragment", field)
	}
}

func (p *problems) env(field, name string) (string, bool) {
	if name == "" {
		p.add("%s must be set", field)
		return "", false
	}
	if !envNameRE.MatchString(name) {
		p.add("%s is not a valid environment variable name", field)
		return "", false
	}
	value, ok := os.LookupEnv(name)
	if !ok || value == "" {
		p.add("%s: environment variable %s is not set or empty", field, name)
		return "", false
	}
	return value, true
}

type bindingRef struct {
	field string
	value string
}

func validateMCP(p *problems, cfg *Config) {
	var bindings []bindingRef
	for _, block := range sortedKeys(cfg.Blocks) {
		for i, tool := range cfg.Blocks[block].MCPTools {
			bindings = append(bindings, bindingRef{
				field: fmt.Sprintf("blocks.%s.mcp_tools[%d]", block, i),
				value: tool,
			})
		}
	}
	if len(bindings) == 0 {
		return
	}
	if cfg.MCP.Config == "" {
		p.add("mcp.config is required when blocks.mcp_tools is set")
		return
	}
	data, err := os.ReadFile(cfg.MCP.Config)
	if err != nil {
		p.add("mcp.config: cannot read %s", cfg.MCP.Config)
		return
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		p.add("mcp.config: invalid JSON in %s", cfg.MCP.Config)
		return
	}
	for _, binding := range bindings {
		server, _, ok := parseMCPTool(binding.value)
		if !ok {
			p.add("%s %q must use the form mcp__<server>__<tool>", binding.field, binding.value)
			continue
		}
		if _, found := doc.MCPServers[server]; !found {
			p.add("%s %q: MCP server %q is not defined in mcp.config", binding.field, binding.value, server)
		}
	}
}

func parseMCPTool(value string) (server, tool string, ok bool) {
	rest, found := strings.CutPrefix(value, "mcp__")
	if !found {
		return "", "", false
	}
	server, tool, found = strings.Cut(rest, "__")
	if !found || server == "" || tool == "" {
		return "", "", false
	}
	return server, tool, true
}

func isProviderProtocol(value string) bool {
	switch value {
	case "anthropic", "openai-chat", "openai-responses":
		return true
	default:
		return false
	}
}

func isUncertainPolicy(value string) bool {
	switch value {
	case "drop", "mark", "judge":
		return true
	default:
		return false
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
