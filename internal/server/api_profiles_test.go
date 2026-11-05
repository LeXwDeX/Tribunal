package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type fakeProfileOverrides struct {
	body     string
	revision int64
	putErr   error
}

func (f *fakeProfileOverrides) GetProfileOverrides(context.Context, string) (*store.ProfileOverrides, error) {
	return &store.ProfileOverrides{Profile: "default", OverridesJSON: f.body, Revision: f.revision}, nil
}
func (f *fakeProfileOverrides) PutProfileOverrides(_ context.Context, profile, body string, revision, _ int64) (*store.ProfileOverrides, error) {
	if f.putErr != nil {
		return nil, f.putErr
	}
	f.body = body
	f.revision = revision + 1
	return &store.ProfileOverrides{Profile: profile, OverridesJSON: body, Revision: f.revision}, nil
}
func (f *fakeProfileOverrides) DeleteProfileOverrides(context.Context, string, int64) error {
	return f.putErr
}

func newProfileServer(t *testing.T, overrides *fakeProfileOverrides) *Server {
	t.Helper()
	return &Server{
		platform:         &config.Config{Profiles: map[string]config.ProfileConfig{"default": {Provider: "config-model"}}},
		profileOverrides: overrides,
		logger:           slog.New(slog.NewTextHandler(&strings.Builder{}, nil)),
		now:              time.Now,
	}
}

func TestProfileOverridesReadAndWrite(t *testing.T) {
	store := &fakeProfileOverrides{body: `{}`}
	server := newProfileServer(t, store)
	get := httptest.NewRecorder()
	server.handleGetDefaultProfile(get, httptest.NewRequest(http.MethodGet, "/api/v1/profiles/default", nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"effective"`) {
		t.Fatalf("get = %d %s", get.Code, get.Body.String())
	}
	put := httptest.NewRecorder()
	server.handlePutProfileOverrides(put, httptest.NewRequest(http.MethodPut, "/api/v1/profiles/default/overrides", strings.NewReader(`{"overrides":{"default":{"model":"config-other"}},"revision":1}`)))
	if put.Code != http.StatusOK || !strings.Contains(put.Body.String(), `"revision":2`) {
		t.Fatalf("put = %d %s", put.Code, put.Body.String())
	}
	del := httptest.NewRecorder()
	server.handleDeleteProfileOverrides(del, httptest.NewRequest(http.MethodDelete, "/api/v1/profiles/default/overrides?revision=2", nil))
	if del.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", del.Code, del.Body.String())
	}
}

func TestProfileOverridesMapErrors(t *testing.T) {
	server := newProfileServer(t, &fakeProfileOverrides{putErr: store.ErrStaleRevision})
	response := httptest.NewRecorder()
	server.handlePutProfileOverrides(response, httptest.NewRequest(http.MethodPut, "/api/v1/profiles/default/overrides", strings.NewReader(`{"overrides":{},"revision":1}`)))
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", response.Code)
	}
}
