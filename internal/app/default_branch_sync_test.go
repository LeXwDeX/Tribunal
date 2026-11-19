package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

// The registered default branch follows the branch GitLab reports (#604):
// workspace refresh, onboarding and review runs resolve the project through
// GitLab anyway, so a changed default is written back instead of refreshing a
// stale branch forever.

const (
	syncedLogMessage = "project default branch synchronized"
	syncFailMessage  = "project default branch sync failed"
)

// baseFetchRecorder records the refspecs fetched into the workspace mirror.
type baseFetchRecorder struct {
	*fakeGitOps
	mu       sync.Mutex
	refspecs []string
}

func (g *baseFetchRecorder) Fetch(ctx context.Context, dir string, auth repo.Auth, refspecs ...string) error {
	g.mu.Lock()
	g.refspecs = append(g.refspecs, refspecs...)
	g.mu.Unlock()
	return g.fakeGitOps.Fetch(ctx, dir, auth, refspecs...)
}

func (g *baseFetchRecorder) fetched() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.refspecs...)
}

// defaultBranchWriteStore intercepts the default branch write of a ReviewStore.
type defaultBranchWriteStore struct {
	ReviewStore
	mu     sync.Mutex
	calls  int
	err    error  // returned instead of writing
	before func() // runs before the real write, like a concurrent writer
}

func (s *defaultBranchWriteStore) UpdateProjectDefaultBranch(ctx context.Context, id int64, previous, next string) (bool, error) {
	s.mu.Lock()
	s.calls++
	err, before := s.err, s.before
	s.mu.Unlock()
	if err != nil {
		return false, err
	}
	if before != nil {
		before()
	}
	return s.ReviewStore.UpdateProjectDefaultBranch(ctx, id, previous, next)
}

func (s *defaultBranchWriteStore) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// recordingJobQueue is a JobEnqueuer that keeps every accepted job.
type recordingJobQueue struct {
	mu   sync.Mutex
	jobs []store.Job
	err  error
}

func (q *recordingJobQueue) Enqueue(_ context.Context, job store.Job) (string, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return "", false, q.err
	}
	q.jobs = append(q.jobs, job)
	return fmt.Sprintf("job-%d", len(q.jobs)), false, nil
}

func (q *recordingJobQueue) enqueued() []store.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]store.Job(nil), q.jobs...)
}

func jsonLogger(logs *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(logs, nil))
}

func decodeLogRecords(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func logRecordsWith(records []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, record := range records {
		if record["msg"] == msg {
			out = append(out, record)
		}
	}
	return out
}

// requireLogFields fails unless record carries the standard slog keys plus
// exactly the wanted ones: a default branch sync must not log branch names,
// URLs or raw error text.
func requireLogFields(t *testing.T, record map[string]any, level string, want ...string) {
	t.Helper()
	allowed := map[string]bool{"time": true, "level": true, "msg": true}
	for _, key := range want {
		allowed[key] = true
		if _, ok := record[key]; !ok {
			t.Errorf("log record %v lacks %q", record, key)
		}
	}
	for key := range record {
		if !allowed[key] {
			t.Errorf("log record %v carries unexpected field %q", record, key)
		}
	}
	if record["level"] != level {
		t.Errorf("log level = %v, want %s", record["level"], level)
	}
	if id, ok := record["project_id"]; ok && id != float64(42) {
		t.Errorf("log project_id = %v, want 42", id)
	}
}

func requireNoLogLeak(t *testing.T, logs *bytes.Buffer, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if strings.Contains(logs.String(), needle) {
			t.Errorf("log leaked %q: %s", needle, logs.String())
		}
	}
}

func storedDefaultBranch(t *testing.T, f *reviewFixture) string {
	t.Helper()
	project, err := f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	return project.DefaultBranch
}

var noRegenerate = WorkspaceRefreshOptions{Reindex: contextBoolPtr(false), RegenerateContext: contextBoolPtr(false)}

func TestRefreshFollowsGitLabDefaultBranch(t *testing.T) {
	ctx := context.Background()

	t.Run("manual refresh fetches the new default branch", func(t *testing.T) {
		f := newReviewFixture(t, reviewTestHead)
		f.gitlab.project.DefaultBranch = "develop"
		service := unrestrictedIndexService(t, f)
		git := &baseFetchRecorder{fakeGitOps: f.git}
		service.git = git
		var logs bytes.Buffer
		service.logger = jsonLogger(&logs)

		if err := service.RefreshWithOptions(ctx, 42, "", noRegenerate); err != nil {
			t.Fatalf("RefreshWithOptions: %v", err)
		}
		if got := git.fetched(); len(got) != 1 || got[0] != "+refs/heads/develop:refs/workspace/base" {
			t.Fatalf("fetched %q, want the new default branch only", got)
		}
		if got := storedDefaultBranch(t, f); got != "develop" {
			t.Fatalf("stored default branch = %q, want develop", got)
		}
		state, err := f.db.GetWorkspace(ctx, 42)
		if err != nil || state.State != "ready" {
			t.Fatalf("workspace = %+v, %v; want ready", state, err)
		}
		synced := logRecordsWith(decodeLogRecords(t, &logs), syncedLogMessage)
		if len(synced) != 1 {
			t.Fatalf("sync log records = %v, want exactly one", synced)
		}
		requireLogFields(t, synced[0], "INFO", "project_id")
	})

	t.Run("a pushed ref keeps its ref while the default is synchronized", func(t *testing.T) {
		f := newReviewFixture(t, reviewTestHead)
		f.gitlab.project.DefaultBranch = "develop"
		service := unrestrictedIndexService(t, f)
		git := &baseFetchRecorder{fakeGitOps: f.git}
		service.git = git

		if err := service.RefreshWithOptions(ctx, 42, "refs/heads/feature-x", noRegenerate); err != nil {
			t.Fatalf("RefreshWithOptions: %v", err)
		}
		if got := git.fetched(); len(got) != 1 || got[0] != "+refs/heads/feature-x:refs/workspace/base" {
			t.Fatalf("fetched %q, want the pushed ref", got)
		}
		if got := storedDefaultBranch(t, f); got != "develop" {
			t.Fatalf("stored default branch = %q, want develop", got)
		}
	})

	t.Run("an unchanged default is not written again", func(t *testing.T) {
		f := newReviewFixture(t, reviewTestHead)
		f.gitlab.project.DefaultBranch = "main"
		service := unrestrictedIndexService(t, f)
		writes := &defaultBranchWriteStore{ReviewStore: f.db}
		service.store = writes
		git := &baseFetchRecorder{fakeGitOps: f.git}
		service.git = git

		if err := service.RefreshWithOptions(ctx, 42, "", noRegenerate); err != nil {
			t.Fatalf("RefreshWithOptions: %v", err)
		}
		if got := git.fetched(); len(got) != 1 || got[0] != "+refs/heads/main:refs/workspace/base" {
			t.Fatalf("fetched %q, want main", got)
		}
		if writes.writes() != 0 {
			t.Fatalf("default branch writes = %d, want 0", writes.writes())
		}
	})

	t.Run("an empty GitLab default keeps the stored branch", func(t *testing.T) {
		f := newReviewFixture(t, reviewTestHead)
		f.gitlab.project.DefaultBranch = ""
		service := unrestrictedIndexService(t, f)
		git := &baseFetchRecorder{fakeGitOps: f.git}
		service.git = git

		if err := service.RefreshWithOptions(ctx, 42, "", noRegenerate); err != nil {
			t.Fatalf("RefreshWithOptions: %v", err)
		}
		if got := git.fetched(); len(got) != 1 || got[0] != "+refs/heads/main:refs/workspace/base" {
			t.Fatalf("fetched %q, want main", got)
		}
		if got := storedDefaultBranch(t, f); got != "main" {
			t.Fatalf("stored default branch = %q, want main", got)
		}
	})
}

