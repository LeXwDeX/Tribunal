package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

type policyGateFixture struct {
	f          *reviewFixture
	dispatcher *Dispatcher
	projects   *ProjectService
	grant      *store.RepositorySkillAuthorization
}

func newPolicyGateFixture(t *testing.T) policyGateFixture {
	t.Helper()
	f := newReviewFixture(t, strings.Repeat("a", 40))
	attachRepositorySource(f, strings.Repeat("a", 40))
	f.publisher.git = f.service.git
	ctx := context.Background()
	settings := json.RawMessage(`{"allow_repo_skills":true,"ignore":[]}`)
	if _, err := f.db.UpdateProject(ctx, 42, store.ProjectUpdate{Settings: settings}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := f.db.CreateSession(ctx, store.Session{IDHash: []byte(strings.Repeat("x", 32)), UserID: 7, Username: "admin", Name: "admin", Role: "admin", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, store.SessionLimits{}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetProjectGrant(ctx, 42, 7, store.ProjectAccessManage); err != nil {
		t.Fatal(err)
	}
	snapshot, err := repo.RepositorySkillsSnapshotSHA256(map[string][]byte{"demo/SKILL.md": []byte(policyGateSkillBody)})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := f.db.SaveRepositorySkillAuthorization(ctx, store.RepositorySkillAuthorization{ProjectID: 42, TargetBranch: "main", Block: "review", TargetSHA: strings.Repeat("a", 40), Skills: []string{"repo:demo"}, SnapshotSHA256: snapshot}, store.BlockBindingAbsentRevision, 7)
	if err != nil {
		t.Fatal(err)
	}
	projects, err := NewProjectService(ProjectServiceConfig{Store: f.db, GitLab: defaultFakeGitLab(), Cipher: f.service.cipher, Profiles: f.profiles, Workspace: f.manager})
	if err != nil {
		t.Fatal(err)
	}
	return policyGateFixture{f: f, dispatcher: &Dispatcher{store: f.db, workspace: f.manager, onboarding: newOnboardingContextService(t, f, f.runner.Run), cipher: f.service.cipher}, projects: projects, grant: grant}
}

// Hold the content lock like RunReview and ignore cancellation until released.
func startPolicyGateLease(t *testing.T, ws *workspace.Workspace) (<-chan struct{}, func(), <-chan error) {
	t.Helper()
	token, err := ws.OutboundToken()
	if err != nil {
		t.Fatal(err)
	}
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	releaseSend := func() { once.Do(func() { close(release) }) }
	go func() {
		lifecycleUnlock, err := ws.LifecycleRLock(context.Background())
		if err != nil {
			done <- err
			return
		}
		defer lifecycleUnlock()
		unlock, err := ws.RLock(context.Background())
		if err != nil {
			done <- err
			return
		}
		defer unlock()
		done <- ws.SendOutbound(context.Background(), token, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("outbound lease did not start")
	}
	t.Cleanup(releaseSend)
	return canceled, releaseSend, done
}

func TestPolicyGateServiceWritesDrainActiveLease(t *testing.T) {
	for _, operation := range []string{"grant", "revoke", "tighten-ignore", "disable", "delete"} {
		t.Run(operation, func(t *testing.T) {
			f := newPolicyGateFixture(t)
			if operation == "grant" {
				source := &pausedPolicySource{fakeGitOps: f.f.git, entered: make(chan struct{}), release: make(chan struct{})}
				close(source.release)
				f.dispatcher.onboarding.git = source
			}
			ws := f.f.manager.Get(42)
			oldToken, err := ws.OutboundToken()
			if err != nil {
				t.Fatal(err)
			}
			canceled, release, sendDone := startPolicyGateLease(t, ws)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				switch operation {
				case "grant":
					_, err := f.dispatcher.ReplaceRepositorySkillAuthorization(ctx, *f.grant, f.grant.Revision, strings.Repeat("a", 40), f.grant.SnapshotSHA256, 7)
					done <- err
				case "revoke":
					grant := *f.grant
					grant.Skills = nil
					_, err := f.dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, f.grant.Revision, "", "", 7)
					done <- err
				case "tighten-ignore":
					settings := json.RawMessage(`{"allow_repo_skills":true,"ignore":[".claude/skills/**"]}`)
					_, err := f.projects.Update(ctx, 42, UpdateProjectInput{Settings: &settings})
					done <- err
				case "disable":
					enabled := false
					_, err := f.projects.Update(ctx, 42, UpdateProjectInput{Enabled: &enabled})
					done <- err
				case "delete":
					done <- f.projects.Delete(ctx, 42)
				}
			}()
			select {
			case <-canceled:
			case err := <-done:
				t.Fatalf("%s finished without canceling active send: %v", operation, err)
			case <-ctx.Done():
				t.Fatalf("%s could not cancel send while it held the content lock", operation)
			}
			current, err := f.f.db.GetRepositorySkillAuthorization(context.Background(), 42, "main", "review")
			if err != nil || current.Revision != f.grant.Revision {
				t.Fatalf("authorization changed before drain: %+v, %v", current, err)
			}
			project, err := f.f.db.GetProject(context.Background(), 42)
			if err != nil || !project.Enabled || string(project.Settings) != `{"allow_repo_skills":true,"ignore":[]}` {
				t.Fatalf("project changed before drain: %+v, %v", project, err)
			}
			select {
			case err := <-done:
				t.Fatalf("%s completed before send returned: %v", operation, err)
			default:
			}
			release()
			if err := <-sendDone; !errors.Is(err, workspace.ErrOutboundInvalidated) {
				t.Fatalf("send result = %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("%s after drain: %v", operation, err)
			}
			if err := ws.SendOutbound(context.Background(), oldToken, func(context.Context) error {
				t.Error("old policy token reached transport")
				return nil
			}); !errors.Is(err, workspace.ErrOutboundInvalidated) {
				t.Fatalf("old policy token after %s = %v", operation, err)
			}
			switch operation {
			case "grant":
				current, err := f.f.db.GetRepositorySkillAuthorization(context.Background(), 42, "main", "review")
				if err != nil || len(current.Skills) != 1 || current.Revision == f.grant.Revision {
					t.Fatalf("grant was not saved: %+v, %v", current, err)
				}
			case "revoke":
				current, err := f.f.db.GetRepositorySkillAuthorization(context.Background(), 42, "main", "review")
				if err != nil || len(current.Skills) != 0 || current.Revision == f.grant.Revision {
					t.Fatalf("revocation was not saved: %+v, %v", current, err)
				}
			case "tighten-ignore", "disable":
				project, err := f.f.db.GetProject(context.Background(), 42)
				if err != nil || (operation == "disable" && project.Enabled) || (operation == "tighten-ignore" && !strings.Contains(string(project.Settings), ".claude/skills/**")) {
					t.Fatalf("project update was not saved: %+v, %v", project, err)
				}
			case "delete":
				if _, err := f.f.db.GetProject(context.Background(), 42); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("project was not deleted: %v", err)
				}
			}
		})
	}
}

func TestPolicyGateServiceCancellationDoesNotCommit(t *testing.T) {
	for _, operation := range []string{"revoke", "settings", "delete"} {
		for _, timeout := range []bool{false, true} {
			name := operation + "/cancel"
			if timeout {
				name = operation + "/timeout"
			}
			t.Run(name, func(t *testing.T) {
				f := newPolicyGateFixture(t)
				canceled, release, sendDone := startPolicyGateLease(t, f.f.manager.Get(42))
				ctx, cancel := context.WithCancel(context.Background())
				if timeout {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
				}
				defer cancel()
				done := make(chan error, 1)
				go func() {
					switch operation {
					case "revoke":
						grant := *f.grant
						grant.Skills = nil
						_, err := f.dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, f.grant.Revision, "", "", 7)
						done <- err
					case "settings":
						settings := json.RawMessage(`{"allow_repo_skills":false}`)
						_, err := f.projects.Update(ctx, 42, UpdateProjectInput{Settings: &settings})
						done <- err
					case "delete":
						done <- f.projects.Delete(ctx, 42)
					}
				}()
				select {
				case <-canceled:
				case err := <-done:
					t.Fatalf("write completed before canceling lease: %v", err)
				case <-time.After(2 * time.Second):
					t.Fatal("lease was not canceled")
				}
				if !timeout {
					cancel()
				}
				want := context.Canceled
				if timeout {
					want = context.DeadlineExceeded
				}
				select {
				case err := <-done:
					if !errors.Is(err, want) {
						t.Fatalf("write returned %v, want %v", err, want)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("canceled policy write stalled")
				}
				current, err := f.f.db.GetRepositorySkillAuthorization(context.Background(), 42, "main", "review")
				if err != nil || current.Revision != f.grant.Revision || len(current.Skills) != 1 {
					t.Fatalf("authorization changed on canceled write: %+v, %v", current, err)
				}
				project, err := f.f.db.GetProject(context.Background(), 42)
				if err != nil || !project.Enabled || string(project.Settings) != `{"allow_repo_skills":true,"ignore":[]}` {
					t.Fatalf("project changed on canceled write: %+v, %v", project, err)
				}
				release()
				if err := <-sendDone; !errors.Is(err, workspace.ErrOutboundInvalidated) {
					t.Fatalf("send = %v", err)
				}
			})
		}
	}
}

func TestPolicyGateRevokeRechecksActorAfterDrain(t *testing.T) {
	for _, change := range []string{"platform", "project", "disabled"} {
		t.Run(change, func(t *testing.T) {
			f := newPolicyGateFixture(t)
			canceled, release, sendDone := startPolicyGateLease(t, f.f.manager.Get(42))
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				grant := *f.grant
				grant.Skills = nil
				_, err := f.dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, f.grant.Revision, "", "", 7)
				done <- err
			}()
			select {
			case <-canceled:
			case <-ctx.Done():
				t.Fatal("revoke did not cancel lease")
			}
			var err error
			switch change {
			case "platform":
				err = f.f.db.SetPlatformRole(context.Background(), 7, "viewer")
			case "project":
				err = f.f.db.SetProjectGrant(context.Background(), 42, 7, store.ProjectAccessRead)
			case "disabled":
				err = f.f.db.SetUserDisabled(context.Background(), 7, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			release()
			if err := <-sendDone; !errors.Is(err, workspace.ErrOutboundInvalidated) {
				t.Fatalf("send = %v", err)
			}
			if err := <-done; !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("revocation by demoted actor = %v", err)
			}
			current, err := f.f.db.GetRepositorySkillAuthorization(context.Background(), 42, "main", "review")
			if err != nil || current.Revision != f.grant.Revision || len(current.Skills) != 1 {
				t.Fatalf("demoted actor changed grant: %+v, %v", current, err)
			}
		})
	}
}

func TestPolicyGateIsolatesProjects(t *testing.T) {
	f := newPolicyGateFixture(t)
	seedProject(t, f.f.db, f.f.service.cipher, 43, "other", `{}`, "default")
	canceled, release, sendDone := startPolicyGateLease(t, f.f.manager.Get(42))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		grant := *f.grant
		grant.Skills = nil
		_, err := f.dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, f.grant.Revision, "", "", 7)
		done <- err
	}()
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("first project did not start revocation")
	}
	other := f.f.manager.Get(43)
	token, err := other.OutboundToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := other.SendOutbound(ctx, token, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("other project outbound: %v", err)
	}
	settings := json.RawMessage(`{"ignore":["secret/**"]}`)
	if _, err := f.projects.Update(ctx, 43, UpdateProjectInput{Settings: &settings}); err != nil {
		t.Fatalf("other project update: %v", err)
	}
	release()
	if err := <-sendDone; !errors.Is(err, workspace.ErrOutboundInvalidated) {
		t.Fatalf("send = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("first project revoke: %v", err)
	}
}

