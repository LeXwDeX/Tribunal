package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func newTestDispatcher(t *testing.T, st *sqlite.Store, api DispatchGitLab) *Dispatcher {
	t.Helper()
	dispatcher, err := NewDispatcher(DispatcherConfig{
		Store:    st,
		GitLab:   api,
		Cipher:   newTestCipher(t),
		Profiles: testProfiles(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return dispatcher
}

func TestDispatcherStartReviewEnqueuesRun(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
	api := defaultFakeGitLab()
	dispatcher := newTestDispatcher(t, st, api)

	jobID, err := dispatcher.StartReview(ctx, 42, 7, "deep", true)
	if err != nil {
		t.Fatalf("StartReview: %v", err)
	}
	job, err := st.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Kind != "review.run" || job.State != store.JobQueued {
		t.Fatalf("job = %+v", job)
	}
	if job.Key != "review:42:7" {
		t.Fatalf("job key = %q", job.Key)
	}
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	want := map[string]any{
		"project_id":       float64(42),
		"mr_iid":           float64(7),
		"head_sha":         sha40,
		"generation":       float64(1),
		"profile":          "deep",
		"profile_explicit": true,
		"trigger":          "manual",
		"full":             true,
	}
	for key, value := range want {
		if payload[key] != value {
			t.Fatalf("payload[%s] = %v, want %v (payload=%v)", key, payload[key], value, payload)
		}
	}

	nextID, err := dispatcher.StartReview(ctx, 42, 7, "missing", false)
	if err != nil {
		t.Fatalf("second StartReview: %v", err)
	}
	next, err := st.GetJob(ctx, nextID)
	if err != nil {
		t.Fatalf("GetJob second: %v", err)
	}
	if nextID != jobID {
		t.Fatalf("queued job was not replaced: first=%s second=%s", jobID, nextID)
	}
	var nextPayload map[string]any
	if err := json.Unmarshal(next.Payload, &nextPayload); err != nil {
		t.Fatal(err)
	}
	if nextPayload["generation"] != float64(2) {
		t.Fatalf("generation = %v, want 2", nextPayload["generation"])
	}
	if nextPayload["profile"] != "default" {
		t.Fatalf("unknown profile did not fall back: %v", nextPayload["profile"])
	}
	if nextPayload["full"] != false {
		t.Fatalf("full = %v, want false", nextPayload["full"])
	}
}

func TestDispatcherImplicitProfileFollowsRepoDefault(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.profiles["deep"] = testReviewProfile()
	f.setRepoConfig(t, "version: 1\nprofile: deep\n")
	dispatcher := newTestDispatcher(t, f.db, f.gitlab)

	jobID, err := dispatcher.StartReview(ctx, 42, 7, "", false)
	if err != nil {
		t.Fatalf("StartReview: %v", err)
	}
	job, err := f.db.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["profile_explicit"] != false {
		t.Fatalf("implicit profile_explicit = %v, want false (payload=%s)", payload["profile_explicit"], job.Payload)
	}
	if err := f.service.RunReview(ctx, &jobs.Job{
		ID: job.ID, Kind: job.Kind, Payload: job.Payload, MaxAttempts: job.MaxAttempts,
	}); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Profile != "deep" {
		t.Fatalf("implicit profile = %q, want repo default deep", workflow.Profile)
	}
}

func TestDispatcherExplicitProfileWinsOverRepoDefault(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.profiles["deep"] = testReviewProfile()
	f.setRepoConfig(t, "version: 1\nprofile: deep\n")
	dispatcher := newTestDispatcher(t, f.db, f.gitlab)

	jobID, err := dispatcher.StartReview(ctx, 42, 7, "default", false)
	if err != nil {
		t.Fatalf("StartReview: %v", err)
	}
	job, err := f.db.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["profile_explicit"] != true {
		t.Fatalf("explicit profile_explicit = %v, want true (payload=%s)", payload["profile_explicit"], job.Payload)
	}
	if err := f.service.RunReview(ctx, &jobs.Job{
		ID: job.ID, Kind: job.Kind, Payload: job.Payload, MaxAttempts: job.MaxAttempts,
	}); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Profile != "default" {
		t.Fatalf("explicit profile = %q, want default", workflow.Profile)
	}
}

func TestDispatcherStartReviewRejectsClosedMR(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
	dispatcher := newTestDispatcher(t, st, defaultFakeGitLab())

	for _, state := range []string{"closed", "merged"} {
		api := defaultFakeGitLab()
		api.mr.State = state
		dispatcher.gitlab = api
		_, err := dispatcher.StartReview(ctx, 42, 7, "", false)
		var appErr *Error
		if !errors.As(err, &appErr) || appErr.Code != "conflict" {
			t.Fatalf("%s error = %v, want conflict", state, err)
		}
		if _, err := st.GetReviewControl(ctx, 42, 7); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s created a review control: %v", state, err)
		}
	}

	notFound := defaultFakeGitLab()
	notFound.mrErr = &gitlab.Error{Status: 404}
	dispatcher.gitlab = notFound
	_, err := dispatcher.StartReview(ctx, 42, 7, "", false)
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "not_found" {
		t.Fatalf("missing MR error = %v, want not_found", err)
	}

	dispatcher.gitlab = defaultFakeGitLab()
	_, err = dispatcher.StartReview(ctx, 99, 7, "", false)
	if !errors.As(err, &appErr) || appErr.Code != "not_found" {
		t.Fatalf("missing project error = %v, want not_found", err)
	}
}

func seedReviewWorkflow(t *testing.T, st *sqlite.Store) string {
	t.Helper()
	ctx := context.Background()
	if _, err := st.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: sha40,
		Payload: json.RawMessage(`{"trigger":"manual","profile":"default"}`),
	}); err != nil {
		t.Fatalf("EnqueueReviewLatest: %v", err)
	}
	control, err := st.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewControl: %v", err)
	}
	workflowID, _, err := st.CreateReviewWorkflow(ctx, sqlite.ReviewWorkflowRequest{
		JobID:     "seed-workflow-job",
		Spec:      dag.GraphSpec{Nodes: []dag.NodeSpec{{ID: "seed", Kind: "noop"}}},
		Meta:      map[string]string{"trigger": "api", "base_sha": strings.Repeat("1", 40), "fingerprint": "fp", "config_hash": "cfg", "prompt_hash": "prompt"},
		ProjectID: 42, MRIID: 7, HeadSHA: sha40,
		DossierVersion: 1, DossierData: json.RawMessage(`{"seed":true}`),
		ExpectedGeneration: control.Generation,
	})
	if err != nil {
		t.Fatalf("CreateReviewWorkflow: %v", err)
	}
	return workflowID
}

