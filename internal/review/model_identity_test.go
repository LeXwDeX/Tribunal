package review

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

func identityFinding(provider, model string) []Proposal {
	return []Proposal{{
		Node:     "cr." + provider,
		Kind:     ContractCodeReview,
		Provider: provider,
		Model:    model,
		Findings: []FindingOut{finding("a.go", 10, SeverityP1, "same issue")},
	}}
}

func TestGatherDoesNotGiveAliasesIndependentVotes(t *testing.T) {
	identities := map[string]string{
		"alias-a": "openai-chat\x1fhttps://gateway.example/v1\x1fsame-model",
		"alias-b": "openai-chat\x1fhttps://gateway.example/v1\x1fsame-model",
		"other":   "anthropic\x1fhttps://other.example\x1fdifferent-model",
	}
	proposals := append(identityFinding("alias-a", "same-model"), identityFinding("alias-b", "same-model")...)
	result, err := Gather(GatherInput{WorkflowID: "wf", Proposals: proposals}, GatherOptions{
		MinVotes:          2,
		ChallengeOnly:     []Severity{SeverityP0, SeverityP1},
		ChallengeProvider: "cross",
		AllowedProviders:  []string{"alias-a", "alias-b", "other"},
		Identities:        identities,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Groups) != 1 {
		t.Fatalf("groups = %d", len(result.Groups))
	}
	if result.Groups[0].Votes != 1.5 {
		t.Fatalf("alias votes = %v, want 1.5", result.Groups[0].Votes)
	}
	if result.Groups[0].Consensus {
		t.Fatal("aliases reached two-vote consensus")
	}
	if len(result.ToChallenge) != 1 || result.ToChallenge[0].Provider != "other" {
		t.Fatalf("challenge target = %+v, want the independent model", result.ToChallenge)
	}
}

func TestGatherCrossUsesIdentityNotProviderName(t *testing.T) {
	identities := map[string]string{
		"a": "same-identity",
		"b": "same-identity",
		"c": "different-identity",
	}
	result, err := Gather(GatherInput{WorkflowID: "wf", Proposals: identityFinding("a", "m")}, GatherOptions{
		ChallengeProvider: "cross",
		AllowedProviders:  []string{"a", "b", "c"},
		Identities:        identities,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToChallenge) != 1 || result.ToChallenge[0].Provider != "c" || result.ToChallenge[0].SameModel {
		t.Fatalf("challenge target = %+v, want c as the independent alias target", result.ToChallenge)
	}

	result, err = Gather(GatherInput{WorkflowID: "wf", Proposals: identityFinding("a", "m")}, GatherOptions{
		ChallengeProvider: "cross",
		AllowedProviders:  []string{"a", "b"},
		FallbackProvider:  "b",
		Identities:        identities,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToChallenge) != 1 || result.ToChallenge[0].Provider != "b" || !result.ToChallenge[0].SameModel {
		t.Fatalf("fallback target = %+v, want same-model fallback", result.ToChallenge)
	}
	if len(result.Warnings) == 0 || !strings.Contains(result.Warnings[0], "not cross-model confirmation") {
		t.Fatalf("fallback warnings = %v", result.Warnings)
	}
}

func TestGatherUnknownModelIdentityIsConservative(t *testing.T) {
	result, err := Gather(GatherInput{WorkflowID: "wf", Proposals: identityFinding("a", "m")}, GatherOptions{
		MinVotes:          2,
		ChallengeOnly:     []Severity{SeverityP1},
		ChallengeProvider: "cross",
		AllowedProviders:  []string{"a", "b"},
		FallbackProvider:  "a",
		Identities:        map[string]string{"a": "known", "b": ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Groups[0].Consensus {
		t.Fatal("unknown identity was treated as an independent vote")
	}
	if len(result.ToChallenge) != 1 || result.ToChallenge[0].Provider != "a" || !result.ToChallenge[0].SameModel {
		t.Fatalf("target = %+v, want conservative fallback", result.ToChallenge)
	}
}

func TestGatherWithoutIdentityMetadataKeepsProviderDefaults(t *testing.T) {
	result, err := Gather(GatherInput{WorkflowID: "wf", Proposals: append(identityFinding("a", "m"), identityFinding("b", "m")...)}, GatherOptions{
		MinVotes:   2,
		Identities: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Groups[0].Consensus {
		t.Fatal("legacy provider/model votes lost consensus")
	}
}

func TestExecutorGatherUsesProviderModelIdentity(t *testing.T) {
	dossier := validDossier(t)
	nodes := []dag.NodeView{
		completedNode("cr.alias-a", KindCodeReview, reviewNodeParams{Provider: "alias-a", Files: []string{"a.go"}}, CodeReviewOut{
			Summary: "a", Findings: []FindingOut{finding("a.go", 10, SeverityP1, "shared issue")},
		}),
		completedNode("cr.alias-b", KindCodeReview, reviewNodeParams{Provider: "alias-b", Files: []string{"a.go"}}, CodeReviewOut{
			Summary: "b", Findings: []FindingOut{finding("a.go", 10, SeverityP1, "shared issue")},
		}),
	}
	wf := evidenceWorkflow(nodes...)
	wf.Spec.Budget.MaxNodes = 30
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier:  dossier,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) { return wf, nil },
		Locate:   func(string, int, Side) bool { return true },
		Clients: metadataClients{
			"alias-a": metadataClient{model: "same-model"},
			"alias-b": metadataClient{model: "same-model"},
			"other":   metadataClient{model: "other-model"},
		},
	})
	params, _ := json.Marshal(gatherParams{
		ChallengeOnly:     []Severity{SeverityP1},
		MinVotes:          2,
		ChallengeProvider: "cross",
		AllowedProviders:  []string{"alias-a", "alias-b", "other"},
	})
	out, err := executor.Execute(context.Background(), dag.NodeInput{
		WorkflowID: wf.ID,
		Node:       dag.NodeSpec{ID: NodeGather, Kind: KindGather, Params: params},
	})
	if err != nil {
		t.Fatal(err)
	}
	gathered, err := decodeGatherOut(out.Data)
	if err != nil {
		t.Fatal(err)
	}
	if len(gathered.Groups) != 1 || gathered.Groups[0].Consensus {
		t.Fatalf("aliases reached consensus: %+v", gathered.Groups)
	}
	if len(gathered.ToChallenge) != 1 || gathered.ToChallenge[0].Provider != "other" {
		t.Fatalf("challenge target = %+v, want other", gathered.ToChallenge)
	}
}

func TestGatherSameProviderChannelsUseOneIdentity(t *testing.T) {
	proposals := append(identityFinding("alias-a", "same-model"), identityFinding("alias-a", "same-model")...)
	proposals[0].Node = "rv.std.p1"
	proposals[1].Node = "rv.intent.p1"
	result, err := Gather(GatherInput{WorkflowID: "wf", Proposals: proposals}, GatherOptions{
		MinVotes:      2,
		ChallengeOnly: []Severity{SeverityP2},
		Identities:    map[string]string{"alias-a": "same-identity"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Groups) != 1 || result.Groups[0].Consensus {
		t.Fatalf("same-provider channels reached consensus: %+v", result.Groups)
	}
	if result.Groups[0].Votes != 1.5 {
		t.Fatalf("votes = %v, want 1.5", result.Groups[0].Votes)
	}
}
