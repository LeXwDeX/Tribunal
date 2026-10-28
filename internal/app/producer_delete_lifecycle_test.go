package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type observingWebhookStore struct {
	WebhookStore
	reads atomic.Int32
}

func TestLifecycleDeletedProjectRejectsExtensionWritesBeforeStaging(t *testing.T) {
	for _, operation := range []string{"upload", "register", "binding", "mcp-create", "mcp-import", "mcp-test"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			r := newLifecycleRuntime(t)
			service, err := NewExtensionService(ExtensionServiceConfig{Store: r.store, Workspace: r.workspace})
			if err != nil {
				t.Fatal(err)
			}
			id := int64(42)
			archive := testSkillArchive(t, testSkillBody)
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte(testSkillBody), 0o600); err != nil {
				t.Fatal(err)
			}
			server, err := service.CreateMCPServer(ctx, MCPServerInput{Scope: "project", ProjectID: &id, Name: "test", Config: json.RawMessage(`{"command":"synthetic"}`)})
			if err != nil {
				t.Fatal(err)
			}
			unlock, err := r.workspace.Get(id).Lock(ctx)
			if err != nil {
				t.Fatal(err)
			}
			wait := &waitContext{Context: ctx, waiting: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "upload":
					_, err = service.UploadSkill(wait, SkillUpload{Scope: "project", ProjectID: &id, Data: archive})
				case "register":
					_, err = service.RegisterSkillDir(wait, SkillDirInput{Scope: "project", ProjectID: &id, Dir: source})
				case "binding":
					_, err = service.UpsertBlockBinding(wait, BlockBindingInput{Scope: "project", ProjectID: &id, Block: "review"})
				case "mcp-create":
					_, err = service.CreateMCPServer(wait, MCPServerInput{Scope: "project", ProjectID: &id, Name: "late", Config: json.RawMessage(`{"command":"synthetic"}`)})
				case "mcp-import":
					_, err = service.ImportMCPServers(wait, MCPImportInput{Scope: "project", ProjectID: &id, Servers: map[string]json.RawMessage{"late": json.RawMessage(`{"command":"synthetic"}`)}})
				case "mcp-test":
					_, err = service.TestMCPServer(wait, *server)
				}
				done <- err
			}()
			waitConsumer(t, wait.waiting)
			if err := r.store.DeleteProject(ctx, id); err != nil {
				unlock()
				t.Fatal(err)
			}
			unlock()
			if err := <-done; err == nil {
				t.Fatal("operation accepted a deleted project")
			}
			for _, path := range []string{r.workspace.Get(id).Root, projectSkillsContainer(r.workspace.Root(), id)} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("operation created %s: %v", path, err)
				}
			}
			rows, err := r.store.ListBlockBindings(ctx, "project", &id)
			if err != nil || len(rows) != 0 {
				t.Fatalf("deleted project bindings = %+v, %v", rows, err)
			}
		})
	}
}

