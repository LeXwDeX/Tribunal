package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// These tests cover how a review and its recovery use a target graph: the
// per-run code graph built when the shared workspace index describes another
// commit than the review base. The graph builder itself is covered in
// target_graph_test.go; here it is replaced by a fake that records its calls.

// targetGraphEvents orders the steps of one test: builds, graph removals and
// worktree removals share one log, so a test can prove which came first.
type targetGraphEvents struct {
	mu   sync.Mutex
	list []string
}

func (e *targetGraphEvents) add(event string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, event)
}

func (e *targetGraphEvents) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.list)
}

// last returns the position of the latest event, or -1 when it never happened.
func (e *targetGraphEvents) last(event string) int {
	all := e.all()
	for i := len(all) - 1; i >= 0; i-- {
		if all[i] == event {
			return i
		}
	}
	return -1
}

// count returns how many times event happened.
func (e *targetGraphEvents) count(event string) int {
	n := 0
	for _, got := range e.all() {
		if got == event {
			n++
		}
	}
	return n
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// loggedRemovalGit records worktree removals next to the target graph events.
type loggedRemovalGit struct {
	*reviewBaseGitOps
	events *targetGraphEvents
}

func (g *loggedRemovalGit) RemoveWorktree(ctx context.Context, mirror, dir string) error {
	g.events.add("worktree:" + filepath.Base(dir))
	return g.reviewBaseGitOps.RemoveWorktree(ctx, mirror, dir)
}

type targetGraphAnswer func(req targetGraphRequest) (*targetGraph, error)

// recordingTargetGraphBuilder answers the nth Build call with answers[n-1];
// the last answer repeats.
type recordingTargetGraphBuilder struct {
	events *targetGraphEvents

	mu       sync.Mutex
	requests []targetGraphRequest
	answers  []targetGraphAnswer
}

func (b *recordingTargetGraphBuilder) Build(_ context.Context, req targetGraphRequest) (*targetGraph, error) {
	b.events.add("build:" + req.RunID)
	b.mu.Lock()
	b.requests = append(b.requests, req)
	answer := b.answers[min(len(b.requests), len(b.answers))-1]
	b.mu.Unlock()
	return answer(req)
}

func (b *recordingTargetGraphBuilder) calls() []targetGraphRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.requests)
}

func graphUnavailable(class string) targetGraphAnswer {
	return func(targetGraphRequest) (*targetGraph, error) {
		return nil, &targetGraphUnavailable{class: class}
	}
}

func graphFails(err error) targetGraphAnswer {
	return func(targetGraphRequest) (*targetGraph, error) { return nil, err }
}

// targetGraphReviewFixture is a review fixture whose project has one MCP
// server, "graph", that block code-review is bound to. It has an index command,
// which makes it an index-backed server; the builder here is a fake, so it never
// runs. The shared manager serves it from sharedURL; a target graph serves it
// from targetURL, so a request count shows which of the two the review used.
type targetGraphReviewFixture struct {
	*reviewFixture
	t          *testing.T
	builder    *recordingTargetGraphBuilder
	events     *targetGraphEvents
	logs       *lockedBuffer
	sharedURL  string
	sharedHits *atomic.Int64
	targetURL  string
	targetHits *atomic.Int64
}

func newTargetGraphReviewFixture(t *testing.T) *targetGraphReviewFixture {
	t.Helper()
	f := &targetGraphReviewFixture{
		reviewFixture: newReviewFixture(t, reviewTestHead),
		t:             t,
		events:        &targetGraphEvents{},
		logs:          &lockedBuffer{},
	}
	// The per-run base checkout is only available when a fetch brings the MR
	// base commit along.
	f.service.git = &loggedRemovalGit{
		reviewBaseGitOps: &reviewBaseGitOps{fakeGitOps: f.git, commits: map[string]bool{}, bringBase: true},
		events:           f.events,
	}
	f.service.logger = slog.New(slog.NewTextHandler(f.logs, nil))
	f.sharedURL, f.sharedHits = newCountingMCP(t, "search_graph")
	f.targetURL, f.targetHits = newCountingMCP(t, "search_graph")
	f.useExtensions(
		mcp.ServerConfig{URL: f.sharedURL, XReview: mcp.XReviewConfig{IndexCommand: []string{indexTrueCommand}}},
		map[string]config.BlockBinding{review.BlockCodeReview: {MCPTools: []string{"mcp__graph__search_graph"}}},
	)
	f.builder = &recordingTargetGraphBuilder{events: f.events, answers: []targetGraphAnswer{f.liveGraph}}
	f.service.targetGraphs = f.builder
	return f
}

// useExtensions gives the project the one MCP server "graph" and the block
// bindings blocks. The server is configured by server; its tools are always
// served from the fixture's shared URL.
func (f *targetGraphReviewFixture) useExtensions(server mcp.ServerConfig, blocks map[string]config.BlockBinding) {
	f.t.Helper()
	resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{
		Store:     f.db,
		Workspace: f.t.TempDir(),
		Blocks:    blocks,
		MCP:       mcp.Config{MCPServers: map[string]mcp.ServerConfig{"graph": server}},
		DeniedEnv: []string{"GITLAB_TOKEN"},
	})
	if err != nil {
		f.t.Fatalf("NewProjectExtensionResolver: %v", err)
	}
	f.t.Cleanup(func() { _ = resolver.Close() })
	f.service.extensions = resolver
}

