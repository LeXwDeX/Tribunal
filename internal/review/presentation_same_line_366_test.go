package review

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// loadSameLineFixture reads the stored findings of two acceptance reviews
// (a probe MR and an inventory MR) whose cross-lane duplicates on one exact
// line were published as separate discussions.
func loadSameLineFixture(t *testing.T) []Group {
	t.Helper()
	raw, err := os.ReadFile("testdata/presentation_same_line_366.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []summaryDupRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	var out []Group
	for _, r := range rows {
		origin := Origin{Node: r.Node, Attempt: 1}
		out = append(out, Group{
			ID: r.ID, Origin: origin, Severity: Severity(r.Severity), Category: r.Category, Path: r.Path, Line: r.Line,
			Side: Side(r.Side), Title: r.Title, Body: r.Body, Evidence: r.Evidence, Confidence: ConfidenceHigh,
			Sources:  []Finding{{ID: r.ID, Origin: origin}},
			Decision: &FindingDecision{Action: Action(r.Decision), Source: "rules"}, PositionOK: true,
		})
	}
	return out
}

func foldedPairs(groups []Group) map[string]int {
	out := map[string]int{}
	for _, g := range groups {
		if g.Published() {
			out[fmt.Sprintf("%s:%d", g.Path, g.Line)]++
		}
	}
	return out
}

func TestPresentationFoldsSameLineCrossLaneDuplicates(t *testing.T) {
	merged := MergePresentation(loadSameLineFixture(t), nil)
	listed := foldedPairs(merged)
	// Each of these lines carried two or three lane-specific copies of one problem.
	for _, line := range []string{
		"stats/avg.py:14", "stats/avg.py:19",
		"inventory/inventory.py:24", "inventory/inventory.py:76", "inventory/inventory.py:81",
	} {
		if listed[line] != 1 {
			t.Errorf("%s published %d discussions, want 1", line, listed[line])
		}
	}
	for _, g := range merged {
		switch g.Path + fmt.Sprintf(":%d", g.Line) {
		case "stats/avg.py:14":
			if g.Published() && (g.EffectiveSeverity() != SeverityP1 || len(g.Related) != 1) {
				t.Errorf("avg.py:14 display must be the P1 member with one related item, got %s related=%d", g.EffectiveSeverity(), len(g.Related))
			}
		case "inventory/inventory.py:81":
			if g.Published() && len(g.Related) != 2 {
				t.Errorf("inventory.py:81 related=%d, want 2", len(g.Related))
			}
		}
	}
	// restock.py:15 is the same problem worded differently (text similarity
	// 0.16, no shared title content), below every existing threshold, so it
	// is deliberately not folded; a shared function name alone never proves it.
	// Different problems on neighbouring lines stay separate.
	for _, line := range []string{"stats/avg.py:13", "inventory/inventory.py:75", "inventory/inventory.py:89"} {
		if listed[line] < 1 {
			t.Errorf("%s lost its discussion", line)
		}
	}
	if listed["inventory/inventory.py:89"] != 2 {
		t.Errorf("inventory.py:89 holds two distinct lane findings, got %d", listed["inventory/inventory.py:89"])
	}
}

func laneGroup(id, node string, line int, cat string, sev Severity, title, body string) Group {
	g := mkGroup(id, line, cat, sev, title, body)
	g.Origin = Origin{Node: node, Attempt: 1}
	g.Sources = []Finding{{ID: id, Origin: g.Origin}}
	return g
}

func TestPresentationSameLineKeepsDistinctIssuesApart(t *testing.T) {
	cases := []struct {
		name string
		a, b Group
	}{
		{
			"unrelated titles and bodies",
			laneGroup("a", "cr.p1", 10, "logic", SeverityP1, "缓存淘汰顺序错误", "淘汰时先删除最新条目而不是最旧条目，命中率下降"),
			laneGroup("b", "rv.p1", 10, "security", SeverityP1, "令牌写入日志", "日志输出包含完整令牌字符串，运维人员可以读取"),
		},
		{
			"severity gap of two levels",
			laneGroup("a", "cr.p1", 10, "logic", SeverityP0, "reserve 用 > 判断导致库存等量无法出库", "当库存恰好等于预留量时 reserve 返回 False，订单被拒绝，应改为 >="),
			laneGroup("b", "reasoner", 10, "api_contract", SeverityP2, "reserve 用 > 判断导致库存等量无法出库", "当库存恰好等于预留量时 reserve 返回 False，订单被拒绝，应改为 >="),
		},
		{
			"same lane never folds on the relaxed rule",
			laneGroup("a", "cr.p1", 10, "logic", SeverityP1, "余额校验方向写反", "条件写成小于号。"),
			laneGroup("b", "cr.p1", 10, "correctness", SeverityP1, "余额校验方向写反导致拒绝", "另一段完全不同措辞的说明，没有共同词汇。"),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			merged := MergePresentation([]Group{c.a, c.b}, nil)
			if got := publishedIDs(merged); len(got) != 2 {
				t.Fatalf("published %v, want both kept separate", got)
			}
		})
	}
}

