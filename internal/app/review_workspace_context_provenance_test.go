package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

type contextEvidenceStore struct {
	ReviewStore
	doc store.ContextDoc
}

func (s contextEvidenceStore) GetCurrentContextDoc(context.Context, int64, string) (*store.ContextDoc, error) {
	doc := s.doc
	return &doc, nil
}

func evidenceWorkflow(t *testing.T, f *reviewFixture, meta map[string]string, completed bool) string {
	t.Helper()
	ctx := context.Background()
	meta["trigger"] = "register"
	id, err := f.engine.Create(ctx, dag.GraphSpec{Nodes: []dag.NodeSpec{{ID: "onboarding", Kind: review.KindOnboarding, Required: true}}}, meta)
	if err != nil {
		t.Fatal(err)
	}
	if completed {
		event := func(kind string, payload any) dag.Event {
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			return dag.Event{Type: kind, Payload: raw, At: time.Now().UTC()}
		}
		if _, err := f.db.Append(ctx, id, 1, event(dag.EventWorkflowStarted, struct{}{}), event(dag.EventNodeStarted, dag.NodeStartedPayload{NodeID: "onboarding", Attempt: 1}), event(dag.EventNodeCompleted, dag.NodeCompletedPayload{NodeID: "onboarding", Attempt: 1, Output: json.RawMessage(`{}`)}), event(dag.EventWorkflowCompleted, dag.WorkflowTerminalPayload{})); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestContextGeneratedRequiresSafeWorkflowEvidence(t *testing.T) {
	target := strings.Repeat("2", 40)
	for _, tc := range []struct {
		name                          string
		source, marker, project, base string
		completed                     bool
		missing                       bool
		want                          bool
	}{
		{"legacy-no-workflow", "generated", "", "42", target, true, true, false},
		{"legacy-no-marker", "generated", "", "42", target, true, false, false},
		{"wrong-marker", "generated", "2", "42", target, true, false, false},
		{"wrong-project", "generated", "1", "42", target, true, false, false},
		{"wrong-base", "generated", "1", "42", strings.Repeat("1", 40), true, false, false},
		{"unfinished", "generated", "1", "42", target, false, false, false},
		{"unknown-source", "unknown", "1", "42", target, true, false, false},
		{"verified", "generated", "1", "42", target, true, false, true},
		{"manual", "manual", "", "", "", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			id := ""
			if !tc.missing {
				id = evidenceWorkflow(t, f, map[string]string{"kind": "onboarding", "project_id": tc.project, "base_sha": tc.base, "workspace_material_version": tc.marker}, tc.completed)
			}
			projectID := int64(42)
			if tc.name == "wrong-project" {
				projectID = 43
			}
			f.service.store = contextEvidenceStore{ReviewStore: f.db, doc: store.ContextDoc{ProjectID: projectID, BaseSHA: target, Kind: "agents", Source: tc.source, Content: "CONTEXT_CANARY", WorkflowID: id, Active: true, Locked: true}}
			content, base := f.service.contextDoc(context.Background(), projectID, target)
			if (content == "CONTEXT_CANARY") != tc.want || (base == target) != tc.want {
				t.Fatalf("context=%q base=%q", content, base)
			}
		})
	}
}

func TestLegacyGeneratedContextNotLaunderedIntoMR(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	target := strings.Repeat("2", 40)
	if _, err := f.db.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", BaseSHA: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SaveContextDoc(ctx, store.ContextDoc{ProjectID: 42, BaseSHA: target, Kind: "agents", Content: "LEGACY_CANARY"}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.LockContextDoc(ctx, 42, target, "agents", true, nil); err != nil {
		t.Fatal(err)
	}
	f.service.reviewDefaults.Ignore, f.publisher.reviewDefaults.Ignore = nil, nil
	job := f.seedReview(t, "legacy-generated-context")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := f.db.GetWorkflowDossier(ctx, wf.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(record.Data), "LEGACY_CANARY") {
		t.Fatal("legacy generated context reached MR")
	}
	doc, err := f.db.GetCurrentContextDoc(ctx, 42, "agents")
	if err != nil || !doc.Locked || doc.Content != "LEGACY_CANARY" {
		t.Fatalf("legacy context changed: %+v, %v", doc, err)
	}
}

func TestSafeGeneratedContextReachesMR(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	svc := newOnboardingContextService(t, f, onboardingIndexRunner)
	svc.reviewDefaults.Ignore = nil
	if err := svc.Onboard(ctx, 42); err != nil {
		t.Fatal(err)
	}
	doc, err := f.db.GetCurrentContextDoc(ctx, 42, "agents")
	if err != nil {
		t.Fatal(err)
	}
	f.gitlab.version.StartSHA = reviewTestHead
	f.service.reviewDefaults.Ignore, f.publisher.reviewDefaults.Ignore = nil, nil
	job := f.seedReview(t, "safe-generated-context")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := f.db.GetWorkflowDossier(ctx, wf.ID)
	if err != nil {
		t.Fatal(err)
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil || dossier.ContextDocs != doc.Content {
		t.Fatalf("safe context not passed: %v", err)
	}
}
