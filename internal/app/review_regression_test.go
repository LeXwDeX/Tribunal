package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/workspace"
)

var errInjectedDossier = errors.New("injected dossier failure")

type recoverPageStore struct {
	*sqlite.Store

	page0   []store.Workflow
	page1   []store.Workflow
	cursors []string
	bad     map[string]bool
}

func (s *recoverPageStore) ListWorkflows(ctx context.Context, filter store.WorkflowFilter, cursor string, limit int) ([]store.Workflow, string, error) {
	if filter.Kind != "review" || filter.Status != "pending" {
		return s.Store.ListWorkflows(ctx, filter, cursor, limit)
	}
	s.cursors = append(s.cursors, cursor)
	switch cursor {
	case "":
		return append([]store.Workflow(nil), s.page0...), "page2", nil
	case "page2":
		return append([]store.Workflow(nil), s.page1...), "", nil
	default:
		return nil, "", fmt.Errorf("unexpected cursor %q", cursor)
	}
}

func (s *recoverPageStore) GetWorkflowDossier(ctx context.Context, workflowID string) (*store.WorkflowDossier, error) {
	if s.bad[workflowID] {
		return nil, errInjectedDossier
	}
	return s.Store.GetWorkflowDossier(ctx, workflowID)
}

func reviewInt64Ptr(v int64) *int64 { return &v }

func badReviewWorkflows(count int) ([]store.Workflow, map[string]bool) {
	out := make([]store.Workflow, 0, count)
	bad := make(map[string]bool, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("bad-%02d", i)
		out = append(out, store.Workflow{
			ID:        id,
			Kind:      "review",
			ProjectID: 42,
			MRIID:     reviewInt64Ptr(7),
			Status:    "pending",
		})
		bad[id] = true
	}
	return out, bad
}

// TestRecoverPaginatesAfterFailedPage keeps the first page pending while a
// later page holds a recoverable workflow. The old loop re-read page one and
// stopped, so the older workflow was never recovered.
func TestRecoverPaginatesAfterFailedPage(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-recover-pages")
	generation := jobGeneration(t, job)

	backID := createPendingReview(t, f, "job-recover-back", generation)
	back, err := f.db.GetWorkflow(context.Background(), backID)
	if err != nil {
		t.Fatal(err)
	}

	bad, badIDs := badReviewWorkflows(50)
	pageStore := &recoverPageStore{
		Store: f.db,
		page0: bad,
		page1: []store.Workflow{*back},
		bad:   badIDs,
	}
	f.service.store = pageStore

	err = f.service.Recover(context.Background())
	if !errors.Is(err, errInjectedDossier) {
		t.Fatalf("Recover error = %v, want the pending page failure", err)
	}
	sawPage2 := false
	for _, cursor := range pageStore.cursors {
		if cursor == "page2" {
			sawPage2 = true
		}
	}
	if !sawPage2 {
		t.Fatalf("Recover cursors = %q, want a page2 request", pageStore.cursors)
	}
	workflow, err := f.db.GetWorkflow(context.Background(), backID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Status != "completed" {
		t.Fatalf("workflow %s status = %q, want completed", backID, workflow.Status)
	}
	for id := range badIDs {
		if _, err := f.db.GetWorkflow(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("pending workflow %s was persisted or recovered: %v", id, err)
		}
	}
}

type canceledPageStore struct {
	*sqlite.Store

	cancel  context.CancelFunc
	lists   int
	dossier int
	bad     map[string]bool
}

func (s *canceledPageStore) ListWorkflows(ctx context.Context, filter store.WorkflowFilter, cursor string, limit int) ([]store.Workflow, string, error) {
	if filter.Kind != "review" || filter.Status != "pending" {
		return s.Store.ListWorkflows(ctx, filter, cursor, limit)
	}
	s.lists++
	s.cancel()
	page := make([]store.Workflow, 0, 50)
	for id := range s.bad {
		page = append(page, store.Workflow{
			ID:        id,
			Kind:      "review",
			ProjectID: 42,
			MRIID:     reviewInt64Ptr(7),
			Status:    "pending",
		})
	}
	return page, "next", nil
}

func (s *canceledPageStore) GetWorkflowDossier(ctx context.Context, workflowID string) (*store.WorkflowDossier, error) {
	s.dossier++
	return nil, errInjectedDossier
}

func TestRecoverStopsOnContextCancel(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, badIDs := badReviewWorkflows(50)
	cancelStore := &canceledPageStore{Store: f.db, cancel: cancel, bad: badIDs}
	f.service.store = cancelStore

	err := f.service.Recover(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Recover error = %v, want context.Canceled", err)
	}
	if cancelStore.lists != 1 {
		t.Fatalf("ListWorkflows calls = %d, want 1", cancelStore.lists)
	}
	if cancelStore.dossier != 0 {
		t.Fatalf("recovered %d workflows after cancellation", cancelStore.dossier)
	}
}

type countingGitOps struct {
	*fakeGitOps
	ensureCalls int
}

func (g *countingGitOps) EnsureMirror(ctx context.Context, dir, remote string, auth repo.Auth) error {
	g.ensureCalls++
	return g.fakeGitOps.EnsureMirror(ctx, dir, remote, auth)
}

func assertOutsideWorkspaceUntouched(t *testing.T, outside string, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Lstat(filepath.Join(outside, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s was created outside the workspace: %v", name, err)
		}
	}
}

func TestPrepareRunRejectsSymlinkedWorkspaceRoot(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	project, err := f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	ws := f.manager.Get(project.ID)
	outside := t.TempDir()
	if err := os.Symlink(outside, ws.Root); err != nil {
		t.Fatal(err)
	}
	git := &countingGitOps{fakeGitOps: f.git}
	f.service.git = git

	dir, cleanup, err := f.service.prepareRun(context.Background(), project, "token", 7, "job-symlink-root", f.head)
	if err == nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatal("prepareRun accepted a symlinked workspace root")
	}
	if dir != "" || cleanup != nil {
		t.Fatalf("prepareRun returned dir %q cleanup %v after rejection", dir, cleanup != nil)
	}
	if git.ensureCalls != 0 {
		t.Fatalf("EnsureMirror calls = %d, want 0 before path validation", git.ensureCalls)
	}
	assertOutsideWorkspaceUntouched(t, outside, "42", "mirror.git", "runs")
}