// liveGraph answers Build like the production builder after a successful
// index: a private directory for the run and a manager that serves the target
// "graph" server.
func (f *targetGraphReviewFixture) liveGraph(req targetGraphRequest) (*targetGraph, error) {
	dir, err := req.Workspace.EnsureGraphDir(req.RunID)
	if err != nil {
		return nil, err
	}
	manager, err := mcp.New(mcp.Options{
		Workspace: dir,
		Platform:  mcp.Config{MCPServers: map[string]mcp.ServerConfig{"graph": {URL: f.targetURL}}},
	})
	if err != nil {
		return nil, err
	}
	return &targetGraph{
		manager: manager,
		servers: map[string]bool{"graph": true},
		dir:     dir,
		remove: func() error {
			f.events.add("graph:" + req.RunID)
			return req.Workspace.RemoveGraphDir(req.RunID)
		},
	}, nil
}

func (f *targetGraphReviewFixture) answers(answers ...targetGraphAnswer) {
	f.builder.answers = answers
}

// recordAgentRoots wraps the agent runner and returns a reader of the sandbox
// roots every code-review run advertises to its model.
func (f *targetGraphReviewFixture) recordAgentRoots() func() []string {
	var (
		mu   sync.Mutex
		seen []string
	)
	inner := f.service.agentRun
	f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		for _, tool := range spec.Tools {
			if def := tool.Def(); def.Name == "read_file" {
				mu.Lock()
				seen = append(seen, def.Description)
				mu.Unlock()
				break
			}
		}
		return inner(ctx, spec)
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

func (f *targetGraphReviewFixture) restrictIgnore() {
	f.defaults.Ignore = append(config.DefaultReviewIgnorePatterns(), "private/**")
	f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
}

func (f *targetGraphReviewFixture) provenance(jobID string) (*review.Dossier, *review.WorkspaceProvenance) {
	f.t.Helper()
	dossier := readReviewDossier(f.t, f.reviewFixture, jobID)
	if dossier.WorkspaceProvenance == nil {
		f.t.Fatal("dossier has no workspace provenance")
	}
	return dossier, dossier.WorkspaceProvenance
}

func (f *targetGraphReviewFixture) perRunBase(jobID string) string {
	return filepath.Join(f.manager.Get(42).Root, "runs", jobID+"-base")
}

func (f *targetGraphReviewFixture) openWorktrees() int {
	f.git.mu.Lock()
	defer f.git.mu.Unlock()
	return len(f.git.worktrees)
}

func codeReviewToolGaps(t *testing.T, f *reviewFixture, workflowID string) []string {
	t.Helper()
	snapshot, err := f.engine.Snapshot(context.Background(), workflowID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var gaps []string
	for _, node := range snapshot.Nodes {
		if node.Spec.Kind != review.KindCodeReview {
			continue
		}
		var out review.CodeReviewOut
		if err := json.Unmarshal(node.Output, &out); err != nil {
			t.Fatalf("decode code review output: %v", err)
		}
		gaps = append(gaps, out.ToolGaps...)
	}
	return gaps
}

func TestRunReviewBuildsTargetGraphWhenSharedIndexIsAtAnotherCommit(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	start := strings.Repeat("2", 40)
	roots := f.recordAgentRoots()
	job := f.seedReview(t, "target-graph-run")

	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}

	ws := f.manager.Get(42)
	wantBase := f.perRunBase(job.ID)
	calls := f.builder.calls()
	if len(calls) != 1 {
		t.Fatalf("Build calls = %d, want 1", len(calls))
	}
	req := calls[0]
	if req.ProjectID != 42 || req.Workspace != ws || req.RunID != job.ID || req.BaseDir != wantBase || req.StartSHA != start {
		t.Fatalf("request = %+v, want project 42, run %s, base %s, start %s", req, job.ID, wantBase, start)
	}
	if got := req.MCP.MCPServers["graph"].URL; got != f.sharedURL {
		t.Fatalf("request MCP set lacks the project's server: %+v", req.MCP)
	}
	if !slices.Equal(req.DeniedEnv, []string{"GITLAB_TOKEN"}) {
		t.Fatalf("request denied env = %v", req.DeniedEnv)
	}

	dossier, provenance := f.provenance(job.ID)
	if !provenance.MCPEligible || !dossier.MCPIndexEligible() ||
		provenance.IndexSource != review.IndexSourceRun || provenance.IndexSHA != start || provenance.TargetSHA != start {
		t.Fatalf("provenance = %+v, want an eligible run-built graph at %s", provenance, start)
	}
	if dossier.BaseDir != wantBase {
		t.Fatalf("dossier base = %q, want the per-run base %q", dossier.BaseDir, wantBase)
	}

	// The tools came from the run's own graph, never from the shared one.
	if f.sharedHits.Load() != 0 || f.targetHits.Load() == 0 {
		t.Fatalf("shared requests = %d, target requests = %d; the review must read the target graph only", f.sharedHits.Load(), f.targetHits.Load())
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gaps := codeReviewToolGaps(t, f.reviewFixture, wf.ID); wf.Verdict != review.VerdictPass || len(gaps) != 0 {
		t.Fatalf("verdict = %s, gaps = %v; want a clean pass", wf.Verdict, gaps)
	}
	if got := roots(); len(got) == 0 || !strings.Contains(got[0], "Available roots: base, head.") {
		t.Fatalf("agent roots = %q, want the base checkout mounted next to head", got)
	}

	// Closed before the base worktree it indexed is removed, and nothing stays.
	events := f.events.all()
	graphClosed := slices.Index(events, "graph:"+job.ID)
	baseRemoved := slices.Index(events, "worktree:"+job.ID+"-base")
	if graphClosed < 0 || baseRemoved < 0 || graphClosed > baseRemoved {
		t.Fatalf("cleanup order = %v, want the graph closed before the base worktree removal", events)
	}
	assertMissing(t, filepath.Join(ws.GraphsDir(), job.ID))
	assertMissing(t, wantBase)
	if got := f.openWorktrees(); got != 0 {
		t.Fatalf("%d worktrees left behind", got)
	}
}

func TestRunReviewSkipsTargetGraphUnderRestrictedIgnorePolicy(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	f.restrictIgnore()
	job := f.seedReview(t, "target-graph-restricted")

	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}

	if got := len(f.builder.calls()); got != 0 {
		t.Fatalf("Build calls = %d; a restricted ignore policy must never be indexed", got)
	}
	dossier, provenance := f.provenance(job.ID)
	if provenance.MCPEligible || provenance.IndexSource != "" {
		t.Fatalf("provenance = %+v, want no code graph", provenance)
	}
	if !dossierWarningContains(dossier, "without the code graph") {
		t.Fatalf("dossier warnings = %v", dossier.Warnings)
	}
	if f.sharedHits.Load() != 0 || f.targetHits.Load() != 0 {
		t.Fatalf("shared = %d, target = %d; no MCP request may be made", f.sharedHits.Load(), f.targetHits.Load())
	}
}

func TestRunReviewDoesNotBuildTargetGraphWithoutPrerequisites(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *targetGraphReviewFixture)
	}{
		{"no builder", func(f *targetGraphReviewFixture) { f.service.targetGraphs = nil }},
		{"no project extensions", func(f *targetGraphReviewFixture) { f.service.extensions = nil }},
		{"no base checkout", func(f *targetGraphReviewFixture) { f.service.git = f.reviewFixture.git }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newTargetGraphReviewFixture(t)
			tc.setup(f)
			job := f.seedReview(t, "target-graph-prerequisite")

			if err := f.service.RunReview(ctx, job); err != nil {
				t.Fatalf("RunReview: %v", err)
			}

			if got := len(f.builder.calls()); got != 0 {
				t.Fatalf("Build calls = %d, want none", got)
			}
			_, provenance := f.provenance(job.ID)
			if provenance.MCPEligible || provenance.IndexSource != "" {
				t.Fatalf("provenance = %+v, want no code graph", provenance)
			}
		})
	}
}

