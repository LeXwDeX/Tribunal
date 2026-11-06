package review

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type summaryDupRow struct {
	ID         string `json:"id"`
	Node       string `json:"origin_node"`
	Severity   string `json:"severity"`
	Category   string `json:"category"`
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Side       string `json:"side"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Evidence   string `json:"evidence"`
	Decision   string `json:"decision"`
	PositionOK bool   `json:"position_ok"`
}

// loadSummaryDuplicates reads the stored findings of an acceptance review whose
// summary listed 18 reasoner findings without a path, almost all of them
// duplicates of inline discussions.
func loadSummaryDuplicates(t *testing.T) []Group {
	t.Helper()
	raw, err := os.ReadFile("testdata/summary_duplicates_361.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []summaryDupRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	var out []Group
	for _, r := range rows {
		out = append(out, Group{
			ID: r.ID, Severity: Severity(r.Severity), Category: r.Category, Path: r.Path, Line: r.Line,
			Side: Side(r.Side), Title: r.Title, Body: r.Body, Evidence: r.Evidence, Confidence: ConfidenceHigh,
			Decision: &FindingDecision{Action: Action(r.Decision), Source: "rules"}, PositionOK: r.PositionOK,
		})
	}
	return out
}

func summaryFor(t *testing.T, groups []Group) string {
	t.Helper()
	body, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: "found problems", Groups: groups,
		Fingerprint: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestSummaryFoldsReasonerDuplicatesOfInlineFindings(t *testing.T) {
	groups := loadSummaryDuplicates(t)
	body := summaryFor(t, groups)
	for _, dup := range []string{
		"charge 的余额校验方向写反，且可能返回负余额",
		"load_config 用 eval 执行配置文件内容",
		"find_user 以 % 格式化拼接 SQL，存在注入且游标未关闭",
		"会话缓存用 pickle 反序列化，可导致任意代码执行",
		"read_token 把支付令牌打印到标准输出",
		"save_state 打开的文件从不关闭",
		"TokenBucket.allow 在 tokens 恰好等于 cost 时拒绝请求",
		"purge 对目录等非普通文件调用 os.remove，会中断整轮清理",
	} {
		if strings.Contains(body, "#### ") && strings.Contains(body, "] "+dup) {
			t.Errorf("duplicate %q is still listed separately", dup)
		}
	}
	// Folded duplicates are referenced under the inline finding, not dropped.
	if !strings.Contains(body, "相关描述") || !strings.Contains(body, "load_config 用 eval 执行配置文件内容") {
		t.Errorf("folded duplicate must be referenced under 相关描述:\n%s", body)
	}
	// Header counts are of listed findings: the 18 duplicates are not counted twice.
	if !strings.Contains(body, "无法定位到行的问题（5）") {
		t.Errorf("unpositioned section must list the 2 distinct findings and the 3 duplicates of prose-only inline titles, which are kept conservatively:\n%s", body)
	}
}

func TestSummaryKeepsDissimilarUnpositionedFindings(t *testing.T) {
	body := summaryFor(t, loadSummaryDuplicates(t))
	for _, keep := range []string{
		"session_id 未校验，os.path.join 可逃出 CACHE_DIR",
		"没有任何代码创建 CACHE_DIR，会话模块首次使用即失败",
	} {
		if !strings.Contains(body, "] "+keep) {
			t.Errorf("dissimilar unpositioned finding %q must stay listed:\n%s", keep, body)
		}
	}
}

func TestSummaryFoldKeepsStoredGroupsAndSeverityGuard(t *testing.T) {
	groups := loadSummaryDuplicates(t)
	before, _ := json.Marshal(groups)
	summaryFor(t, groups)
	after, _ := json.Marshal(groups)
	if string(before) != string(after) {
		t.Fatal("rendering the summary must not mutate groups")
	}

	inline := Group{ID: "a", Severity: SeverityP2, Category: "correctness", Path: "api/x.py", Line: 3, Side: SideNew,
		Title: "load_config 用 eval 执行配置文件", Body: "load_config 把文件内容交给 eval 执行，存在任意代码执行。",
		Decision: &FindingDecision{Action: ActionPublish}, PositionOK: true}
	same := Group{ID: "b", Severity: SeverityP2, Category: "security", Title: "load_config 用 eval 执行配置文件内容",
		Body: "load_config 对配置文件使用 eval，可导致任意代码执行。", Decision: &FindingDecision{Action: ActionPublish}}
	far := same
	far.ID, far.Severity = "c", SeverityP0
	body := summaryFor(t, []Group{inline, same, far})
	if !strings.Contains(body, "[P0] load_config") {
		t.Errorf("a finding two severity levels away must stay listed:\n%s", body)
	}
	if strings.Contains(body, "#### [P2] load_config") {
		t.Errorf("same-severity duplicate must be folded:\n%s", body)
	}
}

func TestSummaryFoldIsDeterministic(t *testing.T) {
	groups := loadSummaryDuplicates(t)
	first := summaryFor(t, groups)
	for i := 0; i < 20; i++ {
		if again := summaryFor(t, groups); again != first {
			t.Fatalf("summary changed between renders:\n%s\n---\n%s", first, again)
		}
	}
}

func TestSummaryNeverFoldsMoreSevereOrDistinctFindings(t *testing.T) {
	publish := &FindingDecision{Action: ActionPublish}
	cases := []struct {
		name   string
		inline Group
		cand   Group
	}{
		{"more severe path traversal", Group{ID: "a", Severity: SeverityP2, Path: "api/x.py", Line: 3, Side: SideNew,
			Title: "读取文件前未限制路径", Body: "load_config 读取调用方给出的文件。", Decision: publish, PositionOK: true},
			Group{ID: "b", Severity: SeverityP1, Title: "load_config 允许路径穿越读取任意文件",
				Body: "load_config 读取调用方给出的文件，../ 可逃出目录。", Decision: publish}},
		{"more severe sql injection", Group{ID: "a", Severity: SeverityP2, Path: "api/x.py", Line: 3, Side: SideNew,
			Title: "用户输入未做校验", Body: "create_user 没有校验 email 格式。", Decision: publish, PositionOK: true},
			Group{ID: "b", Severity: SeverityP1, Title: "create_user 未校验用户输入导致 SQL 注入",
				Body: "create_user 把输入拼进 SQL。", Decision: publish}},
		{"same severity distinct issue", Group{ID: "a", Severity: SeverityP2, Path: "api/x.py", Line: 3, Side: SideNew,
			Title: "用户输入未做校验", Body: "create_user 没有校验 email 格式。", Decision: publish, PositionOK: true},
			Group{ID: "b", Severity: SeverityP2, Title: "create_user 未校验用户输入导致 SQL 注入",
				Body: "create_user 把输入拼进 SQL。", Decision: publish}},
	}
	for _, c := range cases {
		body := summaryFor(t, []Group{c.inline, c.cand})
		if !strings.Contains(body, "#### ["+string(c.cand.Severity)+"] "+c.cand.Title) {
			t.Errorf("%s: candidate must stay listed:\n%s", c.name, body)
		}
	}
	if got := len(SummaryFoldedGroups(loadSummaryDuplicates(t))); got != 25 {
		t.Errorf("SummaryFoldedGroups kept %d groups, want 25", got)
	}
}