func TestPolicyGateDeleteDrainDoesNotBlockOtherProjectDelete(t *testing.T) {
	f := newPolicyGateFixture(t)
	seedProject(t, f.f.db, f.f.service.cipher, 43, "other", `{}`, "default")
	canceled, release, sendDone := startPolicyGateLease(t, f.f.manager.Get(42))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	first := make(chan error, 1)
	go func() { first <- f.projects.Delete(ctx, 42) }()
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("first delete did not cancel its send")
	}
	otherCtx, otherCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer otherCancel()
	other := make(chan error, 1)
	go func() { other <- f.projects.Delete(otherCtx, 43) }()
	select {
	case err := <-other:
		if err != nil {
			t.Fatalf("other project delete = %v", err)
		}
	case <-otherCtx.Done():
		t.Fatal("first project's outbound drain blocked another project delete")
	}
	release()
	if err := <-sendDone; !errors.Is(err, workspace.ErrOutboundInvalidated) {
		t.Fatalf("send = %v", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("first project delete = %v", err)
	}
}

func TestPolicyGateConcurrentWritersDoNotDeadlock(t *testing.T) {
	f := newPolicyGateFixture(t)
	canceled, release, sendDone := startPolicyGateLease(t, f.f.manager.Get(42))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	results := make(chan error, 2)
	go func() {
		grant := *f.grant
		grant.Skills = nil
		_, err := f.dispatcher.ReplaceRepositorySkillAuthorization(ctx, grant, f.grant.Revision, "", "", 7)
		results <- err
	}()
	go func() {
		settings := json.RawMessage(`{"allow_repo_skills":false,"ignore":["secret/**"]}`)
		_, err := f.projects.Update(ctx, 42, UpdateProjectInput{Settings: &settings})
		results <- err
	}()
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("concurrent writers did not cancel lease")
	}
	release()
	if err := <-sendDone; !errors.Is(err, workspace.ErrOutboundInvalidated) {
		t.Fatalf("send = %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("concurrent policy write = %v", err)
			}
		case <-ctx.Done():
			t.Fatal("concurrent policy writes deadlocked")
		}
	}
}

