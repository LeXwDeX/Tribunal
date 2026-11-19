package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

// A review may use the code-graph MCP tools only when the graph describes the
// commit under review. The shared workspace index follows one commit of the
// project, so a merge request whose target branch is somewhere else cannot rely
// on it. A target graph is the per-review alternative: the review's own base
// checkout is indexed by a private MCP manager whose workspace, and therefore
// whose server HOME, is graphs/<runID>. That directory holds exactly one graph,
// for exactly one review. No graph outlives its review: Close stops the servers
// and removes the directory. Only the fact that an input failed to build is
// remembered, for a short while and in memory (targetGraphFailures).

// A Build waits for an index slot and then indexes, and the two have separate
// budgets. Queueing behind other reviews is bounded by targetGraphSlotTimeout;
// the index run, with the checks around it, gets targetGraphIndexTimeout counted
// from the moment the slot is held, so a long queue never eats the time the
// index itself needs.
const (
	targetGraphSlotTimeout  = 5 * time.Minute
	targetGraphIndexTimeout = 10 * time.Minute
)

const (
	// targetGraphFailureTTL is how long a build whose failure is likely to repeat
	// is not attempted again for the same input. Without it every review of the
	// same base would pay for the same failing index.
	targetGraphFailureTTL = 15 * time.Minute
	// targetGraphFailureEntries bounds that record; the oldest entry goes first.
	targetGraphFailureEntries = 256
)

// targetGraphSlots limits the index runs a process executes at once. Indexing
// a whole repository is the heaviest thing a review starts.
var targetGraphSlots = make(chan struct{}, 2)

// Failure classes a degraded Build reports. A timeout or cancelled index
// reports the errclass names instead. They are fixed words, never error text.
const (
	targetGraphInvalidRequest  = "invalid_request"
	targetGraphBaseUnverified  = "base_unverified"
	targetGraphSetupFailed     = "setup_failed"
	targetGraphIndexFailed     = "index_failed"
	targetGraphIndexIncomplete = "index_incomplete"
	// targetGraphRecentlyFailed marks a Build that was skipped because the same
	// input failed a moment ago.
	targetGraphRecentlyFailed = "recently_failed"
	// targetGraphSlotBusy: no index slot became free in time; nothing was run.
	targetGraphSlotBusy = "slot_busy"
)

var (
	errTargetGraphBaseMoved  = errors.New("app: target graph base is not at the start commit")
	errTargetGraphNoGit      = errors.New("app: target graph needs git to verify the base")
	errTargetGraphIncomplete = errors.New("app: target graph index did not cover every server")
)

// targetGraphUnavailable reports that no target graph could be built for a
// reason the review survives. Callers degrade instead of failing: they keep the
// shared workspace graph where it is valid, or withhold the index-backed tools,
// and log the project ID plus the class only. The class is a short fixed word
// and the underlying error is deliberately dropped, because index and git
// errors may carry paths or command output.
type targetGraphUnavailable struct {
	class string
}

func (e *targetGraphUnavailable) Error() string {
	return "app: target graph unavailable: " + e.class
}

// ErrorCode lets errclass.Of report the class.
func (e *targetGraphUnavailable) ErrorCode() string { return e.class }

// targetGraphRequest names the checkout to index. ProjectID must be the
// project of Workspace, and BaseDir the review's base checkout, already at
// StartSHA: an absolute, clean path that is the workspace's shared base or lies
// below its runs directory. The caller holds the workspace read lock for the
// whole review and passes a context bound to the workspace's outbound gate,
// because indexing runs administrator-configured commands.
type targetGraphRequest struct {
	ProjectID int64
	Workspace *workspace.Workspace
	RunID     string
	BaseDir   string
	StartSHA  string
	// MCP is the project's merged server set. Only servers with an index
	// command are copied into the target graph's manager.
	MCP       mcp.Config
	DeniedEnv []string
	// Ignore is the review's effective ignore policy. A restricted policy never
	// reaches an index: Build then does nothing, whatever the caller checked.
	Ignore []string
}

