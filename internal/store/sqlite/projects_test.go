package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func TestListProjectsFiltered(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	mustProject(t, s, 43)

	enabled, err := s.ListProjectsFiltered(ctx, store.ProjectFilter{Enabled: ptrBool(true)})
	if err != nil || len(enabled) != 2 {
		t.Fatalf("enabled list = %+v, %v", enabled, err)
	}
	disabled, err := s.ListProjectsFiltered(ctx, store.ProjectFilter{Enabled: ptrBool(false)})
	if err != nil || len(disabled) != 0 {
		t.Fatalf("disabled list = %+v, %v", disabled, err)
	}
	if _, err := s.UpdateProject(ctx, 43, store.ProjectUpdate{Enabled: ptrBool(false)}); err != nil {
		t.Fatalf("disable project: %v", err)
	}
	disabled, err = s.ListProjectsFiltered(ctx, store.ProjectFilter{Enabled: ptrBool(false)})
	if err != nil || len(disabled) != 1 || disabled[0].ID != 43 {
		t.Fatalf("disabled list = %+v, %v", disabled, err)
	}
	matched, err := s.ListProjectsFiltered(ctx, store.ProjectFilter{Query: "PROJECT-4"})
	if err != nil || len(matched) != 2 {
		t.Fatalf("query list = %+v, %v", matched, err)
	}
	none, err := s.ListProjectsFiltered(ctx, store.ProjectFilter{Query: "absent"})
	if err != nil || len(none) != 0 {
		t.Fatalf("empty query list = %+v, %v", none, err)
	}
	if _, err := s.UpdateProject(ctx, 999, store.ProjectUpdate{Enabled: ptrBool(true)}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing project update = %v", err)
	}
}

func TestUpdateProjectFieldsTokenAndHook(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)

	profile := "deep"
	settings := json.RawMessage(`{"skip_draft":true}`)
	updated, err := s.UpdateProject(ctx, 42, store.ProjectUpdate{
		Profile: &profile, Settings: settings, Enabled: ptrBool(false),
	})
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	if updated.Profile != "deep" || string(updated.Settings) != string(settings) || updated.Enabled {
		t.Fatalf("updated = %+v", updated)
	}

	expires := time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)
	if err := s.UpdateProjectToken(ctx, 42, store.ProjectTokenUpdate{
		TokenEnc: []byte{1, 2, 3}, ExpiresAt: &expires, Scopes: []string{"api"},
	}); err != nil {
		t.Fatalf("UpdateProjectToken: %v", err)
	}
	saved, err := s.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if string(saved.TokenEnc) != string([]byte{1, 2, 3}) || saved.TokenExpiresAt == nil ||
		!saved.TokenExpiresAt.Equal(expires) || len(saved.TokenScopes) != 1 || saved.TokenScopes[0] != "api" {
		t.Fatalf("token = %+v", saved)
	}

	hookID := int64(1234)
	if err := s.UpdateProjectHook(ctx, 42, &hookID, []byte{4, 5}); err != nil {
		t.Fatalf("UpdateProjectHook: %v", err)
	}
	saved, err = s.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if saved.HookID == nil || *saved.HookID != hookID || string(saved.WebhookSecretEnc) != string([]byte{4, 5}) {
		t.Fatalf("hook = %+v", saved)
	}
	if err := s.UpdateProjectToken(ctx, 999, store.ProjectTokenUpdate{TokenEnc: []byte{1}}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing token update = %v", err)
	}
}