func TestRunReviewContinuesWithoutTargetGraphWhenUnavailable(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	f.answers(graphUnavailable(targetGraphIndexFailed))
	job := f.seedReview(t, "target-graph-unavailable")

	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}

	if got := len(f.builder.calls()); got != 1 {
		t.Fatalf("Build calls = %d, want 1", got)
	}
	_, provenance := f.provenance(job.ID)
	if provenance.MCPEligible || provenance.IndexSource != "" || provenance.IndexSHA != "" {
		t.Fatalf("provenance = %+v, want the review to run without a code graph", provenance)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	gaps := codeReviewToolGaps(t, f.reviewFixture, wf.ID)
	if wf.Verdict != review.VerdictIncomplete || len(gaps) != 1 || !strings.Contains(gaps[0], "1 bound tool(s) blocked by workspace index identity") {
		t.Fatalf("verdict = %s, gaps = %v; want the existing disclosed tool gap", wf.Verdict, gaps)
	}
	if f.sharedHits.Load() != 0 || f.targetHits.Load() != 0 {
		t.Fatalf("shared = %d, target = %d; withheld tools must not contact a server", f.sharedHits.Load(), f.targetHits.Load())
	}

	var warning string
	for _, line := range strings.Split(f.logs.String(), "\n") {
		if strings.Contains(line, "target code graph unavailable; continuing without it") {
			warning = line
		}
	}
	if !strings.Contains(warning, "level=WARN") || !strings.Contains(warning, "project_id=42") ||
		!strings.Contains(warning, "error_class="+targetGraphIndexFailed) || strings.Contains(warning, "error=") {
		t.Fatalf("log line = %q, want a warning with project_id and error_class only", warning)
	}
	if f.events.count("worktree:"+job.ID+"-base") != 1 || f.openWorktrees() != 0 {
		t.Fatalf("events = %v, worktrees = %d; the base checkout must still be cleaned up", f.events.all(), f.openWorktrees())
	}
}

func TestRunReviewStopsWhenTargetGraphBuildIsInterrupted(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"canceled", context.Canceled},
		{"outbound invalidated", workspace.ErrOutboundInvalidated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newTargetGraphReviewFixture(t)
			f.answers(graphFails(tc.err))
			job := f.seedReview(t, "target-graph-interrupted")

			err := f.service.RunReview(ctx, job)

			if !errors.Is(err, tc.err) || IsPermanentJob(err) {
				t.Fatalf("RunReview err = %v, want the retryable %v", err, tc.err)
			}
			if wf, wfErr := f.db.GetWorkflowByJob(ctx, job.ID); wfErr == nil {
				t.Fatalf("workflow %s was created for an interrupted review", wf.ID)
			}
			events := f.events.all()
			if !slices.Contains(events, "worktree:"+job.ID+"-base") || !slices.Contains(events, "worktree:"+job.ID) || f.openWorktrees() != 0 {
				t.Fatalf("events = %v, worktrees = %d; both checkouts must be cleaned up", events, f.openWorktrees())
			}
			if strings.Contains(f.logs.String(), "target code graph unavailable") {
				t.Fatalf("an interruption was logged as a degradation:\n%s", f.logs.String())
			}
		})
	}
}

