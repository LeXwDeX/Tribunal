package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/agent"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/jobs"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/review"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/workspace"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/prompts"
)

// reviewGitLab is a small in-memory GitLab fake for F1/F7/F8 tests.
type reviewGitLab struct {
	mu sync.Mutex

	mr      *gitlab.MergeRequest
	version *gitlab.Version
	diffs   []gitlab.FileDiff
	docs    map[string]string
	rawErr  map[string]error
	mrErr   error
	noteErr error

	notes         []gitlab.Note
	nextNoteID    int64
	discussions   []gitlab.Discussion
	nextDiscID    int64
	statuses      []gitlab.CommitStatus
	awards        map[int64][]gitlab.Award
	nextAwardID   int64
	awardErr      error
	addAwardErr   error
	delAwardErr   error
	noteUpdateErr error
	statusNoteErr error
	currentUser   *gitlab.User

	summaryCreates    int
	summaryUpdates    int
	discussionCreates int
	statusUpdates     []string
}

func newReviewGitLab(head string) *reviewGitLab {
	return &reviewGitLab{
		mr: &gitlab.MergeRequest{
			IID:             7,
			State:           "opened",
			TargetProjectID: 42,
			SHA:             head,
			Title:           "Add feature",
			SourceBranch:    "feature",
			TargetBranch:    "main",
			Labels:          []string{},
			WebURL:          "https://gitlab.example.com/group/repo/-/merge_requests/7",
			Author:          &gitlab.User{ID: 12, Username: "alice"},
		},
		version: &gitlab.Version{
			ID:       1,
			BaseSHA:  strings.Repeat("1", 40),
			StartSHA: strings.Repeat("2", 40),
			HeadSHA:  head,
		},
		diffs: []gitlab.FileDiff{{
			NewPath: "main.go",
			Diff:    "@@ -1,2 +1,3 @@\n a\n+bad := 1\n b\n",
		}},
		docs:        map[string]string{},
		rawErr:      map[string]error{},
		nextNoteID:  100,
		nextDiscID:  200,
		awards:      map[int64][]gitlab.Award{},
		nextAwardID: 700,
		currentUser: &gitlab.User{ID: 9, Username: "review-bot"},
	}
}

func (f *reviewGitLab) CurrentUser(context.Context, string) (*gitlab.User, error) {
	return f.currentUser, nil
}

func (f *reviewGitLab) GetMergeRequest(context.Context, string, int64, int64) (*gitlab.MergeRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mrErr != nil {
		return nil, f.mrErr
	}
	copy := *f.mr
	return &copy, nil
}

func (f *reviewGitLab) LatestMRVersion(context.Context, string, int64, int64) (*gitlab.Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	copy := *f.version
	return &copy, nil
}

func (f *reviewGitLab) ListMRDiffs(context.Context, string, int64, int64) ([]gitlab.FileDiff, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gitlab.FileDiff(nil), f.diffs...), nil
}

func (f *reviewGitLab) ListMRPipelines(context.Context, string, int64, int64) ([]gitlab.Pipeline, error) {
	return nil, nil
}

func (f *reviewGitLab) ListPipelineJobs(context.Context, string, int64, int64) ([]gitlab.Job, error) {
	return nil, nil
}

func (f *reviewGitLab) ListClosingIssues(context.Context, string, int64, int64) ([]gitlab.Issue, error) {
	return nil, nil
}

func (f *reviewGitLab) GetRawFile(_ context.Context, _ string, _ int64, path, _ string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.rawErr[path]; ok {
		return nil, err
	}
	if body, ok := f.docs[path]; ok {
		return []byte(body), nil
	}
	return nil, &gitlab.Error{Op: "get_raw_file", Status: 404}
}

func (f *reviewGitLab) ListMRNotes(context.Context, string, int64, int64) ([]gitlab.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gitlab.Note(nil), f.notes...), nil
}

func (f *reviewGitLab) CreateMRNote(_ context.Context, _ string, _, _ int64, body string) (*gitlab.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noteErr != nil {
		return nil, f.noteErr
	}
	if f.statusNoteErr != nil && strings.HasPrefix(body, reviewStatusMarkerPrefix) {
		return nil, f.statusNoteErr
	}
	f.nextNoteID++
	note := gitlab.Note{ID: f.nextNoteID, Body: body, AuthorID: f.currentUser.ID}
	f.notes = append(f.notes, note)
	f.summaryCreates++
	f.recordStatusBody(body)
	return &note, nil
}