func TestPresentationSameLineCrossLaneFoldsOnTitleAndBodyOverlap(t *testing.T) {
	a := laneGroup("a", "cr.p1", 10, "logic", SeverityP1, "余额校验方向写反", "amount_cents 小于 balance_cents 时抛出 insufficient balance，条件写成了小于号。")
	b := laneGroup("b", "rv.p1", 10, "correctness", SeverityP2, "余额校验方向写反导致拒绝", "amount_cents 与 balance_cents 比较方向写反，insufficient balance 被误抛出。")
	merged := MergePresentation([]Group{b, a}, nil)
	got := publishedIDs(merged)
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("published %v, want only the P1 display group a", got)
	}
	for _, g := range merged {
		if g.ID == "a" && (len(g.Related) != 1 || g.Related[0].ID != "b" || g.Related[0].Category != "correctness") {
			t.Fatalf("related = %+v", g.Related)
		}
	}
	// Different lines keep the old rule: no fold without identifiers.
	b.Line = 11
	if got := publishedIDs(MergePresentation([]Group{a, b}, nil)); len(got) != 2 {
		t.Fatalf("non-exact line must keep the old rule, published %v", got)
	}
}

// Titles that share only generic words (函数/未处理) or one identifier, and
// bodies that share only boilerplate advice, are different problems.
func TestPresentationSameLineGenericWordingDoesNotFold(t *testing.T) {
	boiler := "建议补充检查并增加测试。"
	cases := []struct {
		name string
		a, b Group
	}{
		{"average empty vs negative",
			laneGroup("a", "cr.p1", 5, "logic", SeverityP1, "average 函数未处理空列表", "空列表会触发除零。"+boiler),
			laneGroup("b", "rv.p1", 5, "correctness", SeverityP1, "average 函数未处理负数输入", "负数会使均值失真。"+boiler)},
		{"reserve limit vs lock",
			laneGroup("a", "cr.p1", 5, "logic", SeverityP1, "reserve 方法缺少库存上限检查", "没有上限会超卖。"+boiler),
			laneGroup("b", "rv.p1", 5, "concurrency", SeverityP1, "reserve 方法缺少线程锁保护", "并发读改写丢失更新。"+boiler)},
		{"sql injection vs empty value",
			laneGroup("a", "cr.p1", 5, "security", SeverityP0, "函数未处理 SQL 注入", "拼接 SQL 可被注入。"+boiler),
			laneGroup("b", "rv.p1", 5, "correctness", SeverityP1, "函数未处理空值返回", "返回空值时调用方崩溃。"+boiler)},
		{"negative vs precision with boilerplate bodies",
			laneGroup("a", "cr.p1", 5, "logic", SeverityP1, "未处理负数", boiler+"另见调用方的处理说明。"),
			laneGroup("b", "rv.p1", 5, "correctness", SeverityP1, "精度丢失", boiler+"另见调用方的处理说明。")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := publishedIDs(MergePresentation([]Group{c.a, c.b}, nil)); len(got) != 2 {
				t.Fatalf("published %v, want both kept separate", got)
			}
		})
	}
}

func TestPresentationSameLineFoldIsOrderIndependent(t *testing.T) {
	groups := loadSameLineFixture(t)
	want := fmt.Sprint(foldedPairs(MergePresentation(groups, nil)))
	reversed := make([]Group, len(groups))
	for i := range groups {
		reversed[len(groups)-1-i] = groups[i]
	}
	for i := 0; i < 5; i++ {
		if got := fmt.Sprint(foldedPairs(MergePresentation(reversed, nil))); got != want {
			t.Fatalf("order changed the result:\n%s\n%s", got, want)
		}
	}
}
