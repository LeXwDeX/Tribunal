package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

// A platform admin who registers a project through a browser session must see
// it immediately; before #277 the project was stored without a grant, so the
// list was empty and a retry reported a conflict.
func TestRegisterProjectBySessionGrantsRegistrantManage(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	admin := env.loginAs("code-admin") // GitLab user 7

	created := admin.json(http.MethodPost, "/api/v1/projects", map[string]any{
		"project": "42", "token": testProjectTok,
	})
	if created.status != http.StatusCreated {
		t.Fatalf("register = %d %s", created.status, created.body)
	}
	role, err := env.sqlite.GetProjectAccess(context.Background(), 7, 42)
	if err != nil || role != store.ProjectAccessManage {
		t.Fatalf("registrant grant = %q, %v; want manage", role, err)
	}
	list := admin.request(http.MethodGet, "/api/v1/projects", nil, nil)
	if list.status != http.StatusOK {
		t.Fatalf("list = %d %s", list.status, list.body)
	}
	projects, _ := env.decode(t, list)["projects"].([]any)
	if len(projects) != 1 {
		t.Fatalf("listed projects = %v, want the registered project", projects)
	}
	item, _ := projects[0].(map[string]any)
	if item["id"] != float64(42) || item["access"] != store.ProjectAccessManage {
		t.Fatalf("listed project = %v", item)
	}
}

func TestRegisterProjectByBearerWritesNoGrant(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	created := env.bearerClient().json(http.MethodPost, "/api/v1/projects", map[string]any{
		"project": "42", "token": testProjectTok,
	})
	if created.status != http.StatusCreated {
		t.Fatalf("register = %d %s", created.status, created.body)
	}
	grants, _, err := env.sqlite.ListProjectGrants(context.Background(), 42, "", 10)
	if err != nil {
		t.Fatalf("ListProjectGrants: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("bearer registration wrote grants: %+v", grants)
	}
}

// A duplicate registration tells the platform admin where to grant access.
// The route is admin-only, so non-admins never see whether the project exists.
func TestRegisterProjectConflictHintIsAdminOnly(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	body := map[string]any{"project": "42", "token": testProjectTok}
	if got := env.bearerClient().json(http.MethodPost, "/api/v1/projects", body); got.status != http.StatusCreated {
		t.Fatalf("register = %d %s", got.status, got.body)
	}
	admin := env.loginAs("code-admin")
	duplicate := admin.json(http.MethodPost, "/api/v1/projects", body)
	if duplicate.status != http.StatusConflict {
		t.Fatalf("duplicate = %d %s", duplicate.status, duplicate.body)
	}
	if !strings.Contains(string(duplicate.body), "System page") {
		t.Fatalf("duplicate message lacks the grant hint: %s", duplicate.body)
	}
	if _, err := env.sqlite.GetProjectAccess(context.Background(), 7, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("duplicate registration granted access: %v", err)
	}
	viewer := env.loginAs("code-viewer")
	denied := viewer.json(http.MethodPost, "/api/v1/projects", body)
	if denied.status != http.StatusForbidden || strings.Contains(string(denied.body), "registered") {
		t.Fatalf("viewer register = %d %s", denied.status, denied.body)
	}
}
