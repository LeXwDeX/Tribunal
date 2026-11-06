package review

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

func outFinding(path string, line int, severity Severity, title, evidence string) FindingOut {
	return FindingOut{
		Path:       path,
		Line:       line,
		Severity:   severity,
		Category:   "correctness",
		Title:      title,
		Body:       "body for " + title,
		Evidence:   evidence,
		Confidence: ConfidenceHigh,
	}
}

func gatheredFinding(wf, node, provider, model string, attempt, index int, f FindingOut) Finding {
	finding := Finding{
		WorkflowID: wf,
		ID:         StableFindingID(wf, node, attempt, index),
		Origin:     Origin{Node: node, Index: index, Attempt: attempt, Provider: provider, Model: model},
		Severity:   f.Severity,
		Category:   f.Category,
		Path:       f.Path,
		Line:       f.Line,
		Side:       SideNew,
		Title:      f.Title,
		Body:       f.Body,
		Suggestion: f.Suggestion,
		Evidence:   f.Evidence,
		Confidence: f.Confidence,
	}
	finding.PublishKey = PublishKeyFor(finding.Path, finding.Title, snippetFor(finding))
	return finding
}

func TestProposalFromReasonerNormalizesSeverityAndFields(t *testing.T) {
	out := ReasonerOut{
		ResultType: "FINDINGS",
		Findings: []ReasonerFinding{
			{Severity: "critical", Dimension: "API design", Finding: strings.Repeat("长", 140), Where: "MR description", Why: "missing boundary", Assumptions: []string{}, Confidence: ConfidenceHigh},
			{Severity: "notable", Dimension: "data model", Finding: "Notable", Where: "issue #1", Why: "why", Assumptions: []string{}, Confidence: ConfidenceMedium},
			{Severity: "informational", Dimension: "docs", Finding: "Info", Where: "design doc", Why: "why", Assumptions: []string{}, Confidence: ConfidenceLow},
		},
	}
	proposal := ProposalFromReasoner("reasoner", "provider", "model", 2, out)
	if len(proposal.Findings) != 3 {
		t.Fatalf("findings = %d", len(proposal.Findings))
	}
	if proposal.Findings[0].Severity != SeverityP1 || proposal.Findings[1].Severity != SeverityP2 || proposal.Findings[2].Severity != SeverityInfo {
		t.Fatalf("severity mapping = %v/%v/%v", proposal.Findings[0].Severity, proposal.Findings[1].Severity, proposal.Findings[2].Severity)
	}
	if len([]rune(proposal.Findings[0].Title)) != MaxTitleRunes {
		t.Fatalf("title runes = %d", len([]rune(proposal.Findings[0].Title)))
	}
	if proposal.Findings[0].Category != "api_design" {
		t.Fatalf("category = %q", proposal.Findings[0].Category)
	}
	if err := proposal.Findings[0].Validate(); err != nil {
		t.Fatalf("normalized finding: %v", err)
	}
}

func TestDedupeRules(t *testing.T) {
	wf := "wf"
	base := gatheredFinding(wf, "cr.p1", "a", "m", 0, 0, outFinding("a.go", 10, SeverityP2, "One issue", "line ten evidence"))
	cases := []struct {
		name       string
		findings   []Finding
		wantGroups int
		wantHigh   Severity
	}{
		{"within three lines", []Finding{base, gatheredFinding(wf, "cr.p2", "b", "m", 0, 1, outFinding("a.go", 13, SeverityP1, "Same", "evidence two"))}, 1, SeverityP1},
		{"beyond three lines", []Finding{base, gatheredFinding(wf, "cr.p2", "b", "m", 0, 1, outFinding("a.go", 14, SeverityP1, "Same", "evidence two"))}, 2, ""},
		{"different category", []Finding{base, func() Finding {
			f := gatheredFinding(wf, "cr.p2", "b", "m", 0, 1, outFinding("a.go", 11, SeverityP1, "Same", "evidence two"))
			f.Category = "security"
			return f
		}()}, 2, ""},
		{"different path", []Finding{base, gatheredFinding(wf, "cr.p2", "b", "m", 0, 1, outFinding("b.go", 11, SeverityP1, "Same", "evidence two"))}, 2, ""},
		{"line zero different title", []Finding{
			gatheredFinding(wf, "reasoner", "a", "m", 0, 0, outFinding("", 0, SeverityInfo, "Design A", "evidence A")),
			gatheredFinding(wf, "reasoner", "a", "m", 0, 1, outFinding("", 0, SeverityInfo, "Design B", "evidence B")),
		}, 2, ""},
		{"line zero same normalized title", []Finding{
			gatheredFinding(wf, "reasoner", "a", "m", 0, 0, outFinding("", 0, SeverityInfo, "Design  Boundary", "evidence A")),
			gatheredFinding(wf, "reasoner", "a", "m", 0, 1, outFinding("", 0, SeverityInfo, " design boundary ", "evidence B")),
		}, 1, ""},
		{"line zero with line finding", []Finding{
			gatheredFinding(wf, "reasoner", "a", "m", 0, 0, outFinding("", 0, SeverityInfo, "Same title", "evidence A")),
			gatheredFinding(wf, "cr.p1", "a", "m", 0, 1, outFinding("a.go", 5, SeverityInfo, "Same title", "evidence B")),
		}, 2, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groups := Dedupe(tc.findings)
			if len(groups) != tc.wantGroups {
				t.Fatalf("groups = %d, want %d", len(groups), tc.wantGroups)
			}
			if len(groups) == 1 && len(tc.findings) > 1 {
				if len(groups[0].Sources) != len(tc.findings) {
					t.Fatalf("sources = %d", len(groups[0].Sources))
				}
				if tc.wantHigh != "" && groups[0].Severity != tc.wantHigh {
					t.Fatalf("highest severity = %s", groups[0].Severity)
				}
			}
		})
	}
}

