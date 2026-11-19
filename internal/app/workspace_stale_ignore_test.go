package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// Before #457 any non-empty ignore list counted as restricted, and the
// built-in defaults are always present, so every project registered or
// refreshed then was stored as state=ready, index_sha empty and
// last_error="index: blocked by ignore policy". That text was never evaluated
// again: after the upgrade those workspaces kept withholding the shared index
// until a push reached the default branch (#618).

const (
	staleIgnoreHealedMessage  = "stale ignore block refresh enqueued"
	staleIgnoreFailureMessage = "stale ignore block refresh enqueue failed"
)

// staleIgnoreOldBase is the base such a workspace recorded long ago;
// reviewTestHead is what the fake GitLab serves now.
var staleIgnoreOldBase = strings.Repeat("a", 40)

type workspaceStateSaver interface {
	SaveWorkspace(ctx context.Context, ws store.Workspace) (*store.Workspace, error)
}

type workspaceJobClaimer interface {
	Claim(ctx context.Context, worker string, kinds []string, lease time.Duration) (*store.Job, error)
}

// seedWorkspaceState writes the workspace row of one project.
func seedWorkspaceState(t *testing.T, st workspaceStateSaver, projectID int64, state, lastError string) {
	t.Helper()
	refreshedAt := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	if _, err := st.SaveWorkspace(context.Background(), store.Workspace{
		ProjectID: projectID, State: state, BaseSHA: staleIgnoreOldBase, LastRefreshAt: &refreshedAt, LastError: lastError,
	}); err != nil {
		t.Fatalf("SaveWorkspace(%d): %v", projectID, err)
	}
}

// seedStaleIgnoreBlocked leaves the project in the state a pre-#457 refresh
// wrote: ready, no index, the blocked-by-ignore text.
func seedStaleIgnoreBlocked(t *testing.T, st workspaceStateSaver, projectID int64) {
	t.Helper()
	seedWorkspaceState(t, st, projectID, "ready", store.WorkspaceIndexBlockedByIgnore)
}

// newStaleIgnoreService is a workspace service with an index command that
// succeeds and platform defaults that do not restrict the ignore list.
func newStaleIgnoreService(t *testing.T, f *reviewFixture) (*WorkspaceService, *bytes.Buffer) {
	t.Helper()
	service := unrestrictedIndexService(t, f)
	setIndexFactory(service, indexTestManager(t, []string{indexTrueCommand}))
	logs := &bytes.Buffer{}
	service.logger = jsonLogger(logs)
	return service, logs
}