// targetGraph is one review's private code graph: a manager whose servers see
// only the index built for the review base. It lives until Close.
type targetGraph struct {
	manager *mcp.Manager
	// servers names the index-backed servers the manager owns. Their tools are
	// served from manager and never from the shared one.
	servers map[string]bool
	dir     string
	remove  func() error

	closeOnce sync.Once
}

// Close stops the graph's servers, then removes its directory, in that order so
// no server still has the files open. It is nil-safe and idempotent. A failed
// removal is left to the periodic workspace cleanup.
func (g *targetGraph) Close() {
	if g == nil {
		return
	}
	g.closeOnce.Do(func() {
		if g.manager != nil {
			_ = g.manager.Close()
		}
		if g.remove != nil {
			_ = g.remove()
		}
	})
}

// targetGraphBuilder indexes a review's base checkout into a target graph.
type targetGraphBuilder interface {
	// Build returns (nil, nil) when the project has nothing to index locally
	// or the request's ignore policy is restricted. It returns a
	// *targetGraphUnavailable when the graph could not be built, or was not
	// tried because the same input failed a moment ago, and the review should
	// degrade. Any other error, a done caller or a revoked outbound policy,
	// stops the review. A non-nil graph belongs to the caller, who must Close
	// it.
	Build(ctx context.Context, req targetGraphRequest) (*targetGraph, error)
}

// defaultTargetGraphBuilder is the production targetGraphBuilder. The func
// fields are seams for tests.
type defaultTargetGraphBuilder struct {
	git          GitOps
	slots        chan struct{}
	slotTimeout  time.Duration
	indexTimeout time.Duration
	now          func() time.Time
	failures     targetGraphFailures

	newManager func(mcp.Options) (*mcp.Manager, error)
	runIndex   func(ctx context.Context, manager *mcp.Manager, baseDir string) (int, error)
}

var _ targetGraphBuilder = (*defaultTargetGraphBuilder)(nil)

func newTargetGraphBuilder(git GitOps) *defaultTargetGraphBuilder {
	return &defaultTargetGraphBuilder{
		git:          git,
		slots:        targetGraphSlots,
		slotTimeout:  targetGraphSlotTimeout,
		indexTimeout: targetGraphIndexTimeout,
		now:          time.Now,
		newManager:   mcp.New,
		runIndex: func(ctx context.Context, manager *mcp.Manager, baseDir string) (int, error) {
			return manager.RunIndex(ctx, baseDir)
		},
	}
}

