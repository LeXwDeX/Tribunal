package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/workspace"
)

const feedbackRepoFile = "main.go"

func TestFeedbackResolveSignalUsesPublishedLine(t *testing.T) {
	tests := []struct {
		name   string
		line   int
		side   string
		mutate func(t *testing.T, dir string)
		want   string
	}{
		{
			name: "replaced finding line",
			line: 3,
			side: "new",
			mutate: func(t *testing.T, dir string) {
				writeFeedbackFile(t, dir, "one\ntwo\nthree fixed\nfour\nfive\nsix\n")
			},
			want: "resolved_changed",
		},
		{
			name: "same file other line",
			line: 3,
			side: "new",
			mutate: func(t *testing.T, dir string) {
				writeFeedbackFile(t, dir, "one fixed\ntwo\nthree\nfour\nfive\nsix\n")
			},
			want: "resolved_unchanged",
		},
		{
			name: "insertion before finding line",
			line: 3,
			side: "new",
			mutate: func(t *testing.T, dir string) {
				writeFeedbackFile(t, dir, "one\ntwo\ninserted\nthree\nfour\nfive\nsix\n")
			},
			want: "resolved_unchanged",
		},
		{
			name: "delete finding line",
			line: 3,
			side: "new",
			mutate: func(t *testing.T, dir string) {
				writeFeedbackFile(t, dir, "one\ntwo\nfour\nfive\nsix\n")
			},
			want: "resolved_changed",
		},
		{
			name: "pure rename",
			line: 3,
			side: "new",
			mutate: func(t *testing.T, dir string) {
				if err := os.Rename(filepath.Join(dir, feedbackRepoFile), filepath.Join(dir, "renamed.go")); err != nil {
					t.Fatal(err)
				}
			},
			want: "resolved_unchanged",
		},
		{
			name: "rename and edit other line",
			line: 3,
			side: "new",
			mutate: func(t *testing.T, dir string) {
				if err := os.Rename(filepath.Join(dir, feedbackRepoFile), filepath.Join(dir, "renamed.go")); err != nil {
					t.Fatal(err)
				}
				writeFeedbackFileNamed(t, dir, "renamed.go", "one\ntwo fixed\nthree\nfour\nfive\nsix\n")
			},
			want: "resolved_unchanged",
		},
		{
			name: "rename and replace finding line",
			line: 3,
			side: "new",
			mutate: func(t *testing.T, dir string) {
				if err := os.Rename(filepath.Join(dir, feedbackRepoFile), filepath.Join(dir, "renamed.go")); err != nil {
					t.Fatal(err)
				}
				writeFeedbackFileNamed(t, dir, "renamed.go", "one\ntwo\nthree fixed\nfour\nfive\nsix\n")
			},
			want: "resolved_changed",
		},
		{
			name: "deleted finding file",
			line: 3,
			side: "new",
			mutate: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, feedbackRepoFile)); err != nil {
					t.Fatal(err)
				}
			},
			want: "resolved_changed",
		},
		{
			name: "old side has no published head coordinate",
			line: 3,
			side: "old",
			mutate: func(t *testing.T, dir string) {
				writeFeedbackFile(t, dir, "one\ntwo\nthree fixed\nfour\nfive\nsix\n")
			},
			want: "",
		},
		{
			name:   "missing line has no evidence",
			line:   0,
			side:   "new",
			mutate: func(t *testing.T, dir string) { writeFeedbackFile(t, dir, "one fixed\ntwo\nthree\nfour\nfive\nsix\n") },
			want:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			published, current, root := feedbackRepo(t, tc.mutate)
			f, service, workflow, publication := feedbackResolveFixture(t,
				published, current, root, "job-feedback-"+strings.ReplaceAll(tc.name, " ", "-"), tc.line, tc.side)
			project, err := f.db.GetProject(context.Background(), 42)
			if err != nil {
				t.Fatalf("GetProject: %v", err)
			}
			got := service.resolveSignal(context.Background(), "token", project, publication, *workflow, &gitlab.MergeRequest{
				IID: 7, State: "opened", SHA: current,
			}, 7)
			if got != tc.want {
				t.Fatalf("resolveSignal() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFeedbackResolveSignalFetchesMissingHead(t *testing.T) {
	published, current, root, remote := feedbackHTTPRepo(t, func(t *testing.T, dir string) {
		writeFeedbackFile(t, dir, "one\ntwo\nthree fixed\nfour\nfive\nsix\n")
	})
	f, service, workflow, publication := feedbackResolveFixture(t,
		published, current, root, "job-feedback-fetch", 3, "new")
	project, err := f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	project.WebURL = remote
	got := service.resolveSignal(context.Background(), "token", project, publication, *workflow, &gitlab.MergeRequest{
		IID: 7, State: "opened", SHA: current,
	}, 7)
	if got != "resolved_changed" {
		t.Fatalf("resolveSignal() = %q, want resolved_changed", got)
	}
	resolved, err := (&repo.Git{}).Resolve(context.Background(), filepath.Join(root, "42", "mirror.git"), "refs/merge-requests/7/head")
	if err != nil || resolved != current {
		t.Fatalf("fetched merge request head = %q, %v; want %q", resolved, err, current)
	}
}

func TestFeedbackEvidenceFailureRecordsNothing(t *testing.T) {
	_, published, current, root := feedbackRepoPublishedOnly(t, func(t *testing.T, dir string) {
		writeFeedbackFile(t, dir, "one\ntwo\nthree fixed\nfour\nfive\nsix\n")
	})
	f, service, workflow, publication := feedbackResolveFixture(t,
		published, current, root, "job-feedback-evidence-failure", 3, "new")
	f.gitlab.mu.Lock()
	f.gitlab.discussions[0].Notes[0].Resolved = true
	f.gitlab.mu.Unlock()
	project, err := f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	project.WebURL = "http://127.0.0.1:1/group/repo.git"
	service.collectDiscussion(context.Background(), "token", project, publication, *workflow, &gitlab.MergeRequest{
		IID: 7, State: "opened", SHA: current,
	}, 7)
	projectID := int64(42)
	feedback, _, err := f.db.ListFeedback(context.Background(), store.FeedbackFilter{ProjectID: &projectID})
	if err != nil {
		t.Fatalf("ListFeedback: %v", err)
	}
	if len(feedback) != 0 {
		t.Fatalf("feedback rows = %+v, want none when evidence is unavailable", feedback)
	}
}

func TestFeedbackEvidenceFailureRetriesNextPass(t *testing.T) {
	source, published, current, root := feedbackRepoPublishedOnly(t, func(t *testing.T, dir string) {
		writeFeedbackFile(t, dir, "one\ntwo\nthree fixed\nfour\nfive\nsix\n")
	})
	f, service, workflow, publication := feedbackResolveFixture(t,
		published, current, root, "job-feedback-retry", 3, "new")
	f.gitlab.mu.Lock()
	f.gitlab.discussions[0].Notes[0].Resolved = true
	f.gitlab.mu.Unlock()
	project, err := f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	project.WebURL = "http://127.0.0.1:1/group/repo.git"
	mr := &gitlab.MergeRequest{IID: 7, State: "opened", SHA: current}
	service.collectDiscussion(context.Background(), "token", project, publication, *workflow, mr, 7)
	projectID := int64(42)
	feedback, _, err := f.db.ListFeedback(context.Background(), store.FeedbackFilter{ProjectID: &projectID})
	if err != nil || len(feedback) != 0 {
		t.Fatalf("feedback after missing evidence = %+v, %v", feedback, err)
	}

	feedbackGitRun(t, source, "push", "-q", filepath.Join(root, "42", "mirror.git"), current+":refs/heads/main")
	service.collectDiscussion(context.Background(), "token", project, publication, *workflow, mr, 7)
	feedback, _, err = f.db.ListFeedback(context.Background(), store.FeedbackFilter{ProjectID: &projectID, Signal: "resolved_changed"})
	if err != nil || len(feedback) != 1 {
		t.Fatalf("feedback after retry = %+v, %v", feedback, err)
	}
}

func feedbackResolveFixture(t *testing.T, published, current, root, jobID string, line int, side string) (*reviewFixture, *FeedbackService, *store.Workflow, store.Publication) {
	t.Helper()
	f := newReviewFixture(t, published)
	job := f.seedReview(t, jobID)
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, publication := feedbackPublication(t, f)
	findings := feedbackFindings(t, f, workflow.ID, publication.PublishKey)
	if line <= 0 {
		findings[0].Line = nil
	} else {
		findings[0].Line = &line
	}
	findings[0].Side = side
	findings[0].Path = feedbackRepoFile
	if err := f.db.ReplaceFindings(context.Background(), workflow.ID, findings); err != nil {
		t.Fatalf("ReplaceFindings: %v", err)
	}
	f.gitlab.mu.Lock()
	f.gitlab.mr.SHA = current
	f.gitlab.mu.Unlock()
	return f, newFeedbackServiceForRepo(t, f, root), workflow, publication
}

func TestFeedbackCollectsMergedMROnlyWithinWindow(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name     string
		state    string
		mergedAt *time.Time
		want     int
	}{
		{name: "merged within three days", state: "merged", mergedAt: feedbackTimePtr(now.Add(-time.Hour)), want: 1},
		{name: "merged older than three days", state: "merged", mergedAt: feedbackTimePtr(now.Add(-4 * 24 * time.Hour)), want: 0},
		{name: "merged without timestamp", state: "merged", mergedAt: nil, want: 0},
		{name: "opened", state: "opened", mergedAt: nil, want: 1},
		{name: "closed", state: "closed", mergedAt: nil, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			job := f.seedReview(t, "job-feedback-merged-"+strings.ReplaceAll(tc.name, " ", "-"))
			if err := f.service.Handle(context.Background(), job); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			f.gitlab.mu.Lock()
			summaryID := f.gitlab.notes[0].ID
			f.gitlab.awards[summaryID] = []gitlab.Award{{ID: 1, Name: "thumbsup", UserID: 5}}
			f.gitlab.mr.State = tc.state
			f.gitlab.mr.MergedAt = tc.mergedAt
			f.gitlab.mu.Unlock()

			service := newFeedbackService(t, f, &repo.Git{}, f.manager)
			service.now = func() time.Time { return now }
			service.RunOnce(context.Background())
			projectID := int64(42)
			feedback, _, err := f.db.ListFeedback(context.Background(), store.FeedbackFilter{ProjectID: &projectID})
			if err != nil {
				t.Fatalf("ListFeedback: %v", err)
			}
			if len(feedback) != tc.want {
				t.Fatalf("feedback rows = %d, want %d: %+v", len(feedback), tc.want, feedback)
			}
		})
	}
}