type pausedPolicySource struct {
	*fakeGitOps
	entered, release chan struct{}
}

func (g *pausedPolicySource) ResolveCommitRef(context.Context, string, string) (string, error) {
	return strings.Repeat("a", 40), nil
}

func (g *pausedPolicySource) ReadRepositorySkillConfig(context.Context, string, string) ([]byte, bool, error) {
	return nil, false, nil
}

const policyGateSkillBody = "---\nname: demo\ndescription: visible\n---\nbody\n"

func (g *pausedPolicySource) ReadRepositorySkills(ctx context.Context, _, _ string, _ func(string) bool) (map[string][]byte, error) {
	close(g.entered)
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return map[string][]byte{"demo/SKILL.md": []byte(policyGateSkillBody)}, nil
}

func TestPolicyGateRejectsPolicyChangeDuringDiscovery(t *testing.T) {
	for _, operation := range []string{"discovery", "grant"} {
		t.Run(operation, func(t *testing.T) {
			f := newPolicyGateFixture(t)
			source := &pausedPolicySource{fakeGitOps: f.f.git, entered: make(chan struct{}), release: make(chan struct{})}
			f.dispatcher.onboarding.git = source
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if operation == "discovery" {
					_, err := f.dispatcher.DiscoverRepositorySkills(ctx, 42, "main")
					done <- err
				} else {
					_, err := f.dispatcher.ReplaceRepositorySkillAuthorization(ctx, *f.grant, f.grant.Revision, strings.Repeat("a", 40), f.grant.SnapshotSHA256, 7)
					done <- err
				}
			}()
			select {
			case <-source.entered:
			case err := <-done:
				t.Fatalf("%s failed before discovery barrier: %v", operation, err)
			case <-ctx.Done():
				t.Fatal("discovery did not reach barrier")
			}
			settings := json.RawMessage(`{"allow_repo_skills":true,"ignore":[".claude/skills/**"]}`)
			if _, err := f.projects.Update(ctx, 42, UpdateProjectInput{Settings: &settings}); err != nil {
				close(source.release)
				t.Fatalf("tighten policy during %s: %v", operation, err)
			}
			close(source.release)
			if err := <-done; err == nil {
				t.Fatalf("%s returned stale policy data", operation)
			}
			current, err := f.f.db.GetRepositorySkillAuthorization(context.Background(), 42, "main", "review")
			if err != nil || current.Revision != f.grant.Revision {
				t.Fatalf("stale discovery changed grant: %+v, %v", current, err)
			}
		})
	}
}