func TestRunReviewUsesSharedIndexWithoutTargetGraph(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	start := strings.Repeat("2", 40)
	if _, err := f.db.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", BaseSHA: start, IndexSHA: start}); err != nil {
		t.Fatal(err)
	}
	job := f.seedReview(t, "target-graph-shared-index")

	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}

	if got := len(f.builder.calls()); got != 0 {
		t.Fatalf("Build calls = %d; a matching shared index needs no target graph", got)
	}
	_, provenance := f.provenance(job.ID)
	if !provenance.MCPEligible || provenance.IndexSource != review.IndexSourceWorkspace || provenance.IndexSHA != start {
		t.Fatalf("provenance = %+v, want the shared workspace index", provenance)
	}
	if f.sharedHits.Load() == 0 || f.targetHits.Load() != 0 {
		t.Fatalf("shared = %d, target = %d; the review must read the shared index", f.sharedHits.Load(), f.targetHits.Load())
	}
}

func TestRecoverRebuildsTargetGraphInsteadOfCancelling(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	job := f.seedReview(t, "target-graph-recover")
	id := interruptReview(t, f.reviewFixture, job)
	if _, provenance := f.provenance(job.ID); provenance.IndexSource != review.IndexSourceRun || !provenance.MCPEligible {
		t.Fatalf("interrupted provenance = %+v, want a run-built graph", provenance)
	}
	roots := f.recordAgentRoots()
	targetBefore := f.targetHits.Load()

	if err := f.service.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	assertReviewPublished(t, f.reviewFixture, id)
	calls := f.builder.calls()
	if len(calls) != 2 {
		t.Fatalf("Build calls = %d, want the run and its recovery", len(calls))
	}
	if again := calls[1]; again.RunID != job.ID || again.BaseDir != f.perRunBase(job.ID) || again.StartSHA != strings.Repeat("2", 40) {
		t.Fatalf("recovery request = %+v, want run %s on base %s", again, job.ID, f.perRunBase(job.ID))
	}
	if got := roots(); len(got) == 0 || !strings.Contains(got[0], "Available roots: base, head.") {
		t.Fatalf("recovered agent roots = %q, want the rebuilt base mounted", got)
	}
	if f.sharedHits.Load() != 0 || f.targetHits.Load() == targetBefore {
		t.Fatalf("shared = %d, target = %d (was %d); the recovered review must read the rebuilt graph", f.sharedHits.Load(), f.targetHits.Load(), targetBefore)
	}
	build := f.events.last("build:" + job.ID)
	graphClosed := f.events.last("graph:" + job.ID)
	baseRemoved := f.events.last("worktree:" + job.ID + "-base")
	if f.events.count("graph:"+job.ID) != 2 || build > graphClosed || graphClosed > baseRemoved {
		t.Fatalf("events = %v, want each graph closed, the last one before its base worktree is removed", f.events.all())
	}
	assertMissing(t, filepath.Join(f.manager.Get(42).GraphsDir(), job.ID))
	assertMissing(t, f.perRunBase(job.ID))
}

func TestRecoverCancelsWhenTargetGraphIsUnavailable(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	f.answers(f.liveGraph, graphUnavailable(targetGraphBaseUnverified))
	job := f.seedReview(t, "target-graph-recover-unavailable")
	id := interruptReview(t, f.reviewFixture, job)
	before, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	calls := f.runner.count()

	if err := f.service.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if got := len(f.builder.calls()); got != 2 {
		t.Fatalf("Build calls = %d, want the run and its recovery", got)
	}
	old, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil || old.PublicationState != store.WorkflowPublicationSkipped || !strings.Contains(old.PublicationError, "rerun") || !bytes.Equal(old.Data, before.Data) {
		t.Fatalf("old record = %+v, err = %v; want the snapshot kept and publication skipped", old, err)
	}
	if f.runner.count() != calls {
		t.Fatal("recovery executed a review without its code graph")
	}
	if !strings.Contains(f.logs.String(), "cause=index_ineligible") {
		t.Fatalf("recovery log lacks the cause:\n%s", f.logs.String())
	}
	if f.events.last("worktree:"+job.ID+"-base") < f.events.last("build:"+job.ID) || f.openWorktrees() != 0 {
		t.Fatalf("events = %v, worktrees = %d; the rebuilt base must be removed again", f.events.all(), f.openWorktrees())
	}
}

func TestRecoverReturnsInterruptedTargetGraphBuildWithoutCancelling(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"canceled", context.Canceled},
		{"outbound invalidated", workspace.ErrOutboundInvalidated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newTargetGraphReviewFixture(t)
			f.answers(f.liveGraph, graphFails(tc.err))
			job := f.seedReview(t, "target-graph-recover-interrupted")
			id := interruptReview(t, f.reviewFixture, job)
			calls := f.runner.count()

			err := f.service.Recover(ctx)

			if !errors.Is(err, tc.err) {
				t.Fatalf("Recover err = %v, want %v", err, tc.err)
			}
			snapshot, snapErr := f.engine.Snapshot(ctx, id)
			if snapErr != nil || snapshot.Terminal() {
				t.Fatalf("snapshot = %+v, err = %v; an interruption must leave the workflow to run again", snapshot.Status, snapErr)
			}
			old, recErr := f.db.GetWorkflowDossier(ctx, id)
			if recErr != nil || old.PublicationState == store.WorkflowPublicationSkipped || f.runner.count() != calls {
				t.Fatalf("old record = %+v, err = %v; the workflow must not be cancelled", old, recErr)
			}
			if f.openWorktrees() != 0 {
				t.Fatalf("%d worktrees left behind", f.openWorktrees())
			}
		})
	}
}

