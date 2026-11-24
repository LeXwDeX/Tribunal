package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

// seededProjectToken is the token seedProject seals into project 42.
const seededProjectToken = "glpat-seeded-token"

// enqueueTestGeneration creates the review control a status goal is fenced
// against and returns its generation.
func enqueueTestGeneration(t *testing.T, f *reviewFixture, ctx context.Context) int64 {
	t.Helper()
	result, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
		Payload: json.RawMessage(`{"trigger":"mr_open"}`),
	})
	if err != nil {
		t.Fatalf("enqueue review: %v", err)
	}
	return result.Generation
}

// statusNoteRejected401 makes every status note write fail the way GitLab does
// for a revoked credential.
func statusNoteRejected401() error {
	return &gitlab.Error{Op: "create_note", Status: 401}
}

func TestSideEffectCredentialRejectionIsRecorded(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	registry := NewCredentialHealthRegistry(time.Now)
	f.service.credentials = registry
	var logs bytes.Buffer
	f.service.logger = slog.New(slog.NewTextHandler(&logs, nil))

	generation := enqueueTestGeneration(t, f, ctx)
	if ok, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: generation, HeadSHA: f.head,
		State: reviewStatusFailed, Detail: "review failed",
	}); err != nil || !ok {
		t.Fatalf("set status: %v %v", ok, err)
	}
	f.gitlab.statusNoteErr = statusNoteRejected401()
	drainReviewJobs(t, f, "review.status")

	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if row.AppliedVersion != row.Version {
		t.Fatalf("a rejected credential must still settle the row: %+v", row)
	}
	if !strings.HasPrefix(row.LastError, store.ReviewStatusCredentialError) {
		t.Fatalf("last_error = %q, want the %q marker", row.LastError, store.ReviewStatusCredentialError)
	}
	if !strings.Contains(logs.String(), "credential rejected during review side effect") {
		t.Fatalf("rejection was not logged as a warning: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "error_class=") {
		t.Fatalf("warning has no error class: %s", logs.String())
	}

	snapshot := registry.Snapshot()
	if snapshot.Shared.State != CredentialStateUnknown || snapshot.Shared.Failures != 0 {
		t.Fatalf("shared record = %+v, want untouched", snapshot.Shared)
	}
	if len(snapshot.Projects) != 1 {
		t.Fatalf("project records = %+v", snapshot.Projects)
	}
	record := snapshot.Projects[0]
	if record.ProjectID != 42 || record.Source != CredentialSourceProject ||
		record.State != CredentialStateRejected || record.Failures != 1 {
		t.Fatalf("project record = %+v", record)
	}
	if !strings.Contains(record.LastError, "replace the project token") {
		t.Fatalf("last error = %q, want the project token hint", record.LastError)
	}
	if record.LastFailureAt.IsZero() || record.CheckedAt.IsZero() {
		t.Fatalf("record has no timestamps: %+v", record)
	}
}

