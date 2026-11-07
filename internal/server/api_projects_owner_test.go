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
	// The console matches this prefix to show its localized hint.
	message, _ := env.decode(t, duplicate)["error"].(map[string]any)["message"].(string)
	if !strings.HasPrefix(message, "project is already registered") {
		t.Fatalf("duplicate message prefix changed: %q", message)
	}
	if !strings.Contains(string(duplicate.body), "assign it to you") {
		t.Fatalf("duplicate message lacks the assignment hint: %s", duplicate.body)
	}
	if _, err := env.sqlite.GetProjectAccess(context.Background(), 7, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("duplicate registration granted access: %v", err)
	}
	viewer := env.loginAs("code-viewer")
	duplicateAgain := viewer.json(http.MethodPost, "/api/v1/projects", body)
	if duplicateAgain.status != http.StatusConflict || !strings.Contains(string(duplicateAgain.body), "already registered") {
		t.Fatalf("viewer duplicate register = %d %s", duplicateAgain.status, duplicateAgain.body)
	}
}

// Deleting a project and registering it again must leave the new registrant
// with manage access and the project visible, as for a first registration.
func TestReregisterAfterDeleteGrantsRegistrantManage(t *testing.T) {
	for name, firstBySession := range map[string]bool{"first by session": true, "first by bearer": false} {
		t.Run(name, func(t *testing.T) {
			api := defaultStubGitLab()
			env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
				return testProjectService(t, st, api, t.TempDir())
			}})
			ctx := context.Background()
			admin := env.loginAs("code-admin") // GitLab user 7
			body := map[string]any{"project": "42", "token": testProjectTok}
			first := env.bearerClient()
			if firstBySession {
				first = admin
			}
			if got := first.json(http.MethodPost, "/api/v1/projects", body); got.status != http.StatusCreated {
				t.Fatalf("first register = %d %s", got.status, got.body)
			}
			if !firstBySession {
				if err := env.sqlite.SetProjectGrant(ctx, 42, 7, store.ProjectAccessManage); err != nil {
					t.Fatalf("SetProjectGrant: %v", err)
				}
			}
			if got := admin.request(http.MethodDelete, "/api/v1/projects/42", nil, nil); got.status != http.StatusNoContent {
				t.Fatalf("delete = %d %s", got.status, got.body)
			}
			if _, err := env.sqlite.GetProjectAccess(ctx, 7, 42); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("grant survived project deletion: %v", err)
			}

			again := admin.json(http.MethodPost, "/api/v1/projects", body)
			if again.status != http.StatusCreated {
				t.Fatalf("re-register = %d %s", again.status, again.body)
			}
			role, err := env.sqlite.GetProjectAccess(ctx, 7, 42)
			if err != nil || role != store.ProjectAccessManage {
				t.Fatalf("re-registrant grant = %q, %v; want manage", role, err)
			}
			list := admin.request(http.MethodGet, "/api/v1/projects", nil, nil)
			projects, _ := env.decode(t, list)["projects"].([]any)
			if list.status != http.StatusOK || len(projects) != 1 {
				t.Fatalf("list after re-register = %d %s", list.status, list.body)
			}
		})
	}
}

// A user-owned API token registers on behalf of its owner.
func TestRegisterProjectByUserAPITokenGrantsOwnerManage(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	session := env.loginAs("code-admin") // GitLab user 7
	created := session.json(http.MethodPost, "/api/v1/api-tokens", map[string]any{"name": "register"})
	if created.status != http.StatusCreated {
		t.Fatalf("create token = %d %s", created.status, created.body)
	}
	plain, _ := env.decode(t, created)["token"].(string)
	token := &m6Client{t: t, base: env.server.URL, client: &http.Client{}, bearer: plain}
	if got := token.json(http.MethodPost, "/api/v1/projects", map[string]any{
		"project": "42", "token": testProjectTok,
	}); got.status != http.StatusCreated {
		t.Fatalf("register = %d %s", got.status, got.body)
	}
	role, err := env.sqlite.GetProjectAccess(context.Background(), 7, 42)
	if err != nil || role != store.ProjectAccessManage {
		t.Fatalf("token owner grant = %q, %v; want manage", role, err)
	}
}