func TestDedupePreservesAllSourcesAndRepresentativeEvidence(t *testing.T) {
	wf := "wf"
	findings := []Finding{
		gatheredFinding(wf, "a", "provider", "model", 0, 0, outFinding("a.go", 10, SeverityP2, "First", "short")),
		gatheredFinding(wf, "b", "provider", "model", 0, 1, outFinding("a.go", 11, SeverityP0, "Second", "a much longer piece of evidence")),
	}
	groups := Dedupe(findings)
	if len(groups) != 1 {
		t.Fatalf("groups = %d", len(groups))
	}
	group := groups[0]
	if group.Severity != SeverityP0 {
		t.Fatalf("severity = %s", group.Severity)
	}
	if len(group.Sources) != 2 {
		t.Fatalf("sources = %d", len(group.Sources))
	}
	if group.Title != "Second" || group.Evidence != "a much longer piece of evidence" {
		t.Fatalf("representative = %q / %q", group.Title, group.Evidence)
	}
}

func TestVotesUseProviderModelPairAndSameModelWeight(t *testing.T) {
	finding := outFinding("a.go", 10, SeverityP1, "One issue", "evidence")
	sources := []Finding{
		gatheredFinding("wf", "rv.std.p1", "claude", "same", 0, 0, finding),
		gatheredFinding("wf", "rv.intent.p1", "claude", "same", 0, 1, finding),
	}
	if votes := CountVotes(sources, 0.5); votes != 1.5 {
		t.Fatalf("same provider/model votes = %v", votes)
	}
	if votes := CountVotes(sources, 0.25); votes != 1.25 {
		t.Fatalf("custom weight votes = %v", votes)
	}
	independent := append(sources, gatheredFinding("wf", "cr.p1", "gpt", "other", 0, 0, finding))
	if votes := CountVotes(independent, 0.5); votes != 2.5 {
		t.Fatalf("independent votes = %v", votes)
	}

	// A standards and intent channel from the same model must not create two
	// full votes merely because the node kinds differ.
	result, err := Gather(GatherInput{
		WorkflowID: "wf",
		Proposals: []Proposal{
			Proposal{Node: "rv.std.p1", Kind: ContractReviewStandards, Provider: "claude", Model: "same", Findings: []FindingOut{finding}},
			Proposal{Node: "rv.intent.p1", Kind: ContractReviewIntent, Provider: "claude", Model: "same", Findings: []FindingOut{finding}},
		},
	}, GatherOptions{MinVotes: 2, ChallengeProvider: "cross", AllowedProviders: []string{"claude", "gpt"}, SameModelWeight: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Groups) != 1 {
		t.Fatalf("groups = %d", len(result.Groups))
	}
	if result.Groups[0].Consensus {
		t.Fatal("same-model channels must not reach two-vote consensus")
	}
	if len(result.ToChallenge) != 1 || result.ToChallenge[0].Provider != "gpt" {
		t.Fatalf("challenge target = %+v", result.ToChallenge)
	}
}

func TestGatherSelectsCrossChallengerAndFallback(t *testing.T) {
	finding := outFinding("a.go", 10, SeverityP1, "One issue", "evidence")
	proposal := func(node, provider string) Proposal {
		return Proposal{Node: node, Kind: ContractCodeReview, Provider: provider, Model: "m", Findings: []FindingOut{finding}}
	}
	result, err := Gather(GatherInput{WorkflowID: "wf", Proposals: []Proposal{proposal("cr.a", "a")}}, GatherOptions{
		ChallengeProvider: "cross",
		AllowedProviders:  []string{"a", "b", "c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToChallenge) != 1 || result.ToChallenge[0].Provider != "b" || result.ToChallenge[0].SameModel {
		t.Fatalf("cross target = %+v", result.ToChallenge)
	}

	result, err = Gather(GatherInput{WorkflowID: "wf", Proposals: []Proposal{proposal("cr.a", "a"), proposal("cr.b", "b")}}, GatherOptions{
		ChallengeProvider: "cross",
		AllowedProviders:  []string{"a", "b", "c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToChallenge) != 1 || result.ToChallenge[0].Provider != "c" || result.ToChallenge[0].SameModel {
		t.Fatalf("cross target = %+v", result.ToChallenge)
	}

	result, err = Gather(GatherInput{WorkflowID: "wf", Proposals: []Proposal{proposal("cr.a", "a"), proposal("cr.b", "b"), proposal("cr.c", "c")}}, GatherOptions{
		ChallengeProvider: "cross",
		AllowedProviders:  []string{"a", "b", "c"},
		FallbackProvider:  "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToChallenge) != 1 || result.ToChallenge[0].Provider != "a" || !result.ToChallenge[0].SameModel {
		t.Fatalf("fallback target = %+v", result.ToChallenge)
	}
	if len(result.Warnings) == 0 || !strings.Contains(result.Warnings[0], "fallback") {
		t.Fatalf("fallback warning = %v", result.Warnings)
	}

	_, err = Gather(GatherInput{WorkflowID: "wf", Proposals: []Proposal{proposal("cr.a", "a")}}, GatherOptions{
		ChallengeProvider: "cross",
		AllowedProviders:  []string{"a"},
	})
	if err == nil {
		t.Fatal("cross without an unused provider or fallback must fail")
	}

	_, err = Gather(GatherInput{WorkflowID: "wf", Proposals: []Proposal{proposal("cr.a", "a")}}, GatherOptions{
		ChallengeProvider: "c",
		AllowedProviders:  []string{"a", "b"},
	})
	if err == nil {
		t.Fatal("explicit provider outside allowlist must fail")
	}
}

func TestGatherBudgetAndGateDrop(t *testing.T) {
	finding := outFinding("a.go", 10, SeverityP1, "One issue", "evidence")
	result, err := Gather(GatherInput{WorkflowID: "wf", Proposals: []Proposal{{
		Node: "cr.p1", Provider: "a", Model: "m", Findings: []FindingOut{finding},
	}}}, GatherOptions{
		ChallengeProvider: "cross",
		AllowedProviders:  []string{"a", "b"},
		BudgetExhausted:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToChallenge) != 0 {
		t.Fatalf("budget-exhausted targets = %+v", result.ToChallenge)
	}
	if result.Groups[0].Challenge == nil || result.Groups[0].Challenge.Status != ChallengeSkipped {
		t.Fatalf("challenge = %+v", result.Groups[0].Challenge)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "budget") {
		t.Fatalf("warnings = %v", result.Warnings)
	}

	result, err = Gather(GatherInput{
		WorkflowID: "wf",
		Proposals: []Proposal{{
			Node: "rv.std.p1", Provider: "a", Model: "m", Findings: []FindingOut{finding},
		}},
		Dropped: []DroppedFinding{{Ref: Ref{Node: "rv.std.p1", Index: 0}, Reason: "rebutted"}},
	}, GatherOptions{ChallengeProvider: "cross", AllowedProviders: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Groups) != 0 {
		t.Fatalf("dropped finding became a group: %+v", result.Groups)
	}
	if len(result.Dropped) != 1 || result.Dropped[0].Decision == nil || result.Dropped[0].Decision.Action != ActionDrop {
		t.Fatalf("dropped = %+v", result.Dropped)
	}
	if result.Dropped[0].Decision.Reason != "rebutted" {
		t.Fatalf("reason = %q", result.Dropped[0].Decision.Reason)
	}
}

func TestStableIDsAndPublishKeys(t *testing.T) {
	first := StableFindingID("wf", "cr.p1", 0, 0)
	if first != StableFindingID("wf", "cr.p1", 0, 0) {
		t.Fatal("stable finding id changed")
	}
	for _, other := range []string{
		StableFindingID("wf2", "cr.p1", 0, 0),
		StableFindingID("wf", "cr.p2", 0, 0),
		StableFindingID("wf", "cr.p1", 1, 0),
		StableFindingID("wf", "cr.p1", 0, 1),
	} {
		if other == first {
			t.Fatalf("id collision with %s", other)
		}
	}
	if StableGroupID("wf", "cr.p1", 0, 0) == StableFindingID("wf", "cr.p1", 0, 0) {
		t.Fatal("finding and group namespaces collide")
	}

	key := PublishKeyFor("a.go", "Concurrent map write", "results[name] = r")
	if key != PublishKeyFor("a.go", " concurrent   map write ", "results[name] = r") {
		t.Fatal("publish key is not normalized")
	}
	if key != PublishKeyFor("a.go", "Concurrent map write", "results[name] = r") {
		t.Fatal("publish key changed")
	}
	if key == PublishKeyFor("b.go", "Concurrent map write", "results[name] = r") {
		t.Fatal("path must affect publish key")
	}
	if key == PublishKeyFor("a.go", "Other title", "results[name] = r") {
		t.Fatal("title must affect publish key")
	}
	if key == PublishKeyFor("a.go", "Concurrent map write", "other snippet") {
		t.Fatal("snippet must affect publish key")
	}
}

func TestGatherRejectsInvalidInput(t *testing.T) {
	finding := outFinding("a.go", 10, SeverityP1, "One issue", "evidence")
	proposal := Proposal{Node: "cr.p1", Provider: "a", Model: "m", Findings: []FindingOut{finding}}
	cases := []struct {
		name string
		in   GatherInput
		opts GatherOptions
	}{
		{"empty workflow", GatherInput{Proposals: []Proposal{proposal}}, GatherOptions{ChallengeProvider: "cross", AllowedProviders: []string{"a", "b"}}},
		{"bad weight", GatherInput{WorkflowID: "wf", Proposals: []Proposal{proposal}}, GatherOptions{SameModelWeight: -1, ChallengeProvider: "cross", AllowedProviders: []string{"a", "b"}}},
		{"unknown drop", GatherInput{WorkflowID: "wf", Proposals: []Proposal{proposal}, Dropped: []DroppedFinding{{Ref: Ref{Node: "missing", Index: 0}}}}, GatherOptions{ChallengeProvider: "cross", AllowedProviders: []string{"a", "b"}}},
		{"duplicate drop", GatherInput{WorkflowID: "wf", Proposals: []Proposal{proposal}, Dropped: []DroppedFinding{
			{Ref: Ref{Node: "cr.p1", Index: 0}},
			{Ref: Ref{Node: "cr.p1", Index: 0}},
		}}, GatherOptions{ChallengeProvider: "cross", AllowedProviders: []string{"a", "b"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Gather(tc.in, tc.opts); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func nearDup(node, provider, model string, index, line int, category string, severity Severity, title, body string) Finding {
	f := outFinding("pkg/calc.py", line, severity, title, "")
	f.Category = category
	f.Body = body
	return gatheredFinding("wf", node, provider, model, 0, index, f)
}

const splitEvenlyBody = "split_evenly divides total by parts with integer floor division so the remainder cents are silently dropped and the shares no longer sum to the total"

func TestDedupeMergesParaphrasedNearDuplicates(t *testing.T) {
	cases := []struct {
		name     string
		findings []Finding
	}{
		{"same lane different category and nearby line", []Finding{
			nearDup("cr.p1", "a", "m1", 0, 15, "correctness", SeverityP2, "split_evenly drops the remainder", splitEvenlyBody),
			nearDup("cr.p1", "a", "m1", 1, 15, "logic", SeverityP2, "split_evenly drops remainder cents", "split_evenly divides total by parts using floor division so the remainder is dropped and shares do not sum to the total"),
		}},
		{"cross lane same category six lines apart", []Finding{
			nearDup("cr.p1", "a", "m1", 0, 15, "correctness", SeverityInfo, "split_evenly drops the remainder", splitEvenlyBody),
			nearDup("cr.p2", "b", "m2", 0, 21, "correctness", SeverityP2, "split_evenly drops remainder", "The split_evenly function divides total by parts with floor division, so the remainder cents are dropped and the shares do not sum to the total"),
		}},
		{"three-way mixed", []Finding{
			nearDup("cr.p1", "a", "m1", 0, 15, "correctness", SeverityP2, "split_evenly drops the remainder", splitEvenlyBody),
			nearDup("cr.p2", "b", "m2", 0, 15, "data_loss", SeverityP2, "split_evenly drops remainder cents", "split_evenly uses floor division of total by parts, so remainder cents are silently dropped and the shares no longer sum to total"),
			nearDup("cr.p3", "c", "m3", 0, 15, "correctness", SeverityP1, "split_evenly drops the remainder from shares", "Because split_evenly floor divides total by parts, the remainder cents are dropped; shares no longer sum to the total"),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groups := Dedupe(tc.findings)
			if len(groups) != 1 {
				t.Fatalf("groups = %d, want 1 for near-duplicates", len(groups))
			}
			if len(groups[0].Sources) != len(tc.findings) {
				t.Fatalf("sources = %d, want %d", len(groups[0].Sources), len(tc.findings))
			}
			high := tc.findings[0].Severity
			for _, f := range tc.findings {
				if f.Severity.Higher(high) {
					high = f.Severity
				}
			}
			if groups[0].Severity != high {
				t.Fatalf("severity = %s, want %s", groups[0].Severity, high)
			}
		})
	}
}

func TestDedupeKeepsDistinctIssuesApart(t *testing.T) {
	base := nearDup("cr.p1", "a", "m1", 0, 15, "correctness", SeverityP2, "split_evenly drops the remainder", splitEvenlyBody)
	cases := []struct {
		name  string
		other Finding
	}{
		{"nearby lines different substance", nearDup("cr.p2", "b", "m2", 0, 19, "correctness", SeverityP2, "Missing validation of zero parts", "parts equal to zero raises ZeroDivisionError which callers never handle, crashing the request")},
		{"similar text far apart", nearDup("cr.p2", "b", "m2", 0, 60, "correctness", SeverityP2, "split_evenly drops the remainder", splitEvenlyBody)},
		{"different category different substance", nearDup("cr.p2", "b", "m2", 0, 15, "security", SeverityP2, "Unbounded parts allows resource exhaustion", "parts is user controlled and never capped, so a huge value allocates an enormous list and exhausts memory")},
		{"nearby but unrelated wording", nearDup("cr.p2", "b", "m2", 0, 19, "correctness", SeverityP2, "Rounding mode undocumented", "docstring does not state how currency rounding behaves for negative totals")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Dedupe([]Finding{base, tc.other}); len(got) != 2 {
				t.Fatalf("groups = %d, want 2 for distinct issues", len(got))
			}
		})
	}
}

func TestDedupeNearDuplicatesDoNotInflateVotes(t *testing.T) {
	findings := []Finding{
		nearDup("cr.p1", "a", "m1", 0, 15, "correctness", SeverityP2, "split_evenly drops the remainder", splitEvenlyBody),
		nearDup("cr.p1", "a", "m1", 1, 15, "logic", SeverityP2, "split_evenly drops remainder cents", "split_evenly divides total by parts using floor division so the remainder is dropped and shares do not sum to the total"),
		nearDup("cr.p1", "a", "m1", 2, 15, "logic", SeverityP2, "split_evenly drops the remainder cents", "floor division in split_evenly drops the remainder so shares do not sum to the total"),
	}
	groups := Dedupe(findings)
	if len(groups) != 1 {
		t.Fatalf("groups = %d", len(groups))
	}
	if votes := CountVotes(groups[0].Sources, 0); votes > 2 {
		t.Fatalf("one model's 3 duplicates = %.2f votes, want at most 1 + 2*same-model weight", votes)
	}
}

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

func TestDedupeRealSplitEvenlyWorkflowMergesTriple(t *testing.T) {
	findings := loadRealFixture(t)
	groups := Dedupe(findings)
	var merged, structure *Group
	for i := range groups {
		if groups[i].Line == 2 {
			structure = &groups[i]
		} else {
			merged = &groups[i]
		}
	}
	if len(groups) != 2 || merged == nil || structure == nil {
		t.Fatalf("groups = %d, want the line-15 triple merged and line 2 separate", len(groups))
	}
	if len(merged.Sources) != 3 || merged.Severity != SeverityP2 {
		t.Fatalf("merged sources=%d severity=%s", len(merged.Sources), merged.Severity)
	}
	if len(structure.Sources) != 1 || structure.Severity != SeverityInfo {
		t.Fatalf("structure group = %+v", structure)
	}
}

func TestDedupeOverMergeCounterexamples(t *testing.T) {
	mk := func(node string, idx, line int, cat string, sev Severity, title, body string) Finding {
		f := outFinding("pkg/x.go", line, sev, title, "")
		f.Category, f.Body = cat, body
		return gatheredFinding("wf", node, "p", "m", 0, idx, f)
	}
	cases := []struct {
		name string
		a, b Finding
	}{
		{"split_evenly zero people vs negative total",
			mk("cr.p1", 0, 15, "error-handling", SeverityP2, "people=0 抛 ZeroDivisionError", "split_evenly 在 people 为 0 时抛出 ZeroDivisionError，调用方拿到无关异常"),
			mk("cr.p2", 0, 17, "correctness", SeverityP1, "负数 total_cents 余数分配错误", "split_evenly 在 total_cents 为负数时余数分配错误，people 份额之和不等于总额")},
		{"rows.Err vs defer Close",
			mk("cr.p1", 0, 40, "error-handling", SeverityP2, "`rows.Err()` 未检查", "遍历 rows 之后没有检查 rows.Err()，迭代错误被忽略，调用方 return nil"),
			mk("cr.p2", 0, 44, "resource-leak", SeverityP1, "rows 未 `defer Close` 导致连接泄漏", "查询返回的 rows 没有 defer Close，错误分支 return 时连接泄漏")},
		{"Handle err vs concurrent map",
			mk("cr.p1", 0, 30, "error-handling", SeverityP2, "Handle 忽略 err", "Handle 调用后忽略 err 直接 return nil，ctx 取消时 resp 为空"),
			mk("cr.p2", 0, 34, "concurrency", SeverityP1, "Handle 并发写 map", "Handle 在多个 goroutine 中并发写 map，ctx 取消时 err 返回 nil，resp 数据竞争")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Dedupe([]Finding{tc.a, tc.b}); len(got) != 2 {
				t.Fatalf("groups = %d, want 2", len(got))
			}
			if got := Dedupe([]Finding{tc.b, tc.a}); len(got) != 2 {
				t.Fatalf("reversed groups = %d, want 2", len(got))
			}
		})
	}
}

func TestDedupeIsOrderIndependentAndDoesNotChain(t *testing.T) {
	mk := func(idx, line int, cat string, sev Severity, title, body string) Finding {
		f := outFinding("config.py", line, sev, title, "")
		f.Category, f.Body = cat, body
		return gatheredFinding("wf", "cr.p1", "p", "m", 0, idx, f)
	}
	a := mk(0, 10, "error-handling", SeverityP2, "load_config 未处理 FileNotFoundError", "load_config 打开配置文件时未处理 FileNotFoundError，json 解析失败也没有兜底")
	b := mk(1, 16, "error-handling", SeverityP2, "json 解析失败", "load_config 中 json 解析失败时未处理异常，FileNotFoundError 之外的错误直接抛出")
	c := mk(2, 22, "security", SeverityP1, "settings 中 secret 被写入日志", "load_config 把 settings 中的 secret 写入日志，造成凭据泄漏")
	var want []string
	for i, order := range [][]Finding{{a, b, c}, {b, c, a}, {c, a, b}, {a, c, b}, {b, a, c}, {c, b, a}} {
		groups := Dedupe(order)
		var sig []string
		var security bool
		for _, g := range groups {
			ids := make([]string, 0, len(g.Sources))
			for _, src := range g.Sources {
				ids = append(ids, src.Title)
			}
			sort.Strings(ids)
			sig = append(sig, strings.Join(ids, "|"))
			if g.Severity == SeverityP1 && len(g.Sources) != 1 {
				security = true
			}
		}
		sort.Strings(sig)
		if security {
			t.Fatalf("order %d: security finding merged into another group: %v", i, sig)
		}
		if i == 0 {
			want = sig
		} else if strings.Join(sig, "\n") != strings.Join(want, "\n") {
			t.Fatalf("order %d partition %v differs from %v", i, sig, want)
		}
	}
}

func TestTitleIdentityIgnoresGenericIdentifiers(t *testing.T) {
	for _, pair := range [][2]string{
		{"Handle 忽略 err", "Handle 并发写 map"},
		{"return nil 被忽略", "nil 返回 return err"},
		{"`rows.Err()` 未检查", "rows 未 `defer Close` 导致连接泄漏"},
	} {
		if titleIdentityMatches(pair[0], pair[1]) {
			t.Fatalf("titles %q / %q must not match", pair[0], pair[1])
		}
	}
	if !titleIdentityMatches("split_evenly 未校验 people", "split_evenly 完全未校验 people，0 或负值") {
		t.Fatal("real fixture titles must match")
	}
}

func TestDedupeTitleContentCounterexamplesStaySeparate(t *testing.T) {
	mk := func(node string, line int, cat string, sev Severity, title, body string) Finding {
		f := outFinding("pkg/x.go", line, sev, title, "")
		f.Category, f.Body = cat, body
		return gatheredFinding("wf", node, "p"+node, "m", 0, 0, f)
	}
	const amountBody = "parse_amount 读取 raw amount 后写入 ledger，amount 的处理见调用方"
	cases := []struct {
		name string
		a, b Finding
	}{
		{"parse_amount negative vs precision cross-category same line",
			mk("cr.p1", 10, "correctness", SeverityP1, "parse_amount 未处理负数", amountBody),
			mk("cr.p2", 10, "precision", SeverityP2, "parse_amount 精度丢失", amountBody)},
		{"parse_amount same category five lines apart",
			mk("cr.p1", 10, "correctness", SeverityP1, "parse_amount 未处理负数", amountBody),
			mk("cr.p2", 15, "correctness", SeverityP2, "parse_amount 精度丢失", amountBody)},
		{"split_evenly validation vs remainder",
			mk("cr.p1", 15, "correctness", SeverityP2, "split_evenly 未校验 people 导致异常", "split_evenly 对 people 没有校验，total_cents 与 people 相关的异常"),
			mk("cr.p2", 15, "logic", SeverityP2, "split_evenly 余数全部分给第一人", "split_evenly 对 people 分账时 total_cents 的余数全部分给第一人")},
		{"ParseHeader overflow vs nil",
			mk("cr.p1", 20, "correctness", SeverityP1, "ParseHeader 整数溢出", "ParseHeader 解析 header 长度时 raw 值 overflow，buf 越界"),
			mk("cr.p2", 20, "nil-deref", SeverityP2, "ParseHeader 空指针", "ParseHeader 解析 header 时 raw 值 buf 为 nil 导致崩溃")},
		{"fetchUser XSS vs race",
			mk("cr.p1", 8, "security", SeverityP1, "fetchUser 存在 XSS", "fetchUser 把 user 数据写入 DOM，user name 未转义"),
			mk("cr.p2", 8, "concurrency", SeverityP2, "fetchUser 竞态", "fetchUser 在 user 数据写入 DOM 前存在竞态，user name 被覆盖")},
		{"english negative vs float",
			mk("cr.p1", 10, "correctness", SeverityP1, "parse_amount: negative amount", "parse_amount accepts a negative amount and writes it to the ledger"),
			mk("cr.p2", 10, "precision", SeverityP2, "parse_amount: float amount", "parse_amount accepts a float amount and writes it to the ledger")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Dedupe([]Finding{tc.a, tc.b}); len(got) != 2 {
				t.Fatalf("groups = %d, want 2", len(got))
			}
			if got := Dedupe([]Finding{tc.b, tc.a}); len(got) != 2 {
				t.Fatalf("reversed groups = %d, want 2", len(got))
			}
		})
	}
}

func TestApproximateMergeAddsNoVotes(t *testing.T) {
	a := nearDup("cr.p1", "a", "m1", 0, 15, "correctness", SeverityP1, "split_evenly drops the remainder", splitEvenlyBody)
	b := nearDup("cr.p2", "b", "m2", 0, 15, "logic", SeverityP1, "split_evenly drops remainder cents", splitEvenlyBody)
	for _, in := range [][]Finding{{a, b}, {b, a}} {
		groups := Dedupe(in)
		if len(groups) != 1 || len(groups[0].Sources) != 2 {
			t.Fatalf("groups = %d, want 1 approximate merge", len(groups))
		}
		if votes := countVotes(groups[0].Sources, 0, nil); votes != 1 {
			t.Fatalf("approximate merge gave %.2f votes, want 1 (no consensus gain)", votes)
		}
		if meetsVotes(countVotes(groups[0].Sources, 0, nil), 2) {
			t.Fatal("approximate merge reached MinVotes 2 and would skip challenge")
		}
	}
	// The original same-category proximity rule still counts independent models.
	c := nearDup("cr.p2", "b", "m2", 0, 16, "correctness", SeverityP1, "anything", "different words")
	groups := Dedupe([]Finding{a, c})
	if len(groups) != 1 || countVotes(groups[0].Sources, 0, nil) != 2 {
		t.Fatalf("same-category proximity merge must keep independent votes: %+v", groups)
	}
}

func TestTitleContentOverlapReal(t *testing.T) {
	f := loadRealFixture(t)
	for _, p := range [][2]int{{0, 1}, {0, 3}, {1, 3}} {
		n := sharedCount(cjkBigrams(f[p[0]].Title), cjkBigrams(f[p[1]].Title))
		if n < nearDuplicateTitleContent {
			t.Fatalf("fixture titles %d/%d share %d CJK bigrams", p[0], p[1], n)
		}
	}
	if len(englishContentWords("ParseHeader parse_amount v2 fetchUser")) != 0 {
		t.Fatal("code identifiers must not be English content words")
	}
}

func TestRenderFindingListsRelatedDescriptions(t *testing.T) {
	a := nearDup("cr.p1", "a", "m1", 0, 15, "correctness", SeverityP2, "split_evenly drops the remainder", splitEvenlyBody)
	b := nearDup("cr.p2", "b", "m2", 0, 15, "security", SeverityP1, "split_evenly drops remainder cents", splitEvenlyBody)
	groups := Dedupe([]Finding{a, b})
	if len(groups) != 1 {
		t.Fatalf("groups = %d", len(groups))
	}
	group := groups[0]
	group.PositionOK = true
	group.Decision = &FindingDecision{Action: ActionPublish}
	body, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	other := a.Title
	if group.Title == a.Title {
		other = b.Title
	}
	if !strings.Contains(body, "相关描述") || !strings.Contains(body, escapeOneLine(other, MaxTitleRunes)) {
		t.Fatalf("related descriptions missing the other member title %q:\n%s", other, body)
	}
	single := Dedupe([]Finding{a})[0]
	single.PositionOK, single.Decision = true, &FindingDecision{Action: ActionPublish}
	if body, _ := RenderFinding(single, FindingRenderOptions{}); strings.Contains(body, "相关描述") {
		t.Fatalf("single-source finding must not list related descriptions:\n%s", body)
	}
}

func publishedBody(t *testing.T, g Group) string {
	t.Helper()
	g.PositionOK = true
	g.Decision = &FindingDecision{Action: ActionPublish}
	body, err := RenderFinding(g, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestDedupeSeverityGapBlocksApproximateMerge(t *testing.T) {
	a := nearDup("cr.p1", "a", "m1", 0, 20, "error-handling", SeverityP1, "fetch_user 未处理异常", "fetch_user 调用 requests 时没有处理 requests.Timeout，请求挂起")
	a.Severity = SeverityP0
	b := nearDup("cr.p2", "b", "m2", 0, 20, "logic", SeverityP2, "fetch_user 未处理异常", "fetch_user 解析响应时没有处理 JSONDecodeError，请求失败")
	for _, in := range [][]Finding{{a, b}, {b, a}} {
		if got := Dedupe(in); len(got) != 2 {
			t.Fatalf("P0 and P2 must not be approximately merged, groups = %d", len(got))
		}
	}
}

func TestApproximateMergeShowsHighestSeverityAndAllBodies(t *testing.T) {
	a := nearDup("cr.p1", "a", "m1", 0, 20, "error-handling", SeverityP1, "fetch_user 未处理异常", "fetch_user 发送请求后没有处理异常 requests.Timeout，导致调用方崩溃")
	b := nearDup("cr.p2", "b", "m2", 0, 20, "logic", SeverityP2, "fetch_user 未处理异常", "fetch_user 发送请求后没有处理异常 JSONDecodeError，导致调用方崩溃并且正文更长一些")
	c := nearDup("cr.p3", "c", "m3", 0, 20, "logic", SeverityP2, "load_config 缺少空值检查", "load_config 读取配置后对 path 变量没有做空值检查，直接解引用会导致崩溃")
	d := nearDup("cr.p4", "d", "m4", 0, 20, "style", SeverityP2, "load_config 缺少空值检查", "load_config 读取配置后对 mode 变量没有做空值检查，直接解引用会导致崩溃")
	for _, in := range [][]Finding{{a, b}, {b, a}} {
		groups := Dedupe(in)
		if len(groups) != 1 {
			t.Fatalf("groups = %d, want 1", len(groups))
		}
		if groups[0].Severity != SeverityP1 || !strings.Contains(groups[0].Body, "requests.Timeout") {
			t.Fatalf("display must come from the highest-severity member: %q", groups[0].Body)
		}
		if body := publishedBody(t, groups[0]); !strings.Contains(body, "JSONDecodeError") {
			t.Fatalf("other member body missing:\n%s", body)
		}
	}
	for _, in := range [][]Finding{{c, d}, {d, c}} {
		groups := Dedupe(in)
		if len(groups) != 1 {
			t.Fatalf("same-title cross-category groups = %d, want 1", len(groups))
		}
		body := publishedBody(t, groups[0])
		if !strings.Contains(body, "path") || !strings.Contains(body, "mode") {
			t.Fatalf("both variables must be visible:\n%s", body)
		}
	}
}

func TestRelatedDescriptionsCapAndCount(t *testing.T) {
	g := Group{Title: "T0", Body: "b0", Severity: SeverityP2}
	for i := 0; i < 8; i++ {
		g.Sources = append(g.Sources, Finding{Title: fmt.Sprintf("T%d", i), Body: fmt.Sprintf("b%d", i), Severity: SeverityP2, Approximate: i > 0})
	}
	out := relatedDescriptions(g)
	if strings.Count(out, "\n- [") != 5 || !strings.Contains(out, "另有 2 条") {
		t.Fatalf("related list = %q", out)
	}
}

func TestExactDuplicateIsNotStolenByCrossCategoryAnchor(t *testing.T) {
	a := nearDup("cr.p1", "a", "m1", 0, 10, "logic", SeverityP2, "alpha handling", "alpha body one")
	b := nearDup("cr.p2", "b", "m2", 0, 11, "logic", SeverityP2, "beta handling", "other words entirely")
	c := nearDup("cr.p3", "c", "m3", 0, 10, "correctness", SeverityP2, "gamma handling", "something else")
	for i, in := range [][]Finding{{a, b, c}, {c, b, a}, {b, c, a}} {
		groups := Dedupe(in)
		if len(groups) != 2 {
			t.Fatalf("order %d: groups = %d, want 2", i, len(groups))
		}
		for _, g := range groups {
			if len(g.Sources) == 2 {
				if countVotes(g.Sources, 0, nil) != 2 {
					t.Fatalf("exact pair must keep independent votes")
				}
				return
			}
		}
		t.Fatalf("order %d: a and b were not merged", i)
	}
}

func TestApproximateFlagSurvivesJSONRoundTrip(t *testing.T) {
	a := nearDup("cr.p1", "a", "m1", 0, 15, "correctness", SeverityP1, "split_evenly drops the remainder", splitEvenlyBody)
	b := nearDup("cr.p2", "b", "m2", 0, 15, "logic", SeverityP1, "split_evenly drops remainder cents", splitEvenlyBody)
	groups := Dedupe([]Finding{a, b})
	if len(groups) != 1 {
		t.Fatalf("groups = %d", len(groups))
	}
	raw, err := json.Marshal(groups[0].Sources)
	if err != nil {
		t.Fatal(err)
	}
	var back []Finding
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if want, got := countVotes(groups[0].Sources, 0, nil), countVotes(back, 0, nil); want != got || got != 1 {
		t.Fatalf("votes before=%.2f after round trip=%.2f, want both 1", want, got)
	}
}
