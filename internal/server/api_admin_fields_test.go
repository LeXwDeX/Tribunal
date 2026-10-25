package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

// adminFieldsStore adds controlled failures on top of the review fake. The
// dossier read now comes from the required Store contract.
type adminFieldsStore struct {
	*fakeReviewStore
	workspace    *store.Workspace
	workspaceErr error
	workflowsErr error
}

func (s *adminFieldsStore) GetWorkspace(_ context.Context, projectID int64) (*store.Workspace, error) {
	if s.workspaceErr != nil {
		return nil, s.workspaceErr
	}
	if s.workspace == nil || s.workspace.ProjectID != projectID {
		return nil, store.ErrNotFound
	}
	return s.workspace, nil
}

func (s *adminFieldsStore) ListWorkflows(ctx context.Context, filter store.WorkflowFilter, cursor string, limit int) ([]store.Workflow, string, error) {
	if s.workflowsErr != nil {
		return nil, "", s.workflowsErr
	}
	return s.fakeReviewStore.ListWorkflows(ctx, filter, cursor, limit)
}

// adminFieldsAdmin is a bounded AdminService fixture that returns exact
// project data without requiring GitLab or a registration round trip.
type adminFieldsAdmin struct {
	projects []store.Project
	project  *store.Project
	next     string
	listErr  error
	getErr   error
}

func (a *adminFieldsAdmin) Register(context.Context, app.RegisterInput) (*app.Registration, error) {
	return nil, errors.New("not used")
}

func (a *adminFieldsAdmin) Validate(context.Context, app.ValidateProjectInput) (*app.ProjectValidation, error) {
	return nil, errors.New("not used")
}

func (a *adminFieldsAdmin) List(context.Context) ([]store.Project, error) {
	return append([]store.Project(nil), a.projects...), nil
}

func (a *adminFieldsAdmin) ListFiltered(context.Context, app.ProjectListFilter) ([]store.Project, error) {
	return append([]store.Project(nil), a.projects...), nil
}

func (a *adminFieldsAdmin) ListFilteredPage(context.Context, app.ProjectListFilter, string, int) ([]store.Project, string, error) {
	if a.listErr != nil {
		return nil, "", a.listErr
	}
	return append([]store.Project(nil), a.projects...), a.next, nil
}

func (a *adminFieldsAdmin) Get(context.Context, int64) (*store.Project, error) {
	if a.getErr != nil {
		return nil, a.getErr
	}
	return a.project, nil
}

func (a *adminFieldsAdmin) Update(context.Context, int64, app.UpdateProjectInput) (*store.Project, error) {
	return nil, errors.New("not used")
}

func (a *adminFieldsAdmin) RotateToken(context.Context, int64, string) (*store.Project, error) {
	return nil, errors.New("not used")
}

func (a *adminFieldsAdmin) CreateWebhook(context.Context, int64) (*app.WebhookRegistration, error) {
	return nil, errors.New("not used")
}

func (a *adminFieldsAdmin) RebuildWebhook(context.Context, int64) (*app.WebhookRegistration, error) {
	return nil, errors.New("not used")
}

func (a *adminFieldsAdmin) DisableWebhook(context.Context, int64) (*store.Project, error) {
	return nil, errors.New("not used")
}

func (a *adminFieldsAdmin) Delete(context.Context, int64) error {
	return errors.New("not used")
}

func reviewWorkflow(id string, projectID int64, createdAt time.Time) store.Workflow {
	mrIID := int64(7)
	return store.Workflow{
		ID:        id,
		Kind:      "review",
		ProjectID: projectID,
		MRIID:     &mrIID,
		HeadSHA:   strings.Repeat("a", 40),
		Trigger:   "mr_update",
		Status:    "completed",
		CreatedAt: createdAt,
	}
}

func seedAdminReview(t *testing.T, fake *fakeReviewStore, workflow store.Workflow) {
	t.Helper()
	fake.setWorkflow(workflow)
	fake.setNodes(workflow.ID)
	fake.setFindings(workflow.ID)
}