func newFeedbackService(t *testing.T, f *reviewFixture, git GitOps, manager *workspace.Manager) *FeedbackService {
	t.Helper()
	service, err := NewFeedbackService(FeedbackServiceConfig{
		Store:     f.db,
		GitLab:    f.gitlab,
		Cipher:    newTestCipher(t),
		Git:       git,
		Workspace: manager,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewFeedbackService: %v", err)
	}
	return service
}

func newFeedbackServiceForRepo(t *testing.T, f *reviewFixture, root string) *FeedbackService {
	t.Helper()
	git := &repo.Git{}
	return newFeedbackService(t, f, git, workspace.NewManager(root, git))
}

func feedbackPublication(t *testing.T, f *reviewFixture) (*store.Workflow, store.Publication) {
	t.Helper()
	ctx := context.Background()
	workflows, _, err := f.db.ListWorkflows(ctx, store.WorkflowFilter{Kind: "review", Status: "completed"}, "", 10)
	if err != nil || len(workflows) != 1 {
		t.Fatalf("ListWorkflows = %d, %v", len(workflows), err)
	}
	workflow := workflows[0]
	publications, err := f.db.ListPublications(ctx, workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, publication := range publications {
		if publication.Kind == discussionPublishKind {
			return &workflow, publication
		}
	}
	t.Fatal("discussion publication not found")
	return nil, store.Publication{}
}

func feedbackFindings(t *testing.T, f *reviewFixture, workflowID, publishKey string) []store.Finding {
	t.Helper()
	findings, err := f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: workflowID})
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding.PublishKey == publishKey {
			return []store.Finding{finding}
		}
	}
	t.Fatalf("finding for publish key %q not found", publishKey)
	return nil
}

