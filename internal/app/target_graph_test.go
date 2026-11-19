package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

const (
	targetGraphTestStart = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	targetGraphTestOther = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	// targetGraphTestCanary stands in for text that must never reach an error
	// returned to callers, because callers log it.
	targetGraphTestCanary = "canary-secret-value"
)

// targetGraphTestGit answers Resolve only; every other GitOps call would panic
// on the nil embedded interface, which is the point: Build must not use them.
type targetGraphTestGit struct {
	GitOps
	mu    sync.Mutex
	heads []string // consecutive answers; the last one repeats
	err   error
	calls []string
}

func (g *targetGraphTestGit) Resolve(_ context.Context, dir, rev string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, dir+"@"+rev)
	if g.err != nil {
		return "", g.err
	}
	n := min(len(g.calls)-1, len(g.heads)-1)
	return g.heads[n], nil
}

func (g *targetGraphTestGit) resolveCalls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

type targetGraphFixture struct {
	t       *testing.T
	ws      *workspace.Workspace
	git     *targetGraphTestGit
	builder *defaultTargetGraphBuilder
	req     targetGraphRequest
	ctx     context.Context // bound to the workspace's outbound gate

	mu       sync.Mutex
	managers []*mcp.Manager
	options  []mcp.Options
}

func newTargetGraphFixture(t *testing.T) *targetGraphFixture {
	t.Helper()
	ws := workspace.NewManager(t.TempDir(), nil).Get(42)
	git := &targetGraphTestGit{heads: []string{targetGraphTestStart}}
	f := &targetGraphFixture{t: t, ws: ws, git: git}
	f.builder = newTargetGraphBuilder(git)
	f.builder.slots = make(chan struct{}, 1)
	// Tests that wait for a signal fail after this long instead of the
	// production ten minutes; tests about timing set their own.
	f.builder.timeout = 30 * time.Second
	newManager := f.builder.newManager
	f.builder.newManager = func(opts mcp.Options) (*mcp.Manager, error) {
		manager, err := newManager(opts)
		if err == nil {
			f.mu.Lock()
			f.managers = append(f.managers, manager)
			f.options = append(f.options, opts)
			f.mu.Unlock()
			t.Cleanup(func() { _ = manager.Close() })
		}
		return manager, err
	}
	f.req = targetGraphRequest{
		ProjectID: 42,
		Workspace: ws,
		RunID:     "run-1",
		BaseDir:   t.TempDir(),
		StartSHA:  targetGraphTestStart,
		MCP: mcp.Config{MCPServers: map[string]mcp.ServerConfig{
			"graph": {Command: indexTrueCommand, XReview: mcp.XReviewConfig{IndexCommand: []string{indexTrueCommand}}},
		}},
		DeniedEnv: []string{"GITLAB_TOKEN"},
	}
	ctx, err := bindOutbound(context.Background(), ws)
	if err != nil {
		t.Fatalf("bindOutbound: %v", err)
	}
	f.ctx = ctx
	return f
}

func (f *targetGraphFixture) graphDir() string {
	return filepath.Join(f.ws.GraphsDir(), f.req.RunID)
}

func (f *targetGraphFixture) createdManagers() []*mcp.Manager {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*mcp.Manager(nil), f.managers...)
}

// assertNoLeftovers checks the failure contract: every manager the builder
// created is closed and the run's graph directory is gone.
func (f *targetGraphFixture) assertNoLeftovers() {
	f.t.Helper()
	for i, manager := range f.createdManagers() {
		if _, err := manager.RunIndex(context.Background(), f.req.BaseDir); !errors.Is(err, mcp.ErrClosed) {
			f.t.Errorf("manager %d is still open: %v", i, err)
		}
	}
	assertMissing(f.t, f.graphDir())
}

func (f *targetGraphFixture) assertNothingCreated() {
	f.t.Helper()
	if got := f.createdManagers(); len(got) != 0 {
		f.t.Errorf("created %d managers, want none", len(got))
	}
	assertMissing(f.t, f.graphDir())
}

