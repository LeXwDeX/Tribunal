package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func newTestCipher(t *testing.T) *secret.Cipher {
	t.Helper()
	cipher, err := secret.New(testKey)
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	return cipher
}

// testStoreTemplate builds one migrated database in a private directory. It
// closes the only store connection and fails closed if SQLite left a WAL or
// shared-memory file, because then the main file alone is not a complete
// snapshot. Each test receives its own copy, so no mutable state is shared.
var testStoreTemplate = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "tribunal-test-store-*")
	if err != nil {
		return nil, fmt.Errorf("create test store template dir: %w", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "template.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		return nil, fmt.Errorf("open test store template: %w", err)
	}
	if err := st.Close(); err != nil {
		return nil, fmt.Errorf("close test store template: %w", err)
	}
	// SQLite checkpoints and removes WAL files when the last connection
	// closes. A leftover file means the main file is not self-contained.
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			return nil, fmt.Errorf("test store template still has %s file", suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("stat test store template %s: %w", suffix, err)
		}
	}
	snapshot, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read test store template: %w", err)
	}
	return snapshot, nil
})

func openTestStorePath(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	snapshot, err := testStoreTemplate()
	if err != nil {
		t.Fatalf("test store template: %v", err)
	}
	path := filepath.Join(t.TempDir(), "review.db")
	if err := os.WriteFile(path, snapshot, 0o600); err != nil {
		t.Fatalf("write test store: %v", err)
	}
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ConfigureMCPSecrets(context.Background(), newTestCipher(t)); err != nil {
		t.Fatalf("ConfigureMCPSecrets: %v", err)
	}
	return st, path
}

func openTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, _ := openTestStorePath(t)
	return st
}

func TestOpenTestStoreTemplateIsCompleteAndSelfContained(t *testing.T) {
	snapshot, err := testStoreTemplate()
	if err != nil {
		t.Fatalf("test store template: %v", err)
	}
	path := filepath.Join(t.TempDir(), "template.db")
	if err := os.WriteFile(path, snapshot, 0o600); err != nil {
		t.Fatalf("write template: %v", err)
	}
	noSidecars := func() {
		t.Helper()
		for _, suffix := range []string{"-wal", "-shm"} {
			if _, err := os.Stat(path + suffix); err == nil {
				t.Fatalf("template copy has %s", suffix)
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stat template %s: %v", suffix, err)
			}
		}
	}
	noSidecars()

	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(DELETE)")
	if err != nil {
		t.Fatalf("open template: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var journalMode string
	if err := db.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "delete") {
		t.Fatalf("template journal mode = %q, want delete", journalMode)
	}
	noSidecars()

	var integrity string
	if err := db.QueryRowContext(context.Background(), `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q", integrity)
	}

	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	missing := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			missing[entry.Name()] = true
		}
	}
	rows, err := db.QueryContext(context.Background(), `SELECT name FROM schema_migrations`)
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan migration: %v", err)
		}
		if !missing[name] {
			rows.Close()
			t.Fatalf("template has unexpected migration %q", name)
		}
		delete(missing, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close migrations: %v", err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate migrations: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("template is missing migrations %v", missing)
	}

	var projects int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM projects`).Scan(&projects); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	if projects != 0 {
		t.Fatalf("template projects = %d, want 0", projects)
	}
}

func TestOpenTestStoreCopiesAreIsolated(t *testing.T) {
	ctx := context.Background()
	first, firstPath := openTestStorePath(t)
	second, secondPath := openTestStorePath(t)
	if firstPath == secondPath {
		t.Fatalf("two tests share the database path %q", firstPath)
	}

	cipher := newTestCipher(t)
	seedProject(t, first, cipher, 42, "first", "{}", "default")
	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}
	reopened, err := sqlite.Open(ctx, firstPath)
	if err != nil {
		t.Fatalf("reopen first: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.GetProject(ctx, 42); err != nil {
		t.Fatalf("write missing after reopen: %v", err)
	}

	if _, err := second.GetProject(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second copy sees first project: %v", err)
	}
	if projects, err := second.ListProjects(ctx); err != nil || len(projects) != 0 {
		t.Fatalf("second copy projects = %v, %v", projects, err)
	}
	seedProject(t, second, cipher, 43, "second", "{}", "default")
	if _, err := reopened.GetProject(ctx, 43); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("first copy sees second project: %v", err)
	}
}