func TestProjectWebhookStateDefaultsAndUpdates(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)

	saved, err := s.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if saved.WebhookState != store.WebhookStateDisabled || len(saved.WebhookEvents) != 0 ||
		saved.WebhookError != "" || saved.HookID != nil {
		t.Fatalf("defaults = %+v", saved)
	}

	hookID := int64(1234)
	if err := s.UpdateProjectWebhook(ctx, 42, store.ProjectWebhookUpdate{
		HookID: &hookID, SecretEnc: []byte{4, 5},
		State: store.WebhookStateEnabled, Events: []string{"push_events", "note_events"},
	}); err != nil {
		t.Fatalf("UpdateProjectWebhook enabled: %v", err)
	}
	saved, err = s.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if saved.WebhookState != store.WebhookStateEnabled || saved.HookID == nil || *saved.HookID != hookID ||
		string(saved.WebhookSecretEnc) != string([]byte{4, 5}) ||
		len(saved.WebhookEvents) != 2 || saved.WebhookEvents[0] != "push_events" {
		t.Fatalf("enabled = %+v", saved)
	}

	// A failure keeps the recorded hook and secret but replaces state/error.
	if err := s.UpdateProjectWebhook(ctx, 42, store.ProjectWebhookUpdate{
		State: store.WebhookStateFailed, Error: "GitLab webhook list failed (status 403)",
	}); err != nil {
		t.Fatalf("UpdateProjectWebhook failed: %v", err)
	}
	saved, err = s.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if saved.WebhookState != store.WebhookStateFailed ||
		saved.WebhookError != "GitLab webhook list failed (status 403)" ||
		saved.HookID == nil || *saved.HookID != hookID ||
		string(saved.WebhookSecretEnc) != string([]byte{4, 5}) ||
		len(saved.WebhookEvents) != 0 {
		t.Fatalf("failed = %+v", saved)
	}

	long := strings.Repeat("x", 600)
	if err := s.UpdateProjectWebhook(ctx, 42, store.ProjectWebhookUpdate{
		State: store.WebhookStateFailed, Error: long,
	}); err != nil {
		t.Fatal(err)
	}
	saved, err = s.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.WebhookError) != 512 {
		t.Fatalf("error length = %d", len(saved.WebhookError))
	}

	if err := s.UpdateProjectWebhook(ctx, 42, store.ProjectWebhookUpdate{State: "bogus"}); err == nil {
		t.Fatal("invalid state was accepted")
	}
	if err := s.UpdateProjectWebhook(ctx, 999, store.ProjectWebhookUpdate{State: store.WebhookStateDisabled}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing project = %v", err)
	}
}

func TestDeleteProjectCleansExtensionsAndBindings(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	projectID := int64(42)
	otherProject := int64(43)
	mustProject(t, s, otherProject)

	if _, err := s.CreateMCPServer(ctx, store.MCPServer{
		Scope: "project", ProjectID: &projectID, Name: "srv",
		Config: json.RawMessage(`{"command":"x"}`),
	}); err != nil {
		t.Fatalf("create MCP: %v", err)
	}
	if _, err := s.CreateSkill(ctx, store.Skill{
		Scope: "project", ProjectID: &projectID, Name: "skill", Description: "d",
	}); err != nil {
		t.Fatalf("create skill: %v", err)
	}
	for _, owner := range []*int64{&projectID, &otherProject} {
		if _, err := s.UpsertBlockBinding(ctx, store.BlockBinding{
			Scope: "project", ProjectID: owner, Block: "code-review", MCPTools: []string{"t"},
		}); err != nil {
			t.Fatalf("binding: %v", err)
		}
	}
	if _, err := s.UpsertBlockBinding(ctx, store.BlockBinding{
		Scope: "platform", Block: "code-review", MCPTools: []string{"p"},
	}); err != nil {
		t.Fatalf("platform binding: %v", err)
	}

	if err := s.DeleteProject(ctx, projectID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := s.GetProject(ctx, projectID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted project = %v", err)
	}
	if servers, err := s.ListMCPServers(ctx, "project", &projectID); err != nil || len(servers) != 0 {
		t.Fatalf("servers = %+v, %v", servers, err)
	}
	if skills, err := s.ListSkills(ctx, "project", &projectID); err != nil || len(skills) != 0 {
		t.Fatalf("skills = %+v, %v", skills, err)
	}
	if bindings, err := s.ListBlockBindings(ctx, "project", &projectID); err != nil || len(bindings) != 0 {
		t.Fatalf("bindings = %+v, %v", bindings, err)
	}
	otherBindings, err := s.ListBlockBindings(ctx, "project", &otherProject)
	if err != nil || len(otherBindings) != 1 {
		t.Fatalf("other bindings = %+v, %v", otherBindings, err)
	}
	platformBindings, err := s.ListBlockBindings(ctx, "platform", nil)
	if err != nil || len(platformBindings) != 1 {
		t.Fatalf("platform bindings = %+v, %v", platformBindings, err)
	}
	if err := s.DeleteProject(ctx, projectID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
}

func TestDeleteProjectRemovesOnlyProjectJobs(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	mustProject(t, s, 43)

	head := reviewHead("a")
	run42, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	wf42, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(run42.JobID, 1, 42, 7, head))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, Cancel: true}); err != nil {
		t.Fatal(err)
	}
	run43, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 43, MRIID: 7, HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	wf43, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(run43.JobID, 1, 43, 7, head))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 43, MRIID: 7, Cancel: true}); err != nil {
		t.Fatal(err)
	}

	enqueue := func(kind, key string) {
		t.Helper()
		if _, _, err := s.Enqueue(ctx, store.Job{Kind: kind, Key: key, Payload: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("enqueue %s: %v", key, err)
		}
	}
	enqueue("workspace.onboard", "ws:42:onboard")
	enqueue("workspace.refresh", "ws:42")
	enqueue("workspace.onboard", "ws:43:onboard")
	enqueue("workspace.refresh", "ws:43")
	enqueue("review.command", "review.command:42:555")
	enqueue("review.command", "review.command:43:555")
	enqueue("review.publish", "review.publish:"+wf42)
	enqueue("review.publish", "review.publish:"+wf43)
	// Similar keys for other projects must never match the project 42 prefix.
	enqueue("workspace.refresh", "ws:420")
	enqueue("workspace.refresh", "ws:4")
	enqueue("review.run", "review:420:7")
	enqueue("review.command", "review.command:420:1")

	if err := s.DeleteProject(ctx, 42); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	removed := []string{
		"workspace.onboard:42", "ws:42:onboard", "ws:42",
		"review:42:7", "review.cancel:42:7", "review.command:42:555",
		"review.publish:" + wf42,
	}
	for _, key := range removed {
		if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE "key" = ?`, key); got != 0 {
			t.Errorf("project 42 job %q remains", key)
		}
	}
	kept := []string{
		"workspace.onboard:43", "ws:43:onboard", "ws:43",
		"review:43:7", "review.cancel:43:7", "review.command:43:555",
		"review.publish:" + wf43,
		"ws:420", "ws:4", "review:420:7", "review.command:420:1",
	}
	for _, key := range kept {
		if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE "key" = ?`, key); got != 1 {
			t.Errorf("job %q count = %d, want 1", key, got)
		}
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM workflows WHERE project_id = 43 AND id = ?`, wf43); got != 1 {
		t.Errorf("project 43 workflow was removed")
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM workflows WHERE project_id = 42`); got != 0 {
		t.Errorf("project 42 workflows remain")
	}
}

