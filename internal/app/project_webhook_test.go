package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
)

const testHookURL = "https://review.example.com/webhooks/gitlab"

func registerManualProject(t *testing.T, service *ProjectService) *Registration {
	t.Helper()
	registration, err := service.Register(context.Background(), RegisterInput{
		Project:     "42",
		AccessToken: "glpat-project-token",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return registration
}

func TestCreateWebhookReusesRecordedHook(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, cipher, "https://review.example.com")

	registration, err := service.Register(ctx, RegisterInput{
		Project:     "42",
		AccessToken: "glpat-project-token",
		AutoWebhook: true,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	originalSecret := api.hookSpecs[0].Secret

	for i := 0; i < 2; i++ {
		result, err := service.CreateWebhook(ctx, 42)
		if err != nil {
			t.Fatalf("CreateWebhook #%d: %v", i+1, err)
		}
		if result.HookID != 99 {
			t.Fatalf("hook id = %d", result.HookID)
		}
		if result.Project.WebhookState != store.WebhookStateEnabled {
			t.Fatalf("state = %q", result.Project.WebhookState)
		}
		if strings.Join(result.Project.WebhookEvents, ",") != "push_events,merge_requests_events,note_events" {
			t.Fatalf("events = %v", result.Project.WebhookEvents)
		}
	}

	if api.callCount("CreateProjectHook") != 1 {
		t.Fatalf("create calls = %d", api.callCount("CreateProjectHook"))
	}
	if api.callCount("UpdateProjectHook") != 2 {
		t.Fatalf("update calls = %d", api.callCount("UpdateProjectHook"))
	}
	if api.callCount("DeleteProjectHook") != 0 {
		t.Fatalf("delete calls = %d", api.callCount("DeleteProjectHook"))
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := cipher.Decrypt(saved.WebhookSecretEnc)
	if err != nil || string(plain) != originalSecret {
		t.Fatalf("reuse rotated the stored secret: %v", err)
	}
	if saved.WebhookState != store.WebhookStateEnabled || saved.WebhookError != "" {
		t.Fatalf("stored state = %q, error = %q", saved.WebhookState, saved.WebhookError)
	}
	encoded, err := json.Marshal(registration.Project)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{originalSecret, "glpat-project-token", "webhook_secret_enc", "token_enc"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("project JSON leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestCreateWebhookDoesNotAdoptMatchingURLHook(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, cipher, "https://review.example.com")
	registration := registerManualProject(t, service)

	api.hooks = []gitlab.Hook{
		{ID: 7, URL: "https://user.example.com/hook"},
		{ID: 55, URL: testHookURL},
	}
	result, err := service.CreateWebhook(ctx, 42)
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if result.HookID != 99 {
		t.Fatalf("hook = %d, want a new service-owned hook", result.HookID)
	}
	if api.callCount("CreateProjectHook") != 1 || len(api.editedHooks) != 0 {
		t.Fatalf("create=%d edits=%v", api.callCount("CreateProjectHook"), api.editedHooks)
	}
	if len(api.deletedHooks) != 0 {
		t.Fatalf("foreign hook was deleted: %v", api.deletedHooks)
	}
	if len(api.hooks) != 3 {
		t.Fatalf("hooks = %+v", api.hooks)
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if saved.HookID == nil || *saved.HookID != 99 || saved.WebhookState != store.WebhookStateEnabled ||
		saved.WebhookManagedBy != store.WebhookManagedByService {
		t.Fatalf("stored hook = %v, state = %q, owner = %q", saved.HookID, saved.WebhookState, saved.WebhookManagedBy)
	}
	plain, err := cipher.Decrypt(saved.WebhookSecretEnc)
	if err != nil || string(plain) != registration.WebhookSecret {
		t.Fatalf("enable changed the stored secret: %v", err)
	}
}

func TestCreateWebhookRebuildsAfterExternalDelete(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")
	if _, err := service.Register(ctx, RegisterInput{
		Project:     "42",
		AccessToken: "glpat-project-token",
		AutoWebhook: true,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	api.mu.Lock()
	api.hooks = nil
	api.hookID = 100
	api.mu.Unlock()

	result, err := service.CreateWebhook(ctx, 42)
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if result.HookID != 100 || api.callCount("CreateProjectHook") != 2 {
		t.Fatalf("hook id = %d, creates = %d", result.HookID, api.callCount("CreateProjectHook"))
	}
	if len(api.deletedHooks) != 0 {
		t.Fatalf("missing hook was deleted: %v", api.deletedHooks)
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil || saved.HookID == nil || *saved.HookID != 100 {
		t.Fatalf("stored hook = %v, %v", saved, err)
	}
}

func TestCreateWebhookRepairsRecordedHookURL(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")
	registerManualProject(t, service)
	recorded := int64(50)
	if err := st.UpdateProjectWebhook(ctx, 42, store.ProjectWebhookUpdate{
		HookID: &recorded, State: store.WebhookStateUnknown,
		ManagedBy: store.WebhookManagedByService,
	}); err != nil {
		t.Fatal(err)
	}
	api.hooks = []gitlab.Hook{
		{ID: 50, URL: "https://old.example.com/hook"},
		{ID: 55, URL: testHookURL},
	}

	result, err := service.CreateWebhook(ctx, 42)
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if result.HookID != 50 || len(api.deletedHooks) != 0 || len(api.editedHooks) != 1 || api.editedHooks[0] != 50 {
		t.Fatalf("hook id = %d, edits = %v, deletes = %v", result.HookID, api.editedHooks, api.deletedHooks)
	}
	if len(api.hooks) != 2 || api.hooks[0].URL != testHookURL {
		t.Fatalf("hooks = %+v", api.hooks)
	}
}

func TestCreateWebhookFailureState(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")
	registerManualProject(t, service)

	api.listHooksErr = &gitlab.Error{Op: "list_hooks", Status: 403}
	_, err := service.CreateWebhook(ctx, 42)
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "gitlab_rejected" || appErr.Upstream != 403 {
		t.Fatalf("error = %#v", err)
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if saved.WebhookState != store.WebhookStateFailed ||
		saved.WebhookError != "GitLab webhook list failed (status 403)" {
		t.Fatalf("state = %q, error = %q", saved.WebhookState, saved.WebhookError)
	}
	if saved.HookID != nil {
		t.Fatalf("failed provisioning recorded hook %d", *saved.HookID)
	}
	encoded, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"glpat-project-token", "upstream", "response body"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("failure state leaked %q: %s", forbidden, encoded)
		}
	}

	api.listHooksErr = nil
	result, err := service.CreateWebhook(ctx, 42)
	if err != nil {
		t.Fatalf("recovery CreateWebhook: %v", err)
	}
	if result.Project.WebhookState != store.WebhookStateEnabled || result.Project.WebhookError != "" {
		t.Fatalf("recovered state = %q, error = %q", result.Project.WebhookState, result.Project.WebhookError)
	}
}

func TestCreateWebhookUpdateFailureKeepsRecordedHook(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")
	if _, err := service.Register(ctx, RegisterInput{
		Project:     "42",
		AccessToken: "glpat-project-token",
		AutoWebhook: true,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	api.updateHookErr = &gitlab.Error{Op: "update_hook", Status: 403}
	if _, err := service.CreateWebhook(ctx, 42); err == nil {
		t.Fatal("expected update failure")
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if saved.WebhookState != store.WebhookStateFailed || saved.HookID == nil || *saved.HookID != 99 {
		t.Fatalf("state = %q, hook = %v", saved.WebhookState, saved.HookID)
	}
	if saved.WebhookError != "GitLab webhook update failed (status 403)" {
		t.Fatalf("error = %q", saved.WebhookError)
	}
}

func TestCreateWebhookIgnoresForeignHooks(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")
	registerManualProject(t, service)
	api.hooks = []gitlab.Hook{{ID: 7, URL: "https://user.example.com/hook"}}

	result, err := service.CreateWebhook(ctx, 42)
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if result.HookID != 99 || api.callCount("CreateProjectHook") != 1 ||
		len(api.editedHooks) != 0 || len(api.deletedHooks) != 0 {
		t.Fatalf("hook = %d, creates = %d, edits = %v, deletes = %v",
			result.HookID, api.callCount("CreateProjectHook"), api.editedHooks, api.deletedHooks)
	}
	if len(api.hooks) != 2 {
		t.Fatalf("hooks = %+v", api.hooks)
	}
}

// failWebhookUpdateStore fails only the selected UpdateProjectWebhook call.
type failWebhookUpdateStore struct {
	ProjectStore
	failCall int

	mu    sync.Mutex
	calls int
}

func (s *failWebhookUpdateStore) UpdateProjectWebhook(ctx context.Context, id int64, update store.ProjectWebhookUpdate) error {
	s.mu.Lock()
	s.calls++
	fail := s.calls == s.failCall
	s.mu.Unlock()
	if fail {
		return errors.New("disk full")
	}
	return s.ProjectStore.UpdateProjectWebhook(ctx, id, update)
}

func TestCreateWebhookCompensatesPersistenceFailure(t *testing.T) {
	ctx := context.Background()
	base := openTestStore(t)
	api := defaultFakeGitLab()
	wrapped := &failWebhookUpdateStore{ProjectStore: base, failCall: 2}
	service := newProjectService(t, wrapped, api, newTestCipher(t), "https://review.example.com")
	registerManualProject(t, service)

	_, err := service.CreateWebhook(ctx, 42)
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "internal" || appErr.Compensate {
		t.Fatalf("error = %#v", err)
	}
	if len(api.deletedHooks) != 1 || api.deletedHooks[0] != 99 {
		t.Fatalf("compensation deletes = %v", api.deletedHooks)
	}
	saved, err := base.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if saved.WebhookState != store.WebhookStateFailed ||
		saved.WebhookError != "webhook state persistence failed" || saved.HookID != nil {
		t.Fatalf("state = %q, error = %q, hook = %v", saved.WebhookState, saved.WebhookError, saved.HookID)
	}
}

func TestCreateWebhookConcurrentEnableCreatesOnce(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")
	registerManualProject(t, service)

	const workers = 8
	start := make(chan struct{})
	errs := make(chan error, workers)
	results := make(chan *WebhookRegistration, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := service.CreateWebhook(ctx, 42)
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		t.Fatalf("concurrent enable: %v", err)
	}
	if api.callCount("CreateProjectHook") != 1 {
		t.Fatalf("create calls = %d", api.callCount("CreateProjectHook"))
	}
	if api.callCount("UpdateProjectHook") != workers-1 {
		t.Fatalf("update calls = %d", api.callCount("UpdateProjectHook"))
	}
	for result := range results {
		if result.HookID != 99 {
			t.Fatalf("result hook = %d", result.HookID)
		}
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if saved.WebhookState != store.WebhookStateEnabled || saved.HookID == nil || *saved.HookID != 99 ||
		saved.WebhookManagedBy != store.WebhookManagedByService {
		t.Fatalf("saved = %+v", saved)
	}
	if len(api.hooks) != 1 {
		t.Fatalf("hooks = %+v", api.hooks)
	}
}

func TestDisableWebhookConcurrentCallsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")
	if _, err := service.Register(ctx, RegisterInput{
		Project: "42", AccessToken: "glpat-project-token", AutoWebhook: true,
	}); err != nil {
		t.Fatal(err)
	}

	const workers = 8
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := service.DisableWebhook(ctx, 42); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent disable: %v", err)
	}
	if api.callCount("DeleteProjectHook") != 1 || len(api.deletedHooks) != 1 || api.deletedHooks[0] != 99 {
		t.Fatalf("delete calls = %d, deleted = %v", api.callCount("DeleteProjectHook"), api.deletedHooks)
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if saved.WebhookState != store.WebhookStateDisabled || saved.HookID != nil ||
		saved.WebhookError != "" || saved.WebhookManagedBy != store.WebhookManagedByService {
		t.Fatalf("disabled = %+v", saved)
	}
}

func TestDisableWebhookDoesNotDeleteUnrecordedHook(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")
	registerManualProject(t, service)
	api.hooks = []gitlab.Hook{{ID: 55, URL: testHookURL}}

	saved, err := service.DisableWebhook(ctx, 42)
	if err != nil {
		t.Fatalf("DisableWebhook: %v", err)
	}
	if len(api.deletedHooks) != 0 || len(api.hooks) != 1 {
		t.Fatalf("foreign hook was removed: deletes=%v hooks=%+v", api.deletedHooks, api.hooks)
	}
	if saved.WebhookState != store.WebhookStateDisabled || saved.HookID != nil {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestDisableWebhookKeepsRecordedHookOnFailure(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")
	if _, err := service.Register(ctx, RegisterInput{
		Project: "42", AccessToken: "glpat-project-token", AutoWebhook: true,
	}); err != nil {
		t.Fatal(err)
	}
	api.deleteErr = &gitlab.Error{Op: "delete_hook", Status: 403}
	if _, err := service.DisableWebhook(ctx, 42); err == nil {
		t.Fatal("expected delete failure")
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if saved.WebhookState != store.WebhookStateFailed || saved.HookID == nil || *saved.HookID != 99 ||
		saved.WebhookError != "GitLab webhook delete failed (status 403)" {
		t.Fatalf("failed disable = %+v", saved)
	}
}

func TestDisableWebhookTreatsMissingHookAsDisabled(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")
	if _, err := service.Register(ctx, RegisterInput{
		Project: "42", AccessToken: "glpat-project-token", AutoWebhook: true,
	}); err != nil {
		t.Fatal(err)
	}
	api.deleteErr = &gitlab.Error{Op: "delete_hook", Status: 404}
	saved, err := service.DisableWebhook(ctx, 42)
	if err != nil {
		t.Fatalf("DisableWebhook: %v", err)
	}
	if saved.WebhookState != store.WebhookStateDisabled || saved.HookID != nil {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestRebuildWebhookRotatesSecretAndReusesRecordedHook(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, cipher, "https://review.example.com")
	if _, err := service.Register(ctx, RegisterInput{
		Project: "42", AccessToken: "glpat-project-token", AutoWebhook: true,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	oldSecret, err := cipher.Decrypt(before.WebhookSecretEnc)
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.RebuildWebhook(ctx, 42)
	if err != nil {
		t.Fatalf("RebuildWebhook: %v", err)
	}
	if result.HookID != 99 || api.callCount("CreateProjectHook") != 1 ||
		api.callCount("UpdateProjectHook") != 1 || len(api.editedHooks) != 1 || api.editedHooks[0] != 99 {
		t.Fatalf("hook = %d, creates = %d, edits = %v", result.HookID,
			api.callCount("CreateProjectHook"), api.editedHooks)
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	newSecret, err := cipher.Decrypt(saved.WebhookSecretEnc)
	if err != nil {
		t.Fatal(err)
	}
	if string(newSecret) == string(oldSecret) {
		t.Fatal("rebuild reused the old secret")
	}
	if saved.WebhookState != store.WebhookStateEnabled || saved.HookID == nil || *saved.HookID != 99 {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestRebuildWebhookRecreatesExternallyDeletedHook(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")
	if _, err := service.Register(ctx, RegisterInput{
		Project: "42", AccessToken: "glpat-project-token", AutoWebhook: true,
	}); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.hooks = nil
	api.hookID = 100
	api.mu.Unlock()

	result, err := service.RebuildWebhook(ctx, 42)
	if err != nil {
		t.Fatalf("RebuildWebhook: %v", err)
	}
	if result.HookID != 100 || api.callCount("CreateProjectHook") != 2 ||
		api.callCount("UpdateProjectHook") != 0 || len(api.deletedHooks) != 0 {
		t.Fatalf("hook = %d, creates = %d, updates = %d, deletes = %v", result.HookID,
			api.callCount("CreateProjectHook"), api.callCount("UpdateProjectHook"), api.deletedHooks)
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if saved.HookID == nil || *saved.HookID != 100 || saved.WebhookState != store.WebhookStateEnabled {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestDeleteProjectRemovesOnlyRecordedHook(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")
	if _, err := service.Register(ctx, RegisterInput{
		Project: "42", AccessToken: "glpat-project-token", AutoWebhook: true,
	}); err != nil {
		t.Fatal(err)
	}
	api.hooks = append(api.hooks, gitlab.Hook{ID: 55, URL: testHookURL})

	if err := service.Delete(ctx, 42); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(api.deletedHooks) != 1 || api.deletedHooks[0] != 99 {
		t.Fatalf("deleted hooks = %v", api.deletedHooks)
	}
	if len(api.hooks) != 1 || api.hooks[0].ID != 55 {
		t.Fatalf("foreign hook was removed: %+v", api.hooks)
	}
}

func TestCreateWebhookReusesLegacyManagedHook(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")
	registerManualProject(t, service)
	recorded := int64(55)
	if err := st.UpdateProjectWebhook(ctx, 42, store.ProjectWebhookUpdate{
		HookID: &recorded, State: store.WebhookStateUnknown,
	}); err != nil {
		t.Fatal(err)
	}
	api.hooks = []gitlab.Hook{{ID: 55, URL: testHookURL}}

	result, err := service.CreateWebhook(ctx, 42)
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if result.HookID != 55 || api.callCount("CreateProjectHook") != 0 ||
		api.callCount("UpdateProjectHook") != 1 || len(api.deletedHooks) != 0 {
		t.Fatalf("hook = %d, creates = %d, updates = %d, deletes = %v", result.HookID,
			api.callCount("CreateProjectHook"), api.callCount("UpdateProjectHook"), api.deletedHooks)
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if saved.HookID == nil || *saved.HookID != 55 || saved.WebhookState != store.WebhookStateEnabled ||
		saved.WebhookManagedBy != store.WebhookManagedByService {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestRotateTokenKeepsManagedHook(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")
	if _, err := service.Register(ctx, RegisterInput{
		Project: "42", AccessToken: "glpat-project-token", AutoWebhook: true,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := service.RotateToken(ctx, 42, "glpat-new-token"); err != nil {
		t.Fatalf("RotateToken: %v", err)
	}
	if api.callCount("DeleteProjectHook") != 0 || api.callCount("CreateProjectHook") != 1 ||
		api.callCount("UpdateProjectHook") != 0 {
		t.Fatalf("token rotation touched the hook: creates=%d updates=%d deletes=%d",
			api.callCount("CreateProjectHook"), api.callCount("UpdateProjectHook"), api.callCount("DeleteProjectHook"))
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if saved.HookID == nil || *saved.HookID != 99 || saved.WebhookState != store.WebhookStateEnabled {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestCreateWebhookReportsCompensationFailure(t *testing.T) {
	ctx := context.Background()
	base := openTestStore(t)
	api := defaultFakeGitLab()
	wrapped := &failWebhookUpdateStore{ProjectStore: base, failCall: 2}
	service := newProjectService(t, wrapped, api, newTestCipher(t), "https://review.example.com")
	registerManualProject(t, service)
	api.deleteErr = &gitlab.Error{Op: "delete_hook", Status: 500}

	_, err := service.CreateWebhook(ctx, 42)
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "internal" || !appErr.Compensate {
		t.Fatalf("error = %#v", err)
	}
	if len(api.deletedHooks) != 1 || api.deletedHooks[0] != 99 {
		t.Fatalf("compensation deletes = %v", api.deletedHooks)
	}
	saved, err := base.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if saved.WebhookState != store.WebhookStateFailed || saved.HookID != nil {
		t.Fatalf("saved = %+v", saved)
	}
}