func testProfiles() map[string]config.ProfileConfig {
	return map[string]config.ProfileConfig{
		"default": {},
		"deep":    {},
	}
}

func testDefaults() config.ProjectSettings {
	return config.ProjectSettings{
		Language:        "en",
		SkipDraft:       true,
		Ignore:          []string{"**/*.lock"},
		MaxFiles:        200,
		MaxDiffLines:    8000,
		PostFailureNote: true,
		AllowRepoConfig: true,
		ProfileByLabel:  map[string]string{},
	}
}

type fakeGitLab struct {
	mu sync.Mutex

	project     *gitlab.Project
	projectErr  error
	projectRefs []string
	tokenInfo   *gitlab.TokenInfo
	tokenErr    error
	user        *gitlab.User
	userErr     error
	mr          *gitlab.MergeRequest
	mrErr       error
	access      int
	accessErr   error

	hookID        int64
	hookErr       error
	listHooksErr  error
	updateHookErr error
	deleteErr     error
	hookSpecs     []gitlab.HookSpec
	hooks         []gitlab.Hook
	editedHooks   []int64
	deletedHooks  []int64
	calls         []string

	notes         []gitlab.Note
	nextNoteID    int64
	listNotesErr  error
	createNoteErr error
	updateNoteErr error
	noteCreates   int
	noteUpdates   int

	awards       map[int64][]gitlab.Award
	nextAwardID  int64
	listAwardErr error
	addAwardErr  error
	delAwardErr  error
	awardAdds    []string
	awardDeletes []int64
}

func (f *fakeGitLab) record(name string) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
}

func (f *fakeGitLab) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeGitLab) callCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if call == name {
			count++
		}
	}
	return count
}

func (f *fakeGitLab) projectRefSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.projectRefs...)
}

func (f *fakeGitLab) GetProject(_ context.Context, token, ref string) (*gitlab.Project, error) {
	f.record("GetProject")
	f.mu.Lock()
	defer f.mu.Unlock()
	if token == "" {
		return nil, fmt.Errorf("empty token")
	}
	f.projectRefs = append(f.projectRefs, ref)
	if f.projectErr != nil {
		return nil, f.projectErr
	}
	if f.project == nil {
		return nil, fmt.Errorf("no project fixture")
	}
	return f.project, nil
}

func (f *fakeGitLab) GetTokenInfo(_ context.Context, token string) (*gitlab.TokenInfo, error) {
	f.record("GetTokenInfo")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokenErr != nil {
		return nil, f.tokenErr
	}
	return f.tokenInfo, nil
}

func (f *fakeGitLab) CurrentUser(_ context.Context, token string) (*gitlab.User, error) {
	f.record("CurrentUser")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.user, f.userErr
}

func (f *fakeGitLab) GetMergeRequest(_ context.Context, token string, projectID, iid int64) (*gitlab.MergeRequest, error) {
	f.record("GetMergeRequest")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mr, f.mrErr
}

func (f *fakeGitLab) GetMemberAccess(_ context.Context, token string, projectID, userID int64) (int, error) {
	f.record("GetMemberAccess")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.access, f.accessErr
}

func (f *fakeGitLab) ListMRNotes(_ context.Context, _ string, _, _ int64) ([]gitlab.Note, error) {
	f.record("ListMRNotes")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listNotesErr != nil {
		return nil, f.listNotesErr
	}
	return append([]gitlab.Note(nil), f.notes...), nil
}

func (f *fakeGitLab) CreateMRNote(_ context.Context, _ string, _, _ int64, body string) (*gitlab.Note, error) {
	f.record("CreateMRNote")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createNoteErr != nil {
		return nil, f.createNoteErr
	}
	f.nextNoteID++
	var authorID int64
	if f.user != nil {
		authorID = f.user.ID
	}
	note := gitlab.Note{ID: f.nextNoteID, Body: body, AuthorID: authorID}
	f.notes = append(f.notes, note)
	f.noteCreates++
	return &note, nil
}

func (f *fakeGitLab) UpdateMRNote(_ context.Context, _ string, _, _, noteID int64, body string) error {
	f.record("UpdateMRNote")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateNoteErr != nil {
		return f.updateNoteErr
	}
	for i := range f.notes {
		if f.notes[i].ID == noteID {
			f.notes[i].Body = body
			f.noteUpdates++
			return nil
		}
	}
	return &gitlab.Error{Op: "update_note", Status: 404}
}

