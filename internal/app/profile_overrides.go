package app

import (
	"encoding/json"

	"github.com/LeXwDeX/tribunal/internal/config"
)

// profileOverrideDocument is the persisted subset of node provider choices.
// Parameter overrides are validated by the store and applied by later work.
type profileOverrideDocument struct {
	Default *profileNodeOverride `json:"default"`
	Explore *profileNodeOverride `json:"explore"`
	Lanes   struct {
		CodeReview *profileNodeOverride `json:"code_review"`
		Review     *profileNodeOverride `json:"review"`
		Reasoner   *profileNodeOverride `json:"reasoner"`
		Validate   *profileNodeOverride `json:"validate"`
	} `json:"lanes"`
	Challenge *profileNodeOverride `json:"challenge"`
}

type profileNodeOverride struct {
	Model string `json:"model"`
}

func parseProfileOverrides(body string) profileOverrideDocument {
	var doc profileOverrideDocument
	_ = json.Unmarshal([]byte(body), &doc)
	return doc
}

func applyProfileOverrides(cfg config.ProfileConfig, doc profileOverrideDocument) config.ProfileConfig {
	if doc.Default != nil && doc.Default.Model != "" {
		cfg.Provider = doc.Default.Model
	}
	if doc.Explore != nil && doc.Explore.Model != "" {
		if cfg.Explore == nil {
			cfg.Explore = &config.ExploreConfig{}
		}
		cfg.Explore.Provider = doc.Explore.Model
	}
	if doc.Lanes.CodeReview != nil && doc.Lanes.CodeReview.Model != "" {
		if cfg.Lanes == nil {
			cfg.Lanes = &config.LanesConfig{}
		}
		cfg.Lanes.CodeReview.Provider = doc.Lanes.CodeReview.Model
	}
	if doc.Lanes.Review != nil && doc.Lanes.Review.Model != "" {
		if cfg.Lanes == nil {
			cfg.Lanes = &config.LanesConfig{}
		}
		cfg.Lanes.Review.Provider = doc.Lanes.Review.Model
	}
	if doc.Lanes.Reasoner != nil && doc.Lanes.Reasoner.Model != "" {
		if cfg.Lanes == nil {
			cfg.Lanes = &config.LanesConfig{}
		}
		cfg.Lanes.Reasoner.Provider = doc.Lanes.Reasoner.Model
	}
	if doc.Lanes.Validate != nil && doc.Lanes.Validate.Model != "" {
		if cfg.Lanes == nil {
			cfg.Lanes = &config.LanesConfig{}
		}
		cfg.Lanes.Validate.Provider = doc.Lanes.Validate.Model
	}
	if doc.Challenge != nil && doc.Challenge.Model != "" {
		cfg.Challenge.Provider = doc.Challenge.Model
	}
	return cfg
}

// ApplyProfileOverrides merges a stored override document into a profile.
func ApplyProfileOverrides(cfg config.ProfileConfig, body string) config.ProfileConfig {
	return applyProfileOverrides(cfg, parseProfileOverrides(body))
}