func TestPrepareRunRejectsSymlinkedWorkspaceParent(t *testing.T) {
	cases := []struct {
		name          string
		existingChild bool
	}{
		{name: "missing project root"},
		{name: "existing project root", existingChild: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			project, err := f.db.GetProject(context.Background(), 42)
			if err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if tc.existingChild {
				if err := os.MkdirAll(filepath.Join(outside, "42"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			parent := filepath.Join(t.TempDir(), "workspaces")
			if err := os.Symlink(outside, parent); err != nil {
				t.Fatal(err)
			}
			f.service.workspace = workspace.NewManager(parent, &repo.Git{})
			git := &countingGitOps{fakeGitOps: f.git}
			f.service.git = git

			dir, cleanup, err := f.service.prepareRun(context.Background(), project, "token", 7, "job-symlink-parent", f.head)
			if err == nil {
				if cleanup != nil {
					cleanup()
				}
				t.Fatal("prepareRun accepted a symlinked workspace parent")
			}
			if dir != "" || cleanup != nil {
				t.Fatalf("prepareRun returned dir %q cleanup %v after rejection", dir, cleanup != nil)
			}
			if git.ensureCalls != 0 {
				t.Fatalf("EnsureMirror calls = %d, want 0 before path validation", git.ensureCalls)
			}
			assertOutsideWorkspaceUntouched(t, outside, "mirror.git", "runs", "42/mirror.git", "42/runs")
			if !tc.existingChild {
				if _, err := os.Lstat(filepath.Join(outside, "42")); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("project root was created outside the workspace: %v", err)
				}
			}
		})
	}
}

func TestPublishEmptyAcceptsUppercaseHead(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.version.HeadSHA = strings.ToUpper(f.head)
	f.gitlab.diffs = []gitlab.FileDiff{{NewPath: "go.sum", Diff: "@@ -1 +1 @@\n-a\n+b\n"}}
	f.defaults.Ignore = []string{"**/*.sum"}
	f.service.reviewDefaults = f.defaults
	job := f.seedReview(t, "job-empty-upper")

	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if got := f.gitlab.noteCount(); got != 1 {
		t.Fatalf("notes = %d, want one empty summary", got)
	}
}
