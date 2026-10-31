package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

const reviewTestHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestReviewRunPublishesSummaryOnce(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-run-1")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if workflow.Status != "completed" {
		t.Fatalf("workflow status = %q", workflow.Status)
	}
	if workflow.Profile != "default" {
		t.Fatalf("workflow profile = %q", workflow.Profile)
	}
	if _, err := f.db.GetWorkflowDossier(context.Background(), workflow.ID); err != nil {
		t.Fatalf("dossier: %v", err)
	}
	if got := f.gitlab.noteCount(); got != 1 {
		t.Fatalf("notes = %d, want 1", got)
	}
	if f.gitlab.summaryCreates != 1 || f.gitlab.discussionCreates != 1 {
		t.Fatalf("summary creates = %d, discussion creates = %d", f.gitlab.summaryCreates, f.gitlab.discussionCreates)
	}
	publications, err := f.db.ListPublications(context.Background(), workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, publication := range publications {
		kinds[publication.Kind]++
	}
	if kinds["summary"] != 1 || kinds["discussion"] != 1 {
		t.Fatalf("publications = %+v", kinds)
	}
	findings, err := f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: workflow.ID})
	if err != nil || len(findings) != 1 {
		t.Fatalf("findings = %d, err = %v", len(findings), err)
	}

	// A duplicate publish must update the existing summary and skip the
	// existing discussion instead of creating new objects.
	if err := f.publisher.Publish(context.Background(), workflow.ID); err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if got := f.gitlab.noteCount(); got != 1 {
		t.Fatalf("notes after duplicate publish = %d, want 1", got)
	}
	if f.gitlab.summaryCreates != 1 {
		t.Fatalf("summary creates after duplicate publish = %d", f.gitlab.summaryCreates)
	}
	if f.gitlab.discussionCreates != 1 {
		t.Fatalf("discussion creates after duplicate publish = %d", f.gitlab.discussionCreates)
	}
	if f.gitlab.summaryUpdates < 1 {
		t.Fatalf("summary was not updated on duplicate publish")
	}
}

func TestReviewRunSupersededBeforeCreation(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-run-2")
	// A newer cancel (or push) wins before the job starts.
	if _, err := f.db.EnqueueReviewLatest(context.Background(), store.ReviewCommand{
		ProjectID: 42,
		MRIID:     7,
		Cancel:    true,
		Payload:   mustJSONBody(t, map[string]any{"trigger": "command"}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, err := f.db.GetWorkflowByJob(context.Background(), job.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("workflow was created for a superseded job: %v", err)
	}
	if f.gitlab.noteCount() != 0 {
		t.Fatalf("superseded job published %d notes", f.gitlab.noteCount())
	}
}

// incompleteWorkflowStore reports the store sentinel for a workflow row that
// has no dossier. The real window needs a crash between the workflow insert and
// the dossier write, so it cannot be replayed deterministically.
type incompleteWorkflowStore struct {
	*sqlite.Store
}

func (s *incompleteWorkflowStore) CreateReviewWorkflow(context.Context, sqlite.ReviewWorkflowRequest) (string, bool, error) {
	return "", false, fmt.Errorf("%w: workflow wf-incomplete", store.ErrIncompleteWorkflow)
}

func TestRunReviewIncompleteWorkflowIsPermanent(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-run-incomplete")
	f.service.store = &incompleteWorkflowStore{Store: f.db}

	err := f.service.RunReview(context.Background(), job)
	if !IsPermanentJob(err) {
		t.Fatalf("RunReview error = %v, want a permanent error", err)
	}
	if !errors.Is(err, store.ErrIncompleteWorkflow) {
		t.Fatalf("RunReview error = %v, want store.ErrIncompleteWorkflow", err)
	}
}

func TestReviewRunSupersededBeforePublication(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-run-3")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := f.gitlab.noteCount()
	if _, err := f.db.EnqueueReviewLatest(context.Background(), store.ReviewCommand{
		ProjectID: 42,
		MRIID:     7,
		Cancel:    true,
		Payload:   mustJSONBody(t, map[string]any{"trigger": "command"}),
	}); err != nil {
		t.Fatal(err)
	}
	err = f.publisher.Publish(context.Background(), workflow.ID)
	if !errors.Is(err, ErrStalePublication) {
		t.Fatalf("Publish error = %v, want stale", err)
	}
	if f.gitlab.noteCount() != before {
		t.Fatalf("stale publish created a note")
	}
	dossier, err := f.db.GetWorkflowDossier(context.Background(), workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The review was already published before the cancellation. A late stale
	// attempt must not rewrite the published outcome.
	if dossier.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("publication state = %q, want published", dossier.PublicationState)
	}
}

func TestReviewDossierPersistsFreshnessMetadata(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-meta-1")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	record, err := f.db.GetWorkflowDossier(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflowDossier: %v", err)
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		t.Fatalf("decode dossier: %v", err)
	}
	configHash := dossier.Metadata["config_hash"]
	promptHash := dossier.Metadata["prompt_hash"]
	if configHash == "" || promptHash == "" || dossier.Metadata["base_sha"] == "" {
		t.Fatalf("dossier metadata = %+v", dossier.Metadata)
	}
	if promptHash != f.bundle.Hash {
		t.Fatalf("prompt_hash = %q, want %q", promptHash, f.bundle.Hash)
	}
	if dossier.Metadata["base_sha"] != f.gitlab.version.BaseSHA {
		t.Fatalf("base_sha = %q, want %q", dossier.Metadata["base_sha"], f.gitlab.version.BaseSHA)
	}
	if workflow.Fingerprint != reviewFingerprint(record.Hash, configHash, promptHash) {
		t.Fatalf("workflow fingerprint = %q", workflow.Fingerprint)
	}
}

func TestPublishRejectsChangedConfigAndPromptHashes(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, f *reviewFixture)
	}{
		{
			name: "config",
			mutate: func(t *testing.T, f *reviewFixture) {
				f.gitlab.mu.Lock()
				f.gitlab.docs["AGENTS.md"] = "changed repository configuration"
				f.gitlab.mu.Unlock()
			},
		},
		{
			name: "prompt",
			mutate: func(_ *testing.T, f *reviewFixture) {
				f.publisher.promptsHash = "changed-prompt-hash"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			job := f.seedReview(t, "job-stale-"+tc.name)
			if err := f.service.Handle(context.Background(), job); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
			if err != nil {
				t.Fatalf("GetWorkflowByJob: %v", err)
			}
			if err := f.publisher.Publish(context.Background(), workflow.ID); err != nil {
				t.Fatalf("baseline Publish: %v", err)
			}
			tc.mutate(t, f)
			err = f.publisher.Publish(context.Background(), workflow.ID)
			if !errors.Is(err, ErrStalePublication) {
				t.Fatalf("Publish error = %v, want ErrStalePublication", err)
			}
			record, err := f.db.GetWorkflowDossier(context.Background(), workflow.ID)
			if err != nil {
				t.Fatalf("GetWorkflowDossier: %v", err)
			}
			// The baseline publication already completed. A late stale check
			// must preserve published and only report the stale error.
			if record.PublicationState != store.WorkflowPublicationPublished {
				t.Fatalf("publication state = %q, want published", record.PublicationState)
			}
		})
	}
}

