package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

func TestLLMCallAuditGzipBoundAndRetention(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)

	secret := "prompt-text"
	apiKey := "sk-live-should-never-be-stored"
	req := &llm.Request{
		Messages: []llm.Message{{
			Role:  llm.RoleUser,
			Parts: []llm.Part{{Type: llm.PartText, Text: secret}},
		}},
		MaxTokens: 128,
	}
	resp := &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "answer"}}},
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 7, OutputTokens: 5, CacheReadTokens: 2, CacheWriteTokens: 1},
		RequestID:  "req-1",
	}
	call := llm.CallRecord{
		Provider: "p",
		Protocol: "openai-chat",
		Model:    "m",
		Meta:     llm.CallMeta{WorkflowID: "wf-audit", NodeID: "node-1", Turn: 2},
		Attempt:  0,
		Request:  req,
		Response: resp,
		Duration: 1500 * time.Millisecond,
	}
	if err := st.RecordLLMCall(ctx, call); err != nil {
		t.Fatalf("RecordLLMCall: %v", err)
	}

	var request, response []byte
	if err := st.db.QueryRowContext(ctx,
		`SELECT request, response FROM llm_calls WHERE workflow_id = ?`, "wf-audit").
		Scan(&request, &response); err != nil {
		t.Fatalf("read raw payload: %v", err)
	}
	if !bytes.HasPrefix(request, []byte{0x1f, 0x8b}) || !bytes.HasPrefix(response, []byte{0x1f, 0x8b}) {
		t.Fatal("llm payload is not gzip encoded")
	}
	if bytes.Contains(request, []byte(apiKey)) || bytes.Contains(response, []byte(apiKey)) {
		t.Fatal("llm payload stored a credential")
	}

	calls, next, err := st.ListLLMCalls(ctx, store.LLMCallFilter{WorkflowID: "wf-audit", IncludePayload: true})
	if err != nil {
		t.Fatalf("ListLLMCalls: %v", err)
	}
	if next != "" || len(calls) != 1 {
		t.Fatalf("calls = %d next = %q", len(calls), next)
	}
	if calls[0].InputTokens != 7 || calls[0].OutputTokens != 5 || calls[0].CacheRead != 2 ||
		calls[0].CacheWrite != 1 || calls[0].LatencyMS != 1500 || calls[0].StopReason != "end_turn" ||
		calls[0].RequestID != "req-1" || calls[0].Turn != 2 {
		t.Fatalf("call = %+v", calls[0])
	}
	var decoded llm.Request
	if err := json.Unmarshal(calls[0].Request, &decoded); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if decoded.Messages[0].Parts[0].Text != secret {
		t.Fatalf("request message = %q", decoded.Messages[0].Parts[0].Text)
	}
	withoutPayload, _, err := st.ListLLMCalls(ctx, store.LLMCallFilter{WorkflowID: "wf-audit"})
	if err != nil {
		t.Fatalf("ListLLMCalls without payload: %v", err)
	}
	if len(withoutPayload) != 1 || len(withoutPayload[0].Request) != 0 || len(withoutPayload[0].Response) != 0 {
		t.Fatalf("payload leaked without request: %+v", withoutPayload)
	}

	huge := llm.CallRecord{
		Provider: "p", Model: "m",
		Meta:     llm.CallMeta{WorkflowID: "wf-audit", NodeID: "node-2", Turn: 0},
		Request:  &llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: strings.Repeat("x", maxLLMPayload+10)}}}}},
		Duration: time.Second,
	}
	if err := st.RecordLLMCall(ctx, huge); err != nil {
		t.Fatalf("RecordLLMCall huge: %v", err)
	}
	calls, _, err = st.ListLLMCalls(ctx, store.LLMCallFilter{WorkflowID: "wf-audit", NodeID: "node-2", IncludePayload: true})
	if err != nil {
		t.Fatalf("ListLLMCalls huge: %v", err)
	}
	if len(calls) != 1 || !bytes.Contains(calls[0].Request, []byte(`"truncated":true`)) {
		t.Fatalf("large payload was not truncated: %+v", calls)
	}

	errorCall := llm.CallRecord{
		Provider: "p", Model: "m",
		Meta:     llm.CallMeta{WorkflowID: "wf-audit", NodeID: "node-3", Turn: 1},
		Err:      &llm.Error{Kind: llm.ErrRateLimited},
		Duration: 2 * time.Second,
	}
	var recorder llm.Recorder = st
	recorder.RecordCall(ctx, errorCall)
	calls, _, err = st.ListLLMCalls(ctx, store.LLMCallFilter{WorkflowID: "wf-audit", NodeID: "node-3"})
	if err != nil || len(calls) != 1 || calls[0].ErrorKind != "rate_limited" {
		t.Fatalf("error call = %+v, %v", calls, err)
	}

	cleared, err := st.ClearLLMPayloads(ctx, clock.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ClearLLMPayloads: %v", err)
	}
	if cleared != 2 {
		t.Fatalf("cleared rows = %d, want 2", cleared)
	}
	calls, _, err = st.ListLLMCalls(ctx, store.LLMCallFilter{WorkflowID: "wf-audit", IncludePayload: true})
	if err != nil {
		t.Fatalf("ListLLMCalls after clear: %v", err)
	}
	for _, call := range calls {
		if len(call.Request) != 0 || len(call.Response) != 0 {
			t.Fatalf("payload retained after cleanup: %+v", call)
		}
	}
	if calls[len(calls)-1].InputTokens != 7 {
		t.Fatalf("usage was removed with payload: %+v", calls)
	}
}