func (f *fakeGitLab) ListNoteAwards(_ context.Context, _ string, _, _, noteID int64) ([]gitlab.Award, error) {
	f.record("ListNoteAwards")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listAwardErr != nil {
		return nil, f.listAwardErr
	}
	return append([]gitlab.Award(nil), f.awards[noteID]...), nil
}

func (f *fakeGitLab) AddNoteAward(_ context.Context, _ string, _, _, noteID int64, name string) (*gitlab.Award, error) {
	f.record("AddNoteAward")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addAwardErr != nil {
		return nil, f.addAwardErr
	}
	if f.awards == nil {
		f.awards = map[int64][]gitlab.Award{}
	}
	f.nextAwardID++
	award := gitlab.Award{ID: f.nextAwardID, Name: name, UserID: f.user.ID, Username: f.user.Username}
	f.awards[noteID] = append(f.awards[noteID], award)
	f.awardAdds = append(f.awardAdds, name)
	return &award, nil
}

func (f *fakeGitLab) DeleteNoteAward(_ context.Context, _ string, _, _, noteID, awardID int64) error {
	f.record("DeleteNoteAward")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delAwardErr != nil {
		return f.delAwardErr
	}
	awards := f.awards[noteID]
	for i, award := range awards {
		if award.ID != awardID {
			continue
		}
		f.awards[noteID] = append(append([]gitlab.Award(nil), awards[:i]...), awards[i+1:]...)
		f.awardDeletes = append(f.awardDeletes, awardID)
		return nil
	}
	return &gitlab.Error{Op: "delete_note_award", Status: 404}
}

func (f *fakeGitLab) noteSnapshot() []gitlab.Note {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gitlab.Note(nil), f.notes...)
}

func (f *fakeGitLab) ListProjectHooks(_ context.Context, _ string, _ int64) ([]gitlab.Hook, error) {
	f.record("ListProjectHooks")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listHooksErr != nil {
		return nil, f.listHooksErr
	}
	return append([]gitlab.Hook(nil), f.hooks...), nil
}

func (f *fakeGitLab) CreateProjectHook(_ context.Context, token string, projectID int64, spec gitlab.HookSpec) (int64, error) {
	f.record("CreateProjectHook")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hookErr != nil {
		return 0, f.hookErr
	}
	f.hookSpecs = append(f.hookSpecs, spec)
	f.hooks = append(f.hooks, gitlab.Hook{
		ID:                    f.hookID,
		URL:                   spec.URL,
		PushEvents:            spec.PushEvents,
		MergeRequestsEvents:   spec.MergeRequestsEvents,
		NoteEvents:            spec.NoteEvents,
		EnableSSLVerification: spec.EnableSSLVerification,
	})
	return f.hookID, nil
}

func (f *fakeGitLab) UpdateProjectHook(_ context.Context, _ string, _, hookID int64, spec gitlab.HookSpec) error {
	f.record("UpdateProjectHook")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateHookErr != nil {
		return f.updateHookErr
	}
	f.editedHooks = append(f.editedHooks, hookID)
	for i := range f.hooks {
		if f.hooks[i].ID != hookID {
			continue
		}
		f.hooks[i].URL = spec.URL
		f.hooks[i].PushEvents = spec.PushEvents
		f.hooks[i].MergeRequestsEvents = spec.MergeRequestsEvents
		f.hooks[i].NoteEvents = spec.NoteEvents
		f.hooks[i].EnableSSLVerification = spec.EnableSSLVerification
		return nil
	}
	return &gitlab.Error{Op: "update_hook", Status: 404}
}

func (f *fakeGitLab) DeleteProjectHook(_ context.Context, token string, projectID, hookID int64) error {
	f.record("DeleteProjectHook")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedHooks = append(f.deletedHooks, hookID)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	kept := f.hooks[:0]
	for _, hook := range f.hooks {
		if hook.ID != hookID {
			kept = append(kept, hook)
		}
	}
	f.hooks = kept
	return nil
}