func TestReviewRetryRestartsRunningNodeAndKeepsTerminalOutput(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-retry-running")
	generation := jobGeneration(t, job)
	dossier := testDossier(t, f.head)
	configHash, promptHash := currentReviewHashes(t, f)
	dossier.Metadata = map[string]string{
		"config_hash": configHash, "prompt_hash": promptHash,
		"profile_hash":       f.service.reviewProfileHash(review.Profile{Name: "default", Config: f.profiles["default"]}),
		"workspace_identity": "absent",
		"execution_identity": testExecutionIdentity(t, f),
	}
	raw, err := review.MarshalDossier(dossier)
	if err != nil {
		t.Fatalf("MarshalDossier: %v", err)
	}
	hash, err := review.DossierHash(dossier)
	if err != nil {
		t.Fatalf("DossierHash: %v", err)
	}
	spec := dag.GraphSpec{
		Nodes: []dag.NodeSpec{
			{ID: "plan", Kind: review.KindPlan, Required: true, Timeout: time.Minute, Params: json.RawMessage(`{"mode":"code"}`)},
			{ID: "gather", Kind: review.KindGather, DependsOn: []string{"plan"}, Required: true, Timeout: time.Minute, Params: json.RawMessage(`{}`)},
		},
		Budget: dag.Budget{MaxNodes: 30, Deadline: 5 * time.Minute},
	}
	wfID, _, err := f.db.CreateReviewWorkflow(context.Background(), sqlite.ReviewWorkflowRequest{
		JobID:              job.ID,
		Spec:               spec,
		Meta:               map[string]string{"trigger": "mr_open", "base_sha": dossier.DiffVersion.BaseSHA, "fingerprint": reviewFingerprint(hash, configHash, promptHash), "config_hash": configHash, "prompt_hash": promptHash, "profile": "default"},
		DossierVersion:     review.DossierSchemaVersion,
		DossierHash:        hash,
		DossierData:        raw,
		ExpectedGeneration: generation,
		ProjectID:          42,
		MRIID:              7,
		HeadSHA:            f.head,
	})
	if err != nil {
		t.Fatalf("CreateReviewWorkflow: %v", err)
	}
	event := func(kind string, payload any) dag.Event {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %s: %v", kind, err)
		}
		return dag.Event{Type: kind, Payload: encoded, At: time.Now().UTC()}
	}
	if _, err := f.db.Append(context.Background(), wfID, 1,
		event(dag.EventNodeStarted, dag.NodeStartedPayload{NodeID: "plan", Attempt: 1}),
		event(dag.EventNodeCompleted, dag.NodeCompletedPayload{NodeID: "plan", Attempt: 1, Output: json.RawMessage(`{"ok":true}`)}),
		event(dag.EventNodeStarted, dag.NodeStartedPayload{NodeID: "gather", Attempt: 1}),
	); err != nil {
		t.Fatalf("Append setup events: %v", err)
	}

	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("retry Handle: %v", err)
	}
	snapshot, err := f.engine.Snapshot(context.Background(), wfID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Status != dag.WorkflowCompleted {
		t.Fatalf("workflow status = %q, want completed", snapshot.Status)
	}
	gather := snapshot.Nodes["gather"]
	if gather.Attempt != 2 || gather.Status != dag.NodeCompleted {
		t.Fatalf("gather = attempt %d status %q, want attempt 2 completed", gather.Attempt, gather.Status)
	}
	plan := snapshot.Nodes["plan"]
	if plan.Attempt != 1 || plan.Status != dag.NodeCompleted || string(plan.Output) != `{"ok":true}` {
		t.Fatalf("terminal plan changed: attempt %d status %q output %s", plan.Attempt, plan.Status, plan.Output)
	}
	stored, err := f.db.GetWorkflowDossier(context.Background(), wfID)
	if err != nil || string(stored.Data) != string(raw) {
		t.Fatalf("retry did not preserve dossier: %v", err)
	}
}

