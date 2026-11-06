package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

type fakeOpenMRs struct {
	mu     sync.Mutex
	byProj map[int64][]gitlab.MergeRequest
	err    map[int64]error
	limits map[int64]int
	since  map[int64]time.Time
	calls  int
}

func (f *fakeOpenMRs) ListOpenMergeRequests(_ context.Context, token string, projectID int64, limit int, updatedAfter time.Time) ([]gitlab.MergeRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.limits == nil {
		f.limits = map[int64]int{}
	}
	f.limits[projectID] = limit
	if f.since == nil {
		f.since = map[int64]time.Time{}
	}
	f.since[projectID] = updatedAfter
	if err := f.err[projectID]; err != nil {
		return nil, err
	}
	if token != "glpat-seeded-token" {
		return nil, errors.New("unexpected token")
	}
	mrs := f.byProj[projectID]
	if len(mrs) > limit {
		mrs = mrs[:limit]
	}
	return append([]gitlab.MergeRequest(nil), mrs...), nil
}

// deliveryRecorder wraps the real store so review.run deliveries still create
// review controls, while remembering every delivery and its job payload.
type deliveryRecorder struct {
	*sqlite.Store
	mu      sync.Mutex
	records []store.Delivery
	kinds   []string
	payload []json.RawMessage
}

func (r *deliveryRecorder) note(d store.Delivery, kind string, payload json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, d)
	r.kinds = append(r.kinds, kind)
	r.payload = append(r.payload, payload)
}

func (r *deliveryRecorder) RecordReviewDelivery(ctx context.Context, d store.Delivery, command *store.ReviewCommand) (store.ReviewEnqueueResult, error) {
	r.note(d, "review.run", command.Payload)
	return r.Store.RecordReviewDelivery(ctx, d, command)
}

func (r *deliveryRecorder) RecordReviewCandidateDelivery(ctx context.Context, d store.Delivery, c *store.ReviewCandidate) (store.ReviewEnqueueResult, error) {
	r.note(d, "review.candidate", c.Payload)
	return r.Store.RecordReviewCandidateDelivery(ctx, d, c)
}

type catchUpFixture struct {
	t       *testing.T
	st      *sqlite.Store
	rec     *deliveryRecorder
	webhook *WebhookService
	lister  *fakeOpenMRs
	logs    *bytes.Buffer
}

func shaOf(char string) string { return strings.Repeat(char, 40) }

func openMR(iid int64, head string) gitlab.MergeRequest {
	return gitlab.MergeRequest{IID: iid, State: "opened", TargetProjectID: 42, SHA: head, TargetBranch: "main", Title: "Add feature"}
}