// claimWorkspaceRefresh leases the next queued workspace.refresh job the way
// the worker would; it returns nil when none is queued.
func claimWorkspaceRefresh(t *testing.T, st workspaceJobClaimer) *store.Job {
	t.Helper()
	job, err := st.Claim(context.Background(), "stale-ignore-test", []string{"workspace.refresh"}, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return job
}

func staleIgnoreRefreshPayload(projectID int64) map[string]any {
	return map[string]any{
		"project_id": float64(projectID), "ref": "", "branch": "", "after_sha": "",
		"trigger": "stale_ignore_block", "reindex": true, "regenerate_context": false,
	}
}

func requireWorkspaceState(t *testing.T, f *reviewFixture, projectID int64) *store.Workspace {
	t.Helper()
	state, err := f.db.GetWorkspace(context.Background(), projectID)
	if err != nil {
		t.Fatalf("GetWorkspace(%d): %v", projectID, err)
	}
	return state
}

func TestRefreshStaleIgnoreBlockedQueuesOneReindexThatHealsTheWorkspace(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	seedStaleIgnoreBlocked(t, f.db, 42)
	service, logs := newStaleIgnoreService(t, f)

	count, err := service.RefreshStaleIgnoreBlocked(ctx)
	if err != nil || count != 1 {
		t.Fatalf("RefreshStaleIgnoreBlocked = %d, %v; want one queued refresh", count, err)
	}
	job := claimWorkspaceRefresh(t, f.db)
	if job == nil {
		t.Fatal("no workspace.refresh job was queued")
	}
	if job.Kind != "workspace.refresh" || job.Key != "ws:42" {
		t.Fatalf("job = kind %q key %q, want workspace.refresh ws:42", job.Kind, job.Key)
	}
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("decode payload %s: %v", job.Payload, err)
	}
	if want := staleIgnoreRefreshPayload(42); !reflect.DeepEqual(payload, want) {
		t.Fatalf("payload = %v, want %v", payload, want)
	}
	if extra := claimWorkspaceRefresh(t, f.db); extra != nil {
		t.Fatalf("a second refresh was queued: %s", extra.Payload)
	}
	records := logRecordsWith(decodeLogRecords(t, logs), staleIgnoreHealedMessage)
	if len(records) != 1 {
		t.Fatalf("%q logged %d times, want once: %s", staleIgnoreHealedMessage, len(records), logs.String())
	}
	requireLogFields(t, records[0], "INFO", "project_id")

	// The queued job is an ordinary refresh: it indexes the current base and
	// clears the stale text without regenerating the project context.
	if err := service.Handle(ctx, job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	state := requireWorkspaceState(t, f, 42)
	if state.State != "ready" || state.BaseSHA != reviewTestHead || state.IndexSHA != reviewTestHead || state.LastError != "" {
		t.Fatalf("workspace after the refresh = %+v, want ready, indexed at %s, no error", state, reviewTestHead)
	}
	if _, err := f.db.GetCurrentContextDoc(ctx, 42, "agents"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the refresh regenerated the project context: %v", err)
	}

	// Healed: the next startup has nothing left to queue.
	queue := &recordingJobQueue{}
	service.queue = queue
	if count, err := service.RefreshStaleIgnoreBlocked(ctx); err != nil || count != 0 || len(queue.enqueued()) != 0 {
		t.Fatalf("second pass = %d, %v, jobs %v; want nothing queued", count, err, queue.enqueued())
	}
}

// The decision follows the current rule, not the stored text: the built-in
// default list alone no longer restricts (#457), any entry beyond it does.
func TestRefreshStaleIgnoreBlockedFollowsTheCurrentIgnorePolicy(t *testing.T) {
	extra := append(config.DefaultReviewIgnorePatterns(), "docs/**")
	cases := []struct {
		name  string
		setup func(t *testing.T, f *reviewFixture, service *WorkspaceService)
		want  int
	}{
		{
			name: "platform defaults are the built-in list",
			setup: func(_ *testing.T, _ *reviewFixture, service *WorkspaceService) {
				service.reviewDefaults.Ignore = config.DefaultReviewIgnorePatterns()
			},
			want: 1,
		},
		{
			name: "project settings repeat a built-in pattern",
			setup: func(t *testing.T, f *reviewFixture, _ *WorkspaceService) {
				updateProjectSettings(t, f, `{"ignore":["**/*.lock"]}`)
			},
			want: 1,
		},
		{
			name: "platform defaults add an entry",
			setup: func(_ *testing.T, _ *reviewFixture, service *WorkspaceService) {
				service.reviewDefaults.Ignore = extra
			},
			want: 0,
		},
		{
			name: "project settings add an entry",
			setup: func(t *testing.T, f *reviewFixture, _ *WorkspaceService) {
				updateProjectSettings(t, f, `{"ignore":["docs/**"]}`)
			},
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			seedStaleIgnoreBlocked(t, f.db, 42)
			service, _ := newStaleIgnoreService(t, f)
			tc.setup(t, f, service)

			count, err := service.RefreshStaleIgnoreBlocked(context.Background())
			if err != nil || count != tc.want {
				t.Fatalf("RefreshStaleIgnoreBlocked = %d, %v; want %d", count, err, tc.want)
			}
			job := claimWorkspaceRefresh(t, f.db)
			if (job != nil) != (tc.want == 1) {
				t.Fatalf("queued job = %v, want queued = %v", job, tc.want == 1)
			}
			if tc.want == 0 {
				if state := requireWorkspaceState(t, f, 42); state.LastError != store.WorkspaceIndexBlockedByIgnore || state.IndexSHA != "" {
					t.Fatalf("a restricted workspace was modified: %+v", state)
				}
			}
		})
	}
}

