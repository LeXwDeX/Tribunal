package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestModelEndpointsRequirePlatformAdminAndCSRF(t *testing.T) {
	env := newM6Env(t, m6Options{})
	viewer := env.loginAs("code-viewer")
	adminWithoutCSRF := env.loginAs("code-admin").withoutCSRF()
	endpoints := []struct {
		name, method, path string
	}{
		{"list", http.MethodGet, "/api/v1/models"},
		{"presets", http.MethodGet, "/api/v1/model-presets"},
		{"detail", http.MethodGet, "/api/v1/models/user:missing"},
		{"create", http.MethodPost, "/api/v1/models"},
		{"update", http.MethodPatch, "/api/v1/models/user:missing"},
		{"params update", http.MethodPatch, "/api/v1/models/user:missing/params"},
		{"apply preset", http.MethodPost, "/api/v1/models/user:missing/preset"},
		{"delete", http.MethodDelete, "/api/v1/models/user:missing?revision=1"},
		{"saved test", http.MethodPost, "/api/v1/models/user:missing/test"},
		{"draft test", http.MethodPost, "/api/v1/models:test"},
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			response := viewer.request(endpoint.method, endpoint.path, nil, nil)
			if response.status != http.StatusForbidden {
				t.Errorf("viewer status = %d, want 403", response.status)
			}
			if !safeMethod(endpoint.method) {
				response = adminWithoutCSRF.request(endpoint.method, endpoint.path, nil, nil)
				if response.status != http.StatusForbidden {
					t.Errorf("session without CSRF status = %d, want 403", response.status)
				}
			}
		})
	}
}

type fakeModelStore struct {
	updateErr         error
	createdCredential string
	stored            *store.Model
	getModelErr       error
	endpointErr       error
	updated           *store.Model
	updateCredential  *string
	paramsUpdateErr   error
	paramsCalls       int
	paramsPatch       store.ModelParamsPatch
	paramsResult      *store.Model
}

func (f *fakeModelStore) ListModels(context.Context) ([]store.Model, error) { return nil, nil }
func (f *fakeModelStore) GetModel(_ context.Context, id string) (*store.Model, error) {
	if f.getModelErr != nil {
		return nil, f.getModelErr
	}
	if f.stored == nil || f.stored.ID != id {
		return nil, store.ErrNotFound
	}
	return f.stored, nil
}
func (f *fakeModelStore) GetModelEndpoint(_ context.Context, id string) (store.ModelEndpoint, error) {
	if f.endpointErr != nil {
		return store.ModelEndpoint{}, f.endpointErr
	}
	if f.stored == nil || f.stored.ID != id || f.stored.DeletedAt != nil {
		return store.ModelEndpoint{}, store.ErrNotFound
	}
	return store.ModelEndpoint{
		Protocol: f.stored.Protocol, BaseURL: f.stored.BaseURL, HasAPIKey: f.stored.HasAPIKey,
	}, nil
}
func (f *fakeModelStore) CreateModel(_ context.Context, m store.Model, credential string) (*store.Model, error) {
	f.createdCredential = credential
	m.ID = "user:new"
	m.Revision = 1
	return &m, nil
}
func (f *fakeModelStore) UpdateModel(_ context.Context, m store.Model, _ int64, credential *string) (*store.Model, error) {
	f.updated = &m
	f.updateCredential = credential
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	m.Revision++
	return &m, nil
}
func (f *fakeModelStore) UpdateModelParams(_ context.Context, id string, revision int64, patch store.ModelParamsPatch, updatedBy int64) (*store.Model, error) {
	f.paramsCalls++
	f.paramsPatch = patch
	if f.paramsUpdateErr != nil {
		return nil, f.paramsUpdateErr
	}
	if f.stored == nil || f.stored.ID != id {
		return nil, store.ErrNotFound
	}
	updated := *f.stored
	updated.Params = patch.Apply(updated.Params)
	updated.Revision = revision + 1
	updated.UpdatedBy = updatedBy
	f.paramsResult = &updated
	return &updated, nil
}
func (f *fakeModelStore) SoftDeleteModel(context.Context, string, int64, int64) error {
	return f.updateErr
}
func (f *fakeModelStore) OpenModelCredential(context.Context, string) ([]byte, error) {
	return nil, nil
}

