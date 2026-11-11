package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
)

// configEffectiveView is an allowlisted, file-effective configuration view.
// It never contains absolute paths, DSNs, environment variable names, or
// environment values.
type configEffectiveView struct {
	Server         config.ServerConfig             `json:"server"`
	GitLab         config.GitLabConfig             `json:"gitlab"`
	Auth           configEffectiveAuthView         `json:"auth"`
	Storage        configEffectiveStorageView      `json:"storage"`
	Secrets        configEffectiveSecretsView      `json:"secrets"`
	Workspace      configEffectiveWorkspaceView    `json:"workspace"`
	Jobs           config.JobsConfig               `json:"jobs"`
	Engine         config.EngineConfig             `json:"engine"`
	LLM            config.LLMConfig                `json:"llm"`
	Providers      []configEffectiveProviderView   `json:"providers"`
	Profiles       map[string]config.ProfileConfig `json:"profiles"`
	MCP            configEffectiveMCPView          `json:"mcp"`
	Skills         configEffectiveSkillsView       `json:"skills"`
	Blocks         map[string]config.BlockBinding  `json:"blocks"`
	ReviewDefaults config.ProjectSettings          `json:"review_defaults"`
}

type configEffectiveAuthView struct {
	BootstrapAdmins []string      `json:"bootstrap_admins"`
	SessionTTL      time.Duration `json:"session_ttl"`
	OAuthConfigured bool          `json:"oauth_configured"`
}

type configEffectiveStorageView struct {
	Driver            string        `json:"driver"`
	LLMCallRetention  time.Duration `json:"llm_call_retention"`
	DeliveryRetention time.Duration `json:"delivery_retention"`
	JobRetention      time.Duration `json:"job_retention"`
	ContextVersions   int           `json:"context_versions"`
}

type configEffectiveSecretsView struct {
	MasterKeyConfigured bool `json:"master_key_configured"`
}

type configEffectiveWorkspaceView struct {
	ContextRefresh  config.ContextRefreshConfig `json:"context_refresh"`
	RunCleanupAfter time.Duration               `json:"run_cleanup_after"`
}

type configEffectiveMCPView struct {
	Configured bool `json:"configured"`
}

type configEffectiveSkillsView struct {
	Dirs int `json:"dirs"`
}

// configEffectiveProviderView mirrors ProviderConfig without api_key_env.
type configEffectiveProviderView struct {
	Name                     string                  `json:"name"`
	Protocol                 string                  `json:"protocol"`
	BaseURL                  string                  `json:"base_url"`
	Model                    string                  `json:"model"`
	APIKeyConfigured         bool                    `json:"api_key_configured"`
	MaxConcurrency           int                     `json:"max_concurrency"`
	RPM                      int                     `json:"rpm"`
	MaxTokens                int                     `json:"max_tokens"`
	ContextWindow            int                     `json:"context_window,omitempty"`
	ForcedToolChoice         string                  `json:"forced_tool_choice,omitempty"`
	Temperature              *float64                `json:"temperature,omitempty"`
	Thinking                 *config.ThinkingConfig  `json:"thinking,omitempty"`
	Reasoning                *config.ReasoningConfig `json:"reasoning,omitempty"`
	Price                    *config.PriceConfig     `json:"price,omitempty"`
	StrictRoleAlternation    bool                    `json:"strict_role_alternation,omitempty"`
	AllowReasoningEffortNone bool                    `json:"allow_reasoning_effort_none,omitempty"`
	PreserveReasoningContent bool                    `json:"preserve_reasoning_content,omitempty"`
	OutputConfigEffort       bool                    `json:"output_config_effort,omitempty"`
	ResponsesParams          string                  `json:"responses_params,omitempty"`
}

type configFileResponse struct {
	Config          configEffectiveView `json:"config"`
	Origins         map[string]string   `json:"origins"`
	Warnings        []string            `json:"warnings"`
	BaseRevision    string              `json:"base_revision"`
	OverlayRevision string              `json:"overlay_revision"`
	OverlayPresent  bool                `json:"overlay_present"`
	Applied         bool                `json:"applied"`
	RestartRequired bool                `json:"restart_required"`
	LastError       string              `json:"last_error"`
	LastErrorAt     *time.Time          `json:"last_error_at"`
}

type configStatusResponse struct {
	Applied         bool       `json:"applied"`
	RestartRequired bool       `json:"restart_required"`
	BaseRevision    string     `json:"base_revision"`
	OverlayRevision string     `json:"overlay_revision"`
	OverlayPresent  bool       `json:"overlay_present"`
	Warnings        []string   `json:"warnings"`
	LastError       string     `json:"last_error"`
	LastErrorAt     *time.Time `json:"last_error_at"`
	UpdatedAt       *time.Time `json:"updated_at"`
}

// handleConfig returns the file-effective values, their origins, and whether
// the running process already uses them.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	state, ok := s.requireConfigState(w, r)
	if !ok {
		return
	}
	snapshot := state.Snapshot
	applied := s.configApplied(snapshot)
	view := configFileResponse{
		Config:          effectiveConfigView(snapshot.Config),
		Origins:         visibleOrigins(snapshot.Origins),
		Warnings:        stringSlice(snapshot.Warnings),
		BaseRevision:    snapshot.BaseRevision,
		OverlayRevision: snapshot.OverlayRevision,
		OverlayPresent:  snapshot.OverlayPresent,
		Applied:         applied,
		RestartRequired: !applied,
		LastError:       state.LastError,
	}
	if state.LastError != "" {
		at := state.LastErrorAt
		view.LastErrorAt = &at
	}
	writeJSON(w, http.StatusOK, view)
}