func updateProjectSettings(t *testing.T, f *reviewFixture, settings string) {
	t.Helper()
	if _, err := f.db.UpdateProject(context.Background(), 42, store.ProjectUpdate{Settings: json.RawMessage(settings)}); err != nil {
		t.Fatalf("UpdateProject settings: %v", err)
	}
}

// A repository .gitlab-review.yml may still restrict the ignore list. The
// startup check reads only the platform defaults and project settings (no
// GitLab call), so it queues the refresh; the refresh evaluates the full rule
// and writes the same text back instead of indexing.
func TestRefreshStaleIgnoreBlockedLeavesRepositoryRestrictionToTheRefresh(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.docs[".gitlab-review.yml"] = "ignore: [secret/**]\n"
	seedStaleIgnoreBlocked(t, f.db, 42)
	service, _ := newStaleIgnoreService(t, f)

	if count, err := service.RefreshStaleIgnoreBlocked(ctx); err != nil || count != 1 {
		t.Fatalf("RefreshStaleIgnoreBlocked = %d, %v; want one queued refresh", count, err)
	}
	job := claimWorkspaceRefresh(t, f.db)
	if job == nil {
		t.Fatal("no workspace.refresh job was queued")
	}
	if err := service.Handle(ctx, job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	state := requireWorkspaceState(t, f, 42)
	if state.State != "ready" || state.BaseSHA != reviewTestHead || state.IndexSHA != "" || state.LastError != store.WorkspaceIndexBlockedByIgnore {
		t.Fatalf("workspace = %+v, want ready on the new base, not indexed, still blocked by the repository policy", state)
	}
}

func TestRefreshStaleIgnoreBlockedKeepsAnAlreadyQueuedRefresh(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	seedStaleIgnoreBlocked(t, f.db, 42)
	service, logs := newStaleIgnoreService(t, f)
	queuedPayload := `{"project_id":42,"ref":"refs/heads/main","branch":"main","after_sha":"","trigger":"manual","reindex":false,"regenerate_context":false}`
	queuedID, dup, err := f.db.Enqueue(ctx, store.Job{Kind: "workspace.refresh", Key: "ws:42", Payload: json.RawMessage(queuedPayload), MaxAttempts: 3})
	if err != nil || dup {
		t.Fatalf("seed queued refresh: %v (dup %v)", err, dup)
	}

	count, err := service.RefreshStaleIgnoreBlocked(ctx)
	if err != nil || count != 0 {
		t.Fatalf("RefreshStaleIgnoreBlocked = %d, %v; want nothing queued next to the existing refresh", count, err)
	}
	stored, err := f.db.GetJob(ctx, queuedID)
	if err != nil || string(stored.Payload) != queuedPayload {
		t.Fatalf("queued refresh = %+v, %v; want its payload untouched", stored, err)
	}
	job := claimWorkspaceRefresh(t, f.db)
	if job == nil || job.ID != queuedID {
		t.Fatalf("claimed %+v, want the original refresh %s", job, queuedID)
	}
	if extra := claimWorkspaceRefresh(t, f.db); extra != nil {
		t.Fatalf("a duplicate refresh was queued: %s", extra.Payload)
	}
	if records := logRecordsWith(decodeLogRecords(t, logs), staleIgnoreHealedMessage); len(records) != 0 {
		t.Fatalf("logged an enqueue that did not happen: %v", records)
	}
	// The queued manual refresh heals the workspace on its own (below).
	if err := service.Handle(ctx, job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if state := requireWorkspaceState(t, f, 42); state.IndexSHA != reviewTestHead || state.LastError != "" {
		t.Fatalf("workspace after the queued manual refresh = %+v, want indexed and clean", state)
	}
}

func TestRefreshStaleIgnoreBlockedSkipsWorkspacesThatAreNotStale(t *testing.T) {
	cases := []struct {
		name      string
		skipSeed  bool
		state     string
		lastError string
		setup     func(t *testing.T, f *reviewFixture, service *WorkspaceService)
	}{
		{
			name: "project is disabled", state: "ready", lastError: store.WorkspaceIndexBlockedByIgnore,
			setup: func(t *testing.T, f *reviewFixture, _ *WorkspaceService) {
				disabled := false
				if _, err := f.db.UpdateProject(context.Background(), 42, store.ProjectUpdate{Enabled: &disabled}); err != nil {
					t.Fatalf("disable project: %v", err)
				}
			},
		},
		{
			name: "MCP is not configured", state: "ready", lastError: store.WorkspaceIndexBlockedByIgnore,
			setup: func(_ *testing.T, _ *reviewFixture, service *WorkspaceService) { service.mcpFactory = nil },
		},
		{name: "workspace row is missing", skipSeed: true},
		{name: "workspace is new", state: "new", lastError: store.WorkspaceIndexBlockedByIgnore},
		{name: "workspace is onboarding", state: "onboarding", lastError: store.WorkspaceIndexBlockedByIgnore},
		{name: "workspace is refreshing", state: "refreshing", lastError: store.WorkspaceIndexBlockedByIgnore},
		{name: "workspace is in error", state: "error", lastError: store.WorkspaceIndexBlockedByIgnore},
		{name: "no last error", state: "ready", lastError: ""},
		{name: "indexing failed", state: "ready", lastError: store.WorkspaceIndexFailed},
		{name: "another failure", state: "ready", lastError: store.WorkspaceGitNetwork},
		{name: "text differs in case", state: "ready", lastError: "Index: blocked by ignore policy"},
		{name: "text has trailing space", state: "ready", lastError: store.WorkspaceIndexBlockedByIgnore + " "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			if !tc.skipSeed {
				seedWorkspaceState(t, f.db, 42, tc.state, tc.lastError)
			}
			service, logs := newStaleIgnoreService(t, f)
			if tc.setup != nil {
				tc.setup(t, f, service)
			}

			count, err := service.RefreshStaleIgnoreBlocked(context.Background())
			if err != nil || count != 0 {
				t.Fatalf("RefreshStaleIgnoreBlocked = %d, %v; want nothing queued", count, err)
			}
			if job := claimWorkspaceRefresh(t, f.db); job != nil {
				t.Fatalf("a refresh was queued: %s", job.Payload)
			}
			if logs.Len() != 0 {
				t.Fatalf("a skipped workspace logged: %s", logs.String())
			}
		})
	}
}

func TestRefreshStaleIgnoreBlockedWalksEveryProject(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	cipher := newTestCipher(t)
	seedProject(t, f.db, cipher, 43, "whsec", "{}", "default")
	seedProject(t, f.db, cipher, 44, "whsec", "{}", "default")
	seedProject(t, f.db, cipher, 45, "whsec", `{"ignore":["docs/**"]}`, "default")
	for _, id := range []int64{42, 43, 44, 45} {
		seedStaleIgnoreBlocked(t, f.db, id)
	}
	disabled := false
	if _, err := f.db.UpdateProject(ctx, 43, store.ProjectUpdate{Enabled: &disabled}); err != nil {
		t.Fatalf("disable project 43: %v", err)
	}
	service, logs := newStaleIgnoreService(t, f)
	queue := &recordingJobQueue{}
	service.queue = queue

	count, err := service.RefreshStaleIgnoreBlocked(ctx)
	if err != nil || count != 2 {
		t.Fatalf("RefreshStaleIgnoreBlocked = %d, %v; want projects 42 and 44 only", count, err)
	}
	var keys []string
	for _, job := range queue.enqueued() {
		keys = append(keys, job.Kind+" "+job.Key)
	}
	if want := []string{"workspace.refresh ws:42", "workspace.refresh ws:44"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("queued %v, want %v", keys, want)
	}
	var ids []float64
	for _, record := range logRecordsWith(decodeLogRecords(t, logs), staleIgnoreHealedMessage) {
		ids = append(ids, record["project_id"].(float64))
	}
	if want := []float64{42, 44}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("logged projects %v, want %v", ids, want)
	}
}