func TestWorkspaceAndContextLock(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)

	ws, err := st.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", BaseSHA: "old"})
	if err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	if ws.State != "ready" || ws.BaseSHA != "old" {
		t.Fatalf("workspace = %+v", ws)
	}
	if _, err := st.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "bogus"}); err == nil {
		t.Fatal("invalid workspace state accepted")
	}

	doc, err := st.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "old", Kind: "agents", Content: "v1", Model: "m1",
	})
	if err != nil {
		t.Fatalf("SaveContextDoc: %v", err)
	}
	if doc.Locked || doc.Source != "generated" || doc.ID == "" || !doc.Active {
		t.Fatalf("generated doc = %+v", doc)
	}
	if err := st.SetActiveContext(ctx, 42, "old"); err != nil {
		t.Fatalf("SetActiveContext: %v", err)
	}
	current, err := st.GetCurrentContextDoc(ctx, 42, "agents")
	if err != nil {
		t.Fatalf("GetCurrentContextDoc: %v", err)
	}
	if current.ID != doc.ID || !current.Active || current.Stale {
		t.Fatalf("current doc = %+v", current)
	}

	user := int64(7)
	if err := st.LockContextDoc(ctx, 42, "old", "agents", true, &user); err != nil {
		t.Fatalf("LockContextDoc: %v", err)
	}
	refreshed, err := st.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "old", Kind: "agents", Content: "refresh",
	})
	if err != nil {
		t.Fatalf("refresh over locked current: %v", err)
	}
	if refreshed.Active || refreshed.ID == doc.ID {
		t.Fatalf("refresh = %+v, want a new inactive version", refreshed)
	}
	locked, err := st.GetCurrentContextDoc(ctx, 42, "agents")
	if err != nil || locked.ID != doc.ID || locked.Content != "v1" || !locked.Locked {
		t.Fatalf("locked current = %+v, %v", locked, err)
	}

	manual, err := st.SaveManualContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "old", Kind: "agents", Content: "v2", Locked: true, UpdatedBy: &user,
	})
	if err != nil {
		t.Fatalf("SaveManualContextDoc: %v", err)
	}
	if manual.ID == doc.ID || manual.Content != "v2" || !manual.Locked || !manual.Active || manual.Source != "manual" {
		t.Fatalf("manual doc = %+v", manual)
	}
	if old := findContextDoc(t, listAllContextDocs(t, st, 42), doc.ID); !old.Locked || old.Content != "v1" || old.Active {
		t.Fatalf("old locked version changed: %+v", old)
	}

	if _, err := st.SaveContextDoc(ctx, store.ContextDoc{
		ProjectID: 42, BaseSHA: "new", Kind: "agents", Content: "v3",
	}); err != nil {
		t.Fatalf("SaveContextDoc new base: %v", err)
	}
	if _, err := st.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", BaseSHA: "new", ContextSHA: "old"}); err != nil {
		t.Fatalf("SaveWorkspace new base: %v", err)
	}
	old, err := st.GetContextDoc(ctx, 42, "old", "agents")
	if err != nil || !old.Stale || !old.Active || old.ID != manual.ID {
		t.Fatalf("old doc = %+v, %v", old, err)
	}
	if err := st.SetActiveContext(ctx, 42, "new"); err != nil {
		t.Fatalf("SetActiveContext with locked current: %v", err)
	}
	if current, err := st.GetCurrentContextDoc(ctx, 42, "agents"); err != nil || current.ID != manual.ID || !current.Locked {
		t.Fatalf("locked current changed by SetActiveContext: %+v, %v", current, err)
	}
	if err := st.LockContextDoc(ctx, 42, "old", "agents", false, &user); err != nil {
		t.Fatalf("unlock current: %v", err)
	}
	if err := st.SetActiveContext(ctx, 42, "new"); err != nil {
		t.Fatalf("SetActiveContext new: %v", err)
	}
	current, err = st.GetCurrentContextDoc(ctx, 42, "agents")
	if err != nil || current.BaseSHA != "new" || !current.Active {
		t.Fatalf("new current doc = %+v, %v", current, err)
	}
	if old, err := st.GetContextDoc(ctx, 42, "old", "agents"); err != nil || old.Active {
		t.Fatalf("old doc after switch = %+v, %v", old, err)
	}
	if err := st.SetActiveContext(ctx, 42, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing active context error = %v", err)
	}

	docs, cursor, err := st.ListContextDocs(ctx, store.ContextDocFilter{ProjectID: 42, Limit: 1})
	if err != nil {
		t.Fatalf("ListContextDocs: %v", err)
	}
	if len(docs) != 1 || cursor == "" {
		t.Fatalf("context page = %+v cursor %q", docs, cursor)
	}
	rest, _, err := st.ListContextDocs(ctx, store.ContextDocFilter{ProjectID: 42, Limit: 1, Cursor: cursor})
	if err != nil || len(rest) != 1 {
		t.Fatalf("context second page = %+v, %v", rest, err)
	}
	if _, _, err := st.ListContextDocs(ctx, store.ContextDocFilter{ProjectID: 42, Kind: "other"}); err == nil {
		t.Fatal("invalid context kind accepted")
	}
	if _, err := st.GetWorkspace(ctx, 99); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing workspace error = %v", err)
	}
}