func requireTargetGraphUnavailable(t *testing.T, graph *targetGraph, err error, wantClass string) {
	t.Helper()
	if graph != nil {
		t.Fatalf("graph = %+v, want nil", graph)
	}
	var unavailable *targetGraphUnavailable
	if !errors.As(err, &unavailable) || unavailable.class != wantClass {
		t.Fatalf("err = %v, want target graph unavailable with class %q", err, wantClass)
	}
	if got := errclass.Of(err); got != wantClass {
		t.Fatalf("errclass.Of = %q, want %q", got, wantClass)
	}
	if strings.Contains(err.Error(), targetGraphTestCanary) {
		t.Fatalf("error leaks the underlying failure: %v", err)
	}
}

func requirePlainContextError(t *testing.T, graph *targetGraph, err, want error) {
	t.Helper()
	if graph != nil {
		t.Fatalf("graph = %+v, want nil", graph)
	}
	var unavailable *targetGraphUnavailable
	if !errors.Is(err, want) || errors.As(err, &unavailable) {
		t.Fatalf("err = %v, want a plain %v that callers do not mistake for a degradation", err, want)
	}
}

func TestTargetGraphBuildSkipsWithoutLocalIndexedServers(t *testing.T) {
	indexCommand := mcp.XReviewConfig{IndexCommand: []string{indexTrueCommand}}
	for _, tc := range []struct {
		name    string
		servers map[string]mcp.ServerConfig
	}{
		{"no servers", nil},
		{"server without an index command", map[string]mcp.ServerConfig{
			"graph": {Command: indexTrueCommand},
		}},
		{"remote indexed server", map[string]mcp.ServerConfig{
			"graph": {URL: "http://127.0.0.1:1/mcp", XReview: indexCommand},
		}},
		{"remote indexed server next to a local one", map[string]mcp.ServerConfig{
			"graph":  {Command: indexTrueCommand, XReview: indexCommand},
			"remote": {URL: "http://127.0.0.1:1/mcp", XReview: indexCommand},
		}},
		{"remote server typed explicitly", map[string]mcp.ServerConfig{
			"graph": {Type: "http", URL: "http://127.0.0.1:1/mcp", XReview: indexCommand},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTargetGraphFixture(t)
			f.req.MCP = mcp.Config{MCPServers: tc.servers}
			graph, err := f.builder.Build(f.ctx, f.req)
			if graph != nil || err != nil {
				t.Fatalf("Build = %+v, %v; want nil, nil", graph, err)
			}
			f.assertNothingCreated()
			if calls := f.git.resolveCalls(); len(calls) != 0 {
				t.Fatalf("git consulted without a local index: %v", calls)
			}
		})
	}
}

func TestTargetGraphBuildGivesTheServerAnIsolatedWorkspace(t *testing.T) {
	f := newTargetGraphFixture(t)
	f.req.MCP = mcp.Config{MCPServers: map[string]mcp.ServerConfig{
		"graph": {Command: indexTrueCommand, XReview: mcp.XReviewConfig{IndexCommand: []string{indexTrueCommand}}},
		"notes": {Command: indexTrueCommand},
		"docs":  {URL: "http://127.0.0.1:1/mcp"},
	}}
	var indexedBase string
	f.builder.runIndex = func(_ context.Context, manager *mcp.Manager, baseDir string) (int, error) {
		indexedBase = baseDir
		if info, err := os.Stat(f.graphDir()); err != nil || !info.IsDir() {
			t.Errorf("graph dir missing while indexing: %v", err)
		}
		return len(manager.Servers()), nil
	}

	graph, err := f.builder.Build(f.ctx, f.req)
	if err != nil || graph == nil {
		t.Fatalf("Build = %+v, %v", graph, err)
	}
	defer graph.Close()

	if indexedBase != f.req.BaseDir {
		t.Fatalf("indexed %q, want the review base %q", indexedBase, f.req.BaseDir)
	}
	if graph.dir != f.graphDir() {
		t.Fatalf("graph dir = %q, want %q", graph.dir, f.graphDir())
	}
	if !graph.servers["graph"] || len(graph.servers) != 1 {
		t.Fatalf("graph servers = %v, want only the indexed one", graph.servers)
	}
	options := f.options
	if len(options) != 1 {
		t.Fatalf("managers created = %d, want 1", len(options))
	}
	opts := options[0]
	if opts.Workspace != graph.dir {
		t.Fatalf("manager workspace = %q, want the run's graph dir %q", opts.Workspace, graph.dir)
	}
	if got := opts.Platform.MCPServers; len(got) != 1 || len(got["graph"].XReview.IndexCommand) == 0 {
		t.Fatalf("manager servers = %v, want exactly the indexed server", got)
	}
	if len(opts.Project.MCPServers) != 0 {
		t.Fatalf("manager project servers = %v, want none", opts.Project.MCPServers)
	}
	if !slices.Equal(opts.DeniedEnv, f.req.DeniedEnv) {
		t.Fatalf("manager denied env = %v, want %v", opts.DeniedEnv, f.req.DeniedEnv)
	}
	if calls := f.git.resolveCalls(); !slices.Equal(calls, []string{f.req.BaseDir + "@HEAD", f.req.BaseDir + "@HEAD"}) {
		t.Fatalf("git calls = %v, want HEAD of the base before and after indexing", calls)
	}
}

