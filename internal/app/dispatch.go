package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

// DispatchStore is the persistence surface used by the manual review,
// republish and workspace-refresh endpoints.
type DispatchStore interface {
	GetProject(ctx context.Context, id int64) (*store.Project, error)
	GetWorkflow(ctx context.Context, id string) (*store.Workflow, error)
	EnqueueReviewLatest(ctx context.Context, command store.ReviewCommand) (store.ReviewEnqueueResult, error)
	EnqueueLatest(ctx context.Context, job store.Job) (string, bool, error)
}

// DispatchGitLab is the upstream surface needed to resolve a merge request
// before a manual review is queued.
type DispatchGitLab interface {
	GetMergeRequest(ctx context.Context, token string, projectID, iid int64) (*gitlab.MergeRequest, error)
}

// Dispatcher implements the admin review and workspace actions. It only
// enqueues durable jobs; execution stays with the registered job handlers.
type Dispatcher struct {
	store     DispatchStore
	workspace *workspace.Manager
	gitlab    DispatchGitLab
	cipher    *secret.Cipher
	profiles  map[string]config.ProfileConfig
	logger    *slog.Logger
}

// DispatcherConfig configures Dispatcher.
type DispatcherConfig struct {
	Store     DispatchStore
	Workspace *workspace.Manager
	GitLab    DispatchGitLab
	Cipher    *secret.Cipher
	Profiles  map[string]config.ProfileConfig
	Logger    *slog.Logger
}

type dispatchAuthorizationKey struct{}

// WithDispatchAuthorization binds an HTTP caller's project check to dispatch.
// Contexts without this value are reserved for trusted internal callers.
func WithDispatchAuthorization(ctx context.Context, authorize func(context.Context, int64) error) context.Context {
	return context.WithValue(ctx, dispatchAuthorizationKey{}, authorize)
}

func authorizeDispatch(ctx context.Context, projectID int64) error {
	value := ctx.Value(dispatchAuthorizationKey{})
	if value == nil {
		return nil
	}
	authorize, ok := value.(func(context.Context, int64) error)
	if !ok || authorize == nil {
		return store.ErrNotFound
	}
	return authorize(ctx, projectID)
}

// NewDispatcher validates dependencies and builds the dispatcher.
func NewDispatcher(cfg DispatcherConfig) (*Dispatcher, error) {
	if cfg.Store == nil || cfg.GitLab == nil || cfg.Cipher == nil || cfg.Workspace == nil {
		return nil, errors.New("app: dispatcher store, GitLab client, and cipher are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Dispatcher{
		store:     cfg.Store,
		workspace: cfg.Workspace,
		gitlab:    cfg.GitLab,
		cipher:    cfg.Cipher,
		profiles:  cfg.Profiles,
		logger:    logger,
	}, nil
}

// StartReview resolves the merge request head, validates that it is open,
// and enqueues review.run. The store assigns the generation in the same
// transaction as the queue write.
func (d *Dispatcher) StartReview(ctx context.Context, projectID, mrIID int64, profile string, full bool) (string, error) {
	if projectID <= 0 || mrIID <= 0 {
		return "", validationError("project id and merge request iid must be positive",
			Detail{Field: "id", Message: "must be a positive integer"})
	}
	unlock, err := d.workspace.Get(projectID).RLock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := authorizeDispatch(ctx, projectID); err != nil {
		return "", err
	}
	project, err := d.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", notFoundError("project not found")
		}
		return "", err
	}
	if !project.Enabled {
		return "", conflictError("project is disabled")
	}
	token, err := d.cipher.Open(project.TokenEnc, secret.TokenAAD(project.ID))
	if err != nil {
		return "", internalError()
	}
	mr, err := d.gitlab.GetMergeRequest(ctx, string(token), projectID, mrIID)
	if err != nil {
		if gitlab.IsNotFound(err) {
			return "", notFoundError("merge request not found")
		}
		return "", upstreamError(gitlab.StatusOf(err))
	}
	if mr == nil || !isOpenState(mr.State) {
		state := mrState(mr)
		if state == "merged" {
			return "", conflictError("merge request is merged")
		}
		return "", conflictError("merge request is closed")
	}
	head := strings.ToLower(strings.TrimSpace(mr.SHA))
	if !shaRE.MatchString(head) {
		return "", &Error{Code: "gitlab_rejected", Message: "merge request head is unavailable"}
	}
	requested := strings.TrimSpace(profile)
	payload, err := json.Marshal(map[string]any{
		"profile":          d.resolveProfile(project, requested),
		"profile_explicit": requested != "",
		"trigger":          "manual",
		"full":             full,
	})
	if err != nil {
		return "", internalError()
	}
	result, err := d.store.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID:   project.ID,
		MRIID:       mrIID,
		HeadSHA:     head,
		Payload:     payload,
		MaxAttempts: 3,
	})
	if err != nil {
		return "", d.storeError(err)
	}
	return result.JobID, nil
}