func TestReopenedCredentialBlockedStatusIsWrittenAfterRecovery(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	f.service.credentials = NewCredentialHealthRegistry(time.Now)

	generation := enqueueTestGeneration(t, f, ctx)
	if ok, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: generation, HeadSHA: f.head,
		State: reviewStatusFailed, Detail: "review failed",
	}); err != nil || !ok {
		t.Fatalf("set status: %v %v", ok, err)
	}
	f.gitlab.statusNoteErr = statusNoteRejected401()
	drainReviewJobs(t, f, "review.status")

	blocked, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.AppliedVersion < blocked.Version || blocked.LastError == "" {
		t.Fatalf("blocked row = %+v", blocked)
	}

	// The credential is replaced and the periodic pass verifies it again.
	reopened, err := f.db.ReopenCredentialBlockedReviewStatuses(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if reopened != 1 {
		t.Fatalf("reopened = %d, want 1", reopened)
	}
	pending, err := f.db.ListReviewStatusesForVerification(ctx, 42, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("reopened row is not queued for verification: %+v", pending)
	}

	f.gitlab.statusNoteErr = nil
	raw, err := json.Marshal(map[string]any{"project_id": 42, "mr_iid": 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.db.Enqueue(ctx, store.Job{
		Kind: "review.status", Key: "review.status:42:7", Payload: raw,
		MaxAttempts: sqlite.ReviewSideEffectMaxAttempts,
	}); err != nil {
		t.Fatal(err)
	}
	drainReviewJobs(t, f, "review.status")

	recovered, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.AppliedVersion != recovered.Version || recovered.LastError != "" {
		t.Fatalf("recovered row = %+v", recovered)
	}
	if len(f.gitlab.notes) == 0 {
		t.Fatal("no status note was written after the credential recovered")
	}
}

func newCredentialProjectService(t *testing.T, api gitlab.API, registry *CredentialHealthRegistry, now func() time.Time) (*ProjectService, *sqlite.Store) {
	t.Helper()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	seedProject(t, st, cipher, 42, "whsec", "{}", "default")
	service, err := NewProjectService(ProjectServiceConfig{
		Store:             st,
		GitLab:            api,
		Cipher:            cipher,
		Profiles:          testProfiles(),
		Workspace:         workspace.NewManager(t.TempDir(), nil),
		GitLabURL:         "https://gitlab.example.com",
		ValidationTimeout: time.Second,
		Credentials:       registry,
		Logger:            slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		Now:               now,
	})
	if err != nil {
		t.Fatalf("NewProjectService: %v", err)
	}
	return service, st
}

func TestCredentialHealthCheckRecordsProjectToken(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	expiry := now.Add(48 * time.Hour)
	registry := NewCredentialHealthRegistry(func() time.Time { return now })
	api := newSharedBotAPI()
	api.infoByToken[seededProjectToken] = &gitlab.TokenInfo{
		Scopes: []string{"api", "read_repository"}, Active: true, ExpiresAt: &expiry,
	}
	service, _ := newCredentialProjectService(t, api, registry, func() time.Time { return now })

	service.ReconcileCredentialHealth(context.Background())
	snapshot := service.CredentialHealth()
	if len(snapshot.Projects) != 1 {
		t.Fatalf("records = %+v", snapshot.Projects)
	}
	record := snapshot.Projects[0]
	if record.State != CredentialStateOK || record.Source != CredentialSourceProject || record.Failures != 0 {
		t.Fatalf("record = %+v", record)
	}
	if !record.ExpiringSoon || record.TokenExpiresAt == nil {
		t.Fatalf("expiry was not reported: %+v", record)
	}

	// A revoked token is recorded as rejected on the next pass.
	api.infoByToken[seededProjectToken] = nil
	api.infoErrs[seededProjectToken] = &gitlab.Error{Op: "token_info", Status: 401}
	service.ReconcileCredentialHealth(context.Background())
	record = service.CredentialHealth().Projects[0]
	if record.State != CredentialStateRejected || record.Failures != 1 {
		t.Fatalf("rejected record = %+v", record)
	}
	if !strings.Contains(record.LastError, "replace the project token") {
		t.Fatalf("last error = %q", record.LastError)
	}
}

func TestCredentialHealthReopensBlockedRowsOnRecovery(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	registry := NewCredentialHealthRegistry(func() time.Time { return now })
	api := newSharedBotAPI()
	api.infoByToken[seededProjectToken] = &gitlab.TokenInfo{
		Scopes: []string{"api", "read_repository"}, Active: true,
	}
	service, st := newCredentialProjectService(t, api, registry, func() time.Time { return now })

	queued, err := st.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewTestHead,
		Payload: json.RawMessage(`{"trigger":"mr_open"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: queued.Generation, HeadSHA: reviewTestHead,
		State: reviewStatusFailed, Detail: "review failed",
	}); err != nil || !ok {
		t.Fatalf("set status: %v %v", ok, err)
	}
	row, err := st.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	blocked := store.ReviewStatusCredentialError + ": replaced token"
	if err := st.MarkReviewStatusApplied(ctx, 42, 7, int64(row.Version), 0, "", blocked); err != nil {
		t.Fatal(err)
	}

	service.ReconcileCredentialHealth(ctx)
	after, err := st.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if after.AppliedVersion != 0 {
		t.Fatalf("credential-blocked row was not reopened: %+v", after)
	}
	if after.LastError != blocked {
		t.Fatalf("reopen dropped the marker: %q", after.LastError)
	}
}