func TestReviewDetailReadsStoredMRTitleAndCommitStatus(t *testing.T) {
	fake := newFakeReviewStore()
	wf := reviewWorkflow("wf-stored", 42, time.Now().UTC())
	seedAdminReview(t, fake, wf)
	fake.setPublications(wf.ID,
		store.Publication{
			ID: "pub-summary", Kind: "summary", PublishKey: "summary",
			GitlabID: "99102", WorkflowID: wf.ID,
		},
		store.Publication{
			ID: "pub-status", Kind: "commit_status",
			PublishKey: "status:" + wf.HeadSHA + ":failed",
			GitlabID:   "ai-review", WorkflowID: wf.ID,
			UpdatedAt: time.Now().UTC(),
		},
	)
	fake.dossiers[wf.ID] = &store.WorkflowDossier{
		WorkflowID: wf.ID,
		Data: json.RawMessage(`{
			"mr":{"title":"Stored MR title"},
			"project":{"path":"group/repo"},
			"metadata":{"description":"glpat-should-not-leak"}
		}`),
	}
	env := newM6Env(t, m6Options{store: &adminFieldsStore{fakeReviewStore: fake}})
	client := env.businessClient()

	response := client.request(http.MethodGet, "/api/v1/reviews/"+wf.ID, nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)
	if body["mr_title"] != "Stored MR title" {
		t.Fatalf("mr_title = %v, want stored title", body["mr_title"])
	}
	publications := body["publications"].(map[string]any)
	if publications["commit_status"] != "failed" {
		t.Fatalf("commit_status = %v", publications["commit_status"])
	}
	if strings.Contains(string(response.body), "glpat-should-not-leak") {
		t.Fatalf("dossier content leaked into review detail: %s", response.body)
	}
}

func TestReviewDetailOmitsMissingMRTitleAndCommitStatus(t *testing.T) {
	fake := newFakeReviewStore()
	wf := reviewWorkflow("wf-missing", 42, time.Now().UTC())
	seedAdminReview(t, fake, wf)
	// A legacy status key has no recorded state and must not be guessed from
	// the workflow verdict.
	fake.setPublications(wf.ID,
		store.Publication{ID: "pub-discussion", Kind: "discussion", PublishKey: "finding", GitlabID: "555", WorkflowID: wf.ID},
		store.Publication{ID: "pub-legacy-status", Kind: "commit_status", PublishKey: "status:" + wf.HeadSHA, GitlabID: "ai-review", WorkflowID: wf.ID},
	)
	env := newM6Env(t, m6Options{store: &adminFieldsStore{fakeReviewStore: fake}})
	client := env.businessClient()

	response := client.request(http.MethodGet, "/api/v1/reviews/"+wf.ID, nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)
	if _, present := body["mr_title"]; present {
		t.Fatalf("mr_title present without a dossier: %v", body["mr_title"])
	}
	publications := body["publications"].(map[string]any)
	if _, present := publications["commit_status"]; present {
		t.Fatalf("commit_status guessed from legacy publication: %v", publications["commit_status"])
	}
	if publications["discussions"] != float64(1) {
		t.Fatalf("discussions = %v", publications["discussions"])
	}
}

func TestReviewDetailPropagatesDossierBackendFailure(t *testing.T) {
	fake := newFakeReviewStore()
	wf := reviewWorkflow("wf-failure", 42, time.Now().UTC())
	seedAdminReview(t, fake, wf)
	fake.dossierErr = errors.New("database unavailable: glpat-secret")
	env := newM6Env(t, m6Options{store: &adminFieldsStore{fakeReviewStore: fake}})
	client := env.businessClient()

	response := client.request(http.MethodGet, "/api/v1/reviews/"+wf.ID, nil, nil)
	assertStatus(t, response, http.StatusInternalServerError)
	if strings.Contains(string(response.body), "database unavailable") || strings.Contains(string(response.body), "glpat") {
		t.Fatalf("backend error leaked: %s", response.body)
	}
}