func TestListProjectsFilteredPage(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	for _, id := range []int64{42, 43, 44} {
		mustProject(t, s, id)
	}
	if _, err := s.UpdateProject(ctx, 43, store.ProjectUpdate{Enabled: ptrBool(false)}); err != nil {
		t.Fatalf("disable project: %v", err)
	}

	first, cursor, err := s.ListProjectsFilteredPage(ctx, store.ProjectFilter{Query: "project-4"}, "", 1)
	if err != nil || len(first) != 1 || first[0].ID != 42 || cursor == "" {
		t.Fatalf("first page = %+v, %q, %v", first, cursor, err)
	}
	second, cursor, err := s.ListProjectsFilteredPage(ctx, store.ProjectFilter{Query: "project-4"}, cursor, 1)
	if err != nil || len(second) != 1 || second[0].ID != 43 || cursor == "" {
		t.Fatalf("second page = %+v, %q, %v", second, cursor, err)
	}
	third, cursor, err := s.ListProjectsFilteredPage(ctx, store.ProjectFilter{Query: "project-4"}, cursor, 1)
	if err != nil || len(third) != 1 || third[0].ID != 44 || cursor != "" {
		t.Fatalf("third page = %+v, %q, %v", third, cursor, err)
	}

	disabled, cursor, err := s.ListProjectsFilteredPage(ctx,
		store.ProjectFilter{Enabled: ptrBool(false)}, "", 1)
	if err != nil || len(disabled) != 1 || disabled[0].ID != 43 || cursor != "" {
		t.Fatalf("disabled page = %+v, %q, %v", disabled, cursor, err)
	}

	if _, _, err := s.ListProjectsFilteredPage(ctx, store.ProjectFilter{}, "not-a-cursor", 1); !errors.Is(err, store.ErrInvalidCursor) {
		t.Fatalf("invalid cursor = %v", err)
	}

	_, queryCursor, err := s.ListProjectsFilteredPage(ctx, store.ProjectFilter{Query: "project-4"}, "", 1)
	if err != nil || queryCursor == "" {
		t.Fatalf("query cursor = %q, %v", queryCursor, err)
	}
	if _, _, err := s.ListProjectsFilteredPage(ctx, store.ProjectFilter{Query: "project-43"}, queryCursor, 1); !errors.Is(err, store.ErrInvalidCursor) {
		t.Fatalf("changed query cursor = %v", err)
	}
	_, unfilteredCursor, err := s.ListProjectsFilteredPage(ctx, store.ProjectFilter{}, "", 1)
	if err != nil || unfilteredCursor == "" {
		t.Fatalf("unfiltered cursor = %q, %v", unfilteredCursor, err)
	}
	if _, _, err := s.ListProjectsFilteredPage(ctx, store.ProjectFilter{Enabled: ptrBool(false)}, unfilteredCursor, 1); !errors.Is(err, store.ErrInvalidCursor) {
		t.Fatalf("changed enabled cursor = %v", err)
	}

	// A deleted row does not invalidate a keyset cursor.
	head, cursor, err := s.ListProjectsFilteredPage(ctx, store.ProjectFilter{}, "", 1)
	if err != nil || len(head) != 1 || head[0].ID != 42 {
		t.Fatalf("head page = %+v, %q, %v", head, cursor, err)
	}
	if err := s.DeleteProject(ctx, 42); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	afterDelete, _, err := s.ListProjectsFilteredPage(ctx, store.ProjectFilter{}, cursor, 1)
	if err != nil || len(afterDelete) != 1 || afterDelete[0].ID != 43 {
		t.Fatalf("page after delete = %+v, %v", afterDelete, err)
	}
}

