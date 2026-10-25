package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/app"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
	workspacepkg "github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/workspace"
)

// ---------------------------------------------------------------- fake store

type fakeReviewStore struct {
	mu sync.Mutex

	workflows    []store.Workflow
	nodes        map[string][]store.WorkflowNode
	findings     map[string][]store.Finding
	calls        map[string][]store.LLMCall
	publications map[string][]store.Publication
	dossiers     map[string]*store.WorkflowDossier
	dossierErr   error
	stats        store.Stats

	lastFilter     store.WorkflowFilter
	lastLimit      int
	lastCallFilter store.LLMCallFilter
	lastStats      store.StatsFilter
	accessByUser   map[int64]map[int64]bool
}

func newFakeReviewStore() *fakeReviewStore {
	return &fakeReviewStore{
		nodes:        map[string][]store.WorkflowNode{},
		findings:     map[string][]store.Finding{},
		calls:        map[string][]store.LLMCall{},
		publications: map[string][]store.Publication{},
		dossiers:     map[string]*store.WorkflowDossier{},
	}
}

func (f *fakeReviewStore) ListWorkflows(ctx context.Context, filter store.WorkflowFilter, cursor string, limit int) ([]store.Workflow, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	f.lastLimit = limit
	matched := make([]store.Workflow, 0, len(f.workflows))
	for _, wf := range f.workflows {
		if filter.ProjectID != nil && wf.ProjectID != *filter.ProjectID {
			continue
		}
		if filter.AccessUserID != nil && f.accessByUser != nil {
			if !f.accessByUser[*filter.AccessUserID][wf.ProjectID] {
				continue
			}
		}
		if filter.MRIID != nil && (wf.MRIID == nil || *wf.MRIID != *filter.MRIID) {
			continue
		}
		if filter.Kind != "" && wf.Kind != filter.Kind {
			continue
		}
		if filter.Status != "" && wf.Status != filter.Status {
			continue
		}
		if filter.Verdict != "" && wf.Verdict != filter.Verdict {
			continue
		}
		if filter.PublicationState != "" && wf.PublicationState != filter.PublicationState {
			continue
		}
		if filter.Profile != "" && wf.Profile != filter.Profile {
			continue
		}
		if filter.Trigger != "" && wf.Trigger != filter.Trigger {
			continue
		}
		if !filter.Since.IsZero() && wf.CreatedAt.Before(filter.Since) {
			continue
		}
		matched = append(matched, wf)
	}
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].ID > matched[j].ID
		}
		return matched[i].CreatedAt.After(matched[j].CreatedAt)
	})
	start := 0
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, "", store.ErrInvalidCursor
		}
		lastID := string(raw)
		start = -1
		for i := range matched {
			if matched[i].ID == lastID {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return nil, "", store.ErrInvalidCursor
		}
	}
	if start >= len(matched) {
		return []store.Workflow{}, "", nil
	}
	end := start + limit
	next := ""
	if end < len(matched) {
		next = base64.RawURLEncoding.EncodeToString([]byte(matched[end-1].ID))
	} else {
		end = len(matched)
	}
	return matched[start:end], next, nil
}

