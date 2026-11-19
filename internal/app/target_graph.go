package app

import (
	"context"
	"errors"
	"fmt"
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
// for exactly one review. Nothing is cached between reviews: Close stops the
// servers and removes the directory.

// targetGraphTimeout bounds one Build: waiting for a slot plus indexing.
const targetGraphTimeout = 10 * time.Minute

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
// StartSHA. The caller holds the workspace read lock for the whole review and
// passes a context bound to the workspace's outbound gate, because indexing
// runs administrator-configured commands.
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
	// Build returns (nil, nil) when the project has nothing to index locally.
	// It returns a *targetGraphUnavailable when the graph could not be built
	// and the review should degrade. Any other error, a done caller or a
	// revoked outbound policy, stops the review. A non-nil graph belongs to the
	// caller, who must Close it.
	Build(ctx context.Context, req targetGraphRequest) (*targetGraph, error)
}

// defaultTargetGraphBuilder is the production targetGraphBuilder. The func
// fields are seams for tests.
type defaultTargetGraphBuilder struct {
	git     GitOps
	slots   chan struct{}
	timeout time.Duration
	now     func() time.Time

	newManager func(mcp.Options) (*mcp.Manager, error)
	runIndex   func(ctx context.Context, manager *mcp.Manager, baseDir string) (int, error)
}

var _ targetGraphBuilder = (*defaultTargetGraphBuilder)(nil)

func newTargetGraphBuilder(git GitOps) *defaultTargetGraphBuilder {
	return &defaultTargetGraphBuilder{
		git:        git,
		slots:      targetGraphSlots,
		timeout:    targetGraphTimeout,
		now:        time.Now,
		newManager: mcp.New,
		runIndex: func(ctx context.Context, manager *mcp.Manager, baseDir string) (int, error) {
			return manager.RunIndex(ctx, baseDir)
		},
	}
}

func (b *defaultTargetGraphBuilder) Build(ctx context.Context, req targetGraphRequest) (*targetGraph, error) {
	indexed, local := indexedLocalServers(req.MCP)
	if !local || len(indexed) == 0 {
		return nil, nil
	}
	if req.Workspace == nil || req.Workspace.ProjectID != req.ProjectID || !workspace.ValidRunID(req.RunID) ||
		strings.TrimSpace(req.BaseDir) == "" || !shaRE.MatchString(req.StartSHA) {
		return nil, &targetGraphUnavailable{class: targetGraphInvalidRequest}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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

	tctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	case <-tctx.Done():
		return nil, degradeTargetGraph(ctx, tctx.Err(), targetGraphIndexFailed)
	}
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
