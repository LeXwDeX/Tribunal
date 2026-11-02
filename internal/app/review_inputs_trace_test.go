package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
)

func TestCollectPipelineFetchesOnlyFailedJobTraces(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.mu.Lock()
	f.gitlab.pipelines = []gitlab.Pipeline{{ID: 10, Status: "failed", Ref: "feat", SHA: reviewTestHead}}
	f.gitlab.jobs = []gitlab.Job{
		{ID: 1, Name: "unit", Stage: "test", Status: "failed", FailureReason: "script_failure"},
		{ID: 2, Name: "lint", Stage: "test", Status: "success"},
		{ID: 3, Name: "optional", Stage: "test", Status: "failed", AllowFailure: true},
	}
	f.gitlab.jobTraces = map[int64]string{1: "panic at main.go:3"}
	f.gitlab.mu.Unlock()

	summary := f.service.collectPipeline(context.Background(), "token", 42, 7, reviewTestHead)
	if summary == nil || len(summary.FailedJobs) != 1 {
		t.Fatalf("summary = %+v, want one failed job", summary)
	}
	job := summary.FailedJobs[0]
	if job.ID != 1 || job.Trace != "panic at main.go:3" {
		t.Fatalf("job = %+v, want ID and trace", job)
	}
	if got := f.gitlab.traceCallIDs(); fmt.Sprint(got) != "[1]" {
		t.Fatalf("trace calls = %v, want only the failed job", got)
	}
}

func TestCollectPipelineLimitsJobTracesToThree(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	jobs := make([]gitlab.Job, 0, 5)
	traces := make(map[int64]string, 5)
	for id := int64(1); id <= 5; id++ {
		jobs = append(jobs, gitlab.Job{ID: id, Name: fmt.Sprintf("job-%d", id), Status: "failed"})
		traces[id] = fmt.Sprintf("trace-%d", id)
	}
	f.gitlab.mu.Lock()
	f.gitlab.pipelines = []gitlab.Pipeline{{ID: 10, Status: "failed", SHA: reviewTestHead}}
	f.gitlab.jobs = jobs
	f.gitlab.jobTraces = traces
	f.gitlab.mu.Unlock()

	summary := f.service.collectPipeline(context.Background(), "token", 42, 7, reviewTestHead)
	if summary == nil || len(summary.FailedJobs) != 5 {
		t.Fatalf("summary = %+v, want all five failed jobs", summary)
	}
	if got := f.gitlab.traceCallIDs(); len(got) != 3 {
		t.Fatalf("trace calls = %v, want three", got)
	}
	for i, job := range summary.FailedJobs {
		if i < 3 && job.Trace != fmt.Sprintf("trace-%d", i+1) {
			t.Fatalf("job %d trace = %q, want a trace", i, job.Trace)
		}
		if i >= 3 && job.Trace != "" {
			t.Fatalf("job %d trace = %q, want no trace after the cap", i, job.Trace)
		}
	}
}

func TestCollectPipelineContinuesWhenTraceReadFails(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	var logs bytes.Buffer
	f.service.logger = slog.New(slog.NewTextHandler(&logs, nil))
	f.gitlab.mu.Lock()
	f.gitlab.pipelines = []gitlab.Pipeline{{ID: 10, Status: "failed", SHA: reviewTestHead}}
	f.gitlab.jobs = []gitlab.Job{
		{ID: 1, Name: "unit", Status: "failed"},
		{ID: 2, Name: "integration", Status: "failed"},
	}
	f.gitlab.jobTraces = map[int64]string{2: "second trace"}
	f.gitlab.jobTraceErrs = map[int64]error{1: errors.New("upstream secret body")}
	f.gitlab.mu.Unlock()

	summary := f.service.collectPipeline(context.Background(), "token", 42, 7, reviewTestHead)
	if summary == nil || len(summary.FailedJobs) != 2 {
		t.Fatalf("summary = %+v, want both failed jobs", summary)
	}
	if summary.FailedJobs[0].Trace != "" || summary.FailedJobs[1].Trace != "second trace" {
		t.Fatalf("summary = %+v, want failure skipped and next trace kept", summary)
	}
	logged := logs.String()
	if !strings.Contains(logged, "job_id=1") || !strings.Contains(logged, "pipeline_id=10") {
		t.Fatalf("warning does not identify the job: %s", logged)
	}
	if strings.Contains(logged, "upstream secret body") {
		t.Fatalf("warning contains the raw trace error: %s", logged)
	}
}

