package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func testSpec() dag.GraphSpec {
	return dag.GraphSpec{
		Nodes: []dag.NodeSpec{
			{
				ID:       "a",
				Kind:     "code-review",
				Required: true,
				Params:   json.RawMessage(`{"package":"p1"}`),
				Timeout:  time.Minute,
				Expands:  false,
			},
			{
				ID:        "b",
				Kind:      "synthesize",
				Required:  true,
				DependsOn: []string{"a"},
			},
		},
		Budget: dag.Budget{MaxNodes: 10, MaxExtends: 1, MaxTokens: 1000},
	}
}

func reviewMeta(projectID, mrIID int64, head string) map[string]string {
	return map[string]string{
		"project_id":  strconv.FormatInt(projectID, 10),
		"kind":        "review",
		"mr_iid":      strconv.FormatInt(mrIID, 10),
		"head_sha":    head,
		"base_sha":    "base-" + head,
		"fingerprint": "fp-" + head,
		"profile":     "default",
		"trigger":     "mr_open",
	}
}

func testEvent(t *testing.T, typ string, payload any, at time.Time) dag.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s: %v", typ, err)
	}
	return dag.Event{Type: typ, Payload: raw, At: at.UTC()}
}

func createEvent(t *testing.T, wfID string, meta map[string]string, at time.Time) dag.Event {
	t.Helper()
	return testEvent(t, dag.EventWorkflowCreated, dag.WorkflowCreatedPayload{
		WorkflowID: wfID,
		Spec:       testSpec(),
		Meta:       meta,
	}, at)
}

func mustCreateWorkflow(t *testing.T, s *Store, wfID string, meta map[string]string, at time.Time) int64 {
	t.Helper()
	rev, err := s.Append(context.Background(), wfID, 0, createEvent(t, wfID, meta, at))
	if err != nil {
		t.Fatalf("Append created %s: %v", wfID, err)
	}
	return rev
}

func mustAppendEvent(t *testing.T, s *Store, wfID string, rev int64, ev dag.Event) int64 {
	t.Helper()
	next, err := s.Append(context.Background(), wfID, rev, ev)
	if err != nil {
		t.Fatalf("Append %s: %v", ev.Type, err)
	}
	return next
}

func queuedEvent(t *testing.T, node string, attempt int, at time.Time) dag.Event {
	t.Helper()
	return testEvent(t, dag.EventNodeQueued, dag.NodeQueuedPayload{NodeID: node, Attempt: attempt}, at)
}

func startedEvent(t *testing.T, node string, attempt int, provider, model string, at time.Time) dag.Event {
	t.Helper()
	return testEvent(t, dag.EventNodeStarted, dag.NodeStartedPayload{
		NodeID: node, Attempt: attempt, Provider: provider, Model: model,
	}, at)
}

func completedEvent(t *testing.T, node string, attempt int, cost int64, at time.Time) dag.Event {
	t.Helper()
	return testEvent(t, dag.EventNodeCompleted, dag.NodeCompletedPayload{
		NodeID:  node,
		Attempt: attempt,
		Output:  json.RawMessage(`{"ok":true}`),
		Usage:   dag.Usage{InputTokens: 2, OutputTokens: 3, CostMicros: cost},
	}, at)
}

func finishWorkflow(t *testing.T, s *Store, wfID string, rev int64, at time.Time) int64 {
	t.Helper()
	rev = mustAppendEvent(t, s, wfID, rev, testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, at))
	rev = mustAppendEvent(t, s, wfID, rev, queuedEvent(t, "a", 1, at))
	rev = mustAppendEvent(t, s, wfID, rev, startedEvent(t, "a", 1, "p", "m", at))
	rev = mustAppendEvent(t, s, wfID, rev, completedEvent(t, "a", 1, 5, at))
	rev = mustAppendEvent(t, s, wfID, rev, queuedEvent(t, "b", 1, at))
	rev = mustAppendEvent(t, s, wfID, rev, startedEvent(t, "b", 1, "p", "m", at))
	rev = mustAppendEvent(t, s, wfID, rev, completedEvent(t, "b", 1, 7, at))
	return mustAppendEvent(t, s, wfID, rev, testEvent(t, dag.EventWorkflowCompleted, dag.WorkflowTerminalPayload{}, at))
}

func TestWorkflowEventStoreReplayAndRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "review.db")
	clock := newFakeClock()
	open := func() *Store {
		st, err := Open(ctx, path, WithClock(clock.Now))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return st
	}
	st := open()
	mustProject(t, st, 42)
	wfID := "wf-replay"
	rev := mustCreateWorkflow(t, st, wfID, reviewMeta(42, 7, "head-1"), clock.Now())

	wf, err := st.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if wf.Status != "pending" || wf.GraphRev != 1 || wf.MRIID == nil || *wf.MRIID != 7 {
		t.Fatalf("created projection = %+v", wf)
	}
	nodes, err := st.GetWorkflowNodes(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflowNodes: %v", err)
	}
	if len(nodes) != 2 || nodes[0].Status != "pending" || nodes[0].Kind != "code-review" {
		t.Fatalf("created nodes = %+v", nodes)
	}
	if string(nodes[0].Params) != `{"package":"p1"}` || len(nodes[1].DependsOn) != 1 {
		t.Fatalf("node spec projection = %+v", nodes)
	}

	clock.Advance(time.Second)
	rev = mustAppendEvent(t, st, wfID, rev, testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, queuedEvent(t, "a", 1, clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, startedEvent(t, "a", 1, "p", "m", clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, completedEvent(t, "a", 1, 5, clock.Now()))
	if rev != 5 {
		t.Fatalf("revision after completed a = %d, want 5", rev)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st = open()
	events, err := st.Load(ctx, wfID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	replayed, err := dag.Project(events)
	if err != nil {
		t.Fatalf("Project replayed events: %v", err)
	}
	if replayed.Revision != rev || replayed.Nodes["a"].Status != dag.NodeCompleted || replayed.Nodes["b"].Status != dag.NodePending {
		t.Fatalf("replayed view = %+v", replayed)
	}
	wf, err = st.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflow reopened: %v", err)
	}
	if wf.Status != "running" || wf.GraphRev != rev {
		t.Fatalf("reopened projection = %+v", wf)
	}

	clock.Advance(time.Second)
	rev = mustAppendEvent(t, st, wfID, rev, queuedEvent(t, "b", 1, clock.Now()))
	mustAppendEvent(t, st, wfID, rev, startedEvent(t, "b", 1, "p", "m", clock.Now()))
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st = open()
	defer st.Close()
	unfinished, err := st.ListUnfinished(ctx)
	if err != nil {
		t.Fatalf("ListUnfinished: %v", err)
	}
	if len(unfinished) != 1 || unfinished[0] != wfID {
		t.Fatalf("unfinished = %v", unfinished)
	}
	engine := dag.NewEngine(st, nil, nil, dag.Options{Clock: clock.Now})
	if err := engine.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	nodes, err = st.GetWorkflowNodes(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflowNodes after recover: %v", err)
	}
	byID := make(map[string]store.WorkflowNode, len(nodes))
	for _, node := range nodes {
		byID[node.ID] = node
	}
	if byID["a"].Status != "completed" {
		t.Fatalf("completed node changed on recovery: %+v", byID["a"])
	}
	if byID["b"].Status != "pending" || byID["b"].Attempt != 2 {
		t.Fatalf("running node not recovered: %+v", byID["b"])
	}
}

func TestAppendFencesAndRollback(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	wfID := "wf-fences"
	rev := mustCreateWorkflow(t, st, wfID, reviewMeta(42, 1, "head"), clock.Now())

	_, err := st.Append(ctx, wfID, 0, testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, clock.Now()))
	if !errors.Is(err, dag.ErrStaleRevision) {
		t.Fatalf("stale append error = %v, want ErrStaleRevision", err)
	}
	rev = mustAppendEvent(t, st, wfID, rev, testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, clock.Now()))

	before, err := st.Load(ctx, wfID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = st.Append(ctx, wfID, rev,
		queuedEvent(t, "a", 1, clock.Now()),
		completedEvent(t, "b", 1, 0, clock.Now()))
	if !errors.Is(err, dag.ErrInvalidTransition) {
		t.Fatalf("invalid batch error = %v, want ErrInvalidTransition", err)
	}
	after, err := st.Load(ctx, wfID)
	if err != nil {
		t.Fatalf("Load after rollback: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("invalid batch left %d events, want %d", len(after), len(before))
	}

	rev = mustAppendEvent(t, st, wfID, rev, queuedEvent(t, "a", 1, clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, startedEvent(t, "a", 1, "p", "m", clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, testEvent(t, dag.EventNodeRestarted, dag.NodeRestartedPayload{
		NodeID: "a", NextAttempt: 2, Reason: "recovered",
	}, clock.Now()))
	_, err = st.Append(ctx, wfID, rev, completedEvent(t, "a", 1, 0, clock.Now()))
	if !errors.Is(err, dag.ErrStaleAttempt) {
		t.Fatalf("late result error = %v, want ErrStaleAttempt", err)
	}
	rev = mustAppendEvent(t, st, wfID, rev, queuedEvent(t, "a", 2, clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, startedEvent(t, "a", 2, "p", "m", clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, completedEvent(t, "a", 2, 0, clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, queuedEvent(t, "b", 1, clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, startedEvent(t, "b", 1, "p", "m", clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, completedEvent(t, "b", 1, 0, clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, testEvent(t, dag.EventWorkflowCompleted, dag.WorkflowTerminalPayload{}, clock.Now()))

	before, err = st.Load(ctx, wfID)
	if err != nil {
		t.Fatalf("Load terminal: %v", err)
	}
	_, err = st.Append(ctx, wfID, rev, startedEvent(t, "a", 2, "p", "m", clock.Now()))
	if !errors.Is(err, dag.ErrTerminalViolation) {
		t.Fatalf("terminal mutation error = %v, want ErrTerminalViolation", err)
	}
	after, err = st.Load(ctx, wfID)
	if err != nil {
		t.Fatalf("Load after terminal rollback: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("terminal mutation left %d events, want %d", len(after), len(before))
	}
}

func TestAppendRequiresProjectAndMetadata(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)

	cases := []struct {
		name    string
		wfID    string
		meta    map[string]string
		wantErr error
	}{
		{
			name: "missing project",
			wfID: "wf-no-project",
			meta: map[string]string{"kind": "review", "mr_iid": "1", "trigger": "mr_open"},
		},
		{
			name:    "unknown project",
			wfID:    "wf-unknown-project",
			meta:    map[string]string{"project_id": "99", "kind": "review", "mr_iid": "1", "trigger": "mr_open"},
			wantErr: store.ErrNotFound,
		},
		{
			name: "review without mr",
			wfID: "wf-no-mr",
			meta: map[string]string{"project_id": "42", "kind": "review", "trigger": "mr_open"},
		},
		{
			name: "invalid kind",
			wfID: "wf-bad-kind",
			meta: map[string]string{"project_id": "42", "kind": "review.run", "trigger": "mr_open"},
		},
		{
			name: "invalid trigger",
			wfID: "wf-bad-trigger",
			meta: map[string]string{"project_id": "42", "kind": "review", "mr_iid": "1", "trigger": "webhook"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := st.Append(ctx, tc.wfID, 0, createEvent(t, tc.wfID, tc.meta, clock.Now()))
			if err == nil {
				t.Fatal("append succeeded, want error")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if events, loadErr := st.Load(ctx, tc.wfID); loadErr != nil || len(events) != 0 {
				t.Fatalf("events after failed append = %d, err %v", len(events), loadErr)
			}
			if got := rowCount(t, st, `SELECT COUNT(*) FROM workflows WHERE id = ?`, tc.wfID); got != 0 {
				t.Fatalf("workflow rows after failed append = %d", got)
			}
			if got := rowCount(t, st, `SELECT COUNT(*) FROM workflow_nodes WHERE workflow_id = ?`, tc.wfID); got != 0 {
				t.Fatalf("node rows after failed append = %d", got)
			}
		})
	}

	onboarding := map[string]string{
		"project_id": "42",
		"kind":       "onboarding",
		"trigger":    "register",
	}
	if _, err := st.Append(ctx, "wf-onboard", 0, createEvent(t, "wf-onboard", onboarding, clock.Now())); err != nil {
		t.Fatalf("onboarding workflow without MR: %v", err)
	}
}

func TestActiveReviewUniqueness(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)

	rev := mustCreateWorkflow(t, st, "wf-first", reviewMeta(42, 7, "head"), clock.Now())
	_, err := st.Append(ctx, "wf-second", 0, createEvent(t, "wf-second", reviewMeta(42, 7, "head"), clock.Now()))
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second active review error = %v, want ErrConflict", err)
	}
	if got := rowCount(t, st, `SELECT COUNT(*) FROM workflows WHERE id = ?`, "wf-second"); got != 0 {
		t.Fatalf("conflicting workflow row = %d", got)
	}

	finishWorkflow(t, st, "wf-first", rev, clock.Now())
	if _, err := st.Append(ctx, "wf-second", 0, createEvent(t, "wf-second", reviewMeta(42, 7, "head"), clock.Now())); err != nil {
		t.Fatalf("new review after completion: %v", err)
	}
	if _, err := st.FindActiveReview(ctx, 42, 7); err != nil {
		t.Fatalf("FindActiveReview: %v", err)
	}
}

func TestListWorkflowsCursorAndPublication(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)

	ids := []string{"wf-one", "wf-two", "wf-three"}
	for i, id := range ids {
		clock.Advance(time.Minute)
		mustCreateWorkflow(t, st, id, reviewMeta(42, int64(i+1), "head-"+strconv.Itoa(i+1)), clock.Now())
	}

	first, cursor, err := st.ListWorkflows(ctx, store.WorkflowFilter{ProjectID: ptrInt64(42), Kind: "review"}, "", 2)
	if err != nil {
		t.Fatalf("ListWorkflows first page: %v", err)
	}
	if len(first) != 2 || first[0].ID != "wf-three" || first[1].ID != "wf-two" || cursor == "" {
		t.Fatalf("first page = %v cursor %q", idsOf(first), cursor)
	}
	second, next, err := st.ListWorkflows(ctx, store.WorkflowFilter{ProjectID: ptrInt64(42), Kind: "review"}, cursor, 2)
	if err != nil {
		t.Fatalf("ListWorkflows second page: %v", err)
	}
	if len(second) != 1 || second[0].ID != "wf-one" || next != "" {
		t.Fatalf("second page = %v next %q", idsOf(second), next)
	}
	if _, _, err := st.ListWorkflows(ctx, store.WorkflowFilter{}, "not-a-cursor", 2); !errors.Is(err, store.ErrInvalidCursor) {
		t.Fatalf("bad cursor error = %v", err)
	}

	filtered, _, err := st.ListWorkflows(ctx, store.WorkflowFilter{
		ProjectID: ptrInt64(42), MRIID: ptrInt64(2), Status: "pending", HeadSHA: "head-2",
	}, "", 10)
	if err != nil {
		t.Fatalf("filtered ListWorkflows: %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != "wf-two" {
		t.Fatalf("filtered = %v", idsOf(filtered))
	}

	profileFiltered, _, err := st.ListWorkflows(ctx, store.WorkflowFilter{
		ProjectID: ptrInt64(42), Profile: "default", Trigger: "mr_open",
	}, "", 10)
	if err != nil {
		t.Fatalf("profile/trigger filter: %v", err)
	}
	if len(profileFiltered) != 3 {
		t.Fatalf("profile/trigger filter = %v", idsOf(profileFiltered))
	}
	since := clock.Now().Add(-30 * time.Second)
	recent, _, err := st.ListWorkflows(ctx, store.WorkflowFilter{ProjectID: ptrInt64(42), Since: since}, "", 10)
	if err != nil {
		t.Fatalf("since filter: %v", err)
	}
	if len(recent) != 1 || recent[0].ID != "wf-three" {
		t.Fatalf("since filter = %v", idsOf(recent))
	}
	if err := st.SetWorkflowVerdict(ctx, "wf-two", "BLOCKING"); err != nil {
		t.Fatalf("SetWorkflowVerdict: %v", err)
	}
	verdictFiltered, _, err := st.ListWorkflows(ctx, store.WorkflowFilter{
		ProjectID: ptrInt64(42), Verdict: "BLOCKING",
	}, "", 10)
	if err != nil || len(verdictFiltered) != 1 || verdictFiltered[0].ID != "wf-two" {
		t.Fatalf("verdict filter = %v, %v", idsOf(verdictFiltered), err)
	}

	active, err := st.FindActiveReview(ctx, 42, 2)
	if err != nil {
		t.Fatalf("FindActiveReview: %v", err)
	}
	if active.ID != "wf-two" {
		t.Fatalf("active = %s", active.ID)
	}
	if head, err := st.LastPublishedHead(ctx, 42, 2); err != nil || head != "" {
		t.Fatalf("LastPublishedHead before publish = %q, %v", head, err)
	}

	pub, err := st.UpsertPublication(ctx, store.Publication{
		ProjectID: 42, MRIID: 2, Kind: "summary", PublishKey: "summary",
		GitlabID: "note-1", WorkflowID: "wf-two", Fingerprint: "fp-head-2",
	})
	if err != nil {
		t.Fatalf("UpsertPublication: %v", err)
	}
	if pub.ID == "" || pub.CreatedAt.IsZero() {
		t.Fatalf("publication = %+v", pub)
	}
	if head, err := st.LastPublishedHead(ctx, 42, 2); err != nil || head != "head-2" {
		t.Fatalf("LastPublishedHead after publish = %q, %v", head, err)
	}
	list, err := st.ListPublications(ctx, "wf-two")
	if err != nil || len(list) != 1 || list[0].GitlabID != "note-1" {
		t.Fatalf("ListPublications = %+v, %v", list, err)
	}
}

func TestPublicationIdempotency(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	mustCreateWorkflow(t, st, "wf-pub", reviewMeta(42, 3, "head"), clock.Now())

	first, err := st.UpsertPublication(ctx, store.Publication{
		ID: "pub-original", ProjectID: 42, MRIID: 3, Kind: "discussion",
		PublishKey: "finding-1", GitlabID: "discussion-1", WorkflowID: "wf-pub",
		Fingerprint: "fp", BodyHash: "hash-1",
	})
	if err != nil {
		t.Fatalf("first publication: %v", err)
	}
	clock.Advance(time.Minute)
	second, err := st.UpsertPublication(ctx, store.Publication{
		ID: "pub-replacement", ProjectID: 42, MRIID: 3, Kind: "discussion",
		PublishKey: "finding-1", GitlabID: "discussion-2", WorkflowID: "wf-pub",
		Fingerprint: "fp", BodyHash: "hash-2",
	})
	if err != nil {
		t.Fatalf("second publication: %v", err)
	}
	if second.ID != first.ID || !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("identity changed: first %+v second %+v", first, second)
	}
	if second.GitlabID != "discussion-2" || second.BodyHash != "hash-2" {
		t.Fatalf("publication not updated: %+v", second)
	}
	if got := rowCount(t, st, `SELECT COUNT(*) FROM publications WHERE project_id = 42 AND mr_iid = 3`); got != 1 {
		t.Fatalf("publication rows = %d", got)
	}
}

func TestFindingsReplaceAndRollback(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	mustCreateWorkflow(t, st, "wf-findings", reviewMeta(42, 4, "head"), clock.Now())

	line := 12
	ok := true
	findings := []store.Finding{
		{
			ID: "f1", OriginNode: "a", OriginIndex: 0, OriginProvider: "p",
			Severity: "P1", Category: "correctness", Path: "x.go", Line: &line,
			Side: "new", Title: "first", Body: "body", Evidence: "code",
			Confidence: "high", Votes: 2, ChallengeStatus: "confirmed",
			ChallengeNode: "verify.1", ChallengeEvidence: json.RawMessage(`{"status":"CONFIRMED"}`),
			Decision: "publish", FinalSeverity: "P1", PositionOK: &ok, PublishKey: "key-1",
		},
		{
			ID: "f2", OriginNode: "synthesize", OriginIndex: 1,
			Severity: "P2", Category: "logic", Title: "second", Body: "body2",
			Decision: "drop", DecisionReason: "no evidence",
		},
	}
	if err := st.ReplaceFindings(ctx, "wf-findings", findings); err != nil {
		t.Fatalf("ReplaceFindings: %v", err)
	}
	got, err := st.ListFindings(ctx, store.FindingFilter{WorkflowID: "wf-findings"})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(got) != 2 || got[0].ID != "f1" || got[0].Line == nil || *got[0].Line != 12 ||
		got[0].PositionOK == nil || !*got[0].PositionOK || string(got[0].ChallengeEvidence) != `{"status":"CONFIRMED"}` {
		t.Fatalf("findings = %+v", got)
	}
	filtered, err := st.ListFindings(ctx, store.FindingFilter{WorkflowID: "wf-findings", Decision: "drop"})
	if err != nil || len(filtered) != 1 || filtered[0].ID != "f2" {
		t.Fatalf("filtered findings = %+v, %v", filtered, err)
	}

	if err := st.ReplaceFindings(ctx, "wf-findings", []store.Finding{findings[0]}); err != nil {
		t.Fatalf("replace again: %v", err)
	}
	got, err = st.ListFindings(ctx, store.FindingFilter{WorkflowID: "wf-findings"})
	if err != nil || len(got) != 1 || got[0].ID != "f1" {
		t.Fatalf("findings after replace = %+v, %v", got, err)
	}

	bad := findings[0]
	bad.ID = "f3"
	bad.ChallengeEvidence = json.RawMessage(`{broken`)
	if err := st.ReplaceFindings(ctx, "wf-findings", []store.Finding{bad}); err == nil {
		t.Fatal("invalid challenge evidence accepted")
	}
	got, err = st.ListFindings(ctx, store.FindingFilter{WorkflowID: "wf-findings"})
	if err != nil || len(got) != 1 || got[0].ID != "f1" {
		t.Fatalf("failed replace changed findings: %+v, %v", got, err)
	}
}

func ptrInt64(value int64) *int64 { return &value }

func idsOf(workflows []store.Workflow) []string {
	ids := make([]string, len(workflows))
	for i, wf := range workflows {
		ids[i] = wf.ID
	}
	return ids
}
