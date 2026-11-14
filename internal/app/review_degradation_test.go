package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func readReviewDossier(t *testing.T, f *reviewFixture, jobID string) *review.Dossier {
	t.Helper()
	wf, err := f.db.GetWorkflowByJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	record, err := f.db.GetWorkflowDossier(context.Background(), wf.ID)
	if err != nil {
		t.Fatalf("GetWorkflowDossier: %v", err)
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		t.Fatalf("decode dossier: %v", err)
	}
	return &dossier
}

func TestDefaultIgnoreStillUsesContextAndCodeGraph(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.Ignore = config.DefaultReviewIgnorePatterns()
	f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
	svc := newOnboardingContextService(t, f, onboardingIndexRunner)
	svc.reviewDefaults.Ignore = config.DefaultReviewIgnorePatterns()
	if err := svc.Onboard(ctx, 42); err != nil {
		t.Fatalf("Onboard: %v", err)
	}
	contextBase := reviewTestHead
	f.gitlab.version.StartSHA = strings.Repeat("2", 40)
	if _, err := f.db.SaveWorkspace(ctx, store.Workspace{
		ProjectID: 42, State: "ready", BaseSHA: f.gitlab.version.StartSHA,
		IndexSHA: f.gitlab.version.StartSHA, ContextSHA: contextBase,
	}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	provider := &ignoreCountingTools{}
	f.service.tools = provider
	f.service.blocks = map[string]config.BlockBinding{review.BlockCodeReview: {MCPTools: []string{"mcp__graph__search"}}}

	job := f.seedReview(t, "default-ignore-context")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	dossier := readReviewDossier(t, f, job.ID)
	if dossier.ContextDocs == "" || dossier.WorkspaceProvenance == nil || !dossier.WorkspaceProvenance.MCPEligible {
		t.Fatalf("context=%d provenance=%+v", len(dossier.ContextDocs), dossier.WorkspaceProvenance)
	}
	if dossier.WorkspaceProvenance.ContextBaseSHA != contextBase {
		t.Fatalf("context base = %q, want %q", dossier.WorkspaceProvenance.ContextBaseSHA, contextBase)
	}
	if provider.calls == 0 {
		t.Fatal("default ignore patterns withheld the code graph")
	}
}

func TestCustomIgnoreDegradationIsVisible(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.Ignore = append(config.DefaultReviewIgnorePatterns(), "private/**")
	f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
	provider := &ignoreCountingTools{}
	f.service.tools = provider
	f.service.blocks = map[string]config.BlockBinding{review.BlockCodeReview: {MCPTools: []string{"mcp__graph__search"}}}

	job := f.seedReview(t, "custom-ignore-visible")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	dossier := readReviewDossier(t, f, job.ID)
	if dossier.WorkspaceProvenance == nil || dossier.WorkspaceProvenance.MCPEligible {
		t.Fatalf("provenance = %+v, want MCP ineligible", dossier.WorkspaceProvenance)
	}
	if !dossierWarningContains(dossier, "without the code graph") {
		t.Fatalf("dossier warnings = %v", dossier.Warnings)
	}
	if provider.calls != 0 {
		t.Fatal("custom ignore must still withhold the code graph")
	}
	found := false
	for _, body := range f.gitlab.noteBodies() {
		if strings.Contains(body, "未使用代码图谱") {
			found = true
		}
	}
	if !found {
		t.Fatalf("published notes do not disclose the missing code graph: %v", f.gitlab.noteBodies())
	}
}

func TestUnverifiedContextDocumentIsDisclosed(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	// A generated document without its onboarding workflow cannot be verified.
	if _, err := f.db.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: f.gitlab.version.StartSHA, Kind: "agents",
		Source: "generated", Content: "UNVERIFIED_CANARY",
	}); err != nil {
		t.Fatalf("SaveContextDoc: %v", err)
	}
	job := f.seedReview(t, "unverified-context")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	dossier := readReviewDossier(t, f, job.ID)
	if dossier.ContextDocs != "" {
		t.Fatal("unverified context reached the dossier")
	}
	if !dossierWarningContains(dossier, "without the project context document") {
		t.Fatalf("dossier warnings = %v", dossier.Warnings)
	}
}

func dossierWarningContains(dossier *review.Dossier, want string) bool {
	for _, warning := range dossier.Warnings {
		if strings.Contains(warning, want) {
			return true
		}
	}
	return false
}
func TestReviewTokenRejectionIsVisible(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.mu.Lock()
	f.gitlab.mrErr = &gitlab.Error{Op: "get_merge_request", Status: 401}
	f.gitlab.mu.Unlock()
	job := f.seedReview(t, "token-rejected")
	err := f.service.RunReview(ctx, job)
	if err == nil || !IsPermanentJob(err) {
		t.Fatalf("RunReview err = %v, want a permanent auth failure", err)
	}
	status, statusErr := f.db.GetReviewStatus(ctx, 42, 7)
	if statusErr != nil || status.State != store.ReviewStatusFailed || !strings.Contains(status.Detail, "令牌") {
		t.Fatalf("status = %+v, err = %v", status, statusErr)
	}
	project, projectErr := f.db.GetProject(ctx, 42)
	if projectErr != nil || project.WebhookState != store.WebhookStateFailed || !strings.Contains(project.WebhookError, "token") {
		t.Fatalf("project = %+v, err = %v", project, projectErr)
	}

	// A later successful GitLab call restores the project health.
	f.gitlab.mu.Lock()
	f.gitlab.mrErr = nil
	f.gitlab.mu.Unlock()
	job = f.seedReview(t, "token-recovered")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("RunReview after recovery: %v", err)
	}
	project, projectErr = f.db.GetProject(ctx, 42)
	if projectErr != nil || project.WebhookState != store.WebhookStateEnabled || project.WebhookError != "" {
		t.Fatalf("recovered project = %+v, err = %v", project, projectErr)
	}
}

func TestReviewTokenRejectionStopsCandidateRetries(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.mu.Lock()
	f.gitlab.mrErr = &gitlab.Error{Op: "get_merge_request", Status: 403}
	f.gitlab.mu.Unlock()
	webhook, err := NewWebhookService(WebhookServiceConfig{
		Store: f.db, Workspace: f.manager, Cipher: newTestCipher(t),
		Profiles: f.profiles, ReviewDefaults: f.defaults,
	})
	if err != nil {
		t.Fatalf("NewWebhookService: %v", err)
	}
	res, err := webhook.Handle(ctx, WebhookRequest{
		Event: "Merge Request Hook", Token: "whsec", UUID: uuid.NewString(), Body: mrBody(42, 7, nil),
	})
	if err != nil || !res.Accepted {
		t.Fatalf("webhook = %+v, err = %v", res, err)
	}
	if res.JobID == "" {
		t.Fatal("candidate job was not enqueued")
	}
	candidate, err := f.db.Claim(ctx, "test-worker", []string{"review.candidate"}, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := f.service.RunCandidate(ctx, candidate); err == nil || !IsPermanentJob(err) {
		t.Fatalf("candidate err = %v, want a permanent auth failure", err)
	}
	project, _ := f.db.GetProject(ctx, 42)
	if project == nil || project.WebhookState != store.WebhookStateFailed {
		t.Fatalf("project = %+v", project)
	}
}
