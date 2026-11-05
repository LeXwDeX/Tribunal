package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type fakeProfileOverrides struct {
	body     string
	revision int64
	putErr   error
	refs     []store.ProfileModelReference
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
func (f *fakeProfileOverrides) DeleteProfileOverrides(_ context.Context, profile string, revision, userID int64) (*store.ProfileOverrides, error) {
	if f.putErr != nil {
		return nil, f.putErr
	}
	f.body = `{}`
	f.revision = revision + 1
	return &store.ProfileOverrides{Profile: profile, OverridesJSON: `{}`, Revision: f.revision, UpdatedBy: userID}, nil
}
func (f *fakeProfileOverrides) ListProfileModelReferences(context.Context, string) ([]store.ProfileModelReference, error) {
	return f.refs, nil
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

func TestProfileOverridesFirstWriteUsesRevisionZero(t *testing.T) {
	store := &fakeProfileOverrides{body: `{}`}
	server := newProfileServer(t, store)
	response := httptest.NewRecorder()
	server.handlePutProfileOverrides(response, httptest.NewRequest(http.MethodPut, "/api/v1/profiles/default/overrides", strings.NewReader(`{"overrides":{"default":{"model":"config-other"}},"revision":0}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
}

func profileCatalog(t *testing.T) *llm.Catalog {
	t.Helper()
	t.Setenv("PROFILE_TEST_KEY", "k")
	provider := func(model string) config.ProviderConfig {
		return config.ProviderConfig{Protocol: "openai-chat", BaseURL: "https://93.184.216.34/v1", Model: model,
			APIKeyEnv: "PROFILE_TEST_KEY", MaxTokens: 1000, MaxConcurrency: 1}
	}
	catalog, err := llm.NewCatalog(context.Background(), map[string]config.ProviderConfig{
		"alpha": provider("m1"), "beta": provider("m1"), "gamma": provider("m2"),
	}, nil)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	return catalog
}

func rawProfileServer(t *testing.T, overrides *fakeProfileOverrides) *Server {
	t.Helper()
	server := newProfileServer(t, overrides)
	raw := config.ProfileConfig{Provider: "alpha", Lanes: &config.LanesConfig{Review: config.LaneConfig{Provider: "gamma"}}}
	normalized, _ := config.NormalizeReviewProfile(raw)
	server.platform = &config.Config{
		Profiles:    map[string]config.ProfileConfig{"default": normalized},
		RawProfiles: map[string]config.ProfileConfig{"default": raw},
	}
	server.models = profileCatalog(t)
	return server
}

type nodeJSON struct {
	Node             string `json:"node"`
	Model            string `json:"model"`
	Source           string `json:"source"`
	Configured       string `json:"configured"`
	ConfiguredSource string `json:"configured_source"`
	Override         *struct {
		Model string `json:"model"`
	} `json:"override"`
	ParamSource     string `json:"param_source"`
	VoteIdentity    string `json:"vote_identity"`
	EffectiveParams *struct {
		MaxTokens   int      `json:"max_tokens"`
		Temperature *float64 `json:"temperature"`
	} `json:"effective_params"`
}

func decodeNodes(t *testing.T, body []byte) map[string]nodeJSON {
	t.Helper()
	var view struct {
		Nodes []nodeJSON `json:"nodes"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	nodes := map[string]nodeJSON{}
	for _, node := range view.Nodes {
		nodes[node.Node] = node
	}
	return nodes
}

func TestProfileDetailReturnsConfiguredOverrideEffectiveSourceAndVoteIdentity(t *testing.T) {
	overrides := &fakeProfileOverrides{revision: 3, body: `{"explore":{"model":"config:beta","params":{"max_tokens":321,"temperature":0.4}},"lanes":{"validate":{"params":{"max_tokens":99}}}}`}
	server := rawProfileServer(t, overrides)
	response := httptest.NewRecorder()
	server.handleGetDefaultProfile(response, httptest.NewRequest(http.MethodGet, "/api/v1/profiles/default", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	nodes := decodeNodes(t, response.Body.Bytes())
	explore := nodes["explore"]
	if explore.Configured != "config:alpha" || explore.ConfiguredSource != "config_default" ||
		explore.Model != "config:beta" || explore.Source != "override" ||
		explore.Override == nil || explore.Override.Model != "config:beta" || explore.ParamSource != "override" {
		t.Fatalf("explore = %+v", explore)
	}
	if explore.EffectiveParams == nil || explore.EffectiveParams.MaxTokens != 321 || explore.EffectiveParams.Temperature == nil || *explore.EffectiveParams.Temperature != 0.4 {
		t.Fatalf("explore effective params = %+v", explore.EffectiveParams)
	}
	// alpha and beta share endpoint and model, so they vote as one model;
	// gamma differs.
	if nodes["default"].VoteIdentity == "" || nodes["explore"].VoteIdentity != nodes["default"].VoteIdentity {
		t.Fatalf("vote identities: default %q explore %q", nodes["default"].VoteIdentity, nodes["explore"].VoteIdentity)
	}
	review := nodes["lanes.review"]
	if review.Model != "config:gamma" || review.Source != "config" || review.VoteIdentity == nodes["default"].VoteIdentity {
		t.Fatalf("review = %+v", review)
	}
	validate := nodes["lanes.validate"]
	if validate.Source != "config_default" || validate.EffectiveParams == nil || validate.EffectiveParams.MaxTokens != 99 {
		t.Fatalf("validate = %+v", validate)
	}
}

// The list the review page filters on shows the same effective profile as the
// detail.
func TestProfileListMatchesDetailEffectiveValues(t *testing.T) {
	overrides := &fakeProfileOverrides{revision: 1, body: `{"default":{"model":"config:beta"},"lanes":{"code_review":{"model":"config:gamma","params":{"max_tokens":50}}}}`}
	server := rawProfileServer(t, overrides)
	detail := httptest.NewRecorder()
	server.handleGetDefaultProfile(detail, httptest.NewRequest(http.MethodGet, "/api/v1/profiles/default", nil))
	list := httptest.NewRecorder()
	server.handleListProfiles(list, httptest.NewRequest(http.MethodGet, "/api/v1/profiles", nil))
	var detailView struct {
		Effective config.ProfileConfig `json:"effective"`
	}
	var listView struct {
		Profiles []struct {
			Name   string               `json:"name"`
			Config config.ProfileConfig `json:"config"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &detailView); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listView); err != nil || len(listView.Profiles) != 1 {
		t.Fatalf("list = %s (%v)", list.Body.String(), err)
	}
	got := listView.Profiles[0].Config
	if !reflect.DeepEqual(got, detailView.Effective) {
		t.Fatalf("list config %+v != detail effective %+v", got, detailView.Effective)
	}
	if got.Propose[0].Provider != "gamma" || got.Explore.Provider != "beta" || *got.NodeParams[config.NodeCodeReview].MaxTokens != 50 {
		t.Fatalf("list does not show the overrides: %+v", got)
	}
}

func TestProfileOverridesPutValidatesModelsAndParams(t *testing.T) {
	server := rawProfileServer(t, &fakeProfileOverrides{body: `{}`})
	put := func(body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		server.handlePutProfileOverrides(response, httptest.NewRequest(http.MethodPut, "/api/v1/profiles/default/overrides", strings.NewReader(body)))
		return response
	}
	response := put(`{"overrides":{"lanes":{"validate":{"model":"user:deleted"}}},"revision":0}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"field":"overrides.lanes.validate.model"`) {
		t.Fatalf("unknown model = %d %s", response.Code, response.Body.String())
	}
	response = put(`{"overrides":{"explore":{"params":{"temperature":9,"reasoning":{"effort":"extreme"}}}},"revision":0}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `overrides.explore.params`) ||
		!strings.Contains(response.Body.String(), "temperature") || !strings.Contains(response.Body.String(), "effort") {
		t.Fatalf("bad params = %d %s", response.Code, response.Body.String())
	}
	response = put(`{"overrides":{"explore":{"params":{"context_window":1}}},"revision":0}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("non-whitelisted param = %d %s", response.Code, response.Body.String())
	}
	response = put(`{"overrides":{"explore":{"model":"config:beta","params":{"max_tokens":10}}},"revision":0}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"vote_identity"`) {
		t.Fatalf("valid put = %d %s", response.Code, response.Body.String())
	}
}

func TestProfileEndpointsRequirePlatformAdminAndCSRF(t *testing.T) {
	env := newM6Env(t, m6Options{profileOverrides: true})
	viewer := env.loginAs("code-viewer")
	adminWithoutCSRF := env.loginAs("code-admin").withoutCSRF()
	body := []byte(`{"overrides":{},"revision":0}`)
	for _, endpoint := range []struct{ name, method, path string }{
		{"detail", http.MethodGet, "/api/v1/profiles/default"},
		{"list", http.MethodGet, "/api/v1/profiles"},
		{"put", http.MethodPut, "/api/v1/profiles/default/overrides"},
		{"delete", http.MethodDelete, "/api/v1/profiles/default/overrides?revision=1"},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			if response := viewer.request(endpoint.method, endpoint.path, body, nil); response.status != http.StatusForbidden {
				t.Errorf("non-admin status = %d, want 403", response.status)
			}
			if !safeMethod(endpoint.method) {
				if response := adminWithoutCSRF.request(endpoint.method, endpoint.path, body, nil); response.status != http.StatusForbidden {
					t.Errorf("session without CSRF status = %d, want 403", response.status)
				}
			}
		})
	}
}

// Against the real store: an unknown user model is a 400 with the field path,
// a stale revision is a 409.
func TestProfileOverridesHTTPUnknownModelAndRevisionConflict(t *testing.T) {
	catalog, err := llm.NewCatalog(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	platform := &config.Config{Profiles: map[string]config.ProfileConfig{"default": {Provider: "config-model"}}}
	env := newM6Env(t, m6Options{profileOverrides: true, models: catalog, platform: platform})
	admin := env.loginAs("code-admin")
	response := admin.json(http.MethodPut, "/api/v1/profiles/default/overrides", map[string]any{
		"overrides": map[string]any{"default": map[string]any{"model": "user:missing"}}, "revision": 0})
	assertStatus(t, response, http.StatusBadRequest)
	if !strings.Contains(string(response.body), "overrides.default.model") {
		t.Fatalf("400 lacks the field path: %s", response.body)
	}
	response = admin.json(http.MethodPut, "/api/v1/profiles/default/overrides", map[string]any{
		"overrides": map[string]any{"default": map[string]any{"params": map[string]any{"max_tokens": 5}}}, "revision": 0})
	assertStatus(t, response, http.StatusOK)
	response = admin.json(http.MethodPut, "/api/v1/profiles/default/overrides", map[string]any{
		"overrides": map[string]any{}, "revision": 0})
	assertStatus(t, response, http.StatusConflict)
}

// DELETE needs the revision the admin saw: a stale one is a 409 that leaves
// another admin's changes alone, and a good one records the acting user and
// returns the stored revision.
func TestProfileOverridesDeleteChecksRevisionAndRecordsActor(t *testing.T) {
	platform := &config.Config{Profiles: map[string]config.ProfileConfig{"default": {Provider: "config-model"}}}
	env := newM6Env(t, m6Options{profileOverrides: true, platform: platform})
	admin := env.loginAs("code-admin")
	for revision := 0; revision < 2; revision++ {
		assertStatus(t, admin.json(http.MethodPut, "/api/v1/profiles/default/overrides", map[string]any{
			"overrides": map[string]any{"default": map[string]any{"params": map[string]any{"max_tokens": 5 + revision}}}, "revision": revision}), http.StatusOK)
	}
	saved, err := env.sqlite.GetProfileOverrides(context.Background(), "default")
	if err != nil || saved.Revision != 2 || saved.UpdatedBy == 0 {
		t.Fatalf("saved = %+v (%v)", saved, err)
	}
	assertStatus(t, admin.request(http.MethodDelete, "/api/v1/profiles/default/overrides?revision=1", nil, nil), http.StatusConflict)
	if after, _ := env.sqlite.GetProfileOverrides(context.Background(), "default"); after.Revision != 2 || after.OverridesJSON == `{}` {
		t.Fatalf("a stale delete changed the document: %+v", after)
	}
	response := admin.request(http.MethodDelete, "/api/v1/profiles/default/overrides?revision=2", nil, nil)
	assertStatus(t, response, http.StatusOK)
	after, _ := env.sqlite.GetProfileOverrides(context.Background(), "default")
	var view struct {
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(response.body, &view); err != nil {
		t.Fatal(err)
	}
	if after.Revision != 3 || view.Revision != after.Revision || after.UpdatedBy != saved.UpdatedBy || after.OverridesJSON != `{}` {
		t.Fatalf("after delete: %+v, response revision %d", after, view.Revision)
	}
}

func TestProfileOverridesPutRejectsUnavailableAndWhitespaceModels(t *testing.T) {
	server := rawProfileServer(t, &fakeProfileOverrides{body: `{}`})
	put := func(body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		server.handlePutProfileOverrides(response, httptest.NewRequest(http.MethodPut, "/api/v1/profiles/default/overrides", strings.NewReader(body)))
		return response
	}
	if r := put(`{"overrides":{"explore":{"model":" config:beta"}},"revision":0}`); r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "overrides.explore.model") {
		t.Fatalf("whitespace id = %d %s", r.Code, r.Body.String())
	}
	if r := put(`{"overrides":{"explore":{"params":{"thinking":{"budget_tokens":0}}}},"revision":0}`); r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "omit the field to inherit") {
		t.Fatalf("no-op param = %d %s", r.Code, r.Body.String())
	}
}

// PUT, GET, DELETE, GET over real handlers: after the reset the configured
// values are back and the platform profiles were never changed.
func TestProfileOverridesResetRestoresConfiguredValues(t *testing.T) {
	raw := config.ProfileConfig{Provider: "alpha", Lanes: &config.LanesConfig{Reasoner: config.LaneConfig{Provider: "gamma"}}}
	normalized, _ := config.NormalizeReviewProfile(raw)
	platform := &config.Config{
		Profiles:    map[string]config.ProfileConfig{"default": normalized},
		RawProfiles: map[string]config.ProfileConfig{"default": raw},
	}
	before, _ := json.Marshal(platform.Profiles)
	beforeRaw, _ := json.Marshal(platform.RawProfiles)
	env := newM6Env(t, m6Options{profileOverrides: true, platform: platform, models: profileCatalog(t)})
	admin := env.loginAs("code-admin")
	reasoner := func() nodeJSON {
		response := admin.request(http.MethodGet, "/api/v1/profiles/default", nil, nil)
		assertStatus(t, response, http.StatusOK)
		return decodeNodes(t, response.body)["lanes.reasoner"]
	}
	if n := reasoner(); n.Model != "config:gamma" || n.Source != "config" {
		t.Fatalf("initial reasoner = %+v", n)
	}
	assertStatus(t, admin.json(http.MethodPut, "/api/v1/profiles/default/overrides", map[string]any{
		"overrides": map[string]any{"lanes": map[string]any{"reasoner": map[string]any{"model": "config:beta"}}}, "revision": 0}), http.StatusOK)
	if n := reasoner(); n.Model != "config:beta" || n.Source != "override" {
		t.Fatalf("overridden reasoner = %+v", n)
	}
	assertStatus(t, admin.request(http.MethodDelete, "/api/v1/profiles/default/overrides?revision=1", nil, nil), http.StatusOK)
	if n := reasoner(); n.Model != "config:gamma" || n.Source != "config" || n.Override != nil {
		t.Fatalf("reasoner after reset = %+v", n)
	}
	after, _ := json.Marshal(platform.Profiles)
	afterRaw, _ := json.Marshal(platform.RawProfiles)
	if string(after) != string(before) || string(afterRaw) != string(beforeRaw) {
		t.Fatal("platform profiles were mutated")
	}
}
