package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

const (
	indexGapMessage603 = "block explore: MCP tools unavailable: 7 bound tool(s) blocked by workspace index identity (index abcdef01, target 33333333)"
	legacyGapMessage   = `block code-review: MCP tools unavailable: tool "mcp__codebase-memory__search_graph" blocked by workspace index identity`
	serverGapMessage   = `block review: MCP tools unavailable: mcp: server "codebase-memory": connect failed`

	neutralGapMessage603 = "block review: MCP tools unavailable: 7 bound tool(s) blocked by workspace index identity (index unknown, target unknown)"

	indexMismatchText    = "代码图谱索引（abcdef01）与本 MR 目标提交（33333333）不一致，本次未启用代码图谱工具，相关范围可能未被完整评审。"
	indexUnconfirmedText = "无法确认代码图谱索引与本 MR 目标提交一致（工作区索引未就绪或索引失败），本次未启用代码图谱工具，相关范围可能未被完整评审。"
	genericToolText      = "部分评审工具不可用，相关范围可能未被完整评审。"

	// storedSummary603 is the stored form of a deterministic incomplete summary
	// whose first cause is shared by three nodes.
	storedSummary603 = "评审未完成：上游评审存在未解决的证据不足或阻断，不能给出 PASS。具体缺口：" +
		"explore 等 3 个节点: " + indexGapMessage603 + "；" +
		"coverage: pipeline log unavailable；" +
		"review.gate: review gate reported BLOCKING with no accepted findings；另有 2 项"
)

// seedSynthesisWorkflow stores a completed review whose synthesize output
// carries the given review-level fields (summary, warnings, unresolved). The
// workflow also has a completed review node for each of nodeIDs.
func seedSynthesisWorkflow(env *m6Env, wfID, verdict string, fields map[string]any, nodeIDs ...string) {
	mrIID := int64(11)
	started := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Minute)
	fake := env.store.(*fakeReviewStore)
	fake.workflows = append(fake.workflows, store.Workflow{
		ID: wfID, Kind: "review", ProjectID: 42, MRIID: &mrIID,
		Trigger: "mr_update", Status: "completed", Verdict: verdict,
		CreatedAt: started, StartedAt: &started, FinishedAt: &finished,
	})
	body := map[string]any{
		"verdict": verdict,
		"groups":  []any{},
		"output":  map[string]any{"verdict": verdict, "summary": "未完成", "decisions": []any{}},
	}
	for key, value := range fields {
		body[key] = value
	}
	synthesis, _ := json.Marshal(body)
	nodes := make([]store.WorkflowNode, 0, len(nodeIDs)+1)
	for _, id := range nodeIDs {
		nodes = append(nodes, store.WorkflowNode{WorkflowID: wfID, ID: id, Kind: "code-review", Status: "completed", Attempt: 1})
	}
	nodes = append(nodes, store.WorkflowNode{
		WorkflowID: wfID, ID: "synthesize", Kind: "synthesize", Status: "completed",
		Required: true, Attempt: 1, Output: synthesis,
	})
	fake.setNodes(wfID, nodes...)
}

// seedUnresolvedWorkflow stores a completed INCOMPLETE review whose synthesize
// output carries the given stored summary and unresolved signals.
func seedUnresolvedWorkflow(env *m6Env, wfID, summary string, unresolved []map[string]string) {
	seedSynthesisWorkflow(env, wfID, "INCOMPLETE", map[string]any{"summary": summary, "unresolved": unresolved})
}

func stringList(t *testing.T, value any) []string {
	t.Helper()
	raw, ok := value.([]any)
	if !ok {
		t.Fatalf("value = %#v, want a JSON array", value)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		out = append(out, item.(string))
	}
	return out
}

func unresolvedItems(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["unresolved"].([]any)
	if !ok {
		t.Fatalf("unresolved = %v, want a list", body["unresolved"])
	}
	items := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		items = append(items, item.(map[string]any))
	}
	return items
}