func TestCollectPipelineSkipsTraceForDifferentHead(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.mu.Lock()
	f.gitlab.pipelines = []gitlab.Pipeline{{ID: 10, Status: "failed", SHA: strings.Repeat("a", 40)}}
	f.gitlab.jobs = []gitlab.Job{{ID: 1, Name: "unit", Status: "failed"}}
	f.gitlab.jobTraces = map[int64]string{1: "stale trace"}
	f.gitlab.mu.Unlock()

	summary := f.service.collectPipeline(context.Background(), "token", 42, 7, reviewTestHead)
	if summary == nil || len(summary.FailedJobs) != 1 {
		t.Fatalf("summary = %+v, want one failed job for another commit", summary)
	}
	if summary.FailedJobs[0].Trace != "" {
		t.Fatalf("trace = %q, want empty for another commit", summary.FailedJobs[0].Trace)
	}
	if got := f.gitlab.traceCallIDs(); len(got) != 0 {
		t.Fatalf("trace calls = %v, want none for another commit", got)
	}
}

func TestSanitizeJobTraceRedactsSecrets(t *testing.T) {
	raw := strings.Join([]string{
		"token glpat-abcdef123456 used",
		"PRIVATE-TOKEN: private-value",
		"Authorization: Bearer bearer-value",
		"remote https://user:pass@gitlab.example.com/group/repo.git",
		"CI_JOB_TOKEN=job-token-value",
		"GITLAB_TOKEN=gitlab-token-value",
		"glrt-runner-token",
		"DB_PASSWORD=db-pass",
		"AWS_SECRET_ACCESS_KEY=aws-secret",
		"MY_API_KEY=api-key-value",
		"Authorization: Basic basic-value",
		"deploy --token flag-token",
		"CI_JOB_JWT=jwt-value",
		"SSH_PRIVATE_KEY=-----BEGIN OPENSSH PRIVATE KEY-----",
		"pem-body-value",
		"-----END OPENSSH PRIVATE KEY-----",
		"curl -u deploy:hunter2 https://example.com",
		"curl --user=deploy:hunter3 https://example.com",
		"curl -udeploy:hunter4 https://example.com",
		"curl -u $DEPLOY_TOKEN https://example.com",
		"curl -u user: https://example.com",
		"curl -u :pass https://example.com",
		"Authorization: OAuth oauth-value",
		"Authorization: JWT auth-jwt-value",
		"git diff -u a.go",
		"redis://:redis-pass@cache:6379",
		`{"private_token":"json-token-value"}`,
		`{"authorization":"Bearer json-auth-value"}`,
		`{"client-secret":"json-secret-value"}`,
		`{"access-token":"json-access-value"}`,
		`{"x-api-key":"json-api-key"}`,
		`{"auth.token":"json-dot-token"}`,
		"normal line stays",
	}, "\n")

	got := sanitizeJobTrace(raw)
	for _, secret := range []string{
		"glpat-abcdef123456", "private-value", "bearer-value", "user:pass", "job-token-value", "gitlab-token-value",
		"glrt-runner-token", "db-pass", "aws-secret", "api-key-value", "basic-value", "flag-token",
		"jwt-value", "pem-body-value", "deploy:hunter2", "deploy:hunter3", "deploy:hunter4", "redis-pass",
		"json-token-value", "json-auth-value", "json-secret-value", "json-access-value", "json-api-key", "json-dot-token",
		"DEPLOY_TOKEN", "user:", ":pass", "oauth-value", "auth-jwt-value",
	} {
		if strings.Contains(got, secret) {
			t.Fatalf("sanitized trace still contains %q:\n%s", secret, got)
		}
	}
	if strings.Count(got, "[REDACTED]") < 29 {
		t.Fatalf("sanitized trace misses redaction markers:\n%s", got)
	}
	if !strings.Contains(got, "normal line stays") {
		t.Fatalf("sanitized trace dropped ordinary content:\n%s", got)
	}
	if !strings.Contains(got, "git diff -u a.go") {
		t.Fatalf("sanitized trace over-redacted a plain -u argument:\n%s", got)
	}
}