func TestOnboardUsesSynchronizedDefaultBranch(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.project.DefaultBranch = "develop"
	var mu sync.Mutex
	var prompts []string
	service := newOnboardingContextService(t, f, func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		mu.Lock()
		prompts = append(prompts, spec.Prompt)
		mu.Unlock()
		return onboardingIndexRunner(ctx, spec)
	})
	service.reviewDefaults.Ignore = nil
	git := &baseFetchRecorder{fakeGitOps: f.git}
	service.git = git

	if err := service.Onboard(ctx, 42); err != nil {
		t.Fatalf("Onboard: %v", err)
	}
	if got := git.fetched(); len(got) != 1 || got[0] != "+refs/heads/develop:refs/workspace/base" {
		t.Fatalf("fetched %q, want the synchronized default branch only", got)
	}
	if got := storedDefaultBranch(t, f); got != "develop" {
		t.Fatalf("stored default branch = %q, want develop", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(prompts) != 1 || !strings.Contains(prompts[0], "目标分支：develop") {
		t.Fatalf("onboarding prompts = %q, want the synchronized target branch", prompts)
	}
}

func TestRefreshSurvivesDefaultBranchWriteFailure(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.project.DefaultBranch = "develop"
	service := unrestrictedIndexService(t, f)
	const raw = "database is locked token=glpat-synthetic-secret"
	service.store = &defaultBranchWriteStore{ReviewStore: f.db, err: errors.New(raw)}
	git := &baseFetchRecorder{fakeGitOps: f.git}
	service.git = git
	var logs bytes.Buffer
	service.logger = jsonLogger(&logs)

	if err := service.RefreshWithOptions(ctx, 42, "", noRegenerate); err != nil {
		t.Fatalf("a failed default branch write failed the refresh: %v", err)
	}
	if got := git.fetched(); len(got) != 1 || got[0] != "+refs/heads/main:refs/workspace/base" {
		t.Fatalf("fetched %q, want the previous default branch", got)
	}
	if got := storedDefaultBranch(t, f); got != "main" {
		t.Fatalf("stored default branch = %q, want main", got)
	}
	state, err := f.db.GetWorkspace(ctx, 42)
	if err != nil || state.State != "ready" {
		t.Fatalf("workspace = %+v, %v; want ready", state, err)
	}
	warned := logRecordsWith(decodeLogRecords(t, &logs), syncFailMessage)
	if len(warned) != 1 {
		t.Fatalf("failure log records = %v, want exactly one", warned)
	}
	requireLogFields(t, warned[0], "WARN", "project_id", "error_class")
	requireNoLogLeak(t, &logs, "glpat-", "database is locked")
}

func TestSyncDefaultBranchIgnoresUnusableGitLabValue(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	writes := &defaultBranchWriteStore{ReviewStore: f.db}
	for _, tc := range []struct{ name, branch string }{
		{"empty repository", ""},
		{"blank", "   "},
		{"leading space", " develop"},
		{"trailing space", "develop "},
		{"newline", "dev\nelop"},
		{"control character", "dev\x01elop"},
		{"leading dash", "-develop"},
		{"option like", "--upload-pack=x"},
		{"double dot", "a..b"},
		{"trailing slash", "develop/"},
		{"lock suffix", "develop.lock"},
		{"full ref", "refs/heads/develop"},
		{"head", "HEAD"},
		{"colon", "a:b"},
		{"too long", strings.Repeat("a", 2048)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, err := f.db.GetProject(ctx, 42)
			if err != nil {
				t.Fatalf("GetProject: %v", err)
			}
			var logs bytes.Buffer
			changed := syncDefaultBranch(ctx, writes, jsonLogger(&logs), project, &gitlab.Project{ID: 42, DefaultBranch: tc.branch})
			if changed || project.DefaultBranch != "main" {
				t.Fatalf("changed = %v, default = %q; want the stored branch kept", changed, project.DefaultBranch)
			}
			if got := storedDefaultBranch(t, f); got != "main" {
				t.Fatalf("stored default branch = %q, want main", got)
			}
			if logs.Len() != 0 {
				t.Fatalf("unexpected log output: %s", logs.String())
			}
		})
	}
	if writes.writes() != 0 {
		t.Fatalf("default branch writes = %d, want none for unusable values", writes.writes())
	}
}

func TestSyncDefaultBranchAcceptsOrdinaryBranchNames(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	for _, name := range []string{"develop", "release/1.2", "feature/a_b+c", "v2.x", "release-2026.11", "Main"} {
		project, err := f.db.GetProject(ctx, 42)
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if !syncDefaultBranch(ctx, f.db, nil, project, &gitlab.Project{ID: 42, DefaultBranch: name}) {
			t.Fatalf("%q was not accepted", name)
		}
		if project.DefaultBranch != name || storedDefaultBranch(t, f) != name {
			t.Fatalf("%q: in memory %q, stored %q", name, project.DefaultBranch, storedDefaultBranch(t, f))
		}
	}
}