// CancelReview enqueues review.cancel for the workflow's merge request.
func (d *Dispatcher) CancelReview(ctx context.Context, workflowID string) (string, error) {
	workflow, unlock, err := d.lockReviewWorkflow(ctx, workflowID)
	if err != nil {
		return "", err
	}
	defer unlock()
	payload, err := json.Marshal(map[string]any{"trigger": "manual"})
	if err != nil {
		return "", internalError()
	}
	result, err := d.store.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID:   workflow.ProjectID,
		MRIID:       *workflow.MRIID,
		Cancel:      true,
		Payload:     payload,
		MaxAttempts: 3,
	})
	if err != nil {
		return "", d.storeError(err)
	}
	return result.JobID, nil
}

// RepublishReview enqueues review.publish so the worker re-runs the publisher
// with its normal retry and stale-fingerprint semantics.
func (d *Dispatcher) RepublishReview(ctx context.Context, workflowID string) (string, error) {
	workflow, unlock, err := d.lockReviewWorkflow(ctx, workflowID)
	if err != nil {
		return "", err
	}
	defer unlock()
	payload, err := json.Marshal(map[string]any{"workflow_id": workflow.ID})
	if err != nil {
		return "", internalError()
	}
	jobID, _, err := d.store.EnqueueLatest(ctx, store.Job{
		Kind:        "review.publish",
		Key:         "review.publish:" + workflow.ID,
		Payload:     payload,
		MaxAttempts: 3,
	})
	if err != nil {
		return "", d.storeError(err)
	}
	return jobID, nil
}

// RefreshWorkspace enqueues workspace.refresh for the project default branch.
func (d *Dispatcher) RefreshWorkspace(ctx context.Context, projectID int64, regenerateContext, reindex bool) (string, error) {
	if projectID <= 0 {
		return "", validationError("project id must be positive", Detail{Field: "id", Message: "must be a positive integer"})
	}
	unlock, err := d.workspace.Get(projectID).RLock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := authorizeDispatch(ctx, projectID); err != nil {
		return "", err
	}
	project, err := d.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", notFoundError("project not found")
		}
		return "", err
	}
	branch := strings.TrimSpace(project.DefaultBranch)
	if branch == "" {
		return "", internalError()
	}
	payload, err := json.Marshal(map[string]any{
		"project_id":         project.ID,
		"ref":                "refs/heads/" + branch,
		"branch":             branch,
		"trigger":            "manual",
		"regenerate_context": regenerateContext,
		"reindex":            reindex,
	})
	if err != nil {
		return "", internalError()
	}
	jobID, _, err := d.store.EnqueueLatest(ctx, store.Job{
		Kind:        "workspace.refresh",
		Key:         fmt.Sprintf("ws:%d", project.ID),
		Payload:     payload,
		MaxAttempts: 3,
	})
	if err != nil {
		return "", d.storeError(err)
	}
	return jobID, nil
}

// lockReviewWorkflow rechecks the original workflow after entering its project.
func (d *Dispatcher) lockReviewWorkflow(ctx context.Context, id string) (*store.Workflow, func(), error) {
	workflow, err := d.reviewWorkflow(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	ws := d.workspace.Get(workflow.ProjectID)
	if ws == nil {
		return nil, nil, notFoundError("project not found")
	}
	unlock, err := ws.RLock(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := authorizeDispatch(ctx, workflow.ProjectID); err != nil {
		unlock()
		return nil, nil, err
	}
	current, err := d.reviewWorkflow(ctx, id)
	if err == nil && current.ProjectID != workflow.ProjectID {
		err = notFoundError("review not found")
	}
	if err == nil {
		_, err = d.store.GetProject(ctx, workflow.ProjectID)
		if errors.Is(err, store.ErrNotFound) {
			err = notFoundError("project not found")
		}
	}
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return current, unlock, nil
}

func (d *Dispatcher) reviewWorkflow(ctx context.Context, workflowID string) (*store.Workflow, error) {
	if strings.TrimSpace(workflowID) == "" {
		return nil, notFoundError("review not found")
	}
	workflow, err := d.store.GetWorkflow(ctx, workflowID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundError("review not found")
		}
		return nil, err
	}
	if workflow.Kind != "review" || workflow.MRIID == nil || *workflow.MRIID <= 0 {
		return nil, conflictError("workflow is not a review")
	}
	return workflow, nil
}

func (d *Dispatcher) resolveProfile(project *store.Project, requested string) string {
	candidates := []string{requested, project.Profile, "default"}
	for _, name := range candidates {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := d.profiles[name]; ok {
			return name
		}
	}
	return "default"
}

func (d *Dispatcher) storeError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return notFoundError("not found")
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrSuperseded):
		return conflictError("conflict")
	default:
		return err
	}
}
