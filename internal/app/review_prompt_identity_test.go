package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/pkg/dag"
	"github.com/LeXwDeX/tribunal/prompts"
)

// legacyPromptsHash reproduces the bundle hash used before the task-render
// identity was mixed into it. An override file keeps the same content across
// the upgrade, so the legacy value is what an old workflow stored.
func legacyPromptsHash(bundle prompts.Bundle) string {
	names := append([]string(nil), prompts.RoleFiles...)
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00", name, bundle.Versions[name], bundle.Files[name])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func promptOverrideBundle(t *testing.T) (prompts.Bundle, string) {
	t.Helper()
	dir := t.TempDir()
	legacyRules := "<!-- v3 -->\n# Platform rules\n\nLegacy external override body.\n"
	if err := os.WriteFile(filepath.Join(dir, prompts.PlatformRules), []byte(legacyRules), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := prompts.Load(dir)
	if err != nil {
		t.Fatalf("prompts.Load override: %v", err)
	}
	return bundle, legacyPromptsHash(bundle)
}

func applyPromptBundle(f *reviewFixture, bundle prompts.Bundle) {
	f.bundle = bundle
	f.service.prompts = bundle
	f.publisher.promptsHash = bundle.Hash
}

func TestPublishRejectsLegacyPromptRenderIdentityWithOverride(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	bundle, legacy := promptOverrideBundle(t)
	legacyBundle := bundle
	legacyBundle.Hash = legacy
	applyPromptBundle(f, legacyBundle)

	job := f.seedReview(t, "job-legacy-prompt-publish")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle with legacy bundle: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}

	applyPromptBundle(f, bundle)
	err = f.publisher.Publish(context.Background(), workflow.ID)
	if !errors.Is(err, ErrStalePublication) {
		t.Fatalf("Publish error = %v, want ErrStalePublication for the legacy render identity", err)
	}
}

func TestRetryRebuildsLegacyPromptRenderIdentityWithOverride(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	bundle, legacy := promptOverrideBundle(t)
	legacyBundle := bundle
	legacyBundle.Hash = legacy
	applyPromptBundle(f, legacyBundle)

	job := f.seedReview(t, "job-legacy-prompt-retry")
	generation := jobGeneration(t, job)
	configHash, _ := currentReviewHashes(t, f)

	dossier := testDossier(t, f.head)
	dossier.Metadata = map[string]string{
		"config_hash": configHash, "prompt_hash": legacy,
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
	oldID, _, err := f.db.CreateReviewWorkflow(context.Background(), sqlite.ReviewWorkflowRequest{
		JobID:              job.ID,
		Spec:               spec,
		Meta:               map[string]string{"trigger": "mr_open", "base_sha": dossier.DiffVersion.BaseSHA, "fingerprint": reviewFingerprint(hash, configHash, legacy), "config_hash": configHash, "prompt_hash": legacy, "profile": "default"},
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

	applyPromptBundle(f, bundle)
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("retry Handle: %v", err)
	}

	oldWorkflow, err := f.db.GetWorkflow(context.Background(), oldID)
	if err != nil {
		t.Fatalf("GetWorkflow old: %v", err)
	}
	if oldWorkflow.Status != string(dag.WorkflowCancelled) {
		t.Fatalf("old workflow status = %q, want cancelled for the legacy render identity", oldWorkflow.Status)
	}
	newWorkflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if newWorkflow.ID == oldID {
		t.Fatal("retry reused the workflow built with the legacy render identity")
	}
}

func TestRecoveryCancelsLegacyPromptRenderIdentityWithOverride(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	bundle, legacy := promptOverrideBundle(t)
	legacyBundle := bundle
	legacyBundle.Hash = legacy
	applyPromptBundle(f, legacyBundle)

	job := f.seedReview(t, "job-legacy-prompt-recovery")
	wfID := interruptReview(t, f, job)

	applyPromptBundle(f, bundle)
	if err := f.service.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	workflow, err := f.db.GetWorkflow(context.Background(), wfID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if workflow.Status != string(dag.WorkflowCancelled) {
		t.Fatalf("recovered workflow status = %q, want cancelled for the legacy render identity", workflow.Status)
	}
	if got := f.gitlab.noteCount(); got != 0 {
		t.Fatalf("recovery published %d notes from the legacy workflow", got)
	}
}

// legacyPromptDossierStore hides the prompt identity the way a workflow
// created before the identity existed would look: the metadata key is absent
// or present with an empty value. The fingerprint override keeps the workflow
// fingerprint consistent with the old dossier-only formula so the test reaches
// the prompt check instead of an earlier fingerprint mismatch.
type legacyPromptDossierStore struct {
	ReviewStore
	emptyPromptHash bool
}

func (s legacyPromptDossierStore) GetWorkflowDossier(ctx context.Context, id string) (*store.WorkflowDossier, error) {
	record, err := s.ReviewStore.GetWorkflowDossier(ctx, id)
	if err != nil {
		return nil, err
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		return nil, err
	}
	if dossier.Metadata == nil {
		dossier.Metadata = map[string]string{}
	}
	if s.emptyPromptHash {
		dossier.Metadata["prompt_hash"] = ""
	} else {
		delete(dossier.Metadata, "prompt_hash")
	}
	clone := *record
	if clone.Data, err = json.Marshal(dossier); err != nil {
		return nil, err
	}
	return &clone, nil
}

func (s legacyPromptDossierStore) GetWorkflow(ctx context.Context, id string) (*store.Workflow, error) {
	workflow, err := s.ReviewStore.GetWorkflow(ctx, id)
	if err != nil {
		return nil, err
	}
	record, err := s.ReviewStore.GetWorkflowDossier(ctx, id)
	if err != nil {
		return nil, err
	}
	clone := *workflow
	clone.Fingerprint = record.Hash
	return &clone, nil
}

type promptIdentityEffects struct {
	summaryCreates    int
	summaryUpdates    int
	discussionCreates int
	notes             int
	statuses          int
}

func promptIdentitySnapshot(f *reviewFixture) promptIdentityEffects {
	f.gitlab.mu.Lock()
	defer f.gitlab.mu.Unlock()
	return promptIdentityEffects{
		summaryCreates:    f.gitlab.summaryCreates,
		summaryUpdates:    f.gitlab.summaryUpdates,
		discussionCreates: f.gitlab.discussionCreates,
		notes:             len(f.gitlab.notes),
		statuses:          len(f.gitlab.statuses),
	}
}

// publishedCompletedReview runs one normal review through the service and
// returns its completed, published workflow.
func publishedCompletedReview(t *testing.T, f *reviewFixture) *store.Workflow {
	t.Helper()
	job := f.seedReview(t, "job-prompt-identity-completed")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if workflow.Status != string(dag.WorkflowCompleted) || workflow.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("completed workflow = %q/%q, want completed/published", workflow.Status, workflow.PublicationState)
	}
	return workflow
}

// publishedFailureReview runs one review that fails without a submitted
// result and returns its failed workflow with the failure summary published.
func publishedFailureReview(t *testing.T, f *reviewFixture) *store.Workflow {
	t.Helper()
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, agent.ErrNoSubmit
	}
	job := f.seedReview(t, "job-prompt-identity-failed")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if workflow.Status != string(dag.WorkflowFailed) || workflow.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("failed workflow = %q/%q, want failed/published", workflow.Status, workflow.PublicationState)
	}
	return workflow
}