func defaultFakeGitLab() *fakeGitLab {
	return &fakeGitLab{
		project: &gitlab.Project{
			ID:                42,
			PathWithNamespace: "group/repo",
			WebURL:            "https://gitlab.example.com/group/repo",
			HTTPURLToRepo:     "https://gitlab.example.com/group/repo.git",
			DefaultBranch:     "main",
		},
		tokenInfo: &gitlab.TokenInfo{
			Scopes: []string{"api", "read_repository"},
			Active: true,
		},
		hookID:      99,
		awards:      map[int64][]gitlab.Award{},
		nextAwardID: 500,
		user:        &gitlab.User{ID: 9, Username: "review-bot"},
		mr:          &gitlab.MergeRequest{IID: 7, State: "opened", TargetProjectID: 42, SHA: sha40},
		access:      30,
	}
}

const sha40 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func seedProject(t *testing.T, st *sqlite.Store, cipher *secret.Cipher, id int64, webhookSecret, settings, profile string) *store.Project {
	t.Helper()
	tokenEnc, err := cipher.Seal([]byte("glpat-seeded-token"), secret.TokenAAD(id))
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}
	secretEnc, err := cipher.Seal([]byte(webhookSecret), secret.WebhookSecretAAD(id))
	if err != nil {
		t.Fatalf("encrypt secret: %v", err)
	}
	if settings == "" {
		settings = "{}"
	}
	if profile == "" {
		profile = "default"
	}
	if _, err := st.RegisterProject(context.Background(), store.Project{
		ID:                id,
		PathWithNamespace: "group/repo",
		WebURL:            "https://gitlab.example.com/group/repo",
		DefaultBranch:     "main",
		TokenEnc:          tokenEnc,
		TokenScopes:       []string{"api", "read_repository"},
		WebhookSecretEnc:  secretEnc,
		Profile:           profile,
		Settings:          json.RawMessage(settings),
	}); err != nil {
		t.Fatalf("RegisterProject: %v", err)
	}
	project, err := st.GetProject(context.Background(), id)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	return project
}

type recordingDeliveryStore struct {
	WebhookStore
	project *store.Project
	getErr  error
	recErr  error

	mu      sync.Mutex
	records []store.Delivery
	jobs    []*store.Job
}

func (s *recordingDeliveryStore) GetProject(ctx context.Context, id int64) (*store.Project, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.project != nil {
		return s.project, nil
	}
	return s.WebhookStore.GetProject(ctx, id)
}

func (s *recordingDeliveryStore) RecordDelivery(ctx context.Context, d store.Delivery, j *store.Job) (string, bool, error) {
	s.mu.Lock()
	s.records = append(s.records, d)
	if j != nil {
		s.jobs = append(s.jobs, j)
	}
	s.mu.Unlock()
	if s.recErr != nil {
		return "", false, s.recErr
	}
	return s.WebhookStore.RecordDelivery(ctx, d, j)
}

func (s *recordingDeliveryStore) RecordReviewCandidateDelivery(ctx context.Context, d store.Delivery, candidate *store.ReviewCandidate) (store.ReviewEnqueueResult, error) {
	s.mu.Lock()
	s.records = append(s.records, d)
	if candidate != nil {
		payload := candidate.Payload
		s.jobs = append(s.jobs, &store.Job{
			Kind: "review.candidate", Key: "review.candidate:" + itoa(candidate.ProjectID) + ":" + itoa(candidate.MRIID),
			Payload: payload, MaxAttempts: candidate.MaxAttempts,
		})
	}
	s.mu.Unlock()
	if s.recErr != nil {
		return store.ReviewEnqueueResult{}, s.recErr
	}
	return s.WebhookStore.RecordReviewCandidateDelivery(ctx, d, candidate)
}

type failingProjectStore struct {
	ProjectStore
	err   error
	calls int
}

func (s *failingProjectStore) RegisterProject(ctx context.Context, project store.Project) (string, error) {
	s.calls++
	return "", s.err
}

type commandFakeStore struct {
	project *store.Project
	getErr  error

	mu         sync.Mutex
	jobs       []store.Job
	enqueueErr error
	duplicate  bool
}

func (s *commandFakeStore) GetProject(_ context.Context, id int64) (*store.Project, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.project, nil
}

func (s *commandFakeStore) GetJob(_ context.Context, id string) (*store.Job, error) {
	return &store.Job{ID: id, Kind: "review.command", Payload: json.RawMessage(`{"project_id":42}`)}, nil
}

