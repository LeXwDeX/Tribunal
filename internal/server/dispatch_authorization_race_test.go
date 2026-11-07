package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/app"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type registrationGitLab struct{ gitlab.API }

func (registrationGitLab) GetProject(context.Context, string, string) (*gitlab.Project, error) {
	return &gitlab.Project{ID: 42, PathWithNamespace: "group/repo", WebURL: "https://gitlab.example.com/group/repo", DefaultBranch: "main"}, nil
}

func (registrationGitLab) GetTokenInfo(context.Context, string) (*gitlab.TokenInfo, error) {
	return &gitlab.TokenInfo{Active: true, Scopes: []string{"api", "read_repository"}}, nil
}

func (registrationGitLab) CurrentUser(context.Context, string) (*gitlab.User, error) {
	return &gitlab.User{ID: 9, Username: "registrant"}, nil
}

func (registrationGitLab) GetMemberAccess(context.Context, string, int64, int64) (int, error) {
	return 30, nil
}

type dispatchBodyBarrier struct {
	entered chan struct{}
	release chan struct{}
	reader  io.Reader
	once    sync.Once
}

func (b *dispatchBodyBarrier) Read(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.reader.Read(p)
}

func TestDispatchHTTPAuthorizationAfterReregistration(t *testing.T) {
	for _, path := range []string{"/api/v1/projects/42/merge_requests/7/reviews", "/api/v1/projects/42/workspace/refresh"} {
		t.Run(path, func(t *testing.T) {
			env := newWiringEnv(t)
			client := env.loginAs("code-viewer")
			ctx := context.Background()
			if err := env.db.SetProjectGrant(ctx, 42, 8, store.ProjectAccessManage); err != nil {
				t.Fatal(err)
			}
			projects, err := app.NewProjectService(app.ProjectServiceConfig{Store: env.db, GitLab: registrationGitLab{}, Cipher: env.cipher, Workspace: env.manager, Profiles: map[string]config.ProfileConfig{"default": {}}})
			if err != nil {
				t.Fatal(err)
			}
			body := &dispatchBodyBarrier{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(`{}`)}
			req := httptest.NewRequest(http.MethodPost, env.server.URL+path, body)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-CSRF-Token", client.csrf)
			base, _ := url.Parse(env.server.URL)
			for _, cookie := range client.client.Jar.Cookies(base) {
				req.AddCookie(cookie)
			}
			res := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { env.handler.ServeHTTP(res, req); close(done) }()
			select {
			case <-body.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("request did not reach body")
			}
			if err := projects.Delete(ctx, 42); err != nil {
				close(body.release)
				t.Fatal(err)
			}
			_, err = projects.Register(ctx, app.RegisterInput{Project: "42", AccessToken: "new-synthetic-token"})
			if err != nil {
				close(body.release)
				t.Fatal(err)
			}
			if _, err := env.db.GetProjectAccess(ctx, 8, 42); !errors.Is(err, store.ErrNotFound) {
				close(body.release)
				t.Fatalf("old grant survived: %v", err)
			}
			close(body.release)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("request stalled")
			}
			if res.Code != http.StatusNotFound && res.Code != http.StatusForbidden {
				t.Errorf("status = %d, body %s", res.Code, res.Body.String())
			}
			env.api.mu.Lock()
			calls := env.api.calls
			env.api.mu.Unlock()
			if calls != 0 {
				t.Errorf("GitLab calls = %d", calls)
			}
			job, err := env.db.Claim(ctx, "authorization-test", []string{"review.run", "workspace.refresh"}, time.Minute)
			if err != nil || job != nil {
				t.Errorf("unexpected job = %+v, err = %v", job, err)
			}
		})
	}
}

type pausedReviewDispatch struct {
	ReviewController
	entered chan struct{}
	release chan struct{}
}

func (p *pausedReviewDispatch) CancelReview(ctx context.Context, id string) (string, error) {
	close(p.entered)
	<-p.release
	return p.ReviewController.CancelReview(ctx, id)
}

func (p *pausedReviewDispatch) RepublishReview(ctx context.Context, id string) (string, error) {
	close(p.entered)
	<-p.release
	return p.ReviewController.RepublishReview(ctx, id)
}

