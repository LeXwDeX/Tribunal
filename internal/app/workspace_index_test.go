package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/agent"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/mcp"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/review"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
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

func TestOnboardingOnlyRecordsIndexAfterCommandRuns(t *testing.T) {
	cases := []struct {
		name      string
		manager   func(*testing.T) *mcp.Manager
		wantErr   bool
		wantIndex string
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
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			service := newOnboardingContextService(t, f, onboardingIndexRunner)
			setIndexFactory(service, tc.manager(t))

			err := service.Onboard(context.Background(), 42)
			if tc.wantErr {
				if err == nil {
					t.Fatal("Onboard succeeded with a failing index command")
				}
				state, stateErr := f.db.GetWorkspace(context.Background(), 42)
				if stateErr != nil {
					t.Fatalf("GetWorkspace: %v", stateErr)
				}
				if state.IndexSHA != "" {
					t.Fatalf("failed index recorded sha %q", state.IndexSHA)
				}
				return
			}
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
	service := newOnboardingContextService(t, f, onboardingIndexRunner)
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
	service := newOnboardingContextService(t, f, onboardingIndexRunner)
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
