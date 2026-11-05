package llm

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type catalogStore struct {
	mu     sync.Mutex
	models []store.Model
}

func (s *catalogStore) ListModels(context.Context) ([]store.Model, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.Model(nil), s.models...), nil
}
func (s *catalogStore) OpenModelCredential(_ context.Context, id string) ([]byte, error) {
	if id == "user:bad" {
		return nil, errors.New("unavailable")
	}
	return nil, nil
}

func TestCatalogRefreshIsolatesFailuresAndKeepsOldClients(t *testing.T) {
	t.Setenv("CATALOG_TEST_KEY", "key")
	s := &catalogStore{models: []store.Model{
		{ID: "user:good", DisplayName: "Good", Protocol: "openai-chat", Model: "same", Revision: 1, Params: store.ModelParams{MaxTokens: 1, MaxConcurrency: 1}},
		{ID: "user:bad", DisplayName: "Bad", Protocol: "openai-chat", Model: "same", Revision: 1, Params: store.ModelParams{MaxTokens: 1, MaxConcurrency: 1}},
	}}
	c, err := NewCatalog(context.Background(), map[string]config.ProviderConfig{"config-model": {Protocol: "openai-chat", Model: "same", APIKeyEnv: "CATALOG_TEST_KEY"}}, s)
	if err != nil {
		t.Fatal(err)
	}
	old, err := c.Client("user:good")
	if err != nil {
		t.Fatal(err)
	}
	configured, err := c.Client("config:config-model")
	if err != nil {
		t.Fatal(err)
	}
	if old.Info().ModelIdentity() != configured.Info().ModelIdentity() {
		t.Fatal("same endpoint and model must have same identity")
	}
	bad, ok := c.Resolve("user:bad")
	if !ok || !bad.Unavailable || bad.UnavailableReason == "" {
		t.Fatalf("bad model status = %+v", bad)
	}
	if _, err := c.Client("user:bad"); err == nil {
		t.Fatal("bad model must not have a client")
	}
	if _, ok := c.Resolve("config:user:good"); ok {
		t.Fatal("config alias must not resolve a user model")
	}
	if _, err := c.Client("config:user:good"); err == nil {
		t.Fatal("config alias must not resolve a user client")
	}
	s.mu.Lock()
	s.models[0].Revision++
	s.models[0].Model = "updated"
	s.mu.Unlock()
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	updated, err := c.Client("user:good")
	if err != nil {
		t.Fatal(err)
	}
	if updated == old || old.Info().Model != "same" || updated.Info().Model != "updated" {
		t.Fatal("refresh must replace only the changed client")
	}
	configuredAfter, err := c.Client("config-model")
	if err != nil || configuredAfter != configured {
		t.Fatal("configured client must be preserved")
	}
}

func TestCatalogConcurrentRefreshAndReads(t *testing.T) {
	s := &catalogStore{models: []store.Model{{ID: "user:good", DisplayName: "Good", Protocol: "openai-chat", Model: "m", Revision: 1, Params: store.ModelParams{MaxTokens: 1, MaxConcurrency: 1}}}}
	c, err := NewCatalog(context.Background(), nil, s)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = c.Client("user:good")
				_, _ = c.Resolve("user:good")
				_ = c.List()
				snapshot := c.Snapshot()
				client, err := snapshot.Client("user:good")
				model, ok := snapshot.Resolve("user:good")
				if err != nil || !ok || client.Info().Model != model.Model.Model {
					t.Error("snapshot client and model must use the same catalog generation")
					return
				}
			}
		}()
	}
	for i := 0; i < 100; i++ {
		s.mu.Lock()
		s.models[0].Revision++
		s.mu.Unlock()
		if err := c.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestCatalogSnapshotSurvivesRefresh(t *testing.T) {
	s := &catalogStore{models: []store.Model{{ID: "user:good", DisplayName: "Good", Protocol: "openai-chat", BaseURL: "http://127.0.0.1/old", Model: "old", Revision: 1, Params: store.ModelParams{MaxTokens: 1, MaxConcurrency: 1}}}}
	c, err := NewCatalog(context.Background(), nil, s)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := c.Snapshot()
	old, err := snapshot.Client("user:good")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.models[0].Revision++
	s.models[0].BaseURL = "http://127.0.0.1/new"
	s.models[0].Model = "new"
	s.mu.Unlock()
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := snapshot.Client("user:good")
	if err != nil || got != old || got.Info().BaseURL != "http://127.0.0.1/old" {
		t.Fatal("snapshot did not retain the original client")
	}
	model, ok := snapshot.Resolve("user:good")
	if !ok || model.BaseURL != "http://127.0.0.1/old" || model.Model.Model != "old" {
		t.Fatal("snapshot did not retain the original model configuration")
	}
	current, err := c.Client("user:good")
	if err != nil || current == old || current.Info().Model != "new" {
		t.Fatal("catalog did not publish the new client")
	}
}

func TestCatalogLabelsExposeUserDisplayNames(t *testing.T) {
	s := &catalogStore{models: []store.Model{{ID: "user:good", DisplayName: "Good Name", Protocol: "openai-chat", Model: "m", Revision: 1, Params: store.ModelParams{MaxTokens: 1, MaxConcurrency: 1}}}}
	c, err := NewCatalog(context.Background(), nil, s)
	if err != nil {
		t.Fatal(err)
	}
	labels := c.Labels()
	if labels["user:good"] != "Good Name" {
		t.Fatalf("labels = %v", labels)
	}
	if _, ok := labels["config-model"]; ok {
		t.Fatal("configured models must not be relabeled")
	}
}

func TestProviderClientsAcceptEmptyKey(t *testing.T) {
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic"} {
		cfg := config.ProviderConfig{Protocol: protocol, BaseURL: "https://api.example.com", Model: "m", MaxTokens: 1, MaxConcurrency: 1}
		if _, err := newProviderClientWithKey("test", cfg, "", http.DefaultClient, registryOptions{}); err != nil {
			t.Fatalf("%s rejected empty key: %v", protocol, err)
		}
	}
}
