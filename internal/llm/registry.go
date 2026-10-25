package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"golang.org/x/time/rate"
)

const (
	maxRetries         = 4
	defaultBaseDelay   = 250 * time.Millisecond
	defaultConcurrency = 8
	maxRetryDelay      = 60 * time.Second
)

// CallMeta identifies the workflow/node/turn that caused a model call.
type CallMeta struct {
	WorkflowID string `json:"workflow_id,omitempty"`
	NodeID     string `json:"node_id,omitempty"`
	Turn       int    `json:"turn"`
}

type callMetaKey struct{}

// WithCallMeta attaches audit metadata to a context.
func WithCallMeta(ctx context.Context, meta CallMeta) context.Context {
	return context.WithValue(ctx, callMetaKey{}, meta)
}

// CallMetaFromContext returns audit metadata attached by WithCallMeta.
func CallMetaFromContext(ctx context.Context) CallMeta {
	meta, _ := ctx.Value(callMetaKey{}).(CallMeta)
	return meta
}

// CallRecord is one provider attempt. Request and Response are not copied;
// the recorder must not mutate them.
type CallRecord struct {
	Provider string
	Protocol string
	Model    string
	Meta     CallMeta
	Attempt  int
	Request  *Request
	Response *Response
	Err      error
	Duration time.Duration
}

// Recorder persists or logs call records. The store implementation is owned
// by the caller; llm never imports store.
type Recorder interface {
	RecordCall(ctx context.Context, call CallRecord)
}

// RecorderFunc adapts a function to Recorder.
type RecorderFunc func(ctx context.Context, call CallRecord)

// RecordCall implements Recorder.
func (f RecorderFunc) RecordCall(ctx context.Context, call CallRecord) {
	if f != nil {
		f(ctx, call)
	}
}

// RegistryOption configures a Registry.
type RegistryOption func(*registryOptions)

type registryOptions struct {
	httpClient     *http.Client
	recorder       Recorder
	retryBaseDelay time.Duration
}

// WithHTTPClient injects the HTTP client used by all adapters. Callers may
// pass a test client; redirects across origins are still rejected.
func WithHTTPClient(client *http.Client) RegistryOption {
	return func(o *registryOptions) { o.httpClient = client }
}

// WithRecorder injects a call recorder.
func WithRecorder(recorder Recorder) RegistryOption {
	return func(o *registryOptions) { o.recorder = recorder }
}

// WithRetryBaseDelay overrides the exponential backoff base (tests).
func WithRetryBaseDelay(delay time.Duration) RegistryOption {
	return func(o *registryOptions) {
		if delay > 0 {
			o.retryBaseDelay = delay
		}
	}
}

// Registry builds and owns one client per configured provider.
type Registry struct {
	clients map[string]Client
	infos   []ProviderInfo
}

// NewRegistry builds clients for every configured provider. Credentials are
// read only from ProviderConfig.APIKeyEnv; error messages never include the
// value.
func NewRegistry(providers map[string]config.ProviderConfig, opts ...RegistryOption) (*Registry, error) {
	options := registryOptions{retryBaseDelay: defaultBaseDelay}
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	httpClient, err := guardedHTTPClient(options.httpClient)
	if err != nil {
		return nil, err
	}
	registry := &Registry{clients: make(map[string]Client, len(providers))}
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cfg := providers[name]
		client, err := newProviderClient(name, cfg, httpClient, options)
		if err != nil {
			return nil, err
		}
		registry.clients[name] = client
		registry.infos = append(registry.infos, client.Info())
	}
	return registry, nil
}

// Client returns the client for a configured provider.
func (r *Registry) Client(name string) (Client, error) {
	if r == nil {
		return nil, errors.New("llm: nil registry")
	}
	client, ok := r.clients[name]
	if !ok {
		return nil, fmt.Errorf("llm: provider %q is not configured", name)
	}
	return client, nil
}

// Infos returns provider metadata in stable name order.
func (r *Registry) Infos() []ProviderInfo {
	if r == nil {
		return nil
	}
	out := make([]ProviderInfo, len(r.infos))
	copy(out, r.infos)
	return out
}

func newProviderClient(name string, cfg config.ProviderConfig, httpClient *http.Client, options registryOptions) (*providerClient, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("llm: provider name must not be empty")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("llm: provider %q: model must not be empty", name)
	}
	if err := validateBaseURL(cfg.BaseURL); err != nil {
		return nil, fmt.Errorf("llm: provider %q: %w", name, err)
	}
	if strings.TrimSpace(cfg.APIKeyEnv) == "" {
		return nil, fmt.Errorf("llm: provider %q: api_key_env is required", name)
	}
	apiKey := os.Getenv(cfg.APIKeyEnv)
	if apiKey == "" {
		return nil, fmt.Errorf("llm: provider %q: environment variable %s is not set", name, cfg.APIKeyEnv)
	}
	info := ProviderInfo{Name: name, Protocol: cfg.Protocol, Model: cfg.Model, BaseURL: cfg.BaseURL}
	adapter, err := newAdapter(info, cfg, apiKey, httpClient)
	if err != nil {
		return nil, fmt.Errorf("llm: provider %q: %w", name, err)
	}
	client := &providerClient{
		info:      info,
		adapter:   adapter,
		recorder:  options.recorder,
		baseDelay: options.retryBaseDelay,
	}
	if cfg.MaxConcurrency < 0 {
		return nil, fmt.Errorf("llm: provider %q: max_concurrency must not be negative", name)
	}
	if cfg.MaxConcurrency == 0 {
		client.sem = make(chan struct{}, defaultConcurrency)
	} else {
		client.sem = make(chan struct{}, cfg.MaxConcurrency)
	}
	if cfg.RPM > 0 {
		client.limiter = rate.NewLimiter(rate.Limit(float64(cfg.RPM)/60.0), 1)
	}
	return client, nil
}

func validateBaseURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("base_url is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("base_url must use http or https")
	}
	if u.Host == "" {
		return errors.New("base_url must include a host")
	}
	return nil
}

// adapter is implemented by each protocol adapter.
type adapter interface {
	Generate(ctx context.Context, req *Request) (*Response, error)
}

// providerClient adds rate limiting, concurrency, single-layer retry and
// recording around one protocol adapter.
type providerClient struct {
	info      ProviderInfo
	adapter   adapter
	sem       chan struct{}
	limiter   *rate.Limiter
	recorder  Recorder
	baseDelay time.Duration
}

// Info implements Client.
func (c *providerClient) Info() ProviderInfo { return c.info }

// Generate implements Client.
func (c *providerClient) Generate(ctx context.Context, req *Request) (*Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, canceledError(err)
	}
	for attempt := 0; ; attempt++ {
		if err := c.acquire(ctx); err != nil {
			return nil, canceledError(err)
		}
		started := time.Now()
		resp, err := c.adapter.Generate(ctx, req)
		c.release()
		elapsed := time.Since(started)
		var failure *Error
		if err != nil {
			failure = asError(err)
		}
		c.record(ctx, attempt, req, resp, failureOrNil(failure), elapsed)
		if err == nil {
			return resp, nil
		}
		if !failure.Retryable() || attempt >= maxRetries {
			return nil, failure
		}
		if !wait(ctx, c.backoff(attempt, failure)) {
			return nil, canceledError(ctx.Err())
		}
	}
}

func failureOrNil(err *Error) error {
	if err == nil {
		return nil
	}
	return err
}

func (c *providerClient) acquire(ctx context.Context) error {
	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			// rate.Wait rejects a wait that cannot finish before the deadline.
			if _, ok := ctx.Deadline(); ok {
				return context.DeadlineExceeded
			}
			return err
		}
	}
	if c.sem != nil {
		select {
		case c.sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (c *providerClient) release() {
	if c.sem != nil {
		<-c.sem
	}
}

func (c *providerClient) backoff(attempt int, failure *Error) time.Duration {
	delay := c.baseDelay
	if delay <= 0 {
		delay = defaultBaseDelay
	}
	for i := 0; i < attempt; i++ {
		delay *= 2
		if delay > 4*time.Second {
			delay = 4 * time.Second
			break
		}
	}
	if failure != nil && failure.RetryAfter > 0 {
		delay = failure.RetryAfter
	}
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}
	return delay
}

func wait(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (c *providerClient) record(ctx context.Context, attempt int, req *Request, resp *Response, err error, elapsed time.Duration) {
	if c.recorder == nil {
		return
	}
	c.recorder.RecordCall(ctx, CallRecord{
		Provider: c.info.Name,
		Protocol: c.info.Protocol,
		Model:    c.info.Model,
		Meta:     CallMetaFromContext(ctx),
		Attempt:  attempt,
		Request:  req,
		Response: resp,
		Err:      err,
		Duration: elapsed,
	})
}

// asError classifies any adapter error. Adapters normally return *Error.
func asError(err error) *Error {
	var classified *Error
	if errors.As(err, &classified) {
		return classified
	}
	return sdkError(err)
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	if !strings.EqualFold(a.Scheme, b.Scheme) {
		return false
	}
	return strings.EqualFold(originHost(a), originHost(b))
}

func originHost(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return host + ":" + port
}

func guardedHTTPClient(base *http.Client) (*http.Client, error) {
	if base == nil {
		base = http.DefaultClient
	}
	clone := *base
	previous := clone.CheckRedirect
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if previous != nil {
			if err := previous(req, via); err != nil {
				return err
			}
		}
		if len(via) >= 10 {
			return errors.New("llm: too many redirects")
		}
		if len(via) == 0 {
			return nil
		}
		if !sameOrigin(via[0].URL, req.URL) {
			return errors.New("llm: redirect to another origin is not allowed")
		}
		return nil
	}
	return &clone, nil
}

func newAdapter(info ProviderInfo, cfg config.ProviderConfig, apiKey string, httpClient *http.Client) (adapter, error) {
	defaults := providerDefaults{
		maxTokens:   cfg.MaxTokens,
		temperature: cfg.Temperature,
	}
	if cfg.Thinking != nil {
		defaults.budgetTokens = cfg.Thinking.BudgetTokens
	}
	if cfg.Reasoning != nil {
		defaults.effort = cfg.Reasoning.Effort
	}
	switch cfg.Protocol {
	case "openai-chat":
		return newOpenAIChatAdapter(info, defaults, apiKey, httpClient), nil
	case "openai-responses":
		return newOpenAIResponsesAdapter(info, defaults, apiKey, httpClient), nil
	case "anthropic":
		return newAnthropicAdapter(info, defaults, apiKey, httpClient), nil
	default:
		return nil, fmt.Errorf("protocol %q is not supported", cfg.Protocol)
	}
}
