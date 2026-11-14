package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// driftGitOps records the range used for the cumulative context drift check.
type driftGitOps struct {
	*fakeGitOps
	mu             sync.Mutex
	changedFrom    string
	changedTo      string
	changedCalls   int
	changedResults []string
}

func (g *driftGitOps) ChangedFiles(_ context.Context, _, from, to string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.changedCalls++
	g.changedFrom, g.changedTo = from, to
	return append([]string(nil), g.changedResults...), nil
}

func testContextBaseSHA() string { return strings.Repeat("c", 40) }

func seedActiveContextDoc(t *testing.T, f *reviewFixture, base string, locked bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.db.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: base, Kind: "agents", Source: "manual", Content: "project context", Active: true,
	}); err != nil {
		t.Fatalf("SaveContextDoc: %v", err)
	}
	if locked {
		if err := f.db.LockContextDoc(ctx, 42, base, "agents", true, nil); err != nil {
			t.Fatalf("LockContextDoc: %v", err)
		}
	}
}

func TestContextRefreshMeasuresCumulativeDriftFromContextBase(t *testing.T) {
	for _, tc := range []struct {
		name        string
		lastRefresh time.Time
		changed     []string
		want        bool
	}{
		{"accumulated-drift", time.Now().Add(-time.Hour), []string{"a.go", "b.go"}, true},
		{"below-threshold", time.Now().Add(-time.Hour), []string{"a.go"}, false},
		{"older-than-max-age", time.Now().Add(-200 * time.Hour), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			contextBase := testContextBaseSHA()
			seedActiveContextDoc(t, f, contextBase, false)
			state := &store.Workspace{
				ProjectID: 42, State: "ready", BaseSHA: reviewTestHead,
				ContextSHA: contextBase, LastRefreshAt: timePtr(tc.lastRefresh),
			}
			git := &driftGitOps{fakeGitOps: f.git, changedResults: tc.changed}
			now := time.Now()
			svc := &WorkspaceService{
				store: f.db, git: git, workspace: f.manager,
				contextRefresh: config.ContextRefreshConfig{ChangedFiles: 2, MaxAge: 168 * time.Hour},
				now:            func() time.Time { return now },
			}
			if got := svc.shouldRefreshContext(context.Background(), 42, reviewTestHead, state); got != tc.want {
				t.Fatalf("shouldRefreshContext = %v, want %v", got, tc.want)
			}
			if tc.want && len(tc.changed) > 0 {
				if git.changedFrom != contextBase || git.changedTo != reviewTestHead {
					t.Fatalf("drift range = %s..%s, want %s..%s", git.changedFrom, git.changedTo, contextBase, reviewTestHead)
				}
			}
		})
	}
}

func TestContextRefreshUsesActiveDocumentBaseInsteadOfStoredCheckpoint(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	docBase := strings.Repeat("d", 40)
	seedActiveContextDoc(t, f, docBase, false)
	state := &store.Workspace{
		ProjectID: 42, State: "ready", BaseSHA: reviewTestHead,
		ContextSHA: testContextBaseSHA(), LastRefreshAt: timePtr(time.Now().Add(-time.Hour)),
	}
	git := &driftGitOps{fakeGitOps: f.git, changedResults: []string{"a.go", "b.go"}}
	svc := &WorkspaceService{
		store: f.db, git: git, workspace: f.manager,
		contextRefresh: config.ContextRefreshConfig{ChangedFiles: 2, MaxAge: 168 * time.Hour},
		now:            time.Now,
	}
	if !svc.shouldRefreshContext(context.Background(), 42, reviewTestHead, state) {
		t.Fatal("drift from the active document base must trigger regeneration")
	}
	if git.changedFrom != docBase {
		t.Fatalf("drift from = %s, want active document base %s", git.changedFrom, docBase)
	}
}

func TestRefreshDoesNotAdvanceLastRefreshAtWithoutRegeneration(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	seedActiveContextDoc(t, f, testContextBaseSHA(), false)
	lastRefresh := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	if _, err := f.db.SaveWorkspace(ctx, store.Workspace{
		ProjectID: 42, State: "ready", BaseSHA: reviewTestHead,
		ContextSHA: testContextBaseSHA(), LastRefreshAt: timePtr(lastRefresh),
	}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	svc := newOnboardingContextService(t, f, onboardingIndexRunner)
	svc.contextRefresh = config.ContextRefreshConfig{ChangedFiles: 200, MaxAge: 168 * time.Hour}
	svc.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	generated := 0
	svc.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		generated++
		return onboardingIndexRunner(ctx, spec)
	}

	if err := svc.Refresh(ctx, 42, "main"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	state, err := f.db.GetWorkspace(ctx, 42)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if generated != 0 {
		t.Fatalf("generated context %d times, want 0", generated)
	}
	if state.LastRefreshAt == nil || !state.LastRefreshAt.Equal(lastRefresh) {
		t.Fatalf("last_refresh_at = %v, want unchanged %v", state.LastRefreshAt, lastRefresh)
	}
}

func TestOnboardingRecordsContextRefreshTime(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	svc := newOnboardingContextService(t, f, onboardingIndexRunner)
	now := time.Now().UTC().Truncate(time.Second)
	svc.now = func() time.Time { return now }
	if err := svc.Onboard(ctx, 42); err != nil {
		t.Fatalf("Onboard: %v", err)
	}
	state, err := f.db.GetWorkspace(ctx, 42)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if state.LastRefreshAt == nil || !state.LastRefreshAt.Equal(now) {
		t.Fatalf("last_refresh_at = %v, want %v", state.LastRefreshAt, now)
	}
}