// A failed enqueue never fails the startup: it is logged by error class (the
// raw error may carry paths or payloads) and the pass moves on.
func TestRefreshStaleIgnoreBlockedReportsEnqueueFailureByClass(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	cipher := newTestCipher(t)
	seedProject(t, f.db, cipher, 43, "whsec", "{}", "default")
	seedStaleIgnoreBlocked(t, f.db, 42)
	seedStaleIgnoreBlocked(t, f.db, 43)
	service, logs := newStaleIgnoreService(t, f)
	service.queue = &recordingJobQueue{err: errors.New("queue unavailable: /var/lib/tribunal/secret-token")}

	count, err := service.RefreshStaleIgnoreBlocked(context.Background())
	if err != nil || count != 0 {
		t.Fatalf("RefreshStaleIgnoreBlocked = %d, %v; want the failure logged, not returned", count, err)
	}
	records := logRecordsWith(decodeLogRecords(t, logs), staleIgnoreFailureMessage)
	if len(records) != 2 {
		t.Fatalf("%q logged %d times, want once per project: %s", staleIgnoreFailureMessage, len(records), logs.String())
	}
	for _, record := range records {
		if record["level"] != "WARN" || record["error_class"] == nil || record["project_id"] == nil || len(record) != 5 {
			t.Fatalf("failure record = %v, want a WARN with project_id and error_class only", record)
		}
	}
	requireNoLogLeak(t, logs, "queue unavailable", "secret-token", "/var/lib")
}