func newModelTestServer(t *testing.T, models *llm.Catalog, modelsStore store.ModelStore) (*Server, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	return &Server{
		models: models, modelsStore: modelsStore,
		logger: slog.New(slog.NewJSONHandler(&logs, nil)), now: time.Now,
	}, &logs
}

func TestModelWriteEndpointsRejectConfiguredModels(t *testing.T) {
	catalog, err := llm.NewCatalog(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, _ := newModelTestServer(t, catalog, &fakeModelStore{})
	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		request := httptest.NewRequest(method, "/api/v1/models/config:primary?revision=1", strings.NewReader(`{"revision":1}`))
		request.SetPathValue("id", "config:primary")
		response := httptest.NewRecorder()
		if method == http.MethodPatch {
			server.handleUpdateModel(response, request)
		} else {
			server.handleDeleteModel(response, request)
		}
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d, want 403", method, response.Code)
		}
	}
}

func TestModelUpdateMapsStaleRevision(t *testing.T) {
	catalog, err := llm.NewCatalog(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, _ := newModelTestServer(t, catalog, &fakeModelStore{
		updateErr: store.ErrStaleRevision,
		stored:    &store.Model{ID: "user:x", Protocol: "openai-chat", BaseURL: "https://example.com"},
	})
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/models/user:x", strings.NewReader(`{"display_name":"x","protocol":"openai-chat","base_url":"https://example.com","model":"m","params":{"max_tokens":16,"max_concurrency":1},"revision":2}`))
	request.SetPathValue("id", "user:x")
	response := httptest.NewRecorder()
	server.handleUpdateModel(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", response.Code)
	}
}

func TestModelCreateDoesNotLeakCredential(t *testing.T) {
	catalog, err := llm.NewCatalog(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeModelStore{}
	server, logs := newModelTestServer(t, catalog, store)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/models", strings.NewReader(`{"display_name":"m","protocol":"openai-chat","base_url":"https://example.com","model":"m","params":{"max_tokens":1,"max_concurrency":1},"api_key":"secret-canary"}`))
	response := httptest.NewRecorder()
	server.handleCreateModel(response, request)
	if store.createdCredential != "secret-canary" {
		t.Fatal("credential was not passed to the store")
	}
	if strings.Contains(response.Body.String(), "secret-canary") || strings.Contains(logs.String(), "secret-canary") {
		t.Fatal("credential leaked")
	}
}

func TestModelWriteEndpointsRejectPrice(t *testing.T) {
	catalog, err := llm.NewCatalog(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeModelStore{}
	server, _ := newModelTestServer(t, catalog, fake)
	create := httptest.NewRequest(http.MethodPost, "/api/v1/models", strings.NewReader(`{"display_name":"m","protocol":"openai-chat","base_url":"https://example.com","model":"m","params":{"max_tokens":1,"max_concurrency":1,"price":{"input_per_mtok":1}},"api_key":"key"}`))
	response := httptest.NewRecorder()
	server.handleCreateModel(response, create)
	if response.Code != http.StatusBadRequest || fake.createdCredential != "" {
		t.Fatalf("create status = %d credential = %q, want 400 and no store call", response.Code, fake.createdCredential)
	}
	update := httptest.NewRequest(http.MethodPatch, "/api/v1/models/user:x", strings.NewReader(`{"display_name":"m","protocol":"openai-chat","base_url":"https://example.com","model":"m","params":{"max_tokens":1,"max_concurrency":1,"price":null},"revision":1}`))
	update.SetPathValue("id", "user:x")
	response = httptest.NewRecorder()
	server.handleUpdateModel(response, update)
	if response.Code != http.StatusBadRequest || fake.updated != nil {
		t.Fatalf("update status = %d updated = %+v, want 400 and no store call", response.Code, fake.updated)
	}
}

func TestDeleteReferencedModelReturnsReferences(t *testing.T) {
	catalog, err := llm.NewCatalog(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, _ := newModelTestServer(t, catalog, &fakeModelStore{updateErr: store.ErrConflict})
	server.profileOverrides = &fakeProfileOverrides{refs: []store.ProfileModelReference{{Profile: "default", Node: "lanes.review"}}}
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/models/user:x?revision=1", nil)
	request.SetPathValue("id", "user:x")
	response := httptest.NewRecorder()
	server.handleDeleteModel(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "default/lanes.review") {
		t.Fatalf("delete = %d %s", response.Code, response.Body.String())
	}
}
