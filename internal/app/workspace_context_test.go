package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

var onboardingContextBase = strings.Repeat("b", 40)

func contextBoolPtr(value bool) *bool { return &value }

func onboardingContextProfile() config.ProfileConfig {
	return config.ProfileConfig{Agent: &config.AgentConfig{Provider: "fake"}}
}

func newOnboardingContextService(t *testing.T, f *reviewFixture, runner review.AgentRunner) *WorkspaceService {
	t.Helper()
	svc, err := NewWorkspaceService(WorkspaceServiceConfig{
		Store:          f.db,
		Queue:          f.db,
		GitLab:         f.gitlab,
		Cipher:         newTestCipher(t),
		Workspace:      f.manager,
		Git:            f.git,
		Engine:         f.engine,
		Executors:      f.executors,
		Profiles:       map[string]config.ProfileConfig{"onboarding": onboardingContextProfile()},
		ReviewDefaults: f.defaults,
		Prompts:        f.bundle,
		AgentRun:       runner,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewWorkspaceService: %v", err)
	}
	return svc
}

func newContextService(t *testing.T, f *reviewFixture) *ContextService {
	t.Helper()
	svc, err := NewContextService(ContextServiceConfig{Store: f.db, Workspace: f.manager})
	if err != nil {
		t.Fatalf("NewContextService: %v", err)
	}
	return svc
}

func seedWorkspaceContext(t *testing.T, f *reviewFixture, contexts *ContextService, locked bool) *store.ContextDoc {
	t.Helper()
	ctx := context.Background()
	if _, err := f.db.SaveWorkspace(ctx, store.Workspace{
		ProjectID: 42, State: "ready",
		BaseSHA: onboardingContextBase, ContextSHA: onboardingContextBase,
	}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	doc, err := contexts.SaveContext(ctx, 42, "agents", "existing context", nil, nil)
	if err != nil {
		t.Fatalf("SaveContext: %v", err)
	}
	if locked {
		if err := contexts.LockContext(ctx, 42, "agents", true, nil); err != nil {
			t.Fatalf("LockContext: %v", err)
		}
	}
	return doc
}

func TestRefreshKeepsLockedContextCurrent(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	contexts := newContextService(t, f)
	old := seedWorkspaceContext(t, f, contexts, true)
	generated := 0
	svc := newOnboardingContextService(t, f, func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		generated++
		return validOnboardingContext(ctx, spec)
	})

	if err := svc.RefreshWithOptions(context.Background(), 42, "main", WorkspaceRefreshOptions{
		RegenerateContext: contextBoolPtr(true),
	}); err != nil {
		t.Fatalf("RefreshWithOptions: %v", err)
	}
	state, err := f.db.GetWorkspace(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if state.BaseSHA != reviewTestHead {
		t.Fatalf("workspace base = %q, want %q", state.BaseSHA, reviewTestHead)
	}
	if state.ContextSHA != onboardingContextBase {
		t.Fatalf("workspace context = %q, want locked base %q", state.ContextSHA, onboardingContextBase)
	}
	current, err := f.db.GetCurrentContextDoc(context.Background(), 42, "agents")
	if err != nil || current.ID != old.ID || !current.Active || !current.Locked || current.BaseSHA != onboardingContextBase || current.Content != "existing context" {
		t.Fatalf("current context = %+v, %v; want unchanged locked version", current, err)
	}
	if generated != 1 {
		t.Fatalf("generated %d context documents, want 1", generated)
	}
	docs, _, err := f.db.ListContextDocs(context.Background(), store.ContextDocFilter{ProjectID: 42, Limit: 20})
	if err != nil || len(docs) != 2 {
		t.Fatalf("versions = %+v, %v; want old and generated", docs, err)
	}
	for _, doc := range docs {
		if doc.ID != old.ID && (doc.Active || doc.BaseSHA != reviewTestHead || doc.Content == "" || doc.Source != "generated") {
			t.Fatalf("generated version = %+v; want inactive generated revision", doc)
		}
	}
	ws := f.manager.Get(42)
	data, err := ws.ReadContext(onboardingContextBase, "AGENTS.md")
	if err != nil || string(data) != "existing context" {
		t.Fatalf("locked projection = %q, %v", data, err)
	}
	meta, err := ws.ContextMeta(onboardingContextBase)
	if err != nil || !meta.Locked {
		t.Fatalf("locked metadata = %+v, %v", meta, err)
	}
}

type failCurrentAfterSaveStore struct {
	ReviewStore
	err      error
	saved    bool
	inactive bool
}

func (s *failCurrentAfterSaveStore) SaveContextDoc(ctx context.Context, doc store.ContextDoc) (*store.ContextDoc, error) {
	saved, err := s.ReviewStore.SaveContextDoc(ctx, doc)
	if err == nil {
		s.saved = true
		s.inactive = !saved.Active
	}
	return saved, err
}

func (s *failCurrentAfterSaveStore) GetCurrentContextDoc(ctx context.Context, projectID int64, kind string) (*store.ContextDoc, error) {
	if s.saved {
		return nil, s.err
	}
	return s.ReviewStore.GetCurrentContextDoc(ctx, projectID, kind)
}

func TestRefreshCurrentReadFailureAfterInactiveSave(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	old := seedWorkspaceContext(t, f, newContextService(t, f), true)
	svc := newOnboardingContextService(t, f, validOnboardingContext)
	failure := errors.New("sensitive-store-error")
	wrapped := &failCurrentAfterSaveStore{ReviewStore: f.db, err: failure}
	svc.store = wrapped
	var logs bytes.Buffer
	svc.logger = slog.New(slog.NewTextHandler(&logs, nil))

	err := svc.RefreshWithOptions(ctx, 42, "main", WorkspaceRefreshOptions{RegenerateContext: contextBoolPtr(true)})
	if !errors.Is(err, failure) {
		t.Fatalf("refresh error = %v, want underlying current read failure", err)
	}
	if strings.Contains(err.Error(), failure.Error()) || strings.Contains(logs.String(), failure.Error()) {
		t.Fatal("store error leaked in error or logs")
	}
	if !wrapped.saved || !wrapped.inactive {
		t.Fatal("expected inactive revision to be saved before current read failure")
	}
	state, err := f.db.GetWorkspace(ctx, 42)
	if err != nil || state.State != "error" || state.ContextSHA != onboardingContextBase || state.LastError != store.WorkspaceOperationFailed {
		t.Fatalf("workspace = %+v, %v; want error with original context SHA", state, err)
	}
	current, err := f.db.GetCurrentContextDoc(ctx, 42, "agents")
	if err != nil || current.ID != old.ID || !current.Active || !current.Locked || current.Content != "existing context" {
		t.Fatalf("current = %+v, %v; want original locked revision", current, err)
	}
	docs, _, err := f.db.ListContextDocs(ctx, store.ContextDocFilter{ProjectID: 42, Limit: 20})
	if err != nil || len(docs) != 2 {
		t.Fatalf("versions = %+v, %v; want original and inactive revision", docs, err)
	}
	for _, doc := range docs {
		if doc.ID != old.ID && (doc.Active || doc.BaseSHA != reviewTestHead || doc.Source != "generated") {
			t.Fatalf("generated revision = %+v; want inactive revision at new base", doc)
		}
	}
}

func TestOnboardCurrentReadFailureAfterInactiveSave(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	old := seedWorkspaceContext(t, f, newContextService(t, f), true)
	svc := newOnboardingContextService(t, f, validOnboardingContext)
	failure := errors.New("sensitive-store-error")
	wrapped := &failCurrentAfterSaveStore{ReviewStore: f.db, err: failure}
	svc.store = wrapped
	err := svc.Onboard(ctx, 42)
	if !errors.Is(err, failure) || strings.Contains(err.Error(), failure.Error()) || !wrapped.saved || !wrapped.inactive {
		t.Fatalf("onboard error = %v, saved = %t, inactive = %t", err, wrapped.saved, wrapped.inactive)
	}
	state, err := f.db.GetWorkspace(ctx, 42)
	if err != nil || state.State != "error" || state.ContextSHA != onboardingContextBase || state.LastError != store.WorkspaceOperationFailed {
		t.Fatalf("workspace = %+v, %v; want error with original context SHA", state, err)
	}
	current, err := f.db.GetCurrentContextDoc(ctx, 42, "agents")
	if err != nil || current.ID != old.ID || !current.Active || !current.Locked {
		t.Fatalf("current = %+v, %v; want original locked revision", current, err)
	}
}

func TestActiveContextBaseFallsBackWhenNotFound(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	svc := newOnboardingContextService(t, f, validOnboardingContext)
	base, err := svc.activeContextBase(context.Background(), 42, onboardingContextBase)
	if err != nil || base != onboardingContextBase {
		t.Fatalf("active context base = %q, %v; want fallback", base, err)
	}
}

func TestRefreshMissingCurrentAfterInactiveSaveKeepsPriorContextSHA(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	old := seedWorkspaceContext(t, f, newContextService(t, f), true)
	svc := newOnboardingContextService(t, f, validOnboardingContext)
	wrapped := &failCurrentAfterSaveStore{ReviewStore: f.db, err: store.ErrNotFound}
	svc.store = wrapped
	if err := svc.RefreshWithOptions(ctx, 42, "main", WorkspaceRefreshOptions{RegenerateContext: contextBoolPtr(true)}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	state, err := f.db.GetWorkspace(ctx, 42)
	if err != nil || !wrapped.inactive || state.ContextSHA != onboardingContextBase {
		t.Fatalf("workspace = %+v, %v; want original context SHA", state, err)
	}
	current, err := f.db.GetCurrentContextDoc(ctx, 42, "agents")
	if err != nil || current.ID != old.ID || !current.Active || !current.Locked {
		t.Fatalf("current = %+v, %v; want original locked revision", current, err)
	}
}

func validOnboardingContext(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
	out := review.OnboardingOut{
		Overview: "overview", Architecture: "architecture",
		Modules:     []review.Module{{Path: "internal", Purpose: "core"}},
		Conventions: []string{"testing"}, BuildAndTest: []string{"go test ./..."},
		RiskHotspots: []string{"none"}, ReviewFocus: []string{"contracts"},
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	decoded, err := spec.Submit.Decode(raw)
	if err != nil {
		return nil, err
	}
	return &agent.RunResult{Output: decoded, Usage: llm.Usage{InputTokens: 3, OutputTokens: 4}}, nil
}

func TestAutomaticRefreshSkipsLockedContext(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	old := seedWorkspaceContext(t, f, newContextService(t, f), true)
	generated := 0
	svc := newOnboardingContextService(t, f, func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		generated++
		return validOnboardingContext(ctx, spec)
	})
	svc.contextRefresh.MaxAge = time.Nanosecond
	if svc.shouldRefreshContext(context.Background(), 42, reviewTestHead, nil) {
		t.Fatal("automatic refresh with an empty workspace state must skip locked context")
	}
	if err := svc.Refresh(context.Background(), 42, "main"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	docs, _, err := f.db.ListContextDocs(context.Background(), store.ContextDocFilter{ProjectID: 42, Limit: 20})
	if err != nil || generated != 0 || len(docs) != 1 || docs[0].ID != old.ID {
		t.Fatalf("generated = %d, versions = %+v, %v; want only locked revision", generated, docs, err)
	}
}

func TestRefreshActivatesGeneratedContextWhenUnlocked(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	contexts := newContextService(t, f)
	old := seedWorkspaceContext(t, f, contexts, false)
	svc := newOnboardingContextService(t, f, validOnboardingContext)

	if err := svc.RefreshWithOptions(context.Background(), 42, "main", WorkspaceRefreshOptions{
		RegenerateContext: contextBoolPtr(true),
	}); err != nil {
		t.Fatalf("RefreshWithOptions: %v", err)
	}
	state, err := f.db.GetWorkspace(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if state.BaseSHA != reviewTestHead || state.ContextSHA != reviewTestHead {
		t.Fatalf("workspace = base %q context %q, want both %q", state.BaseSHA, state.ContextSHA, reviewTestHead)
	}
	current, err := f.db.GetCurrentContextDoc(context.Background(), 42, "agents")
	if err != nil || !current.Active || current.BaseSHA != reviewTestHead || current.ID == old.ID {
		t.Fatalf("current context = %+v, %v; want a new active version at %q", current, err, reviewTestHead)
	}
	docs, _, err := f.db.ListContextDocs(context.Background(), store.ContextDocFilter{ProjectID: 42, Limit: 20})
	if err != nil || len(docs) != 2 {
		t.Fatalf("versions = %+v, %v; want old and generated", docs, err)
	}
	for _, doc := range docs {
		if doc.ID == old.ID && (doc.Active || doc.Content != "existing context") {
			t.Fatalf("old version changed: %+v", doc)
		}
	}
	ws := f.manager.Get(42)
	data, err := ws.ReadContext(reviewTestHead, "AGENTS.md")
	if err != nil || string(data) != current.Content {
		t.Fatalf("generated projection = %q, %v; want stored content", data, err)
	}
	meta, err := ws.ContextMeta(reviewTestHead)
	if err != nil || meta.GeneratedAt != current.CreatedAt || meta.SourceSHA != reviewTestHead {
		t.Fatalf("generated metadata = %+v, %v", meta, err)
	}
}

func TestRefreshSurfacesContextProjectionFailure(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	contexts := newContextService(t, f)
	seedWorkspaceContext(t, f, contexts, false)
	svc := newOnboardingContextService(t, f, func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		out := review.OnboardingOut{
			Overview:     "overview",
			Architecture: "architecture",
			Modules:      []review.Module{{Path: "internal", Purpose: "core"}},
			Conventions:  []string{"testing"},
			BuildAndTest: []string{"go test ./..."},
			RiskHotspots: []string{"none"},
			ReviewFocus:  []string{"contracts"},
		}
		raw, _ := json.Marshal(out)
		decoded, err := spec.Submit.Decode(raw)
		if err != nil {
			return nil, err
		}
		return &agent.RunResult{Output: decoded}, nil
	})
	// Replace the context directory with a regular file so the projection fails.
	contextDir := filepath.Join(f.manager.Get(42).Root, "context")
	if err := os.RemoveAll(contextDir); err != nil {
		t.Fatalf("remove context dir: %v", err)
	}
	if err := os.WriteFile(contextDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("replace context dir: %v", err)
	}

	err := svc.RefreshWithOptions(context.Background(), 42, "main", WorkspaceRefreshOptions{
		RegenerateContext: contextBoolPtr(true),
	})
	if !errors.Is(err, ErrContextProjection) {
		t.Fatalf("refresh error = %v, want ErrContextProjection", err)
	}
	state, stateErr := f.db.GetWorkspace(context.Background(), 42)
	if stateErr != nil {
		t.Fatalf("GetWorkspace: %v", stateErr)
	}
	if state.State != "error" {
		t.Fatalf("workspace state = %q, want error", state.State)
	}
	current, currentErr := f.db.GetCurrentContextDoc(context.Background(), 42, "agents")
	if currentErr != nil || current.BaseSHA != reviewTestHead {
		t.Fatalf("database current = %+v, %v; database stays authoritative", current, currentErr)
	}
}