func (f *fakeReviewStore) GetWorkflow(ctx context.Context, id string) (*store.Workflow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.workflows {
		if f.workflows[i].ID == id {
			wf := f.workflows[i]
			return &wf, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeReviewStore) setWorkflow(wf store.Workflow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.workflows {
		if f.workflows[i].ID == wf.ID {
			f.workflows[i] = wf
			return
		}
	}
	f.workflows = append(f.workflows, wf)
}

func (f *fakeReviewStore) GetWorkflowDossier(_ context.Context, workflowID string) (*store.WorkflowDossier, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dossierErr != nil {
		return nil, f.dossierErr
	}
	if dossier, ok := f.dossiers[workflowID]; ok {
		return dossier, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeReviewStore) GetWorkflowNodes(ctx context.Context, workflowID string) ([]store.WorkflowNode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.nodes[workflowID]; !ok {
		return nil, store.ErrNotFound
	}
	return append([]store.WorkflowNode(nil), f.nodes[workflowID]...), nil
}

func (f *fakeReviewStore) setNodes(workflowID string, nodes ...store.WorkflowNode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[workflowID] = nodes
}

func (f *fakeReviewStore) setFindings(workflowID string, findings ...store.Finding) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findings[workflowID] = findings
}

func (f *fakeReviewStore) setPublications(workflowID string, publications ...store.Publication) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publications[workflowID] = publications
}

func (f *fakeReviewStore) setCalls(workflowID, nodeID string, calls ...store.LLMCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range calls {
		calls[i].WorkflowID = workflowID
		calls[i].NodeID = nodeID
	}
	f.calls[workflowID+"\x00"+nodeID] = calls
}

func (f *fakeReviewStore) ListFindings(ctx context.Context, filter store.FindingFilter) ([]store.Finding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := f.findings[filter.WorkflowID]
	out := make([]store.Finding, 0, len(items))
	for _, finding := range items {
		if filter.Decision != "" && finding.Decision != filter.Decision {
			continue
		}
		if filter.Severity != "" && finding.Severity != filter.Severity {
			continue
		}
		out = append(out, finding)
	}
	return out, nil
}

func (f *fakeReviewStore) SetFindingAnnotation(ctx context.Context, workflowID, findingID, annotation, annotatedBy string) (store.Finding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := f.findings[workflowID]
	for i := range items {
		if items[i].ID != findingID {
			continue
		}
		items[i].Annotation = annotation
		if annotation == "" {
			items[i].AnnotatedBy = ""
			items[i].AnnotatedAt = nil
		} else {
			items[i].AnnotatedBy = annotatedBy
			at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
			items[i].AnnotatedAt = &at
		}
		f.findings[workflowID] = items
		return items[i], nil
	}
	return store.Finding{}, store.ErrNotFound
}

func (f *fakeReviewStore) ListLLMCalls(ctx context.Context, filter store.LLMCallFilter) ([]store.LLMCall, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCallFilter = filter
	items := f.calls[filter.WorkflowID+"\x00"+filter.NodeID]
	start := 0
	if filter.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(filter.Cursor)
		if err != nil {
			return nil, "", store.ErrInvalidCursor
		}
		start = -1
		for i := range items {
			if items[i].ID == string(raw) {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return nil, "", store.ErrInvalidCursor
		}
	}
	if start >= len(items) {
		return []store.LLMCall{}, "", nil
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultPageLimit
	}
	end := start + limit
	next := ""
	if end < len(items) {
		next = base64.RawURLEncoding.EncodeToString([]byte(items[end-1].ID))
	} else {
		end = len(items)
	}
	page := append([]store.LLMCall(nil), items[start:end]...)
	if !filter.IncludePayload {
		for i := range page {
			page[i].Request = nil
			page[i].Response = nil
		}
	}
	return page, next, nil
}

func (f *fakeReviewStore) ListPublications(ctx context.Context, workflowID string) ([]store.Publication, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Publication(nil), f.publications[workflowID]...), nil
}

func (f *fakeReviewStore) Stats(ctx context.Context, filter store.StatsFilter) (store.Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStats = filter
	return f.stats, nil
}

func (f *fakeReviewStore) GetWorkspace(ctx context.Context, projectID int64) (*store.Workspace, error) {
	return nil, store.ErrNotFound
}

func (f *fakeReviewStore) GetCurrentContextDoc(ctx context.Context, projectID int64, kind string) (*store.ContextDoc, error) {
	return nil, store.ErrNotFound
}

func (f *fakeReviewStore) ListContextDocs(ctx context.Context, filter store.ContextDocFilter) ([]store.ContextDoc, string, error) {
	return []store.ContextDoc{}, "", nil
}