func TestFeedbackUpsertNilUserAndTotals(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	mustCreateWorkflow(t, st, "wf-feedback", reviewMeta(42, 8, "head"), clock.Now())

	anon := store.Feedback{
		ProjectID: 42, MRIID: 8, PublicationID: "pub-1", FindingID: "f1",
		Signal: "thumbsup",
	}
	first, err := st.UpsertFeedback(ctx, anon)
	if err != nil {
		t.Fatalf("anonymous feedback: %v", err)
	}
	anon.FindingID = "f2"
	second, err := st.UpsertFeedback(ctx, anon)
	if err != nil {
		t.Fatalf("anonymous feedback repeat: %v", err)
	}
	if second.ID != first.ID || second.FindingID != "f2" {
		t.Fatalf("anonymous upsert = %+v -> %+v", first, second)
	}
	if got := rowCount(t, st, `SELECT COUNT(*) FROM feedback WHERE publication_id = 'pub-1' AND gitlab_user_id IS NULL`); got != 1 {
		t.Fatalf("anonymous feedback rows = %d", got)
	}

	user := int64(5)
	named := store.Feedback{
		ProjectID: 42, MRIID: 8, PublicationID: "pub-1", FindingID: "f1",
		Signal: "thumbsup", GitlabUserID: &user,
	}
	if _, err := st.UpsertFeedback(ctx, named); err != nil {
		t.Fatalf("named feedback: %v", err)
	}
	named.FindingID = "f2"
	namedAgain, err := st.UpsertFeedback(ctx, named)
	if err != nil {
		t.Fatalf("named feedback repeat: %v", err)
	}
	if namedAgain.FindingID != "f2" {
		t.Fatalf("named feedback = %+v", namedAgain)
	}
	if got := rowCount(t, st, `SELECT COUNT(*) FROM feedback`); got != 2 {
		t.Fatalf("feedback rows = %d, want 2", got)
	}

	totals, err := st.FeedbackTotals(ctx, store.FeedbackFilter{ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("FeedbackTotals: %v", err)
	}
	if totals.Total != 2 || totals.BySignal["thumbsup"] != 2 {
		t.Fatalf("totals = %+v", totals)
	}
	items, cursor, err := st.ListFeedback(ctx, store.FeedbackFilter{ProjectID: ptrInt64(42), Limit: 1})
	if err != nil || len(items) != 1 || cursor == "" {
		t.Fatalf("feedback page = %+v cursor %q, %v", items, cursor, err)
	}
	rest, next, err := st.ListFeedback(ctx, store.FeedbackFilter{ProjectID: ptrInt64(42), Limit: 1, Cursor: cursor})
	if err != nil || len(rest) != 1 || next != "" {
		t.Fatalf("feedback second page = %+v next %q, %v", rest, next, err)
	}
}

func TestStatsFeedbackTotalsHonorFilters(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	mustProject(t, st, 43)
	base := clock.Now().UTC()
	items := []store.Feedback{
		{ProjectID: 42, MRIID: 1, PublicationID: "before", Signal: "thumbsdown", ObservedAt: base.Add(-time.Nanosecond)},
		{ProjectID: 42, MRIID: 1, PublicationID: "from", Signal: "thumbsup", ObservedAt: base},
		{ProjectID: 42, MRIID: 1, PublicationID: "inside", Signal: "thumbsup", ObservedAt: base.Add(time.Hour)},
		{ProjectID: 42, MRIID: 1, PublicationID: "to", Signal: "resolved_changed", ObservedAt: base.Add(2 * time.Hour)},
		{ProjectID: 43, MRIID: 1, PublicationID: "other-project", Signal: "thumbsdown", ObservedAt: base.Add(time.Hour)},
	}
	for _, item := range items {
		if _, err := st.UpsertFeedback(ctx, item); err != nil {
			t.Fatalf("UpsertFeedback(%s): %v", item.PublicationID, err)
		}
	}

	stats, err := st.Stats(ctx, store.StatsFilter{
		From:      base,
		To:        base.Add(2 * time.Hour),
		ProjectID: ptrInt64(42),
	})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Feedback == nil {
		t.Fatal("Stats feedback = nil, want known counts")
	}
	if stats.Feedback.Total != 2 || stats.Feedback.BySignal["thumbsup"] != 2 {
		t.Fatalf("feedback = %+v, want two in-window thumbsup", *stats.Feedback)
	}
	if _, ok := stats.Feedback.BySignal["thumbsdown"]; ok {
		t.Fatalf("feedback includes out-of-window or other-project signals: %+v", *stats.Feedback)
	}

	empty, err := st.Stats(ctx, store.StatsFilter{
		From:      base.Add(3 * time.Hour),
		To:        base.Add(4 * time.Hour),
		ProjectID: ptrInt64(42),
	})
	if err != nil {
		t.Fatalf("Stats empty: %v", err)
	}
	if empty.Feedback == nil || empty.Feedback.Total != 0 || empty.Feedback.BySignal == nil {
		t.Fatalf("empty feedback = %+v, want known zero counts", empty.Feedback)
	}
}

// attributedFeedbackFixture creates one review workflow whose single finding
// has a proposer and an optional challenger provider.
func attributedFeedbackFixture(t *testing.T, st *Store, workflowID string, projectID, mrIID int64, findingID, proposer, challenger, publicationID, publishKey string) {
	t.Helper()
	ctx := context.Background()
	at := st.now().UTC()
	rev := mustCreateWorkflow(t, st, workflowID, reviewMeta(projectID, mrIID, "head-"+workflowID), at)
	if challenger != "" {
		rev = mustAppendEvent(t, st, workflowID, rev, queuedEvent(t, "a", 1, at))
		rev = mustAppendEvent(t, st, workflowID, rev, startedEvent(t, "a", 1, challenger, "m", at))
		// An unrelated node with a different provider must not multiply or
		// attribute the feedback rows joined through the challenge node.
		rev = mustAppendEvent(t, st, workflowID, rev, queuedEvent(t, "b", 1, at))
		_ = mustAppendEvent(t, st, workflowID, rev, startedEvent(t, "b", 1, "extra-provider", "m", at))
	}
	findings := []store.Finding{{
		ID: findingID, OriginNode: "a", OriginIndex: 0, OriginProvider: proposer,
		Severity: "P1", Category: "correctness", Title: "title", Body: "body",
		ChallengeNode: "a", PublishKey: publishKey,
	}}
	if err := st.ReplaceFindings(ctx, workflowID, findings); err != nil {
		t.Fatalf("ReplaceFindings(%s): %v", workflowID, err)
	}
	if _, err := st.UpsertPublication(ctx, store.Publication{
		ID: publicationID, ProjectID: projectID, MRIID: mrIID, Kind: "discussion",
		PublishKey: publishKey, GitlabID: "gl-" + publicationID, WorkflowID: workflowID,
		Fingerprint: "fp-" + publicationID,
	}); err != nil {
		t.Fatalf("UpsertPublication(%s): %v", publicationID, err)
	}
}

func recordTestFeedback(t *testing.T, st *Store, projectID, mrIID int64, publicationID, findingID, signal string, user int64, observedAt time.Time) {
	t.Helper()
	if _, err := st.UpsertFeedback(context.Background(), store.Feedback{
		ProjectID: projectID, MRIID: mrIID, PublicationID: publicationID,
		FindingID: findingID, Signal: signal, GitlabUserID: ptrInt64(user),
		ObservedAt: observedAt,
	}); err != nil {
		t.Fatalf("UpsertFeedback(%s/%s): %v", publicationID, signal, err)
	}
}

func providerStatsIndex(stats []store.FeedbackProviderStats) map[string]store.FeedbackProviderStats {
	out := make(map[string]store.FeedbackProviderStats, len(stats))
	for _, entry := range stats {
		out[entry.Provider+"/"+entry.Role] = entry
	}
	return out
}

func TestStatsFeedbackByProvider(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	mustProject(t, st, 43)
	at := clock.Now().UTC()

	// Known review: proposer anthropic, challenger openai.
	attributedFeedbackFixture(t, st, "wf-known", 42, 7, "f-known", "anthropic", "openai", "p-known", "known-key")
	// Same provider owns both roles.
	attributedFeedbackFixture(t, st, "wf-self", 42, 8, "f-self", "anthropic", "anthropic", "p-self", "self-key")
	// Challenge node was never started, so the challenger stays unknown.
	attributedFeedbackFixture(t, st, "wf-unknown", 42, 9, "f-unknown", "google", "", "p-unknown", "unknown-key")
	attributedFeedbackFixture(t, st, "wf-other", 43, 7, "f-other", "other", "", "p-other", "other-key")

	// Excluded publications: summary, unlinked, mismatched project/MR, and a
	// publication pointing at a finding from another workflow.
	for _, publication := range []store.Publication{
		{ID: "p-summary", ProjectID: 42, MRIID: 7, Kind: "summary", PublishKey: "summary-key", GitlabID: "summary", WorkflowID: "wf-known", Fingerprint: "fp-summary"},
		{ID: "p-unlinked", ProjectID: 42, MRIID: 7, Kind: "discussion", PublishKey: "unlinked-key", GitlabID: "unlinked", WorkflowID: "wf-known", Fingerprint: "fp-unlinked"},
		{ID: "p-mismatch", ProjectID: 42, MRIID: 8, Kind: "discussion", PublishKey: "mismatch-key", GitlabID: "mismatch", WorkflowID: "wf-known", Fingerprint: "fp-mismatch"},
		{ID: "p-cross", ProjectID: 42, MRIID: 8, Kind: "discussion", PublishKey: "cross-key", GitlabID: "cross", WorkflowID: "wf-self", Fingerprint: "fp-cross"},
	} {
		if _, err := st.UpsertPublication(ctx, publication); err != nil {
			t.Fatalf("UpsertPublication(%s): %v", publication.ID, err)
		}
	}

	recordTestFeedback(t, st, 42, 7, "p-known", "f-known", "thumbsup", 1, at)
	recordTestFeedback(t, st, 42, 7, "p-known", "f-known", "thumbsup", 11, at)
	recordTestFeedback(t, st, 42, 7, "p-known", "f-known", "thumbsdown", 2, at)
	recordTestFeedback(t, st, 42, 7, "p-known", "f-known", "resolved_changed", 3, at)
	recordTestFeedback(t, st, 42, 8, "p-self", "f-self", "thumbsup", 4, at)
	recordTestFeedback(t, st, 42, 9, "p-unknown", "f-unknown", "thumbsup", 5, at)
	recordTestFeedback(t, st, 42, 7, "p-summary", "f-known", "thumbsup", 6, at)
	recordTestFeedback(t, st, 42, 7, "p-unlinked", "", "thumbsup", 7, at)
	recordTestFeedback(t, st, 42, 7, "p-mismatch", "f-known", "thumbsdown", 8, at)
	recordTestFeedback(t, st, 42, 7, "p-cross", "f-known", "thumbsup", 9, at)
	recordTestFeedback(t, st, 43, 7, "p-other", "f-other", "thumbsup", 10, at)

	stats, err := st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Feedback == nil {
		t.Fatal("Stats feedback = nil")
	}
	if stats.Feedback.Total != 10 || stats.Feedback.BySignal["thumbsup"] != 7 ||
		stats.Feedback.BySignal["thumbsdown"] != 2 || stats.Feedback.BySignal["resolved_changed"] != 1 {
		t.Fatalf("raw feedback = %+v, want all ten project-42 signals unchanged", *stats.Feedback)
	}

	got := providerStatsIndex(stats.Feedback.ByProvider)
	if len(got) != 4 {
		t.Fatalf("by_provider = %+v, want four provider/role records", stats.Feedback.ByProvider)
	}
	if _, ok := got["extra-provider/challenger"]; ok {
		t.Fatalf("unrelated node provider multiplied feedback: %+v", stats.Feedback.ByProvider)
	}
	want := map[string]store.FeedbackProviderStats{
		"anthropic/proposer": {
			Provider: "anthropic", Role: "proposer", Total: 5,
			BySignal: map[string]int64{"thumbsup": 3, "thumbsdown": 1, "resolved_changed": 1},
		},
		"anthropic/challenger": {
			Provider: "anthropic", Role: "challenger", Total: 1,
			BySignal: map[string]int64{"thumbsup": 1},
		},
		"google/proposer": {
			Provider: "google", Role: "proposer", Total: 1,
			BySignal: map[string]int64{"thumbsup": 1},
		},
		"openai/challenger": {
			Provider: "openai", Role: "challenger", Total: 4,
			BySignal: map[string]int64{"thumbsup": 2, "thumbsdown": 1, "resolved_changed": 1},
		},
	}
	for key, expected := range want {
		actual, ok := got[key]
		if !ok {
			t.Fatalf("by_provider missing %s: %+v", key, stats.Feedback.ByProvider)
		}
		if actual.Role != expected.Role || actual.Total != expected.Total || len(actual.BySignal) != len(expected.BySignal) {
			t.Fatalf("by_provider[%s] = %+v, want %+v", key, actual, expected)
		}
		for signal, count := range expected.BySignal {
			if actual.BySignal[signal] != count {
				t.Fatalf("by_provider[%s].by_signal[%s] = %d, want %d", key, signal, actual.BySignal[signal], count)
			}
		}
	}
	for i := 1; i < len(stats.Feedback.ByProvider); i++ {
		previous, current := stats.Feedback.ByProvider[i-1], stats.Feedback.ByProvider[i]
		if previous.Provider > current.Provider ||
			(previous.Provider == current.Provider && previous.Role > current.Role) {
			t.Fatalf("by_provider order is not stable: %+v", stats.Feedback.ByProvider)
		}
	}
}

func TestStatsFeedbackByProviderHonorsObservedWindow(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	mustProject(t, st, 43)
	base := clock.Now().UTC()
	attributedFeedbackFixture(t, st, "wf-window", 42, 10, "f-window", "anthropic", "openai", "p-window", "window-key")
	attributedFeedbackFixture(t, st, "wf-outside", 43, 10, "f-outside", "other", "", "p-outside", "outside-key")

	recordTestFeedback(t, st, 42, 10, "p-window", "f-window", "thumbsup", 1, base.Add(-time.Nanosecond))
	recordTestFeedback(t, st, 42, 10, "p-window", "f-window", "thumbsup", 2, base)
	recordTestFeedback(t, st, 42, 10, "p-window", "f-window", "thumbsdown", 3, base.Add(time.Hour))
	recordTestFeedback(t, st, 42, 10, "p-window", "f-window", "thumbsdown", 4, base.Add(2*time.Hour))
	recordTestFeedback(t, st, 43, 10, "p-outside", "f-outside", "thumbsup", 5, base.Add(time.Hour))

	stats, err := st.Stats(ctx, store.StatsFilter{
		From: base, To: base.Add(2 * time.Hour), ProjectID: ptrInt64(42),
	})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Feedback == nil || stats.Feedback.Total != 2 {
		t.Fatalf("raw feedback = %+v, want [from,to) project-42 rows", stats.Feedback)
	}
	got := providerStatsIndex(stats.Feedback.ByProvider)
	proposer, ok := got["anthropic/proposer"]
	if !ok || proposer.Total != 2 || proposer.BySignal["thumbsup"] != 1 || proposer.BySignal["thumbsdown"] != 1 {
		t.Fatalf("proposer stats = %+v", proposer)
	}
	challenger, ok := got["openai/challenger"]
	if !ok || challenger.Total != 2 {
		t.Fatalf("challenger stats = %+v", challenger)
	}
	if _, ok := got["other/proposer"]; ok {
		t.Fatalf("other-project feedback leaked into by_provider: %+v", stats.Feedback.ByProvider)
	}
	if len(stats.Feedback.ByProvider) != 2 {
		t.Fatalf("by_provider = %+v, want proposer and challenger only", stats.Feedback.ByProvider)
	}
}

func TestStatsFeedbackByProviderEmptyIsKnown(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	mustProject(t, st, 42)

	stats, err := st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Feedback == nil || stats.Feedback.ByProvider == nil || len(stats.Feedback.ByProvider) != 0 {
		t.Fatalf("empty by_provider = %+v, want a known empty slice", stats.Feedback)
	}
}

func TestStatsReturnsFeedbackError(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	mustProject(t, st, 42)
	if _, err := st.db.ExecContext(ctx, `DROP TABLE feedback`); err != nil {
		t.Fatalf("drop feedback: %v", err)
	}
	_, err := st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(42)})
	if err == nil || !strings.Contains(err.Error(), "feedback") {
		t.Fatalf("Stats error = %v, want a feedback error", err)
	}
}