func (f *reviewGitLab) UpdateMRNote(_ context.Context, _ string, _, _, noteID int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noteUpdateErr != nil {
		return f.noteUpdateErr
	}
	if f.statusNoteErr != nil && strings.HasPrefix(body, reviewStatusMarkerPrefix) {
		return f.statusNoteErr
	}
	for i := range f.notes {
		if f.notes[i].ID == noteID {
			f.notes[i].Body = body
			f.summaryUpdates++
			f.recordStatusBody(body)
			return nil
		}
	}
	return &gitlab.Error{Op: "update_note", Status: 404}
}

func (f *reviewGitLab) recordStatusBody(body string) {
	if strings.HasPrefix(body, reviewStatusMarkerPrefix) {
		f.statusUpdates = append(f.statusUpdates, body)
	}
}

func (f *reviewGitLab) ListDiscussions(context.Context, string, int64, int64) ([]gitlab.Discussion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]gitlab.Discussion, len(f.discussions))
	copy(out, f.discussions)
	return out, nil
}

func (f *reviewGitLab) CreateDiscussion(_ context.Context, _ string, _, _ int64, d gitlab.NewDiscussion) (*gitlab.Discussion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextDiscID++
	noteID := f.nextNoteID + 1000 + f.nextDiscID
	discussion := gitlab.Discussion{
		ID:    fmt.Sprintf("discussion-%d", f.nextDiscID),
		Notes: []gitlab.Note{{ID: noteID, Body: d.Body, AuthorID: f.currentUser.ID, Resolvable: true}},
	}
	f.discussions = append(f.discussions, discussion)
	f.discussionCreates++
	return &discussion, nil
}

func (f *reviewGitLab) SetCommitStatus(_ context.Context, _ string, _ int64, _ string, st gitlab.CommitStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, st)
	return nil
}

func (f *reviewGitLab) ListNoteAwards(_ context.Context, _ string, _, _, noteID int64) ([]gitlab.Award, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.awardErr != nil {
		return nil, f.awardErr
	}
	return append([]gitlab.Award(nil), f.awards[noteID]...), nil
}

func (f *reviewGitLab) AddNoteAward(_ context.Context, _ string, _, _, noteID int64, name string) (*gitlab.Award, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addAwardErr != nil {
		return nil, f.addAwardErr
	}
	f.nextAwardID++
	award := gitlab.Award{ID: f.nextAwardID, Name: name, UserID: f.currentUser.ID, Username: f.currentUser.Username}
	f.awards[noteID] = append(f.awards[noteID], award)
	return &award, nil
}

func (f *reviewGitLab) DeleteNoteAward(_ context.Context, _ string, _, _, noteID, awardID int64) error {
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
		return nil
	}
	return &gitlab.Error{Op: "delete_note_award", Status: 404}
}

func (f *reviewGitLab) noteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.notes)
}

// fakeGitOps implements GitOps without touching a real repository.
type fakeGitOps struct {
	mu          sync.Mutex
	head        string
	ancestor    bool
	changed     []string
	worktrees   map[string]bool
	dirHeads    map[string]string
	addCalls    int
	removeCalls int
}

func newFakeGitOps(head string) *fakeGitOps {
	return &fakeGitOps{head: head, worktrees: map[string]bool{}, dirHeads: map[string]string{}}
}

func (f *fakeGitOps) EnsureMirror(context.Context, string, string, repo.Auth) error { return nil }
func (f *fakeGitOps) Fetch(context.Context, string, repo.Auth, ...string) error     { return nil }

func (f *fakeGitOps) Resolve(_ context.Context, dir, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if head, ok := f.dirHeads[dir]; ok {
		return head, nil
	}
	return f.head, nil
}

func (f *fakeGitOps) AddWorktree(_ context.Context, _, dir, head string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addCalls++
	f.worktrees[dir] = true
	f.dirHeads[dir] = head
	return writeTestDir(dir)
}

func (f *fakeGitOps) RemoveWorktree(_ context.Context, _, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls++
	delete(f.worktrees, dir)
	delete(f.dirHeads, dir)
	return nil
}

func (f *fakeGitOps) Checkout(context.Context, string, string) error { return nil }

