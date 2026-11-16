package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// registerSharedWithHook registers project 42 in shared-credential mode with
// an auto-created webhook, leaving a zero-length token sentinel in storage.
func registerSharedWithHook(t *testing.T, ctx context.Context, st *sqlite.Store, api *sharedBotAPI) (*ProjectService, *store.Project) {
	t.Helper()
	service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)
	if _, err := service.Register(ctx, RegisterInput{
		Project: "42", OwnerUserID: registrantUserID, AutoWebhook: true,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	return service, saved
}

func TestTokenRejectedTextsNameSharedCredential(t *testing.T) {
	sharedDetail, sharedNote := tokenRejectedTexts(true)
	for _, text := range []string{sharedDetail, sharedNote} {
		if !strings.Contains(text, "共享") {
			t.Fatalf("shared text does not name the shared credential: %q", text)
		}
		for _, forbidden := range []string{"项目访问令牌", "更换令牌", "project token"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("shared text contains %q: %q", forbidden, text)
			}
		}
	}
	for _, status := range []int{401, 403} {
		message := tokenRejectedHealthMessage(status, true)
		if !strings.Contains(message, "shared bot") || strings.Contains(message, "project token") {
			t.Fatalf("shared health text for %d = %q", status, message)
		}
	}
	projectMessage := tokenRejectedHealthMessage(401, false)
	if !strings.Contains(projectMessage, "project token") {
		t.Fatalf("project health text = %q", projectMessage)
	}
}

func TestMarkProjectTokenRejectedLeavesSharedProjectState(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	_, saved := registerSharedWithHook(t, ctx, st, api)
	service := &ReviewService{
		store:  st,
		tokens: newProjectTokenResolver(newTestCipher(t), sharedBotToken),
		logger: discardLogger(),
	}

	service.markProjectTokenRejected(ctx, 42, 401)
	after, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if after.WebhookState != saved.WebhookState || after.WebhookError != saved.WebhookError {
		t.Fatalf("shared project health changed: %+v -> %+v", saved, after)
	}
}

func TestMarkProjectTokenRejectedMarksProjectTokenProject(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	seedProject(t, st, cipher, 42, "whsec", "{}", "default")
	service := &ReviewService{
		store:  st,
		tokens: newProjectTokenResolver(cipher, sharedBotToken),
		logger: discardLogger(),
	}

	service.markProjectTokenRejected(ctx, 42, 403)
	after, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if after.WebhookState != store.WebhookStateFailed || after.WebhookError != tokenRejected403Message {
		t.Fatalf("project health = %q/%q", after.WebhookState, after.WebhookError)
	}
}

func TestWebhookFailureNamesSharedCredential(t *testing.T) {
	cases := []struct {
		status int
		field  string
	}{
		{status: 401, field: "shared_bot_credential"},
		{status: 403, field: "shared_bot_access"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d", tc.status), func(t *testing.T) {
			ctx := context.Background()
			st := openTestStore(t)
			api := newSharedBotAPI()
			configureSharedBot(api)
			service, _ := registerSharedWithHook(t, ctx, st, api)
			api.listHooksErr = &gitlab.Error{Op: "list_hooks", Status: tc.status}

			_, err := service.CreateWebhook(ctx, 42)
			var appErr *Error
			if !errors.As(err, &appErr) || appErr.Code != "gitlab_rejected" || appErr.Upstream != tc.status {
				t.Fatalf("error = %#v", err)
			}
			if len(appErr.Details) != 1 || appErr.Details[0].Field != tc.field {
				t.Fatalf("details = %+v, want field %q", appErr.Details, tc.field)
			}
			if strings.Contains(appErr.Details[0].Message, "project token") {
				t.Fatalf("shared failure tells the user to replace a project token: %q", appErr.Details[0].Message)
			}
			saved, err := st.GetProject(ctx, 42)
			if err != nil {
				t.Fatal(err)
			}
			if saved.WebhookState != store.WebhookStateFailed {
				t.Fatalf("state = %q", saved.WebhookState)
			}
			if !strings.Contains(saved.WebhookError, "shared bot") || strings.Contains(saved.WebhookError, "project token") {
				t.Fatalf("stored webhook error = %q", saved.WebhookError)
			}
		})
	}
}

func TestRegisterSharedCredentialRejectionNamesSharedCredential(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			ctx := context.Background()
			api := newSharedBotAPI()
			configureSharedBot(api)
			api.projectErr = &gitlab.Error{Op: "get_project", Status: status}
			service := newSharedBotProjectService(t, openTestStore(t), api, sharedBotToken, nil)

			_, err := service.Register(ctx, RegisterInput{Project: "42"})
			var appErr *Error
			if !errors.As(err, &appErr) || appErr.Code != "gitlab_rejected" {
				t.Fatalf("error = %#v", err)
			}
			if len(appErr.Details) != 1 || appErr.Details[0].Field != "shared_bot_credential" {
				t.Fatalf("details = %+v", appErr.Details)
			}
			if strings.Contains(appErr.Details[0].Message, "project token") {
				t.Fatalf("shared rejection mentions a project token: %q", appErr.Details[0].Message)
			}
			if api.callCount("GetMemberAccess") != 0 {
				t.Fatalf("membership checks ran after credential rejection: %v", api.tokenUses)
			}
		})
	}
}

func TestRegisterProjectLookupDenialIsIndistinguishable(t *testing.T) {
	for _, status := range []int{403, 404} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			ctx := context.Background()
			api := defaultFakeGitLab()
			api.projectErr = &gitlab.Error{Op: "get_project", Status: status}
			service := newProjectService(t, openTestStore(t), api, newTestCipher(t), "")

			_, err := service.Register(ctx, RegisterInput{Project: "42", AccessToken: "glpat-project-token"})
			assertAppError(t, err, "validation_failed", "project")
			if api.callCount("GetTokenInfo") != 0 {
				t.Fatal("token checks ran after a denied project lookup")
			}
		})
	}
}

// catchUpSharedAPI satisfies the catch-up scan surface while reusing the
// shared-bot fake for hook lookups.
type catchUpSharedAPI struct{ *sharedBotAPI }

func (f *catchUpSharedAPI) ListOpenMergeRequests(context.Context, string, int64, int, time.Time) ([]gitlab.MergeRequest, error) {
	return nil, nil
}

func TestReconcileHookHealthNamesSharedCredential(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	_, saved := registerSharedWithHook(t, ctx, st, api)
	if saved.HookID == nil {
		t.Fatal("shared project has no hook id")
	}
	api.listHooksErr = &gitlab.Error{Op: "list_hooks", Status: 401}
	service := &CatchUpService{
		store:    st,
		webhooks: &WebhookService{store: st},
		gitlab:   &catchUpSharedAPI{sharedBotAPI: api},
		cipher:   newTestCipher(t),
		tokens:   newProjectTokenResolver(newTestCipher(t), sharedBotToken),
		logger:   discardLogger(),
	}

	service.ReconcileHookHealth(ctx)
	after, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if after.WebhookState != store.WebhookStateFailed || after.WebhookError != webhookSharedTokenRejectedMessage {
		t.Fatalf("hook health = %q/%q", after.WebhookState, after.WebhookError)
	}
	if strings.Contains(after.WebhookError, "project token") {
		t.Fatalf("shared hook health mentions a project token: %q", after.WebhookError)
	}
}