func TestSummarizePublicationsUsesNewestRecordedCommitStatus(t *testing.T) {
	older := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	newer := older.Add(time.Minute)
	summary := summarizePublications([]store.Publication{
		{ID: "new-success", Kind: "commit_status", PublishKey: "status:" + strings.Repeat("a", 40) + ":success", UpdatedAt: newer},
		{ID: "old-failed", Kind: "commit_status", PublishKey: "status:" + strings.Repeat("a", 40) + ":failed", UpdatedAt: older},
		{ID: "legacy", Kind: "commit_status", PublishKey: "status:" + strings.Repeat("a", 40), UpdatedAt: newer.Add(time.Minute)},
	})
	if summary.CommitStatus == nil || *summary.CommitStatus != "success" {
		t.Fatalf("commit_status = %v, want newest recorded success", summary.CommitStatus)
	}
}

// Newest means the greatest UpdatedAt, then the greatest ID (UUIDv7). The
// result must not depend on the input slice order.
func TestSummarizePublicationsUsesNewestUpdatedAtThenID(t *testing.T) {
	at := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	older := store.Publication{
		ID: "01900000-0000-7000-8000-000000000001", Kind: "commit_status",
		PublishKey: "status:" + strings.Repeat("a", 40) + ":success", UpdatedAt: at,
	}
	newer := store.Publication{
		ID: "01900000-0000-7000-8000-000000000002", Kind: "commit_status",
		PublishKey: "status:" + strings.Repeat("a", 40) + ":failed", UpdatedAt: at,
	}
	for _, order := range [][]store.Publication{{newer, older}, {older, newer}} {
		summary := summarizePublications(order)
		if summary.CommitStatus == nil || *summary.CommitStatus != "failed" {
			t.Fatalf("commit_status = %v, want highest-ID failed for order %v",
				summary.CommitStatus, []string{order[0].ID, order[1].ID})
		}
	}
}

// The store orders equal timestamps by ID descending, and the summary must
// agree with that order instead of flipping to the last slice entry.
func TestSummarizePublicationsSelectsNewestFromSQLite(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "status.db"), sqlite.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, publication := range []store.Publication{
		{
			ID: "01900000-0000-7000-8000-000000000001", ProjectID: 42, MRIID: 7,
			Kind: "commit_status", PublishKey: "status:" + strings.Repeat("a", 40) + ":success",
			GitlabID: "ai-review", WorkflowID: "wf-status", Fingerprint: "fp",
		},
		{
			ID: "01900000-0000-7000-8000-000000000002", ProjectID: 42, MRIID: 7,
			Kind: "commit_status", PublishKey: "status:" + strings.Repeat("a", 40) + ":failed",
			GitlabID: "ai-review", WorkflowID: "wf-status", Fingerprint: "fp",
		},
	} {
		if _, err := st.UpsertPublication(ctx, publication); err != nil {
			t.Fatalf("UpsertPublication: %v", err)
		}
	}
	publications, err := st.ListPublications(ctx, "wf-status")
	if err != nil {
		t.Fatalf("ListPublications: %v", err)
	}
	if len(publications) != 2 || publications[0].ID != "01900000-0000-7000-8000-000000000002" {
		t.Fatalf("ListPublications order = %+v", publications)
	}
	summary := summarizePublications(publications)
	if summary.CommitStatus == nil || *summary.CommitStatus != "failed" {
		t.Fatalf("commit_status = %v, want newest failed", summary.CommitStatus)
	}
}

