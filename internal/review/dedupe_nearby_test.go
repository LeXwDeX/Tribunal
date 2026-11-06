package review

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
)

func loadFixtureFindings(t *testing.T, path string) []Finding {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []realFixtureFinding
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	out := make([]Finding, 0, len(rows))
	for i, r := range rows {
		out = append(out, gatheredFinding("wf", r.OriginNode, r.OriginProvider, "model", 0, i, FindingOut{
			Path: r.Path, Line: r.Line, Side: r.Side, Severity: Severity(r.Severity), Category: r.Category,
			Title: r.Title, Body: r.Body, Evidence: r.Evidence, Confidence: Confidence(r.Confidence),
		}))
	}
	return out
}

// partition renders groups as sorted "category: member origin nodes@lines".
func partition(groups []Group) []string {
	var out []string
	for _, g := range groups {
		var members []string
		for _, s := range g.Sources {
			members = append(members, fmt.Sprintf("%s@%d", s.Origin.Node, s.Line))
		}
		sort.Strings(members)
		out = append(out, fmt.Sprintf("%s:%v", g.Category, members))
	}
	sort.Strings(out)
	return out
}

// Real review (MR !22): the distinct bug at avg.py:13 (reported by three lanes)
// was swallowed by the unrelated issue at :11 by the line-only rule.
func TestDedupeKeepsDistinctAdjacentIssuesSeparate(t *testing.T) {
	findings := loadFixtureFindings(t, "testdata/dedupe_real_adjacent_median.json")
	want := []string{
		"correctness:[cr.p1@11 reasoner@11 rv.p1@11]",
		"correctness:[cr.p1@13 reasoner@13 rv.p1@13]",
		"correctness:[reasoner@18 rv.p1@18]",
		"logic:[cr.p1@18]",
	}
	check := func(name string, in []Finding) {
		t.Helper()
		got := partition(Dedupe(in))
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s: groups = %v, want %v", name, got, want)
		}
	}
	check("fixture order", findings)
	// Determinism: every rotation and the reverse order yield one partition.
	for shift := 1; shift < len(findings); shift++ {
		rotated := append(append([]Finding{}, findings[shift:]...), findings[:shift]...)
		check(fmt.Sprintf("rotation %d", shift), rotated)
	}
	reversed := make([]Finding, len(findings))
	for i, f := range findings {
		reversed[len(findings)-1-i] = f
	}
	check("reversed", reversed)
}

func TestDedupeMergesSameIssueWithLineDriftAndKeepsVotes(t *testing.T) {
	const title = "Cache eviction race loses entries"
	body := func(f FindingOut) FindingOut {
		f.Body = "Concurrent eviction in evictExpired drops entries still referenced by lookup."
		return f
	}
	findings := []Finding{
		gatheredFinding("wf", "cr.p1", "a", "m1", 0, 0, body(outFinding("c.go", 10, SeverityP2, title, "evidence one"))),
		gatheredFinding("wf", "rv.p1", "b", "m2", 0, 1, body(outFinding("c.go", 12, SeverityP2, title, "evidence two"))),
		gatheredFinding("wf", "reasoner", "c", "m3", 0, 2, body(outFinding("c.go", 13, SeverityP2, title, "evidence three"))),
	}
	groups := Dedupe(findings)
	if len(groups) != 1 || len(groups[0].Sources) != 3 {
		t.Fatalf("groups = %v, want one group of three sources", partition(groups))
	}
	if votes := CountVotes(groups[0].Sources, 0.5); votes != 3 {
		t.Fatalf("votes = %v, want 3 independent votes", votes)
	}
}

func TestDedupeSplitsDistinctSameCategoryIssuesOnAdjacentLines(t *testing.T) {
	a := outFinding("d.go", 20, SeverityP1, "Nil pointer dereference in parseHeader", "x := h.Name")
	a.Body = "parseHeader dereferences h before the nil check, so an empty header panics."
	b := outFinding("d.go", 21, SeverityP1, "Unbounded retry loop never backs off", "for { retry() }")
	b.Body = "The loop retries immediately forever, which hammers the upstream service."
	groups := Dedupe([]Finding{
		gatheredFinding("wf", "cr.p1", "a", "m", 0, 0, a),
		gatheredFinding("wf", "rv.p1", "b", "m", 0, 1, b),
	})
	if len(groups) != 2 {
		t.Fatalf("groups = %v, want two distinct issues", partition(groups))
	}
}