func (f *fakeGitOps) ChangedFiles(context.Context, string, string, string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.changed...), nil
}

func (f *fakeGitOps) IsAncestor(context.Context, string, string, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ancestor, nil
}

func (f *fakeGitOps) FileDiff(_ context.Context, _, _, _, path string) (*repo.FileDiff, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, changed := range f.changed {
		if changed == path {
			return &repo.FileDiff{OldPath: path, Deleted: true}, nil
		}
	}
	return nil, nil
}

func writeTestDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

type fakeAgentRunner struct {
	mu       sync.Mutex
	calls    int
	block    <-chan struct{}
	started  chan struct{}
	startOne sync.Once
}

func (f *fakeAgentRunner) Run(ctx context.Context, _ agent.RunSpec) (*agent.RunResult, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.started != nil {
		f.startOne.Do(func() { close(f.started) })
	}
	if f.block != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.block:
		}
	}
	return &agent.RunResult{
		Output: &review.CodeReviewOut{
			Summary: "one issue found",
			Findings: []review.FindingOut{{
				Path:       "main.go",
				Line:       2,
				Side:       "new",
				Severity:   review.SeverityP2,
				Category:   "correctness",
				Title:      "bad assignment",
				Body:       "the assignment is wrong",
				Evidence:   "bad := 1",
				Confidence: review.ConfidenceHigh,
			}},
		},
		Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
	}, nil
}