func TestCommitStatusStateRequiresValidSHA(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	sha256 := strings.Repeat("b", 64)
	cases := []struct {
		key   string
		state string
		ok    bool
	}{
		{key: "status:" + sha1 + ":success", state: "success", ok: true},
		{key: "status:" + sha256 + ":failed", state: "failed", ok: true},
		{key: "status:" + sha1, ok: false},
		{key: "status:" + strings.Repeat("a", 39) + ":failed", ok: false},
		{key: "status:" + strings.Repeat("a", 41) + ":failed", ok: false},
		{key: "status:" + strings.Repeat("z", 40) + ":failed", ok: false},
		{key: "status:" + sha1 + ":pending", ok: false},
		{key: "status:" + sha1 + ":", ok: false},
		{key: "commit:" + sha1 + ":failed", ok: false},
	}
	for _, tc := range cases {
		state, ok := commitStatusState(tc.key)
		if ok != tc.ok || state != tc.state {
			t.Fatalf("commitStatusState(%q) = %q, %v; want %q, %v", tc.key, state, ok, tc.state, tc.ok)
		}
	}
}

func TestWorkspaceIndexStatusUnknownWithoutIndexCheckpoint(t *testing.T) {
	workspace := &store.Workspace{State: "ready", BaseSHA: strings.Repeat("b", 40)}
	if got := workspaceIndexStatus(workspace); got != "unknown" {
		t.Fatalf("index_status without index checkpoint = %q, want unknown", got)
	}
}

func TestWorkspaceResponseReportsBackendFailure(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: &adminFieldsStore{
		fakeReviewStore: fake,
		workspaceErr:    errors.New("workspace backend failed"),
	}})
	response := env.businessClient().request(http.MethodGet, "/api/v1/projects/42/workspace", nil, nil)
	assertStatus(t, response, http.StatusInternalServerError)
}

func TestWorkspaceIndexStatusSemantics(t *testing.T) {
	base := strings.Repeat("b", 40)
	stale := strings.Repeat("c", 40)
	cases := []struct {
		name      string
		workspace store.Workspace
		mcpPath   string
		want      string
	}{
		{
			name:      "indexed current base",
			workspace: store.Workspace{ProjectID: 42, State: "ready", BaseSHA: base, IndexSHA: base},
			mcpPath:   "config/mcp.json",
			want:      "ready",
		},
		{
			name:      "indexed older base",
			workspace: store.Workspace{ProjectID: 42, State: "ready", BaseSHA: base, IndexSHA: stale},
			mcpPath:   "config/mcp.json",
			want:      "stale",
		},
		{
			name:      "index outcome not recorded",
			workspace: store.Workspace{ProjectID: 42, State: "ready", BaseSHA: base},
			mcpPath:   "config/mcp.json",
			want:      "unknown",
		},
		{
			name:      "no recorded index without platform MCP config",
			workspace: store.Workspace{ProjectID: 42, State: "ready", BaseSHA: base},
			want:      "unknown",
		},
		{
			name:      "project index does not require platform MCP config",
			workspace: store.Workspace{ProjectID: 42, State: "ready", BaseSHA: base, IndexSHA: base},
			want:      "ready",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newM6Env(t, m6Options{platform: &config.Config{MCP: config.MCPConfig{Config: tc.mcpPath}}})
			prepareProject(t, env, 42, tc.workspace.BaseSHA)
			if _, err := env.sqlite.SaveWorkspace(context.Background(), tc.workspace); err != nil {
				t.Fatalf("SaveWorkspace: %v", err)
			}
			response := env.businessClient().request(http.MethodGet, "/api/v1/projects/42/workspace", nil, nil)
			assertStatus(t, response, http.StatusOK)
			body := env.decode(t, response)
			if body["index_status"] != tc.want {
				t.Fatalf("index_status = %v, want %s", body["index_status"], tc.want)
			}
			if body["base_sha"] != tc.workspace.BaseSHA || body["state"] != "ready" {
				t.Fatalf("workspace fields changed: %v", body)
			}
		})
	}
}