// The default seams run the configured index command for real: the server's
// HOME lands inside the run's graph directory, which Close then removes.
func TestTargetGraphBuildRunsTheIndexCommandInsideTheRunDirectory(t *testing.T) {
	f := newTargetGraphFixture(t)
	f.req.StartSHA = strings.ToUpper(targetGraphTestStart)

	graph, err := f.builder.Build(f.ctx, f.req)
	if err != nil || graph == nil {
		t.Fatalf("Build = %+v, %v", graph, err)
	}
	home := filepath.Join(graph.dir, ".mcp", "graph", "home")
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		t.Fatalf("server home %s: %v", home, err)
	}
	if _, err := graph.manager.RunIndex(context.Background(), f.req.BaseDir); err != nil {
		t.Fatalf("the built manager must stay usable until Close: %v", err)
	}
	if got := len(f.builder.slots); got != 0 {
		t.Fatalf("slots held after Build = %d, want 0", got)
	}

	graph.Close()
	f.assertNoLeftovers()
}

func TestTargetGraphBuildDegradesWhenBaseIsNotTheStartSHA(t *testing.T) {
	for _, tc := range []struct {
		name  string
		heads []string
		err   error
	}{
		{name: "another commit", heads: []string{targetGraphTestOther}},
		{name: "not an object ID", heads: []string{"HEAD"}},
		{name: "git failure", err: errors.New(targetGraphTestCanary)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTargetGraphFixture(t)
			f.git.heads, f.git.err = tc.heads, tc.err
			// A crashed earlier attempt of the same run left its graph behind.
			stale := filepath.Join(f.graphDir(), ".mcp", "graph", "home")
			if err := os.MkdirAll(stale, 0o700); err != nil {
				t.Fatal(err)
			}
			graph, err := f.builder.Build(f.ctx, f.req)
			requireTargetGraphUnavailable(t, graph, err, "base_unverified")
			f.assertNothingCreated()
			if calls := f.git.resolveCalls(); !slices.Equal(calls, []string{f.req.BaseDir + "@HEAD"}) {
				t.Fatalf("git calls = %v, want one HEAD lookup of the base", calls)
			}
		})
	}
}

func TestTargetGraphBuildDegradesWhenBaseMovesWhileIndexing(t *testing.T) {
	f := newTargetGraphFixture(t)
	f.git.heads = []string{targetGraphTestStart, targetGraphTestOther}
	f.builder.runIndex = func(context.Context, *mcp.Manager, string) (int, error) { return 1, nil }

	graph, err := f.builder.Build(f.ctx, f.req)
	requireTargetGraphUnavailable(t, graph, err, "base_unverified")
	f.assertNoLeftovers()
	if len(f.createdManagers()) != 1 {
		t.Fatalf("managers created = %d, want the one that indexed", len(f.createdManagers()))
	}
}

func TestTargetGraphBuildDegradesOnIndexFailure(t *testing.T) {
	f := newTargetGraphFixture(t)
	f.builder.runIndex = func(context.Context, *mcp.Manager, string) (int, error) {
		return 0, errors.New(targetGraphTestCanary)
	}
	graph, err := f.builder.Build(f.ctx, f.req)
	requireTargetGraphUnavailable(t, graph, err, "index_failed")
	f.assertNoLeftovers()
	if len(f.createdManagers()) != 1 {
		t.Fatalf("managers created = %d, want 1", len(f.createdManagers()))
	}
	if got := len(f.builder.slots); got != 0 {
		t.Fatalf("slots held after a failed Build = %d, want 0", got)
	}
}

