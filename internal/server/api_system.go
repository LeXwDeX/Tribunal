package server

import (
	"net/http"
	"sort"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

type providerStatsView struct {
	Calls        int64    `json:"calls"`
	Errors       int64    `json:"errors"`
	ErrorRate    *float64 `json:"error_rate"`
	P50LatencyMS *float64 `json:"p50_latency_ms"`
}

type providerView struct {
	Name           string            `json:"name"`
	Protocol       string            `json:"protocol"`
	BaseURL        string            `json:"base_url"`
	Model          string            `json:"model"`
	MaxConcurrency int               `json:"max_concurrency"`
	RPM            int               `json:"rpm"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
	Stats24h       providerStatsView `json:"stats_24h"`
}

type providerTestResponse struct {
	OK        bool   `json:"ok"`
	Message   string `json:"message,omitempty"`
	Model     string `json:"model,omitempty"`
	LatencyMS *int64 `json:"latency_ms,omitempty"`
}

type profileView struct {
	Name   string               `json:"name"`
	Config config.ProfileConfig `json:"config"`
}

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	if !s.requireAccess(w, r) {
		return
	}
	now := s.now()
	// Provider usage is still business data. Scope it to the caller's grants
	// even for platform administrators; the static bearer (ID 0) sees none.
	stats, err := s.store.Stats(r.Context(), store.StatsFilter{
		From:         now.Add(-24 * time.Hour),
		To:           now,
		AccessUserID: accessUserIDPtr(currentUserID(r)),
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	aggregated := aggregateProviderStats(stats.ByModel)
	providers := make([]providerView, 0)
	if s.platform != nil {
		for _, name := range sortedProviderNames(s.platform.Providers) {
			provider := s.platform.Providers[name]
			providers = append(providers, providerView{
				Name:           name,
				Protocol:       provider.Protocol,
				BaseURL:        provider.BaseURL,
				Model:          provider.Model,
				MaxConcurrency: provider.MaxConcurrency,
				RPM:            provider.RPM,
				MaxTokens:      provider.MaxTokens,
				Stats24h:       aggregated[name],
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
}

func sortedProviderNames(providers map[string]config.ProviderConfig) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// aggregateProviderStats folds per-model audit rows into per-provider counts.
// The store aggregates latency per provider+model, so the provider P50 uses
// its busiest model. A provider with no calls keeps nil ratio and latency
// instead of reporting a fake zero.
func aggregateProviderStats(models []store.ModelStats) map[string]providerStatsView {
	out := make(map[string]providerStatsView, len(models))
	busiest := make(map[string]int64, len(models))
	for _, model := range models {
		entry := out[model.Provider]
		entry.Calls += model.Calls
		entry.Errors += model.Errors
		if model.P50LatencyMS != nil && model.Calls > busiest[model.Provider] {
			busiest[model.Provider] = model.Calls
			entry.P50LatencyMS = model.P50LatencyMS
		}
		out[model.Provider] = entry
	}
	for name, entry := range out {
		if entry.Calls > 0 {
			rate := clamp01(float64(entry.Errors) / float64(entry.Calls))
			entry.ErrorRate = &rate
		}
		out[name] = entry
	}
	return out
}

func clamp01(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.platform == nil {
		writeError(w, r, http.StatusNotFound, "not_found", "provider not found", nil)
		return
	}
	if _, ok := s.platform.Providers[name]; !ok {
		writeError(w, r, http.StatusNotFound, "not_found", "provider not found", nil)
		return
	}
	if s.llm == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "provider tester is not configured", nil)
		return
	}
	result, err := s.llm.TestProvider(r.Context(), name)
	if err != nil {
		// The error text can contain a base URL or provider detail; log only
		// the provider name and return a generic message with ok:false.
		s.logger.Warn("provider connectivity test failed",
			"request_id", requestID(r.Context()), "provider", name)
		writeJSON(w, http.StatusOK, providerTestResponse{OK: false, Message: "connectivity test failed"})
		return
	}
	latency := result.LatencyMS
	writeJSON(w, http.StatusOK, providerTestResponse{
		OK:        true,
		Model:     result.Model,
		LatencyMS: &latency,
	})
}

func (s *Server) handleListProfiles(w http.ResponseWriter, r *http.Request) {
	profiles := make([]profileView, 0)
	if s.platform != nil {
		names := make([]string, 0, len(s.platform.Profiles))
		for name := range s.platform.Profiles {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			profiles = append(profiles, profileView{Name: name, Config: s.platform.Profiles[name]})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": profiles})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	if !s.requireAccess(w, r) {
		return
	}
	from, err := queryTime(r, "from")
	if err != nil {
		validationFailed(w, r, "from", err.Error())
		return
	}
	to, err := queryTime(r, "to")
	if err != nil {
		validationFailed(w, r, "to", err.Error())
		return
	}
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		validationFailed(w, r, "from", "from must be before to")
		return
	}
	projectID, err := queryInt64(r, "project_id")
	if err != nil {
		validationFailed(w, r, "project_id", err.Error())
		return
	}
	if projectID != nil {
		if _, ok := s.requireProjectAccess(w, r, *projectID, false); !ok {
			return
		}
	}
	stats, err := s.store.Stats(r.Context(), store.StatsFilter{
		From:         from,
		To:           to,
		ProjectID:    projectID,
		AccessUserID: accessUserIDPtr(currentUserID(r)),
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if stats.ByProject == nil {
		stats.ByProject = []store.ProjectStats{}
	}
	for i := range stats.ByProject {
		if stats.ByProject[i].Trend == nil {
			stats.ByProject[i].Trend = []store.DailyStats{}
		}
	}
	if stats.ByModel == nil {
		stats.ByModel = []store.ModelStats{}
	}
	if stats.ByProvider == nil {
		stats.ByProvider = []store.ProviderStats{}
	}
	for i := range stats.ByProvider {
		if stats.ByProvider[i].Trend == nil {
			stats.ByProvider[i].Trend = []store.DailyStats{}
		}
	}
	if stats.ByBlock == nil {
		stats.ByBlock = []store.BlockStats{}
	}
	if stats.QualityByProvider == nil {
		stats.QualityByProvider = []store.ProviderQualityStats{}
	}
	for i := range stats.ByBlock {
		if stats.ByBlock[i].Trend == nil {
			stats.ByBlock[i].Trend = []store.DailyStats{}
		}
	}
	if stats.Trend == nil {
		stats.Trend = []store.DailyStats{}
	}
	if stats.Decisions == nil {
		stats.Decisions = map[string]int64{}
	}
	if stats.Feedback != nil && stats.Feedback.BySignal == nil {
		stats.Feedback.BySignal = map[string]int64{}
	}
	if stats.Feedback != nil && stats.Feedback.ByProvider == nil {
		stats.Feedback.ByProvider = []store.FeedbackProviderStats{}
	}
	writeJSON(w, http.StatusOK, stats)
}

type settingsSecretsView struct {
	MasterKeyConfigured bool `json:"master_key_configured"`
}

type settingsServerView struct {
	Listen         string        `json:"listen"`
	PublicURL      string        `json:"public_url"`
	ReadTimeout    time.Duration `json:"read_timeout"`
	WriteTimeout   time.Duration `json:"write_timeout"`
	TrustedProxies []string      `json:"trusted_proxies"`
}

type settingsGitLabView struct {
	URL            string        `json:"url"`
	RequestTimeout time.Duration `json:"request_timeout"`
	MaxConcurrency int           `json:"max_concurrency"`
	BotUsernames   []string      `json:"bot_usernames"`
}

type settingsAuthView struct {
	BootstrapAdmins []string      `json:"bootstrap_admins"`
	SessionTTL      time.Duration `json:"session_ttl"`
	OAuthConfigured bool          `json:"oauth_configured"`
}

type settingsStorageView struct {
	Driver           string        `json:"driver"`
	LLMCallRetention time.Duration `json:"llm_call_retention"`
}

type settingsWorkspaceView struct {
	ContextRefresh  config.ContextRefreshConfig `json:"context_refresh"`
	RunCleanupAfter time.Duration               `json:"run_cleanup_after"`
}

type settingsJobsView struct {
	Workers int           `json:"workers"`
	Lease   time.Duration `json:"lease"`
}

type settingsEngineView struct {
	MaxConcurrentNodes int           `json:"max_concurrent_nodes"`
	DefaultNodeTimeout time.Duration `json:"default_node_timeout"`
}

type settingsProviderView struct {
	Name             string `json:"name"`
	Protocol         string `json:"protocol"`
	BaseURL          string `json:"base_url"`
	Model            string `json:"model"`
	APIKeyConfigured bool   `json:"api_key_configured"`
	MaxConcurrency   int    `json:"max_concurrency"`
	RPM              int    `json:"rpm"`
}

type settingsMCPView struct {
	Configured bool `json:"configured"`
}

type settingsSkillsView struct {
	Dirs int `json:"dirs"`
}

type settingsView struct {
	Server         settingsServerView             `json:"server"`
	GitLab         settingsGitLabView             `json:"gitlab"`
	Auth           settingsAuthView               `json:"auth"`
	Storage        settingsStorageView            `json:"storage"`
	Secrets        settingsSecretsView            `json:"secrets"`
	Workspace      settingsWorkspaceView          `json:"workspace"`
	Jobs           settingsJobsView               `json:"jobs"`
	Engine         settingsEngineView             `json:"engine"`
	Providers      []settingsProviderView         `json:"providers"`
	Profiles       []string                       `json:"profiles"`
	MCP            settingsMCPView                `json:"mcp"`
	Skills         settingsSkillsView             `json:"skills"`
	Blocks         map[string]config.BlockBinding `json:"blocks"`
	ReviewDefaults config.ProjectSettings         `json:"review_defaults"`
	PromptsDir     string                         `json:"prompts_dir,omitempty"`
}

// handleSettings returns an allowlisted, read-only configuration view. API
// keys, environment variable names, DSNs, workspace root paths and the master
// key value are never included; only "configured" booleans remain.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if s.platform == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	cfg := s.platform
	providers := make([]settingsProviderView, 0, len(cfg.Providers))
	for _, name := range sortedProviderNames(cfg.Providers) {
		provider := cfg.Providers[name]
		providers = append(providers, settingsProviderView{
			Name:             name,
			Protocol:         provider.Protocol,
			BaseURL:          provider.BaseURL,
			Model:            provider.Model,
			APIKeyConfigured: provider.APIKeyEnv != "",
			MaxConcurrency:   provider.MaxConcurrency,
			RPM:              provider.RPM,
		})
	}
	profiles := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		profiles = append(profiles, name)
	}
	sort.Strings(profiles)
	blocks := cfg.Blocks
	if blocks == nil {
		blocks = map[string]config.BlockBinding{}
	}
	view := settingsView{
		Server: settingsServerView{
			Listen:         cfg.Server.Listen,
			PublicURL:      cfg.Server.PublicURL,
			ReadTimeout:    cfg.Server.ReadTimeout,
			WriteTimeout:   cfg.Server.WriteTimeout,
			TrustedProxies: cfg.Server.TrustedProxies,
		},
		GitLab: settingsGitLabView{
			URL:            cfg.GitLab.URL,
			RequestTimeout: cfg.GitLab.RequestTimeout,
			MaxConcurrency: cfg.GitLab.MaxConcurrency,
			BotUsernames:   cfg.GitLab.BotUsernames,
		},
		Auth: settingsAuthView{
			BootstrapAdmins: cfg.Auth.BootstrapAdmins,
			SessionTTL:      cfg.Auth.SessionTTL,
			OAuthConfigured: cfg.Auth.OAuth.ClientIDEnv != "" && cfg.Auth.OAuth.ClientSecretEnv != "",
		},
		Storage: settingsStorageView{
			Driver:           cfg.Storage.Driver,
			LLMCallRetention: cfg.Storage.LLMCallRetention,
		},
		Secrets: settingsSecretsView{
			MasterKeyConfigured: cfg.Secrets.MasterKeyEnv != "",
		},
		Workspace: settingsWorkspaceView{
			ContextRefresh:  cfg.Workspace.ContextRefresh,
			RunCleanupAfter: cfg.Workspace.RunCleanupAfter,
		},
		Jobs: settingsJobsView{
			Workers: cfg.Jobs.Workers,
			Lease:   cfg.Jobs.Lease,
		},
		Engine: settingsEngineView{
			MaxConcurrentNodes: cfg.Engine.MaxConcurrentNodes,
			DefaultNodeTimeout: cfg.Engine.DefaultNodeTimeout,
		},
		Providers:      providers,
		Profiles:       profiles,
		MCP:            settingsMCPView{Configured: cfg.MCP.Config != ""},
		Skills:         settingsSkillsView{Dirs: len(cfg.Skills.Dirs)},
		Blocks:         blocks,
		ReviewDefaults: cfg.ReviewDefaults,
		PromptsDir:     cfg.PromptsDir,
	}
	writeJSON(w, http.StatusOK, view)
}
