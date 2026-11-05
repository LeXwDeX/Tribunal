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
		{"detail", http.MethodGet, "/api/v1/models/user:missing"},
		{"create", http.MethodPost, "/api/v1/models"},
		{"update", http.MethodPatch, "/api/v1/models/user:missing"},
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
}

func (f *fakeModelStore) ListModels(context.Context) ([]store.Model, error) { return nil, nil }
func (f *fakeModelStore) GetModel(context.Context, string) (*store.Model, error) {
	return nil, store.ErrNotFound
}
func (f *fakeModelStore) CreateModel(_ context.Context, m store.Model, credential string) (*store.Model, error) {
	f.createdCredential = credential
	m.ID = "user:new"
	m.Revision = 1
	return &m, nil
}
func (f *fakeModelStore) UpdateModel(context.Context, store.Model, int64, *string) (*store.Model, error) {
	return nil, f.updateErr
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
	server, _ := newModelTestServer(t, catalog, &fakeModelStore{updateErr: store.ErrStaleRevision})
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/models/user:x", strings.NewReader(`{"revision":2}`))
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