func TestSyncDefaultBranchAdoptsStoredValueWhenWriteLosesRace(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	project, err := f.db.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	racing := &defaultBranchWriteStore{ReviewStore: f.db}
	racing.before = func() {
		if applied, err := f.db.UpdateProjectDefaultBranch(ctx, 42, "main", "hotfix"); err != nil || !applied {
			t.Errorf("competing write = %v, %v", applied, err)
		}
	}
	var logs bytes.Buffer

	changed := syncDefaultBranch(ctx, racing, jsonLogger(&logs), project, &gitlab.Project{ID: 42, DefaultBranch: "develop"})
	if changed {
		t.Fatal("a lost compare-and-set reported a change")
	}
	if project.DefaultBranch != "hotfix" {
		t.Fatalf("in-memory default = %q, want the concurrent writer's hotfix", project.DefaultBranch)
	}
	if got := storedDefaultBranch(t, f); got != "hotfix" {
		t.Fatalf("stored default branch = %q, want hotfix", got)
	}
	if synced := logRecordsWith(decodeLogRecords(t, &logs), syncedLogMessage); len(synced) != 0 {
		t.Fatalf("a lost write logged a synchronization: %v", synced)
	}
}

func TestSyncDefaultBranchKeepsValueOnStoreFailure(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	project, err := f.db.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	failing := &defaultBranchWriteStore{ReviewStore: f.db, err: errors.New("disk I/O error on /srv/data/review.db")}
	var logs bytes.Buffer

	if syncDefaultBranch(ctx, failing, jsonLogger(&logs), project, &gitlab.Project{ID: 42, DefaultBranch: "develop"}) {
		t.Fatal("a failed write reported a change")
	}
	if project.DefaultBranch != "main" || storedDefaultBranch(t, f) != "main" {
		t.Fatalf("in memory %q, stored %q; want main kept", project.DefaultBranch, storedDefaultBranch(t, f))
	}
	warned := logRecordsWith(decodeLogRecords(t, &logs), syncFailMessage)
	if len(warned) != 1 {
		t.Fatalf("failure log records = %v, want exactly one", warned)
	}
	requireLogFields(t, warned[0], "WARN", "project_id", "error_class")
	requireNoLogLeak(t, &logs, "/srv/data", "disk I/O")

	// A nil logger must not turn the failure into a panic.
	if syncDefaultBranch(ctx, failing, nil, project, &gitlab.Project{ID: 42, DefaultBranch: "develop"}) {
		t.Fatal("a failed write reported a change")
	}
}

func TestSyncDefaultBranchToleratesMissingInputs(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	project := &store.Project{ID: 42, DefaultBranch: "main"}
	current := &gitlab.Project{ID: 42, DefaultBranch: "develop"}
	if syncDefaultBranch(ctx, nil, nil, project, current) || project.DefaultBranch != "main" {
		t.Fatal("a missing store changed the project")
	}
	if syncDefaultBranch(ctx, f.db, nil, nil, current) {
		t.Fatal("a missing project reported a change")
	}
	if syncDefaultBranch(ctx, f.db, nil, project, nil) || project.DefaultBranch != "main" {
		t.Fatal("a missing GitLab project changed the project")
	}
	if got := storedDefaultBranch(t, f); got != "main" {
		t.Fatalf("stored default branch = %q, want main", got)
	}
}

func TestCurrentProjectInfoReturnsVerifiedRemoteAndProject(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.project.DefaultBranch = "develop"
	project, err := f.db.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}

	remote, current, err := currentProjectInfo(ctx, f.gitlab, project, "token")
	if err != nil {
		t.Fatalf("currentProjectInfo: %v", err)
	}
	wantRemote, err := currentProjectRemote(ctx, f.gitlab, project, "token")
	if err != nil || remote != wantRemote || remote != "https://gitlab.example.com/group/repo.git" {
		t.Fatalf("remote = %q, currentProjectRemote = %q, %v", remote, wantRemote, err)
	}
	if current == nil || current.ID != 42 || current.DefaultBranch != "develop" {
		t.Fatalf("current = %+v, want the project GitLab reported", current)
	}

	// The identity checks are unchanged: a different project ID is rejected
	// and no project record escapes.
	f.gitlab.project.ID = 99
	remote, current, err = currentProjectInfo(ctx, f.gitlab, project, "token")
	if !errors.Is(err, errProjectIdentityChanged) || remote != "" || current != nil {
		t.Fatalf("changed identity = %q, %+v, %v; want errProjectIdentityChanged only", remote, current, err)
	}
	if _, err := currentProjectRemote(ctx, f.gitlab, project, "token"); !errors.Is(err, errProjectIdentityChanged) {
		t.Fatalf("currentProjectRemote error = %v, want errProjectIdentityChanged", err)
	}
}