func (f *fakeReviewStore) SaveManualContextDoc(ctx context.Context, doc store.ContextDoc) (*store.ContextDoc, error) {
	return nil, store.ErrNotFound
}

func (f *fakeReviewStore) LockContextDoc(ctx context.Context, projectID int64, baseSHA, kind string, locked bool, userID *int64) error {
	return store.ErrNotFound
}

// ------------------------------------------------------------ fake services

type reviewStartCall struct {
	projectID int64
	mrIID     int64
	profile   string
	full      bool
}

type fakeReviews struct {
	mu          sync.Mutex
	startCalls  []reviewStartCall
	cancelIDs   []string
	republishID []string
	err         error
}

func (f *fakeReviews) StartReview(ctx context.Context, projectID, mrIID int64, profile string, full bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls = append(f.startCalls, reviewStartCall{projectID: projectID, mrIID: mrIID, profile: profile, full: full})
	if f.err != nil {
		return "", f.err
	}
	return "job-start", nil
}

func (f *fakeReviews) CancelReview(ctx context.Context, workflowID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelIDs = append(f.cancelIDs, workflowID)
	if f.err != nil {
		return "", f.err
	}
	return "job-cancel", nil
}

func (f *fakeReviews) RepublishReview(ctx context.Context, workflowID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.republishID = append(f.republishID, workflowID)
	if f.err != nil {
		return "", f.err
	}
	return "job-republish", nil
}

type refreshCall struct {
	projectID         int64
	regenerateContext bool
	reindex           bool
}

type fakeWorkspace struct {
	mu    sync.Mutex
	calls []refreshCall
	err   error
}

func (f *fakeWorkspace) RefreshWorkspace(ctx context.Context, projectID int64, regenerateContext, reindex bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, refreshCall{projectID: projectID, regenerateContext: regenerateContext, reindex: reindex})
	if f.err != nil {
		return "", f.err
	}
	return "job-refresh", nil
}

type fakeMCP struct {
	mu      sync.Mutex
	servers []MCPServer
	tools   []MCPTool
	err     error
}

func (f *fakeMCP) TestMCPServer(ctx context.Context, server MCPServer) ([]MCPTool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers = append(f.servers, server)
	return f.tools, f.err
}

type fakeLLM struct {
	mu     sync.Mutex
	names  []string
	result ProviderTestResult
	err    error
}

func (f *fakeLLM) TestProvider(ctx context.Context, name string) (ProviderTestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names = append(f.names, name)
	return f.result, f.err
}

type fakeExtensions struct {
	mu       sync.Mutex
	servers  []MCPServer
	skills   []Skill
	bindings []BlockBinding
	updated  []MCPServerUpdate
	lastID   string
	imports  []MCPImportInput
	uploads  []SkillUpload
	dirs     []SkillDirInput
	deleted  []string
	err      error
}

func (f *fakeExtensions) ListMCPServers(ctx context.Context, scope string, projectID *int64) ([]MCPServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]MCPServer, 0)
	for _, server := range f.servers {
		if scope != "" && server.Scope != scope {
			continue
		}
		if projectID != nil && (server.ProjectID == nil || *server.ProjectID != *projectID) {
			continue
		}
		out = append(out, server)
	}
	return out, f.err
}

func (f *fakeExtensions) ListMCPServersPage(ctx context.Context, scope string, projectID *int64, cursor string, limit int) ([]MCPServer, string, error) {
	servers, err := f.ListMCPServers(ctx, scope, projectID)
	return servers, "", err
}