func TestRetryResumesWorkflowWhenTargetGraphIsRebuilt(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	job := f.seedReview(t, "target-graph-retry")
	id := interruptReview(t, f.reviewFixture, job)

	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("retried Handle: %v", err)
	}

	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil || wf.ID != id {
		t.Fatalf("workflow = %+v, err = %v; want the interrupted workflow %s resumed in place", wf, err, id)
	}
	assertReviewPublished(t, f.reviewFixture, id)
	calls := f.builder.calls()
	if len(calls) != 2 || calls[1].RunID != job.ID || calls[1].BaseDir != f.perRunBase(job.ID) {
		t.Fatalf("Build calls = %+v, want the graph rebuilt for the same run", calls)
	}
	dossier, provenance := f.provenance(job.ID)
	if !dossier.MCPIndexEligible() || provenance.IndexSource != review.IndexSourceRun {
		t.Fatalf("provenance = %+v, want the stored run-built evidence kept", provenance)
	}
	if f.sharedHits.Load() != 0 {
		t.Fatalf("shared requests = %d; the resumed review must read the rebuilt graph", f.sharedHits.Load())
	}
	if f.events.count("graph:"+job.ID) != 2 {
		t.Fatalf("events = %v, want both graphs closed", f.events.all())
	}
	assertMissing(t, filepath.Join(f.manager.Get(42).GraphsDir(), job.ID))
}

func TestRetryCancelsWorkflowWhenTargetGraphCannotBeRebuilt(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	f.answers(f.liveGraph, graphUnavailable(targetGraphIndexFailed))
	job := f.seedReview(t, "target-graph-retry-unavailable")
	id := interruptReview(t, f.reviewFixture, job)
	before, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("retried Handle: %v", err)
	}

	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil || wf.ID == id {
		t.Fatalf("workflow = %+v, err = %v; want a fresh workflow instead of the graph-less old one", wf, err)
	}
	old, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil || old.PublicationState != store.WorkflowPublicationSkipped || !bytes.Equal(old.Data, before.Data) {
		t.Fatalf("old record = %+v, err = %v; want the snapshot kept and publication skipped", old, err)
	}
	if _, provenance := f.provenance(job.ID); provenance.MCPEligible || provenance.IndexSource != "" {
		t.Fatalf("fresh provenance = %+v, want no code graph", provenance)
	}
}

// TestRunReviewEmptyDiffBuildsNoTargetGraph keeps an MR without reviewable
// files from paying for an index it would never use.
func TestRunReviewEmptyDiffBuildsNoTargetGraph(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	job := f.seedReview(t, "target-graph-empty-diff")
	f.gitlab.diffs = []gitlab.FileDiff{}

	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if got := len(f.builder.calls()); got != 0 {
		t.Fatalf("Build calls = %d; an empty diff must not be indexed", got)
	}
	assertMissing(t, filepath.Join(f.manager.Get(42).GraphsDir(), job.ID))
}