func newCatchUp(t *testing.T, settings config.CatchUpConfig, projectSettings string, mrs ...gitlab.MergeRequest) (*CatchUpService, *catchUpFixture) {
	t.Helper()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	seedProject(t, st, cipher, 42, "whsec", projectSettings, "default")
	rec := &deliveryRecorder{Store: st}
	mgr := workspace.NewManager(t.TempDir(), nil)
	webhooks, err := NewWebhookService(WebhookServiceConfig{
		Store: rec, Workspace: mgr, Cipher: cipher, Profiles: testProfiles(), ReviewDefaults: testDefaults(),
	})
	if err != nil {
		t.Fatalf("NewWebhookService: %v", err)
	}
	lister := &fakeOpenMRs{byProj: map[int64][]gitlab.MergeRequest{42: mrs}, err: map[int64]error{}}
	logs := &bytes.Buffer{}
	svc, err := NewCatchUpService(CatchUpServiceConfig{
		Store: st, GitLab: lister, Webhooks: webhooks, Workspace: mgr, Cipher: cipher,
		Settings: settings, Logger: slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewCatchUpService: %v", err)
	}
	return svc, &catchUpFixture{t: t, st: st, rec: rec, webhook: webhooks, lister: lister, logs: logs}
}

var defaultCatchUp = config.CatchUpConfig{Enabled: true, MaxPerProject: 50, MaxTotal: 200, MaxAge: 24 * time.Hour}

// enqueuedPayloads decodes the jobs recorded by catch-up deliveries.
func (f *catchUpFixture) enqueued() []map[string]any {
	f.rec.mu.Lock()
	defer f.rec.mu.Unlock()
	var out []map[string]any
	for i, d := range f.rec.records {
		if d.Event != catchUpEvent {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(f.rec.payload[i], &payload); err != nil {
			f.t.Fatalf("decode payload: %v", err)
		}
		out = append(out, payload)
	}
	return out
}

func TestCatchUpEnqueuesMissedHeadWithCatchUpTrigger(t *testing.T) {
	svc, f := newCatchUp(t, defaultCatchUp, `{"allow_repo_config":false}`, openMR(7, shaOf("c")))
	result, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != (CatchUpResult{Projects: 1, MRsChecked: 1, Enqueued: 1}) {
		t.Fatalf("result = %+v", result)
	}
	jobs := f.enqueued()
	if len(jobs) != 1 || jobs[0]["trigger"] != "catch_up" || jobs[0]["head_sha"] != shaOf("c") || jobs[0]["mr_iid"] != float64(7) {
		t.Fatalf("enqueued = %+v, want one catch_up review.run for the missed head", jobs)
	}
	control, err := f.st.GetReviewControl(context.Background(), 42, 7)
	if err != nil || control.HeadSHA != shaOf("c") {
		t.Fatalf("control = %+v, err = %v; want head recorded by the enqueue", control, err)
	}
	if !strings.Contains(f.logs.String(), "catch-up enqueued review") || !strings.Contains(f.logs.String(), "head=cccccccc") {
		t.Fatalf("log = %q, want a per-MR enqueue line with the short head", f.logs.String())
	}
	if !strings.Contains(f.logs.String(), "catch-up scan finished") || !strings.Contains(f.logs.String(), "enqueued=1") {
		t.Fatalf("log = %q", f.logs.String())
	}
	// A second scan finds a control for the head and enqueues nothing.
	again, err := svc.Run(context.Background())
	if err != nil || again.Enqueued != 0 || again.MRsChecked != 1 {
		t.Fatalf("second scan = %+v, err = %v", again, err)
	}
}

func TestCatchUpUsesCandidatePathWhenRepoConfigAllowed(t *testing.T) {
	svc, f := newCatchUp(t, defaultCatchUp, `{}`, openMR(7, shaOf("c")))
	if result, err := svc.Run(context.Background()); err != nil || result.Enqueued != 1 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	f.rec.mu.Lock()
	defer f.rec.mu.Unlock()
	if len(f.rec.kinds) != 1 || f.rec.kinds[0] != "review.candidate" {
		t.Fatalf("kinds = %+v, want the deferred candidate path the webhook uses", f.rec.kinds)
	}
}

func TestCatchUpSkipsHeadThatAlreadyHasControl(t *testing.T) {
	svc, f := newCatchUp(t, defaultCatchUp, `{"allow_repo_config":false}`, openMR(7, shaOf("c")))
	// The webhook for this head arrived normally.
	body := mrBody(42, 7, func(b map[string]any) {
		patchAttr(b, "last_commit", map[string]any{"id": shaOf("c")})
	})
	if res, err := f.webhook.Handle(context.Background(), WebhookRequest{
		Event: "Merge Request Hook", Token: "whsec", UUID: uuid.NewString(), Body: body,
	}); err != nil || !res.Accepted {
		t.Fatalf("webhook = %+v, err = %v", res, err)
	}
	result, err := svc.Run(context.Background())
	if err != nil || result.Enqueued != 0 || result.MRsChecked != 1 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if got := f.enqueued(); len(got) != 0 {
		t.Fatalf("catch-up enqueued %+v for an already-handled head", got)
	}
}

func TestCatchUpEnqueuesWhenControlHoldsOlderHead(t *testing.T) {
	svc, f := newCatchUp(t, defaultCatchUp, `{"allow_repo_config":false}`, openMR(7, shaOf("d")))
	body := mrBody(42, 7, nil) // head sha40: the older head
	if res, err := f.webhook.Handle(context.Background(), WebhookRequest{
		Event: "Merge Request Hook", Token: "whsec", UUID: uuid.NewString(), Body: body,
	}); err != nil || !res.Accepted {
		t.Fatalf("webhook = %+v, err = %v", res, err)
	}
	result, err := svc.Run(context.Background())
	if err != nil || result.Enqueued != 1 {
		t.Fatalf("result = %+v, err = %v; the newer head was missed", result, err)
	}
}

func TestCatchUpHonorsWebhookFilters(t *testing.T) {
	draft := openMR(1, shaOf("1"))
	draft.Draft = true
	titled := openMR(2, shaOf("2"))
	titled.Title = "WIP: later"
	wrongBranch := openMR(3, shaOf("3"))
	wrongBranch.TargetBranch = "release"
	labelled := openMR(4, shaOf("4"))
	labelled.Labels = []string{"skip-review"}
	closed := openMR(5, shaOf("5"))
	closed.State = "closed"
	otherProject := openMR(6, shaOf("6"))
	otherProject.TargetProjectID = 99
	badSHA := openMR(8, "not-a-sha")
	ok := openMR(9, shaOf("9"))
	settings := `{"allow_repo_config":false,"target_branches":["main"],"labels_exclude":["skip-review"]}`
	svc, f := newCatchUp(t, defaultCatchUp, settings, draft, titled, wrongBranch, labelled, closed, otherProject, badSHA, ok)
	result, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.MRsChecked != 8 || result.Enqueued != 1 {
		t.Fatalf("result = %+v, want 8 checked and only the eligible MR enqueued", result)
	}
	jobs := f.enqueued()
	if len(jobs) != 1 || jobs[0]["mr_iid"] != float64(9) {
		t.Fatalf("enqueued = %+v", jobs)
	}
}

func TestCatchUpSkipsDisabledProjectAndBadToken(t *testing.T) {
	svc, f := newCatchUp(t, defaultCatchUp, `{"allow_repo_config":false}`, openMR(7, shaOf("c")))
	off := false
	if _, err := f.st.UpdateProject(context.Background(), 42, store.ProjectUpdate{Enabled: &off}); err != nil {
		t.Fatalf("disable project: %v", err)
	}
	result, err := svc.Run(context.Background())
	if err != nil || result != (CatchUpResult{}) || f.lister.calls != 0 {
		t.Fatalf("disabled project: result = %+v, err = %v, GitLab calls = %d", result, err, f.lister.calls)
	}
}

func TestCatchUpListErrorSkipsOnlyThatProject(t *testing.T) {
	svc, f := newCatchUp(t, defaultCatchUp, `{"allow_repo_config":false}`, openMR(7, shaOf("c")))
	f.lister.err[42] = errors.New("boom glpat-secret")
	result, err := svc.Run(context.Background())
	if err != nil || result != (CatchUpResult{}) {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if strings.Contains(f.logs.String(), "glpat-secret") {
		t.Fatalf("log leaked the raw error: %q", f.logs.String())
	}
}

func TestCatchUpBounds(t *testing.T) {
	var mrs []gitlab.MergeRequest
	for i := int64(1); i <= 10; i++ {
		mrs = append(mrs, openMR(i, strings.Repeat(string("0123456789abcdef"[i]), 40)))
	}
	t.Run("per project", func(t *testing.T) {
		svc, f := newCatchUp(t, config.CatchUpConfig{Enabled: true, MaxPerProject: 3, MaxTotal: 100}, `{"allow_repo_config":false}`, mrs...)
		result, err := svc.Run(context.Background())
		if err != nil || result.MRsChecked != 3 || result.Enqueued != 3 || f.lister.limits[42] != 3 {
			t.Fatalf("result = %+v, err = %v, limit = %d", result, err, f.lister.limits[42])
		}
	})
	t.Run("overall", func(t *testing.T) {
		svc, f := newCatchUp(t, config.CatchUpConfig{Enabled: true, MaxPerProject: 50, MaxTotal: 4}, `{"allow_repo_config":false}`, mrs...)
		result, err := svc.Run(context.Background())
		if err != nil || result.MRsChecked != 4 || result.Enqueued != 4 || f.lister.limits[42] != 4 {
			t.Fatalf("result = %+v, err = %v, limit = %d", result, err, f.lister.limits[42])
		}
	})
}

func TestCatchUpDisabledSwitch(t *testing.T) {
	svc, f := newCatchUp(t, config.CatchUpConfig{Enabled: false, MaxPerProject: 50, MaxTotal: 200, MaxAge: time.Hour}, `{"allow_repo_config":false}`, openMR(7, shaOf("c")))
	result, err := svc.Run(context.Background())
	if err != nil || result != (CatchUpResult{}) || f.lister.calls != 0 || len(f.enqueued()) != 0 {
		t.Fatalf("result = %+v, err = %v, GitLab calls = %d", result, err, f.lister.calls)
	}
}

func TestReviewFilterReasonAppliesToCatchUpTrigger(t *testing.T) {
	mr := &gitlab.MergeRequest{Draft: true, TargetBranch: "main"}
	project := &store.Project{DefaultBranch: "main"}
	settings := config.ProjectSettings{SkipDraft: true}
	if got := reviewFilterReason(catchUpTrigger, mr, project, settings); got != "draft" {
		t.Fatalf("reason = %q, want draft: the worker must re-filter catch_up like webhook triggers", got)
	}
}

func TestCatchUpPassesAgeWindowToGitLab(t *testing.T) {
	svc, f := newCatchUp(t, defaultCatchUp, `{"allow_repo_config":false}`, openMR(7, shaOf("c")))
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	if _, err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := f.lister.since[42], now.Add(-24*time.Hour); !got.Equal(want) {
		t.Fatalf("updatedAfter = %v, want %v", got, want)
	}
}
