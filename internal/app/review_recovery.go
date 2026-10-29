package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// Recover rebuilds executors for unfinished review workflows and re-runs them,
// then publishes completed workflows whose publication is still pending.
func (s *ReviewService) Recover(ctx context.Context) error {
	var errs []error
	// Running nodes left by a crash must restart before Engine.Run sees them.
	if err := s.engine.Recover(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.settleOrphanDossiers(ctx); err != nil {
		errs = append(errs, err)
	}
statuses:
	for _, status := range []string{"running", "pending"} {
		cursor := ""
		for {
			if err := ctx.Err(); err != nil {
				errs = append(errs, err)
				break statuses
			}
			workflows, next, err := s.store.ListWorkflows(ctx, store.WorkflowFilter{Kind: "review", Status: status}, cursor, 50)
			if err != nil {
				errs = append(errs, err)
				break
			}
			if len(workflows) == 0 {
				break
			}
			for _, workflow := range workflows {
				if err := ctx.Err(); err != nil {
					errs = append(errs, err)
					break statuses
				}
				if err := s.recoverWorkflow(ctx, workflow); err != nil {
					errs = append(errs, fmt.Errorf("app: recover workflow %s: %w", workflow.ID, err))
				}
			}
			if next == "" || next == cursor {
				break
			}
			cursor = next
		}
	}
	if ctx.Err() == nil {
		if err := s.PublishPending(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if ctx.Err() == nil {
		if err := s.ReconcileSideEffects(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *ReviewService) settleOrphanDossiers(ctx context.Context) error {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, project := range projects {
		ws := s.workspace.Get(project.ID)
		if ws == nil {
			continue
		}
		unlock, err := ws.Lock(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		current, projectErr := s.store.GetProject(ctx, project.ID)
		state, stateErr := s.store.GetWorkspace(ctx, project.ID)
		if projectErr == nil && stateErr == nil && current != nil && current.ID == project.ID && state != nil && state.ProjectID == project.ID {
			if err := s.store.SettleOrphanDossiers(ctx, project.ID); err != nil {
				errs = append(errs, err)
			}
		} else {
			for _, err := range []error{projectErr, stateErr} {
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					errs = append(errs, err)
				}
			}
		}
		unlock()
	}
	return errors.Join(errs...)
}

func (s *ReviewService) recoverWorkflow(ctx context.Context, workflow store.Workflow) error {
	if workflow.MRIID == nil || s.publisher == nil {
		return nil
	}
	ws := s.workspace.Get(workflow.ProjectID)
	if ws == nil {
		return nil
	}
	unlock, err := ws.RLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := s.store.GetWorkflow(ctx, workflow.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if current == nil || current.ID != workflow.ID || current.ProjectID != workflow.ProjectID ||
		current.Kind != "review" || current.MRIID == nil || *current.MRIID != *workflow.MRIID ||
		(current.Status != "running" && current.Status != "pending") {
		return nil
	}
	workflow = *current
	project, token, err := s.projectToken(ctx, workflow.ProjectID)
	if err != nil {
		return err
	}
	record, err := s.store.GetWorkflowDossier(ctx, workflow.ID)
	if err != nil {
		return err
	}
	ownershipKey := strings.TrimSpace(record.JobID)
	if ownershipKey == "" {
		ownershipKey = workflow.ID
	}
	if !s.executors.Acquire(ownershipKey) {
		s.logger.Info("recovery skipped workflow owned by another handler", "workflow_id", workflow.ID)
		return nil
	}
	defer s.executors.Release(ownershipKey)
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		return s.cancelWorkspaceWorkflow(ctx, workflow.ID)
	}
	if err := review.ValidateDossier(&dossier); err != nil {
		return s.cancelWorkspaceWorkflow(ctx, workflow.ID)
	}
	if dossier.Project.ID != workflow.ProjectID || dossier.MR.IID != *workflow.MRIID {
		return s.cancelWorkflow(ctx, workflow.ID)
	}
	if _, err := s.currentControl(ctx, project.ID, *workflow.MRIID, record.Generation, dossier.DiffVersion.HeadSHA); err != nil {
		if IsPermanentJob(err) {
			if cancelErr := s.engine.Cancel(context.WithoutCancel(ctx), workflow.ID, "superseded"); cancelErr != nil &&
				!errors.Is(cancelErr, dag.ErrNotFound) {
				return cancelErr
			}
			return s.skipPublication(ctx, workflow.ID, reviewSupersededReason)
		}
		return err
	}
	if !validPublicationPolicy(&dossier) {
		if !dossier.WorkspaceProvenanceValid() {
			return s.cancelWorkspaceWorkflow(ctx, workflow.ID)
		}
		return s.cancelWorkflow(ctx, workflow.ID)
	}
	mr, _, err := s.collectFilterInputs(ctx, project, token, *workflow.MRIID, dossier.DiffVersion.HeadSHA)
	if err != nil {
		if IsPermanentJob(err) {
			return s.cancelWorkflow(ctx, workflow.ID)
		}
		return err
	}
	version, err := s.gitlab.LatestMRVersion(ctx, token, project.ID, *workflow.MRIID)
	if err != nil {
		return err
	}
	if version == nil || dossier.MR.TargetBranch != mr.TargetBranch ||
		dossier.DiffVersion.BaseSHA == "" || dossier.DiffVersion.StartSHA == "" ||
		!strings.EqualFold(dossier.DiffVersion.BaseSHA, version.BaseSHA) ||
		!strings.EqualFold(dossier.DiffVersion.StartSHA, version.StartSHA) ||
		!strings.EqualFold(dossier.DiffVersion.HeadSHA, version.HeadSHA) {
		return s.cancelWorkflow(ctx, workflow.ID)
	}
	docs, err := s.repoDocs(ctx, token, project.ID, mr.TargetBranch)
	if err != nil {
		return err
	}
	currentConfig, err := resolveSecureReviewConfig(project, docs, s.reviewDefaults, s.profiles)
	if err != nil {
		if errors.Is(err, ErrInvalidRepoConfig) {
			return s.cancelWorkflow(ctx, workflow.ID)
		}
		return err
	}
	storedHash := strings.TrimSpace(dossier.Metadata["config_hash"])
	if !slices.Equal(dossier.IgnorePolicy.Patterns, currentConfig.Settings.Ignore) ||
		storedHash == "" || reviewConfigHash(currentConfig) != storedHash ||
		dossier.Metadata["prompt_hash"] == "" || dossier.Metadata["prompt_hash"] != s.prompts.Hash {
		// Policy freshness is mandatory even when the optional config hash is absent.
		if err := s.cancelWorkflow(ctx, workflow.ID); err != nil {
			return err
		}
		s.logger.Info("recovery skipped workflow with changed configuration",
			"workflow_id", workflow.ID, "config_hash", storedHash)
		return nil
	}
	explicitValue := dossier.Metadata["profile_explicit"]
	if explicitValue != "true" && explicitValue != "false" {
		return s.cancelWorkflow(ctx, workflow.ID)
	}
	profileName, profileCfg := s.profileFor(project, dossier.Metadata["requested_profile"], currentConfig.Profile, mr.Labels, explicitValue == "true")
	profile := review.Profile{Name: profileName, Config: profileCfg}
	if workflow.Profile != profileName || dossier.Metadata["profile_hash"] == "" || dossier.Metadata["profile_hash"] != s.reviewProfileHash(profile) {
		return s.cancelWorkflow(ctx, workflow.ID)
	}
	execution, err := s.resolveReviewExecution(ctx, project.ID, profile)
	if err != nil {
		return err
	}
	if !s.matchesReviewExecution(project.ID, dossier.Metadata["execution_identity"], execution.digest) {
		return s.cancelWorkflow(ctx, workflow.ID)
	}
	headDir, cleanup, err := s.recoverRun(ctx, project, token, *workflow.MRIID, record.JobID, workflow.ID, dossier)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	baseDir, workspaceIdentity, err := s.reviewBaseIdentity(ctx, project.ID, version.StartSHA)
	if err != nil {
		return err
	}
	if workspaceIdentity == "" || dossier.Metadata["workspace_identity"] != workspaceIdentity {
		return s.cancelWorkflow(ctx, workflow.ID)
	}
	_, indexEligible, err := s.reviewIndexState(ctx, project.ID, version.StartSHA, currentConfig.Settings.Ignore)
	if err != nil {
		return err
	}
	if dossier.WorkspaceProvenance.MCPEligible && !indexEligible {
		return s.cancelWorkspaceWorkflow(ctx, workflow.ID)
	}
	diffs := dossierDiffs(&dossier)
	index := gitlab.BuildDiffIndex(dossier.DiffVersion, diffs)
	resolved := dossier
	resolved.HeadDir = headDir
	resolved.BaseDir = baseDir
	executor, err := review.NewExecutor(review.ExecutorDeps{
		Dossier:  &resolved,
		Profile:  profile,
		Graph:    review.GraphConfig{AllowedProviders: s.allowedProviders()},
		Blocks:   execution.blocks,
		Clients:  s.clients,
		Prompts:  s.prompts,
		AgentRun: execution.agentRunner(s.agentRun),
		Snapshot: engineSnapshot(s.engine),
		Tools:    execution.tools,
		Skills:   execution.skills,
		Locate:   locateFunc(index),
		Cost:     s.cost,
	})
	if err != nil {
		return err
	}
	s.executors.Register(workflow.ID, executor)
	defer s.executors.Unregister(workflow.ID)
	noteID := dossierNoteID(dossier)
	s.setReviewStatus(ctx, project.ID, *workflow.MRIID, record.Generation, 0,
		dossier.DiffVersion.HeadSHA, profileName, reviewStatusRunning, "")
	superseded, err := s.runWithControl(ctx, project.ID, *workflow.MRIID, workflow.ID, record.Generation, dossier.DiffVersion.HeadSHA)
	if err != nil {
		return err
	}
	if superseded {
		s.settleRecoveredSuperseded(ctx, project.ID, *workflow.MRIID, record.Generation,
			dossier.DiffVersion.HeadSHA, profileName, noteID, workflow.ID)
		return s.skipPublication(ctx, workflow.ID, reviewSupersededReason)
	}
	snapshot, err := s.engine.Snapshot(ctx, workflow.ID)
	if err != nil {
		return err
	}
	cfg := s.resolveConfig(project, dossier.RepoDocs)
	switch snapshot.Status {
	case dag.WorkflowCompleted:
		s.setReviewStatus(ctx, project.ID, *workflow.MRIID, record.Generation, 0,
			dossier.DiffVersion.HeadSHA, profileName, reviewStatusPublishing, "")
		if err := s.publish(ctx, workflow.ID); err != nil {
			state := reviewStatusFailed
			reaction := reviewAwardFail
			detail := "publication failed"
			if errors.Is(err, ErrStalePublication) {
				state = reviewStatusStale
				reaction = reviewAwardNone
				detail = "review is no longer current"
			}
			s.settleReviewStatus(ctx, project.ID, *workflow.MRIID, record.Generation, 0,
				dossier.DiffVersion.HeadSHA, profileName, state, detail, noteID, reaction)
			return err
		}
		state, reaction := reviewStatusForVerdict(s.workflowVerdict(ctx, workflow.ID))
		s.settleReviewStatus(ctx, project.ID, *workflow.MRIID, record.Generation, 0,
			dossier.DiffVersion.HeadSHA, profileName, state, "", noteID, reaction)
		return nil
	case dag.WorkflowFailed:
		s.setReviewStatus(ctx, project.ID, *workflow.MRIID, record.Generation, 0,
			dossier.DiffVersion.HeadSHA, profileName, reviewStatusPublishing, "")
		var publishErr error
		if cfg.Settings.PostFailureNote {
			publishErr = s.publishFailure(ctx, workflow.ID, snapshot)
		} else {
			publishErr = s.skipPublication(ctx, workflow.ID, failureNoteDisabledReason)
		}
		s.settleReviewStatus(ctx, project.ID, *workflow.MRIID, record.Generation, 0,
			dossier.DiffVersion.HeadSHA, profileName, reviewStatusFailed, "", noteID, reviewAwardFail)
		return publishErr
	case dag.WorkflowCancelled:
		publishErr := s.skipPublication(ctx, workflow.ID, reviewCancelledReason)
		s.settleReviewStatus(ctx, project.ID, *workflow.MRIID, record.Generation, 0,
			dossier.DiffVersion.HeadSHA, profileName, reviewStatusCancelled, "", noteID, reviewAwardNone)
		return publishErr
	}
	return nil
}

func (s *ReviewService) settleRecoveredSuperseded(ctx context.Context, projectID, mrIID, generation int64, head, profile string, noteID int64, workflowID string) {
	control, err := s.store.GetReviewControl(ctx, projectID, mrIID)
	if err != nil || control.Generation != generation {
		return
	}
	state := reviewStatusStale
	detail := "merge request changed before publication"
	if control.Cancelled {
		state = reviewStatusCancelled
		detail = "review cancelled"
	}
	if noteID <= 0 {
		noteID = s.workflowNoteID(ctx, workflowID)
	}
	s.settleReviewStatus(ctx, projectID, mrIID, generation, 0, head, profile, state, detail, noteID, reviewAwardNone)
}

// recoverRun requires the caller to hold the workspace read lock.
func (s *ReviewService) recoverRun(ctx context.Context, project *store.Project, token string, mrIID int64, jobID, workflowID string, dossier review.Dossier) (string, func(), error) {
	ws := s.workspace.Get(project.ID)
	if dossier.HeadDir != "" && ws != nil && runDirOwned(ws, dossier.HeadDir) {
		info, statErr := os.Lstat(dossier.HeadDir)
		switch {
		case statErr == nil && info.Mode()&os.ModeSymlink != 0:
			return "", nil, fmt.Errorf("app: review worktree path is a symlink")
		case statErr == nil && !info.IsDir():
			return "", nil, fmt.Errorf("app: review worktree path is not a directory")
		case statErr == nil:
			head, resolveErr := s.git.Resolve(ctx, dossier.HeadDir, "HEAD")
			if resolveErr == nil && strings.EqualFold(head, dossier.DiffVersion.HeadSHA) {
				return dossier.HeadDir, nil, nil
			}
		case !errors.Is(statErr, os.ErrNotExist):
			return "", nil, statErr
		}
		// A stale, unreadable, or missing worktree is rebuilt from the mirror.
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		if err := s.git.RemoveWorktree(ctx, ws.MirrorDir(), dossier.HeadDir); err != nil {
			return "", nil, fmt.Errorf("app: remove stale review worktree: %w", err)
		}
		if err := removeRunDir(dossier.HeadDir); err != nil {
			return "", nil, err
		}
	}
	runID := jobID
	if runID == "" {
		runID = workflowID
	}
	return s.prepareRun(ctx, project, token, mrIID, runID, dossier.DiffVersion.HeadSHA)
}

// runDirOwned reports whether dir is a direct child of the workspace runs
// directory, so recovery never deletes a path outside its workspace.
func runDirOwned(ws *workspace.Workspace, dir string) bool {
	if ws == nil || ws.Root == "" || dir == "" || !filepath.IsAbs(dir) {
		return false
	}
	runsDir := filepath.Join(ws.Root, "runs")
	if info, err := os.Lstat(runsDir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	rel, err := filepath.Rel(runsDir, filepath.Clean(dir))
	if err != nil || rel == "." || rel == ".." || strings.ContainsRune(rel, filepath.Separator) {
		return false
	}
	return true
}

// failureNoteEnabled reads the persisted configuration snapshot for a failed
// workflow. It avoids publishing a failure note that the project disabled when
// the process died between the workflow failure and its publication state.
func (s *ReviewService) failureNoteEnabled(ctx context.Context, workflowID string) (bool, error) {
	workflow, err := s.store.GetWorkflow(ctx, workflowID)
	if err != nil {
		return false, err
	}
	record, err := s.store.GetWorkflowDossier(ctx, workflowID)
	if err != nil {
		return false, err
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		return false, fmt.Errorf("decode dossier: %w", err)
	}
	projectRow, err := s.store.GetProject(ctx, workflow.ProjectID)
	if err != nil {
		return false, err
	}
	if projectRow == nil {
		return false, fmt.Errorf("project %d is missing", workflow.ProjectID)
	}
	return s.resolveConfig(projectRow, dossier.RepoDocs).Settings.PostFailureNote, nil
}

// PublishPending retries terminal workflows whose publication state is still
// pending. It is safe to call at startup and after a crash.
func (s *ReviewService) PublishPending(ctx context.Context) error {
	var errs []error
	normalRounds := 0
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		pending, err := s.store.ListPendingPublications(ctx, 50)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		if len(pending) == 0 {
			return errors.Join(errs...)
		}
		progress := false
		normalRound := false
		for _, item := range pending {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(errs, err)...)
			}
			if item.Status != string(dag.WorkflowCancelled) {
				if normalRounds >= 3 {
					continue
				}
				normalRound = true
			}
			advanced, err := s.publishPendingItem(ctx, item)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			progress = progress || advanced
		}
		if normalRound {
			normalRounds++
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if !progress {
			break
		}
	}
	return errors.Join(errs...)
}

func (s *ReviewService) publishPendingItem(ctx context.Context, item store.PendingPublication) (bool, error) {
	if item.Status == string(dag.WorkflowCancelled) {
		ws := s.workspace.Get(item.ProjectID)
		if ws == nil {
			return false, errors.New("app: workspace is unavailable")
		}
		unlock, err := ws.RLock(ctx)
		if err != nil {
			return false, err
		}
		defer unlock()
		wf, err := s.store.GetWorkflow(ctx, item.WorkflowID)
		if errors.Is(err, store.ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if wf == nil || wf.ID != item.WorkflowID || wf.ProjectID != item.ProjectID || wf.Status != item.Status {
			return false, nil
		}
		if err := s.skipPublication(ctx, item.WorkflowID, reviewCancelledReason); err != nil {
			return false, err
		}
		record, err := s.store.GetWorkflowDossier(ctx, item.WorkflowID)
		if errors.Is(err, store.ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return record != nil && record.PublicationState != store.WorkflowPublicationPending, nil
	}
	if s.publisher == nil {
		return false, nil
	}
	unlock, err := s.publisher.lockWorkflow(ctx, item.WorkflowID)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer unlock()
	wf, err := s.store.GetWorkflow(ctx, item.WorkflowID)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if wf == nil || wf.ProjectID != item.ProjectID || wf.Status != item.Status {
		return true, nil
	}
	switch item.Status {
	case string(dag.WorkflowFailed):
		record, recordErr := s.store.GetWorkflowDossier(ctx, item.WorkflowID)
		if recordErr != nil {
			return false, recordErr
		}
		var dossier review.Dossier
		if json.Unmarshal(record.Data, &dossier) != nil || !validPublicationPolicy(&dossier) {
			if skipErr := s.skipPublication(ctx, item.WorkflowID, publicationPolicyReason(&dossier)); skipErr != nil {
				return false, skipErr
			}
			return true, nil
		}
		wantsNote, configErr := s.failureNoteEnabled(ctx, item.WorkflowID)
		if configErr != nil {
			return false, configErr
		}
		if !wantsNote {
			if skipErr := s.skipPublication(ctx, item.WorkflowID, failureNoteDisabledReason); skipErr != nil {
				return false, skipErr
			}
			return true, nil
		}
		err = s.publisher.publishFailureLocked(ctx, item.WorkflowID, reviewExecutionFailedReason)
	case string(dag.WorkflowCompleted):
		err = s.publisher.publishLocked(ctx, item.WorkflowID)
	default:
		return false, nil
	}
	if err != nil {
		if IsPermanentJob(err) {
			return true, nil
		}
		return false, err
	}
	return true, nil
}

func dossierDiffs(d *review.Dossier) []gitlab.FileDiff {
	out := make([]gitlab.FileDiff, 0, len(d.Files))
	for _, file := range d.Files {
		diff := gitlab.FileDiff{
			OldPath: file.OldPath,
			NewPath: file.Path,
			Diff:    file.Diff,
		}
		switch file.Status {
		case "added", "new":
			diff.NewFile = true
		case "deleted":
			diff.DeletedFile = true
		case "renamed":
			diff.RenamedFile = true
		}
		out = append(out, diff)
	}
	return out
}