func TestReviewDetailClassifiesAndLocalizesUnresolvedSignals(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedUnresolvedWorkflow(env, "wf-603", storedSummary603, []map[string]string{
		{"node": "explore", "kind": "evidence_gap", "message": indexGapMessage603},
		{"node": "cr.p1", "kind": "evidence_gap", "message": legacyGapMessage},
		{"node": "rv.p1", "kind": "evidence_gap", "message": serverGapMessage},
		{"node": "coverage", "kind": "evidence_gap", "message": "pipeline log unavailable"},
		{"node": "review.gate", "kind": "review_blocking", "message": "review gate reported BLOCKING with no accepted findings"},
	})
	response := env.businessClient().request(http.MethodGet, "/api/v1/reviews/wf-603", nil, nil)
	assertStatus(t, response, http.StatusOK)
	items := unresolvedItems(t, env.decode(t, response))
	if len(items) != 5 {
		t.Fatalf("unresolved = %v, want 5 items", items)
	}

	want := []struct {
		node, kind, message, category, text string
	}{
		{"explore", "evidence_gap", indexGapMessage603, "tool_unavailable", indexMismatchText},
		{"cr.p1", "evidence_gap", legacyGapMessage, "tool_unavailable", indexUnconfirmedText},
		{"rv.p1", "evidence_gap", serverGapMessage, "tool_unavailable", genericToolText},
		// No template maps this message: the raw text stays the readable text.
		{"coverage", "evidence_gap", "pipeline log unavailable", "evidence_gap", "pipeline log unavailable"},
		{"review.gate", "review_blocking", "review gate reported BLOCKING with no accepted findings", "review_blocking", "复核通道判定阻断，但未接受任何具体问题。"},
	}
	for i, tc := range want {
		item := items[i]
		if item["node"] != tc.node || item["kind"] != tc.kind || item["message"] != tc.message {
			t.Fatalf("unresolved[%d] changed an existing field: %v", i, item)
		}
		if item["category"] != tc.category || item["text"] != tc.text {
			t.Fatalf("unresolved[%d] category/text = %v / %v, want %s / %s", i, item["category"], item["text"], tc.category, tc.text)
		}
		keys := make([]string, 0, len(item))
		for key := range item {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != "category,kind,message,node,text" {
			t.Fatalf("unresolved[%d] keys = %v, want only the existing fields plus category and text", i, keys)
		}
	}
}

func TestReviewDetailLocalizesTheIncompleteSummaryAsPlainText(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedUnresolvedWorkflow(env, "wf-603-summary", storedSummary603, []map[string]string{
		{"node": "explore", "kind": "evidence_gap", "message": indexGapMessage603},
	})
	response := env.businessClient().request(http.MethodGet, "/api/v1/reviews/wf-603-summary", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)

	if body["summary"] != storedSummary603 {
		t.Fatalf("stored summary changed: %v", body["summary"])
	}
	want := "评审未完成：上游评审存在未解决的证据不足或阻断，不能给出 PASS。具体缺口：" +
		"代码图谱索引（abcdef01）与本 MR 目标提交（33333333）不一致，本次未启用代码图谱工具，相关范围可能未被完整评审；" +
		"变更文件收集未完成，具体原因与日志见控制台；" +
		"复核通道判定阻断，但未接受任何具体问题。另有 2 项检查未完成。"
	if body["summary_text"] != want {
		t.Fatalf("summary_text = %v\nwant %s", body["summary_text"], want)
	}
	if _, present := body["warnings_text"]; present {
		t.Fatalf("warnings_text = %v for a review without warnings", body["warnings_text"])
	}
	text := body["summary_text"].(string)
	for _, banned := range []string{"**", "\n", "MCP tools", "explore", "等 3 个节点"} {
		if strings.Contains(text, banned) {
			t.Fatalf("summary_text contains %q: %s", banned, text)
		}
	}
}

// A summary that is not the deterministic incomplete summary has no localized
// form; the field stays absent and every existing field is unchanged.
func TestReviewDetailOmitsSummaryTextForOtherSummaries(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedDiagnosticWorkflow(env)
	response := env.businessClient().request(http.MethodGet, "/api/v1/reviews/wf-diag", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)

	if _, present := body["summary_text"]; present {
		t.Fatalf("summary_text = %v for a summary that is not an incomplete summary", body["summary_text"])
	}
	if body["summary"] != "评审未完成：上游证据不足，不能给出 PASS。" {
		t.Fatalf("summary = %v", body["summary"])
	}
	items := unresolvedItems(t, body)
	if len(items) != 2 {
		t.Fatalf("unresolved = %v", items)
	}
	// Chinese prose from a model or an older build is already readable text.
	first := items[0]
	if first["message"] != "缺少流水线日志" || first["text"] != "缺少流水线日志" || first["category"] != "evidence_gap" {
		t.Fatalf("unresolved[0] = %v", first)
	}
	second := items[1]
	if second["category"] != "review_blocking" || second["text"] != "档位给出阻断但没有任何 finding" {
		t.Fatalf("unresolved[1] = %v", second)
	}
}

// A review with no unresolved signals and no summary keeps its response shape.
func TestReviewDetailWithoutDiagnosticsAddsNoNewFields(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	mrIID := int64(12)
	started := time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC)
	fake.workflows = append(fake.workflows, store.Workflow{
		ID: "wf-603-clean", Kind: "review", ProjectID: 42, MRIID: &mrIID,
		Trigger: "mr_update", Status: "completed", Verdict: "PASS", CreatedAt: started,
	})
	fake.setNodes("wf-603-clean",
		store.WorkflowNode{WorkflowID: "wf-603-clean", ID: "plan", Kind: "plan", Status: "completed", Required: true, Attempt: 1},
	)
	response := env.businessClient().request(http.MethodGet, "/api/v1/reviews/wf-603-clean", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)
	for _, key := range []string{"summary", "summary_text", "unresolved", "warnings", "warnings_text"} {
		if _, present := body[key]; present {
			t.Fatalf("%s = %v on a review without diagnostics", key, body[key])
		}
	}
}