func TestPolicyGateSendWaitingForContentCannotDeadlockWriter(t *testing.T) {
	f := newPolicyGateFixture(t)
	source := &pausedPolicySource{fakeGitOps: f.f.git, entered: make(chan struct{}), release: make(chan struct{})}
	f.dispatcher.onboarding.git = source
	var once sync.Once
	release := func() { once.Do(func() { close(source.release) }) }
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	writeDone := make(chan error, 1)
	go func() {
		_, err := f.dispatcher.ReplaceRepositorySkillAuthorization(ctx, *f.grant, f.grant.Revision, strings.Repeat("a", 40), f.grant.SnapshotSHA256, 7)
		writeDone <- err
	}()
	select {
	case <-source.entered:
	case err := <-writeDone:
		t.Fatalf("grant failed before discovery barrier: %v", err)
	case <-ctx.Done():
		t.Fatal("discovery did not reach barrier")
	}
	ws := f.f.manager.Get(42)
	token, err := ws.OutboundToken()
	if err != nil {
		t.Fatal(err)
	}
	started, sendDone := make(chan struct{}), make(chan error, 1)
	go func() {
		sendDone <- ws.SendOutbound(context.Background(), token, func(context.Context) error {
			close(started)
			unlock, err := ws.RLock(context.Background())
			if err == nil {
				unlock()
			}
			return err
		})
	}()
	<-started
	release()
	select {
	case err := <-writeDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("writer with blocked send = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer deadlocked with content-waiting send")
	}
	select {
	case err := <-sendDone:
		if !errors.Is(err, workspace.ErrOutboundInvalidated) {
			t.Fatalf("send = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not exit after writer released content lock")
	}
	current, err := f.f.db.GetRepositorySkillAuthorization(context.Background(), 42, "main", "review")
	if err != nil || current.Revision != f.grant.Revision {
		t.Fatalf("timed-out grant changed authorization: %+v, %v", current, err)
	}
}