func TestPrepareRunSynchronizesDefaultBranchAndSchedulesRefresh(t *testing.T) {
	ctx := context.Background()
	newRun := func(t *testing.T, gitlabDefault string) (*reviewFixture, *recordingJobQueue, *store.Project) {
		t.Helper()
		f := newReviewFixture(t, reviewTestHead)
		f.gitlab.project.DefaultBranch = gitlabDefault
		queue := &recordingJobQueue{}
		f.service.queue = queue
		project, err := f.db.GetProject(ctx, 42)
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		consumerReadLock(t, f.manager, 42)
		return f, queue, project
	}

	t.Run("drift updates the store and enqueues one refresh", func(t *testing.T) {
		f, queue, project := newRun(t, "develop")
		var logs bytes.Buffer
		f.service.logger = jsonLogger(&logs)

		_, cleanup, err := f.service.prepareRun(ctx, project, "token", 7, "run-drift", f.head)
		if err != nil {
			t.Fatalf("prepareRun: %v", err)
		}
		defer cleanup()
		if project.DefaultBranch != "develop" {
			t.Fatalf("in-memory default = %q, want develop", project.DefaultBranch)
		}
		if got := storedDefaultBranch(t, f); got != "develop" {
			t.Fatalf("stored default branch = %q, want develop", got)
		}
		jobs := queue.enqueued()
		if len(jobs) != 1 {
			t.Fatalf("enqueued %d jobs, want exactly one", len(jobs))
		}
		if jobs[0].Kind != "workspace.refresh" || jobs[0].Key != "ws:42" {
			t.Fatalf("job = %s/%s, want workspace.refresh keyed ws:42", jobs[0].Kind, jobs[0].Key)
		}
		var payload workspaceJobPayload
		if err := json.Unmarshal(jobs[0].Payload, &payload); err != nil {
			t.Fatalf("decode payload %s: %v", jobs[0].Payload, err)
		}
		if payload.ProjectID != 42 || payload.Ref != "" || payload.Branch != "" || payload.AfterSHA != "" || payload.Trigger != "default_branch_changed" {
			t.Fatalf("payload = %+v, want an unscoped default_branch_changed refresh of project 42", payload)
		}
		if payload.RegenerateContext != nil || payload.Reindex != nil {
			t.Fatalf("payload overrides the refresh defaults: %+v", payload)
		}
		synced := logRecordsWith(decodeLogRecords(t, &logs), syncedLogMessage)
		if len(synced) != 1 {
			t.Fatalf("sync log records = %v, want exactly one", synced)
		}
		requireLogFields(t, synced[0], "INFO", "project_id")
	})

	t.Run("a repeated run after the sync enqueues nothing more", func(t *testing.T) {
		f, queue, project := newRun(t, "develop")
		_, cleanup, err := f.service.prepareRun(ctx, project, "token", 7, "run-first", f.head)
		if err != nil {
			t.Fatalf("first prepareRun: %v", err)
		}
		defer cleanup()
		again, err := f.db.GetProject(ctx, 42)
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		_, cleanupAgain, err := f.service.prepareRun(ctx, again, "token", 7, "run-second", f.head)
		if err != nil {
			t.Fatalf("second prepareRun: %v", err)
		}
		defer cleanupAgain()
		if got := len(queue.enqueued()); got != 1 {
			t.Fatalf("enqueued %d jobs across two runs, want one", got)
		}
	})

	t.Run("no drift enqueues nothing", func(t *testing.T) {
		f, queue, project := newRun(t, "main")
		_, cleanup, err := f.service.prepareRun(ctx, project, "token", 7, "run-same", f.head)
		if err != nil {
			t.Fatalf("prepareRun: %v", err)
		}
		defer cleanup()
		if got := len(queue.enqueued()); got != 0 {
			t.Fatalf("enqueued %d jobs without drift", got)
		}
		if got := storedDefaultBranch(t, f); got != "main" {
			t.Fatalf("stored default branch = %q, want main", got)
		}
	})

	t.Run("an unusable GitLab default enqueues nothing", func(t *testing.T) {
		for _, branch := range []string{"", "-bad", "a b"} {
			f, queue, project := newRun(t, branch)
			_, cleanup, err := f.service.prepareRun(ctx, project, "token", 7, "run-unusable", f.head)
			if err != nil {
				t.Fatalf("prepareRun with %q: %v", branch, err)
			}
			cleanup()
			if got := len(queue.enqueued()); got != 0 {
				t.Fatalf("enqueued %d jobs for default %q", got, branch)
			}
			if got := storedDefaultBranch(t, f); got != "main" {
				t.Fatalf("stored default branch = %q for %q, want main", got, branch)
			}
		}
	})

	t.Run("without a queue the default still follows GitLab", func(t *testing.T) {
		f, _, project := newRun(t, "develop")
		f.service.queue = nil
		_, cleanup, err := f.service.prepareRun(ctx, project, "token", 7, "run-no-queue", f.head)
		if err != nil {
			t.Fatalf("prepareRun: %v", err)
		}
		defer cleanup()
		if got := storedDefaultBranch(t, f); got != "develop" {
			t.Fatalf("stored default branch = %q, want develop", got)
		}
	})

	t.Run("an enqueue failure does not fail the run", func(t *testing.T) {
		f, queue, project := newRun(t, "develop")
		queue.err = errors.New("queue unavailable at /srv/data token=glpat-synthetic-secret")
		var logs bytes.Buffer
		f.service.logger = jsonLogger(&logs)

		_, cleanup, err := f.service.prepareRun(ctx, project, "token", 7, "run-enqueue-fails", f.head)
		if err != nil {
			t.Fatalf("an enqueue failure failed the run: %v", err)
		}
		defer cleanup()
		if got := storedDefaultBranch(t, f); got != "develop" {
			t.Fatalf("stored default branch = %q, want develop", got)
		}
		warned := logRecordsWith(decodeLogRecords(t, &logs), "default branch refresh enqueue failed")
		if len(warned) != 1 {
			t.Fatalf("enqueue failure log records = %v, want exactly one", warned)
		}
		requireLogFields(t, warned[0], "WARN", "project_id", "error_class")
		requireNoLogLeak(t, &logs, "glpat-", "/srv/data", "queue unavailable")
	})

	t.Run("a store failure does not fail the run or enqueue", func(t *testing.T) {
		f, queue, project := newRun(t, "develop")
		f.service.store = &defaultBranchWriteStore{ReviewStore: f.db, err: errors.New("database is locked")}
		_, cleanup, err := f.service.prepareRun(ctx, project, "token", 7, "run-store-fails", f.head)
		if err != nil {
			t.Fatalf("a store failure failed the run: %v", err)
		}
		defer cleanup()
		if got := len(queue.enqueued()); got != 0 {
			t.Fatalf("enqueued %d jobs although the default was not stored", got)
		}
		if project.DefaultBranch != "main" {
			t.Fatalf("in-memory default = %q, want main", project.DefaultBranch)
		}
	})
}

// TestDefaultBranchRefreshJobRunsThroughWorkspaceHandler follows the queued
// refresh end to end: a review run detects the drift, the job lands in the real
// queue, and the workspace handler accepts its payload and refreshes the new
// default branch.
func TestDefaultBranchRefreshJobRunsThroughWorkspaceHandler(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.queue = f.db
	f.gitlab.project.DefaultBranch = "develop"
	project, err := f.db.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}

	// The review path holds the read lock only while it prepares the run; the
	// refresh job needs the write lock afterwards.
	unlock, err := f.manager.Get(42).RLock(ctx)
	if err != nil {
		t.Fatalf("RLock: %v", err)
	}
	_, cleanup, err := f.service.prepareRun(ctx, project, "token", 7, "run-e2e", f.head)
	if cleanup != nil {
		cleanup()
	}
	unlock()
	if err != nil {
		t.Fatalf("prepareRun: %v", err)
	}

	job, err := f.db.Claim(ctx, "test-worker", []string{"workspace.refresh"}, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("Claim = %+v, %v; want the queued refresh", job, err)
	}
	service := unrestrictedIndexService(t, f)
	git := &baseFetchRecorder{fakeGitOps: f.git}
	service.git = git
	if err := service.Handle(ctx, job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := git.fetched(); len(got) != 1 || got[0] != "+refs/heads/develop:refs/workspace/base" {
		t.Fatalf("refresh fetched %q, want the new default branch", got)
	}
	state, err := f.db.GetWorkspace(ctx, 42)
	if err != nil || state.State != "ready" {
		t.Fatalf("workspace = %+v, %v; want ready", state, err)
	}
}