func (f *fakeExtensions) GetMCPServer(ctx context.Context, id string) (*MCPServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastID = id
	for i := range f.servers {
		if f.servers[i].ID == id {
			server := f.servers[i]
			return &server, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeExtensions) CreateMCPServer(ctx context.Context, in MCPServerInput) (*MCPServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	server := MCPServer{ID: "mcp-new", Scope: in.Scope, ProjectID: in.ProjectID,
		Name: in.Name, Config: in.Config, Enabled: in.Enabled}
	f.servers = append(f.servers, server)
	return &server, nil
}

func (f *fakeExtensions) UpdateMCPServer(ctx context.Context, id string, in MCPServerUpdate) (*MCPServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated = append(f.updated, in)
	f.lastID = id
	for i := range f.servers {
		if f.servers[i].ID != id {
			continue
		}
		if in.Name != nil {
			f.servers[i].Name = *in.Name
		}
		if in.Config != nil {
			f.servers[i].Config = in.Config
		}
		if in.Enabled != nil {
			f.servers[i].Enabled = *in.Enabled
		}
		server := f.servers[i]
		return &server, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeExtensions) DeleteMCPServer(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return f.err
}

func (f *fakeExtensions) ImportMCPServers(ctx context.Context, in MCPImportInput) ([]MCPServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imports = append(f.imports, in)
	if f.err != nil {
		return nil, f.err
	}
	names := make([]string, 0, len(in.Servers))
	for name := range in.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]MCPServer, 0, len(names))
	for _, name := range names {
		server := MCPServer{ID: "mcp-" + name, Scope: in.Scope, ProjectID: in.ProjectID,
			Name: name, Config: in.Servers[name], Enabled: true}
		f.servers = append(f.servers, server)
		out = append(out, server)
	}
	return out, nil
}

func (f *fakeExtensions) ListSkills(ctx context.Context, scope string, projectID *int64) ([]Skill, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Skill(nil), f.skills...), f.err
}

func (f *fakeExtensions) ListSkillsPage(ctx context.Context, scope string, projectID *int64, cursor string, limit int) ([]Skill, string, error) {
	skills, err := f.ListSkills(ctx, scope, projectID)
	return skills, "", err
}

func (f *fakeExtensions) UploadSkill(ctx context.Context, in SkillUpload) (*Skill, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, in)
	if f.err != nil {
		return nil, f.err
	}
	skill := Skill{ID: "skill-upload", Scope: in.Scope, ProjectID: in.ProjectID,
		Name: in.Name, Description: in.Description, Enabled: true}
	f.skills = append(f.skills, skill)
	return &skill, nil
}

func (f *fakeExtensions) RegisterSkillDir(ctx context.Context, in SkillDirInput) (*Skill, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs = append(f.dirs, in)
	if f.err != nil {
		return nil, f.err
	}
	skill := Skill{ID: "skill-dir", Scope: in.Scope, ProjectID: in.ProjectID,
		Name: "dir-skill", Description: "registered", Dir: in.Dir, Enabled: true}
	f.skills = append(f.skills, skill)
	return &skill, nil
}

func (f *fakeExtensions) DeleteSkill(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return f.err
}

func (f *fakeExtensions) ListBlockBindings(ctx context.Context, scope string, projectID *int64) ([]BlockBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]BlockBinding(nil), f.bindings...), f.err
}

func (f *fakeExtensions) ListBlockBindingsPage(ctx context.Context, scope string, projectID *int64, cursor string, limit int) ([]BlockBinding, string, error) {
	bindings, err := f.ListBlockBindings(ctx, scope, projectID)
	return bindings, "", err
}

func (f *fakeExtensions) UpsertBlockBinding(ctx context.Context, in BlockBindingInput) (*BlockBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	binding := BlockBinding{Scope: in.Scope, ProjectID: in.ProjectID, Block: in.Block,
		MCPTools: in.MCPTools, Skills: in.Skills}
	f.bindings = append(f.bindings, binding)
	return &binding, nil
}

// ------------------------------------------------------------------ test env

type m6Env struct {
	t          *testing.T
	server     *httptest.Server
	gitlab     *httptest.Server
	sqlite     *sqlite.Store
	store      Store
	reviews    *fakeReviews
	workspace  *fakeWorkspace
	contexts   ContextService
	extensions *fakeExtensions
	mcp        *fakeMCP
	llm        *fakeLLM
	platform   *config.Config
}