func (s *commandFakeStore) EnqueueReviewLatest(_ context.Context, command store.ReviewCommand) (store.ReviewEnqueueResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enqueueErr != nil {
		return store.ReviewEnqueueResult{}, s.enqueueErr
	}
	kind, key := "review.run", reviewRunKey(command.ProjectID, command.MRIID)
	if command.Cancel {
		kind, key = "review.cancel", reviewCancelKey(command.ProjectID, command.MRIID)
	}
	s.jobs = append(s.jobs, store.Job{
		Kind: kind, Key: key, Payload: command.Payload, MaxAttempts: command.MaxAttempts,
	})
	return store.ReviewEnqueueResult{JobID: "job-1", Duplicate: s.duplicate}, nil
}

func (s *commandFakeStore) enqueued() []store.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.Job(nil), s.jobs...)
}

func newCommandTestWorker(t *testing.T, api *fakeGitLab, store *commandFakeStore, bots []string) *CommandWorker {
	t.Helper()
	worker, err := NewCommandWorker(CommandWorkerConfig{
		Store:        store,
		Workspace:    workspace.NewManager(t.TempDir(), nil),
		GitLab:       api,
		Cipher:       newTestCipher(t),
		Profiles:     testProfiles(),
		BotUsernames: bots,
	})
	if err != nil {
		t.Fatalf("NewCommandWorker: %v", err)
	}
	return worker
}

func commandJob(t *testing.T, payload commandPayload) *store.Job {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &store.Job{ID: "command-job", Kind: "review.command", Payload: raw}
}

func persistedCommandJob(t *testing.T, db *sqlite.Store, payload commandPayload) *store.Job {
	t.Helper()
	job := commandJob(t, payload)
	job.ID = ""
	job.Key = commandKey(payload.ProjectID, payload.NoteID)
	id, _, err := db.Enqueue(context.Background(), *job)
	if err != nil {
		t.Fatal(err)
	}
	job, err = db.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func openFakeStore(t *testing.T, cipher *secret.Cipher) *commandFakeStore {
	t.Helper()
	project := seedProject(t, openTestStore(t), cipher, 42, "whsec", "{}", "default")
	return &commandFakeStore{project: project}
}

func mrBody(projectID, iid int64, changes func(map[string]any)) []byte {
	attrs := map[string]any{
		"id":                1000 + iid,
		"iid":               iid,
		"target_project_id": projectID,
		"target_branch":     "main",
		"source_branch":     "feature",
		"state":             "opened",
		"action":            "open",
		"oldrev":            "",
		"last_commit":       map[string]any{"id": sha40},
		"title":             "Add feature",
		"labels":            []string{},
		"draft":             false,
		"work_in_progress":  false,
	}
	mr := map[string]any{
		"iid":               iid,
		"target_project_id": projectID,
		"target_branch":     "main",
		"state":             "opened",
		"last_commit":       map[string]any{"id": sha40},
		"labels":            []string{},
		"title":             "Add feature",
	}
	body := map[string]any{
		"object_kind":       "merge_request",
		"project":           map[string]any{"id": projectID, "path_with_namespace": "group/repo"},
		"object_attributes": attrs,
		"merge_request":     mr,
		"labels":            []string{},
	}
	if changes != nil {
		changes(body)
	}
	raw, _ := json.Marshal(body)
	return raw
}

func patchAttr(body map[string]any, key string, value any) {
	body["object_attributes"].(map[string]any)[key] = value
}

func patchMR(body map[string]any, key string, value any) {
	body["merge_request"].(map[string]any)[key] = value
}

func noteBody(projectID, iid, noteID, authorID int64, username, note string) []byte {
	body := map[string]any{
		"object_kind": "note",
		"project":     map[string]any{"id": projectID},
		"user":        map[string]any{"id": authorID, "username": username, "bot": false},
		"object_attributes": map[string]any{
			"id":            noteID,
			"note":          note,
			"noteable_type": "MergeRequest",
			"noteable_iid":  iid,
			"action":        "create",
			"author_id":     authorID,
		},
		"merge_request": map[string]any{
			"iid":               iid,
			"target_project_id": projectID,
			"state":             "opened",
			"labels":            []string{},
		},
	}
	raw, _ := json.Marshal(body)
	return raw
}

func pushBody(projectID int64, ref, after string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"object_kind": "push",
		"project":     map[string]any{"id": projectID},
		"ref":         ref,
		"before":      sha40,
		"after":       after,
	})
	return raw
}
