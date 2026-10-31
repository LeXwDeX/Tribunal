package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRepositorySkillHTTPRequiresApprovedSnapshotHash(t *testing.T) {
	sha := strings.Repeat("a", 40)
	hash := strings.Repeat("b", 64)
	for _, tc := range []struct {
		name      string
		targetSHA string
		snapshot  string
		want      int
	}{
		{"valid", sha, hash, http.StatusOK},
		{"missing", sha, "", http.StatusBadRequest},
		{"short", sha, strings.Repeat("b", 63), http.StatusBadRequest},
		{"uppercase", sha, strings.Repeat("B", 64), http.StatusBadRequest},
		{"nonhex", sha, strings.Repeat("g", 64), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &repositorySkillHTTPFake{}
			s := &Server{workspace: backend, access: paginationAccess{}, logger: newTestLogger(io.Discard)}
			body := `{"expected_revision":"absent","skills":["repo:valid"],"target_sha":"` + tc.targetSHA + `","snapshot_sha256":"` + tc.snapshot + `"}`
			rec := httptest.NewRecorder()
			req := repositorySkillRequest(http.MethodPut, "/api/v1/admin/projects/42/repository-skill-authorizations?target_branch=main&block=review", body, roleAdmin)
			s.handleReplaceRepositorySkillAuthorization(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d, want %d, body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusOK {
				if backend.calls != 1 || backend.targetSHA != tc.targetSHA || backend.snapshotSHA256 != tc.snapshot {
					t.Fatalf("service calls=%d target=%q snapshot=%q", backend.calls, backend.targetSHA, backend.snapshotSHA256)
				}
			}
		})
	}
	// Revocation must remain possible without a stale or unknown snapshot hash.
	backend := &repositorySkillHTTPFake{}
	s := &Server{workspace: backend, access: paginationAccess{}, logger: newTestLogger(io.Discard)}
	rec := httptest.NewRecorder()
	req := repositorySkillRequest(http.MethodPut, "/api/v1/admin/projects/42/repository-skill-authorizations?target_branch=main&block=review", `{"expected_revision":"absent","skills":[]}`, roleAdmin)
	s.handleReplaceRepositorySkillAuthorization(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("revocation status=%d, body=%s", rec.Code, rec.Body.String())
	}
}
