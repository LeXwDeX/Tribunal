package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

// A platform administrator manages every registered project without a
// per-project grant. The project must still exist, so a deleted project keeps
// returning 404.
func TestPlatformAdminManagesEveryProjectWithoutGrant(t *testing.T) {
	env, _ := isolationEnv(t)
	admin := env.loginAs("code-admin") // GitLab user 7

	list := admin.request(http.MethodGet, "/api/v1/projects", nil, nil)
	assertStatus(t, list, http.StatusOK)
	projects, _ := env.decode(t, list)["projects"].([]any)
	if len(projects) != 2 {
		t.Fatalf("admin projects = %v, want both registered projects", projects)
	}
	for _, raw := range projects {
		item, _ := raw.(map[string]any)
		if item["access"] != store.ProjectAccessManage || item["access_source"] != "admin" {
			t.Fatalf("admin project = %v, want manage from admin", item)
		}
	}

	if got := admin.request(http.MethodGet, "/api/v1/projects/43", nil, nil); got.status != http.StatusOK {
		t.Fatalf("admin project detail = %d %s", got.status, got.body)
	}
	if got := admin.json(http.MethodDelete, "/api/v1/projects/43", nil); got.status != http.StatusNoContent {
		t.Fatalf("admin delete = %d %s", got.status, got.body)
	}
	if got := admin.request(http.MethodGet, "/api/v1/projects/43", nil, nil); got.status != http.StatusNotFound {
		t.Fatalf("deleted project detail = %d %s", got.status, got.body)
	}
}

// Any signed-in user may register a project and immediately owns it.
func TestViewerRegistersAndDeletesOwnProject(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	viewer := env.loginAs("code-viewer") // GitLab user 8

	created := viewer.json(http.MethodPost, "/api/v1/projects", map[string]any{
		"project": "42", "token": testProjectTok,
	})
	if created.status != http.StatusCreated {
		t.Fatalf("viewer register = %d %s", created.status, created.body)
	}
	role, err := env.sqlite.GetProjectAccess(context.Background(), 8, 42)
	if err != nil || role != store.ProjectAccessManage {
		t.Fatalf("registrant grant = %q, %v; want manage", role, err)
	}

	list := viewer.request(http.MethodGet, "/api/v1/projects", nil, nil)
	assertStatus(t, list, http.StatusOK)
	items, _ := env.decode(t, list)["projects"].([]any)
	if len(items) != 1 {
		t.Fatalf("registrant projects = %v", items)
	}
	if item, _ := items[0].(map[string]any); item["access"] != store.ProjectAccessManage {
		t.Fatalf("registrant project = %v", item)
	}

	if got := viewer.json(http.MethodDelete, "/api/v1/projects/42", nil); got.status != http.StatusNoContent {
		t.Fatalf("registrant delete = %d %s", got.status, got.body)
	}
	if got := viewer.request(http.MethodGet, "/api/v1/projects/42", nil, nil); got.status != http.StatusNotFound {
		t.Fatalf("deleted project = %d %s", got.status, got.body)
	}
}

// Assignment writes manage, which lets the assignee delete the project;
// read-only assignment cannot delete.
func TestAssignedManageCanDeleteButReadCannot(t *testing.T) {
	env, _ := isolationEnv(t)
	env.grant(8, 42, store.ProjectAccessManage)
	env.grant(8, 43, store.ProjectAccessRead)
	bob := env.loginAs("code-viewer")

	if got := bob.json(http.MethodDelete, "/api/v1/projects/43", nil); got.status != http.StatusForbidden {
		t.Fatalf("read delete = %d %s", got.status, got.body)
	}
	if got := bob.json(http.MethodDelete, "/api/v1/projects/42", nil); got.status != http.StatusNoContent {
		t.Fatalf("manage delete = %d %s", got.status, got.body)
	}
}
