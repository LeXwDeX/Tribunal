package server

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

// A platform administrator can dispatch project work without a grant.
func TestPlatformAdminDispatchesWithoutGrant(t *testing.T) {
	env := newWiringEnv(t)
	client := env.loginAs("code-admin")
	got := client.json(http.MethodPost, "/api/v1/projects/42/workspace/refresh", nil)
	if got.status != http.StatusAccepted {
		t.Fatalf("admin dispatch = %d %s", got.status, got.body)
	}
}

// A signed-in user cannot claim a project their token can only read: the
// registrant must be a Reporter or higher. Platform administrators and the
// static bearer skip the check because they already manage every project.
func TestRegistrationRequiresReporterMembership(t *testing.T) {
	newEnv := func(memberAccess int, memberErr error) *m6Env {
		t.Helper()
		api := defaultStubGitLab()
		api.memberAccess = memberAccess
		api.memberErr = memberErr
		return newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
			return testProjectService(t, st, api, t.TempDir())
		}})
	}
	register := func(client *m6Client) apiResponse {
		t.Helper()
		return client.json(http.MethodPost, "/api/v1/projects", map[string]any{
			"project": "42", "token": testProjectTok,
		})
	}
	requireNotStored := func(t *testing.T, env *m6Env) {
		t.Helper()
		if _, err := env.sqlite.GetProject(context.Background(), 42); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("rejected registration stored the project: %v", err)
		}
		if _, err := env.sqlite.GetProjectAccess(context.Background(), 8, 42); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("rejected registration granted access: %v", err)
		}
	}

	t.Run("viewer below reporter", func(t *testing.T) {
		env := newEnv(10, nil)
		denied := register(env.loginAs("code-viewer"))
		if denied.status != http.StatusBadRequest {
			t.Fatalf("non-member register = %d %s", denied.status, denied.body)
		}
		requireNotStored(t, env)
	})

	t.Run("viewer not a member", func(t *testing.T) {
		env := newEnv(0, &gitlab.Error{Op: "get_member", Status: http.StatusNotFound})
		denied := register(env.loginAs("code-viewer"))
		if denied.status != http.StatusBadRequest {
			t.Fatalf("missing member register = %d %s", denied.status, denied.body)
		}
		requireNotStored(t, env)
	})

	t.Run("static bearer skips membership", func(t *testing.T) {
		env := newEnv(10, nil)
		created := register(env.bearerClient())
		if created.status != http.StatusCreated {
			t.Fatalf("bearer register = %d %s", created.status, created.body)
		}
		grants, _, err := env.sqlite.ListProjectGrants(context.Background(), 42, "", 10)
		if err != nil || len(grants) != 0 {
			t.Fatalf("bearer registration wrote grants: %+v, %v", grants, err)
		}
	})

	t.Run("admin skips membership", func(t *testing.T) {
		env := newEnv(10, nil)
		created := register(env.loginAs("code-admin"))
		if created.status != http.StatusCreated {
			t.Fatalf("admin register without membership = %d %s", created.status, created.body)
		}
	})
}

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
