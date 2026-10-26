package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// DefaultConnectTimeout bounds the initial MCP handshake.
	DefaultConnectTimeout = 30 * time.Second
	// DefaultReapInterval is the longest idle-check period.
	DefaultReapInterval = 30 * time.Second
	connectPollEvery    = 100 * time.Millisecond
)

var (
	// ErrClosed reports use of a closed manager.
	ErrClosed = errors.New("mcp: manager is closed")
	// ErrUnknownServer reports an allowlist entry for an unconfigured server.
	ErrUnknownServer = errors.New("mcp: unknown server")
	// ErrConnectionClosed reports a dead server connection.
	ErrConnectionClosed = errors.New("mcp: server connection closed")
	// ErrCallFailed reports a tool call that failed for a non-secret reason.
	ErrCallFailed = errors.New("mcp: tool call failed")
)

// Options configures a Manager. Platform and Project follow the Claude
// mcpServers document; Project entries replace Platform entries by name.
//
// Workspace is required. DeniedEnv names are never inherited by stdio servers
// even when listed in env_allowlist. The application should pass every project
// token and provider API key variable name.
type Options struct {
	Platform   Config
	Project    Config
	Workspace  string
	DeniedEnv  []string
	Now        func() time.Time
	HTTPClient *http.Client
}

// Manager owns per-workspace MCP sessions keyed by server and configuration
// fingerprint. A stdio process outlives one request; caller cancellation only
// cancels the call. Close releases every process and connection.
type Manager struct {
	ctx       context.Context
	cancel    context.CancelFunc
	workspace string
	servers   map[string]ServerConfig
	env       envPolicy
	now       func() time.Time
	baseHTTP  *http.Client

	mu      sync.Mutex
	entries map[entryKey]*serverEntry
	closed  bool
	wg      sync.WaitGroup
}

// HasServer reports whether name is a configured server. Disabled and
// deleted servers are not configured.
func (m *Manager) HasServer(name string) bool {
	if m == nil {
		return false
	}
	_, ok := m.servers[name]
	return ok
}

type entryKey struct {
	workspace   string
	server      string
	fingerprint string
}

func New(opts Options) (*Manager, error) {
	workspace, err := prepareWorkspace(opts.Workspace)
	if err != nil {
		return nil, err
	}
	servers := Merge(opts.Platform, opts.Project).MCPServers
	clean := make(map[string]ServerConfig, len(servers))
	for name, cfg := range servers {
		cfg = cfg.withDefaults()
		if err := cfg.Validate(name); err != nil {
			return nil, err
		}
		clean[name] = cfg
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		ctx:       ctx,
		cancel:    cancel,
		workspace: workspace,
		servers:   clean,
		env:       newEnvPolicy(nil, opts.DeniedEnv),
		now:       now,
		baseHTTP:  opts.HTTPClient,
		entries:   make(map[entryKey]*serverEntry),
	}
	if len(clean) > 0 {
		every := reapInterval(clean)
		m.wg.Add(1)
		go m.reapLoop(every)
	}
	return m, nil
}

// Servers returns configured names in stable order.
func (m *Manager) Servers() []string {
	names := make([]string, 0, len(m.servers))
	for name := range m.servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Close terminates all sessions and waits for their processes to exit.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.cancel()
	entries := make([]*serverEntry, 0, len(m.entries))
	for _, entry := range m.entries {
		entries = append(entries, entry)
	}
	m.entries = make(map[entryKey]*serverEntry)
	m.mu.Unlock()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed []string
	for _, entry := range entries {
		wg.Add(1)
		go func(entry *serverEntry) {
			defer wg.Done()
			if err := entry.close(); err != nil {
				mu.Lock()
				failed = append(failed, entry.key.server)
				mu.Unlock()
			}
		}(entry)
	}
	wg.Wait()
	m.wg.Wait()
	if len(failed) > 0 {
		sort.Strings(failed)
		return fmt.Errorf("mcp: close failed for server(s): %s", strings.Join(failed, ", "))
	}
	return nil
}