func feedbackRepo(t *testing.T, mutate func(t *testing.T, dir string)) (string, string, string) {
	t.Helper()
	_, published, current, root := buildFeedbackRepo(t, mutate, false)
	return published, current, root
}

func feedbackRepoPublishedOnly(t *testing.T, mutate func(t *testing.T, dir string)) (string, string, string, string) {
	t.Helper()
	return buildFeedbackRepo(t, mutate, true)
}

func buildFeedbackRepo(t *testing.T, mutate func(t *testing.T, dir string), publishOnly bool) (string, string, string, string) {
	t.Helper()
	source := t.TempDir()
	feedbackGitRun(t, source, "init", "-q", "-b", "main")
	feedbackGitRun(t, source, "config", "user.email", "test@example.com")
	feedbackGitRun(t, source, "config", "user.name", "Test")
	writeFeedbackFile(t, source, "one\ntwo\nthree\nfour\nfive\nsix\n")
	feedbackGitRun(t, source, "add", feedbackRepoFile)
	feedbackGitRun(t, source, "commit", "-q", "-m", "published")
	published := strings.TrimSpace(feedbackGitRun(t, source, "rev-parse", "HEAD"))

	mutate(t, source)
	feedbackGitRun(t, source, "add", "-A")
	feedbackGitRun(t, source, "commit", "-q", "-m", "current")
	current := strings.TrimSpace(feedbackGitRun(t, source, "rev-parse", "HEAD"))

	root := t.TempDir()
	mirror := filepath.Join(root, "42", "mirror.git")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
		t.Fatal(err)
	}
	feedbackGitRun(t, source, "init", "--bare", "-q", mirror)
	ref := "HEAD"
	if publishOnly {
		ref = published
	}
	feedbackGitRun(t, source, "push", "-q", mirror, ref+":refs/heads/main")
	return source, published, current, root
}