type m6Options struct {
	store        Store
	admin        AdminService
	adminFactory func(*sqlite.Store) AdminService
	platform     *config.Config
	streamPoll   time.Duration
	noContexts   bool
}

func newM6Env(t *testing.T, opts m6Options) *m6Env {
	t.Helper()
	st, err := sqlite.Open(context.Background(), t.TempDir()+"/m6.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	view := opts.store
	if view == nil {
		view = st
	}
	platform := opts.platform
	if platform == nil {
		platform = &config.Config{}
	}
	streamPoll := opts.streamPoll
	if streamPoll <= 0 {
		streamPoll = 10 * time.Millisecond
	}
	gitlab := newFakeGitLab(t)
	admin := opts.admin
	if admin == nil && opts.adminFactory != nil {
		admin = opts.adminFactory(st)
	}
	if admin == nil {
		admin = stubAdmin{}
	}
	reviews := &fakeReviews{}
	workspace := &fakeWorkspace{}
	var contexts ContextService
	if !opts.noContexts {
		contexts, err = app.NewContextService(app.ContextServiceConfig{
			Store:     st,
			Workspace: workspacepkg.NewManager(t.TempDir(), &repo.Git{}),
		})
		if err != nil {
			t.Fatalf("NewContextService: %v", err)
		}
	}
	extensions := &fakeExtensions{}
	mcp := &fakeMCP{}
	llm := &fakeLLM{}
	apiServer, err := New(Config{
		AdminToken: testAdminToken,
		Projects:   admin,
		Webhooks:   stubWebhookAcceptor{},
		Auth: AuthConfig{
			GitLabURL:       gitlab.URL,
			ClientID:        testOAuthClientID,
			ClientSecret:    testOAuthClientSecret,
			RedirectURL:     testOAuthRedirectURL,
			SessionKey:      []byte("0123456789abcdef0123456789abcdef"),
			SessionTTL:      time.Hour,
			BootstrapAdmins: []string{"alice"},
			HTTPClient:      gitlab.Client(),
			Now:             time.Now,
		},
		Store:      view,
		Access:     st,
		Reviews:    reviews,
		Workspace:  workspace,
		Contexts:   contexts,
		Extensions: extensions,
		MCP:        mcp,
		LLM:        llm,
		Tokens:     st,
		Platform:   platform,
		StreamPoll: streamPoll,
		Logger:     newTestLogger(io.Discard),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	server := httptest.NewServer(apiServer.Handler())
	t.Cleanup(func() {
		server.Close()
		gitlab.Close()
	})
	env := &m6Env{t: t, server: server, gitlab: gitlab, sqlite: st, store: view,
		reviews: reviews, workspace: workspace, contexts: contexts,
		extensions: extensions, mcp: mcp, llm: llm, platform: platform}
	return env
}

// ensureFixtureProject creates the minimum project row needed by a grant when
// a test uses a fake business store.
func (e *m6Env) ensureFixtureProject(projectID int64) {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.sqlite.GetProject(ctx, projectID); err == nil {
		return
	}
	path := "fixture/project-" + strconv.FormatInt(projectID, 10)
	if _, err := e.sqlite.RegisterProject(ctx, store.Project{
		ID: projectID, PathWithNamespace: path,
		WebURL: "https://gitlab.example.com/" + path, DefaultBranch: "main",
		TokenEnc: []byte{1}, WebhookSecretEnc: []byte{2}, TokenScopes: []string{"api"},
	}); err != nil {
		e.t.Fatalf("fixture RegisterProject(%d): %v", projectID, err)
	}
}

// grant records one explicit project grant used by a test fixture.
func (e *m6Env) grant(userID, projectID int64, role string) {
	e.t.Helper()
	e.ensureFixtureProject(projectID)
	switch userID {
	case 7:
		e.ensureUser(7, "alice", "Alice Admin", roleAdmin)
	case 8:
		e.ensureUser(8, "bob", "Bob Viewer", roleViewer)
	default:
		e.ensureUser(userID, "fixture-"+strconv.FormatInt(userID, 10), "Fixture", roleViewer)
	}
	if err := e.sqlite.SetProjectGrant(context.Background(), projectID, userID, role); err != nil {
		e.t.Fatalf("SetProjectGrant(%d,%d): %v", projectID, userID, err)
	}
}

// businessClient is the platform-admin session with explicit fixture grants.
// It is used where the test exercises project business data.
func (e *m6Env) businessClient() *m6Client {
	e.t.Helper()
	client := e.loginAs("code-admin")
	for _, projectID := range []int64{42, 43, 44} {
		e.grant(7, projectID, store.ProjectAccessManage)
	}
	return client
}

// ensureUser creates the local user row a grant references. It uses the
// session store because tests cannot insert users through the public API.
func (e *m6Env) ensureUser(userID int64, username, name, role string) {
	e.t.Helper()
	if _, err := e.sqlite.GetUser(context.Background(), userID); err == nil {
		return
	}
	now := time.Now()
	sum := sha256.Sum256([]byte("fixture-user-" + strconv.FormatInt(userID, 10)))
	if err := e.sqlite.CreateSession(context.Background(), store.Session{
		IDHash: sum[:], UserID: userID, Username: username, Name: name, Role: role,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}, 4096); err != nil {
		e.t.Fatalf("fixture user %d: %v", userID, err)
	}
}

type m6Client struct {
	t      *testing.T
	base   string
	client *http.Client
	bearer string
	csrf   string
}

func (e *m6Env) newHTTPClient() *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		e.t.Fatalf("cookiejar.New: %v", err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (e *m6Env) loginAs(code string) *m6Client {
	e.t.Helper()
	client := e.newHTTPClient()
	c := &m6Client{t: e.t, base: e.server.URL, client: client}
	login := c.request(http.MethodGet, "/auth/login", nil, nil)
	if login.status != http.StatusFound {
		e.t.Fatalf("login status = %d, body=%s", login.status, login.body)
	}
	location, err := url.Parse(login.header.Get("Location"))
	if err != nil {
		e.t.Fatalf("parse login location: %v", err)
	}
	query := url.Values{"code": {code}, "state": {location.Query().Get("state")}}
	callback := c.request(http.MethodGet, "/auth/callback?"+query.Encode(), nil, nil)
	if callback.status != http.StatusSeeOther {
		e.t.Fatalf("callback status = %d, body=%s", callback.status, callback.body)
	}
	me := c.request(http.MethodGet, "/api/v1/me", nil, nil)
	body := decodeMap(e.t, me.body)
	csrf, _ := body["csrf_token"].(string)
	if csrf == "" {
		e.t.Fatal("login did not return a CSRF token")
	}
	c.csrf = csrf
	return c
}

func (e *m6Env) bearerClient() *m6Client {
	return &m6Client{t: e.t, base: e.server.URL, client: &http.Client{}, bearer: testAdminToken}
}

func (e *m6Env) anonymousClient() *m6Client {
	return &m6Client{t: e.t, base: e.server.URL, client: &http.Client{}}
}

func (c *m6Client) request(method, path string, body []byte, headers map[string]string) apiResponse {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		c.t.Fatalf("NewRequest: %v", err)
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.csrf != "" && !safeMethod(method) {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("ReadAll: %v", err)
	}
	return apiResponse{status: resp.StatusCode, header: resp.Header, body: data}
}

func (c *m6Client) json(method, path string, body any) apiResponse {
	c.t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
	}
	return c.request(method, path, raw, map[string]string{"Content-Type": "application/json"})
}

func (c *m6Client) withCSRF(csrf string) *m6Client {
	copy := *c
	copy.csrf = csrf
	return &copy
}

func (c *m6Client) withoutCSRF() *m6Client {
	copy := *c
	copy.csrf = ""
	return &copy
}

func ptr[T any](value T) *T {
	return &value
}

// seedWorkflow builds a completed review used by review endpoint tests.
func seedWorkflow(env *m6Env) {
	wfID := "wf-1"
	mrIID := int64(7)
	started := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Minute)
	env.store.(*fakeReviewStore).workflows = append(env.store.(*fakeReviewStore).workflows, store.Workflow{
		ID: wfID, Kind: "review", ProjectID: 42, MRIID: &mrIID, HeadSHA: "head-1",
		Profile: "default", Trigger: "mr_update", Status: "completed", Verdict: "BLOCKING",
		TokensIn: 1000, TokensOut: 200, CostMicros: 5000,
		CreatedAt: started, StartedAt: &started, FinishedAt: &finished,
	})
	planStart := started
	planEnd := started.Add(8 * time.Second)
	env.store.(*fakeReviewStore).setNodes(wfID,
		store.WorkflowNode{WorkflowID: wfID, ID: "plan", Kind: "plan", Status: "completed",
			Required: true, Provider: "claude", Attempt: 1,
			TokensIn: 100, TokensOut: 20, CostMicros: 100,
			StartedAt: &planStart, FinishedAt: &planEnd},
		store.WorkflowNode{WorkflowID: wfID, ID: "review.std.p1", Kind: "review.standards", Status: "completed",
			DependsOn: []string{"plan"}, Provider: "claude", Attempt: 1,
			TokensIn: 200, TokensOut: 40, CostMicros: 200},
		store.WorkflowNode{WorkflowID: wfID, ID: "verify.f7", Kind: "verify", Status: "completed",
			DependsOn: []string{"review.std.p1"}, Provider: "gpt", Attempt: 1,
			TokensIn: 300, TokensOut: 60, CostMicros: 300},
	)
	env.store.(*fakeReviewStore).setFindings(wfID,
		store.Finding{ID: "f1", WorkflowID: wfID, OriginNode: "review.std.p1", Severity: "P1",
			Category: "bug", Title: "one", Body: "body", Decision: "publish", FinalSeverity: "P1"},
		store.Finding{ID: "f2", WorkflowID: wfID, OriginNode: "review.std.p1", Severity: "P2",
			Category: "style", Title: "two", Body: "body", Decision: "mark", FinalSeverity: "P2"},
		store.Finding{ID: "f3", WorkflowID: wfID, OriginNode: "verify.f7", Severity: "P2",
			Category: "style", Title: "three", Body: "body", Decision: "drop"},
	)
	env.store.(*fakeReviewStore).setPublications(wfID,
		store.Publication{ID: "p1", ProjectID: 42, MRIID: mrIID, Kind: "summary", PublishKey: "summary",
			GitlabID: "99102", WorkflowID: wfID},
		store.Publication{ID: "p2", ProjectID: 42, MRIID: mrIID, Kind: "discussion", PublishKey: "f1",
			GitlabID: "555", WorkflowID: wfID},
	)
	env.store.(*fakeReviewStore).setCalls(wfID, "plan",
		store.LLMCall{ID: "c1", Turn: 1, Provider: "claude", Model: "sonnet", InputTokens: 10,
			OutputTokens: 5, LatencyMS: 120, CreatedAt: started,
			Request: json.RawMessage(`{"prompt":"secret prompt"}`)},
	)
}

func (e *m6Env) decode(t *testing.T, response apiResponse) map[string]any {
	t.Helper()
	return decodeMap(t, response.body)
}

func assertStatus(t *testing.T, response apiResponse, want int) {
	t.Helper()
	if response.status != want {
		t.Fatalf("status = %d, want %d, body=%s", response.status, want, response.body)
	}
}