// TestPrepareRunQueuesRefreshWithoutTakingWorkspaceLock guards the lock order:
// the review run holds the workspace read lock, and the lock is writer
// preferring, so a second read lock taken for the enqueue would wait behind a
// queued refresh that in turn waits for this run. The refresh must be queued
// without touching the workspace lock.
func TestPrepareRunQueuesRefreshWithoutTakingWorkspaceLock(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	queue := &recordingJobQueue{}
	f.service.queue = queue
	f.gitlab.project.DefaultBranch = "develop"
	project, err := f.db.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}

	ws := f.manager.Get(42)
	unlockRead, err := ws.RLock(ctx)
	if err != nil {
		t.Fatalf("RLock: %v", err)
	}
	writerCtx, stopWriter := context.WithCancel(ctx)
	t.Cleanup(stopWriter)
	writerHolds := make(chan func(), 1)
	go func() {
		release, err := ws.Lock(writerCtx)
		if err != nil {
			close(writerHolds)
			return
		}
		writerHolds <- release
	}()
	// A new reader is refused once a writer is queued, which is how the test
	// knows the refresh writer is waiting behind the review's read lock.
	deadline := time.Now().Add(10 * time.Second)
	for {
		probe, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		release, err := ws.RLock(probe)
		cancel()
		if err != nil {
			break
		}
		release()
		if time.Now().After(deadline) {
			t.Fatal("the workspace writer never queued behind the read lock")
		}
	}

	runCtx, cancelRun := context.WithTimeout(ctx, 10*time.Second)
	defer cancelRun()
	prepared := make(chan error, 1)
	go func() {
		_, cleanup, err := f.service.prepareRun(runCtx, project, "token", 7, "run-writer-waiting", f.head)
		if cleanup != nil {
			cleanup()
		}
		prepared <- err
	}()
	select {
	case err := <-prepared:
		if err != nil {
			t.Fatalf("prepareRun: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prepareRun blocked behind the queued workspace writer")
	}
	if got := len(queue.enqueued()); got != 1 {
		t.Fatalf("enqueued %d jobs, want one", got)
	}

	unlockRead()
	release, ok := <-writerHolds
	if !ok {
		t.Fatal("the workspace writer never acquired the lock")
	}
	release()
}