func TestReviewRetryAfterConfigChangeRebuildsWorkflow(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-config-change")
	generation := jobGeneration(t, job)
	oldConfigHash, oldPromptHash := currentReviewHashes(t, f)

	dossier := testDossier(t, f.head)
	dossier.Metadata = map[string]string{"config_hash": oldConfigHash, "prompt_hash": oldPromptHash}
	raw, err := review.MarshalDossier(dossier)
	if err != nil {
		t.Fatalf("MarshalDossier: %v", err)
	}
	hash, err := review.DossierHash(dossier)
	if err != nil {
		t.Fatalf("DossierHash: %v", err)
	}
	spec := dag.GraphSpec{
		Nodes: []dag.NodeSpec{
			{ID: "plan", Kind: review.KindPlan, Required: true, Timeout: time.Minute, Params: json.RawMessage(`{"mode":"code"}`)},
			{ID: "gather", Kind: review.KindGather, DependsOn: []string{"plan"}, Required: true, Timeout: time.Minute, Params: json.RawMessage(`{}`)},
		},
		Budget: dag.Budget{MaxNodes: 30, Deadline: 5 * time.Minute},
	}
	oldID, _, err := f.db.CreateReviewWorkflow(context.Background(), sqlite.ReviewWorkflowRequest{
		JobID:              job.ID,
		Spec:               spec,
		Meta:               map[string]string{"trigger": "mr_open", "base_sha": dossier.DiffVersion.BaseSHA, "fingerprint": reviewFingerprint(hash, oldConfigHash, oldPromptHash), "config_hash": oldConfigHash, "prompt_hash": oldPromptHash},
		DossierVersion:     review.DossierSchemaVersion,
		DossierHash:        hash,
		DossierData:        raw,
		ExpectedGeneration: generation,
		ProjectID:          42,
		MRIID:              7,
		HeadSHA:            f.head,
	})
	if err != nil {
		t.Fatalf("CreateReviewWorkflow: %v", err)
	}

	f.gitlab.mu.Lock()
	f.gitlab.docs["AGENTS.md"] = "changed repository configuration"
	f.gitlab.mu.Unlock()

	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	oldWorkflow, err := f.db.GetWorkflow(context.Background(), oldID)
	if err != nil {
		t.Fatalf("GetWorkflow old: %v", err)
	}
	if oldWorkflow.Status != string(dag.WorkflowCancelled) {
		t.Fatalf("old workflow status = %q, want cancelled", oldWorkflow.Status)
	}
	oldDossier, err := f.db.GetWorkflowDossier(context.Background(), oldID)
	if err != nil {
		t.Fatalf("GetWorkflowDossier old: %v", err)
	}
	if oldDossier.JobID != "" || oldDossier.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("old dossier = job %q state %q, want detached and skipped", oldDossier.JobID, oldDossier.PublicationState)
	}
	publications, err := f.db.ListPublications(context.Background(), oldID)
	if err != nil {
		t.Fatal(err)
	}
	if len(publications) != 0 {
		t.Fatalf("old workflow published %d objects", len(publications))
	}

	newWorkflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if newWorkflow.ID == oldID {
		t.Fatal("retry reused the workflow built for the old configuration")
	}
	if newWorkflow.Status != string(dag.WorkflowCompleted) {
		t.Fatalf("new workflow status = %q", newWorkflow.Status)
	}
	newDossier, err := f.db.GetWorkflowDossier(context.Background(), newWorkflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflowDossier new: %v", err)
	}
	var stored review.Dossier
	if err := json.Unmarshal(newDossier.Data, &stored); err != nil {
		t.Fatalf("decode dossier: %v", err)
	}
	newConfigHash := stored.Metadata["config_hash"]
	if newConfigHash == "" || newConfigHash == oldConfigHash {
		t.Fatalf("config_hash = %q, want a new hash different from %q", newConfigHash, oldConfigHash)
	}
	if newDossier.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("new dossier publication state = %q, want published", newDossier.PublicationState)
	}
	if got := f.gitlab.noteCount(); got != 1 {
		t.Fatalf("notes = %d, want 1", got)
	}
}

// staleControlStore reports the old generation once, then the durable control.
// It reproduces a lease expiry where a successor job advances the generation
// between the first check and the configuration-change branch.
type staleControlStore struct {
	*sqlite.Store

	mu    sync.Mutex
	calls int
	stale store.ReviewControl
}

func (s *staleControlStore) GetReviewControl(ctx context.Context, projectID, mrIID int64) (*store.ReviewControl, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls == 1 {
		control := s.stale
		return &control, nil
	}
	return s.Store.GetReviewControl(ctx, projectID, mrIID)
}

// successorStore runs onRead after the resumable dossier is read. It
// reproduces a successor job replacing the workflow while this worker runs.
type successorStore struct {
	*sqlite.Store

	mu        sync.Mutex
	triggered bool
	onRead    func()
}

func (s *successorStore) GetWorkflowDossier(ctx context.Context, workflowID string) (*store.WorkflowDossier, error) {
	dossier, err := s.Store.GetWorkflowDossier(ctx, workflowID)
	if err == nil {
		s.mu.Lock()
		run := !s.triggered
		s.triggered = true
		s.mu.Unlock()
		if run {
			s.onRead()
		}
	}
	return dossier, err
}

