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
}

type modelReader interface {
	ListModels(context.Context) ([]store.Model, error)
	OpenModelCredential(context.Context, string) ([]byte, error)
}

// ClientHolder publishes immutable registries while existing clients remain usable.
type ClientHolder struct{ registry atomic.Pointer[Registry] }

func (h *ClientHolder) Client(name string) (Client, error) {
	r := h.registry.Load()
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

// Catalog merges configured providers with database models.
type Catalog struct {
	ClientHolder
	mu         sync.Mutex
	store      modelReader
	configured *Registry
	options    registryOptions
}

func NewCatalog(ctx context.Context, providers map[string]config.ProviderConfig, models modelReader, opts ...RegistryOption) (*Catalog, error) {
	registry, err := NewRegistry(providers, opts...)
	if err != nil {
		return nil, err
	}
	registry.models = make(map[string]CatalogModel, len(providers))
	for name, cfg := range providers {
		info := registry.clients[name].Info()
		registry.models[name] = cloneCatalogModel(CatalogModel{Key: name, Model: store.Model{ID: "config:" + name, DisplayName: name, Protocol: cfg.Protocol, BaseURL: cfg.BaseURL, Model: cfg.Model, Params: paramsFromConfig(cfg), HasAPIKey: true, Unavailable: info.Unavailable}, UnavailableReason: info.UnavailableReason})
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
	return store.ModelParams{MaxTokens: p.MaxTokens, ContextWindow: p.ContextWindow, Temperature: p.Temperature, Reasoning: p.Reasoning, Thinking: p.Thinking, ForcedToolChoice: p.ForcedToolChoice, StrictRoleAlternation: p.StrictRoleAlternation, MaxConcurrency: p.MaxConcurrency, RPM: p.RPM, Price: p.Price}
}

func modelConfig(m store.Model) config.ProviderConfig {
	p := m.Params
	return config.ProviderConfig{Protocol: m.Protocol, BaseURL: m.BaseURL, Model: m.Model, MaxTokens: p.MaxTokens, ContextWindow: p.ContextWindow, Temperature: p.Temperature, Reasoning: p.Reasoning, Thinking: p.Thinking, ForcedToolChoice: p.ForcedToolChoice, StrictRoleAlternation: p.StrictRoleAlternation, MaxConcurrency: p.MaxConcurrency, RPM: p.RPM, Price: p.Price}
}

// Refresh atomically replaces the registry after rebuilding changed models.
func (c *Catalog) Refresh(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var models []store.Model
	var err error
	if c.store != nil {
		models, err = c.store.ListModels(ctx)
		if err != nil {
			return err
		}
	}
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
		if old, ok := previous.models[model.ID]; ok && old.Revision == model.Revision && !old.Unavailable && !model.Unavailable {
			entry = old
			next.clients[model.ID] = previous.clients[model.ID]
		} else {
			cfg := modelConfig(model)
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
	return nil
}

func (c *Catalog) Resolve(id string) (CatalogModel, bool) {
	if c == nil || c.registry.Load() == nil {
		return CatalogModel{}, false
	}
	m, ok := c.registry.Load().models[strings.TrimPrefix(id, "config:")]
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
	if p.Price != nil {
		v := *p.Price
		p.Price = &v
	}
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
	if m.DeletedAt != nil {
		v := *m.DeletedAt
		m.DeletedAt = &v
	}
	return m
}
