package app

import (
	"bytes"
	"context"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// mutableModelStore lets a test add user models after the catalog was built.
type mutableModelStore struct {
	mu     sync.Mutex
	models []store.Model
}

func (m *mutableModelStore) ListModels(context.Context) ([]store.Model, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.Model(nil), m.models...), nil
}

func (m *mutableModelStore) OpenModelCredential(context.Context, string) ([]byte, error) {
	return []byte("sk-dynamic-secret-bytes"), nil
}

func (m *mutableModelStore) GetModelDefaults(context.Context) (*store.ModelDefaults, error) {
	return &store.ModelDefaults{}, nil
}

func (m *mutableModelStore) set(models ...store.Model) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.models = models
}

func userModel(id, baseURL, model string, revision int64) store.Model {
	return store.Model{
		ID: id, DisplayName: id, Protocol: "openai-chat", BaseURL: baseURL, Model: model,
		Revision: revision, HasAPIKey: true,
		Params: store.ModelParams{MaxTokens: 1000, MaxConcurrency: 1},
	}
}

func attachCatalog(t *testing.T, f *reviewFixture, models ...store.Model) (*mutableModelStore, *llm.Catalog) {
	t.Helper()
	ms := &mutableModelStore{}
	ms.set(models...)
	catalog, err := llm.NewCatalog(context.Background(), nil, ms)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	f.service.catalog = catalog
	return ms, catalog
}

func profileWithProvider(provider string) review.Profile {
	cfg := testReviewProfile()
	cfg.Propose[0].Provider = provider
	return review.Profile{Name: "default", Config: cfg}
}

func TestReviewProfileHashIgnoresUnreferencedUserModels(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	profile := review.Profile{Name: "default", Config: f.profiles["default"]}
	before := f.service.reviewProfileHash(profile)

	attachCatalog(t, f, userModel("user:unused", "https://llm.example.test/v1", "m1", 1))
	if got := f.service.reviewProfileHash(profile); got != before {
		t.Fatalf("hash changed after adding an unreferenced user model: %s != %s", got, before)
	}

	// A restart rebuilds the service with the model already in the catalog;
	// the static provider list must still be configured names only.
	f2 := newReviewFixture(t, reviewTestHead)
	attachCatalog(t, f2, userModel("user:unused", "https://llm.example.test/v1", "m1", 1))
	if got := f2.service.reviewProfileHash(profile); got != before {
		t.Fatalf("hash after restart with user model = %s, want %s", got, before)
	}
}

func TestReviewProfileHashTracksReferencedUserModel(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	ms, catalog := attachCatalog(t, f, userModel("user:ref", "https://llm.example.test/v1", "m1", 1))
	profile := profileWithProvider("user:ref")
	h1 := f.service.reviewProfileHash(profile)
	if h1 == "" {
		t.Fatal("hash is empty")
	}
	if h1 == f.service.reviewProfileHash(review.Profile{Name: "default", Config: f.profiles["default"]}) {
		t.Fatal("referenced user model must contribute to the hash")
	}

	ms.set(userModel("user:ref", "https://llm.example.test/v2", "m1", 2))
	if err := catalog.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	h2 := f.service.reviewProfileHash(profile)
	if h2 == h1 {
		t.Fatal("hash must change when a referenced user model base_url changes")
	}

	ms.set(userModel("user:ref", "https://llm.example.test/v2", "m2", 3))
	if err := catalog.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h3 := f.service.reviewProfileHash(profile); h3 == h2 {
		t.Fatal("hash must change when a referenced user model name changes")
	}
}

func TestReviewProfileHashOmitsCredentials(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	attachCatalog(t, f, userModel("user:ref", "https://llm.example.test/v1", "m1", 1))
	data := f.service.reviewProfileHashPayload(profileWithProvider("user:ref"))
	if !bytes.Contains(data, []byte(`"user_models"`)) || !bytes.Contains(data, []byte("llm.example.test")) {
		t.Fatalf("payload lacks the referenced user model: %s", data)
	}
	for _, secret := range []string{"sk-dynamic-secret-bytes"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("hash payload contains %q", secret)
		}
	}
	if bytes.Contains(data, []byte(`"api_key_env":"X`)) || !bytes.Contains(data, []byte(`"user_models":{"user:ref":{`)) {
		t.Fatalf("user model entry malformed: %s", data)
	}
}

