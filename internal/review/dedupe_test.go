package review

import (
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
