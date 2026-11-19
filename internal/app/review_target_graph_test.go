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

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
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
// server, "graph", that block code-review is bound to. The shared manager
// serves it from sharedURL; a target graph serves it from targetURL, so a
// request count shows which of the two the review used.
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
	resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{
		Store:     f.db,
		Workspace: t.TempDir(),
		Blocks: map[string]config.BlockBinding{
			review.BlockCodeReview: {MCPTools: []string{"mcp__graph__search_graph"}},
		},
		MCP:       mcp.Config{MCPServers: map[string]mcp.ServerConfig{"graph": {URL: f.sharedURL}}},
		DeniedEnv: []string{"GITLAB_TOKEN"},
	})
	if err != nil {
		t.Fatalf("NewProjectExtensionResolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })
	f.service.extensions = resolver
	f.builder = &recordingTargetGraphBuilder{events: f.events, answers: []targetGraphAnswer{f.liveGraph}}
	f.service.targetGraphs = f.builder
	return f
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
	if wf.Verdict != review.VerdictIncomplete || len(gaps) != 1 || !strings.Contains(gaps[0], "mcp__graph__search_graph") {
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
