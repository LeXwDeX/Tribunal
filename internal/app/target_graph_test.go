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
	// production budgets; tests about timing set their own.
	f.builder.slotTimeout = 30 * time.Second
	f.builder.indexTimeout = 30 * time.Second
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
	// The review base is a checkout the workspace made: below its runs directory.
	if err := ws.EnsureRoot(); err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}
	baseDir := filepath.Join(ws.Root, "runs", "run-1-base")
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.req = targetGraphRequest{
		ProjectID: 42,
		Workspace: ws,
		RunID:     "run-1",
		BaseDir:   baseDir,
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

// targetGraphClock is a clock that moves only when a test says so.
type targetGraphClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTargetGraphClock() *targetGraphClock {
	return &targetGraphClock{now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
}

func (c *targetGraphClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *targetGraphClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
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
		f.builder.slotTimeout = 30 * time.Millisecond
		// The wait has a budget of its own: a generous index budget must not
		// let a build queue for longer than that.
		f.builder.indexTimeout = time.Hour
		indexRuns := 0
		f.builder.runIndex = func(context.Context, *mcp.Manager, string) (int, error) {
			indexRuns++
			return 1, nil
		}
		f.builder.slots <- struct{}{}
		graph, err := f.builder.Build(f.ctx, f.req)
		requireTargetGraphUnavailable(t, graph, err, "slot_busy")
		f.assertNothingCreated()
		if calls := f.git.resolveCalls(); len(calls) != 0 {
			t.Fatalf("git consulted before a slot was held: %v", calls)
		}
		if indexRuns != 0 {
			t.Fatalf("the index ran %d times without a slot", indexRuns)
		}
		if got := len(f.builder.slots); got != 1 {
			t.Fatalf("slots held = %d, want the occupant's slot untouched", got)
		}
		// Queueing says nothing about the input: once a slot is free the
		// same input is indexed instead of being reported as recently failed.
		<-f.builder.slots
		graph, err = f.builder.Build(f.ctx, f.req)
		if err != nil || graph == nil {
			t.Fatalf("Build after a busy slot = %v, %v; want a graph", graph, err)
		}
		graph.Close()
		if indexRuns != 1 {
			t.Fatalf("index runs after the slot freed = %d, want 1", indexRuns)
		}
	})
	t.Run("indexing", func(t *testing.T) {
		f := newTargetGraphFixture(t)
		f.builder.indexTimeout = 30 * time.Millisecond
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
	f.builder.slotTimeout = 5 * time.Second
	// The failed input is remembered for a while; the second build comes later.
	clock := newTargetGraphClock()
	f.builder.now = clock.Now
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
	clock.Advance(targetGraphFailureTTL)
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
		{"relative base directory", func(r *targetGraphRequest) { r.BaseDir = "runs/run-1-base" }},
		{"base directory outside the workspace", func(r *targetGraphRequest) { r.BaseDir = filepath.Dir(r.Workspace.Root) }},
		{"base directory of another project", func(r *targetGraphRequest) {
			r.BaseDir = filepath.Join(filepath.Dir(r.Workspace.Root), "43", "base")
		}},
		{"runs directory itself", func(r *targetGraphRequest) { r.BaseDir = filepath.Join(r.Workspace.Root, "runs") }},
		{"graph directory", func(r *targetGraphRequest) { r.BaseDir = filepath.Join(r.Workspace.Root, "graphs", "run-1") }},
		{"base directory that is not clean", func(r *targetGraphRequest) { r.BaseDir = r.Workspace.Root + "/runs/../base" }},
		{"base directory that climbs out of runs", func(r *targetGraphRequest) { r.BaseDir = r.Workspace.Root + "/runs/../../x" }},
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

// The review base is either a checkout of its own below runs/ or, when the
// shared base is already at the start commit, the shared base itself.
func TestTargetGraphBuildAcceptsTheSharedBaseAndRunCheckouts(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  func(f *targetGraphFixture) string
	}{
		{"shared base", func(f *targetGraphFixture) string { return f.ws.BaseDir() }},
		{"run checkout", func(f *targetGraphFixture) string { return filepath.Join(f.ws.Root, "runs", "other-base") }},
		{"deeper in the runs directory", func(f *targetGraphFixture) string { return filepath.Join(f.ws.Root, "runs", "other-base", "sub") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTargetGraphFixture(t)
			f.req.BaseDir = tc.dir(f)
			if err := os.MkdirAll(f.req.BaseDir, 0o700); err != nil {
				t.Fatal(err)
			}
			graph, err := f.builder.Build(f.ctx, f.req)
			if err != nil || graph == nil {
				t.Fatalf("Build(%s) = %+v, %v; want a graph", f.req.BaseDir, graph, err)
			}
			graph.Close()
		})
	}
}

// A restricted ignore policy must never reach an index. Build refuses on its own
// account, with no side effect, even if a caller forgot its gate.
func TestTargetGraphBuildDoesNothingUnderRestrictedIgnorePolicy(t *testing.T) {
	f := newTargetGraphFixture(t)
	indexRuns := 0
	f.builder.runIndex = func(context.Context, *mcp.Manager, string) (int, error) {
		indexRuns++
		return 1, nil
	}
	f.req.Ignore = append(config.DefaultReviewIgnorePatterns(), "private/**")
	// A request that is invalid for another reason still ends the same way: the
	// ignore boundary is checked before anything else is looked at.
	f.req.RunID = "../evil"

	graph, err := f.builder.Build(f.ctx, f.req)
	if graph != nil || err != nil {
		t.Fatalf("Build = %+v, %v; want nil, nil", graph, err)
	}
	f.assertNothingCreated()
	if calls := f.git.resolveCalls(); len(calls) != 0 {
		t.Fatalf("git consulted under a restricted policy: %v", calls)
	}
	if indexRuns != 0 || len(f.builder.slots) != 0 {
		t.Fatalf("index runs = %d, slots held = %d; nothing may start", indexRuns, len(f.builder.slots))
	}
	if _, statErr := os.Lstat(f.ws.GraphsDir()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("graphs dir touched under a restricted policy: %v", statErr)
	}
}

// The built-in ignore defaults only exclude build artifacts and are no
// restriction.
func TestTargetGraphBuildIndexesUnderTheDefaultIgnorePolicy(t *testing.T) {
	f := newTargetGraphFixture(t)
	f.req.Ignore = config.DefaultReviewIgnorePatterns()
	graph, err := f.builder.Build(f.ctx, f.req)
	if err != nil || graph == nil {
		t.Fatalf("Build = %+v, %v; want a graph", graph, err)
	}
	graph.Close()
}

// The index budget starts when the slot is held, not when the build starts
// queueing: a build that waited for longer than the whole index budget still
// gets all of it.
func TestTargetGraphBuildStartsTheIndexBudgetWhenTheSlotIsHeld(t *testing.T) {
	f := newTargetGraphFixture(t)
	f.builder.slotTimeout = time.Minute
	f.builder.indexTimeout = 250 * time.Millisecond
	started := make(chan struct{})
	f.builder.runIndex = func(ctx context.Context, _ *mcp.Manager, _ string) (int, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	}
	f.builder.slots <- struct{}{}
	type result struct {
		graph *targetGraph
		err   error
	}
	done := make(chan result, 1)
	go func() {
		graph, err := f.builder.Build(f.ctx, f.req)
		done <- result{graph, err}
	}()

	time.Sleep(2 * f.builder.indexTimeout)
	<-f.builder.slots
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the index never started: the wait for a slot used up the index budget")
	}
	got := <-done
	requireTargetGraphUnavailable(t, got.graph, got.err, "timeout")
	f.assertNoLeftovers()
	if n := len(f.builder.slots); n != 0 {
		t.Fatalf("slots held after the timeout = %d, want 0", n)
	}
}

// failedTargetGraphBuilds makes every build attempt of f fail with class and
// returns the number of attempts so far, counted as managers the builder began
// to create.
func failedTargetGraphBuilds(t *testing.T, f *targetGraphFixture, class string) *atomic.Int32 {
	t.Helper()
	switch class {
	case targetGraphIndexFailed:
		f.builder.runIndex = func(context.Context, *mcp.Manager, string) (int, error) {
			return 0, errors.New(targetGraphTestCanary)
		}
	case targetGraphIndexIncomplete:
		f.builder.runIndex = func(context.Context, *mcp.Manager, string) (int, error) { return 0, nil }
	case errclass.Timeout:
		f.builder.indexTimeout = 100 * time.Millisecond
		f.builder.runIndex = func(ctx context.Context, _ *mcp.Manager, _ string) (int, error) {
			<-ctx.Done()
			return 0, ctx.Err()
		}
	case targetGraphSetupFailed:
		f.builder.newManager = func(mcp.Options) (*mcp.Manager, error) {
			return nil, errors.New(targetGraphTestCanary)
		}
	default:
		t.Fatalf("no recipe for class %q", class)
	}
	attempts := new(atomic.Int32)
	inner := f.builder.newManager
	f.builder.newManager = func(opts mcp.Options) (*mcp.Manager, error) {
		attempts.Add(1)
		return inner(opts)
	}
	return attempts
}

// An input whose build failed in a way that is likely to repeat is not built
// again for a while: the skip costs no slot, no manager and no index run.
func TestTargetGraphBuildSkipsAnInputThatFailedRecently(t *testing.T) {
	for _, class := range []string{targetGraphIndexFailed, targetGraphIndexIncomplete, errclass.Timeout, targetGraphSetupFailed} {
		t.Run(class, func(t *testing.T) {
			f := newTargetGraphFixture(t)
			clock := newTargetGraphClock()
			f.builder.now = clock.Now
			attempts := failedTargetGraphBuilds(t, f, class)

			graph, err := f.builder.Build(f.ctx, f.req)
			requireTargetGraphUnavailable(t, graph, err, class)
			if got := attempts.Load(); got != 1 {
				t.Fatalf("attempts after the first build = %d, want 1", got)
			}
			resolves := len(f.git.resolveCalls())

			clock.Advance(targetGraphFailureTTL - time.Second)
			graph, err = f.builder.Build(f.ctx, f.req)
			requireTargetGraphUnavailable(t, graph, err, targetGraphRecentlyFailed)
			if got := attempts.Load(); got != 1 || len(f.git.resolveCalls()) != resolves {
				t.Fatalf("a skipped build did work: attempts = %d, git calls = %d (was %d)", got, len(f.git.resolveCalls()), resolves)
			}
			if got := len(f.builder.slots); got != 0 {
				t.Fatalf("slots held after a skipped build = %d, want 0", got)
			}

			clock.Advance(2 * time.Second)
			graph, err = f.builder.Build(f.ctx, f.req)
			requireTargetGraphUnavailable(t, graph, err, class)
			if got := attempts.Load(); got != 2 {
				t.Fatalf("attempts after the memory expired = %d, want 2", got)
			}
		})
	}
}

// The memory is per input: another start commit or another index configuration
// is its own build, and a build that worked clears the note.
func TestTargetGraphBuildFailureMemoryIsPerInput(t *testing.T) {
	f := newTargetGraphFixture(t)
	clock := newTargetGraphClock()
	f.builder.now = clock.Now
	var runs atomic.Int32
	f.builder.runIndex = func(context.Context, *mcp.Manager, string) (int, error) {
		runs.Add(1)
		return 0, errors.New(targetGraphTestCanary)
	}

	graph, err := f.builder.Build(f.ctx, f.req)
	requireTargetGraphUnavailable(t, graph, err, targetGraphIndexFailed)
	graph, err = f.builder.Build(f.ctx, f.req)
	requireTargetGraphUnavailable(t, graph, err, targetGraphRecentlyFailed)
	if got := runs.Load(); got != 1 {
		t.Fatalf("index runs = %d, want the second build skipped", got)
	}

	otherCommit := f.req
	otherCommit.StartSHA = targetGraphTestOther
	f.git.heads = []string{targetGraphTestOther}
	graph, err = f.builder.Build(f.ctx, otherCommit)
	requireTargetGraphUnavailable(t, graph, err, targetGraphIndexFailed)
	if got := runs.Load(); got != 2 {
		t.Fatalf("index runs = %d, want another start commit built on its own", got)
	}

	f.git.heads = []string{targetGraphTestStart}
	otherConfig := f.req
	otherConfig.MCP = mcp.Config{MCPServers: map[string]mcp.ServerConfig{
		"graph": {Command: indexTrueCommand, XReview: mcp.XReviewConfig{IndexCommand: []string{indexTrueCommand, "--other"}}},
	}}
	graph, err = f.builder.Build(f.ctx, otherConfig)
	requireTargetGraphUnavailable(t, graph, err, targetGraphIndexFailed)
	if got := runs.Load(); got != 3 {
		t.Fatalf("index runs = %d, want another index configuration built on its own", got)
	}

	// After the memory expired the input is built, and a success clears the note,
	// so the next failure is a fresh one.
	clock.Advance(targetGraphFailureTTL)
	f.builder.runIndex = func(context.Context, *mcp.Manager, string) (int, error) { return 1, nil }
	graph, err = f.builder.Build(f.ctx, f.req)
	if err != nil || graph == nil {
		t.Fatalf("Build after the memory expired = %+v, %v", graph, err)
	}
	graph.Close()
	if got := len(f.builder.failures.until); got != 2 {
		t.Fatalf("remembered inputs = %d, want only the two inputs that did not build since", got)
	}
}

// Only failures that say something about the input are remembered. A moved base
// and a caller that is done describe one run.
func TestTargetGraphBuildDoesNotRememberFailuresOfASingleRun(t *testing.T) {
	t.Run("base moved", func(t *testing.T) {
		f := newTargetGraphFixture(t)
		clock := newTargetGraphClock()
		f.builder.now = clock.Now
		f.git.heads = []string{targetGraphTestOther}
		graph, err := f.builder.Build(f.ctx, f.req)
		requireTargetGraphUnavailable(t, graph, err, targetGraphBaseUnverified)

		f.git.heads = []string{targetGraphTestStart}
		graph, err = f.builder.Build(f.ctx, f.req)
		if err != nil || graph == nil {
			t.Fatalf("Build with the base back at the start commit = %+v, %v; want a graph", graph, err)
		}
		graph.Close()
	})
	t.Run("caller done", func(t *testing.T) {
		f := newTargetGraphFixture(t)
		clock := newTargetGraphClock()
		f.builder.now = clock.Now
		ctx, cancel := context.WithCancel(f.ctx)
		f.builder.runIndex = func(c context.Context, _ *mcp.Manager, _ string) (int, error) {
			cancel()
			<-c.Done()
			return 0, c.Err()
		}
		graph, err := f.builder.Build(ctx, f.req)
		requirePlainContextError(t, graph, err, context.Canceled)

		f.builder.runIndex = func(context.Context, *mcp.Manager, string) (int, error) { return 1, nil }
		graph, err = f.builder.Build(f.ctx, f.req)
		if err != nil || graph == nil {
			t.Fatalf("Build after a cancelled one = %+v, %v; want a graph", graph, err)
		}
		graph.Close()
	})
}

func TestTargetGraphFailuresKeepInputsApartAndExpire(t *testing.T) {
	var failures targetGraphFailures
	clock := newTargetGraphClock()
	key := targetGraphFailureKey{projectID: 42, startSHA: targetGraphTestStart, config: "a"}
	for name, other := range map[string]targetGraphFailureKey{
		"project": {projectID: 43, startSHA: targetGraphTestStart, config: "a"},
		"commit":  {projectID: 42, startSHA: targetGraphTestOther, config: "a"},
		"config":  {projectID: 42, startSHA: targetGraphTestStart, config: "b"},
	} {
		if failures.recent(other, clock.Now()) {
			t.Fatalf("%s: unknown input reported as failed", name)
		}
	}
	failures.record(key, clock.Now())
	if !failures.recent(key, clock.Now()) {
		t.Fatal("recorded input not reported")
	}
	for name, other := range map[string]targetGraphFailureKey{
		"project": {projectID: 43, startSHA: targetGraphTestStart, config: "a"},
		"commit":  {projectID: 42, startSHA: targetGraphTestOther, config: "a"},
		"config":  {projectID: 42, startSHA: targetGraphTestStart, config: "b"},
	} {
		if failures.recent(other, clock.Now()) {
			t.Fatalf("%s: another input reported as failed", name)
		}
	}
	clock.Advance(targetGraphFailureTTL - time.Nanosecond)
	if !failures.recent(key, clock.Now()) {
		t.Fatal("note expired early")
	}
	clock.Advance(time.Nanosecond)
	if failures.recent(key, clock.Now()) || len(failures.until) != 0 || len(failures.order) != 0 {
		t.Fatalf("expired note kept: until = %v, order = %v", failures.until, failures.order)
	}
}

func TestTargetGraphFailuresAreBoundedAndForgetTheOldestFirst(t *testing.T) {
	var failures targetGraphFailures
	clock := newTargetGraphClock()
	key := func(i int) targetGraphFailureKey {
		return targetGraphFailureKey{projectID: int64(i), startSHA: targetGraphTestStart, config: "a"}
	}
	for i := range targetGraphFailureEntries + 44 {
		failures.record(key(i), clock.Now())
		clock.Advance(time.Second)
	}
	if got := len(failures.until); got != targetGraphFailureEntries || len(failures.order) != targetGraphFailureEntries {
		t.Fatalf("remembered inputs = %d (order %d), want the bound %d", got, len(failures.order), targetGraphFailureEntries)
	}
	if failures.recent(key(0), clock.Now()) || failures.recent(key(43), clock.Now()) {
		t.Fatal("the oldest inputs are still remembered")
	}
	if !failures.recent(key(44), clock.Now()) || !failures.recent(key(targetGraphFailureEntries+43), clock.Now()) {
		t.Fatal("recent inputs were forgotten")
	}
	// A repeated failure refreshes its note instead of adding a second one.
	failures.record(key(44), clock.Now())
	if got := len(failures.until); got != targetGraphFailureEntries {
		t.Fatalf("remembered inputs after a repeat = %d", got)
	}
	if last := failures.order[len(failures.order)-1]; last != key(44) {
		t.Fatalf("repeated input is not the newest: %+v", last)
	}
	// Expired notes make room before live ones are dropped.
	clock.Advance(targetGraphFailureTTL)
	failures.record(key(9999), clock.Now())
	if got := len(failures.until); got != 1 {
		t.Fatalf("remembered inputs after everything expired = %d, want only the new one", got)
	}
}

// The record is shared by every review of the process.
func TestTargetGraphFailuresAreSafeForConcurrentUse(t *testing.T) {
	var failures targetGraphFailures
	now := newTargetGraphClock().Now()
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				key := targetGraphFailureKey{projectID: int64(worker*1000 + i), startSHA: targetGraphTestStart, config: "a"}
				failures.record(key, now)
				failures.recent(key, now)
				if i%3 == 0 {
					failures.forget(key)
				}
			}
		}()
	}
	wg.Wait()
	if got := len(failures.until); got > targetGraphFailureEntries || got != len(failures.order) {
		t.Fatalf("until = %d, order = %d, bound = %d", got, len(failures.order), targetGraphFailureEntries)
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