func TestDispatcherCancelReviewEnqueuesCancel(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
	dispatcher := newTestDispatcher(t, st, defaultFakeGitLab())
	workflowID := seedReviewWorkflow(t, st)

	jobID, err := dispatcher.CancelReview(ctx, workflowID)
	if err != nil {
		t.Fatalf("CancelReview: %v", err)
	}
	job, err := st.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Kind != "review.cancel" || job.Key != "review.cancel:42:7" {
		t.Fatalf("cancel job = %+v", job)
	}
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["project_id"] != float64(42) || payload["mr_iid"] != float64(7) {
		t.Fatalf("cancel payload = %v", payload)
	}
	if _, ok := payload["head_sha"]; ok {
		t.Fatalf("cancel payload must not carry head_sha: %v", payload)
	}
	control, err := st.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewControl: %v", err)
	}
	if !control.Cancelled {
		t.Fatal("review control was not marked cancelled")
	}

	if _, err := dispatcher.CancelReview(ctx, "missing"); err == nil {
		t.Fatal("unknown workflow accepted")
	}
}

func TestDispatcherRepublishEnqueuesPublishAndWorkerRunsIt(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-republish-source")
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("seed review: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	dispatcher := newTestDispatcher(t, f.db, f.gitlab)

	publishID, err := dispatcher.RepublishReview(ctx, workflow.ID)
	if err != nil {
		t.Fatalf("RepublishReview: %v", err)
	}
	publishJob, err := f.db.GetJob(ctx, publishID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if publishJob.Kind != "review.publish" {
		t.Fatalf("kind = %q", publishJob.Kind)
	}
	if publishJob.Key != "review.publish:"+workflow.ID {
		t.Fatalf("key = %q", publishJob.Key)
	}
	var payload map[string]any
	if err := json.Unmarshal(publishJob.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["workflow_id"] != workflow.ID {
		t.Fatalf("payload = %v", payload)
	}

	updatesBefore := f.gitlab.summaryUpdates
	if err := f.service.Handle(ctx, &jobs.Job{
		ID:      publishID,
		Kind:    publishJob.Kind,
		Payload: publishJob.Payload,
	}); err != nil {
		t.Fatalf("publish handler: %v", err)
	}
	if f.gitlab.summaryUpdates <= updatesBefore {
		t.Fatal("review.publish job did not re-run publication")
	}
	dossier, err := f.db.GetWorkflowDossier(ctx, workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dossier.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("publication state = %q", dossier.PublicationState)
	}
}

func TestDispatcherRefreshWorkspaceEnqueuesFlags(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
	dispatcher := newTestDispatcher(t, st, defaultFakeGitLab())

	jobID, err := dispatcher.RefreshWorkspace(ctx, 42, true, false)
	if err != nil {
		t.Fatalf("RefreshWorkspace: %v", err)
	}
	job, err := st.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Kind != "workspace.refresh" || job.Key != "ws:42" {
		t.Fatalf("refresh job = %+v", job)
	}
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{
		"project_id":         float64(42),
		"ref":                "refs/heads/main",
		"branch":             "main",
		"trigger":            "manual",
		"regenerate_context": true,
		"reindex":            false,
	}
	for key, value := range expected {
		if payload[key] != value {
			t.Fatalf("payload[%s] = %v, want %v (payload=%v)", key, payload[key], value, payload)
		}
	}

	nextID, err := dispatcher.RefreshWorkspace(ctx, 42, false, true)
	if err != nil {
		t.Fatalf("second RefreshWorkspace: %v", err)
	}
	if nextID != jobID {
		t.Fatalf("queued refresh was not replaced: %s != %s", nextID, jobID)
	}
	next, err := st.GetJob(ctx, nextID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(next.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["regenerate_context"] != false || payload["reindex"] != true {
		t.Fatalf("replaced flags = %v", payload)
	}

	if _, err := dispatcher.RefreshWorkspace(ctx, 99, false, false); err == nil {
		t.Fatal("unknown project accepted")
	}
}

func TestWorkspaceJobPayloadKeepsLegacyDecoding(t *testing.T) {
	var legacy workspaceJobPayload
	if err := json.Unmarshal([]byte(`{"project_id":42,"ref":"refs/heads/main","trigger":"push"}`), &legacy); err != nil {
		t.Fatalf("legacy decode: %v", err)
	}
	if legacy.RegenerateContext != nil || legacy.Reindex != nil {
		t.Fatalf("legacy flags = %v %v", legacy.RegenerateContext, legacy.Reindex)
	}
	var current workspaceJobPayload
	if err := json.Unmarshal([]byte(`{"project_id":42,"regenerate_context":true,"reindex":false}`), &current); err != nil {
		t.Fatal(err)
	}
	if current.RegenerateContext == nil || !*current.RegenerateContext {
		t.Fatalf("regenerate flag = %v", current.RegenerateContext)
	}
	if current.Reindex == nil || *current.Reindex {
		t.Fatalf("reindex flag = %v", current.Reindex)
	}
}

func TestReviewWorkerRegistersPublishHandler(t *testing.T) {
	worker, err := NewReviewWorker(&ReviewService{}, &WorkspaceService{})
	if err != nil {
		t.Fatalf("NewReviewWorker: %v", err)
	}
	if _, ok := worker.Handlers()["review.publish"]; !ok {
		t.Fatal("review.publish handler is not registered")
	}
}

func TestReviewWorkerRegistersCandidateHandler(t *testing.T) {
	worker, err := NewReviewWorker(&ReviewService{}, &WorkspaceService{})
	if err != nil {
		t.Fatalf("NewReviewWorker: %v", err)
	}
	if _, ok := worker.Handlers()["review.candidate"]; !ok {
		t.Fatal("review.candidate handler is not registered")
	}
}