// seedSuccessorReview advances the MR generation and creates its pending
// workflow, as a successor job would after this worker's lease expired.
func (f *reviewFixture) seedSuccessorReview(t *testing.T, jobID, executionIdentity string) string {
	t.Helper()
	result, err := f.db.EnqueueReviewLatest(context.Background(), store.ReviewCommand{
		ProjectID:   42,
		MRIID:       7,
		HeadSHA:     f.head,
		Payload:     json.RawMessage(`{"trigger":"mr_update","profile":"default","full":true}`),
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest successor: %v", err)
	}
	return createPendingReviewIdentity(t, f, jobID, result.Generation, executionIdentity)
}

func TestStaleWorkerDoesNotCancelNewerGenerationOnConfigChange(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	oldJob := f.seedReview(t, "job-stale-config")
	oldWorkflowID := createPendingReview(t, f, oldJob.ID, jobGeneration(t, oldJob))
	f.gitlab.mu.Lock()
	f.gitlab.docs["AGENTS.md"] = "changed repository configuration"
	f.gitlab.mu.Unlock()

	identity := testExecutionIdentity(t, f)
	store := &successorStore{Store: f.db}
	store.onRead = func() {
		if err := f.engine.Cancel(context.Background(), oldWorkflowID, "successor"); err != nil {
			t.Fatalf("cancel old workflow: %v", err)
		}
		f.seedSuccessorReview(t, "job-successor-config", identity)
	}
	f.service.store = store

	err := f.service.RunReview(context.Background(), oldJob)
	if !errors.Is(err, ErrReviewSuperseded) {
		t.Fatalf("RunReview error = %v, want ErrReviewSuperseded", err)
	}
	newWorkflow, err := f.db.GetWorkflowByJob(context.Background(), "job-successor-config")
	if err != nil {
		t.Fatalf("GetWorkflowByJob successor: %v", err)
	}
	if newWorkflow.Status != string(dag.WorkflowPending) {
		t.Fatalf("newer workflow status = %q, want pending", newWorkflow.Status)
	}
	oldWorkflow, err := f.db.GetWorkflow(context.Background(), oldWorkflowID)
	if err != nil {
		t.Fatalf("GetWorkflow old: %v", err)
	}
	if oldWorkflow.Status != string(dag.WorkflowCancelled) {
		t.Fatalf("old workflow status = %q, want cancelled by the successor", oldWorkflow.Status)
	}
}

func TestRunStartCancelDoesNotCancelNewerGeneration(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	oldJob := f.seedReview(t, "job-start-cancel")
	oldGeneration := jobGeneration(t, oldJob)
	oldWorkflowID := createPendingReview(t, f, oldJob.ID, oldGeneration)
	f.gitlab.mu.Lock()
	f.gitlab.docs["AGENTS.md"] = "changed repository configuration"
	f.gitlab.mu.Unlock()
	if err := f.engine.Cancel(context.Background(), oldWorkflowID, "seed"); err != nil {
		t.Fatalf("seed cancel: %v", err)
	}
	newWorkflowID := f.seedSuccessorReview(t, "job-start-successor", testExecutionIdentity(t, f))

	f.service.store = &staleControlStore{
		Store: f.db,
		stale: store.ReviewControl{
			ProjectID:  42,
			MRIID:      7,
			Generation: oldGeneration,
			HeadSHA:    f.head,
		},
	}

	err := f.service.RunReview(context.Background(), oldJob)
	if !errors.Is(err, ErrReviewSuperseded) {
		t.Fatalf("RunReview error = %v, want ErrReviewSuperseded", err)
	}
	newWorkflow, err := f.db.GetWorkflow(context.Background(), newWorkflowID)
	if err != nil {
		t.Fatalf("GetWorkflow new: %v", err)
	}
	if newWorkflow.Status != string(dag.WorkflowPending) {
		t.Fatalf("newer workflow status = %q, want pending", newWorkflow.Status)
	}
	oldWorkflow, err := f.db.GetWorkflow(context.Background(), oldWorkflowID)
	if err != nil {
		t.Fatalf("GetWorkflow old: %v", err)
	}
	if oldWorkflow.Status != string(dag.WorkflowCancelled) {
		t.Fatalf("old workflow status = %q, want cancelled", oldWorkflow.Status)
	}
}

func TestCancelActiveCancelsPreviousGeneration(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.seedReview(t, "job-generation-1")
	oldWorkflowID := createPendingReview(t, f, "job-old-generation", 1)
	newJob := f.seedReview(t, "job-generation-2")

	if err := f.service.cancelActive(context.Background(), 42, 7, jobGeneration(t, newJob)); err != nil {
		t.Fatalf("cancelActive: %v", err)
	}
	oldWorkflow, err := f.db.GetWorkflow(context.Background(), oldWorkflowID)
	if err != nil {
		t.Fatalf("GetWorkflow old: %v", err)
	}
	if oldWorkflow.Status != string(dag.WorkflowCancelled) {
		t.Fatalf("previous generation status = %q, want cancelled", oldWorkflow.Status)
	}
}

func TestMaxFilesZeroNormalizesToSameReviewHash(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.MaxFiles = 0
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults

	project, err := f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	serviceCfg := f.service.resolveConfig(project, review.RepoDocs{})
	publisherCfg, err := f.publisher.freshConfig(context.Background(), &loadedReview{
		workflow: &store.Workflow{ID: "wf-max-files"},
		dossier:  testDossier(t, f.head),
		project:  project,
		token:    "token",
		mrIID:    7,
	})
	if err != nil {
		t.Fatalf("freshConfig: %v", err)
	}
	if serviceCfg.Settings.MaxFiles != review.DefaultMaxFiles || publisherCfg.Settings.MaxFiles != review.DefaultMaxFiles {
		t.Fatalf("max files = service %d publisher %d, want %d",
			serviceCfg.Settings.MaxFiles, publisherCfg.Settings.MaxFiles, review.DefaultMaxFiles)
	}
	if serviceHash, publisherHash := reviewConfigHash(serviceCfg), reviewConfigHash(publisherCfg); serviceHash != publisherHash {
		t.Fatalf("config hashes differ: service %q publisher %q", serviceHash, publisherHash)
	}
}

func TestRecoverRunVerifiesWorktreeHead(t *testing.T) {
	t.Run("matching head is reused", func(t *testing.T) {
		f := newReviewFixture(t, reviewTestHead)
		consumerReadLock(t, f.manager, 42)
		project, err := f.db.GetProject(context.Background(), 42)
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		headDir := filepath.Join(f.manager.Get(42).Root, "runs", "job-r8")
		if err := os.MkdirAll(headDir, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		f.git.dirHeads[headDir] = f.head
		dossier := testDossier(t, f.head)
		dossier.HeadDir = headDir
		dir, cleanup, err := f.service.recoverRun(context.Background(), project, "token", 7, "job-r8", "wf-r8", *dossier)
		if err != nil {
			t.Fatalf("recoverRun: %v", err)
		}
		if cleanup != nil {
			defer cleanup()
		}
		if dir != headDir {
			t.Fatalf("dir = %q, want %q", dir, headDir)
		}
		if f.git.addCalls != 0 || f.git.removeCalls != 0 {
			t.Fatalf("worktree was rebuilt: add=%d remove=%d", f.git.addCalls, f.git.removeCalls)
		}
	})

	t.Run("wrong head rebuilds", func(t *testing.T) {
		f := newReviewFixture(t, reviewTestHead)
		consumerReadLock(t, f.manager, 42)
		project, err := f.db.GetProject(context.Background(), 42)
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		headDir := filepath.Join(f.manager.Get(42).Root, "runs", "job-r7")
		if err := os.MkdirAll(headDir, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		f.git.dirHeads[headDir] = strings.Repeat("3", 40)
		dossier := testDossier(t, f.head)
		dossier.HeadDir = headDir
		dir, cleanup, err := f.service.recoverRun(context.Background(), project, "token", 7, "job-r7", "wf-r7", *dossier)
		if err != nil {
			t.Fatalf("recoverRun: %v", err)
		}
		if cleanup != nil {
			defer cleanup()
		}
		if dir != headDir {
			t.Fatalf("dir = %q, want rebuilt %q", dir, headDir)
		}
		if f.git.addCalls != 1 || f.git.removeCalls != 1 {
			t.Fatalf("worktree rebuild calls: add=%d remove=%d", f.git.addCalls, f.git.removeCalls)
		}
		if f.git.dirHeads[headDir] != f.head {
			t.Fatalf("rebuilt head = %q, want %q", f.git.dirHeads[headDir], f.head)
		}
	})

	t.Run("unowned path is never deleted", func(t *testing.T) {
		f := newReviewFixture(t, reviewTestHead)
		consumerReadLock(t, f.manager, 42)
		project, err := f.db.GetProject(context.Background(), 42)
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		outside := t.TempDir()
		f.git.dirHeads[outside] = strings.Repeat("4", 40)
		dossier := testDossier(t, f.head)
		dossier.HeadDir = outside
		dir, cleanup, err := f.service.recoverRun(context.Background(), project, "token", 7, "job-r9", "wf-r9", *dossier)
		if err != nil {
			t.Fatalf("recoverRun: %v", err)
		}
		if cleanup != nil {
			defer cleanup()
		}
		if dir == outside {
			t.Fatalf("unowned path was reused")
		}
		if _, err := os.Stat(outside); err != nil {
			t.Fatalf("unowned path was removed: %v", err)
		}
	})
}

func TestReviewCancelSettlesWorkflow(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-run-4")
	generation := jobGeneration(t, job)
	wfID := createPendingReview(t, f, "job-pending-4", generation)
	cancelJob := f.seedCancel(t, "job-cancel-4")
	if err := f.service.Handle(context.Background(), cancelJob); err != nil {
		t.Fatalf("cancel Handle: %v", err)
	}
	workflow, err := f.db.GetWorkflow(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Status != "cancelled" {
		t.Fatalf("workflow status = %q, want cancelled", workflow.Status)
	}
	publications, err := f.db.ListPublications(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if len(publications) != 0 {
		t.Fatalf("cancel published %d objects", len(publications))
	}
	if _, err := f.db.FindActiveReview(context.Background(), 42, 7); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("active review remains after cancel: %v", err)
	}
	_ = job
}

// TestCancelReviewSettlesSameGeneration covers the normal cancel acceptance
// criterion: the cancel command bumps the generation while keeping the head,
// and the pending workflow of the previous generation is cancelled.
func TestCancelReviewSettlesSameGeneration(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	runJob := f.seedReview(t, "job-cancel-same-gen-run")
	generation := jobGeneration(t, runJob)
	wfID := createPendingReview(t, f, runJob.ID, generation)
	cancelJob := f.seedCancel(t, "job-cancel-same-gen")
	if got := jobGeneration(t, cancelJob); got != generation+1 {
		t.Fatalf("cancel generation = %d, want %d", got, generation+1)
	}

	if err := f.service.CancelReview(context.Background(), cancelJob); err != nil {
		t.Fatalf("CancelReview: %v", err)
	}
	workflow, err := f.db.GetWorkflow(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Status != string(dag.WorkflowCancelled) {
		t.Fatalf("workflow status = %q, want cancelled", workflow.Status)
	}
	if _, err := f.db.FindActiveReview(context.Background(), 42, 7); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("active review remains after cancel: %v", err)
	}
}

func TestReviewRecoverRunsPendingAndPublishes(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-run-5")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	first, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstRev := first.GraphRev
	pendingID := createPendingReview(t, f, "job-pending-5", jobGeneration(t, job))
	// A completed workflow whose publication was lost must be retried too.
	if err := f.db.SetWorkflowPublicationState(context.Background(), first.ID, store.WorkflowPublicationPending, ""); err != nil {
		t.Fatal(err)
	}
	beforeNotes := f.gitlab.noteCount()
	if err := f.service.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	pending, err := f.db.GetWorkflow(context.Background(), pendingID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "completed" {
		t.Fatalf("pending workflow status = %q", pending.Status)
	}
	reloaded, err := f.db.GetWorkflow(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.GraphRev != firstRev {
		t.Fatalf("completed workflow was re-run: rev %d -> %d", firstRev, reloaded.GraphRev)
	}
	if got := f.gitlab.noteCount(); got != beforeNotes {
		t.Fatalf("recovery created a new summary note: %d -> %d", beforeNotes, got)
	}
	if f.runner.count() != 2 {
		t.Fatalf("agent runs = %d, want 2 (first run + pending recovery)", f.runner.count())
	}
}

func TestReviewRunClosedMRIsPermanent(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.mr.State = "merged"
	job := f.seedReview(t, "job-run-6")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("closed MR should settle the job, got %v", err)
	}
	if _, err := f.db.GetWorkflowByJob(context.Background(), job.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("closed MR created a workflow")
	}
}

func TestReviewRunNoReviewableFilesPublishesSummary(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.diffs = []gitlab.FileDiff{{NewPath: "go.sum", Diff: "@@ -1 +1 @@\n-a\n+b\n"}}
	f.defaults.Ignore = []string{"**/*.sum"}
	f.service.reviewDefaults = f.defaults
	job := f.seedReview(t, "job-run-7")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if f.gitlab.noteCount() != 1 {
		t.Fatalf("notes = %d, want one empty summary", f.gitlab.noteCount())
	}
	if !strings.Contains(f.gitlab.notes[0].Body, review.SummaryMarkerPrefix) {
		t.Fatalf("summary marker missing")
	}
}

// cancelJobWithNote enqueues a real /review cancel command and returns the job
// payload the handler receives. The enqueue owns the durable cancel-note eyes
// and queued status.
func cancelJobWithNote(t *testing.T, f *reviewFixture, _ string, noteID int64) *jobs.Job {
	t.Helper()
	result, err := f.db.EnqueueReviewLatest(context.Background(), store.ReviewCommand{
		ProjectID: 42,
		MRIID:     7,
		Cancel:    true,
		Payload: mustJSONBody(t, map[string]any{
			"trigger": "command",
			"note_id": noteID,
		}),
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("enqueue cancel: %v", err)
	}
	job, err := f.db.GetJob(context.Background(), result.JobID)
	if err != nil {
		t.Fatalf("GetJob fixture cancel job: %v", err)
	}
	return job
}

func addStatusEyes(f *reviewFixture, noteID, userID int64) {
	f.gitlab.mu.Lock()
	defer f.gitlab.mu.Unlock()
	f.gitlab.nextAwardID++
	f.gitlab.awards[noteID] = append(f.gitlab.awards[noteID], gitlab.Award{
		ID: f.gitlab.nextAwardID, Name: reviewAwardEyes, UserID: userID,
	})
}

// seedWorkflowNoteID stores the command note identity the review started from.
func seedWorkflowNoteID(t *testing.T, f *reviewFixture, workflowID string, noteID int64) {
	t.Helper()
	record, err := f.db.GetWorkflowDossier(context.Background(), workflowID)
	if err != nil {
		t.Fatal(err)
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		t.Fatal(err)
	}
	if dossier.Metadata == nil {
		dossier.Metadata = map[string]string{}
	}
	dossier.Metadata["note_id"] = itoa(noteID)
	raw, err := review.MarshalDossier(&dossier)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := review.DossierHash(&dossier)
	if err != nil {
		t.Fatal(err)
	}
	record.Data = raw
	record.Hash = hash
	if _, err := f.db.SaveWorkflowDossier(context.Background(), *record); err != nil {
		t.Fatal(err)
	}
}

func TestReviewCancelSettlesRunningWorkflow(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	f.runner.setBlock(make(chan struct{}), make(chan struct{}))
	runJob := f.seedReview(t, "job-running-cancel")
	wfID := createPendingReview(t, f, "job-running-cancel", jobGeneration(t, runJob))
	seedWorkflowNoteID(t, f, wfID, 555)
	dossier := testDossier(t, f.head)
	index := gitlab.BuildDiffIndex(dossier.DiffVersion, dossierDiffs(dossier))
	executor, err := review.NewExecutor(review.ExecutorDeps{
		Dossier:  dossier,
		Profile:  review.Profile{Name: "default", Config: testReviewProfile()},
		Graph:    review.GraphConfig{AllowedProviders: []string{"fake"}},
		Prompts:  f.bundle,
		AgentRun: f.runner.Run,
		Snapshot: engineSnapshot(f.engine),
		Locate:   locateFunc(index),
	})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	f.executors.Register(wfID, executor)
	defer f.executors.Unregister(wfID)

	runDone := make(chan error, 1)
	go func() { runDone <- f.engine.Run(context.Background(), wfID) }()
	select {
	case <-f.runner.startedSignal():
	case <-time.After(2 * time.Second):
		t.Fatal("review did not start")
	}
	f.service.setReviewReaction(context.Background(), 42, 7, 555, jobGeneration(t, runJob), reviewAwardEyes)
	addStatusEyes(f, 555, f.gitlab.currentUser.ID)
	addStatusEyes(f, 777, f.gitlab.currentUser.ID)
	addStatusEyes(f, 777, 3)
	cancelJob := cancelJobWithNote(t, f, "job-cancel-running", 777)
	if err := f.service.Handle(context.Background(), cancelJob); err != nil {
		t.Fatalf("cancel Handle: %v", err)
	}
	drainReviewSideEffects(t, f)
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("engine did not stop after cancel")
	}
	workflow, err := f.db.GetWorkflow(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Status != "cancelled" {
		t.Fatalf("workflow status = %q, want cancelled", workflow.Status)
	}
	publications, err := f.db.ListPublications(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if len(publications) != 0 {
		t.Fatalf("cancelled workflow published %d objects", len(publications))
	}
	if awards := f.gitlab.awardSnapshot(777); len(awards) != 1 || awards[0].UserID != 3 {
		t.Fatalf("cancel note awards = %+v, want only the foreign user's eyes", awards)
	}
	if awards := f.gitlab.awardSnapshot(555); len(awards) != 0 {
		t.Fatalf("review note awards = %+v, want bot eyes cleared", awards)
	}
	if phases := statusPhases(f.gitlab.statusUpdates); len(phases) != 1 || phases[0] != reviewStatusCancelled {
		t.Fatalf("status phases = %v, want one cancelled", phases)
	}
	if err := f.service.CancelReview(context.Background(), cancelJob); err != nil {
		t.Fatalf("retried cancel: %v", err)
	}
	if phases := statusPhases(f.gitlab.statusUpdates); len(phases) != 1 {
		t.Fatalf("retried cancel rewrote status phases: %v", phases)
	}
}

// cancelRaceStore triggers a successor review exactly when the cancel job
// looks up the active workflow, reproducing a push landing in that window.
type cancelRaceStore struct {
	*sqlite.Store
	once   sync.Once
	onFind func()
}

func (s *cancelRaceStore) FindActiveReview(ctx context.Context, projectID, mrIID int64) (*store.Workflow, error) {
	s.once.Do(func() {
		if s.onFind != nil {
			s.onFind()
		}
	})
	return s.Store.FindActiveReview(ctx, projectID, mrIID)
}

func TestCancelReviewDoesNotCancelNewerGeneration(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	oldJob := f.seedReview(t, "job-cancel-race-old")
	oldWorkflow := createPendingReview(t, f, oldJob.ID, jobGeneration(t, oldJob))
	cancelJob := f.seedCancel(t, "job-cancel-race")

	var newerWorkflow string
	race := &cancelRaceStore{Store: f.db}
	race.onFind = func() {
		if err := f.engine.Cancel(context.Background(), oldWorkflow, "successor"); err != nil {
			t.Fatalf("cancel old workflow: %v", err)
		}
		result, err := f.db.EnqueueReviewLatest(context.Background(), store.ReviewCommand{
			ProjectID:   42,
			MRIID:       7,
			HeadSHA:     f.head,
			Payload:     json.RawMessage(`{"trigger":"mr_update","profile":"default","full":true}`),
			MaxAttempts: 3,
		})
		if err != nil {
			t.Fatalf("successor enqueue: %v", err)
		}
		newerWorkflow = createPendingReview(t, f, "job-cancel-race-new", result.Generation)
	}
	f.service.store = race

	err := f.service.CancelReview(context.Background(), cancelJob)
	if !IsPermanentJob(err) || !errors.Is(err, ErrReviewSuperseded) {
		t.Fatalf("CancelReview error = %v, want a permanent ErrReviewSuperseded", err)
	}
	if newerWorkflow == "" {
		t.Fatal("successor workflow was not created")
	}
	snapshot, err := f.engine.Snapshot(context.Background(), newerWorkflow)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Status == dag.WorkflowCancelled {
		t.Fatalf("newer generation workflow was cancelled")
	}
	// The successor handler cancelled the old workflow; the invariant under
	// test is that the newer one is untouched.
	oldSnapshot, err := f.engine.Snapshot(context.Background(), oldWorkflow)
	if err != nil {
		t.Fatalf("old Snapshot: %v", err)
	}
	if oldSnapshot.Status != dag.WorkflowCancelled {
		t.Fatalf("old workflow status = %s, want cancelled", oldSnapshot.Status)
	}
}

// dossierOverrideStore replaces the active workflow dossier so the cancel
// generation checks can be exercised without reproducing a store crash.
type dossierOverrideStore struct {
	*sqlite.Store
	dossier *store.WorkflowDossier
	err     error
}

func (s *dossierOverrideStore) GetWorkflowDossier(ctx context.Context, workflowID string) (*store.WorkflowDossier, error) {
	if s.err != nil {
		return nil, s.err
	}
	dossier := *s.dossier
	dossier.WorkflowID = workflowID
	return &dossier, nil
}

func TestCancelReviewDossierMissingIsPermanent(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	runJob := f.seedReview(t, "job-cancel-no-dossier-run")
	wfID := createPendingReview(t, f, runJob.ID, jobGeneration(t, runJob))
	cancelJob := f.seedCancel(t, "job-cancel-no-dossier")
	f.service.store = &dossierOverrideStore{Store: f.db, err: store.ErrNotFound}

	err := f.service.CancelReview(context.Background(), cancelJob)
	if !IsPermanentJob(err) {
		t.Fatalf("CancelReview error = %v, want a permanent error", err)
	}
	if !errors.Is(err, store.ErrIncompleteWorkflow) {
		t.Fatalf("CancelReview error = %v, want store.ErrIncompleteWorkflow", err)
	}
	if !strings.Contains(err.Error(), wfID) {
		t.Fatalf("CancelReview error = %v, want workflow id %s", err, wfID)
	}
	workflow, err := f.db.GetWorkflow(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Status != string(dag.WorkflowPending) {
		t.Fatalf("workflow status = %q, want pending", workflow.Status)
	}
}

func TestCancelReviewNewerDossierIsSuperseded(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	runJob := f.seedReview(t, "job-cancel-newer-dossier-run")
	wfID := createPendingReview(t, f, runJob.ID, jobGeneration(t, runJob))
	cancelJob := f.seedCancel(t, "job-cancel-newer-dossier")
	f.service.store = &dossierOverrideStore{Store: f.db, dossier: &store.WorkflowDossier{
		Generation: jobGeneration(t, cancelJob) + 1,
	}}

	err := f.service.CancelReview(context.Background(), cancelJob)
	if !IsPermanentJob(err) {
		t.Fatalf("CancelReview error = %v, want a permanent error", err)
	}
	if !errors.Is(err, ErrReviewSuperseded) {
		t.Fatalf("CancelReview error = %v, want ErrReviewSuperseded", err)
	}
	workflow, err := f.db.GetWorkflow(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Status == string(dag.WorkflowCancelled) {
		t.Fatalf("newer dossier workflow was cancelled")
	}
}

func TestCancelReviewNewerDossierClearsOnlyCancelEyes(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	runJob := f.seedReview(t, "job-cancel-superseded-run")
	wfID := createPendingReview(t, f, runJob.ID, jobGeneration(t, runJob))
	seedWorkflowNoteID(t, f, wfID, 555)
	before := append([]string(nil), f.gitlab.statusUpdates...)
	addStatusEyes(f, 555, f.gitlab.currentUser.ID)
	addStatusEyes(f, 888, f.gitlab.currentUser.ID)
	cancelJob := cancelJobWithNote(t, f, "job-cancel-superseded", 777)
	addStatusEyes(f, 777, f.gitlab.currentUser.ID)
	f.service.store = &dossierOverrideStore{Store: f.db, dossier: &store.WorkflowDossier{
		Generation: jobGeneration(t, cancelJob) + 1,
	}}

	err := f.service.CancelReview(context.Background(), cancelJob)
	if !IsPermanentJob(err) || !errors.Is(err, ErrReviewSuperseded) {
		t.Fatalf("CancelReview error = %v, want a permanent ErrReviewSuperseded", err)
	}
	drainReviewJobs(t, f, "review.reaction")
	if awards := f.gitlab.awardSnapshot(777); len(awards) != 0 {
		t.Fatalf("cancel note awards = %+v, want cleared", awards)
	}
	if awards := f.gitlab.awardSnapshot(555); len(awards) != 1 {
		t.Fatalf("review note awards = %+v, want untouched", awards)
	}
	if awards := f.gitlab.awardSnapshot(888); len(awards) != 1 {
		t.Fatalf("successor note awards = %+v, want untouched", awards)
	}
	if len(f.gitlab.statusUpdates) != len(before) {
		t.Fatalf("stale cancel rewrote successor status: %v", f.gitlab.statusUpdates)
	}
	workflow, err := f.db.GetWorkflow(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Status != string(dag.WorkflowPending) {
		t.Fatalf("workflow status = %q, want pending", workflow.Status)
	}
}

func TestSupersededCancelClearsOnlyItsOwnEyes(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	runJob := f.seedReview(t, "job-superseded-old")
	wfID := createPendingReview(t, f, runJob.ID, jobGeneration(t, runJob))
	seedWorkflowNoteID(t, f, wfID, 555)
	f.service.setReviewReaction(context.Background(), 42, 7, 555, jobGeneration(t, runJob), reviewAwardEyes)
	cancelJob := cancelJobWithNote(t, f, "job-superseded-cancel", 777)
	addStatusEyes(f, 555, f.gitlab.currentUser.ID)
	addStatusEyes(f, 888, f.gitlab.currentUser.ID)
	addStatusEyes(f, 777, f.gitlab.currentUser.ID)
	if _, err := f.db.EnqueueReviewLatest(context.Background(), store.ReviewCommand{
		ProjectID:   42,
		MRIID:       7,
		HeadSHA:     f.head,
		Payload:     json.RawMessage(`{"trigger":"mr_update","profile":"default","full":true}`),
		MaxAttempts: 3,
	}); err != nil {
		t.Fatalf("successor enqueue: %v", err)
	}

	if err := f.service.CancelReview(context.Background(), cancelJob); err != nil {
		t.Fatalf("CancelReview: %v", err)
	}
	drainReviewJobs(t, f, "review.reaction")
	if awards := f.gitlab.awardSnapshot(777); len(awards) != 0 {
		t.Fatalf("cancel note awards = %+v, want cleared", awards)
	}
	if awards := f.gitlab.awardSnapshot(555); len(awards) != 0 {
		t.Fatalf("old review note awards = %+v, want the stale bot eyes cleared", awards)
	}
	if awards := f.gitlab.awardSnapshot(888); len(awards) != 1 {
		t.Fatalf("successor note awards = %+v, want untouched", awards)
	}
	if len(f.gitlab.statusUpdates) != 0 {
		t.Fatalf("stale cancel wrote a status note: %v", f.gitlab.statusUpdates)
	}
	workflow, err := f.db.GetWorkflow(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Status != string(dag.WorkflowPending) {
		t.Fatalf("workflow status = %q, want pending", workflow.Status)
	}
}

func TestCancelReviewQueuedClearsCancelNoteEyes(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	cancelJob := cancelJobWithNote(t, f, "job-cancel-queued", 777)
	addStatusEyes(f, 777, f.gitlab.currentUser.ID)
	addStatusEyes(f, 777, 3)

	for i := 0; i < 2; i++ {
		if err := f.service.CancelReview(context.Background(), cancelJob); err != nil {
			t.Fatalf("CancelReview %d: %v", i, err)
		}
	}
	drainReviewSideEffects(t, f)
	if awards := f.gitlab.awardSnapshot(777); len(awards) != 1 || awards[0].UserID != 3 {
		t.Fatalf("cancel note awards = %+v, want only the foreign user's eyes", awards)
	}
	if got := f.gitlab.noteCount(); got != 1 {
		t.Fatalf("notes = %d, want only the status note", got)
	}
	if phases := statusPhases(f.gitlab.statusUpdates); len(phases) != 1 || phases[0] != reviewStatusCancelled {
		t.Fatalf("status phases = %v, want one cancelled", phases)
	}
}

func TestCancelReviewRejectsNegativeNoteID(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	cancelJob := f.seedCancel(t, "job-cancel-negative")
	var payload map[string]any
	if err := json.Unmarshal(cancelJob.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload["note_id"] = -1
	cancelJob.Payload = mustJSONBody(t, payload)

	if err := f.service.CancelReview(context.Background(), cancelJob); !IsPermanentJob(err) {
		t.Fatalf("CancelReview error = %v, want a permanent error", err)
	}
}

// publishRaceStore advances the review generation before the second control
// read inside publishEmpty.
type publishRaceStore struct {
	*sqlite.Store
	mu       sync.Mutex
	calls    int
	onSecond func()
}

func (s *publishRaceStore) GetReviewControl(ctx context.Context, projectID, mrIID int64) (*store.ReviewControl, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call == 2 && s.onSecond != nil {
		s.onSecond()
	}
	return s.Store.GetReviewControl(ctx, projectID, mrIID)
}

func TestPublishEmptyRejectsControlChangeBeforePublish(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-empty-race")
	control, err := f.db.GetReviewControl(context.Background(), 42, 7)
	if err != nil {
		t.Fatalf("GetReviewControl: %v", err)
	}
	project, err := f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	race := &publishRaceStore{Store: f.db}
	race.onSecond = func() {
		if _, err := f.db.EnqueueReviewLatest(context.Background(), store.ReviewCommand{
			ProjectID:   42,
			MRIID:       7,
			HeadSHA:     f.head,
			Payload:     json.RawMessage(`{"trigger":"mr_update","profile":"default","full":true}`),
			MaxAttempts: 3,
		}); err != nil {
			t.Fatalf("successor enqueue: %v", err)
		}
	}
	f.service.store = race
	inputs := &reviewInputs{
		MR:      &gitlab.MergeRequest{IID: 7, State: "opened", SHA: f.head, Labels: nil},
		Version: &gitlab.Version{HeadSHA: f.head, BaseSHA: strings.Repeat("c", 40), StartSHA: strings.Repeat("c", 40)},
	}
	err = f.service.publishEmpty(context.Background(), project, "token", control, inputs, effectiveReviewConfig{Settings: f.defaults}, review.DiffSummary{}, nil)
	if !errors.Is(err, ErrReviewSuperseded) {
		t.Fatalf("publishEmpty error = %v, want ErrReviewSuperseded", err)
	}
	if f.gitlab.noteCount() != 0 {
		t.Fatalf("stale empty summary was published (%d notes)", f.gitlab.noteCount())
	}
	if jobGeneration(t, job) <= 0 {
		t.Fatal("seed job has no generation")
	}
}

func TestPublishEmptyRejectsIncompleteInputs(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.seedReview(t, "job-empty-inputs")
	control, err := f.db.GetReviewControl(context.Background(), 42, 7)
	if err != nil {
		t.Fatalf("GetReviewControl: %v", err)
	}
	project, err := f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	cases := []struct {
		name   string
		inputs *reviewInputs
	}{
		{name: "nil inputs", inputs: nil},
		{name: "missing merge request", inputs: &reviewInputs{
			Version: &gitlab.Version{HeadSHA: f.head},
		}},
		{name: "missing version", inputs: &reviewInputs{
			MR: &gitlab.MergeRequest{IID: 7, State: "opened", SHA: f.head},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := f.service.publishEmpty(context.Background(), project, "token", control, tc.inputs, effectiveReviewConfig{Settings: f.defaults}, review.DiffSummary{}, nil)
			if !IsPermanentJob(err) {
				t.Fatalf("publishEmpty error = %v, want a permanent error", err)
			}
			if f.gitlab.noteCount() != 0 {
				t.Fatalf("incomplete inputs published %d notes", f.gitlab.noteCount())
			}
		})
	}
}

var _ = dag.WorkflowCancelled
