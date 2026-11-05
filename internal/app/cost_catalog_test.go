package app

import (
	"context"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type costCatalogStore struct{}

func (costCatalogStore) ListModels(context.Context) ([]store.Model, error) {
	return []store.Model{{
		ID: "user:cost", Protocol: "openai-chat", Model: "m",
		Params: store.ModelParams{
			MaxTokens: 1, MaxConcurrency: 1,
			Price: &config.PriceConfig{InputPerMTok: 2, OutputPerMTok: 4},
		},
	}}, nil
}

func (costCatalogStore) OpenModelCredential(context.Context, string) ([]byte, error) {
	return nil, nil
}

func TestCostFuncUsesUserModelPrice(t *testing.T) {
	catalog, err := llm.NewCatalog(context.Background(), nil, costCatalogStore{})
	if err != nil {
		t.Fatal(err)
	}
	cost := costFunc(catalog, nil)
	usage := llm.Usage{InputTokens: 1_000_000, OutputTokens: 500_000}
	if got := cost("user:cost", usage); got != 4_000_000 {
		t.Fatalf("cost = %d micros, want 4000000", got)
	}
}