// TestRuntimeGivesReviewServiceTheWorkspaceQueue keeps the refresh alive in
// production: a review service built without a queue still follows the default
// branch but never re-bases the workspace on it.
func TestRuntimeGivesReviewServiceTheWorkspaceQueue(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cfg := &config.Config{
		Workspace: config.WorkspaceConfig{Root: t.TempDir(), RunCleanupAfter: time.Hour},
		Storage: config.StorageConfig{
			LLMCallRetention:  time.Hour,
			DeliveryRetention: time.Hour,
			JobRetention:      time.Hour,
			ContextVersions:   1,
		},
		Engine:   config.EngineConfig{MaxConcurrentNodes: 1, DefaultNodeTimeout: time.Minute},
		Profiles: testProfiles(),
	}
	runtime, err := NewRuntime(ctx, cfg, RuntimeDeps{
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

	if runtime.reviews == nil || runtime.workspaces == nil {
		t.Fatal("runtime did not build the review and workspace services")
	}
	if runtime.reviews.queue == nil || runtime.reviews.queue != runtime.workspaces.queue {
		t.Fatalf("review queue = %v, workspace queue = %v; want the same queue", runtime.reviews.queue, runtime.workspaces.queue)
	}
}

// TestDefaultBranchRefreshTriggersFollowCurrentDefault covers refresh jobs that
// captured the default branch when they were queued (manual and catch-up). After
// GitLab renames the default branch they must fetch the new default, not the
// stale ref in the payload. A push refresh keeps its explicit ref, except a push
// to the branch that was the default: it passed the stored-default filter, and
// that branch is no longer the one to follow once the sync moved the default
// (#604).
func TestDefaultBranchRefreshTriggersFollowCurrentDefault(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger string
		ref     string
		gitlab  string // default branch GitLab reports; "" = develop
		want    string
	}{
		{trigger: "manual", ref: "refs/heads/main", want: "+refs/heads/develop:refs/workspace/base"},
		{trigger: catchUpTrigger, ref: "refs/heads/main", want: "+refs/heads/develop:refs/workspace/base"},
		{trigger: "default_branch_changed", ref: "", want: "+refs/heads/develop:refs/workspace/base"},
		{trigger: "push", ref: "refs/heads/release", want: "+refs/heads/release:refs/workspace/base"},
		{name: "push to the previous default", trigger: "push", ref: "refs/heads/main", want: "+refs/heads/develop:refs/workspace/base"},
		{name: "push to the unchanged default", trigger: "push", ref: "refs/heads/main", gitlab: "main", want: "+refs/heads/main:refs/workspace/base"},
		{name: "push to the new default", trigger: "push", ref: "refs/heads/develop", want: "+refs/heads/develop:refs/workspace/base"},
		{name: "push to a branch named like the previous default", trigger: "push", ref: "refs/heads/main-2", want: "+refs/heads/main-2:refs/workspace/base"},
	} {
		name := tc.name
		if name == "" {
			name = tc.trigger
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			f.gitlab.project.DefaultBranch = "develop"
			if tc.gitlab != "" {
				f.gitlab.project.DefaultBranch = tc.gitlab
			}
			payload, err := json.Marshal(map[string]any{
				"project_id": 42, "ref": tc.ref, "branch": strings.TrimPrefix(tc.ref, "refs/heads/"), "trigger": tc.trigger,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := f.db.Enqueue(ctx, store.Job{Kind: "workspace.refresh", Key: "ws:42", Payload: payload, MaxAttempts: 3}); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			job, err := f.db.Claim(ctx, "test-worker", []string{"workspace.refresh"}, time.Minute)
			if err != nil || job == nil {
				t.Fatalf("Claim = %+v, %v", job, err)
			}
			service := unrestrictedIndexService(t, f)
			git := &baseFetchRecorder{fakeGitOps: f.git}
			service.git = git
			if err := service.Handle(ctx, job); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got := git.fetched(); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("refresh fetched %q, want %q", got, tc.want)
			}
		})
	}
}

// The webhook filters events with the stored default branch and never asks
// GitLab, so after a rename every event on the new default branch is filtered
// out and nothing would ever read the project from GitLab. The payload's
// project.default_branch is only a hint: it queues a refresh, which verifies the
// change with GitLab before the store follows it (#604).

// withDefaultBranch sets project.default_branch in a webhook body.
func withDefaultBranch(t *testing.T, body []byte, defaultBranch string) []byte {
	t.Helper()
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("decode webhook body: %v", err)
	}
	project, ok := generic["project"].(map[string]any)
	if !ok {
		t.Fatalf("webhook body has no project object: %s", body)
	}
	project["default_branch"] = defaultBranch
	out, err := json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// mrTargeting is a merge request open event whose target branch is branch.
func mrTargeting(branch string) func(map[string]any) {
	return func(body map[string]any) {
		patchAttr(body, "target_branch", branch)
		body["merge_request"].(map[string]any)["target_branch"] = branch
	}
}

// defaultBranchRefreshes returns the hint deliveries and jobs the service tried
// to record (a duplicate delivery is still listed) and the number of hint
// deliveries the store accepted.
func (f *webhookFixture) defaultBranchRefreshes() (records []store.Delivery, jobs []*store.Job) {
	for _, d := range f.records() {
		if d.Event == defaultBranchRefreshEvent {
			records = append(records, d)
		}
	}
	for _, j := range f.jobs() {
		if j.Kind == "workspace.refresh" && strings.Contains(string(j.Payload), `"trigger":"`+defaultBranchChangedTrigger+`"`) {
			jobs = append(jobs, j)
		}
	}
	return records, jobs
}

// storedDeliveries counts the stored push-group deliveries (no merge request)
// by outcome: the hint deliveries are enqueued, the filtered events ignored.
func (f *webhookFixture) storedDeliveries(t *testing.T) (enqueued, ignored int64) {
	t.Helper()
	zero := int64(0)
	rows, _, err := f.store.ListWebhookDeliverySummaries(context.Background(), store.WebhookDeliveryFilter{MRIID: &zero}, "", 50)
	if err != nil {
		t.Fatalf("ListWebhookDeliverySummaries: %v", err)
	}
	for _, row := range rows {
		enqueued += row.Enqueued
		ignored += row.Ignored
	}
	return enqueued, ignored
}

func (f *webhookFixture) storedDefault(t *testing.T) string {
	t.Helper()
	project, err := f.store.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	return project.DefaultBranch
}

func TestWebhookDefaultBranchHintQueuesRefresh(t *testing.T) {
	// The stored default is main; the payloads report develop.
	for _, tc := range []struct {
		name     string
		event    string
		body     func(t *testing.T) []byte
		accepted bool   // the event's own decision
		ignored  string // the event's own decision
	}{
		{
			name: "push to the new default", event: "Push Hook", ignored: "branch_filtered",
			body: func(t *testing.T) []byte {
				return withDefaultBranch(t, pushBody(42, "refs/heads/develop", sha40), "develop")
			},
		},
		{
			name: "push to another branch", event: "Push Hook", ignored: "branch_filtered",
			body: func(t *testing.T) []byte {
				return withDefaultBranch(t, pushBody(42, "refs/heads/feature", sha40), "develop")
			},
		},
		{
			name: "merge request targeting the new default", event: "Merge Request Hook", ignored: "branch_filtered",
			body: func(t *testing.T) []byte {
				return withDefaultBranch(t, mrBody(42, 7, mrTargeting("develop")), "develop")
			},
		},
		{
			name: "merge request closed", event: "Merge Request Hook", accepted: true,
			body: func(t *testing.T) []byte {
				return withDefaultBranch(t, mrBody(42, 7, func(b map[string]any) { patchAttr(b, "action", "close") }), "develop")
			},
		},
		{
			name: "review command note", event: "Note Hook", accepted: true,
			body: func(t *testing.T) []byte {
				return withDefaultBranch(t, noteBody(42, 7, 555, 12, "alice", "/review"), "develop")
			},
		},
		{
			name: "event ignored for another reason", event: "Note Hook", ignored: "not_review_command",
			body: func(t *testing.T) []byte {
				return withDefaultBranch(t, noteBody(42, 7, 556, 12, "alice", "thanks"), "develop")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWebhookFixture(t, "{}")
			result, err := f.request(tc.event, "whsec-test", tc.body(t))
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			// The event keeps the decision the stored default gives it.
			if result.Accepted != tc.accepted || result.Ignored != tc.ignored {
				t.Fatalf("result = %+v, want accepted %v ignored %q", result, tc.accepted, tc.ignored)
			}
			records, jobs := f.defaultBranchRefreshes()
			if len(records) != 1 || len(jobs) != 1 {
				t.Fatalf("hint records %d, jobs %d; want one of each", len(records), len(jobs))
			}
			if records[0].Outcome != store.DeliveryEnqueued || records[0].ProjectID != 42 || records[0].MRIID != 0 {
				t.Fatalf("hint delivery = %+v", records[0])
			}
			job := jobs[0]
			if job.Kind != "workspace.refresh" || job.Key != "ws:42" {
				t.Fatalf("hint job = %+v, want workspace.refresh keyed ws:42", job)
			}
			var payload map[string]any
			if err := json.Unmarshal(job.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"project_id": float64(42), "ref": "", "branch": "", "after_sha": "", "trigger": "default_branch_changed"}
			if len(payload) != len(want) {
				t.Fatalf("payload = %v, want %v", payload, want)
			}
			for key, value := range want {
				if payload[key] != value {
					t.Fatalf("payload = %v, want %v", payload, want)
				}
			}
			// A hint is never written: only a refresh that reads GitLab may.
			if got := f.storedDefault(t); got != "main" {
				t.Fatalf("stored default branch = %q, want main", got)
			}
		})
	}
}

// The hint is recorded before the event's own job. A queued push refresh keeps
// the key of the hint's job, so it replaces it and still synchronizes the
// default when it runs; recording them the other way round would drop the
// pushed branch instead.
func TestWebhookDefaultBranchHintDoesNotReplaceTheEventsOwnRefresh(t *testing.T) {
	f := newWebhookFixture(t, "{}")
	result, err := f.request("Push Hook", "whsec-test", withDefaultBranch(t, pushBody(42, "refs/heads/main", sha40), "develop"))
	if err != nil || !result.Accepted {
		t.Fatalf("result = %+v, err = %v; want the push to the stored default accepted", result, err)
	}
	records := f.records()
	if len(records) != 2 || records[0].Event != defaultBranchRefreshEvent || records[1].Event != "Push Hook" {
		t.Fatalf("records = %+v, want the hint first and the push second", records)
	}
	job := f.job(result.JobID)
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["trigger"] != "push" || payload["ref"] != "refs/heads/main" {
		t.Fatalf("queued job payload = %v, want the push refresh", payload)
	}
}

func TestWebhookDefaultBranchHintIgnoresUnusableHints(t *testing.T) {
	for _, tc := range []struct {
		name string
		hint string
	}{
		{name: "absent", hint: ""},
		{name: "same as stored", hint: "main"},
		{name: "option-like", hint: "-x"},
		{name: "option-like with value", hint: "--upload-pack=x"},
		{name: "double dot", hint: "a..b"},
		{name: "full ref", hint: "refs/heads/develop"},
		{name: "leading space", hint: " develop"},
		{name: "trailing space", hint: "develop "},
		{name: "inner space", hint: "dev elop"},
		{name: "control character", hint: "dev\x00elop"},
		{name: "reflog syntax", hint: "dev@{1}"},
		{name: "trailing slash", hint: "develop/"},
		{name: "lock suffix", hint: "develop.lock"},
		{name: "too long", hint: strings.Repeat("a", 5000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWebhookFixture(t, "{}")
			body := pushBody(42, "refs/heads/main", sha40)
			if tc.hint != "" {
				body = withDefaultBranch(t, body, tc.hint)
			}
			result, err := f.request("Push Hook", "whsec-test", body)
			if err != nil || !result.Accepted {
				t.Fatalf("result = %+v, err = %v; want the push accepted as before", result, err)
			}
			if records, jobs := f.defaultBranchRefreshes(); len(records) != 0 || len(jobs) != 0 {
				t.Fatalf("hint records %d, jobs %d; want none", len(records), len(jobs))
			}
			if got := len(f.records()); got != 1 {
				t.Fatalf("recorded %d deliveries, want only the push", got)
			}
			if got := f.storedDefault(t); got != "main" {
				t.Fatalf("stored default branch = %q, want main", got)
			}
		})
	}

	t.Run("a hint without the project id is not attributed to the project", func(t *testing.T) {
		f := newWebhookFixture(t, "{}")
		raw, err := json.Marshal(map[string]any{
			"object_kind": "push", "project_id": 42, "project": map[string]any{"default_branch": "develop"},
			"ref": "refs/heads/main", "before": sha40, "after": sha40,
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := f.request("Push Hook", "whsec-test", raw)
		if err != nil || !result.Accepted {
			t.Fatalf("result = %+v, err = %v; want the push accepted", result, err)
		}
		if records, _ := f.defaultBranchRefreshes(); len(records) != 0 {
			t.Fatalf("hint records %d, want none", len(records))
		}
	})

	t.Run("an unauthenticated delivery queues nothing", func(t *testing.T) {
		f := newWebhookFixture(t, "{}")
		result, err := f.request("Push Hook", "wrong-secret", withDefaultBranch(t, pushBody(42, "refs/heads/develop", sha40), "develop"))
		if err != nil || !result.Unauthorized {
			t.Fatalf("result = %+v, err = %v; want unauthorized", result, err)
		}
		if records, _ := f.defaultBranchRefreshes(); len(records) != 0 {
			t.Fatalf("hint records %d, want none", len(records))
		}
	})

	t.Run("an event of the wrong kind queues nothing", func(t *testing.T) {
		f := newWebhookFixture(t, "{}")
		result, err := f.request("Merge Request Hook", "whsec-test", withDefaultBranch(t, pushBody(42, "refs/heads/develop", sha40), "develop"))
		if err != nil || result.Ignored != "event_mismatch" {
			t.Fatalf("result = %+v, err = %v; want event_mismatch", result, err)
		}
		if records, _ := f.defaultBranchRefreshes(); len(records) != 0 {
			t.Fatalf("hint records %d, want none", len(records))
		}
	})
}

// Every event of a project carries the changed default until a refresh has
// synchronized it, so the hint is recorded once per project, reported branch and
// minute. A refresh that could not synchronize is queued again by a later
// minute, and another reported branch is a different change.
func TestWebhookDefaultBranchHintIsBoundedPerMinute(t *testing.T) {
	f := newWebhookFixture(t, "{}")
	now := time.Date(2026, 11, 19, 10, 0, 5, 0, time.UTC)
	f.service.now = func() time.Time { return now }
	push := func(hint string) {
		t.Helper()
		result, err := f.request("Push Hook", "whsec-test", withDefaultBranch(t, pushBody(42, "refs/heads/"+hint, sha40), hint))
		if err != nil || result.Ignored != "branch_filtered" {
			t.Fatalf("result = %+v, err = %v; want branch_filtered", result, err)
		}
	}
	counts := func() (enqueued, ignored int64) { return f.storedDeliveries(t) }

	for range 3 {
		push("develop")
	}
	if enqueued, ignored := counts(); enqueued != 1 || ignored != 3 {
		t.Fatalf("after three events in one minute: %d hint deliveries, %d ignored events; want 1 and 3", enqueued, ignored)
	}

	now = now.Add(30 * time.Second) // still the same minute
	push("develop")
	if enqueued, _ := counts(); enqueued != 1 {
		t.Fatalf("a later event in the same minute stored %d hint deliveries, want 1", enqueued)
	}

	push("trunk")
	if enqueued, _ := counts(); enqueued != 2 {
		t.Fatalf("another reported branch stored %d hint deliveries, want 2", enqueued)
	}

	now = now.Add(time.Minute) // the next minute retries
	push("develop")
	if enqueued, ignored := counts(); enqueued != 3 || ignored != 6 {
		t.Fatalf("after the next minute: %d hint deliveries, %d ignored events; want 3 and 6", enqueued, ignored)
	}
}

// hintFailureStore fails only the delivery that carries the hint.
type hintFailureStore struct {
	WebhookStore
}

func (s hintFailureStore) RecordDelivery(ctx context.Context, d store.Delivery, j *store.Job) (string, bool, error) {
	if d.Event == defaultBranchRefreshEvent {
		return "", false, errors.New("disk full while recording develop for group/repo")
	}
	return s.WebhookStore.RecordDelivery(ctx, d, j)
}

// A failed hint never fails the event, and its log names neither the branch nor
// the raw error.
func TestWebhookDefaultBranchHintFailureDoesNotFailTheEvent(t *testing.T) {
	f := newWebhookFixture(t, "{}")
	var logs bytes.Buffer
	f.service.logger = jsonLogger(&logs)
	f.service.store = hintFailureStore{WebhookStore: f.service.store}

	result, err := f.request("Push Hook", "whsec-test", withDefaultBranch(t, pushBody(42, "refs/heads/develop", sha40), "develop"))
	if err != nil || result.Ignored != "branch_filtered" {
		t.Fatalf("result = %+v, err = %v; want the event's own outcome", result, err)
	}
	failed := logRecordsWith(decodeLogRecords(t, &logs), "default branch refresh enqueue failed")
	if len(failed) != 1 {
		t.Fatalf("failure log records = %v, want exactly one", failed)
	}
	requireLogFields(t, failed[0], "WARN", "project_id", "error_class")
	requireNoLogLeak(t, &logs, "develop", "disk full", "group/repo")

	// The event's own delivery was recorded as usual.
	if _, ignored := f.storedDeliveries(t); ignored != 1 {
		t.Fatalf("ignored deliveries = %d, want the event's own record", ignored)
	}
}

// TestWebhookDefaultBranchHintTakesNoWorkspaceLock guards the lock order. The
// ingress holds the project lifecycle read lock; the workspace content lock is
// writer preferring and held for a whole refresh or review, and the ingress has
// a 900ms budget. Queuing the hint through WorkspaceService.EnqueueRefresh would
// wait for it.
func TestWebhookDefaultBranchHintTakesNoWorkspaceLock(t *testing.T) {
	ctx := context.Background()
	f := newWebhookFixture(t, "{}")
	manager := workspace.NewManager(t.TempDir(), nil)
	f.service.workspace = manager
	release, err := manager.Get(42).Lock(ctx) // a running refresh holds the write lock
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer release()

	body := withDefaultBranch(t, pushBody(42, "refs/heads/develop", sha40), "develop")
	handled := make(chan error, 1)
	go func() {
		result, err := f.request("Push Hook", "whsec-test", body)
		if err == nil && result.Ignored != "branch_filtered" {
			err = fmt.Errorf("result = %+v, want branch_filtered", result)
		}
		handled <- err
	}()
	select {
	case err := <-handled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the webhook blocked behind the workspace content lock")
	}
	if records, _ := f.defaultBranchRefreshes(); len(records) != 1 {
		t.Fatalf("hint records %d, want 1", len(records))
	}
}

// TestWebhookDefaultBranchHintSelfHealsThroughRefreshJob follows the hint end to
// end on the real store and queue: the event is filtered by the stale default,
// the queued job is the one the workspace handler runs, GitLab confirms the new
// default, and the next event on that branch is accepted.
func TestWebhookDefaultBranchHintSelfHealsThroughRefreshJob(t *testing.T) {
	ctx := context.Background()
	newHook := func(t *testing.T, f *reviewFixture) *WebhookService {
		t.Helper()
		hook, err := NewWebhookService(WebhookServiceConfig{
			Store:          f.db,
			Workspace:      f.manager,
			Cipher:         newTestCipher(t),
			Profiles:       testProfiles(),
			ReviewDefaults: testDefaults(),
		})
		if err != nil {
			t.Fatalf("NewWebhookService: %v", err)
		}
		return hook
	}
	push := func(t *testing.T, hook *WebhookService, ref, hint string) WebhookResult {
		t.Helper()
		result, err := hook.Handle(ctx, WebhookRequest{
			Event: "Push Hook", Token: "whsec", UUID: uuid.NewString(),
			Body: withDefaultBranch(t, pushBody(42, ref, sha40), hint),
		})
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		return result
	}
	runRefresh := func(t *testing.T, f *reviewFixture) *baseFetchRecorder {
		t.Helper()
		job, err := f.db.Claim(ctx, "test-worker", []string{"workspace.refresh"}, time.Minute)
		if err != nil || job == nil {
			t.Fatalf("Claim = %+v, %v; want the queued refresh", job, err)
		}
		service := unrestrictedIndexService(t, f)
		git := &baseFetchRecorder{fakeGitOps: f.git}
		service.git = git
		if err := service.Handle(ctx, job); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		return git
	}

	t.Run("push to the new default", func(t *testing.T) {
		f := newReviewFixture(t, reviewTestHead)
		f.gitlab.project.DefaultBranch = "develop"
		hook := newHook(t, f)

		if result := push(t, hook, "refs/heads/develop", "develop"); result.Ignored != "branch_filtered" {
			t.Fatalf("first push = %+v, want branch_filtered by the stale default", result)
		}
		if got := storedDefaultBranch(t, f); got != "main" {
			t.Fatalf("stored default branch = %q before the refresh, want main", got)
		}
		git := runRefresh(t, f)
		if got := git.fetched(); len(got) != 1 || got[0] != "+refs/heads/develop:refs/workspace/base" {
			t.Fatalf("refresh fetched %q, want the new default branch only", got)
		}
		if got := storedDefaultBranch(t, f); got != "develop" {
			t.Fatalf("stored default branch = %q after the refresh, want develop", got)
		}
		result := push(t, hook, "refs/heads/develop", "develop")
		if !result.Accepted {
			t.Fatalf("second push = %+v, want it accepted once the default followed", result)
		}
		job, err := f.db.GetJob(ctx, result.JobID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if !strings.Contains(string(job.Payload), `"trigger":"push"`) {
			t.Fatalf("second push job = %s, want a push refresh", job.Payload)
		}
	})

	t.Run("push to the previous default", func(t *testing.T) {
		f := newReviewFixture(t, reviewTestHead)
		f.gitlab.project.DefaultBranch = "develop"
		hook := newHook(t, f)

		// The stale default still admits a push to main, so this event queues a
		// push refresh on top of the hint and replaces it.
		if result := push(t, hook, "refs/heads/main", "develop"); !result.Accepted {
			t.Fatalf("push = %+v, want it accepted by the stale default", result)
		}
		git := runRefresh(t, f)
		if got := git.fetched(); len(got) != 1 || got[0] != "+refs/heads/develop:refs/workspace/base" {
			t.Fatalf("refresh fetched %q, want the new default branch, not the pushed old one", got)
		}
		if got := storedDefaultBranch(t, f); got != "develop" {
			t.Fatalf("stored default branch = %q, want develop", got)
		}
	})

	t.Run("GitLab keeps the old default", func(t *testing.T) {
		f := newReviewFixture(t, reviewTestHead)
		f.gitlab.project.DefaultBranch = "main"
		hook := newHook(t, f)

		// A stale or forged hint changes nothing: the refresh trusts GitLab.
		if result := push(t, hook, "refs/heads/develop", "develop"); result.Ignored != "branch_filtered" {
			t.Fatalf("push = %+v, want branch_filtered", result)
		}
		git := runRefresh(t, f)
		if got := git.fetched(); len(got) != 1 || got[0] != "+refs/heads/main:refs/workspace/base" {
			t.Fatalf("refresh fetched %q, want the default GitLab reports", got)
		}
		if got := storedDefaultBranch(t, f); got != "main" {
			t.Fatalf("stored default branch = %q, want main", got)
		}
	})
}