func TestReviewDetailLocalizesWarningsAndDropsWhatUnresolvedAlreadyShows(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	warnings := []string{
		"explore: " + indexGapMessage603,
		"cr.p1: " + indexGapMessage603,
		"rv.p1: " + neutralGapMessage603,
		"reasoner: " + neutralGapMessage603,
		"coverage: pipeline log unavailable",
		"unresolved evidence or review-level blocking cannot be settled as PASS",
		"验证通道 BLOCKED：没有流水线",
	}
	seedSynthesisWorkflow(env, "wf-603-warnings", "INCOMPLETE", map[string]any{
		"summary":  storedSummary603,
		"warnings": warnings,
		"unresolved": []map[string]string{
			{"node": "explore", "kind": "evidence_gap", "message": indexGapMessage603},
			{"node": "cr.p1", "kind": "evidence_gap", "message": indexGapMessage603},
			{"node": "coverage", "kind": "evidence_gap", "message": "pipeline log unavailable"},
		},
	}, "explore", "cr.p1", "rv.p1", "reasoner")
	response := env.businessClient().request(http.MethodGet, "/api/v1/reviews/wf-603-warnings", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)

	if got := stringList(t, body["warnings"]); !reflect.DeepEqual(got, warnings) {
		t.Fatalf("warnings changed:\n got %q\nwant %q", got, warnings)
	}
	// The explore and cr.p1 gaps and the coverage gap are already unresolved
	// signal groups, so only the other causes remain, merged by text.
	want := []string{
		indexUnconfirmedText + "（rv.p1 · reasoner）",
		"存在未解决的证据不足或阻断，不能判定为通过。",
		"验证通道 BLOCKED：没有流水线",
	}
	if got := stringList(t, body["warnings_text"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("warnings_text:\n got %q\nwant %q", got, want)
	}
}

// A BLOCKING review keeps no unresolved list, so its warnings are the only
// place the gaps appear and none is dropped.
func TestReviewDetailAggregatesTheWarningsOfABlockingReview(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedSynthesisWorkflow(env, "wf-603-blocking", "BLOCKING", map[string]any{
		"summary": "发现 1 个 P1 问题。",
		"warnings": []string{
			"explore: " + indexGapMessage603,
			"cr.p1: " + indexGapMessage603,
			"budget exhausted: some findings were not verified",
		},
	}, "explore", "cr.p1")
	response := env.businessClient().request(http.MethodGet, "/api/v1/reviews/wf-603-blocking", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)

	if _, present := body["unresolved"]; present {
		t.Fatalf("a BLOCKING review carries unresolved = %v", body["unresolved"])
	}
	want := []string{
		indexMismatchText + "（explore · cr.p1）",
		"预算耗尽：部分问题未完成二次验证，已按存疑处理。",
	}
	if got := stringList(t, body["warnings_text"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("warnings_text:\n got %q\nwant %q", got, want)
	}
}

// Every warning repeats an unresolved signal: the field is still sent, as an
// empty array, so the console does not fall back to the raw warnings.
func TestReviewDetailSendsAnEmptyWarningsTextWhenEveryWarningRepeats(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedSynthesisWorkflow(env, "wf-603-repeat", "INCOMPLETE", map[string]any{
		"summary":    storedSummary603,
		"warnings":   []string{"explore: " + indexGapMessage603},
		"unresolved": []map[string]string{{"node": "explore", "kind": "evidence_gap", "message": indexGapMessage603}},
	}, "explore")
	response := env.businessClient().request(http.MethodGet, "/api/v1/reviews/wf-603-repeat", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)

	if got := stringList(t, body["warnings"]); len(got) != 1 {
		t.Fatalf("warnings = %q", got)
	}
	value, present := body["warnings_text"]
	if !present {
		t.Fatal("warnings_text is absent although the review has warnings")
	}
	if got := stringList(t, value); len(got) != 0 {
		t.Fatalf("warnings_text = %q, want an empty array", got)
	}
}

// The warnings the existing diagnostics fixture stores are sanitized first, so
// the localized lines never carry the stored verify diagnostic.
func TestReviewDetailLocalizesTheSanitizedWarnings(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedDiagnosticWorkflow(env)
	response := env.businessClient().request(http.MethodGet, "/api/v1/reviews/wf-diag", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)

	want := []string{"验证通道 BLOCKED：没有流水线", "部分问题的二次验证未完成，已按存疑处理。"}
	if got := stringList(t, body["warnings_text"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("warnings_text:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(string(response.body), "leaked upstream body") {
		t.Fatalf("response leaks the stored verify diagnostic: %s", response.body)
	}
}

// A warning is read as "<node>: <message>" only when the prefix names a node of
// this workflow; any other prefix is part of the warning.
func TestReviewDetailKeepsWarningsWithAnUnknownPrefixWhole(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedSynthesisWorkflow(env, "wf-603-prefix", "BLOCKING", map[string]any{
		"summary": "发现 1 个 P1 问题。",
		"warnings": []string{
			"error: " + indexGapMessage603,
			"P1: missing null check in the loader",
			"explore: " + indexGapMessage603,
		},
	}, "explore")
	response := env.businessClient().request(http.MethodGet, "/api/v1/reviews/wf-603-prefix", nil, nil)
	assertStatus(t, response, http.StatusOK)
	want := []string{
		"error: " + indexGapMessage603,
		"P1: missing null check in the loader",
		indexMismatchText + "（explore）",
	}
	if got := stringList(t, env.decode(t, response)["warnings_text"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("warnings_text:\n got %q\nwant %q", got, want)
	}
}

// The unresolved group lists explore only, so the same cause on cr.p1 is not a
// repeat and stays in the warnings.
func TestReviewDetailDropsAWarningOnlyForTheNodeOfItsUnresolvedGroup(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedSynthesisWorkflow(env, "wf-603-node", "INCOMPLETE", map[string]any{
		"summary":    storedSummary603,
		"warnings":   []string{"explore: " + indexGapMessage603, "cr.p1: " + indexGapMessage603},
		"unresolved": []map[string]string{{"node": "explore", "kind": "evidence_gap", "message": indexGapMessage603}},
	}, "explore", "cr.p1")
	response := env.businessClient().request(http.MethodGet, "/api/v1/reviews/wf-603-node", nil, nil)
	assertStatus(t, response, http.StatusOK)
	want := []string{indexMismatchText + "（cr.p1）"}
	if got := stringList(t, env.decode(t, response)["warnings_text"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("warnings_text:\n got %q\nwant %q", got, want)
	}
}

// Model-supplied evidence gaps reach the API as signals verbatim. Only the shape
// the executor writes is a tool gap: text that mentions the phrase keeps its
// category and its own wording, in the signals, the summary and the warnings.
func TestReviewDetailDoesNotTreatModelGapsAsToolGaps(t *testing.T) {
	const (
		prose   = "the build log says MCP tools unavailable: retry later"
		indexed = "evidence is blocked by workspace index identity (index abcdef01, target 33333333)"
	)
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake})
	seedSynthesisWorkflow(env, "wf-603-spoof", "INCOMPLETE", map[string]any{
		"summary": "评审未完成：上游评审存在未解决的证据不足或阻断，不能给出 PASS。具体缺口：rv.p1: " + prose + "；rv.p2: " + indexed,
		"warnings": []string{
			"reasoner: " + prose,
			"validate: " + indexed,
		},
		"unresolved": []map[string]string{
			{"node": "rv.p1", "kind": "evidence_gap", "message": prose},
			{"node": "rv.p2", "kind": "evidence_gap", "message": indexed},
			{"node": "explore", "kind": "evidence_gap", "message": indexGapMessage603},
		},
	}, "explore", "rv.p1", "rv.p2", "reasoner", "validate")
	response := env.businessClient().request(http.MethodGet, "/api/v1/reviews/wf-603-spoof", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)

	items := unresolvedItems(t, body)
	if len(items) != 3 {
		t.Fatalf("unresolved = %v", items)
	}
	for i, want := range []struct{ category, text string }{
		{"evidence_gap", prose},
		{"evidence_gap", indexed},
		{"tool_unavailable", indexMismatchText},
	} {
		if items[i]["category"] != want.category || items[i]["text"] != want.text {
			t.Fatalf("unresolved[%d] category/text = %v / %v, want %s / %s", i, items[i]["category"], items[i]["text"], want.category, want.text)
		}
	}
	// The same sentences as warnings keep their own wording and their node.
	wantWarnings := []string{prose + "（reasoner）", indexed + "（validate）"}
	if got := stringList(t, body["warnings_text"]); !reflect.DeepEqual(got, wantWarnings) {
		t.Fatalf("warnings_text:\n got %q\nwant %q", got, wantWarnings)
	}
	text, _ := body["summary_text"].(string)
	if text == "" || strings.Contains(text, genericToolText) || strings.Contains(text, "不一致") {
		t.Fatalf("summary_text words model text as a tool gap: %q", text)
	}
}
