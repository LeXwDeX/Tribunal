package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// indexTrueCommand is an executable that exits successfully without touching
// the working tree. macOS has no /bin/true.
const indexTrueCommand = "/usr/bin/true"

func onboardingIndexRunner(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
	out := review.OnboardingOut{
		Overview:     "overview",
		Architecture: "architecture",
		Modules:      []review.Module{{Path: "internal", Purpose: "core"}},
		Conventions:  []string{"testing"},
		BuildAndTest: []string{"go test ./..."},
		RiskHotspots: []string{"none"},
		ReviewFocus:  []string{"contracts"},
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	decoded, err := spec.Submit.Decode(raw)
	if err != nil {
		return nil, err
	}
	return &agent.RunResult{Output: decoded}, nil
}

func indexTestManager(t *testing.T, commands []string) *mcp.Manager {
	t.Helper()
	server := mcp.ServerConfig{Command: indexTrueCommand}
	if commands != nil {
		server.XReview.IndexCommand = append([]string(nil), commands...)
	}
	manager, err := mcp.New(mcp.Options{
		Workspace: t.TempDir(),
		Project:   mcp.Config{MCPServers: map[string]mcp.ServerConfig{"indexer": server}},
	})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func setIndexFactory(service *WorkspaceService, manager *mcp.Manager) {
	service.mcpFactory = func(context.Context, int64) (*mcp.Manager, error) {
		return manager, nil
	}
}

func unrestrictedIndexService(t *testing.T, f *reviewFixture) *WorkspaceService {
	t.Helper()
	service := newOnboardingContextService(t, f, onboardingIndexRunner)
	service.reviewDefaults.Ignore = nil
	return service
}

func TestOnboardingOnlyRecordsIndexAfterCommandRuns(t *testing.T) {
	cases := []struct {
		name      string
		manager   func(*testing.T) *mcp.Manager
		wantIndex string
		wantError string
	}{
		{
			name:    "no manager",
			manager: func(*testing.T) *mcp.Manager { return nil },
		},
		{
			name: "no configured index command",
			manager: func(t *testing.T) *mcp.Manager {
				return indexTestManager(t, nil)
			},
		},
		{
			name: "configured index command succeeds",
			manager: func(t *testing.T) *mcp.Manager {
				return indexTestManager(t, []string{indexTrueCommand})
			},
			wantIndex: reviewTestHead,
		},
		{
			name: "configured index command fails",
			manager: func(t *testing.T) *mcp.Manager {
				return indexTestManager(t, []string{"/definitely/not/a/command"})
			},
			// A missing or broken indexer degrades onboarding instead of
			// failing it: context documents are still generated and the
			// workspace records a fixed, non-sensitive error.
			wantError: "index: code graph indexing failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newReviewFixture(t, reviewTestHead)
			service := unrestrictedIndexService(t, f)
			setIndexFactory(service, tc.manager(t))

			err := service.Onboard(context.Background(), 42)
			if err != nil {
				t.Fatalf("Onboard: %v", err)
			}
			state, stateErr := f.db.GetWorkspace(context.Background(), 42)
			if stateErr != nil {
				t.Fatalf("GetWorkspace: %v", stateErr)
			}
			if state.IndexSHA != tc.wantIndex {
				t.Fatalf("index sha = %q, want %q", state.IndexSHA, tc.wantIndex)
			}
			if state.State != "ready" {
				t.Fatalf("workspace state = %q, want ready", state.State)
			}
			if tc.wantError == "" && state.LastError != "" {
				t.Fatalf("last error = %q, want none", state.LastError)
			}
			if state.LastError != tc.wantError {
				t.Fatalf("last error = %q, want %q", state.LastError, tc.wantError)
			}
		})
	}
}

func TestRefreshDoesNotRecordIndexWithoutCommand(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	oldBase := strings.Repeat("a", 40)
	if _, err := f.db.SaveWorkspace(context.Background(), store.Workspace{
		ProjectID: 42, State: "ready", BaseSHA: oldBase, IndexSHA: oldBase,
	}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	service := unrestrictedIndexService(t, f)
	setIndexFactory(service, indexTestManager(t, nil))

	if err := service.RefreshWithOptions(context.Background(), 42, "main", WorkspaceRefreshOptions{
		Reindex:           contextBoolPtr(true),
		RegenerateContext: contextBoolPtr(false),
	}); err != nil {
		t.Fatalf("RefreshWithOptions: %v", err)
	}
	state, err := f.db.GetWorkspace(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if state.BaseSHA != reviewTestHead {
		t.Fatalf("base sha = %q, want %q", state.BaseSHA, reviewTestHead)
	}
	if state.IndexSHA != oldBase {
		t.Fatalf("index sha = %q, want the previous index %q", state.IndexSHA, oldBase)
	}
}

func TestRefreshRecordsIndexWhenCommandRuns(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	oldBase := strings.Repeat("a", 40)
	if _, err := f.db.SaveWorkspace(context.Background(), store.Workspace{
		ProjectID: 42, State: "ready", BaseSHA: oldBase, IndexSHA: oldBase,
	}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	service := unrestrictedIndexService(t, f)
	setIndexFactory(service, indexTestManager(t, []string{indexTrueCommand}))

	if err := service.RefreshWithOptions(context.Background(), 42, "main", WorkspaceRefreshOptions{
		Reindex:           contextBoolPtr(true),
		RegenerateContext: contextBoolPtr(false),
	}); err != nil {
		t.Fatalf("RefreshWithOptions: %v", err)
	}
	state, err := f.db.GetWorkspace(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if state.IndexSHA != reviewTestHead {
		t.Fatalf("index sha = %q, want %q after the configured command ran", state.IndexSHA, reviewTestHead)
	}
}

func TestRefreshKeepsPreviousIndexWhenIndexerFails(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	oldBase := strings.Repeat("a", 40)
	if _, err := f.db.SaveWorkspace(context.Background(), store.Workspace{
		ProjectID: 42, State: "ready", BaseSHA: oldBase, IndexSHA: oldBase,
	}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	service := unrestrictedIndexService(t, f)
	setIndexFactory(service, indexTestManager(t, []string{"/definitely/not/a/command"}))

	if err := service.RefreshWithOptions(context.Background(), 42, "main", WorkspaceRefreshOptions{
		Reindex:           contextBoolPtr(true),
		RegenerateContext: contextBoolPtr(false),
	}); err != nil {
		t.Fatalf("RefreshWithOptions failed instead of degrading: %v", err)
	}
	state, err := f.db.GetWorkspace(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if state.State != "ready" || state.BaseSHA != reviewTestHead {
		t.Fatalf("workspace = %+v, want ready on the new base", state)
	}
	if state.IndexSHA != oldBase {
		t.Fatalf("index sha = %q, want the previous index %q", state.IndexSHA, oldBase)
	}
	if state.LastError != "index: code graph indexing failed" {
		t.Fatalf("last error = %q, want the index failure", state.LastError)
	}
}

type failingWorkspaceGit struct {
	GitOps
	err error
}

func (g failingWorkspaceGit) EnsureMirror(context.Context, string, string, repo.Auth) error {
	return g.err
}

func TestWorkspaceFailuresPersistAndLogOnlyFixedData(t *testing.T) {
	const raw = "/private/work tree/file\nglpat-synthetic-secret more failure details"
	cases := []struct {
		name  string
		cause error
		class string
	}{
		{"untrusted custom code", &Error{Code: "synth_secret", Message: raw}, "internal"},
		{"timeout", fmt.Errorf("%s: %w", raw, context.DeadlineExceeded), "timeout"},
		{"canceled error with active context", fmt.Errorf("%s: %w", raw, context.Canceled), "canceled"},
		{"panic", fmt.Errorf("%s: %w", raw, errclass.ErrPanic), "panic"},
	}
	for _, tc := range cases {
		for _, mode := range []string{"onboard", "refresh"} {
			for _, failure := range []string{"workspace", "index"} {
				t.Run(tc.name+"/"+mode+"/"+failure, func(t *testing.T) {
					f := newReviewFixture(t, reviewTestHead)
					oldBase := strings.Repeat("a", 40)
					if _, err := f.db.SaveWorkspace(context.Background(), store.Workspace{
						ProjectID: 42, State: "ready", BaseSHA: oldBase, IndexSHA: oldBase, ContextSHA: "old-context",
					}); err != nil {
						t.Fatalf("SaveWorkspace: %v", err)
					}
					service := unrestrictedIndexService(t, f)
					var logs bytes.Buffer
					service.logger = slog.New(slog.NewJSONHandler(&logs, nil))
					if failure == "workspace" {
						service.git = failingWorkspaceGit{GitOps: f.git, err: tc.cause}
					} else {
						service.mcpFactory = func(context.Context, int64) (*mcp.Manager, error) {
							return nil, tc.cause
						}
					}
					var err error
					if mode == "onboard" {
						err = service.Onboard(context.Background(), 42)
					} else {
						err = service.RefreshWithOptions(context.Background(), 42, "main", WorkspaceRefreshOptions{
							Reindex: contextBoolPtr(true), RegenerateContext: contextBoolPtr(false),
						})
					}
					if failure == "workspace" && !errors.Is(err, tc.cause) {
						t.Fatalf("workspace failure = %v, want original cause", err)
					}
					if failure == "index" && err != nil {
						t.Fatalf("index failure must not block %s: %v", mode, err)
					}
					state, err := f.db.GetWorkspace(context.Background(), 42)
					if err != nil {
						t.Fatalf("GetWorkspace: %v", err)
					}
					wantState, wantError := "error", "workspace operation failed"
					if failure == "index" {
						wantState, wantError = "ready", "index: code graph indexing failed"
					}
					if state.State != wantState || state.LastError != wantError {
						t.Errorf("workspace state = %q, last error = %q; want %q, %q", state.State, state.LastError, wantState, wantError)
					}
					if mode == "refresh" && state.IndexSHA != oldBase {
						t.Errorf("refresh lost previous index: %q", state.IndexSHA)
					}
					if mode == "refresh" && state.ContextSHA != "old-context" {
						t.Errorf("refresh lost previous context: %q", state.ContextSHA)
					}
					if mode == "refresh" && failure == "workspace" && state.BaseSHA != oldBase {
						t.Errorf("failed refresh lost previous base: %q", state.BaseSHA)
					}
					if mode == "refresh" && failure == "index" && state.LastRefreshAt == nil {
						t.Error("degraded refresh did not record refresh time")
					}
					if failure == "index" && state.BaseSHA != reviewTestHead {
						t.Errorf("degraded %s base = %q, want %q", mode, state.BaseSHA, reviewTestHead)
					}
					var event map[string]any
					if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
						t.Fatalf("one structured warning required: %q: %v", logs.String(), err)
					}
					if event["level"] != "WARN" || event["project_id"] != float64(42) || event["error_class"] != tc.class {
						t.Errorf("warning = %v, want project 42 and fixed class %q", event, tc.class)
					}
					if strings.Contains(logs.String(), "synth_secret") || strings.Contains(logs.String(), "glpat-") || strings.Contains(logs.String(), "/private/") {
						t.Errorf("warning leaked untrusted data: %s", logs.String())
					}
				})
			}
		}
	}
}

func TestMarkWorkspaceErrorKeepsExistingCheckpoints(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	refreshed := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	if _, err := f.db.SaveWorkspace(context.Background(), store.Workspace{
		ProjectID: 42, State: "ready", BaseSHA: "base", IndexSHA: "index", ContextSHA: "context", LastRefreshAt: &refreshed,
	}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	service := unrestrictedIndexService(t, f)
	service.markWorkspaceError(context.Background(), 42, errors.New("/private/work tree glpat-synthetic-secret"))
	state, err := f.db.GetWorkspace(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if state.State != "error" || state.LastError != "workspace operation failed" || state.BaseSHA != "base" || state.IndexSHA != "index" || state.ContextSHA != "context" || state.LastRefreshAt == nil || !state.LastRefreshAt.Equal(refreshed) {
		t.Fatalf("workspace error lost checkpoints or persisted raw data: %+v", state)
	}
}

func TestWorkspaceFailureLogsUpstreamClassAndStatus(t *testing.T) {
	const raw = "https://gitlab.example/private?token=glpat-synthetic-secret"
	cases := []struct {
		name   string
		cause  error
		class  string
		status float64
	}{
		{"auth", &llm.Error{Kind: llm.ErrAuth, Status: 403, Err: errors.New(raw)}, "upstream_auth", 403},
		{"rate limited", &llm.Error{Kind: llm.ErrRateLimited, Status: 429, Err: errors.New(raw)}, "rate_limited", 429},
		{"network", &llm.Error{Kind: llm.ErrTransient, Err: errors.New(raw)}, "network", 0},
		{"config", &llm.Error{Kind: llm.ErrBadRequest, Status: 400, Err: errors.New(raw)}, "config", 400},
		{"overloaded", &llm.Error{Kind: llm.ErrOverloaded, Status: 529}, "overloaded", 529},
		{"context too long", &llm.Error{Kind: llm.ErrContextTooLong, Status: 413}, "context_too_long", 413},
		{"wrapped auth", fmt.Errorf("%s: %w", raw, &llm.Error{Kind: llm.ErrAuth, Status: 403}), "upstream_auth", 403},
		{"canceled", &llm.Error{Kind: llm.ErrCanceled, Err: context.Canceled}, "canceled", 0},
		{"deadline", &llm.Error{Kind: llm.ErrCanceled, Err: context.DeadlineExceeded}, "timeout", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			service := unrestrictedIndexService(t, f)
			var logs bytes.Buffer
			service.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			service.markWorkspaceError(context.Background(), 42, tc.cause)

			state, err := f.db.GetWorkspace(context.Background(), 42)
			if err != nil {
				t.Fatalf("GetWorkspace: %v", err)
			}
			if state.State != "error" || state.LastError != store.WorkspaceOperationFailed {
				t.Fatalf("workspace = %+v, want error state with the fixed message", state)
			}
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatalf("one structured warning required: %q: %v", logs.String(), err)
			}
			if event["level"] != "WARN" || event["project_id"] != float64(42) || event["error_class"] != tc.class {
				t.Errorf("warning = %v, want project 42 and class %q", event, tc.class)
			}
			status, ok := event["upstream_status"]
			if tc.status == 0 {
				if ok {
					t.Errorf("warning recorded upstream_status=%v for a failure without an HTTP status", status)
				}
			} else if status != tc.status {
				t.Errorf("upstream_status = %v, want %v", status, tc.status)
			}
			for _, secret := range []string{"glpat-", "gitlab.example", "/private"} {
				if strings.Contains(logs.String(), secret) {
					t.Errorf("warning leaked %q: %s", secret, logs.String())
				}
			}
		})
	}
}

func TestWorkspaceIndexCancellationStillReturnsError(t *testing.T) {
	for _, mode := range []string{"onboard", "refresh"} {
		t.Run(mode, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			oldBase := strings.Repeat("a", 40)
			if _, err := f.db.SaveWorkspace(context.Background(), store.Workspace{
				ProjectID: 42, State: "ready", BaseSHA: oldBase, IndexSHA: oldBase,
			}); err != nil {
				t.Fatalf("SaveWorkspace: %v", err)
			}
			service := unrestrictedIndexService(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			service.mcpFactory = func(context.Context, int64) (*mcp.Manager, error) {
				cancel()
				return nil, fmt.Errorf("/private/work tree glpat-synthetic-secret: %w", context.Canceled)
			}
			var err error
			if mode == "onboard" {
				err = service.Onboard(ctx, 42)
			} else {
				err = service.Refresh(ctx, 42, "main")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s = %v, want cancellation", mode, err)
			}
			state, err := f.db.GetWorkspace(context.Background(), 42)
			if err != nil {
				t.Fatalf("GetWorkspace: %v", err)
			}
			if state.State == "ready" || strings.Contains(state.LastError, "glpat-") || strings.Contains(state.LastError, "/private/") {
				t.Fatalf("canceled operation reported success or stored raw error: %+v", state)
			}
			if mode == "refresh" && state.IndexSHA != oldBase {
				t.Fatalf("canceled refresh lost previous index: %q", state.IndexSHA)
			}
		})
	}
}

func TestOnboardingFailureLogsUpstreamClassAndStatus(t *testing.T) {
	const raw = "https://gitlab.example/private?token=glpat-synthetic-secret"
	f := newReviewFixture(t, reviewTestHead)
	service := newOnboardingContextService(t, f, func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, &llm.Error{Kind: llm.ErrAuth, Status: 403, Err: errors.New(raw)}
	})
	var logs bytes.Buffer
	service.logger = slog.New(slog.NewJSONHandler(&logs, nil))

	if err := service.Onboard(context.Background(), 42); err == nil {
		t.Fatal("onboarding with an upstream auth failure succeeded")
	}
	var event map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var candidate map[string]any
		if err := json.Unmarshal([]byte(line), &candidate); err != nil {
			t.Fatalf("decode log %q: %v", line, err)
		}
		if candidate["msg"] == "workspace operation failed" {
			event = candidate
		}
	}
	if event == nil {
		t.Fatalf("workspace failure was not logged: %s", logs.String())
	}
	if event["error_class"] != "upstream_auth" || event["upstream_status"] != float64(403) {
		t.Errorf("warning = %v, want upstream_auth with status 403", event)
	}
	for _, secret := range []string{"glpat-", "gitlab.example", "/private"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("warning leaked %q: %s", secret, logs.String())
		}
	}
	state, err := f.db.GetWorkspace(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if state.State != "error" || state.LastError != store.WorkspaceOperationFailed {
		t.Fatalf("workspace = %+v, want the fixed error state", state)
	}
}
