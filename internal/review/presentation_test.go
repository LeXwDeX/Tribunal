package review

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

type realFixtureFinding struct {
	OriginNode     string `json:"origin_node"`
	OriginProvider string `json:"origin_provider"`
	Severity       string `json:"severity"`
	Category       string `json:"category"`
	Path           string `json:"path"`
	Line           int    `json:"line"`
	Side           string `json:"side"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	Evidence       string `json:"evidence"`
	Confidence     string `json:"confidence"`
}

func loadRealFixture(t *testing.T) []Finding {
	t.Helper()
	raw, err := os.ReadFile("testdata/dedupe_real_split_evenly.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []realFixtureFinding
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	var out []Finding
	for i, r := range rows {
		out = append(out, gatheredFinding("wf", r.OriginNode, r.OriginProvider, "model", 0, i, FindingOut{
			Path: r.Path, Line: r.Line, Side: r.Side, Severity: Severity(r.Severity), Category: r.Category,
			Title: r.Title, Body: r.Body, Evidence: r.Evidence, Confidence: Confidence(r.Confidence),
		}))
	}
	return out
}

func decide(groups []Group, action Action) []Group {
	out := make([]Group, len(groups))
	copy(out, groups)
	for i := range out {
		out[i].Decision = &FindingDecision{Action: action, Source: "rules"}
		out[i].PositionOK = true
	}
	return out
}

func mkGroup(id string, line int, cat string, sev Severity, title, body string) Group {
	return Group{
		ID: id, Path: "pkg/x.go", Line: line, Side: SideNew, Category: cat, Severity: sev,
		Title: title, Body: body, Confidence: ConfidenceHigh,
		Decision: &FindingDecision{Action: ActionPublish, Source: "rules"}, PositionOK: true,
	}
}

func publishedIDs(groups []Group) []string {
	var ids []string
	for _, g := range groups {
		if g.Published() {
			ids = append(ids, g.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func TestPresentationMergesRealSplitEvenlyTriple(t *testing.T) {
	groups := Dedupe(loadRealFixture(t))
	if len(groups) != 4 {
		t.Fatalf("pass 1 dedupe must keep main behaviour (4 groups across categories), got %d", len(groups))
	}
	merged := MergePresentation(decide(groups, ActionPublish), nil)
	published := publishedIDs(merged)
	if len(published) != 2 {
		t.Fatalf("published after presentation = %v, want the line-15 triple folded and line 2 kept", published)
	}
	for _, g := range merged {
		if g.Published() && g.Line == 15 {
			if len(g.Related) != 2 || g.Severity != SeverityP2 {
				t.Fatalf("display related=%d severity=%s", len(g.Related), g.Severity)
			}
		}
		if g.Line == 2 && (!g.Published() || len(g.Related) != 0) {
			t.Fatalf("line-2 finding must stay separate: %+v", g)
		}
	}
}

func TestPresentationLeavesDistinctIssuesApart(t *testing.T) {
	const amountBody = "parse_amount 读取 raw amount 后写入 ledger，amount 的处理见调用方"
	cases := []struct {
		name string
		a, b Group
	}{
		{"split_evenly zero people vs negative total",
			mkGroup("a", 15, "error-handling", SeverityP2, "people=0 抛 ZeroDivisionError", "split_evenly 在 people 为 0 时抛出 ZeroDivisionError，调用方拿到无关异常"),
			mkGroup("b", 17, "correctness", SeverityP1, "负数 total_cents 余数分配错误", "split_evenly 在 total_cents 为负数时余数分配错误，people 份额之和不等于总额")},
		{"rows.Err vs defer Close",
			mkGroup("a", 40, "error-handling", SeverityP2, "`rows.Err()` 未检查", "遍历 rows 之后没有检查 rows.Err()，迭代错误被忽略，调用方 return nil"),
			mkGroup("b", 44, "resource-leak", SeverityP1, "rows 未 `defer Close` 导致连接泄漏", "查询返回的 rows 没有 defer Close，错误分支 return 时连接泄漏")},
		{"Handle err vs concurrent map",
			mkGroup("a", 30, "error-handling", SeverityP2, "Handle 忽略 err", "Handle 调用后忽略 err 直接 return nil，ctx 取消时 resp 为空"),
			mkGroup("b", 34, "concurrency", SeverityP1, "Handle 并发写 map", "Handle 在多个 goroutine 中并发写 map，ctx 取消时 err 返回 nil，resp 数据竞争")},
		{"parse_amount negative vs precision",
			mkGroup("a", 10, "correctness", SeverityP1, "parse_amount 未处理负数", amountBody),
			mkGroup("b", 10, "precision", SeverityP2, "parse_amount 精度丢失", amountBody)},
		{"parse_amount same category five lines apart",
			mkGroup("a", 10, "correctness", SeverityP1, "parse_amount 未处理负数", amountBody),
			mkGroup("b", 15, "correctness", SeverityP2, "parse_amount 精度丢失", amountBody)},
		{"ParseHeader overflow vs nil",
			mkGroup("a", 20, "correctness", SeverityP1, "ParseHeader 整数溢出", "ParseHeader 解析 header 长度时 raw 值 overflow，buf 越界"),
			mkGroup("b", 20, "nil-deref", SeverityP2, "ParseHeader 空指针", "ParseHeader 解析 header 时 raw 值 buf 为 nil 导致崩溃")},
		{"fetchUser XSS vs race",
			mkGroup("a", 8, "security", SeverityP1, "fetchUser 存在 XSS", "fetchUser 把 user 数据写入 DOM，user name 未转义"),
			mkGroup("b", 8, "concurrency", SeverityP2, "fetchUser 竞态", "fetchUser 在 user 数据写入 DOM 前存在竞态，user name 被覆盖")},
		{"english negative vs float",
			mkGroup("a", 10, "correctness", SeverityP1, "parse_amount: negative amount", "parse_amount accepts a negative amount and writes it to the ledger"),
			mkGroup("b", 10, "precision", SeverityP2, "parse_amount: float amount", "parse_amount accepts a float amount and writes it to the ledger")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, in := range [][]Group{{tc.a, tc.b}, {tc.b, tc.a}} {
				out := MergePresentation(in, nil)
				if got := publishedIDs(out); len(got) != 2 {
					t.Fatalf("published = %v, want both kept", got)
				}
				for _, g := range out {
					if len(g.Related) != 0 {
						t.Fatalf("unexpected related on %s", g.ID)
					}
				}
			}
		})
	}
}

func TestPresentationKeepsBothBodiesVisible(t *testing.T) {
	a := mkGroup("a", 20, "error-handling", SeverityP1, "fetch_user 未处理异常", "fetch_user 发送请求后没有处理异常 requests.Timeout，导致调用方崩溃")
	b := mkGroup("b", 20, "logic", SeverityP2, "fetch_user 未处理异常", "fetch_user 发送请求后没有处理异常 JSONDecodeError，导致调用方崩溃并且正文更长一些")
	out := MergePresentation([]Group{b, a}, nil)
	var display Group
	for _, g := range out {
		if g.Published() {
			display = g
		}
	}
	if display.ID != "a" || !strings.Contains(display.Body, "requests.Timeout") {
		t.Fatalf("display must be the highest-severity group: %+v", display)
	}
	body, err := RenderFinding(display, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "相关描述") || !strings.Contains(body, "JSONDecodeError") {
		t.Fatalf("other group's body missing:\n%s", body)
	}
}

func TestPresentationDoesNotChangeGroupsThatAreNotPublished(t *testing.T) {
	a := mkGroup("a", 20, "error-handling", SeverityP2, "fetch_user 未处理异常", "fetch_user 发送请求后没有处理异常，导致调用方崩溃")
	b := mkGroup("b", 20, "logic", SeverityP2, "fetch_user 未处理异常", "fetch_user 发送请求后没有处理异常，导致调用方崩溃")
	dropped := b
	dropped.Decision = &FindingDecision{Action: ActionDrop, Reason: "challenge refuted", Source: "rules"}
	out := MergePresentation([]Group{a, dropped}, nil)
	if out[1].Decision.Reason != "challenge refuted" || len(out[0].Related) != 0 {
		t.Fatalf("challenge-dropped group must not take part: %+v / %+v", out[0], out[1])
	}
	marked := b
	marked.Decision = &FindingDecision{Action: ActionMark, Reason: "uncertain", Source: "rules"}
	out = MergePresentation([]Group{a, marked}, nil)
	if len(out[0].Related) != 0 || !out[1].Published() {
		t.Fatalf("an uncertain group must not hide in a confident discussion")
	}
}

func TestPresentationLeavesDecisionsVotesAndInputUntouched(t *testing.T) {
	a := mkGroup("a", 20, "error-handling", SeverityP1, "fetch_user 未处理异常", "fetch_user 发送请求后没有处理异常，导致调用方崩溃")
	a.Votes, a.Consensus = 2, true
	b := mkGroup("b", 20, "logic", SeverityP2, "fetch_user 未处理异常", "fetch_user 发送请求后没有处理异常，导致调用方崩溃")
	b.Votes = 1
	b.Challenge = &Challenge{Status: ChallengeConfirmed}
	in := []Group{a, b}
	out := MergePresentation(in, nil)
	if in[1].Decision.Action != ActionPublish || len(in[0].Related) != 0 {
		t.Fatal("input slice must not be modified")
	}
	if out[0].Votes != 2 || !out[0].Consensus || out[1].Votes != 1 || out[1].Challenge == nil || out[0].Severity != SeverityP1 {
		t.Fatalf("votes, consensus, challenge and severity must be preserved: %+v %+v", out[0], out[1])
	}
	if out[1].Published() || out[1].Decision.Source != "presentation" {
		t.Fatalf("folded group must be marked as presentation-only: %+v", out[1].Decision)
	}
}

func TestPresentationIsDeterministicAndCapsRelated(t *testing.T) {
	var groups []Group
	for i := 0; i < 8; i++ {
		sev := SeverityP2
		if i == 7 {
			sev = SeverityP1
		}
		groups = append(groups, mkGroup(fmt.Sprintf("g%d", i), 20, "logic", sev,
			"fetch_user 未处理异常", fmt.Sprintf("fetch_user 发送请求后没有处理异常，导致调用方崩溃 变体%d", i)))
	}
	signature := func(in []Group) string {
		sig := ""
		for _, g := range MergePresentation(in, nil) {
			if g.Published() {
				sig += g.ID
				for _, r := range g.Related {
					sig += "|" + r.ID
				}
			}
		}
		return sig
	}
	want := signature(groups)
	for n := 1; n < len(groups); n++ {
		rotated := append(append([]Group(nil), groups[n:]...), groups[:n]...)
		if got := signature(rotated); got != want {
			t.Fatalf("rotation %d: %s != %s", n, got, want)
		}
	}
	reversed := make([]Group, len(groups))
	for i := range groups {
		reversed[len(groups)-1-i] = groups[i]
	}
	if got := signature(reversed); got != want {
		t.Fatalf("reversed: %s != %s", got, want)
	}
	out := MergePresentation(groups, nil)
	for _, g := range out {
		if g.Published() {
			if g.ID != "g7" || len(g.Related) != 7 {
				t.Fatalf("display %s related %d, want P1 g7 with 7 related", g.ID, len(g.Related))
			}
			body, err := RenderFinding(g, FindingRenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(body, "\n- [") != 5 || !strings.Contains(body, "另有 2 条") {
				t.Fatalf("cap/count wrong:\n%s", body)
			}
		}
	}
}

func TestSummaryItemShowsRelatedDescriptions(t *testing.T) {
	a := mkGroup("a", 20, "error-handling", SeverityP1, "fetch_user 未处理异常", "fetch_user 发送请求后没有处理异常 requests.Timeout，导致调用方崩溃")
	b := mkGroup("b", 20, "logic", SeverityP2, "fetch_user 未处理异常", "fetch_user 发送请求后没有处理异常 JSONDecodeError，导致调用方崩溃")
	for _, action := range []Action{ActionPublish, ActionMark} {
		a.Decision = &FindingDecision{Action: action, Reason: "r", Source: "rules"}
		b.Decision = &FindingDecision{Action: action, Reason: "r", Source: "rules"}
		out := MergePresentation([]Group{a, b}, nil)
		out[0].PositionOK, out[1].PositionOK = false, false
		summary, err := RenderSummary(SummaryInput{Verdict: VerdictBlocking, Summary: "s", Fingerprint: "abc", Groups: out})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(summary, "相关描述") || !strings.Contains(summary, "JSONDecodeError") {
			t.Fatalf("%s summary row lacks related descriptions:\n%s", action, summary)
		}
	}
}

func TestGatherKeepsSingleModelP1ChallengedNextToP2Consensus(t *testing.T) {
	const body = "split_evenly drops the remainder because floor division loses cents"
	mk := func(node, provider string, line int, sev Severity, title string) Proposal {
		return ProposalFromCodeReview(node, provider, provider+"-m", 1, CodeReviewOut{Summary: "s", Findings: []FindingOut{{
			Path: "pkg/calc.py", Line: line, Severity: sev, Category: "correctness",
			Title: title, Body: body, Evidence: "ev", Confidence: ConfidenceHigh,
		}}})
	}
	proposals := []Proposal{
		mk("cr.p1", "a", 20, SeverityP2, "split_evenly drops the remainder"),
		mk("cr.p2", "b", 21, SeverityP2, "split_evenly drops the remainder"),
		mk("cr.p3", "c", 25, SeverityP1, "split_evenly drops remainder cents"),
	}
	result, err := Gather(GatherInput{WorkflowID: "wf", Proposals: proposals}, GatherOptions{
		ChallengeOnly: []Severity{SeverityP0, SeverityP1}, MinVotes: 2, ChallengeProvider: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Groups) != 2 {
		t.Fatalf("groups = %d, want the P2 consensus pair and the P1 kept apart before adjudication", len(result.Groups))
	}
	for _, g := range result.Groups {
		if g.Severity == SeverityP1 && (g.Consensus || g.Votes != 1) {
			t.Fatalf("single-model P1 borrowed votes: %+v", g)
		}
	}
	if len(result.ToChallenge) != 1 {
		t.Fatalf("the P1 group must still be challenged, got %+v", result.ToChallenge)
	}
}

func TestGroupRelatedSurvivesJSONRoundTrip(t *testing.T) {
	a := mkGroup("a", 20, "error-handling", SeverityP1, "fetch_user 未处理异常", "fetch_user 发送请求后没有处理异常，导致调用方崩溃")
	b := mkGroup("b", 20, "logic", SeverityP2, "fetch_user 未处理异常", "fetch_user 发送请求后没有处理异常，导致调用方崩溃")
	out := MergePresentation([]Group{a, b}, nil)
	raw, err := json.Marshal(out[0])
	if err != nil {
		t.Fatal(err)
	}
	var back Group
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Related) != 1 || back.Related[0].ID != "b" {
		t.Fatalf("related lost in round trip: %+v", back.Related)
	}
}