func TestRefreshStaleIgnoreBlockedStopsWhenTheContextEnds(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	seedStaleIgnoreBlocked(t, f.db, 42)
	service, _ := newStaleIgnoreService(t, f)
	queue := &recordingJobQueue{}
	service.queue = queue
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	count, err := service.RefreshStaleIgnoreBlocked(ctx)
	if !errors.Is(err, context.Canceled) || count != 0 || len(queue.enqueued()) != 0 {
		t.Fatalf("RefreshStaleIgnoreBlocked = %d, %v, jobs %v; want a canceled pass that queued nothing", count, err, queue.enqueued())
	}
}

// A manual refresh without reindex used to rewrite the stale text to "code
// graph indexing failed" without ever indexing, hiding the real reason. With a
// stale text and a policy that no longer restricts, it rebuilds the index.
func TestRefreshWithoutReindexHealsStaleIgnoreBlockedWorkspace(t *testing.T) {
	cases := []struct {
		name    string
		reindex *bool
	}{
		{name: "plain refresh", reindex: contextBoolPtr(false)},
		{name: "reindex left to the default", reindex: nil},
		{name: "explicit reindex", reindex: contextBoolPtr(true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			seedStaleIgnoreBlocked(t, f.db, 42)
			service, _ := newStaleIgnoreService(t, f)

			if err := service.RefreshWithOptions(context.Background(), 42, "main", WorkspaceRefreshOptions{
				Reindex: tc.reindex, RegenerateContext: contextBoolPtr(false),
			}); err != nil {
				t.Fatalf("RefreshWithOptions: %v", err)
			}
			state := requireWorkspaceState(t, f, 42)
			if state.State != "ready" || state.BaseSHA != reviewTestHead || state.IndexSHA != reviewTestHead || state.LastError != "" {
				t.Fatalf("workspace = %+v, want ready and indexed at %s with no error", state, reviewTestHead)
			}
		})
	}
}