func feedbackHTTPRepo(t *testing.T, mutate func(t *testing.T, dir string)) (string, string, string, string) {
	t.Helper()
	source := t.TempDir()
	feedbackGitRun(t, source, "init", "-q", "-b", "main")
	feedbackGitRun(t, source, "config", "user.email", "test@example.com")
	feedbackGitRun(t, source, "config", "user.name", "Test")
	writeFeedbackFile(t, source, "one\ntwo\nthree\nfour\nfive\nsix\n")
	feedbackGitRun(t, source, "add", feedbackRepoFile)
	feedbackGitRun(t, source, "commit", "-q", "-m", "published")
	published := strings.TrimSpace(feedbackGitRun(t, source, "rev-parse", "HEAD"))
	mutate(t, source)
	feedbackGitRun(t, source, "add", "-A")
	feedbackGitRun(t, source, "commit", "-q", "-m", "current")
	current := strings.TrimSpace(feedbackGitRun(t, source, "rev-parse", "HEAD"))

	serverRoot := t.TempDir()
	served := filepath.Join(serverRoot, "repo.git")
	feedbackGitRun(t, "", "clone", "--bare", "-q", source, served)
	feedbackGitRun(t, served, "fetch", "-q", source, current+":refs/merge-requests/7/head")
	feedbackGitRun(t, served, "update-server-info")
	server := httptest.NewServer(http.FileServer(http.Dir(serverRoot)))
	t.Cleanup(server.Close)

	root := t.TempDir()
	mirror := filepath.Join(root, "42", "mirror.git")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
		t.Fatal(err)
	}
	feedbackGitRun(t, source, "init", "--bare", "-q", mirror)
	feedbackGitRun(t, source, "push", "-q", mirror, published+":refs/heads/main")
	return published, current, root, server.URL + "/repo.git"
}

func writeFeedbackFile(t *testing.T, dir, body string) {
	t.Helper()
	writeFeedbackFileNamed(t, dir, feedbackRepoFile, body)
}

func writeFeedbackFileNamed(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func feedbackGitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
	)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func feedbackTimePtr(value time.Time) *time.Time { return &value }