func TestGraphProvidersIncludeUserModelsCreatedAfterStartup(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	ms, catalog := attachCatalog(t, f)
	if err := review.ValidateProfileProviders(profileWithProvider("user:late"), f.service.graphProviders()); err == nil {
		t.Fatal("unknown user model must be rejected")
	}
	ms.set(userModel("user:late", "https://llm.example.test/v1", "m1", 1))
	if err := catalog.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := review.ValidateProfileProviders(profileWithProvider("user:late"), f.service.graphProviders()); err != nil {
		t.Fatalf("user model created after startup rejected: %v", err)
	}
	ms.set()
	if err := catalog.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := review.ValidateProfileProviders(profileWithProvider("user:late"), f.service.graphProviders()); err == nil {
		t.Fatal("deleted user model must be rejected")
	}
}

func TestReviewRunReachesWorkflowWithUserModelCreatedAfterStartup(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	ms, catalog := attachCatalog(t, f)
	ms.set(userModel("user:late", "https://llm.example.test/v1", "m1", 1))
	if err := catalog.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.profiles["default"] = profileWithProvider("user:late").Config
	f.service.profiles["default"] = f.profiles["default"]

	job := f.seedReview(t, "job-late-user-model")
	_ = f.service.RunReview(context.Background(), job)
	if _, err := f.db.GetWorkflowByJob(context.Background(), job.ID); err != nil {
		t.Fatalf("workflow was not created for a runtime-created user model: %v", err)
	}
}

func TestRunReviewLogsStageAndErrorClassOnPermanentFailure(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	attachCatalog(t, f)
	var buf bytes.Buffer
	f.service.logger = slog.New(slog.NewTextHandler(&buf, nil))
	f.profiles["default"] = profileWithProvider("user:missing").Config
	f.service.profiles["default"] = f.profiles["default"]

	job := f.seedReview(t, "job-permanent-log")
	err := f.service.RunReview(context.Background(), job)
	if !IsPermanentJob(err) {
		t.Fatalf("RunReview error = %v, want permanent", err)
	}
	out := buf.String()
	for _, want := range []string{
		"review run failed permanently", "project_id=42", "mr_iid=7", "stage=execution",
		"error_class=" + errclass.Of(err),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "user:missing") {
		t.Fatalf("log leaks the raw error:\n%s", out)
	}
}

// A user model that already exists when the runtime starts must not leak into
// the provider list that feeds the profile hash (#299, #327): a restart would
// otherwise change every in-flight workflow's hash.
func TestRuntimeHashedProvidersExcludeStartupUserModels(t *testing.T) {
	ctx := context.Background()
	t.Setenv("RUNTIME_PROVIDER_TEST_KEY", "k")
	st := openTestStore(t)
	if err := st.ConfigureModelSecrets(ctx, newTestCipher(t)); err != nil {
		t.Fatalf("ConfigureModelSecrets: %v", err)
	}
	created, err := st.CreateModel(ctx, userModel("user:startup", "https://llm.example.test/v1", "m1", 1), "sk-startup-secret")
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	cfg := &config.Config{
		Workspace: config.WorkspaceConfig{Root: t.TempDir(), RunCleanupAfter: time.Hour},
		Storage:   config.StorageConfig{LLMCallRetention: time.Hour, DeliveryRetention: time.Hour, JobRetention: time.Hour, ContextVersions: 1},
		Engine:    config.EngineConfig{MaxConcurrentNodes: 1, DefaultNodeTimeout: time.Minute},
		Profiles:  testProfiles(),
		Providers: map[string]config.ProviderConfig{
			"cfgprov": {Protocol: "openai-chat", BaseURL: "https://cfg.example.test/v1", Model: "m", APIKeyEnv: "RUNTIME_PROVIDER_TEST_KEY", MaxTokens: 100, MaxConcurrency: 1},
		},
	}
	runtime, err := NewRuntime(ctx, cfg, RuntimeDeps{
		Store:  st,
		Cipher: newTestCipher(t),
		GitLab: newRuntimeTestGitLab(),
		Git:    repoGit{git: &repo.Git{}},
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	if _, ok := runtime.catalog.Resolve(created.ID); !ok {
		t.Fatal("fixture user model is missing from the catalog")
	}
	got := runtime.reviews.providerNames
	if len(got) != 1 || got[0] != "cfgprov" {
		t.Fatalf("hashed provider list = %v, want configured names only", got)
	}
	if graph := runtime.reviews.graphProviders(); !slices.Contains(graph, created.ID) {
		t.Fatalf("graph validation list = %v, want it to include the catalog user model", graph)
	}
}