// ReapIdle closes sessions that have been idle for their configured timeout.
// It returns the number of sessions closed. It is safe to call concurrently.
func (m *Manager) ReapIdle() int {
	now := m.now()
	var closing []*serverEntry
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return 0
	}
	for key, entry := range m.entries {
		if entry.takeIdle(now) {
			delete(m.entries, key)
			closing = append(closing, entry)
		}
	}
	m.mu.Unlock()
	for _, entry := range closing {
		_ = entry.close()
	}
	return len(closing)
}

func (m *Manager) reapLoop(every time.Duration) {
	defer m.wg.Done()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.ReapIdle()
		}
	}
}

func reapInterval(servers map[string]ServerConfig) time.Duration {
	smallest := DefaultIdleTimeout
	for _, cfg := range servers {
		idle := cfg.XReview.IdleTimeout.AsDuration()
		if idle > 0 && idle < smallest {
			smallest = idle
		}
	}
	every := smallest / 4
	if every < 25*time.Millisecond {
		every = 25 * time.Millisecond
	}
	if every > DefaultReapInterval {
		every = DefaultReapInterval
	}
	return every
}

func (m *Manager) acquire(ctx context.Context, server string) (*serverEntry, error) {
	cfg, ok := m.servers[server]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownServer, server)
	}
	key := entryKey{workspace: m.workspace, server: server, fingerprint: fingerprint(cfg)}
	for range 2 {
		entry, err := m.entryFor(ctx, key, cfg)
		if err != nil {
			return nil, err
		}
		if entry.begin() {
			return entry, nil
		}
		m.discard(entry)
	}
	return nil, fmt.Errorf("%w: %q", ErrConnectionClosed, server)
}

func (m *Manager) entryFor(ctx context.Context, key entryKey, cfg ServerConfig) (*serverEntry, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	if entry := m.entries[key]; entry != nil {
		m.mu.Unlock()
		return m.await(ctx, entry)
	}
	entry := &serverEntry{
		m:     m,
		key:   key,
		cfg:   cfg,
		ready: make(chan struct{}),
	}
	entry.lastUsed = m.now()
	m.entries[key] = entry
	m.wg.Add(1)
	go entry.start()
	m.mu.Unlock()
	return m.await(ctx, entry)
}

func (m *Manager) await(ctx context.Context, entry *serverEntry) (*serverEntry, error) {
	select {
	case <-entry.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.ctx.Done():
		return nil, ErrClosed
	}
	entry.mu.Lock()
	err := entry.err
	dead := entry.dead
	session := entry.session
	entry.mu.Unlock()
	if err != nil {
		return nil, entry.connectError()
	}
	if dead || session == nil {
		return nil, fmt.Errorf("%w: %q", ErrConnectionClosed, entry.key.server)
	}
	return entry, nil
}

func (m *Manager) discard(entry *serverEntry) {
	entry.mu.Lock()
	entry.closing = true
	entry.mu.Unlock()
	m.remove(entry)
	_ = entry.close()
}

func (m *Manager) remove(entry *serverEntry) {
	m.mu.Lock()
	if current := m.entries[entry.key]; current == entry {
		delete(m.entries, entry.key)
	}
	m.mu.Unlock()
}

func (entry *serverEntry) connectError() error {
	if errors.Is(entry.err, context.DeadlineExceeded) {
		return fmt.Errorf("mcp: server %q: connect timed out", entry.key.server)
	}
	return fmt.Errorf("mcp: server %q: connect failed", entry.key.server)
}

type serverEntry struct {
	m   *Manager
	key entryKey
	cfg ServerConfig

	ready chan struct{}

	mu        sync.Mutex
	session   *sdk.ClientSession
	err       error
	dead      bool
	closing   bool
	connected bool
	inflight  int
	lastUsed  time.Time

	closeOnce sync.Once
	closeErr  error
}