func TestRefreshWithoutReindexKeepsBlockedTextWhileIgnoreIsRestricted(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	seedStaleIgnoreBlocked(t, f.db, 42)
	service, _ := newStaleIgnoreService(t, f)
	service.reviewDefaults.Ignore = append(config.DefaultReviewIgnorePatterns(), "docs/**")

	if err := service.RefreshWithOptions(context.Background(), 42, "main", WorkspaceRefreshOptions{
		Reindex: contextBoolPtr(false), RegenerateContext: contextBoolPtr(false),
	}); err != nil {
		t.Fatalf("RefreshWithOptions: %v", err)
	}
	state := requireWorkspaceState(t, f, 42)
	if state.IndexSHA != "" || state.LastError != store.WorkspaceIndexBlockedByIgnore {
		t.Fatalf("workspace = %+v, want the blocked text kept and no index while the policy restricts", state)
	}
}

// Without the stale text a plain refresh still does not index (the old
// behavior this change leaves alone).
func TestRefreshWithoutReindexStillSkipsIndexingForOtherStates(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	seedWorkspaceState(t, f.db, 42, "ready", "")
	service, _ := newStaleIgnoreService(t, f)

	if err := service.RefreshWithOptions(context.Background(), 42, "main", WorkspaceRefreshOptions{
		Reindex: contextBoolPtr(false), RegenerateContext: contextBoolPtr(false),
	}); err != nil {
		t.Fatalf("RefreshWithOptions: %v", err)
	}
	if state := requireWorkspaceState(t, f, 42); state.IndexSHA != "" || state.LastError != "" {
		t.Fatalf("workspace = %+v, want a plain refresh to leave the index alone", state)
	}
}

// Recover runs the self-heal after recovery and the catch-up scan, so the
// refresh it queues is picked up by the already running worker.
func TestRecoverQueuesStaleIgnoreBlockedRefresh(t *testing.T) {
	ctx := context.Background()
	runtime := newLifecycleRuntime(t)
	seedStaleIgnoreBlocked(t, runtime.store, 42)

	if err := runtime.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	job := claimWorkspaceRefresh(t, runtime.store)
	if job == nil {
		t.Fatal("Recover queued no workspace.refresh job")
	}
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("decode payload %s: %v", job.Payload, err)
	}
	if job.Key != "ws:42" || !reflect.DeepEqual(payload, staleIgnoreRefreshPayload(42)) {
		t.Fatalf("job = key %q payload %v, want the stale-ignore refresh of project 42", job.Key, payload)
	}
}

func TestRecoverWithoutWorkspaceServiceQueuesNothing(t *testing.T) {
	runtime := newLifecycleRuntime(t)
	seedStaleIgnoreBlocked(t, runtime.store, 42)
	runtime.workspaces = nil

	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if job := claimWorkspaceRefresh(t, runtime.store); job != nil {
		t.Fatalf("Recover queued %s without a workspace service", job.Payload)
	}
}

func TestRecoverDoesNotFailWhenTheSelfHealCannotQueue(t *testing.T) {
	runtime := newLifecycleRuntime(t)
	seedStaleIgnoreBlocked(t, runtime.store, 42)
	logs := &bytes.Buffer{}
	runtime.logger = jsonLogger(logs)
	runtime.workspaces.logger = jsonLogger(logs)
	runtime.workspaces.queue = &recordingJobQueue{err: errors.New("queue unavailable")}

	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if records := logRecordsWith(decodeLogRecords(t, logs), staleIgnoreFailureMessage); len(records) != 1 {
		t.Fatalf("%q logged %d times, want once: %s", staleIgnoreFailureMessage, len(records), logs.String())
	}
}
