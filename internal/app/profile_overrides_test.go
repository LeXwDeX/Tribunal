package app

import (
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
)

func TestResolveProfileNodesSources(t *testing.T) {
	cfg := config.ProfileConfig{
		Provider: "base",
		Explore:  &config.ExploreConfig{Provider: "config-explore"},
		Lanes: &config.LanesConfig{
			CodeReview: config.LaneConfig{Provider: "config-code"},
		},
		Challenge: config.ChallengeConfig{Provider: "config-challenge"},
	}
	body := `{"default":{"model":"override-default"},"lanes":{"review":{"model":"override-review"}}}`
	nodes := ResolveProfileNodes(cfg, body)
	got := map[string][2]string{}
	for _, node := range nodes {
		got[node.Node] = [2]string{node.Model, node.Source}
	}
	if got["default"] != [2]string{"override-default", "override"} {
		t.Fatalf("default = %v", got["default"])
	}
	if got["explore"] != [2]string{"config-explore", "config"} {
		t.Fatalf("explore = %v", got["explore"])
	}
	if got["lanes.code_review"] != [2]string{"config-code", "config"} {
		t.Fatalf("code review = %v", got["lanes.code_review"])
	}
	if got["lanes.review"] != [2]string{"override-review", "override"} {
		t.Fatalf("review = %v", got["lanes.review"])
	}
	if got["lanes.reasoner"] != [2]string{"base", "config_default"} {
		t.Fatalf("reasoner = %v", got["lanes.reasoner"])
	}
	if got["challenge"] != [2]string{"config-challenge", "config"} {
		t.Fatalf("challenge = %v", got["challenge"])
	}
}