func (entry *serverEntry) start() {
	defer entry.m.wg.Done()
	ctx, cancel := context.WithTimeout(entry.m.ctx, DefaultConnectTimeout)
	defer cancel()
	session, err := entry.connect(ctx)

	entry.mu.Lock()
	if err == nil {
		entry.session = session
		entry.connected = true
		entry.lastUsed = entry.m.now()
	} else {
		entry.err = err
		entry.dead = true
	}
	entry.mu.Unlock()
	if err != nil {
		entry.m.remove(entry)
	}
	close(entry.ready)
	if session != nil {
		entry.m.wg.Add(1)
		go entry.watch(session)
	}
}

func (entry *serverEntry) watch(session *sdk.ClientSession) {
	defer entry.m.wg.Done()
	waitErr := session.Wait()
	entry.mu.Lock()
	if entry.session == session {
		entry.dead = true
		if entry.err == nil {
			entry.err = waitErr
		}
	}
	entry.mu.Unlock()
	entry.m.remove(entry)
}

func (entry *serverEntry) begin() bool {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.dead || entry.closing || entry.session == nil {
		return false
	}
	entry.inflight++
	entry.lastUsed = entry.m.now()
	return true
}

func (entry *serverEntry) end() {
	entry.mu.Lock()
	if entry.inflight > 0 {
		entry.inflight--
	}
	entry.lastUsed = entry.m.now()
	entry.mu.Unlock()
}

func (entry *serverEntry) current() (*sdk.ClientSession, error) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.dead || entry.session == nil {
		return nil, fmt.Errorf("%w: %q", ErrConnectionClosed, entry.key.server)
	}
	return entry.session, nil
}

func (entry *serverEntry) takeIdle(now time.Time) bool {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.inflight > 0 || entry.dead || entry.closing || !entry.connected || entry.session == nil {
		return false
	}
	idle := entry.cfg.XReview.IdleTimeout.AsDuration()
	if idle <= 0 || now.Sub(entry.lastUsed) < idle {
		return false
	}
	entry.closing = true
	return true
}

func (entry *serverEntry) markDead(err error) {
	entry.mu.Lock()
	entry.dead = true
	if entry.err == nil {
		entry.err = err
	}
	entry.mu.Unlock()
	entry.m.remove(entry)
}

func (entry *serverEntry) close() error {
	entry.closeOnce.Do(func() {
		select {
		case <-entry.ready:
		case <-time.After(5 * time.Second):
		}
		entry.mu.Lock()
		session := entry.session
		entry.mu.Unlock()
		if session != nil {
			entry.closeErr = session.Close()
		}
	})
	return entry.closeErr
}

func (entry *serverEntry) connect(ctx context.Context) (*sdk.ClientSession, error) {
	client := sdk.NewClient(
		&sdk.Implementation{Name: "gitlab-ce-review", Version: "0.1.0"},
		&sdk.ClientOptions{Capabilities: &sdk.ClientCapabilities{}},
	)
	if entry.cfg.URL != "" {
		httpClient := entry.httpClient()
		transport := &sdk.StreamableClientTransport{
			Endpoint:             entry.cfg.URL,
			HTTPClient:           httpClient,
			MaxRetries:           -1,
			DisableStandaloneSSE: true,
		}
		return client.Connect(ctx, transport, nil)
	}
	env, _, err := entry.m.env.build(entry.cfg, entry.m.workspace, entry.key.server)
	if err != nil {
		return nil, err
	}
	// The command deliberately does not use exec.CommandContext: a request
	// context must stop one call, not the shared process. Transport.Close owns
	// graceful termination; the connect context only bounds the handshake.
	cmd := exec.Command(entry.cfg.Command, entry.cfg.Args...)
	cmd.Dir = entry.m.workspace
	cmd.Env = env
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 5 * time.Second
	transport := &sdk.CommandTransport{Command: cmd}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return session, err
}

func (entry *serverEntry) httpClient() *http.Client {
	var transport http.RoundTripper = http.DefaultTransport
	if base := entry.m.baseHTTP; base != nil && base.Transport != nil {
		transport = base.Transport
	}
	return &http.Client{
		Transport: &headerTransport{base: transport, headers: entry.cfg.Headers},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	for name, value := range t.headers {
		clone.Header.Set(name, value)
	}
	return t.base.RoundTrip(clone)
}