// TestRetryResumesIneligibleWorkflowWithoutTargetGraph keeps a resumed workflow
// whose stored dossier never had a code graph from building one it cannot use.
func TestRetryResumesIneligibleWorkflowWithoutTargetGraph(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	f.answers(graphUnavailable(targetGraphIndexFailed), f.liveGraph)
	job := f.seedReview(t, "target-graph-retry-ineligible")
	id := interruptReview(t, f.reviewFixture, job)

	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("retried Handle: %v", err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil || wf.ID != id {
		t.Fatalf("workflow = %+v, err = %v; want the interrupted workflow %s resumed in place", wf, err, id)
	}
	if got := len(f.builder.calls()); got != 1 {
		t.Fatalf("Build calls = %d, want only the first run's attempt", got)
	}
}

// supersedeAfterBaseGit advances the MR generation, as a newer push would, as
// soon as the review base checkout exists: after the run's own preparation and
// before the first thing that could use a code graph.
type supersedeAfterBaseGit struct {
	GitOps
	once      sync.Once
	supersede func()
}

func (g *supersedeAfterBaseGit) AddWorktree(ctx context.Context, mirror, dir, sha string) error {
	err := g.GitOps.AddWorktree(ctx, mirror, dir, sha)
	if err == nil && strings.HasSuffix(dir, "-base") {
		g.once.Do(g.supersede)
	}
	return err
}

// supersedeOnVersionGitLab advances the MR generation the first time a recovery
// asks GitLab for the latest MR version, which a recovery does after its first
// look at the review control and before it rebuilds a graph.
type supersedeOnVersionGitLab struct {
	*reviewGitLab
	once      sync.Once
	supersede func()
}

func (g *supersedeOnVersionGitLab) LatestMRVersion(ctx context.Context, token string, projectID, iid int64) (*gitlab.Version, error) {
	g.once.Do(g.supersede)
	return g.reviewGitLab.LatestMRVersion(ctx, token, projectID, iid)
}

// newerGeneration enqueues a newer review of the same merge request, which makes
// the generation of every earlier job stale.
func (f *targetGraphReviewFixture) newerGeneration() {
	f.t.Helper()
	if _, err := f.db.EnqueueReviewLatest(context.Background(), store.ReviewCommand{
		ProjectID:   42,
		MRIID:       7,
		HeadSHA:     f.head,
		Payload:     json.RawMessage(`{"trigger":"mr_update","profile":"default","full":true}`),
		MaxAttempts: 3,
	}); err != nil {
		f.t.Errorf("EnqueueReviewLatest: %v", err)
	}
}

// TestRunReviewDoesNotBuildTargetGraphForSupersededRun keeps a run that a newer
// generation has replaced from paying for an index that nobody will read.
func TestRunReviewDoesNotBuildTargetGraphForSupersededRun(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	job := f.seedReview(t, "target-graph-superseded")
	f.service.git = &supersedeAfterBaseGit{GitOps: f.service.git, supersede: f.newerGeneration}

	err := f.service.RunReview(ctx, job)

	if !errors.Is(err, ErrReviewSuperseded) || !IsPermanentJob(err) {
		t.Fatalf("RunReview err = %v, want the permanent ErrReviewSuperseded", err)
	}
	if got := len(f.builder.calls()); got != 0 {
		t.Fatalf("Build calls = %d; a superseded run must not index", got)
	}
	if got := f.openWorktrees(); got != 0 {
		t.Fatalf("%d worktrees left behind", got)
	}
	assertMissing(t, filepath.Join(f.manager.Get(42).GraphsDir(), job.ID))
}

// TestRecoverDoesNotBuildTargetGraphForSupersededWorkflow is the recovery side:
// a successor that appears while the recovery prepares its inputs ends the
// recovery before the base is fetched and indexed.
func TestRecoverDoesNotBuildTargetGraphForSupersededWorkflow(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	job := f.seedReview(t, "target-graph-recover-superseded")
	id := interruptReview(t, f.reviewFixture, job)
	f.service.gitlab = &supersedeOnVersionGitLab{reviewGitLab: f.gitlab, supersede: f.newerGeneration}
	agentCalls := f.runner.count()

	if err := f.service.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if got := len(f.builder.calls()); got != 1 {
		t.Fatalf("Build calls = %d, want only the interrupted run's own", got)
	}
	snapshot, err := f.engine.Snapshot(ctx, id)
	if err != nil || snapshot.Status != dag.WorkflowCancelled {
		t.Fatalf("snapshot = %+v, err = %v; want the superseded workflow cancelled", snapshot.Status, err)
	}
	old, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil || old.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("old record = %+v, err = %v; want publication skipped", old, err)
	}
	if f.runner.count() != agentCalls {
		t.Fatal("a superseded workflow executed review nodes")
	}
	if !strings.Contains(f.logs.String(), "cause=control_not_current") {
		t.Fatalf("recovery log lacks the cause:\n%s", f.logs.String())
	}
	if f.openWorktrees() != 0 {
		t.Fatalf("%d worktrees left behind", f.openWorktrees())
	}
}

// TestRetryDoesNotBuildTargetGraphForWorkflowWhoseConfigChanged keeps a resumable
// workflow that is being replaced anyway from paying for a graph build. The new
// diff has nothing to review, so the replacing run needs no graph either.
func TestRetryDoesNotBuildTargetGraphForWorkflowWhoseConfigChanged(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	job := f.seedReview(t, "target-graph-retry-config")
	id := interruptReview(t, f.reviewFixture, job)
	f.gitlab.mu.Lock()
	f.gitlab.docs["AGENTS.md"] = "changed repository configuration"
	f.gitlab.mu.Unlock()
	f.gitlab.diffs = []gitlab.FileDiff{}

	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("retried Handle: %v", err)
	}

	if got := len(f.builder.calls()); got != 1 {
		t.Fatalf("Build calls = %d, want only the interrupted run's own: a workflow being replaced needs no graph", got)
	}
	snapshot, err := f.engine.Snapshot(ctx, id)
	if err != nil || snapshot.Status != dag.WorkflowCancelled {
		t.Fatalf("snapshot = %+v, err = %v; want the old workflow cancelled", snapshot.Status, err)
	}
	old, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil || old.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("old record = %+v, err = %v; want publication skipped", old, err)
	}
	if f.openWorktrees() != 0 {
		t.Fatalf("%d worktrees left behind", f.openWorktrees())
	}
}

// A restricted ignore policy is a different configuration from the one a stored
// workflow ran under, so the resume path replaces the workflow, and neither it
// nor the run that replaces it indexes anything.
func TestRetryDoesNotBuildTargetGraphUnderRestrictedIgnorePolicy(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	job := f.seedReview(t, "target-graph-retry-restricted")
	id := interruptReview(t, f.reviewFixture, job)
	f.restrictIgnore()

	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("retried Handle: %v", err)
	}

	if got := len(f.builder.calls()); got != 1 {
		t.Fatalf("Build calls = %d; a restricted policy must never be indexed", got)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil || wf.ID == id {
		t.Fatalf("workflow = %+v, err = %v; want a fresh workflow instead of the old one", wf, err)
	}
	dossier, provenance := f.provenance(job.ID)
	if provenance.MCPEligible || provenance.IndexSource != "" || !dossierWarningContains(dossier, "without the code graph") {
		t.Fatalf("provenance = %+v, warnings = %v; want a review without the code graph", provenance, dossier.Warnings)
	}
}