func TestTargetGraphBuildDegradesOnIncompleteIndex(t *testing.T) {
	f := newTargetGraphFixture(t)
	f.req.MCP.MCPServers["code"] = mcp.ServerConfig{Command: indexTrueCommand, XReview: mcp.XReviewConfig{IndexCommand: []string{indexTrueCommand}}}
	f.builder.runIndex = func(context.Context, *mcp.Manager, string) (int, error) { return 1, nil }

	graph, err := f.builder.Build(f.ctx, f.req)
	requireTargetGraphUnavailable(t, graph, err, "index_incomplete")
	f.assertNoLeftovers()
}

func TestTargetGraphBuildDegradesWhenTheGraphCannotBeSetUp(t *testing.T) {
	t.Run("manager", func(t *testing.T) {
		f := newTargetGraphFixture(t)
		f.builder.newManager = func(mcp.Options) (*mcp.Manager, error) {
			return nil, errors.New(targetGraphTestCanary)
		}
		graph, err := f.builder.Build(f.ctx, f.req)
		requireTargetGraphUnavailable(t, graph, err, "setup_failed")
		assertMissing(t, f.graphDir())
	})
	t.Run("graphs directory", func(t *testing.T) {
		f := newTargetGraphFixture(t)
		if err := f.ws.EnsureRoot(); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		if err := os.Symlink(outside, f.ws.GraphsDir()); err != nil {
			t.Fatal(err)
		}
		graph, err := f.builder.Build(f.ctx, f.req)
		requireTargetGraphUnavailable(t, graph, err, "setup_failed")
		f.assertNothingCreated()
		if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
			t.Fatalf("build wrote through the symlink: %v, %v", entries, err)
		}
	})
}

func TestTargetGraphBuildDegradesOnTimeout(t *testing.T) {
	t.Run("waiting for a slot", func(t *testing.T) {
		f := newTargetGraphFixture(t)
		f.builder.timeout = 30 * time.Millisecond
		f.builder.slots <- struct{}{}
		graph, err := f.builder.Build(f.ctx, f.req)
		requireTargetGraphUnavailable(t, graph, err, "timeout")
		f.assertNothingCreated()
		if calls := f.git.resolveCalls(); len(calls) != 0 {
			t.Fatalf("git consulted before a slot was held: %v", calls)
		}
		if got := len(f.builder.slots); got != 1 {
			t.Fatalf("slots held = %d, want the occupant's slot untouched", got)
		}
	})
	t.Run("indexing", func(t *testing.T) {
		f := newTargetGraphFixture(t)
		f.builder.timeout = 30 * time.Millisecond
		f.builder.runIndex = func(ctx context.Context, _ *mcp.Manager, _ string) (int, error) {
			<-ctx.Done()
			return 0, ctx.Err()
		}
		graph, err := f.builder.Build(f.ctx, f.req)
		requireTargetGraphUnavailable(t, graph, err, "timeout")
		f.assertNoLeftovers()
		if got := len(f.builder.slots); got != 0 {
			t.Fatalf("slots held after a timeout = %d, want 0", got)
		}
	})
}