func (b *defaultTargetGraphBuilder) Build(ctx context.Context, req targetGraphRequest) (*targetGraph, error) {
	// Fail closed: callers already keep a restricted policy away from an index,
	// and this is the last place that can still refuse before anything runs.
	if config.ReviewIgnoreRestricted(req.Ignore) {
		return nil, nil
	}
	indexed, local := indexedLocalServers(req.MCP)
	if !local || len(indexed) == 0 {
		return nil, nil
	}
	if !validTargetGraphRequest(req) {
		return nil, &targetGraphUnavailable{class: targetGraphInvalidRequest}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// The same input that failed a moment ago is not worth another index run.
	key := targetGraphFailureKey{
		projectID: req.ProjectID,
		startSHA:  strings.ToLower(req.StartSHA),
		config:    targetGraphFingerprint(indexed, req.DeniedEnv),
	}
	remember := key.config != ""
	if remember && b.failures.recent(key, b.now()) {
		return nil, &targetGraphUnavailable{class: targetGraphRecentlyFailed}
	}
	graph, err := b.build(ctx, req, indexed)
	if remember {
		var unavailable *targetGraphUnavailable
		switch {
		case err == nil && graph != nil:
			b.failures.forget(key)
		case errors.As(err, &unavailable) && repeatsTargetGraphFailure(unavailable.class):
			b.failures.record(key, b.now())
		}
	}
	return graph, err
}

// build indexes the request. indexed is the request's validated server set.
func (b *defaultTargetGraphBuilder) build(ctx context.Context, req targetGraphRequest, indexed map[string]mcp.ServerConfig) (*targetGraph, error) {
	// A failure leaves neither a running server nor a directory behind, not
	// even a directory an earlier attempt of this run left: the manager is
	// closed first, then the directory goes.
	var manager *mcp.Manager
	built := false
	defer func() {
		if built {
			return
		}
		if manager != nil {
			_ = manager.Close()
		}
		_ = req.Workspace.RemoveGraphDir(req.RunID)
	}()

	// Waiting for a slot has a budget of its own, so queueing behind other
	// reviews never counts against the index.
	waitCtx, cancelWait := context.WithTimeout(ctx, b.slotTimeout)
	select {
	case b.slots <- struct{}{}:
		cancelWait()
		defer func() { <-b.slots }()
	case <-waitCtx.Done():
		cancelWait()
		// Every slot stayed taken by other reviews. That says nothing about
		// this input, so it is reported, and remembered, apart from a timeout
		// of the index itself.
		if err := degradeTargetGraph(ctx, waitCtx.Err(), targetGraphSlotBusy); !isTargetGraphUnavailable(err) {
			return nil, err
		}
		return nil, &targetGraphUnavailable{class: targetGraphSlotBusy}
	}
	// The index budget starts here, with the slot in hand.
	tctx, cancel := context.WithTimeout(ctx, b.indexTimeout)
	defer cancel()
	if err := tctx.Err(); err != nil {
		return nil, degradeTargetGraph(ctx, err, targetGraphIndexFailed)
	}

	if err := b.verifyBase(tctx, req); err != nil {
		return nil, degradeTargetGraph(ctx, err, targetGraphBaseUnverified)
	}

	dir, err := req.Workspace.EnsureGraphDir(req.RunID)
	if err != nil {
		return nil, degradeTargetGraph(ctx, err, targetGraphSetupFailed)
	}
	created, err := b.newManager(mcp.Options{
		Platform:  mcp.Config{MCPServers: indexed},
		Workspace: dir,
		DeniedEnv: req.DeniedEnv,
		Now:       b.now,
	})
	if err != nil {
		return nil, degradeTargetGraph(ctx, err, targetGraphSetupFailed)
	}
	manager = created

	var count int
	err = sendOutbound(tctx, req.Workspace, func(indexCtx context.Context) error {
		var indexErr error
		count, indexErr = b.runIndex(indexCtx, manager, req.BaseDir)
		return indexErr
	})
	if err != nil {
		return nil, degradeTargetGraph(ctx, err, targetGraphIndexFailed)
	}
	if count != len(indexed) {
		return nil, degradeTargetGraph(ctx, errTargetGraphIncomplete, targetGraphIndexIncomplete)
	}
	if err := b.verifyBase(tctx, req); err != nil {
		return nil, degradeTargetGraph(ctx, err, targetGraphBaseUnverified)
	}

	servers := make(map[string]bool, len(indexed))
	for name := range indexed {
		servers[name] = true
	}
	built = true
	return &targetGraph{
		manager: manager,
		servers: servers,
		dir:     dir,
		remove:  func() error { return req.Workspace.RemoveGraphDir(req.RunID) },
	}, nil
}

// validTargetGraphRequest is the request self-check: the workspace must belong
// to the project, the run ID and start commit must be well formed, and BaseDir
// must be a checkout the workspace itself made.
func validTargetGraphRequest(req targetGraphRequest) bool {
	return req.Workspace != nil && req.Workspace.ProjectID == req.ProjectID &&
		workspace.ValidRunID(req.RunID) && shaRE.MatchString(req.StartSHA) &&
		targetGraphBaseAllowed(req.Workspace, req.BaseDir)
}

// targetGraphBaseAllowed reports whether dir may be indexed for ws: the shared
// base, or a directory below the workspace's runs directory. The path must be
// absolute and already clean, because a ".." that follows a symlink does not
// mean what its lexical cleaning says.
func targetGraphBaseAllowed(ws *workspace.Workspace, dir string) bool {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return false
	}
	if dir == filepath.Clean(ws.BaseDir()) {
		return true
	}
	rel, err := filepath.Rel(filepath.Join(ws.Root, "runs"), dir)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// repeatsTargetGraphFailure reports whether a degradation class says the same
// input is likely to fail again. A moved base, a caller that is done and an
// invalid request describe one run, not the input.
func repeatsTargetGraphFailure(class string) bool {
	switch class {
	case targetGraphIndexFailed, targetGraphIndexIncomplete, targetGraphSetupFailed, errclass.Timeout:
		return true
	}
	return false
}

// targetGraphFingerprint identifies what an index run depends on besides the
// commit: the index-backed servers and the environment they are denied. It is
// empty when the input cannot be encoded.
func targetGraphFingerprint(indexed map[string]mcp.ServerConfig, deniedEnv []string) string {
	data, err := json.Marshal(struct {
		Servers   map[string]mcp.ServerConfig
		DeniedEnv []string
	}{indexed, deniedEnv})
	if err != nil {
		return ""
	}
	return sha256Hex(string(data))
}

// targetGraphFailureKey is the input of one index run.
type targetGraphFailureKey struct {
	projectID int64
	startSHA  string
	config    string
}

// targetGraphFailures remembers recent failed builds in memory. Every entry
// lives the same time, so the oldest entry is also the one that expires first.
// The zero value is ready to use.
type targetGraphFailures struct {
	mu    sync.Mutex
	until map[targetGraphFailureKey]time.Time
	order []targetGraphFailureKey // oldest first
}

// recent reports whether key failed less than targetGraphFailureTTL ago.
func (f *targetGraphFailures) recent(key targetGraphFailureKey, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	until, ok := f.until[key]
	if !ok {
		return false
	}
	if !now.Before(until) {
		f.dropLocked(key)
		return false
	}
	return true
}

// record notes a failed build of key, replacing an older note of the same key.
// A full record loses its oldest entries first.
func (f *targetGraphFailures) record(key targetGraphFailureKey, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.until == nil {
		f.until = make(map[targetGraphFailureKey]time.Time)
	}
	f.dropLocked(key)
	for len(f.order) > 0 && !now.Before(f.until[f.order[0]]) {
		f.dropLocked(f.order[0])
	}
	for len(f.order) >= targetGraphFailureEntries {
		f.dropLocked(f.order[0])
	}
	f.until[key] = now.Add(targetGraphFailureTTL)
	f.order = append(f.order, key)
}

// forget drops the note of key, as after a build that worked.
func (f *targetGraphFailures) forget(key targetGraphFailureKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropLocked(key)
}

func (f *targetGraphFailures) dropLocked(key targetGraphFailureKey) {
	if _, ok := f.until[key]; !ok {
		return
	}
	delete(f.until, key)
	for i, queued := range f.order {
		if queued == key {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
}

// verifyBase proves the base checkout is still at the review's start commit.
func (b *defaultTargetGraphBuilder) verifyBase(ctx context.Context, req targetGraphRequest) error {
	if b.git == nil {
		return errTargetGraphNoGit
	}
	head, err := b.git.Resolve(ctx, req.BaseDir, "HEAD")
	if err != nil {
		return err
	}
	if !shaRE.MatchString(head) || !strings.EqualFold(head, req.StartSHA) {
		return errTargetGraphBaseMoved
	}
	return nil
}

// degradeTargetGraph classifies a Build failure. A caller that is done and a
// revoked outbound policy stop the review, so they pass through for the caller
// to recognise with errors.Is. Anything else, including this build's own
// timeout, only costs the review its target graph.
func degradeTargetGraph(parent context.Context, err error, class string) error {
	if parentErr := parent.Err(); parentErr != nil {
		return parentErr
	}
	if errors.Is(err, workspace.ErrOutboundInvalidated) {
		return err
	}
	if c := workspaceErrorClass(err); c == errclass.Timeout || c == errclass.Canceled {
		class = c
	}
	return &targetGraphUnavailable{class: class}
}

// indexedLocalServers returns the servers that have an index command. local is
// false when one of them is not a stdio server: a remote server serves its own
// index, so a per-run local one would not be what the review's tools read.
func indexedLocalServers(cfg mcp.Config) (indexed map[string]mcp.ServerConfig, local bool) {
	indexed = make(map[string]mcp.ServerConfig)
	for name, server := range cfg.MCPServers {
		if len(server.XReview.IndexCommand) == 0 {
			continue
		}
		if !isStdioServer(server) {
			return nil, false
		}
		indexed[name] = server
	}
	return indexed, true
}

func isStdioServer(cfg mcp.ServerConfig) bool {
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "", "stdio":
		return cfg.Command != ""
	}
	return false
}

// routeTargetGraphTools returns the review's tool provider while a target graph
// is live. Tools of the servers the graph owns come from the graph's manager
// only, and every other tool keeps coming from the shared one, so an
// index-backed tool can never read the shared index. Without a graph the plain
// shared provider is returned. blocks is not modified.
func routeTargetGraphTools(shared *mcp.Manager, g *targetGraph, blocks map[string]config.BlockBinding) review.ToolProvider {
	if g == nil || g.manager == nil || len(g.servers) == 0 {
		return NewMCPToolProvider(shared, blocks)
	}
	sharedBlocks := make(map[string]config.BlockBinding, len(blocks))
	targetBlocks := make(map[string]config.BlockBinding, len(blocks))
	for block, binding := range blocks {
		var sharedTools, targetTools []string
		for _, tool := range binding.MCPTools {
			if server, ok := mcpToolServer(tool); ok && g.servers[server] {
				targetTools = append(targetTools, tool)
			} else {
				sharedTools = append(sharedTools, tool)
			}
		}
		sharedBinding, targetBinding := binding, binding
		sharedBinding.MCPTools = sharedTools
		targetBinding.MCPTools = targetTools
		sharedBlocks[block] = sharedBinding
		targetBlocks[block] = targetBinding
	}
	return &routedToolProvider{
		shared: NewMCPToolProvider(shared, sharedBlocks),
		target: NewMCPToolProvider(g.manager, targetBlocks),
	}
}

// mcpToolServer reads the server out of an mcp__server__tool binding exactly
// as mcpToolProvider does. ok is false for a name without the separator, which
// the manager then reports as a gap.
func mcpToolServer(tool string) (server string, ok bool) {
	server, _, ok = strings.Cut(strings.TrimPrefix(tool, "mcp__"), "__")
	return server, ok
}

// routedToolProvider combines the shared provider with the target graph's. It
// implements review.ToolProvider and review.ToolBindingProvider.
type routedToolProvider struct {
	shared review.ToolProvider
	target review.ToolProvider
}

func (p *routedToolProvider) Tools(ctx context.Context, block string) ([]agent.Tool, error) {
	var (
		tools   []agent.Tool
		gaps    []string
		partial bool
	)
	for _, part := range []struct {
		label    string
		provider review.ToolProvider
	}{{"shared", p.shared}, {"target graph", p.target}} {
		got, err := part.provider.Tools(ctx, block)
		var incomplete *review.PartialToolsError
		switch {
		case errors.As(err, &incomplete):
			partial = true
			tools = append(tools, incomplete.Tools...)
			gaps = append(gaps, incomplete.Gaps...)
		case err != nil:
			return nil, fmt.Errorf("app: %s MCP tools for block %q: %w", part.label, block, err)
		default:
			tools = append(tools, got...)
		}
	}
	if partial {
		return nil, &review.PartialToolsError{Tools: tools, Gaps: gaps}
	}
	return tools, nil
}

func (p *routedToolProvider) BoundToolNames(ctx context.Context, block string) ([]string, error) {
	var names []string
	for _, part := range []struct {
		label    string
		provider review.ToolProvider
	}{{"shared", p.shared}, {"target graph", p.target}} {
		binding, ok := part.provider.(review.ToolBindingProvider)
		if !ok {
			return nil, errors.New("app: MCP binding metadata unavailable")
		}
		got, err := binding.BoundToolNames(ctx, block)
		if err != nil {
			return nil, fmt.Errorf("app: %s MCP bindings for block %q: %w", part.label, block, err)
		}
		names = append(names, got...)
	}
	return names, nil
}

func isTargetGraphUnavailable(err error) bool {
	var unavailable *targetGraphUnavailable
	return errors.As(err, &unavailable)
}
