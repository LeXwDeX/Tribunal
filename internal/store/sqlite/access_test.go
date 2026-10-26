package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

func accessHash(label string) []byte {
	sum := sha256.Sum256([]byte(label))
	return sum[:]
}

func mustAccessUser(t *testing.T, s *Store, id int64, username, role string) {
	t.Helper()
	now := s.now().UTC()
	if err := s.CreateSession(context.Background(), store.Session{
		IDHash:    accessHash(fmt.Sprintf("bootstrap-user-%d", id)),
		UserID:    id,
		Username:  username,
		Name:      username,
		Role:      role,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}, store.SessionLimits{}); err != nil {
		t.Fatalf("create user %d: %v", id, err)
	}
}

func mustAccessToken(t *testing.T, s *Store, user store.TokenUser, label string) []byte {
	t.Helper()
	hash := accessHash(label)
	if _, err := s.CreateAPIToken(context.Background(), user, store.APIToken{
		Name: label, TokenHash: hash,
	}); err != nil {
		t.Fatalf("create token %s: %v", label, err)
	}
	return hash
}

func TestProjectGrantLifecycleAndCascades(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	mustProject(t, s, 43)
	mustAccessUser(t, s, 7, "alice", "admin")
	mustAccessUser(t, s, 8, "bob", "viewer")
	mustAccessUser(t, s, 9, "carol", "viewer")
	mustAccessUser(t, s, 10, "dave", "viewer")

	if _, err := s.GetProjectAccess(ctx, 7, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no grant = %v, want not found", err)
	}
	if _, err := s.GetProjectAccess(ctx, 999, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing user access = %v, want not found", err)
	}
	if _, err := s.GetProjectAccess(ctx, 7, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing project access = %v, want not found", err)
	}
	if err := s.SetProjectGrant(ctx, 42, 7, "admin"); err == nil {
		t.Fatal("invalid role accepted")
	}
	if err := s.SetProjectGrant(ctx, 0, 7, store.ProjectAccessRead); err == nil {
		t.Fatal("zero project id accepted")
	}
	if err := s.SetProjectGrant(ctx, 42, 7, ""); err == nil {
		t.Fatal("empty role accepted")
	}
	if err := s.SetProjectGrant(ctx, 999, 7, store.ProjectAccessRead); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing project grant = %v, want not found", err)
	}
	if err := s.SetProjectGrant(ctx, 42, 999, store.ProjectAccessRead); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing user grant = %v, want not found", err)
	}

	if err := s.SetProjectGrant(ctx, 42, 7, store.ProjectAccessRead); err != nil {
		t.Fatalf("SetProjectGrant read: %v", err)
	}
	role, err := s.GetProjectAccess(ctx, 7, 42)
	if err != nil || role != store.ProjectAccessRead {
		t.Fatalf("GetProjectAccess = %q, %v", role, err)
	}
	grants, next, err := s.ListProjectGrants(ctx, 42, "", 10)
	if err != nil || next != "" || len(grants) != 1 {
		t.Fatalf("ListProjectGrants = %+v, %q, %v", grants, next, err)
	}
	if grants[0].ProjectID != 42 || grants[0].UserID != 7 ||
		grants[0].Role != store.ProjectAccessRead || grants[0].Username != "alice" {
		t.Fatalf("grant = %+v", grants[0])
	}
	if err := s.SetProjectGrant(ctx, 42, 7, store.ProjectAccessManage); err != nil {
		t.Fatalf("SetProjectGrant manage: %v", err)
	}
	if role, err := s.GetProjectAccess(ctx, 7, 42); err != nil || role != store.ProjectAccessManage {
		t.Fatalf("updated role = %q, %v", role, err)
	}

	if err := s.SetProjectGrant(ctx, 42, 8, store.ProjectAccessRead); err != nil {
		t.Fatalf("second grant: %v", err)
	}
	if err := s.SetProjectGrant(ctx, 42, 9, store.ProjectAccessRead); err != nil {
		t.Fatalf("third grant: %v", err)
	}
	var seen []int64
	cursor := ""
	for {
		page, next, err := s.ListProjectGrants(ctx, 42, cursor, 2)
		if err != nil {
			t.Fatalf("ListProjectGrants page: %v", err)
		}
		for _, grant := range page {
			seen = append(seen, grant.UserID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if fmt.Sprint(seen) != "[7 8 9]" {
		t.Fatalf("paged grants = %v, want [7 8 9]", seen)
	}
	if _, _, err := s.ListProjectGrants(ctx, 42, "not-a-cursor", 2); !errors.Is(err, store.ErrInvalidCursor) {
		t.Fatalf("bad cursor = %v", err)
	}
	if _, _, err := s.ListProjectGrants(ctx, 999, "", 2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing project list = %v", err)
	}

	if err := s.DeleteProjectGrant(ctx, 42, 9); err != nil {
		t.Fatalf("DeleteProjectGrant: %v", err)
	}
	if _, err := s.GetProjectAccess(ctx, 9, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted grant = %v", err)
	}
	if err := s.DeleteProjectGrant(ctx, 42, 9); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}

	if err := s.SetProjectGrant(ctx, 43, 8, store.ProjectAccessManage); err != nil {
		t.Fatalf("grant on project 43: %v", err)
	}
	if err := s.DeleteProject(ctx, 42); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM project_grants WHERE project_id = 42`); got != 0 {
		t.Fatalf("project grants after project delete = %d", got)
	}
	if _, _, err := s.ListProjectGrants(ctx, 42, "", 10); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("list deleted project grants = %v", err)
	}

	if _, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = 8`); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM project_grants WHERE user_id = 8`); got != 0 {
		t.Fatalf("user grants after user delete = %d", got)
	}
	if _, err := s.GetUser(ctx, 8); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted user = %v", err)
	}
}

func TestUsersDisabledAndCredentialRevocation(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	mustAccessUser(t, s, 7, "alice", "admin")
	mustAccessUser(t, s, 8, "bob", "viewer")
	mustAccessUser(t, s, 9, "carol", "viewer")
	if err := s.SetProjectGrant(ctx, 42, 7, store.ProjectAccessRead); err != nil {
		t.Fatalf("grant before disable: %v", err)
	}

	page, next, err := s.ListUsers(ctx, "", 2)
	if err != nil || next == "" || len(page) != 2 || page[0].ID != 7 || page[1].ID != 8 {
		t.Fatalf("first users page = %+v, %q, %v", page, next, err)
	}
	page, next, err = s.ListUsers(ctx, next, 2)
	if err != nil || next != "" || len(page) != 1 || page[0].ID != 9 {
		t.Fatalf("second users page = %+v, %q, %v", page, next, err)
	}
	if _, _, err := s.ListUsers(ctx, "bad", 2); !errors.Is(err, store.ErrInvalidCursor) {
		t.Fatalf("bad users cursor = %v", err)
	}
	user, err := s.GetUser(ctx, 7)
	if err != nil || user.Username != "alice" || user.Name != "alice" ||
		user.Disabled || user.LastLoginAt == nil {
		t.Fatalf("GetUser = %+v, %v", user, err)
	}
	if _, err := s.GetUser(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing user = %v", err)
	}

	sessionToken := "pre-disable-session"
	if err := s.CreateSession(ctx, store.Session{
		IDHash: accessHash(sessionToken), UserID: 7, Username: "alice", Name: "Alice",
		Role: "admin", CreatedAt: s.now().UTC(), ExpiresAt: s.now().UTC().Add(time.Hour),
	}, store.SessionLimits{}); err != nil {
		t.Fatalf("pre-disable session: %v", err)
	}
	tokenHash := mustAccessToken(t, s, store.TokenUser{ID: 7, Username: "alice", Name: "Alice", Role: "admin"}, "pre-disable-token")
	otherToken := mustAccessToken(t, s, store.TokenUser{ID: 8, Username: "bob", Name: "Bob", Role: "viewer"}, "other-token")

	if err := s.SetUserDisabled(ctx, 7, true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	user, err = s.GetUser(ctx, 7)
	if err != nil || !user.Disabled {
		t.Fatalf("disabled user = %+v, %v", user, err)
	}
	if _, err := s.GetSession(ctx, accessHash(sessionToken), s.now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("disabled session = %v", err)
	}
	if _, _, err := s.FindAPITokenByHash(ctx, tokenHash, s.now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("disabled token = %v", err)
	}
	if _, _, err := s.FindAPITokenByHash(ctx, otherToken, s.now()); err != nil {
		t.Fatalf("other user's token: %v", err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM sessions WHERE user_id = 7`); got != 0 {
		t.Fatalf("sessions after disable = %d", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM api_tokens WHERE user_id = 7`); got != 0 {
		t.Fatalf("api tokens after disable = %d", got)
	}
	if _, err := s.GetProjectAccess(ctx, 7, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("disabled user access = %v", err)
	}

	if err := s.CreateSession(ctx, store.Session{
		IDHash: accessHash("rejected-session"), UserID: 7, Username: "alice", Name: "Alice",
		Role: "admin", CreatedAt: s.now().UTC(), ExpiresAt: s.now().UTC().Add(time.Hour),
	}, store.SessionLimits{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session for disabled user = %v", err)
	}
	if _, err := s.CreateAPIToken(ctx, store.TokenUser{ID: 7, Username: "alice", Name: "Alice", Role: "admin"},
		store.APIToken{Name: "rejected", TokenHash: accessHash("rejected-token")}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("token for disabled user = %v", err)
	}
	user, err = s.GetUser(ctx, 7)
	if err != nil || !user.Disabled {
		t.Fatalf("disabled flag after rejected upsert = %+v, %v", user, err)
	}

	if err := s.SetUserDisabled(ctx, 7, false); err != nil {
		t.Fatalf("enable user: %v", err)
	}
	if role, err := s.GetProjectAccess(ctx, 7, 42); err != nil || role != store.ProjectAccessRead {
		t.Fatalf("grant after enable = %q, %v", role, err)
	}
	if _, err := s.GetSession(ctx, accessHash(sessionToken), s.now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old session restored = %v", err)
	}
	if _, _, err := s.FindAPITokenByHash(ctx, tokenHash, s.now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old token restored = %v", err)
	}
	if err := s.CreateSession(ctx, store.Session{
		IDHash: accessHash("fresh-session"), UserID: 7, Username: "alice", Name: "Alice",
		Role: "admin", CreatedAt: s.now().UTC(), ExpiresAt: s.now().UTC().Add(time.Hour),
	}, store.SessionLimits{}); err != nil {
		t.Fatalf("enabled session: %v", err)
	}
	freshToken := mustAccessToken(t, s, store.TokenUser{ID: 7, Username: "alice", Name: "Alice", Role: "admin"}, "fresh-token")
	if _, err := s.GetSession(ctx, accessHash("fresh-session"), s.now()); err != nil {
		t.Fatalf("fresh session: %v", err)
	}
	if _, _, err := s.FindAPITokenByHash(ctx, freshToken, s.now()); err != nil {
		t.Fatalf("fresh token: %v", err)
	}

	if err := s.RevokeUserCredentials(ctx, 7); err != nil {
		t.Fatalf("RevokeUserCredentials: %v", err)
	}
	if _, err := s.GetSession(ctx, accessHash("fresh-session"), s.now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked session = %v", err)
	}
	if _, _, err := s.FindAPITokenByHash(ctx, freshToken, s.now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked token = %v", err)
	}
	if _, err := s.GetUser(ctx, 7); err != nil {
		t.Fatalf("revoke deleted user: %v", err)
	}
	if err := s.RevokeUserCredentials(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoke missing user = %v", err)
	}
	if err := s.SetUserDisabled(ctx, 999, true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("disable missing user = %v", err)
	}
}

func TestProjectAccessFiltersQueryBeforePaging(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)
	mustProject(t, s, 43)
	mustAccessUser(t, s, 7, "alice", "viewer")
	mustAccessUser(t, s, 8, "bob", "viewer")
	if err := s.SetProjectGrant(ctx, 42, 7, store.ProjectAccessRead); err != nil {
		t.Fatalf("grant user 7: %v", err)
	}
	if err := s.SetProjectGrant(ctx, 43, 8, store.ProjectAccessManage); err != nil {
		t.Fatalf("grant user 8: %v", err)
	}

	base := clock.Now().UTC()
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("wf-42-%d", i)
		rev := mustCreateWorkflow(t, s, id, reviewMeta(42, int64(i+1), id), base.Add(time.Duration(i)*time.Minute))
		_ = rev
		if i == 0 {
			if err := s.ReplaceFindings(ctx, id, []store.Finding{{
				ID: "finding-42", OriginNode: "a", OriginIndex: 0, Severity: "P1",
				Category: "logic", Title: "t", Body: "b", Decision: "publish",
			}}); err != nil {
				t.Fatalf("findings 42: %v", err)
			}
		}
	}
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("wf-43-%d", i)
		mustCreateWorkflow(t, s, id, reviewMeta(43, int64(i+1), id), base.Add(time.Duration(i)*time.Minute))
		if i == 0 {
			if err := s.ReplaceFindings(ctx, "wf-43-0", []store.Finding{{
				ID: "finding-43", OriginNode: "a", OriginIndex: 0, Severity: "P1",
				Category: "logic", Title: "t", Body: "b", Decision: "publish",
			}}); err != nil {
				t.Fatalf("findings 43: %v", err)
			}
		}
	}
	for _, call := range []struct {
		id, workflow, provider string
	}{
		{"call-42", "wf-42-0", "anthropic"},
		{"call-43", "wf-43-0", "openai"},
	} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO llm_calls
			(id, workflow_id, node_id, turn, provider, model, input_tokens,
			 output_tokens, cache_read, cache_write, latency_ms, created_at)
			VALUES (?, ?, 'n', 0, ?, 'm', 1, 1, 0, 0, 1, ?)`,
			call.id, call.workflow, call.provider, formatTime(base)); err != nil {
			t.Fatalf("insert llm call %s: %v", call.id, err)
		}
	}
	// Started nodes and attributed findings give every stats branch data.
	for _, node := range []struct {
		workflow, provider string
	}{
		{"wf-42-0", "anthropic"}, {"wf-43-0", "openai"},
	} {
		if _, err := s.db.ExecContext(ctx, `UPDATE workflow_nodes
			SET provider = ?, model = 'm', started_at = ?, finished_at = ?
			WHERE workflow_id = ? AND id = 'a'`,
			node.provider, formatTime(base), formatTime(base.Add(time.Second)), node.workflow); err != nil {
			t.Fatalf("update node %s: %v", node.workflow, err)
		}
	}
	for _, finding := range []struct {
		id, provider string
	}{
		{"finding-42", "anthropic"}, {"finding-43", "openai"},
	} {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE findings SET origin_provider = ? WHERE id = ?`, finding.provider, finding.id); err != nil {
			t.Fatalf("update finding %s: %v", finding.id, err)
		}
	}
	for _, feedback := range []struct {
		id      string
		project int64
	}{
		{"feedback-42", 42}, {"feedback-43", 43},
	} {
		if _, err := s.UpsertFeedback(ctx, store.Feedback{
			ID: feedback.id, ProjectID: feedback.project, MRIID: 1,
			PublicationID: "pub-" + feedback.id, Signal: "thumbsup", ObservedAt: base,
		}); err != nil {
			t.Fatalf("insert feedback %s: %v", feedback.id, err)
		}
	}

	access := int64(7)
	var workflows []store.Workflow
	cursor := ""
	for {
		page, next, err := s.ListWorkflows(ctx, store.WorkflowFilter{AccessUserID: &access}, cursor, 1)
		if err != nil {
			t.Fatalf("ListWorkflows: %v", err)
		}
		workflows = append(workflows, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(workflows) != 3 {
		t.Fatalf("accessible workflows = %d, want 3", len(workflows))
	}
	for _, wf := range workflows {
		if wf.ProjectID != 42 {
			t.Fatalf("hidden project workflow leaked: %+v", wf)
		}
	}
	zero := int64(0)
	if page, _, err := s.ListWorkflows(ctx, store.WorkflowFilter{AccessUserID: &zero}, "", 10); err != nil || len(page) != 0 {
		t.Fatalf("zero access workflows = %+v, %v", page, err)
	}
	if page, _, err := s.ListWorkflows(ctx, store.WorkflowFilter{}, "", 10); err != nil || len(page) != 5 {
		t.Fatalf("unrestricted workflows = %d, %v", len(page), err)
	}

	projects, err := s.ListProjectsFiltered(ctx, store.ProjectFilter{AccessUserID: &access})
	if err != nil || len(projects) != 1 || projects[0].ID != 42 {
		t.Fatalf("filtered projects = %+v, %v", projects, err)
	}
	if page, _, err := s.ListProjectsFilteredPage(ctx, store.ProjectFilter{AccessUserID: &zero}, "", 10); err != nil || len(page) != 0 {
		t.Fatalf("zero access projects = %+v, %v", page, err)
	}

	stats, err := s.Stats(ctx, store.StatsFilter{AccessUserID: &access})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Runs != 3 || stats.Published != 1 || len(stats.ByProject) != 1 ||
		stats.ByProject[0].ProjectID != 42 || stats.ByProject[0].Published != 1 {
		t.Fatalf("filtered stats = %+v", stats)
	}
	if len(stats.ByModel) != 1 || stats.ByModel[0].Provider != "anthropic" {
		t.Fatalf("filtered models = %+v", stats.ByModel)
	}
	if stats.Decisions["publish"] != 1 {
		t.Fatalf("filtered decisions = %+v", stats.Decisions)
	}
	if len(stats.ByProvider) != 1 || stats.ByProvider[0].Provider != "anthropic" ||
		stats.ByProvider[0].Runs != 1 {
		t.Fatalf("filtered providers = %+v", stats.ByProvider)
	}
	if len(stats.ByBlock) != 1 || stats.ByBlock[0].Block != "code-review" {
		t.Fatalf("filtered blocks = %+v", stats.ByBlock)
	}
	if len(stats.QualityByProvider) != 1 || stats.QualityByProvider[0].Provider != "anthropic" ||
		stats.QualityByProvider[0].Proposed != 1 {
		t.Fatalf("filtered quality = %+v", stats.QualityByProvider)
	}
	if len(stats.Trend) != 1 || stats.Trend[0].Runs != 3 {
		t.Fatalf("filtered trend = %+v", stats.Trend)
	}
	if stats.ByProject[0].Trend == nil || len(stats.ByProject[0].Trend) != 1 ||
		stats.ByProject[0].Trend[0].Runs != 3 {
		t.Fatalf("filtered project trend = %+v", stats.ByProject[0].Trend)
	}
	if stats.Feedback == nil || stats.Feedback.Total != 1 {
		t.Fatalf("filtered stats feedback = %+v", stats.Feedback)
	}
	other := int64(43)
	empty, err := s.Stats(ctx, store.StatsFilter{AccessUserID: &access, ProjectID: &other})
	if err != nil || empty.Runs != 0 || len(empty.ByProject) != 0 || len(empty.ByModel) != 0 ||
		len(empty.ByProvider) != 0 || len(empty.ByBlock) != 0 || len(empty.QualityByProvider) != 0 ||
		len(empty.Trend) != 0 || len(empty.Decisions) != 0 || empty.Feedback == nil || empty.Feedback.Total != 0 {
		t.Fatalf("cross-project stats = %+v, %v", empty, err)
	}
	denied, err := s.Stats(ctx, store.StatsFilter{AccessUserID: &zero})
	if err != nil || denied.Runs != 0 || len(denied.ByProject) != 0 || len(denied.ByModel) != 0 ||
		len(denied.ByProvider) != 0 || len(denied.ByBlock) != 0 || len(denied.QualityByProvider) != 0 ||
		len(denied.Trend) != 0 || len(denied.Decisions) != 0 || denied.Feedback == nil || denied.Feedback.Total != 0 {
		t.Fatalf("denied stats = %+v, %v", denied, err)
	}

	feedback, _, err := s.ListFeedback(ctx, store.FeedbackFilter{AccessUserID: &access})
	if err != nil || len(feedback) != 1 || feedback[0].ProjectID != 42 {
		t.Fatalf("filtered feedback = %+v, %v", feedback, err)
	}
	totals, err := s.FeedbackTotals(ctx, store.FeedbackFilter{AccessUserID: &access})
	if err != nil || totals.Total != 1 {
		t.Fatalf("filtered feedback totals = %+v, %v", totals, err)
	}
	deniedTotals, err := s.FeedbackTotals(ctx, store.FeedbackFilter{AccessUserID: &zero})
	if err != nil || deniedTotals.Total != 0 {
		t.Fatalf("denied feedback totals = %+v, %v", deniedTotals, err)
	}
}

func TestDisableUserSerializesWithCredentialIssuance(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustAccessUser(t, s, 7, "alice", "admin")
	mustAccessUser(t, s, 8, "bob", "viewer")
	original := mustAccessToken(t, s, store.TokenUser{ID: 7, Username: "alice", Name: "Alice", Role: "admin"}, "original")
	mustAccessToken(t, s, store.TokenUser{ID: 8, Username: "bob", Name: "Bob", Role: "viewer"}, "unaffected")

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- s.SetUserDisabled(ctx, 7, true)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			hash := accessHash(fmt.Sprintf("racing-token-%d", i))
			_, err := s.CreateAPIToken(ctx, store.TokenUser{ID: 7, Username: "alice", Name: "Alice", Role: "admin"},
				store.APIToken{Name: "racing", TokenHash: hash})
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				errs <- err
				return
			}
			err = s.CreateSession(ctx, store.Session{
				IDHash: accessHash(fmt.Sprintf("racing-session-%d", i)), UserID: 7,
				Username: "alice", Name: "Alice", Role: "admin",
				CreatedAt: s.now().UTC(), ExpiresAt: s.now().UTC().Add(time.Hour),
			}, store.SessionLimits{})
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				errs <- err
				return
			}
		}
		errs <- nil
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent operation: %v", err)
		}
	}

	if got := rowCount(t, s, `SELECT COUNT(*) FROM api_tokens WHERE user_id = 7`); got != 0 {
		t.Fatalf("api tokens after concurrent disable = %d", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM sessions WHERE user_id = 7`); got != 0 {
		t.Fatalf("sessions after concurrent disable = %d", got)
	}
	if _, _, err := s.FindAPITokenByHash(ctx, original, s.now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("original token after disable = %v", err)
	}
	for i := 0; i < 20; i++ {
		hash := accessHash(fmt.Sprintf("racing-token-%d", i))
		if _, _, err := s.FindAPITokenByHash(ctx, hash, s.now()); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("racing token %d usable = %v", i, err)
		}
		if _, err := s.GetSession(ctx, accessHash(fmt.Sprintf("racing-session-%d", i)), s.now()); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("racing session %d usable = %v", i, err)
		}
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM api_tokens WHERE user_id = 8`); got != 1 {
		t.Fatalf("other user's tokens = %d, want 1", got)
	}
}

func TestProjectAccessMigrationFreshAndLegacy(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustAccessUser(t, s, 7, "alice", "admin")
	user, err := s.GetUser(ctx, 7)
	if err != nil || user.Disabled {
		t.Fatalf("fresh user = %+v, %v", user, err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM project_grants`); got != 0 {
		t.Fatalf("fresh grants = %d", got)
	}

	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", withPragmas(path))
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	applied := []string{"0001_init.sql", "0003_review_state.sql"}
	for _, name := range applied {
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := legacy.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	stamp := "2026-10-20T12:00:00.000000000Z"
	if _, err := legacy.Exec(`INSERT INTO users (id, username, name, role, last_login_at)
		VALUES (7, 'alice', 'Alice', 'admin', ?)`, stamp); err != nil {
		t.Fatalf("insert legacy user: %v", err)
	}
	for _, name := range applied {
		if _, err := legacy.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			migrationVersionForTest(t, name), name, stamp); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err = Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("Open legacy db: %v", err)
	}
	defer s.Close()
	user, err = s.GetUser(ctx, 7)
	if err != nil || user.Disabled {
		t.Fatalf("legacy user = %+v, %v", user, err)
	}
	legacyProject := store.Project{
		ID: 42, PathWithNamespace: "group/legacy", WebURL: "https://gitlab.example.com/group/legacy",
		DefaultBranch: "main", TokenEnc: []byte{1}, WebhookSecretEnc: []byte{2},
	}
	if _, err := s.RegisterProject(ctx, legacyProject); err != nil {
		t.Fatalf("register legacy project: %v", err)
	}
	if _, err := s.GetProjectAccess(ctx, 7, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("legacy implicit grant = %v", err)
	}
	if err := s.SetProjectGrant(ctx, 42, 7, store.ProjectAccessRead); err != nil {
		t.Fatalf("legacy grant: %v", err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM schema_migrations`); got != migrationCount(t) {
		t.Fatalf("schema_migrations = %d, want %d", got, migrationCount(t))
	}
}