func (f *fakeAgentRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func testReviewProfile() config.ProfileConfig {
	return config.ProfileConfig{
		Propose: []config.ProposeConfig{
			{Block: "code-review", Provider: "fake"},
		},
		Challenge:   config.ChallengeConfig{Provider: "fake", Only: []string{"P0", "P1"}},
		OnUncertain: "mark",
		Synthesize:  config.SynthesizeConfig{Mode: "code"},
		Plan:        config.PlanConfig{Mode: "code", PackageTokens: 60000, MaxPackages: 6},
		Budget:      config.BudgetConfig{MaxNodes: 30, Deadline: 5 * time.Minute},
	}
}

type reviewFixture struct {
	db        *sqlite.Store
	gitlab    *reviewGitLab
	git       *fakeGitOps
	runner    *fakeAgentRunner
	service   *ReviewService
	engine    *dag.Engine
	executors *executorRegistry
	manager   *workspace.Manager
	publisher *Publisher
	profiles  map[string]config.ProfileConfig
	defaults  config.ProjectSettings
	bundle    prompts.Bundle
	head      string
}

func newReviewFixture(t *testing.T, head string) *reviewFixture {
	t.Helper()
	db := openTestStore(t)
	cipher := newTestCipher(t)
	seedProject(t, db, cipher, 42, "whsec", "{}", "default")
	api := newReviewGitLab(head)
	git := newFakeGitOps(head)
	runner := &fakeAgentRunner{}
	bundle, err := prompts.Load("")
	if err != nil {
		t.Fatalf("prompts.Load: %v", err)
	}
	profiles := map[string]config.ProfileConfig{"default": testReviewProfile()}
	defaults := testDefaults()
	defaults.AllowRepoConfig = true
	manager := workspace.NewManager(t.TempDir(), &repo.Git{})
	executors := newExecutorRegistry()
	engine := dag.NewEngine(db, executors, executors, dag.Options{Clock: func() time.Time { return time.Now().UTC() }})
	publisher, err := NewPublisher(PublisherConfig{
		Store:          db,
		GitLab:         api,
		Cipher:         cipher,
		Engine:         engine,
		ReviewDefaults: defaults,
		Profiles:       profiles,
		PromptsHash:    bundle.Hash,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	service, err := NewReviewService(ReviewServiceConfig{
		Store:            db,
		GitLab:           api,
		Cipher:           cipher,
		Workspace:        manager,
		Git:              git,
		Engine:           engine,
		Executors:        executors,
		Profiles:         profiles,
		ReviewDefaults:   defaults,
		Prompts:          bundle,
		AgentRun:         runner.Run,
		Publisher:        publisher,
		AllowedProviders: []string{"fake"},
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:              time.Now,
		CancelPoll:       10 * time.Millisecond,
		CancelWait:       2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewReviewService: %v", err)
	}
	return &reviewFixture{
		db: db, gitlab: api, git: git, runner: runner, service: service,
		engine: engine, executors: executors, manager: manager,
		publisher: publisher, profiles: profiles, defaults: defaults,
		bundle: bundle, head: head,
	}
}

// seedReviewControl enqueues a generation-aware review job and returns the
// job payload accepted by the handler.
func (f *reviewFixture) seedReview(t *testing.T, jobID string) *jobs.Job {
	t.Helper()
	result, err := f.db.EnqueueReviewLatest(context.Background(), store.ReviewCommand{
		ProjectID:   42,
		MRIID:       7,
		HeadSHA:     f.head,
		Payload:     json.RawMessage(`{"trigger":"mr_open","profile":"default","full":true}`),
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"project_id": 42,
		"mr_iid":     7,
		"head_sha":   f.head,
		"generation": result.Generation,
		"profile":    "default",
		"trigger":    "mr_open",
		"full":       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &jobs.Job{ID: jobID, Kind: "review.run", Payload: payload, MaxAttempts: 3}
}

func (f *reviewFixture) seedCancel(t *testing.T, jobID string) *jobs.Job {
	t.Helper()
	result, err := f.db.EnqueueReviewLatest(context.Background(), store.ReviewCommand{
		ProjectID:   42,
		MRIID:       7,
		Cancel:      true,
		Payload:     json.RawMessage(`{"trigger":"command"}`),
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest cancel: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"project_id": 42,
		"mr_iid":     7,
		"generation": result.Generation,
		"trigger":    "command",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &jobs.Job{ID: jobID, Kind: "review.cancel", Payload: payload, MaxAttempts: 3}
}

func mustJSONBody(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func testDossier(t *testing.T, head string) *review.Dossier {
	t.Helper()
	dossier, err := review.BuildDossier(review.DossierInput{
		Project: review.ProjectRef{ID: 42, Path: "group/repo"},
		MR: review.MRMeta{
			IID:          7,
			Title:        "Add feature",
			SourceBranch: "feature",
			TargetBranch: "main",
		},
		DiffVersion: gitlab.Version{
			ID:       1,
			BaseSHA:  strings.Repeat("1", 40),
			StartSHA: strings.Repeat("2", 40),
			HeadSHA:  head,
		},
		DiffSummary: review.DiffSummary{Files: 1, Additions: 1},
		Files: []review.FileChange{{
			Path:      "main.go",
			Status:    "modified",
			Diff:      "@@ -1,2 +1,3 @@\n a\n+bad := 1\n b\n",
			Additions: 1,
		}},
		GeneratedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("BuildDossier: %v", err)
	}
	return dossier
}

func testGraph(t *testing.T, dossier *review.Dossier) (dag.GraphSpec, string) {
	t.Helper()
	profile := review.Profile{Name: "default", Config: testReviewProfile()}
	spec, err := review.BuildGraph(profile, dossier, review.GraphConfig{AllowedProviders: []string{"fake"}})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	return spec, "default"
}

// currentReviewHashes returns the configuration and prompt hashes a fresh run
// of the fixture job would record.
func currentReviewHashes(t *testing.T, f *reviewFixture) (string, string) {
	t.Helper()
	project, err := f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	cfg := f.service.resolveConfig(project, review.RepoDocs{})
	return reviewConfigHash(cfg), f.bundle.Hash
}

func jobGeneration(t *testing.T, job *jobs.Job) int64 {
	t.Helper()
	var payload struct {
		Generation int64 `json:"generation"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Generation
}

func createPendingReview(t *testing.T, f *reviewFixture, jobID string, generation int64) string {
	t.Helper()
	project, err := f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	configHash := reviewConfigHash(f.service.resolveConfig(project, review.RepoDocs{}))
	dossier := testDossier(t, f.head)
	dossier.Metadata = map[string]string{"config_hash": configHash, "prompt_hash": f.bundle.Hash}
	raw, err := review.MarshalDossier(dossier)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := review.DossierHash(dossier)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := testGraph(t, dossier)
	wfID, _, err := f.db.CreateReviewWorkflow(context.Background(), sqlite.ReviewWorkflowRequest{
		JobID:              jobID,
		Spec:               spec,
		Meta:               map[string]string{"trigger": "mr_open", "base_sha": dossier.DiffVersion.BaseSHA, "fingerprint": reviewFingerprint(hash, configHash, f.bundle.Hash), "config_hash": configHash, "prompt_hash": f.bundle.Hash},
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
	return wfID
}