func TestProjectDetailIncludesBoundedWorkspaceAndRecentReviews(t *testing.T) {
	fake := newFakeReviewStore()
	base := strings.Repeat("d", 40)
	fakeWorkspace := &store.Workspace{
		ProjectID: 42, State: "ready", BaseSHA: base, IndexSHA: base,
	}
	start := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		fake.setWorkflow(reviewWorkflow("wf-"+strings.Repeat("x", i), 42, start.Add(time.Duration(i)*time.Minute)))
	}
	admin := &adminFieldsAdmin{project: &store.Project{
		ID: 42, PathWithNamespace: "group/repo", WebURL: "https://gitlab.example.com/group/repo",
		DefaultBranch: "main", Profile: "default", Settings: json.RawMessage(`{}`), Enabled: true,
	}}
	env := newM6Env(t, m6Options{
		store:    &adminFieldsStore{fakeReviewStore: fake, workspace: fakeWorkspace},
		admin:    admin,
		platform: &config.Config{MCP: config.MCPConfig{Config: "config/mcp.json"}},
	})
	response := env.businessClient().request(http.MethodGet, "/api/v1/projects/42", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)
	if body["path_with_namespace"] != "group/repo" || body["id"] != float64(42) {
		t.Fatalf("flat project fields changed: %v", body)
	}
	workspace := body["workspace"].(map[string]any)
	if workspace["state"] != "ready" || workspace["index_status"] != "ready" {
		t.Fatalf("workspace = %v", workspace)
	}
	reviews := body["recent_reviews"].([]any)
	if len(reviews) != 20 {
		t.Fatalf("recent_reviews = %d, want bounded 20", len(reviews))
	}
	if reviews[0].(map[string]any)["id"] != "wf-"+strings.Repeat("x", 24) {
		t.Fatalf("recent_reviews not newest first: %v", reviews[0])
	}
}

func TestProjectDetailOmitsMissingWorkspaceAndReportsBackendFailure(t *testing.T) {
	fake := newFakeReviewStore()
	admin := &adminFieldsAdmin{project: &store.Project{ID: 42, PathWithNamespace: "group/repo"}}
	env := newM6Env(t, m6Options{store: &adminFieldsStore{fakeReviewStore: fake}, admin: admin})
	response := env.businessClient().request(http.MethodGet, "/api/v1/projects/42", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)
	if _, present := body["workspace"]; present {
		t.Fatalf("workspace must be omitted when no row exists: %v", body["workspace"])
	}
	if reviews, ok := body["recent_reviews"].([]any); !ok || len(reviews) != 0 {
		t.Fatalf("recent_reviews = %v, want an empty list", body["recent_reviews"])
	}

	failing := newM6Env(t, m6Options{
		store: &adminFieldsStore{fakeReviewStore: fake, workspaceErr: errors.New("workspace backend failed")},
		admin: admin,
	})
	failed := failing.businessClient().request(http.MethodGet, "/api/v1/projects/42", nil, nil)
	assertStatus(t, failed, http.StatusInternalServerError)

	reviewsFailing := newM6Env(t, m6Options{
		store: &adminFieldsStore{fakeReviewStore: fake, workflowsErr: errors.New("reviews backend failed")},
		admin: admin,
	})
	reviewsFailed := reviewsFailing.businessClient().request(http.MethodGet, "/api/v1/projects/42", nil, nil)
	assertStatus(t, reviewsFailed, http.StatusInternalServerError)
}

