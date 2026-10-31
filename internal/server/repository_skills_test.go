package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type repositorySkillHTTPFake struct {
	WorkspaceService
	calls          int
	err            error
	targetSHA      string
	snapshotSHA256 string
}

func (f *repositorySkillHTTPFake) DiscoverRepositorySkills(_ context.Context, id int64, branch string) (*app.RepositorySkillDiscovery, error) {
	f.calls++
	return &app.RepositorySkillDiscovery{ProjectID: id, TargetBranch: branch, Enabled: true, Skills: []app.RepositorySkillMetadata{}}, f.err
}

func (f *repositorySkillHTTPFake) RepositorySkillAuthorization(_ context.Context, id int64, branch, block string) (*store.RepositorySkillAuthorization, error) {
	f.calls++
	return &store.RepositorySkillAuthorization{ProjectID: id, TargetBranch: branch, Block: block, Skills: []string{}, Revision: store.BlockBindingAbsentRevision}, f.err
}

func (f *repositorySkillHTTPFake) ReplaceRepositorySkillAuthorization(_ context.Context, grant store.RepositorySkillAuthorization, _, targetSHA, snapshotSHA256 string, _ int64) (*store.RepositorySkillAuthorization, error) {
	f.calls++
	f.targetSHA = targetSHA
	f.snapshotSHA256 = snapshotSHA256
	grant.TargetSHA = targetSHA
	grant.SnapshotSHA256 = snapshotSHA256
	return &grant, f.err
}

type repositorySkillHTTPAccess struct {
	paginationAccess
	role string
	err  error
}

func (a repositorySkillHTTPAccess) GetProjectAccess(context.Context, int64, int64) (string, error) {
	return a.role, a.err
}

func repositorySkillRequest(method, url, body, platformRole string) *http.Request {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.SetPathValue("id", "42")
	req = req.WithContext(context.WithValue(req.Context(), principalKey, principal{user: sessionUser{ID: 7, Role: platformRole}}))
	return req
}

func TestRepositorySkillRoutesSuccessAuthAndConflict(t *testing.T) {
	env := newWiringEnv(t)
	backend := &repositorySkillHTTPFake{}
	env.service.workspace = backend
	admin := env.business()

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/admin/projects/42/repository-skills?target_branch=main", ""},
		{http.MethodGet, "/api/v1/admin/projects/42/repository-skill-authorizations?target_branch=main&block=review", ""},
		{http.MethodPut, "/api/v1/admin/projects/42/repository-skill-authorizations?target_branch=main&block=review", `{"expected_revision":"absent","skills":[]}`},
	} {
		response := admin.request(tc.method, tc.path, []byte(tc.body), map[string]string{"Content-Type": "application/json"})
		if response.status != http.StatusOK {
			t.Errorf("%s %s: status=%d body=%s", tc.method, tc.path, response.status, response.body)
		}
	}
	if backend.calls != 3 {
		t.Fatalf("successful route calls = %d, want 3", backend.calls)
	}

	backend.err = &app.Error{Code: "conflict", Message: "authorization changed"}
	conflict := admin.request(http.MethodPut, "/api/v1/admin/projects/42/repository-skill-authorizations?target_branch=main&block=review",
		[]byte(`{"expected_revision":"absent","skills":[]}`), map[string]string{"Content-Type": "application/json"})
	if conflict.status != http.StatusConflict {
		t.Fatalf("conflict status = %d body=%s", conflict.status, conflict.body)
	}
	backend.err = nil

	anonymous := &m6Client{t: t, base: env.server.URL, client: &http.Client{}}
	if response := anonymous.request(http.MethodGet, "/api/v1/admin/projects/42/repository-skills?target_branch=main", nil, nil); response.status != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", response.status)
	}
	viewer := env.loginAs("code-viewer")
	backend.calls = 0
	if response := viewer.request(http.MethodGet, "/api/v1/admin/projects/42/repository-skills?target_branch=main", nil, nil); response.status != http.StatusForbidden || backend.calls != 0 {
		t.Fatalf("viewer status = %d calls=%d", response.status, backend.calls)
	}
	if err := env.db.DeleteProjectGrant(context.Background(), 42, 7); err != nil {
		t.Fatal(err)
	}
	if response := admin.request(http.MethodGet, "/api/v1/admin/projects/42/repository-skills?target_branch=main", nil, nil); response.status != http.StatusNotFound || backend.calls != 0 {
		t.Fatalf("admin without grant status = %d calls=%d", response.status, backend.calls)
	}
}

