package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
)

func hasWarning(warnings []ProjectWarning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code && strings.TrimSpace(warning.Message) != "" {
			return true
		}
	}
	return false
}

func TestValidateWarnsWhenTokenCannotManageWebhooks(t *testing.T) {
	for _, tc := range []struct {
		access int
		warn   bool
	}{{20, true}, {30, true}, {40, false}, {50, false}} {
		api := defaultFakeGitLab()
		api.access = tc.access
		probe := func(context.Context, string, string) (string, error) { return sha40, nil }
		service := newValidationService(t, openTestStore(t), api, "https://gitlab.example.com", probe)
		result, err := service.Validate(context.Background(), ValidateProjectInput{Project: "42", AccessToken: "glpat-project-token"})
		if err != nil {
			t.Fatalf("access %d: Validate: %v", tc.access, err)
		}
		if got := hasWarning(result.Warnings, WarningWebhookRequiresMaintainer); got != tc.warn {
			t.Fatalf("access %d: warnings = %+v, want maintainer warning %v", tc.access, result.Warnings, tc.warn)
		}
	}
}

func TestRotateTokenChecksRoleAndWarnsBelowMaintainer(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := defaultFakeGitLab()
	api.access = 40
	service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")
	if _, err := service.Register(ctx, RegisterInput{Project: "42", AccessToken: "glpat-project-token"}); err != nil {
		t.Fatal(err)
	}

	rotation, err := service.RotateToken(ctx, 42, "glpat-maintainer-token")
	if err != nil {
		t.Fatalf("RotateToken(maintainer): %v", err)
	}
	if rotation.Project == nil || rotation.Project.ID != 42 || len(rotation.Warnings) != 0 {
		t.Fatalf("maintainer rotation = %+v", rotation)
	}

	api.access = 30
	rotation, err = service.RotateToken(ctx, 42, "glpat-developer-token")
	if err != nil {
		t.Fatalf("RotateToken(developer): %v", err)
	}
	if !hasWarning(rotation.Warnings, WarningWebhookRequiresMaintainer) {
		t.Fatalf("developer rotation warnings = %+v, want maintainer warning", rotation.Warnings)
	}

	api.access = 10
	if _, err := service.RotateToken(ctx, 42, "glpat-guest-token"); err == nil {
		t.Fatal("RotateToken accepted a Guest token")
	} else {
		var appErr *Error
		if !errors.As(err, &appErr) || appErr.Code != "validation_failed" {
			t.Fatalf("guest rotation error = %v, want validation_failed", err)
		}
	}
}

func TestWebhookFailureNamesTokenOrRoleCause(t *testing.T) {
	cases := []struct {
		status int
		field  string
		stored string
	}{
		{401, "token", "project token rejected"},
		{403, "access_level", "Maintainer role required"},
	}
	for _, tc := range cases {
		ctx := context.Background()
		st := openTestStore(t)
		api := defaultFakeGitLab()
		service := newProjectService(t, st, api, newTestCipher(t), "https://review.example.com")
		if _, err := service.Register(ctx, RegisterInput{Project: "42", AccessToken: "glpat-project-token"}); err != nil {
			t.Fatal(err)
		}
		api.listHooksErr = &gitlab.Error{Op: "list_hooks", Status: tc.status}
		_, err := service.CreateWebhook(ctx, 42)
		var appErr *Error
		if !errors.As(err, &appErr) || appErr.Code != "gitlab_rejected" || appErr.Upstream != tc.status {
			t.Fatalf("status %d: error = %#v, want gitlab_rejected", tc.status, err)
		}
		if len(appErr.Details) != 1 || appErr.Details[0].Field != tc.field || appErr.Details[0].Message == "" {
			t.Fatalf("status %d: details = %+v, want one %q detail", tc.status, appErr.Details, tc.field)
		}
		saved, err := st.GetProject(ctx, 42)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(saved.WebhookError, tc.stored) {
			t.Fatalf("status %d: stored webhook error = %q, want %q", tc.status, saved.WebhookError, tc.stored)
		}
	}
}

func TestAutoWebhookRegistrationNamesRoleCause(t *testing.T) {
	ctx := context.Background()
	api := defaultFakeGitLab()
	api.hookErr = &gitlab.Error{Op: "create_hook", Status: 403}
	service := newProjectService(t, openTestStore(t), api, newTestCipher(t), "https://review.example.com")
	_, err := service.Register(ctx, RegisterInput{Project: "42", AccessToken: "glpat-project-token", AutoWebhook: true})
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "gitlab_rejected" || len(appErr.Details) != 1 || appErr.Details[0].Field != "access_level" {
		t.Fatalf("auto webhook registration error = %#v, want access_level detail", err)
	}
}