func TestLifecycleExtensionOldResourcesCannotCrossRegistration(t *testing.T) {
	ctx := context.Background()
	r := newLifecycleRuntime(t)
	s, err := NewExtensionService(ExtensionServiceConfig{Store: r.store, Workspace: r.workspace})
	if err != nil {
		t.Fatal(err)
	}
	id := int64(42)
	archive := testSkillArchive(t, testSkillBody)
	oldSkill, err := s.UploadSkill(ctx, SkillUpload{Scope: "project", ProjectID: &id, Data: archive})
	if err != nil {
		t.Fatal(err)
	}
	oldServer, err := s.CreateMCPServer(ctx, MCPServerInput{Scope: "project", ProjectID: &id, Name: "server", Config: json.RawMessage(`{"command":"synthetic"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Projects().Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	seedProject(t, r.store, r.cipher, id, "new-secret", "{}", "default")
	newSkill, err := s.UploadSkill(ctx, SkillUpload{Scope: "project", ProjectID: &id, Data: archive})
	if err != nil {
		t.Fatal(err)
	}
	newServer, err := s.CreateMCPServer(ctx, MCPServerInput{Scope: "project", ProjectID: &id, Name: "server", Config: json.RawMessage(`{"command":"synthetic"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSkill(ctx, oldSkill.ID); err == nil {
		t.Fatal("old skill deletion succeeded")
	}
	if _, err := s.SetSkillEnabled(ctx, oldSkill.ID, false); err == nil {
		t.Fatal("old skill update succeeded")
	}
	if _, err := s.UpdateMCPServer(ctx, oldServer.ID, MCPServerUpdate{}); err == nil {
		t.Fatal("old server update succeeded")
	}
	if err := s.DeleteMCPServer(ctx, oldServer.ID); err == nil {
		t.Fatal("old server deletion succeeded")
	}
	s.newMCPTest = func(mcp.Options) (mcpTestSession, error) {
		t.Error("old server started an MCP test")
		return nil, errors.New("unexpected MCP")
	}
	if _, err := s.TestMCPServer(ctx, *oldServer); err == nil {
		t.Fatal("old server test succeeded")
	}
	if _, err := s.GetSkill(ctx, newSkill.ID); err != nil {
		t.Fatalf("new skill missing: %v", err)
	}
	if _, err := s.GetMCPServer(ctx, newServer.ID); err != nil {
		t.Fatalf("new server missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(newSkill.Dir, "SKILL.md")); err != nil {
		t.Fatalf("new skill directory missing: %v", err)
	}
}

type blockingMCPTestSession struct {
	entered chan struct{}
	release chan struct{}
}

func (s *blockingMCPTestSession) ListTools(context.Context, string) ([]mcp.ToolInfo, error) {
	return []mcp.ToolInfo{{Name: "synthetic"}}, nil
}

func (s *blockingMCPTestSession) Close() error {
	close(s.entered)
	<-s.release
	return errors.New("private synthetic close error")
}

func TestLifecycleMCPTestClosesBeforeDeleteAndReportsCloseFailure(t *testing.T) {
	r := newLifecycleRuntime(t)
	s, err := NewExtensionService(ExtensionServiceConfig{Store: r.store, Workspace: r.workspace})
	if err != nil {
		t.Fatal(err)
	}
	id := int64(42)
	server, err := s.CreateMCPServer(context.Background(), MCPServerInput{Scope: "project", ProjectID: &id, Name: "test", Config: json.RawMessage(`{"command":"synthetic"}`)})
	if err != nil {
		t.Fatal(err)
	}
	session := &blockingMCPTestSession{entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(session.release) })
	s.newMCPTest = func(mcp.Options) (mcpTestSession, error) { return session, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.TestMCPServer(ctx, *server); done <- err }()
	waitConsumer(t, session.entered)
	deleteCtx := &waitContext{Context: ctx, waiting: make(chan struct{})}
	deleted := make(chan error, 1)
	go func() { deleted <- r.Projects().Delete(deleteCtx, id) }()
	waitConsumer(t, deleteCtx.waiting)
	if _, err := r.store.GetProject(ctx, id); err != nil {
		t.Fatalf("project deleted before MCP close: %v", err)
	}
	release.Do(func() { close(session.release) })
	if err := <-done; err == nil || strings.Contains(err.Error(), "private synthetic") {
		t.Fatalf("close error was hidden or leaked: %v", err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
}

type enqueueBarrier struct {
	entered chan struct{}
	release chan struct{}
}

func (b *enqueueBarrier) wait(ctx context.Context) error {
	close(b.entered)
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type barrierWebhookEnqueue struct {
	WebhookStore
	barrier *enqueueBarrier
}

func (s *barrierWebhookEnqueue) RecordDelivery(ctx context.Context, d store.Delivery, j *store.Job) (string, bool, error) {
	if err := s.barrier.wait(ctx); err != nil {
		return "", false, err
	}
	return s.WebhookStore.RecordDelivery(ctx, d, j)
}

type barrierDispatchEnqueue struct {
	DispatchStore
	barrier *enqueueBarrier
}

func (s *barrierDispatchEnqueue) EnqueueReviewLatest(ctx context.Context, command store.ReviewCommand) (store.ReviewEnqueueResult, error) {
	if err := s.barrier.wait(ctx); err != nil {
		return store.ReviewEnqueueResult{}, err
	}
	return s.DispatchStore.EnqueueReviewLatest(ctx, command)
}

func (s *barrierDispatchEnqueue) EnqueueLatest(ctx context.Context, job store.Job) (string, bool, error) {
	if err := s.barrier.wait(ctx); err != nil {
		return "", false, err
	}
	return s.DispatchStore.EnqueueLatest(ctx, job)
}

type barrierCommandEnqueue struct {
	CommandStore
	barrier *enqueueBarrier
}

func (s *barrierCommandEnqueue) EnqueueReviewLatest(ctx context.Context, command store.ReviewCommand) (store.ReviewEnqueueResult, error) {
	if err := s.barrier.wait(ctx); err != nil {
		return store.ReviewEnqueueResult{}, err
	}
	return s.CommandStore.EnqueueReviewLatest(ctx, command)
}

type barrierWorkspaceEnqueue struct {
	JobEnqueuer
	barrier *enqueueBarrier
}

func (s *barrierWorkspaceEnqueue) Enqueue(ctx context.Context, job store.Job) (string, bool, error) {
	if err := s.barrier.wait(ctx); err != nil {
		return "", false, err
	}
	return s.JobEnqueuer.Enqueue(ctx, job)
}

func TestLifecycleDeleteWaitsForProducerEnqueue(t *testing.T) {
	for _, operation := range []string{"webhook", "dispatch-review", "dispatch-refresh", "command", "onboard", "refresh"} {
		t.Run(operation, func(t *testing.T) {
			r := newLifecycleRuntime(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			b := &enqueueBarrier{entered: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			defer release.Do(func() { close(b.release) })
			api := defaultFakeGitLab()
			dispatcher, err := NewDispatcher(DispatcherConfig{Store: &barrierDispatchEnqueue{DispatchStore: r.store, barrier: b}, GitLab: api, Cipher: r.cipher, Workspace: r.workspace, Profiles: testProfiles()})
			if err != nil {
				t.Fatal(err)
			}
			r.webhooks.store = &barrierWebhookEnqueue{WebhookStore: r.store, barrier: b}
			r.commands.store = &barrierCommandEnqueue{CommandStore: r.store, barrier: b}
			r.commands.gitlab = api
			r.workspaces.queue = &barrierWorkspaceEnqueue{JobEnqueuer: r.store, barrier: b}
			command := persistedCommandJob(t, r.store, defaultCommandPayload("run"))
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "webhook":
					_, err = r.webhooks.Handle(ctx, WebhookRequest{Event: "Push Hook", Token: "secret", UUID: "d20388c2-1c6b-49fb-9faf-1cb9a2b6d405", Body: pushBody(42, "refs/heads/main", sha40)})
				case "dispatch-review":
					_, err = dispatcher.StartReview(ctx, 42, 7, "", false)
				case "dispatch-refresh":
					_, err = dispatcher.RefreshWorkspace(ctx, 42, true, true)
				case "command":
					err = r.commands.Handle(ctx, command)
				case "onboard":
					_, err = r.workspaces.EnqueueOnboard(ctx, 42)
				case "refresh":
					_, err = r.workspaces.EnqueueRefresh(ctx, 42, "refs/heads/main", "main", sha40)
				}
				done <- err
			}()
			waitConsumer(t, b.entered)
			deleteCtx := &waitContext{Context: ctx, waiting: make(chan struct{})}
			deleted := make(chan error, 1)
			go func() { deleted <- r.Projects().Delete(deleteCtx, 42) }()
			waitConsumer(t, deleteCtx.waiting)
			if _, err := r.store.GetProject(ctx, 42); err != nil {
				t.Fatalf("project disappeared before enqueue committed: %v", err)
			}
			release.Do(func() { close(b.release) })
			if err := <-done; err != nil {
				t.Fatalf("producer with queued delete: %v", err)
			}
			if err := <-deleted; err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if _, err := r.store.GetJob(ctx, command.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("old command job survived deletion: %v", err)
			}
		})
	}
}

func TestDispatchAuthorizationHoldsLifecycleLockWithQueuedDelete(t *testing.T) {
	for _, operation := range []string{"review", "refresh"} {
		t.Run(operation, func(t *testing.T) {
			r := newLifecycleRuntime(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			dispatcher, err := NewDispatcher(DispatcherConfig{Store: r.store, GitLab: defaultFakeGitLab(), Cipher: r.cipher, Workspace: r.workspace, Profiles: testProfiles()})
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			authorized := WithDispatchAuthorization(ctx, func(ctx context.Context, id int64) error {
				if id != 42 {
					return store.ErrNotFound
				}
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			done := make(chan error, 1)
			go func() {
				var err error
				if operation == "review" {
					_, err = dispatcher.StartReview(authorized, 42, 7, "", false)
				} else {
					_, err = dispatcher.RefreshWorkspace(authorized, 42, false, false)
				}
				done <- err
			}()
			waitConsumer(t, entered)
			deleteCtx := &waitContext{Context: ctx, waiting: make(chan struct{})}
			deleted := make(chan error, 1)
			go func() { deleted <- r.Projects().Delete(deleteCtx, 42) }()
			waitConsumer(t, deleteCtx.waiting)
			if _, err := r.store.GetProject(ctx, 42); err != nil {
				t.Fatalf("delete entered during authorization: %v", err)
			}
			once.Do(func() { close(release) })
			if err := <-done; err != nil {
				t.Fatalf("dispatch with writer queued: %v", err)
			}
			if err := <-deleted; err != nil {
				t.Fatalf("delete: %v", err)
			}
		})
	}
}

func TestLifecycleOldCommandJobCannotEnqueueAfterReregister(t *testing.T) {
	ctx := context.Background()
	r := newLifecycleRuntime(t)
	job := persistedCommandJob(t, r.store, defaultCommandPayload("run"))
	if err := r.Projects().Delete(ctx, 42); err != nil {
		t.Fatal(err)
	}
	seedProject(t, r.store, r.cipher, 42, "new-secret", "{}", "default")
	api := defaultFakeGitLab()
	r.commands.gitlab = api
	if err := r.commands.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	if len(api.callList()) != 0 {
		t.Fatalf("old command reached GitLab: %v", api.callList())
	}
	if _, err := r.store.GetReviewControl(ctx, 42, 7); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old command changed new project: %v", err)
	}
}

func TestLifecycleOldCancelJobCannotSettleNewRegistration(t *testing.T) {
	ctx := context.Background()
	r := newLifecycleRuntime(t)
	old, err := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, Cancel: true, Payload: json.RawMessage(`{"note_id":555}`)})
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.store.GetJob(ctx, old.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Projects().Delete(ctx, 42); err != nil {
		t.Fatal(err)
	}
	seedProject(t, r.store, r.cipher, 42, "new-secret", "{}", "default")
	if _, err := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, Cancel: true, Payload: json.RawMessage(`{"note_id":555}`)}); err != nil {
		t.Fatal(err)
	}
	if err := r.reviews.CancelReview(ctx, job); err != nil && !IsPermanentJob(err) {
		t.Fatal(err)
	}
	status, err := r.store.GetReviewStatus(ctx, 42, 7)
	if err != nil || status.State != store.ReviewStatusQueued {
		t.Fatalf("old cancel changed new status: %+v, %v", status, err)
	}
	reaction, err := r.store.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil || reaction.Desired != store.ReviewReactionEyes {
		t.Fatalf("old cancel changed new reaction: %+v, %v", reaction, err)
	}
}

func TestLifecycleOldCandidateActivationRejectedAfterReregister(t *testing.T) {
	ctx := context.Background()
	r := newLifecycleRuntime(t)
	_, err := r.store.RecordReviewCandidateDelivery(ctx, store.Delivery{UUID: "candidate-old", ProjectID: 42, Event: "Merge Request Hook", Outcome: store.DeliveryEnqueued}, &store.ReviewCandidate{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"trigger":"mr_open"}`), MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.store.Claim(ctx, "synthetic-worker", []string{"review.candidate"}, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim candidate: %+v, %v", job, err)
	}
	if err := r.Projects().Delete(ctx, 42); err != nil {
		t.Fatal(err)
	}
	seedProject(t, r.store, r.cipher, 42, "new-secret", "{}", "default")
	_, _, err = r.store.ActivateReviewCandidate(ctx, store.ReviewCandidateActivation{JobID: job.ID, LeaseOwner: job.LeaseOwner, Attempts: job.Attempts, ProjectID: 42, MRIID: 7, HeadSHA: sha40, CandidateSeq: 1})
	if !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("old candidate activation = %v", err)
	}
	if _, err := r.store.GetReviewControl(ctx, 42, 7); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old candidate changed new project: %v", err)
	}
}

func TestLifecycleOldStatusJobCannotApplyNewRegistration(t *testing.T) {
	ctx := context.Background()
	r := newLifecycleRuntime(t)
	id, _, err := r.store.Enqueue(ctx, store.Job{Kind: "review.status", Key: "review.status:42:7", Payload: json.RawMessage(`{"project_id":42,"mr_iid":7}`)})
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.store.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Projects().Delete(ctx, 42); err != nil {
		t.Fatal(err)
	}
	seedProject(t, r.store, r.cipher, 42, "new-secret", "{}", "default")
	if _, err := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"profile":"default"}`)}); err != nil {
		t.Fatal(err)
	}
	r.reviews.status = testStatusReporter(newReviewGitLab(sha40))
	if err := r.reviews.RunStatusJob(ctx, job); err != nil && !IsPermanentJob(err) {
		t.Fatal(err)
	}
	status, err := r.store.GetReviewStatus(ctx, 42, 7)
	if err != nil || status.AppliedVersion != 0 {
		t.Fatalf("old status job applied new status: %+v, %v", status, err)
	}
}

func (s *observingWebhookStore) GetProject(ctx context.Context, id int64) (*store.Project, error) {
	s.reads.Add(1)
	return s.WebhookStore.GetProject(ctx, id)
}

func TestLifecycleProducerWaitsBeforeProjectReadOrSkillCreation(t *testing.T) {
	for _, operation := range []string{"webhook", "upload"} {
		t.Run(operation, func(t *testing.T) {
			r := newLifecycleRuntime(t)
			observed := &observingWebhookStore{WebhookStore: r.store}
			r.webhooks.store = observed
			service, err := NewExtensionService(ExtensionServiceConfig{Store: r.store, Workspace: r.workspace})
			if err != nil {
				t.Fatal(err)
			}
			archive := testSkillArchive(t, testSkillBody)
			// The webhook entry serializes on the lifecycle lock; the upload
			// path still takes the content lock.
			lockProject := r.workspace.Get(42).Lock
			if operation == "webhook" {
				lockProject = r.workspace.Get(42).LifecycleLock
			}
			unlock, err := lockProject(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wait := &waitContext{Context: ctx, waiting: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				if operation == "webhook" {
					_, err := r.webhooks.Handle(wait, WebhookRequest{Event: "Merge Request Hook", Token: "secret", UUID: "d20388c2-1c6b-49fb-9faf-1cb9a2b6d404", Body: mrBody(42, 7, nil)})
					done <- err
					return
				}
				id := int64(42)
				_, err := service.UploadSkill(wait, SkillUpload{Scope: "project", ProjectID: &id, Data: archive})
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("operation bypassed project lock: %v", err)
			case <-wait.waiting:
			case <-time.After(5 * time.Second):
				t.Fatal("operation did not reach lock")
			}
			if operation == "webhook" && observed.reads.Load() != 0 {
				t.Error("webhook read project before taking lock")
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("operation = %v, want canceled", err)
			}
			if _, err := os.Lstat(filepath.Join(r.workspace.Root(), "skills", "project-42")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("skill directory created before lock: %v", err)
			}
		})
	}
}

func TestLifecycleCleanupRechecksOwnershipAfterLock(t *testing.T) {
	ctx := context.Background()
	r := newLifecycleRuntime(t)
	if _, err := r.store.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready"}); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(r.workspace.Get(42).Root, "runs", "old-run", "canary")
	writeProjectCanary(t, canary)
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(filepath.Dir(canary), old, old); err != nil {
		t.Fatal(err)
	}
	git := &cleanupTestGit{}
	r.runCleanup.git = git
	unlock, err := r.workspace.Get(42).Lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wait := &waitContext{Context: ctx, waiting: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- r.runCleanup.RunOnce(wait) }()
	waitConsumer(t, wait.waiting)
	if err := r.store.DeleteProject(ctx, 42); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	requireProjectCanary(t, canary)
	if got := git.removedPaths(); len(got) != 0 {
		t.Fatalf("cleanup touched unowned run: %v", got)
	}
}
