package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/prompts"
)

type discoveryPauseStore struct {
	app.DispatchStore
	entered, release chan struct{}
}

func (s *discoveryPauseStore) GetProject(ctx context.Context, id int64) (*store.Project, error) {
	project, err := s.DispatchStore.GetProject(ctx, id)
	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return project, err
}

type discoveryUnusedGit struct{ app.GitOps }
type discoveryUnusedGitLab struct{ app.GitLabClient }

func TestRepositorySkillDiscoveryRechecksCurrentAccess(t *testing.T) {
	for _, change := range []string{"unchanged", "platform", "configured-platform", "project", "disabled", "missing-current-access"} {
		t.Run(change, func(t *testing.T) { testRepositorySkillDiscoveryAccessChange(t, change) })
	}
}

func testRepositorySkillDiscoveryAccessChange(t *testing.T, change string) {
	t.Helper()
	env := newWiringEnv(t)
	_ = env.business()
	bundle, err := prompts.Load("")
	if err != nil {
		t.Fatal(err)
	}
	onboarding, err := app.NewWorkspaceService(app.WorkspaceServiceConfig{Store: env.db, GitLab: discoveryUnusedGitLab{}, Cipher: env.cipher, Workspace: env.manager, Git: discoveryUnusedGit{}, Prompts: bundle})
	if err != nil {
		t.Fatal(err)
	}
	paused := &discoveryPauseStore{DispatchStore: env.db, entered: make(chan struct{}), release: make(chan struct{})}
	dispatcher, err := app.NewDispatcher(app.DispatcherConfig{Store: paused, Workspace: env.manager, GitLab: env.api, Cipher: env.cipher, Onboarding: onboarding})
	if err != nil {
		t.Fatal(err)
	}
	env.service.workspace = dispatcher
	req := repositorySkillRequest(http.MethodGet, "/api/v1/admin/projects/42/repository-skills?target_branch=main", "", roleAdmin)
	req = req.WithContext(context.WithValue(req.Context(), principalKey, principal{user: sessionUser{ID: 7, Username: "alice", Role: roleAdmin}}))
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { env.service.handleDiscoverRepositorySkills(response, req); close(done) }()
	select {
	case <-paused.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("discovery did not reach project read")
	}
	var changeErr error
	switch change {
	case "platform":
		changeErr = env.db.SetPlatformRole(context.Background(), 7, roleViewer)
	case "configured-platform":
		delete(env.service.auth.admins, "alice")
	case "project":
		changeErr = env.db.SetProjectGrant(context.Background(), 42, 7, store.ProjectAccessRead)
	case "disabled":
		changeErr = env.db.SetUserDisabled(context.Background(), 7, true)
	case "missing-current-access":
		env.service.access = repositorySkillHTTPAccess{role: store.ProjectAccessManage}
	}
	if changeErr != nil {
		close(paused.release)
		t.Fatal(changeErr)
	}
	close(paused.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("discovery did not finish")
	}
	if change == "unchanged" && response.Code != http.StatusOK {
		t.Fatalf("current admin and manager denied discovery: %d, %s", response.Code, response.Body.String())
	}
	if change != "unchanged" && response.Code == http.StatusOK {
		t.Fatalf("changed access received discovery response: %s", response.Body.String())
	}
}