// handleConfigStatus returns the last-good revisions and the latest reload
// outcome without dumping configuration values.
func (s *Server) handleConfigStatus(w http.ResponseWriter, r *http.Request) {
	state, ok := s.requireConfigState(w, r)
	if !ok {
		return
	}
	snapshot := state.Snapshot
	applied := s.configApplied(snapshot)
	view := configStatusResponse{
		Applied:         applied,
		RestartRequired: !applied,
		BaseRevision:    snapshot.BaseRevision,
		OverlayRevision: snapshot.OverlayRevision,
		OverlayPresent:  snapshot.OverlayPresent,
		Warnings:        stringSlice(snapshot.Warnings),
		LastError:       state.LastError,
	}
	if state.LastError != "" {
		at := state.LastErrorAt
		view.LastErrorAt = &at
	}
	if !state.UpdatedAt.IsZero() {
		at := state.UpdatedAt
		view.UpdatedAt = &at
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) requireConfigState(w http.ResponseWriter, r *http.Request) (*config.State, bool) {
	state := s.fileConfig.Load()
	if state == nil || state.Snapshot == nil || state.Snapshot.Config == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "configuration snapshot is not available", nil)
		return nil, false
	}
	return state, true
}

// configApplied reports whether the display snapshot matches the running
// configuration. A server without a wired runtime config has nothing to apply.
func (s *Server) configApplied(snapshot *config.Snapshot) bool {
	if s.platform == nil {
		return true
	}
	return config.EqualRuntime(snapshot.Config, s.platform)
}

func effectiveConfigView(cfg *config.Config) configEffectiveView {
	profiles := cfg.Profiles
	if profiles == nil {
		profiles = map[string]config.ProfileConfig{}
	}
	blocks := cfg.Blocks
	if blocks == nil {
		blocks = map[string]config.BlockBinding{}
	}
	providers := make([]configEffectiveProviderView, 0, len(cfg.Providers))
	for _, name := range sortedProviderNames(cfg.Providers) {
		provider := cfg.Providers[name]
		providers = append(providers, configEffectiveProviderView{
			Name:                     name,
			Protocol:                 provider.Protocol,
			BaseURL:                  provider.BaseURL,
			Model:                    provider.Model,
			APIKeyConfigured:         provider.APIKeyEnv != "",
			MaxConcurrency:           provider.MaxConcurrency,
			RPM:                      provider.RPM,
			MaxTokens:                provider.MaxTokens,
			ContextWindow:            provider.ContextWindow,
			ForcedToolChoice:         provider.ForcedToolChoice,
			Temperature:              provider.Temperature,
			Thinking:                 provider.Thinking,
			Reasoning:                provider.Reasoning,
			Price:                    provider.Price,
			StrictRoleAlternation:    provider.StrictRoleAlternation,
			AllowReasoningEffortNone: provider.AllowReasoningEffortNone,
			PreserveReasoningContent: provider.PreserveReasoningContent,
			OutputConfigEffort:       provider.OutputConfigEffort,
			ResponsesParams:          provider.ResponsesParams,
		})
	}
	return configEffectiveView{
		Server: cfg.Server,
		GitLab: cfg.GitLab,
		Auth: configEffectiveAuthView{
			BootstrapAdmins: cfg.Auth.BootstrapAdmins,
			SessionTTL:      cfg.Auth.SessionTTL,
			OAuthConfigured: cfg.Auth.OAuth.ClientIDEnv != "" && cfg.Auth.OAuth.ClientSecretEnv != "",
		},
		Storage: configEffectiveStorageView{
			Driver:            cfg.Storage.Driver,
			LLMCallRetention:  cfg.Storage.LLMCallRetention,
			DeliveryRetention: cfg.Storage.DeliveryRetention,
			JobRetention:      cfg.Storage.JobRetention,
			ContextVersions:   cfg.Storage.ContextVersions,
		},
		Secrets: configEffectiveSecretsView{MasterKeyConfigured: cfg.Secrets.MasterKeyEnv != ""},
		Workspace: configEffectiveWorkspaceView{
			ContextRefresh:  cfg.Workspace.ContextRefresh,
			RunCleanupAfter: cfg.Workspace.RunCleanupAfter,
		},
		Jobs:           cfg.Jobs,
		Engine:         cfg.Engine,
		LLM:            cfg.LLM,
		Providers:      providers,
		Profiles:       profiles,
		MCP:            configEffectiveMCPView{Configured: cfg.MCP.Config != ""},
		Skills:         configEffectiveSkillsView{Dirs: len(cfg.Skills.Dirs)},
		Blocks:         blocks,
		ReviewDefaults: cfg.ReviewDefaults,
	}
}

// visibleOrigins drops paths whose field values are not part of the
// effective view. Origin keys themselves are not secret, but excluding them
// keeps the response free of environment variable names and internal paths.
func visibleOrigins(origins map[string]config.Origin) map[string]string {
	visible := make(map[string]string, len(origins))
	for path, origin := range origins {
		if internalConfigPath(path) {
			continue
		}
		visible[path] = string(origin)
	}
	return visible
}

func internalConfigPath(path string) bool {
	switch path {
	case "storage.dsn", "workspace.root", "workspace.git_bin",
		"secrets.master_key_env", "auth.bootstrap_token_env",
		"auth.oauth.client_id_env", "auth.oauth.client_secret_env",
		"mcp.config", "prompts_dir":
		return true
	}
	if strings.HasPrefix(path, "skills.dirs") || strings.HasPrefix(path, "default_extensions.") {
		return true
	}
	return strings.HasPrefix(path, "providers.") && strings.HasSuffix(path, ".api_key_env")
}

// stringSlice renders nil as a JSON array instead of null.
func stringSlice(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
