package llm

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// CatalogModel keeps the public ID separate from the internal provider key.
type CatalogModel struct {
	store.Model
	Key               string `json:"-"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	// EffectiveParams are the stored parameters with the platform defaults
	// applied. Config models carry their configured parameters unchanged.
	EffectiveParams store.ModelParams `json:"effective_params"`
	// ParamSources names the layer that supplied each effective parameter
	// (store.ParamSourceModel or store.ParamSourceDefault); parameters set by
	// neither layer are omitted.
	ParamSources map[string]string `json:"param_sources,omitempty"`
}

type modelReader interface {
	ListModels(context.Context) ([]store.Model, error)
	OpenModelCredential(context.Context, string) ([]byte, error)
	GetModelDefaults(context.Context) (*store.ModelDefaults, error)
}

// ClientHolder publishes immutable registries while existing clients remain usable.
type ClientHolder struct{ registry atomic.Pointer[Registry] }

func (h *ClientHolder) Client(name string) (Client, error) {
	return clientFromRegistry(h.registry.Load(), name)
}

func clientFromRegistry(r *Registry, name string) (Client, error) {
	if r == nil {
		return nil, errors.New("llm: provider is not configured")
	}
	if strings.HasPrefix(name, "config:") {
		key := strings.TrimPrefix(name, "config:")
		if model, ok := r.models[key]; !ok || model.ID != name {
			return nil, errors.New("llm: provider is not configured")
		}
		name = key
	}
	return r.Client(name)
}

func (h *ClientHolder) Infos() []ProviderInfo { return h.registry.Load().Infos() }

// CatalogSnapshot retains one immutable catalog generation for a workflow.
type CatalogSnapshot struct{ registry *Registry }

// Snapshot captures clients and model configurations from the same generation.
func (c *Catalog) Snapshot() *CatalogSnapshot {
	return &CatalogSnapshot{registry: c.registry.Load()}
}

func (s *CatalogSnapshot) Client(name string) (Client, error) {
	return clientFromRegistry(s.registry, name)
}

func (s *CatalogSnapshot) Resolve(id string) (CatalogModel, bool) {
	return resolveCatalogModel(s.registry, id)
}

// ProviderConfig returns the model's non-secret provider settings, including
// platform defaults; the credential is never part of the result.
func (m CatalogModel) ProviderConfig() config.ProviderConfig {
	return modelConfigWithParams(m.Model, m.EffectiveParams)
}

// Catalog merges configured providers with database models.
type Catalog struct {
	ClientHolder
	mu               sync.Mutex
	store            modelReader
	configured       *Registry
	options          registryOptions
	defaultsRevision int64
}

func NewCatalog(ctx context.Context, providers map[string]config.ProviderConfig, models modelReader, opts ...RegistryOption) (*Catalog, error) {
	registry, err := NewRegistry(providers, opts...)
	if err != nil {
		return nil, err
	}
	registry.models = make(map[string]CatalogModel, len(providers))
	for name, cfg := range providers {
		info := registry.clients[name].Info()
		params := paramsFromConfig(cfg)
		registry.models[name] = cloneCatalogModel(CatalogModel{Key: name, Model: store.Model{ID: "config:" + name, DisplayName: name, Protocol: cfg.Protocol, BaseURL: cfg.BaseURL, Model: cfg.Model, Params: params, HasAPIKey: true, Unavailable: info.Unavailable}, EffectiveParams: params, UnavailableReason: info.UnavailableReason})
	}
	c := &Catalog{store: models, configured: registry, options: registryOptions{retryBaseDelay: defaultBaseDelay}}
	for _, opt := range opts {
		if opt != nil {
			opt(&c.options)
		}
	}
	c.registry.Store(registry)
	if err := c.Refresh(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func paramsFromConfig(p config.ProviderConfig) store.ModelParams {
	return store.ModelParams{MaxTokens: p.MaxTokens, ContextWindow: p.ContextWindow, Temperature: p.Temperature, Reasoning: p.Reasoning, Thinking: p.Thinking, ForcedToolChoice: p.ForcedToolChoice, StrictRoleAlternation: p.StrictRoleAlternation, MaxConcurrency: p.MaxConcurrency, RPM: p.RPM,
		AllowReasoningEffortNone: p.AllowReasoningEffortNone, PreserveReasoningContent: p.PreserveReasoningContent, OutputConfigEffort: p.OutputConfigEffort, ResponsesParams: p.ResponsesParams}
}

func modelConfig(m store.Model) config.ProviderConfig {
	return modelConfigWithParams(m, m.Params)
}

func modelConfigWithParams(m store.Model, params store.ModelParams) config.ProviderConfig {
	p := params
	return config.ProviderConfig{Protocol: m.Protocol, BaseURL: m.BaseURL, Model: m.Model, MaxTokens: p.MaxTokens, ContextWindow: p.ContextWindow, Temperature: p.Temperature, Reasoning: p.Reasoning, Thinking: p.Thinking, ForcedToolChoice: p.ForcedToolChoice, StrictRoleAlternation: p.StrictRoleAlternation, MaxConcurrency: p.MaxConcurrency, RPM: p.RPM,
		AllowReasoningEffortNone: p.AllowReasoningEffortNone, PreserveReasoningContent: p.PreserveReasoningContent, OutputConfigEffort: p.OutputConfigEffort, ResponsesParams: p.ResponsesParams}
}

// ModelConfig converts a stored model definition into a provider config.
func ModelConfig(m store.Model) config.ProviderConfig { return modelConfig(m) }

// NewDraftClient builds a throwaway client for an unsaved model. It applies the
// catalog's egress policy and HTTP client, is not registered in the catalog,
// and records nothing: the call recorder is deliberately dropped.
func (c *Catalog) NewDraftClient(model store.Model, apiKey string) (Client, func(), error) {
	options := c.options
	options.recorder = nil
	policy, err := newEgressPolicy(options.egress)
	if err != nil {
		return nil, nil, err
	}
	cfg := modelConfig(model)
	if providerUnavailable(cfg.BaseURL, policy) == "egress_denied" {
		return nil, nil, &Error{Kind: ErrEgress, Err: ErrEgressDenied}
	}
	httpClient, err := newEgressHTTPClient(options.httpClient, options.egress)
	if err != nil {
		return nil, nil, err
	}
	client, err := newProviderClientWithKey("draft", cfg, apiKey, httpClient, options)
	if err != nil {
		return nil, nil, err
	}
	return client, httpClient.CloseIdleConnections, nil
}

// Refresh atomically replaces the registry after rebuilding changed models.
func (c *Catalog) Refresh(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var models []store.Model
	var defaults store.ModelParams
	var defaultsRevision int64
	if c.store != nil {
		var err error
		models, err = c.store.ListModels(ctx)
		if err != nil {
			return err
		}
		storedDefaults, err := c.store.GetModelDefaults(ctx)
		if err != nil {
			return err
		}
		if storedDefaults != nil {
			defaults = storedDefaults.Params
			defaultsRevision = storedDefaults.Revision
		}
	}
	// A defaults change invalidates every user-model client because its
	// effective configuration may have changed without a model revision bump.
	defaultsChanged := defaultsRevision != c.defaultsRevision
	policy, err := newEgressPolicy(c.options.egress)
	if err != nil {
		return err
	}
	httpClient, err := newEgressHTTPClient(c.options.httpClient, c.options.egress)
	if err != nil {
		return err
	}
	previous := c.registry.Load()
	next := &Registry{clients: make(map[string]Client), models: make(map[string]CatalogModel)}
	for key, model := range c.configured.models {
		next.models[key] = model
		next.clients[key] = c.configured.clients[key]
	}
	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := cloneCatalogModel(CatalogModel{Model: model, Key: model.ID})
		if strings.HasPrefix(model.ID, "user:") {
			entry.EffectiveParams, entry.ParamSources = store.MergeModelParams(model.Params, defaults)
		} else {
			entry.EffectiveParams = model.Params
		}
		if old, ok := previous.models[model.ID]; ok && !defaultsChanged && old.Revision == model.Revision && !old.Unavailable && !model.Unavailable {
			entry = old
			next.clients[model.ID] = previous.clients[model.ID]
		} else {
			cfg := modelConfigWithParams(model, entry.EffectiveParams)
			switch {
			case !strings.HasPrefix(model.ID, "user:"):
				entry.UnavailableReason = "validation"
			case config.ValidateProviderConfig("model", cfg, config.ProviderValidationOptions{}) != nil:
				entry.UnavailableReason = "validation"
			case model.Unavailable:
				entry.UnavailableReason = "credential"
			default:
				key, keyErr := c.store.OpenModelCredential(ctx, model.ID)
				if keyErr != nil {
					entry.UnavailableReason = "credential"
				} else if reason := providerUnavailable(cfg.BaseURL, policy); reason != "" {
					entry.UnavailableReason = reason
				} else if client, clientErr := newProviderClientWithKey(model.ID, cfg, string(key), httpClient, c.options); clientErr != nil {
					entry.UnavailableReason = "validation"
				} else {
					next.clients[model.ID] = client
				}
			}
			entry.Unavailable = entry.UnavailableReason != ""
		}
		next.models[model.ID] = entry
	}
	for _, client := range next.clients {
		next.infos = append(next.infos, client.Info())
	}
	for _, model := range next.models {
		if model.Unavailable && strings.HasPrefix(model.Key, "user:") {
			next.infos = append(next.infos, ProviderInfo{Name: model.Key, Protocol: model.Protocol, Model: model.Model.Model, BaseURL: model.BaseURL, Unavailable: true, UnavailableReason: model.UnavailableReason})
		}
	}
	sort.Slice(next.infos, func(i, j int) bool { return next.infos[i].Name < next.infos[j].Name })
	if err := ctx.Err(); err != nil {
		return err
	}
	c.registry.Store(next)
	c.defaultsRevision = defaultsRevision
	return nil
}

func (c *Catalog) Resolve(id string) (CatalogModel, bool) {
	if c == nil || c.registry.Load() == nil {
		return CatalogModel{}, false
	}
	return resolveCatalogModel(c.registry.Load(), id)
}

func resolveCatalogModel(r *Registry, id string) (CatalogModel, bool) {
	if r == nil {
		return CatalogModel{}, false
	}
	m, ok := r.models[strings.TrimPrefix(id, "config:")]
	if strings.HasPrefix(id, "config:") && m.ID != id {
		return CatalogModel{}, false
	}
	return cloneCatalogModel(m), ok
}

// Labels maps internal keys to display names for user-facing attribution.
func (c *Catalog) Labels() map[string]string {
	if c == nil || c.registry.Load() == nil {
		return nil
	}
	r := c.registry.Load()
	labels := make(map[string]string, len(r.models))
	for key, model := range r.models {
		if strings.HasPrefix(model.ID, "user:") {
			labels[key] = model.DisplayName
		}
	}
	return labels
}

func (c *Catalog) List() []CatalogModel {
	if c == nil || c.registry.Load() == nil {
		return nil
	}
	r := c.registry.Load()
	result := make([]CatalogModel, 0, len(r.models))
	for _, m := range r.models {
		result = append(result, cloneCatalogModel(m))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func cloneCatalogModel(m CatalogModel) CatalogModel {
	p := &m.Params
	if p.Temperature != nil {
		v := *p.Temperature
		p.Temperature = &v
	}
	if p.Reasoning != nil {
		v := *p.Reasoning
		p.Reasoning = &v
	}
	if p.Thinking != nil {
		v := *p.Thinking
		p.Thinking = &v
	}
	effective := &m.EffectiveParams
	if effective.Temperature != nil {
		v := *effective.Temperature
		effective.Temperature = &v
	}
	if effective.Reasoning != nil {
		v := *effective.Reasoning
		effective.Reasoning = &v
	}
	if effective.Thinking != nil {
		v := *effective.Thinking
		effective.Thinking = &v
	}
	if m.ParamSources != nil {
		sources := make(map[string]string, len(m.ParamSources))
		for name, source := range m.ParamSources {
			sources[name] = source
		}
		m.ParamSources = sources
	}
	if m.DeletedAt != nil {
		v := *m.DeletedAt
		m.DeletedAt = &v
	}
	return m
}