func TestDispatchReviewActionsRecheckGrant(t *testing.T) {
	for _, action := range []string{"cancel", "republish"} {
		t.Run(action, func(t *testing.T) {
			env := newWiringEnv(t)
			client := env.loginAs("code-viewer")
			if err := env.db.SetProjectGrant(context.Background(), 42, 8, store.ProjectAccessManage); err != nil {
				t.Fatal(err)
			}
			assertStatus(t, client.json(http.MethodPost, "/api/v1/projects/42/merge_requests/7/reviews", nil), http.StatusAccepted)
			id := env.seededWorkflow()
			paused := &pausedReviewDispatch{ReviewController: env.dispatcher, entered: make(chan struct{}), release: make(chan struct{})}
			env.service.reviews = paused
			done := make(chan apiResponse, 1)
			go func() { done <- client.request(http.MethodPost, "/api/v1/reviews/"+id+"/"+action, nil, nil) }()
			select {
			case <-paused.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("dispatch did not start")
			}
			if err := env.db.DeleteProjectGrant(context.Background(), 42, 8); err != nil {
				close(paused.release)
				t.Fatal(err)
			}
			close(paused.release)
			select {
			case response := <-done:
				if response.status != http.StatusNotFound && response.status != http.StatusForbidden {
					t.Fatalf("status = %d", response.status)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("dispatch stalled")
			}
			job, err := env.db.Claim(context.Background(), "authorization-test", []string{"review.cancel", "review.publish"}, time.Minute)
			if err != nil || job != nil {
				t.Fatalf("unexpected job = %+v, err = %v", job, err)
			}
		})
	}
}

func TestDispatchHTTPRechecksCurrentGrantAndEnabledUser(t *testing.T) {
	for _, change := range []string{"revoke", "read", "disabled"} {
		t.Run(change, func(t *testing.T) {
			env := newWiringEnv(t)
			client := env.loginAs("code-viewer")
			if err := env.db.SetProjectGrant(context.Background(), 42, 8, store.ProjectAccessManage); err != nil {
				t.Fatal(err)
			}
			body := &dispatchBodyBarrier{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(`{}`)}
			req := httptest.NewRequest(http.MethodPost, env.server.URL+"/api/v1/projects/42/workspace/refresh", body)
			req.Header.Set("X-CSRF-Token", client.csrf)
			base, _ := url.Parse(env.server.URL)
			for _, cookie := range client.client.Jar.Cookies(base) {
				req.AddCookie(cookie)
			}
			res := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { env.handler.ServeHTTP(res, req); close(done) }()
			select {
			case <-body.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("request did not reach body")
			}
			ctx := context.Background()
			var err error
			switch change {
			case "revoke":
				err = env.db.DeleteProjectGrant(ctx, 42, 8)
			case "read":
				err = env.db.SetProjectGrant(ctx, 42, 8, store.ProjectAccessRead)
			case "disabled":
				err = env.db.SetUserDisabled(ctx, 8, true)
			}
			close(body.release)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("dispatch stalled")
			}
			if res.Code != http.StatusNotFound && res.Code != http.StatusForbidden {
				t.Fatalf("status = %d", res.Code)
			}
			job, err := env.db.Claim(ctx, "authorization-test", []string{"workspace.refresh"}, time.Minute)
			if err != nil || job != nil {
				t.Fatalf("unexpected job = %+v, err = %v", job, err)
			}
		})
	}
}

// A platform administrator waiting on the project lifecycle lock must lose
// dispatch permission if the account is disabled while it waits. Configured
// administrators cannot be demoted at runtime (a configuration change requires
// a restart); granted administrators are re-evaluated by roleFor inside the
// same callback.
func TestDispatchAdminRechecksAccountStateAfterLockWait(t *testing.T) {
	for _, change := range []string{"disabled"} {
		t.Run(change, func(t *testing.T) {
			env := newWiringEnv(t)
			ctx := context.Background()
			client := env.loginAs("code-admin")
			userID := int64(7)
			body := &dispatchBodyBarrier{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(`{}`)}
			req := httptest.NewRequest(http.MethodPost, env.server.URL+"/api/v1/projects/42/workspace/refresh", body)
			req.Header.Set("X-CSRF-Token", client.csrf)
			base, _ := url.Parse(env.server.URL)
			for _, cookie := range client.client.Jar.Cookies(base) {
				req.AddCookie(cookie)
			}
			res := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { env.handler.ServeHTTP(res, req); close(done) }()
			select {
			case <-body.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("request did not reach body")
			}
			err := env.db.SetUserDisabled(ctx, userID, true)
			close(body.release)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("dispatch stalled")
			}
			if res.Code != http.StatusNotFound && res.Code != http.StatusForbidden {
				t.Fatalf("status = %d", res.Code)
			}
			job, err := env.db.Claim(ctx, "authorization-test", []string{"workspace.refresh"}, time.Minute)
			if err != nil || job != nil {
				t.Fatalf("unexpected job = %+v, err = %v", job, err)
			}
		})
	}
}