func TestPublishRejectsLegacyPromptDossierIdentity(t *testing.T) {
	for _, mode := range []string{"missing", "empty"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			workflow := publishedCompletedReview(t, f)
			before := promptIdentitySnapshot(f)
			f.publisher.store = legacyPromptDossierStore{ReviewStore: f.db, emptyPromptHash: mode == "empty"}

			err := f.publisher.Publish(ctx, workflow.ID)
			if !errors.Is(err, ErrStalePublication) {
				t.Fatalf("Publish error = %v, want ErrStalePublication", err)
			}
			if after := promptIdentitySnapshot(f); after != before {
				t.Fatalf("stale publication wrote GitLab effects: before=%+v after=%+v", before, after)
			}
			state, err := f.db.GetWorkflow(ctx, workflow.ID)
			if err != nil {
				t.Fatalf("GetWorkflow: %v", err)
			}
			if state.PublicationState != store.WorkflowPublicationPublished {
				t.Fatalf("publication state = %q, want published", state.PublicationState)
			}
		})
	}
}

func TestPublishFailureRejectsLegacyPromptDossierIdentity(t *testing.T) {
	for _, mode := range []string{"missing", "empty"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			workflow := publishedFailureReview(t, f)
			before := promptIdentitySnapshot(f)
			f.publisher.store = legacyPromptDossierStore{ReviewStore: f.db, emptyPromptHash: mode == "empty"}

			err := f.publisher.PublishFailure(ctx, workflow.ID, "review execution failed")
			if !errors.Is(err, ErrStalePublication) {
				t.Fatalf("PublishFailure error = %v, want ErrStalePublication", err)
			}
			if after := promptIdentitySnapshot(f); after != before {
				t.Fatalf("stale failure publication wrote GitLab effects: before=%+v after=%+v", before, after)
			}
			state, err := f.db.GetWorkflow(ctx, workflow.ID)
			if err != nil {
				t.Fatalf("GetWorkflow: %v", err)
			}
			if state.PublicationState != store.WorkflowPublicationPublished {
				t.Fatalf("publication state = %q, want published", state.PublicationState)
			}
		})
	}
}