// A caller that is done is not a degradation: the review itself is stopping.
func TestTargetGraphBuildPassesCallerCancellationThrough(t *testing.T) {
	t.Run("deadline while waiting for a slot", func(t *testing.T) {
		f := newTargetGraphFixture(t)
		f.builder.slots <- struct{}{}
		ctx, cancel := context.WithTimeout(f.ctx, 30*time.Millisecond)
		defer cancel()
		graph, err := f.builder.Build(ctx, f.req)
		requirePlainContextError(t, graph, err, context.DeadlineExceeded)
		f.assertNothingCreated()
	})
	t.Run("already cancelled", func(t *testing.T) {
		f := newTargetGraphFixture(t)
		ctx, cancel := context.WithCancel(f.ctx)
		cancel()
		graph, err := f.builder.Build(ctx, f.req)
		requirePlainContextError(t, graph, err, context.Canceled)
		f.assertNothingCreated()
	})
	t.Run("cancelled while indexing", func(t *testing.T) {
		f := newTargetGraphFixture(t)
		ctx, cancel := context.WithCancel(f.ctx)
		defer cancel()
		started := make(chan struct{})
		f.builder.runIndex = func(ctx context.Context, _ *mcp.Manager, _ string) (int, error) {
			close(started)
			<-ctx.Done()
			return 0, ctx.Err()
		}
		type result struct {
			graph *targetGraph
			err   error
		}
		done := make(chan result, 1)
		go func() {
			graph, err := f.builder.Build(ctx, f.req)
			done <- result{graph, err}
		}()
		<-started
		cancel()
		got := <-done
		requirePlainContextError(t, got.graph, got.err, context.Canceled)
		f.assertNoLeftovers()
		if n := len(f.builder.slots); n != 0 {
			t.Fatalf("slots held after cancellation = %d, want 0", n)
		}
	})
}

// A revoked outbound policy stops the review like any other outbound send, so
// it is not reported as a graph the review can do without.
func TestTargetGraphBuildPassesOutboundInvalidationThrough(t *testing.T) {
	t.Run("policy changes while indexing", func(t *testing.T) {
		f := newTargetGraphFixture(t)
		started := make(chan struct{})
		f.builder.runIndex = func(ctx context.Context, _ *mcp.Manager, _ string) (int, error) {
			close(started)
			<-ctx.Done()
			return 0, ctx.Err()
		}
		type result struct {
			graph *targetGraph
			err   error
		}
		done := make(chan result, 1)
		go func() {
			graph, err := f.builder.Build(f.ctx, f.req)
			done <- result{graph, err}
		}()
		<-started
		if err := f.ws.ChangeOutbound(context.Background(), func(context.Context) error { return nil }); err != nil {
			t.Fatalf("ChangeOutbound: %v", err)
		}
		got := <-done
		var unavailable *targetGraphUnavailable
		if got.graph != nil || !errors.Is(got.err, workspace.ErrOutboundInvalidated) || errors.As(got.err, &unavailable) {
			t.Fatalf("Build = %+v, %v; want ErrOutboundInvalidated", got.graph, got.err)
		}
		f.assertNoLeftovers()
	})
	t.Run("context without an outbound binding", func(t *testing.T) {
		f := newTargetGraphFixture(t)
		calls := 0
		f.builder.runIndex = func(context.Context, *mcp.Manager, string) (int, error) {
			calls++
			return 1, nil
		}
		graph, err := f.builder.Build(context.Background(), f.req)
		if graph != nil || !errors.Is(err, workspace.ErrOutboundInvalidated) {
			t.Fatalf("Build = %+v, %v; want ErrOutboundInvalidated", graph, err)
		}
		if calls != 0 {
			t.Fatal("the index command ran outside the outbound gate")
		}
		f.assertNoLeftovers()
	})
}

func TestTargetGraphBuildReleasesItsSlot(t *testing.T) {
	f := newTargetGraphFixture(t)
	f.builder.timeout = 5 * time.Second
	indexErr := errors.New("index failed")
	f.builder.runIndex = func(context.Context, *mcp.Manager, string) (int, error) {
		if indexErr != nil {
			return 0, indexErr
		}
		return 1, nil
	}
	if graph, err := f.builder.Build(f.ctx, f.req); graph != nil || err == nil {
		t.Fatalf("first Build = %+v, %v; want a failure", graph, err)
	}
	indexErr = nil
	graph, err := f.builder.Build(f.ctx, f.req)
	if err != nil || graph == nil {
		t.Fatalf("second Build with a single slot = %+v, %v", graph, err)
	}
	if got := len(f.builder.slots); got != 0 {
		t.Fatalf("slots held after Build = %d, want 0", got)
	}
	graph.Close()
}

func TestTargetGraphBuildRejectsInvalidRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*targetGraphRequest)
	}{
		{"no workspace", func(r *targetGraphRequest) { r.Workspace = nil }},
		{"workspace of another project", func(r *targetGraphRequest) { r.ProjectID++ }},
		{"unsafe run ID", func(r *targetGraphRequest) { r.RunID = "../evil" }},
		{"empty run ID", func(r *targetGraphRequest) { r.RunID = "" }},
		{"empty base directory", func(r *targetGraphRequest) { r.BaseDir = " " }},
		{"start SHA is a ref", func(r *targetGraphRequest) { r.StartSHA = "main" }},
		{"start SHA is short", func(r *targetGraphRequest) { r.StartSHA = targetGraphTestStart[:12] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTargetGraphFixture(t)
			tc.change(&f.req)
			graph, err := f.builder.Build(f.ctx, f.req)
			requireTargetGraphUnavailable(t, graph, err, "invalid_request")
			if got := f.createdManagers(); len(got) != 0 {
				t.Fatalf("managers created = %d, want none", len(got))
			}
			if _, statErr := os.Lstat(f.ws.GraphsDir()); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("graphs dir touched for an invalid request: %v", statErr)
			}
		})
	}
}

func TestTargetGraphCloseStopsTheServerBeforeRemovingItsDirectory(t *testing.T) {
	var nilGraph *targetGraph
	nilGraph.Close()

	workspaceDir := t.TempDir()
	manager, err := mcp.New(mcp.Options{
		Workspace: workspaceDir,
		Platform: mcp.Config{MCPServers: map[string]mcp.ServerConfig{
			"graph": {Command: indexTrueCommand, XReview: mcp.XReviewConfig{IndexCommand: []string{indexTrueCommand}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	removed := 0
	graph := &targetGraph{
		manager: manager,
		servers: map[string]bool{"graph": true},
		dir:     workspaceDir,
		remove: func() error {
			removed++
			if _, err := manager.RunIndex(context.Background(), workspaceDir); !errors.Is(err, mcp.ErrClosed) {
				t.Errorf("directory removed while the manager is open: %v", err)
			}
			return nil
		},
	}
	graph.Close()
	graph.Close()
	if removed != 1 {
		t.Fatalf("remove called %d times, want exactly once", removed)
	}
	if _, err := manager.RunIndex(context.Background(), workspaceDir); !errors.Is(err, mcp.ErrClosed) {
		t.Fatalf("manager is still open after Close: %v", err)
	}

	// A graph without a manager or a remove hook still closes.
	(&targetGraph{}).Close()
}

// newCountingMCP serves the named read-only tools over HTTP and counts every
// request that reaches it.
func newCountingMCP(t *testing.T, tools ...string) (url string, hits *atomic.Int64) {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "fake", Version: "1.0.0"}, nil)
	for _, name := range tools {
		server.AddTool(&sdk.Tool{
			Name:        name,
			Description: name,
			InputSchema: json.RawMessage(`{"type":"object"}`),
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil
		})
	}
	handler := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	hits = new(atomic.Int64)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	return httpServer.URL + "/mcp", hits
}

func newRoutingManager(t *testing.T, servers map[string]string) *mcp.Manager {
	t.Helper()
	configured := make(map[string]mcp.ServerConfig, len(servers))
	for name, url := range servers {
		configured[name] = mcp.ServerConfig{URL: url}
	}
	manager, err := mcp.New(mcp.Options{Workspace: t.TempDir(), Platform: mcp.Config{MCPServers: configured}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

type routingFixture struct {
	shared, target  *mcp.Manager
	sharedGraphHits *atomic.Int64
	targetGraphHits *atomic.Int64
	docsHits        *atomic.Int64
	graph           *targetGraph
	blocks          map[string]config.BlockBinding
	originalTools   []string
}

// newRoutingFixture builds a shared manager that knows both servers and a
// target manager that knows only the index-backed one. The two "graph"
// servers are separate processes so a request shows who served the tool.
func newRoutingFixture(t *testing.T) *routingFixture {
	t.Helper()
	sharedGraphURL, sharedGraphHits := newCountingMCP(t, "search_graph", "trace_path")
	targetGraphURL, targetGraphHits := newCountingMCP(t, "search_graph", "trace_path")
	docsURL, docsHits := newCountingMCP(t, "lookup")
	f := &routingFixture{
		shared:          newRoutingManager(t, map[string]string{"graph": sharedGraphURL, "docs": docsURL}),
		target:          newRoutingManager(t, map[string]string{"graph": targetGraphURL}),
		sharedGraphHits: sharedGraphHits,
		targetGraphHits: targetGraphHits,
		docsHits:        docsHits,
		blocks: map[string]config.BlockBinding{
			"code-review": {MCPTools: []string{"mcp__graph__search_graph", "mcp__docs__lookup", "mcp__graph__trace_path"}, Skills: []string{"s"}},
			"verify":      {MCPTools: []string{"mcp__docs__lookup"}},
			"summary":     {MCPTools: []string{"mcp__graph__search_graph"}},
			"empty":       {},
		},
	}
	f.graph = &targetGraph{manager: f.target, servers: map[string]bool{"graph": true}}
	f.originalTools = slices.Clone(f.blocks["code-review"].MCPTools)
	return f
}

func TestRouteTargetGraphToolsServesIndexBackedToolsFromTheTargetManagerOnly(t *testing.T) {
	f := newRoutingFixture(t)
	provider := routeTargetGraphTools(f.shared, f.graph, f.blocks)
	ctx := context.Background()

	for block, want := range map[string][]string{
		"code-review": {"mcp__docs__lookup", "mcp__graph__search_graph", "mcp__graph__trace_path"},
		"verify":      {"mcp__docs__lookup"},
		"summary":     {"mcp__graph__search_graph"},
		"empty":       nil,
		"unknown":     nil,
	} {
		tools, err := provider.Tools(ctx, block)
		if err != nil {
			t.Fatalf("Tools(%s): %v", block, err)
		}
		var got []string
		for _, tool := range tools {
			got = append(got, tool.Def().Name)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("Tools(%s) = %v, want %v", block, got, want)
		}
	}
	if got := f.sharedGraphHits.Load(); got != 0 {
		t.Fatalf("the shared manager served %d requests for an index-backed server, want none", got)
	}
	if f.targetGraphHits.Load() == 0 || f.docsHits.Load() == 0 {
		t.Fatalf("target hits = %d, docs hits = %d; each server must serve its own tools", f.targetGraphHits.Load(), f.docsHits.Load())
	}
	if got := f.blocks["code-review"].MCPTools; !slices.Equal(got, f.originalTools) {
		t.Fatalf("routing mutated the caller's bindings: %v", got)
	}
	if _, ok := provider.(review.ToolBindingProvider); !ok {
		t.Fatal("routed provider must expose binding names like the plain provider")
	}
}

func TestRouteTargetGraphToolsDoesNotFallBackToTheSharedManager(t *testing.T) {
	f := newRoutingFixture(t)
	provider := routeTargetGraphTools(f.shared, f.graph, f.blocks)
	if err := f.target.Close(); err != nil {
		t.Fatal(err)
	}

	tools, err := provider.Tools(context.Background(), "summary")
	if !errors.Is(err, mcp.ErrClosed) || len(tools) != 0 {
		t.Fatalf("Tools = %d, %v; want the target manager's failure", len(tools), err)
	}
	if got := f.sharedGraphHits.Load(); got != 0 {
		t.Fatalf("the shared index server was asked for tools after the target failed: %d requests", got)
	}
}

func TestRouteTargetGraphToolsReportsAHardErrorFromEitherSide(t *testing.T) {
	t.Run("shared manager", func(t *testing.T) {
		f := newRoutingFixture(t)
		provider := routeTargetGraphTools(f.shared, f.graph, f.blocks)
		if err := f.shared.Close(); err != nil {
			t.Fatal(err)
		}
		tools, err := provider.Tools(context.Background(), "code-review")
		if !errors.Is(err, mcp.ErrClosed) || len(tools) != 0 {
			t.Fatalf("Tools = %d, %v; want the shared manager's failure", len(tools), err)
		}
		var partial *review.PartialToolsError
		if errors.As(err, &partial) {
			t.Fatal("a hard failure must not be reported as a partial binding")
		}
	})
	t.Run("target manager", func(t *testing.T) {
		f := newRoutingFixture(t)
		provider := routeTargetGraphTools(f.shared, f.graph, f.blocks)
		if err := f.target.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Tools(context.Background(), "code-review"); !errors.Is(err, mcp.ErrClosed) {
			t.Fatalf("Tools = %v; want the target manager's failure", err)
		}
	})
}

func TestRouteTargetGraphToolsMergesPartialBindings(t *testing.T) {
	f := newRoutingFixture(t)
	f.blocks["code-review"] = config.BlockBinding{MCPTools: []string{
		"mcp__graph__search_graph",
		"mcp__graph__missing_graph_tool",
		"mcp__docs__lookup",
		"mcp__docs__missing_docs_tool",
	}}
	provider := routeTargetGraphTools(f.shared, f.graph, f.blocks)

	tools, err := provider.Tools(context.Background(), "code-review")
	var partial *review.PartialToolsError
	if !errors.As(err, &partial) {
		t.Fatalf("Tools = %d, %v; want one partial tool set error", len(tools), err)
	}
	var got []string
	for _, tool := range partial.Tools {
		got = append(got, tool.Def().Name)
	}
	slices.Sort(got)
	if want := []string{"mcp__docs__lookup", "mcp__graph__search_graph"}; !slices.Equal(got, want) {
		t.Fatalf("partial tools = %v, want %v", got, want)
	}
	if len(partial.Gaps) != 2 {
		t.Fatalf("gaps = %q, want one per missing tool from both sides", partial.Gaps)
	}
	joined := strings.Join(partial.Gaps, "\n")
	for _, missing := range []string{"missing_graph_tool", "missing_docs_tool"} {
		if !strings.Contains(joined, missing) {
			t.Fatalf("gaps %q do not name %s", partial.Gaps, missing)
		}
	}
}

func TestRouteTargetGraphToolsKeepsTheBindingNamesOfThePlainProvider(t *testing.T) {
	f := newRoutingFixture(t)
	f.blocks["code-review"] = config.BlockBinding{MCPTools: []string{
		"mcp__graph__search_graph",
		"mcp__docs__lookup",
		"mcp__gone__tool", // a server nobody configured is filtered out
		"not-a-tool-name", // malformed names are left for the manager to reject
		"mcp__graph__trace_path",
	}}
	ctx := context.Background()

	original, err := NewMCPToolProvider(f.shared, f.blocks).(review.ToolBindingProvider).BoundToolNames(ctx, "code-review")
	if err != nil {
		t.Fatal(err)
	}
	routed, err := routeTargetGraphTools(f.shared, f.graph, f.blocks).(review.ToolBindingProvider).BoundToolNames(ctx, "code-review")
	if err != nil {
		t.Fatal(err)
	}
	if len(original) != 4 {
		t.Fatalf("plain binding names = %v; the fixture should keep four", original)
	}
	slices.Sort(original)
	slices.Sort(routed)
	if !slices.Equal(routed, original) {
		t.Fatalf("routed binding names = %v, want %v", routed, original)
	}
	if got := f.sharedGraphHits.Load() + f.targetGraphHits.Load() + f.docsHits.Load(); got != 0 {
		t.Fatalf("listing bindings contacted MCP servers: %d requests", got)
	}

	// Shared tools come first, then the target graph's.
	ordered, _ := routeTargetGraphTools(f.shared, f.graph, f.blocks).(review.ToolBindingProvider).BoundToolNames(ctx, "code-review")
	if want := []string{"mcp__docs__lookup", "not-a-tool-name", "mcp__graph__search_graph", "mcp__graph__trace_path"}; !slices.Equal(ordered, want) {
		t.Fatalf("binding order = %v, want %v", ordered, want)
	}
}

func TestRouteTargetGraphToolsWithoutAGraphIsThePlainProvider(t *testing.T) {
	f := newRoutingFixture(t)
	provider := routeTargetGraphTools(f.shared, nil, f.blocks)
	tools, err := provider.Tools(context.Background(), "summary")
	if err != nil || len(tools) != 1 {
		t.Fatalf("Tools = %d, %v; want the shared graph tool", len(tools), err)
	}
	if f.sharedGraphHits.Load() == 0 || f.targetGraphHits.Load() != 0 {
		t.Fatalf("shared hits = %d, target hits = %d", f.sharedGraphHits.Load(), f.targetGraphHits.Load())
	}
}