func TestStatsAggregates(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)

	wfID := "wf-stats"
	rev := mustCreateWorkflow(t, st, wfID, reviewMeta(42, 9, "head"), clock.Now())
	clock.Advance(2 * time.Second)
	startedAt := clock.Now()
	rev = mustAppendEvent(t, st, wfID, rev, testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, startedAt))
	rev = mustAppendEvent(t, st, wfID, rev, queuedEvent(t, "a", 1, startedAt))
	rev = mustAppendEvent(t, st, wfID, rev, startedEvent(t, "a", 1, "p", "m", startedAt))
	rev = mustAppendEvent(t, st, wfID, rev, completedEvent(t, "a", 1, 5, startedAt))
	rev = mustAppendEvent(t, st, wfID, rev, queuedEvent(t, "b", 1, startedAt))
	rev = mustAppendEvent(t, st, wfID, rev, startedEvent(t, "b", 1, "p", "m", startedAt))
	rev = mustAppendEvent(t, st, wfID, rev, completedEvent(t, "b", 1, 7, startedAt))
	clock.Advance(2 * time.Second)
	finishedAt := clock.Now()
	if _, err := st.Append(ctx, wfID, rev, testEvent(t, dag.EventWorkflowCompleted, dag.WorkflowTerminalPayload{}, finishedAt)); err != nil {
		t.Fatalf("complete workflow: %v", err)
	}
	if err := st.SetWorkflowVerdict(ctx, wfID, "BLOCKING"); err != nil {
		t.Fatalf("SetWorkflowVerdict: %v", err)
	}
	findings := []store.Finding{
		{ID: "f-publish", OriginNode: "a", OriginIndex: 0, Severity: "P1", Category: "correctness", Title: "a", Body: "b", Decision: "publish"},
		{ID: "f-drop", OriginNode: "a", OriginIndex: 1, Severity: "P2", Category: "logic", Title: "b", Body: "b", Decision: "drop"},
		{ID: "f-mark", OriginNode: "a", OriginIndex: 2, Severity: "P2", Category: "logic", Title: "c", Body: "c", Decision: "mark"},
	}
	if err := st.ReplaceFindings(ctx, wfID, findings); err != nil {
		t.Fatalf("ReplaceFindings: %v", err)
	}
	if err := st.RecordLLMCall(ctx, llm.CallRecord{
		Provider: "p", Model: "m",
		Meta:     llm.CallMeta{WorkflowID: wfID, NodeID: "a", Turn: 1},
		Response: &llm.Response{Usage: llm.Usage{InputTokens: 10, OutputTokens: 4}, StopReason: llm.StopEndTurn},
		Duration: time.Second,
	}); err != nil {
		t.Fatalf("RecordLLMCall: %v", err)
	}
	if err := st.RecordLLMCall(ctx, llm.CallRecord{
		Provider: "p", Model: "m",
		Meta:     llm.CallMeta{WorkflowID: wfID, NodeID: "a", Turn: 2},
		Err:      &llm.Error{Kind: llm.ErrTransient},
		Duration: 3 * time.Second,
	}); err != nil {
		t.Fatalf("RecordLLMCall error: %v", err)
	}

	stats, err := st.Stats(ctx, store.StatsFilter{
		From:      clock.Now().Add(-time.Hour),
		To:        clock.Now().Add(time.Hour),
		ProjectID: ptrInt64(42),
	})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Runs != 1 || stats.Blocking != 1 || stats.Published != 2 {
		t.Fatalf("stats counts = %+v", stats)
	}
	if stats.BlockingRatio == nil || *stats.BlockingRatio != 1 {
		t.Fatalf("blocking ratio = %v", stats.BlockingRatio)
	}
	if stats.OverturnedRatio == nil || *stats.OverturnedRatio != 1.0/3.0 {
		t.Fatalf("overturned ratio = %v", stats.OverturnedRatio)
	}
	if stats.AvgDurationMS == nil || *stats.AvgDurationMS != 2000 || stats.P95DurationMS == nil || *stats.P95DurationMS != 2000 {
		t.Fatalf("durations = %v / %v", stats.AvgDurationMS, stats.P95DurationMS)
	}
	if stats.Decisions["publish"] != 1 || stats.Decisions["drop"] != 1 || stats.Decisions["mark"] != 1 {
		t.Fatalf("decisions = %+v", stats.Decisions)
	}
	if stats.CostMicros != 12 {
		t.Fatalf("cost = %d", stats.CostMicros)
	}
	if len(stats.ByProject) != 1 || stats.ByProject[0].ProjectID != 42 ||
		stats.ByProject[0].Runs != 1 || stats.ByProject[0].Blocking != 1 ||
		stats.ByProject[0].Published != 2 || stats.ByProject[0].CostMicros != 12 {
		t.Fatalf("by project = %+v", stats.ByProject)
	}
	if len(stats.ByModel) != 1 {
		t.Fatalf("by model = %+v", stats.ByModel)
	}
	model := stats.ByModel[0]
	if model.Provider != "p" || model.Model != "m" || model.Calls != 2 || model.Errors != 1 ||
		model.InputTokens != 10 || model.OutputTokens != 4 || model.CostMicros != 12 ||
		model.AvgLatencyMS == nil || *model.AvgLatencyMS != 2000 ||
		model.P50LatencyMS == nil || *model.P50LatencyMS != 1000 ||
		model.ErrorRate == nil || *model.ErrorRate != 0.5 {
		t.Fatalf("model stats = %+v", model)
	}
}