// The recovery of a workflow whose ignore policy has become restricted ends
// before anything is prepared for a graph.
func TestRecoverDoesNotBuildTargetGraphUnderRestrictedIgnorePolicy(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	job := f.seedReview(t, "target-graph-recover-restricted")
	id := interruptReview(t, f.reviewFixture, job)
	f.restrictIgnore()
	agentCalls := f.runner.count()

	if err := f.service.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if got := len(f.builder.calls()); got != 1 {
		t.Fatalf("Build calls = %d; a restricted policy must never be indexed", got)
	}
	snapshot, err := f.engine.Snapshot(ctx, id)
	if err != nil || snapshot.Status != dag.WorkflowCancelled {
		t.Fatalf("snapshot = %+v, err = %v; want the workflow cancelled", snapshot.Status, err)
	}
	if f.runner.count() != agentCalls {
		t.Fatal("recovery executed a review under a changed ignore policy")
	}
	if !strings.Contains(f.logs.String(), "cause=config_changed") {
		t.Fatalf("recovery log lacks the cause:\n%s", f.logs.String())
	}
}

// resolveExecution pins the project's extensions like a review does and returns
// the execution; the caller holds nothing afterwards.
func (f *targetGraphReviewFixture) resolveExecution() *reviewExecution {
	f.t.Helper()
	ctx := context.Background()
	unlock, err := f.manager.Get(42).RLock(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(unlock)
	project, err := f.db.GetProject(ctx, 42)
	if err != nil {
		f.t.Fatal(err)
	}
	execution, err := f.service.resolveReviewExecution(ctx, project, "", review.Profile{Name: "default", Config: f.profiles["default"]}, "main", nil)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(execution.releaseResolverSet)
	return execution
}

// Rebuilding a graph for a restricted policy must not even prepare the base
// checkout: no fetch, no worktree, no Build.
func TestRebuildTargetGraphPreparesNothingUnderRestrictedIgnorePolicy(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	execution := f.resolveExecution()
	project, err := f.db.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	fetches := f.service.git.(*loggedRemovalGit).fetchCount()
	restricted := append(config.DefaultReviewIgnorePatterns(), "private/**")

	base, cleanup, err := f.service.rebuildTargetGraph(ctx, project, "", f.manager.Get(42), execution, 7, "target-graph-rebuild", strings.Repeat("2", 40), "main", restricted)

	if base != "" || cleanup != nil || err != nil {
		t.Fatalf("rebuildTargetGraph = %q, %v, %v; want nothing", base, cleanup != nil, err)
	}
	if got := len(f.builder.calls()); got != 0 {
		t.Fatalf("Build calls = %d, want none", got)
	}
	if got := f.service.git.(*loggedRemovalGit).fetchCount(); got != fetches || f.openWorktrees() != 0 {
		t.Fatalf("fetches = %d (was %d), worktrees = %d; nothing may be prepared", got, fetches, f.openWorktrees())
	}
}

func TestReviewExecutionBindsIndexedTool(t *testing.T) {
	indexed := mcp.ServerConfig{Command: indexTrueCommand, XReview: mcp.XReviewConfig{IndexCommand: []string{indexTrueCommand}}}
	plain := mcp.ServerConfig{Command: indexTrueCommand}
	servers := map[string]mcp.ServerConfig{"graph": indexed, "docs": plain}
	active := map[string]bool{"code-review": true, "verify": true}
	for _, tc := range []struct {
		name      string
		blocks    map[string]config.BlockBinding
		active    map[string]bool
		want      bool
		noServers bool
	}{
		{"bound in an active block", map[string]config.BlockBinding{"code-review": {MCPTools: []string{"mcp__docs__lookup", "mcp__graph__search_graph"}}}, active, true, false},
		{"bound in another active block", map[string]config.BlockBinding{"verify": {MCPTools: []string{"mcp__graph__trace_path"}}}, active, true, false},
		{"bound only in a block the graph does not run", map[string]config.BlockBinding{"reasoner": {MCPTools: []string{"mcp__graph__search_graph"}}}, active, false, false},
		{"active block binds only a server without an index", map[string]config.BlockBinding{"code-review": {MCPTools: []string{"mcp__docs__lookup"}}}, active, false, false},
		{"active block binds nothing", map[string]config.BlockBinding{"code-review": {}}, active, false, false},
		{"malformed tool name", map[string]config.BlockBinding{"code-review": {MCPTools: []string{"graph"}}}, active, false, false},
		{"server nobody configured", map[string]config.BlockBinding{"code-review": {MCPTools: []string{"mcp__gone__tool"}}}, active, false, false},
		{"no active blocks", map[string]config.BlockBinding{"code-review": {MCPTools: []string{"mcp__graph__search_graph"}}}, nil, false, false},
		{"no bindings", nil, active, false, false},
		{"no servers", map[string]config.BlockBinding{"code-review": {MCPTools: []string{"mcp__graph__search_graph"}}}, active, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execution := &reviewExecution{blocks: tc.blocks, activeBlocks: tc.active, mcpConfig: mcp.Config{MCPServers: servers}}
			if tc.noServers {
				execution.mcpConfig = mcp.Config{}
			}
			if got := execution.bindsIndexedTool(); got != tc.want {
				t.Fatalf("bindsIndexedTool() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRunReviewBuildsNoTargetGraphWhenNoActiveBlockBindsAnIndexedTool keeps a
// review from indexing a base that none of its blocks could query.
func TestRunReviewBuildsNoTargetGraphWhenNoActiveBlockBindsAnIndexedTool(t *testing.T) {
	ctx := context.Background()
	f := newTargetGraphReviewFixture(t)
	// The default profile runs code-review and verify. The legacy reasoner
	// passes no block to an agent call, so a binding on it is never used.
	f.useExtensions(
		mcp.ServerConfig{URL: f.sharedURL, XReview: mcp.XReviewConfig{IndexCommand: []string{indexTrueCommand}}},
		map[string]config.BlockBinding{review.BlockReasoner: {MCPTools: []string{"mcp__graph__search_graph"}}},
	)
	job := f.seedReview(t, "target-graph-unbound")

	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}

	if got := len(f.builder.calls()); got != 0 {
		t.Fatalf("Build calls = %d; an index no active block can query must not be built", got)
	}
	_, provenance := f.provenance(job.ID)
	if provenance.MCPEligible || provenance.IndexSource != "" {
		t.Fatalf("provenance = %+v, want no code graph", provenance)
	}
	if f.openWorktrees() != 0 {
		t.Fatalf("%d worktrees left behind", f.openWorktrees())
	}
}

// A remote index-backed server serves its own index, so there is no graph to
// build per run. The review says so once, at debug level and without naming the
// server; a local one has nothing to report.
func TestRunReviewLogsWhenAnIndexedServerIsRemote(t *testing.T) {
	for _, tc := range []struct {
		name   string
		server func(f *targetGraphReviewFixture) mcp.ServerConfig
		want   int
	}{
		{"remote server", func(f *targetGraphReviewFixture) mcp.ServerConfig {
			return mcp.ServerConfig{URL: f.sharedURL, XReview: mcp.XReviewConfig{IndexCommand: []string{indexTrueCommand}}}
		}, 1},
		{"local server", func(*targetGraphReviewFixture) mcp.ServerConfig {
			return mcp.ServerConfig{Command: indexTrueCommand, XReview: mcp.XReviewConfig{IndexCommand: []string{indexTrueCommand}}}
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newTargetGraphReviewFixture(t)
			f.service.logger = slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			f.useExtensions(tc.server(f), map[string]config.BlockBinding{review.BlockCodeReview: {MCPTools: []string{"mcp__graph__search_graph"}}})
			// What the production builder answers for a remote server: nothing.
			f.answers(func(targetGraphRequest) (*targetGraph, error) { return nil, nil })
			job := f.seedReview(t, "target-graph-remote")

			if err := f.service.RunReview(ctx, job); err != nil {
				t.Fatalf("RunReview: %v", err)
			}

			var lines []string
			for _, line := range strings.Split(f.logs.String(), "\n") {
				if strings.Contains(line, "target code graph skipped") {
					lines = append(lines, line)
				}
			}
			if len(lines) != tc.want {
				t.Fatalf("skip log lines = %q, want %d", lines, tc.want)
			}
			for _, line := range lines {
				if !strings.Contains(line, "level=DEBUG") || !strings.Contains(line, "project_id=42") || strings.Contains(line, "server=") || strings.Contains(line, "error") {
					t.Fatalf("log line = %q, want a debug line with project_id only", line)
				}
			}
		})
	}
}

// The production wiring: a review service built from its configuration owns a
// builder that is ready to index, so the feature cannot be silently off.
func TestNewReviewServiceWiresTheTargetGraphBuilder(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	assertProductionTargetGraphBuilder(t, f.service)
}

func TestNewRuntimeWiresTheTargetGraphBuilder(t *testing.T) {
	st := openTestStore(t)
	cfg := &config.Config{
		Workspace: config.WorkspaceConfig{Root: t.TempDir(), RunCleanupAfter: time.Hour},
		Storage:   config.StorageConfig{LLMCallRetention: time.Hour, DeliveryRetention: time.Hour, JobRetention: time.Hour, ContextVersions: 1},
		Engine:    config.EngineConfig{MaxConcurrentNodes: 1, DefaultNodeTimeout: time.Minute},
		Profiles:  testProfiles(),
	}
	runtime, err := NewRuntime(context.Background(), cfg, RuntimeDeps{
		Store:  st,
		Cipher: newTestCipher(t),
		GitLab: newRuntimeTestGitLab(),
		LLM:    runtimeTestClients{},
		Git:    repoGit{git: &repo.Git{}},
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	assertProductionTargetGraphBuilder(t, runtime.reviews)
}

func assertProductionTargetGraphBuilder(t *testing.T, service *ReviewService) {
	t.Helper()
	builder, ok := service.targetGraphs.(*defaultTargetGraphBuilder)
	if !ok || builder == nil {
		t.Fatalf("targetGraphs = %T, want the production builder", service.targetGraphs)
	}
	if builder.git == nil || builder.git != service.git {
		t.Fatalf("builder git = %v, want the service's git", builder.git)
	}
	if builder.slots == nil || cap(builder.slots) == 0 || builder.now == nil || builder.newManager == nil || builder.runIndex == nil {
		t.Fatalf("builder = %+v, want slots and seams set", builder)
	}
	if builder.slotTimeout != targetGraphSlotTimeout || builder.indexTimeout != targetGraphIndexTimeout {
		t.Fatalf("builder timeouts = %v / %v, want %v / %v", builder.slotTimeout, builder.indexTimeout, targetGraphSlotTimeout, targetGraphIndexTimeout)
	}
}