func TestUpdateProjectWebhookTracksOwnershipAndClearsHookID(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)

	hookID := int64(1234)
	if err := s.UpdateProjectWebhook(ctx, 42, store.ProjectWebhookUpdate{
		HookID: &hookID, SecretEnc: []byte{4, 5},
		State: store.WebhookStateEnabled, Events: []string{"push_events"},
		ManagedBy: store.WebhookManagedByService,
	}); err != nil {
		t.Fatalf("UpdateProjectWebhook: %v", err)
	}
	saved, err := s.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if saved.WebhookManagedBy != store.WebhookManagedByService ||
		saved.WebhookCreatedAt == nil || saved.WebhookUpdatedAt == nil ||
		!saved.WebhookUpdatedAt.Equal(clock.Now()) {
		t.Fatalf("ownership = %+v", saved)
	}
	firstCreated := *saved.WebhookCreatedAt

	clock.Advance(time.Minute)
	if err := s.UpdateProjectWebhook(ctx, 42, store.ProjectWebhookUpdate{
		State: store.WebhookStateDisabled, ClearHookID: true,
	}); err != nil {
		t.Fatalf("clear hook: %v", err)
	}
	saved, err = s.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if saved.HookID != nil || saved.WebhookState != store.WebhookStateDisabled ||
		string(saved.WebhookSecretEnc) != string([]byte{4, 5}) ||
		saved.WebhookManagedBy != store.WebhookManagedByService ||
		saved.WebhookCreatedAt == nil || !saved.WebhookCreatedAt.Equal(firstCreated) {
		t.Fatalf("cleared hook = %+v", saved)
	}
	if saved.WebhookUpdatedAt == nil || !saved.WebhookUpdatedAt.Equal(clock.Now()) {
		t.Fatalf("updated_at = %v", saved.WebhookUpdatedAt)
	}

	if err := s.UpdateProjectWebhook(ctx, 42, store.ProjectWebhookUpdate{
		HookID: &hookID, ClearHookID: true, State: store.WebhookStateDisabled,
	}); err == nil {
		t.Fatal("setting and clearing the hook in one update was accepted")
	}
}

func TestDeleteProjectRemovesReviewSideEffectsAndJobs(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	mustProject(t, s, 43)

	for _, projectID := range []int64{42, 43} {
		if _, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
			ProjectID: projectID, MRIID: 7, HeadSHA: reviewHead("a"),
			Payload: json.RawMessage(`{"trigger":"command","note_id":555}`),
		}); err != nil {
			t.Fatalf("EnqueueReviewLatest(%d): %v", projectID, err)
		}
	}
	for _, key := range []string{
		"review.candidate:42:7", "review.candidate:43:7",
	} {
		if _, _, err := s.Enqueue(ctx, store.Job{Kind: "review.candidate", Key: key, Payload: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("enqueue %s: %v", key, err)
		}
	}

	if err := s.DeleteProject(ctx, 42); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM review_status WHERE project_id = 42`,
		`SELECT COUNT(*) FROM review_reactions WHERE project_id = 42`,
		`SELECT COUNT(*) FROM jobs WHERE "key" LIKE 'review.candidate:42:%'`,
		`SELECT COUNT(*) FROM jobs WHERE "key" LIKE 'review.status:42:%'`,
		`SELECT COUNT(*) FROM jobs WHERE "key" LIKE 'review.reaction:42:%'`,
	} {
		if got := rowCount(t, s, query); got != 0 {
			t.Fatalf("%s = %d", query, got)
		}
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_status WHERE project_id = 43`); got != 1 {
		t.Fatalf("other project review_status = %d", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_reactions WHERE project_id = 43`); got != 1 {
		t.Fatalf("other project review_reactions = %d", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE "key" LIKE 'review.candidate:43:%'`); got != 1 {
		t.Fatalf("other project candidate jobs = %d", got)
	}
	if _, err := s.GetProject(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted project = %v", err)
	}

	mustProject(t, s, 42)
	if _, err := s.GetProject(ctx, 42); err != nil {
		t.Fatalf("re-registered project: %v", err)
	}
}

func ptrBool(value bool) *bool { return &value }