func TestRepositorySkillHTTPRequiresPlatformAdminAndProjectManage(t *testing.T) {
	for _, tc := range []struct {
		name, platform, project string
		err                     error
		want                    int
	}{
		{"project manager without admin", "user", store.ProjectAccessManage, nil, http.StatusForbidden},
		{"admin with read", roleAdmin, store.ProjectAccessRead, nil, http.StatusForbidden},
		{"admin without grant", roleAdmin, "", store.ErrNotFound, http.StatusNotFound},
		{"admin with manage", roleAdmin, store.ProjectAccessManage, nil, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &repositorySkillHTTPFake{}
			s := &Server{workspace: backend, access: repositorySkillHTTPAccess{role: tc.project, err: tc.err}, logger: newTestLogger(io.Discard)}
			rec := httptest.NewRecorder()
			req := repositorySkillRequest(http.MethodGet, "/api/v1/admin/projects/42/repository-skills?target_branch=main", "", tc.platform)
			s.handleDiscoverRepositorySkills(rec, req)
			if rec.Code != tc.want || (tc.want != http.StatusOK && backend.calls != 0) {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, backend.calls, rec.Body.String())
			}
		})
	}
}

func TestRepositorySkillHTTPRequiresExplicitConditionalQualifiedGrants(t *testing.T) {
	for _, body := range []string{
		`{"expected_revision":"absent"}`,
		`{"skills":[]}`,
		`{"expected_revision":"absent","skills":["legacy"]}`,
		`{"expected_revision":"absent","skills":["repo:valid"]}`,
	} {
		backend := &repositorySkillHTTPFake{}
		s := &Server{workspace: backend, access: paginationAccess{}, logger: newTestLogger(io.Discard)}
		rec := httptest.NewRecorder()
		req := repositorySkillRequest(http.MethodPut, "/api/v1/admin/projects/42/repository-skill-authorizations?target_branch=main&block=review", body, roleAdmin)
		s.handleReplaceRepositorySkillAuthorization(rec, req)
		if rec.Code != http.StatusBadRequest || backend.calls != 0 {
			t.Errorf("body %s: status=%d calls=%d response=%s", body, rec.Code, backend.calls, rec.Body.String())
		}
	}
}

func TestRepositorySkillAuthorizationHTTPAccessAndRevisionConflict(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		for _, tc := range []struct {
			platform, project string
			want              int
		}{
			{"user", store.ProjectAccessManage, http.StatusForbidden},
			{roleAdmin, store.ProjectAccessRead, http.StatusForbidden},
			{roleAdmin, store.ProjectAccessManage, http.StatusOK},
		} {
			backend := &repositorySkillHTTPFake{}
			s := &Server{workspace: backend, access: repositorySkillHTTPAccess{role: tc.project}, logger: newTestLogger(io.Discard)}
			rec := httptest.NewRecorder()
			req := repositorySkillRequest(method, "/api/v1/admin/projects/42/repository-skill-authorizations?target_branch=main&block=review", `{"expected_revision":"absent","skills":[]}`, tc.platform)
			if method == http.MethodGet {
				s.handleGetRepositorySkillAuthorization(rec, req)
			} else {
				s.handleReplaceRepositorySkillAuthorization(rec, req)
			}
			if rec.Code != tc.want || (tc.want != http.StatusOK && backend.calls != 0) {
				t.Errorf("%s platform=%s project=%s: status=%d calls=%d", method, tc.platform, tc.project, rec.Code, backend.calls)
			}
		}
	}
	backend := &repositorySkillHTTPFake{err: &app.Error{Code: "conflict", Message: "authorization changed"}}
	s := &Server{workspace: backend, access: paginationAccess{}, logger: newTestLogger(io.Discard)}
	rec := httptest.NewRecorder()
	req := repositorySkillRequest(http.MethodPut, "/api/v1/admin/projects/42/repository-skill-authorizations?target_branch=main&block=review", `{"expected_revision":"absent","skills":[]}`, roleAdmin)
	s.handleReplaceRepositorySkillAuthorization(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale revision: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