func TestProjectAdminFieldsDoNotExposeSecrets(t *testing.T) {
	fake := newFakeReviewStore()
	admin := &adminFieldsAdmin{
		project: &store.Project{
			ID: 42, PathWithNamespace: "group/repo", Profile: "default",
			Settings:         json.RawMessage(`{}`),
			TokenEnc:         []byte("glpat-project-secret"),
			WebhookSecretEnc: []byte("whsec-project-secret"),
		},
		projects: []store.Project{{
			ID: 42, PathWithNamespace: "group/repo", Profile: "default",
			Settings:         json.RawMessage(`{}`),
			TokenEnc:         []byte("glpat-project-secret"),
			WebhookSecretEnc: []byte("whsec-project-secret"),
		}},
	}
	env := newM6Env(t, m6Options{store: &adminFieldsStore{fakeReviewStore: fake}, admin: admin})
	client := env.businessClient()

	for _, path := range []string{"/api/v1/projects/42", "/api/v1/projects"} {
		response := client.request(http.MethodGet, path, nil, nil)
		assertStatus(t, response, http.StatusOK)
		body := string(response.body)
		if strings.Contains(body, "glpat-project-secret") || strings.Contains(body, "whsec-project-secret") {
			t.Fatalf("%s exposed project secrets: %s", path, body)
		}
		if strings.Contains(body, "token_enc") || strings.Contains(body, "webhook_secret_enc") {
			t.Fatalf("%s exposed encrypted project fields: %s", path, body)
		}
	}
}

func TestProjectListAddsWorkspaceAndLatestReviewWithoutChangingFlatFields(t *testing.T) {
	fake := newFakeReviewStore()
	base := strings.Repeat("e", 40)
	older := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	fake.setWorkflow(reviewWorkflow("wf-42-older", 42, older))
	fake.setWorkflow(reviewWorkflow("wf-42-latest", 42, older.Add(time.Hour)))
	fake.setWorkflow(reviewWorkflow("wf-43-latest", 43, older))
	admin := &adminFieldsAdmin{projects: []store.Project{
		{ID: 42, PathWithNamespace: "group/repo", Profile: "default", Settings: json.RawMessage(`{}`), Enabled: true},
		{ID: 43, PathWithNamespace: "group/other", Profile: "default", Settings: json.RawMessage(`{}`), Enabled: true},
	}}
	env := newM6Env(t, m6Options{
		store:    &adminFieldsStore{fakeReviewStore: fake, workspace: &store.Workspace{ProjectID: 42, State: "ready", BaseSHA: base}},
		admin:    admin,
		platform: &config.Config{},
	})
	response := env.businessClient().request(http.MethodGet, "/api/v1/projects?limit=2", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)
	projects := body["projects"].([]any)
	if len(projects) != 2 {
		t.Fatalf("projects = %v", projects)
	}
	first := projects[0].(map[string]any)
	if first["id"] != float64(42) || first["path_with_namespace"] != "group/repo" {
		t.Fatalf("flat project fields changed: %v", first)
	}
	if _, wrapped := first["project"]; wrapped {
		t.Fatalf("project response gained a wrapper: %v", first)
	}
	workspace := first["workspace"].(map[string]any)
	if workspace["state"] != "ready" || workspace["index_status"] != "unknown" {
		t.Fatalf("workspace = %v", workspace)
	}
	lastReview := first["last_review"].(map[string]any)
	if lastReview["id"] != "wf-42-latest" {
		t.Fatalf("last_review = %v", lastReview)
	}
	second := projects[1].(map[string]any)
	if _, present := second["workspace"]; present {
		t.Fatalf("workspace present without a row: %v", second["workspace"])
	}
	if second["last_review"].(map[string]any)["id"] != "wf-43-latest" {
		t.Fatalf("second last_review = %v", second["last_review"])
	}
}

func TestProjectListReportsStoreBackendFailure(t *testing.T) {
	admin := &adminFieldsAdmin{projects: []store.Project{
		{ID: 42, PathWithNamespace: "group/repo", Settings: json.RawMessage(`{}`)},
	}}
	env := newM6Env(t, m6Options{
		store: &adminFieldsStore{
			fakeReviewStore: newFakeReviewStore(),
			workflowsErr:    errors.New("reviews unavailable"),
		},
		admin: admin,
	})
	response := env.businessClient().request(http.MethodGet, "/api/v1/projects", nil, nil)
	assertStatus(t, response, http.StatusInternalServerError)
}
