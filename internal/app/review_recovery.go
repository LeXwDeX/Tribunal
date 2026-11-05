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

	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// recoveryCancel records the stable cause and then runs one recovery cancel
// path, so a later failure while waiting for the terminal state or settling
// publication cannot drop the cause. It never changes the cancel reason or
// publication semantics. The cause is logged even when the workflow turns out
// to be already terminal; the message says "cancelling", not "cancelled".
func (s *ReviewService) recoveryCancel(ctx context.Context, workflowID, cause string, cancel func(context.Context, string) error) error {
	s.logRecoveryCancel(workflowID, cause)
	return cancel(ctx, workflowID)
}

// recoveryCancelWorkflow is the recovery cancel path for causes that used to
// report "review superseded". It keeps the engine cancel reason, records the
// real cause as the publication reason and, when the generation is still the
// MR's current one, settles the status so it does not stay "running".
func (s *ReviewService) recoveryCancelWorkflow(engineReason, cause string) func(context.Context, string) error {
	return func(ctx context.Context, id string) error {
		if err := s.cancelWorkflowSkipping(ctx, id, engineReason, recoveryCancelReason(cause)); err != nil {
			return err
		}
		s.settleRecoveryCancel(ctx, id, cause)
		return nil
	}
}

// recoveryCancelThenSettle adds the status settlement to a cancel path that
// owns its own engine and publication reasons.
func (s *ReviewService) recoveryCancelThenSettle(cancel func(context.Context, string) error, cause string) func(context.Context, string) error {
	return func(ctx context.Context, id string) error {
		if err := cancel(ctx, id); err != nil {
			return err
		}
		s.settleRecoveryCancel(ctx, id, cause)
		return nil
	}
}

func recoveryCancelReason(cause string) string { return "cancelled on recovery: " + cause }

// settleRecoveryCancel terminates the review status of a workflow cancelled by
// recovery, but only while its generation and head are still current; a newer
// generation's status is never overwritten.
func (s *ReviewService) settleRecoveryCancel(ctx context.Context, workflowID, cause string) {
	warn := func(err error) {
		s.logger.Warn("recovery cancel status not settled", "workflow_id", workflowID, "cause", cause, "error_class", errclass.Of(err))
	}
	wf, err := s.store.GetWorkflow(ctx, workflowID)
	if err != nil {
		warn(err)
		return
	}
	if wf == nil || wf.MRIID == nil {
		return
	}
	record, err := s.store.GetWorkflowDossier(ctx, workflowID)
	if err != nil {
		warn(err)
		return
	}
	var dossier review.Dossier
	if record == nil {
		return
	}
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		// An undecodable dossier has no trustworthy head to settle against.
		warn(err)
		return
	}
	head := dossier.DiffVersion.HeadSHA
	if _, err := s.currentControl(ctx, wf.ProjectID, *wf.MRIID, record.Generation, head); err != nil {
		if !IsPermanentJob(err) {
			warn(err)
		}
		return
	}
	s.settleReviewStatus(ctx, wf.ProjectID, *wf.MRIID, record.Generation, 0, head, wf.Profile,
		reviewStatusCancelled, recoveryCancelReason(cause)+"; please re-trigger the review",
		dossierNoteID(dossier), reviewAwardNone)
}

func (s *ReviewService) logRecoveryCancel(workflowID, cause string) {
	s.logger.Info("recovery cancelling workflow", "workflow_id", workflowID, "cause", cause)
}

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
		if err := s.recoverRepositorySkillOutcomes(ctx); err != nil {
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

// A crash after skipping publication can leave its status or reaction unwritten.
func (s *ReviewService) recoverRepositorySkillOutcomes(ctx context.Context) error {
	if s.publisher == nil {
		return nil
	}
	var errs []error
	cursor := ""
	for {
		workflows, next, err := s.store.ListWorkflows(ctx, store.WorkflowFilter{
			Kind: "review", PublicationState: store.WorkflowPublicationSkipped,
		}, cursor, 50)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, wf := range workflows {
			if !workflowTerminal(wf.Status) || wf.MRIID == nil {
				continue
			}
			record, err := s.store.GetWorkflowDossier(ctx, wf.ID)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if record == nil || (record.PublicationError != repositorySkillEvidenceReason &&
				(wf.Status == string(dag.WorkflowCompleted) || review.DossierHasSupportedRepositorySkillCapability(record.Data))) {
				continue
			}
			lockedCtx, unlock, err := s.publisher.lockWorkflow(ctx, wf.ID)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			err = s.recoverRepositorySkillOutcomeLocked(lockedCtx, wf.ID)
			unlock()
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				errs = append(errs, err)
			}
		}
		if next == "" || next == cursor {
			return errors.Join(errs...)
		}
		cursor = next
	}
}

// Recheck the durable reason under the publication locks before settling.
func (s *ReviewService) recoverRepositorySkillOutcomeLocked(ctx context.Context, id string) error {
	mrUnlock, err := s.publisher.lockReviewMR(ctx, id)
	if err != nil {
		return err
	}
	defer mrUnlock()
	unlock, err := s.publisher.locks.lock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	wf, err := s.store.GetWorkflow(ctx, id)
	if err != nil || wf == nil {
		return err
	}
	record, err := s.store.GetWorkflowDossier(ctx, id)
	if err != nil || record == nil {
		return err
	}
	if !workflowTerminal(wf.Status) || wf.MRIID == nil || record.PublicationState != store.WorkflowPublicationSkipped ||
		(record.PublicationError != repositorySkillEvidenceReason &&
			(wf.Status == string(dag.WorkflowCompleted) || review.DossierHasSupportedRepositorySkillCapability(record.Data))) {
		return nil
	}
	_, _, err = s.publisher.rejectRepositorySkillPublicationLocked(ctx, s.store, id)
	return err
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
	ctx, err = bindOutbound(ctx, ws)
	if err != nil {
		return err
	}
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
	if !review.DossierHasSupportedRepositorySkillCapability(record.Data) {
		return s.recoveryCancel(ctx, workflow.ID, "repository_skill_capability_unsupported", s.cancelRepositorySkillWorkflow)
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		return s.recoveryCancel(ctx, workflow.ID, "dossier_invalid", s.recoveryCancelThenSettle(s.cancelWorkspaceWorkflow, "dossier_invalid"))
	}
	if err := review.ValidateDossier(&dossier); err != nil {
		return s.recoveryCancel(ctx, workflow.ID, "dossier_invalid", s.recoveryCancelThenSettle(s.cancelWorkspaceWorkflow, "dossier_invalid"))
	}
	if dossier.Project.ID != workflow.ProjectID || dossier.MR.IID != *workflow.MRIID {
		return s.recoveryCancel(ctx, workflow.ID, "dossier_mismatch", s.recoveryCancelWorkflow("superseded", "dossier_mismatch"))
	}
	if _, err := s.currentControl(ctx, project.ID, *workflow.MRIID, record.Generation, dossier.DiffVersion.HeadSHA); err != nil {
		if IsPermanentJob(err) {
			s.logRecoveryCancel(workflow.ID, "control_not_current")
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
			return s.recoveryCancel(ctx, workflow.ID, "workspace_provenance_invalid", s.recoveryCancelThenSettle(s.cancelWorkspaceWorkflow, "workspace_provenance_invalid"))
		}
		return s.recoveryCancel(ctx, workflow.ID, "publication_policy_invalid", s.recoveryCancelWorkflow("superseded", "publication_policy_invalid"))
	}
	mr, _, err := s.collectFilterInputs(ctx, project, token, *workflow.MRIID, dossier.DiffVersion.HeadSHA)
	if err != nil {
		if IsPermanentJob(err) {
			return s.recoveryCancel(ctx, workflow.ID, "mr_inputs_unavailable", s.recoveryCancelWorkflow("superseded", "mr_inputs_unavailable"))
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
		return s.recoveryCancel(ctx, workflow.ID, "diff_version_changed", s.recoveryCancelWorkflow("superseded", "diff_version_changed"))
	}
	docs, err := s.repoDocs(ctx, token, project.ID, mr.TargetBranch)
	if err != nil {
		return err
	}
	currentConfig, err := resolveSecureReviewConfig(project, docs, s.reviewDefaults, s.profiles)
	if err != nil {
		if errors.Is(err, ErrInvalidRepoConfig) {
			return s.recoveryCancel(ctx, workflow.ID, "repo_config_invalid", func(ctx context.Context, _ string) error {
				return s.cancelInvalidRepoConfig(ctx, &workflow, &dossier, record.Generation, token)
			})
		}
		return err
	}
	storedHash := strings.TrimSpace(dossier.Metadata["config_hash"])
	if !slices.Equal(dossier.IgnorePolicy.Patterns, currentConfig.Settings.Ignore) ||
		storedHash == "" || reviewConfigHash(currentConfig) != storedHash ||
		dossier.Metadata["prompt_hash"] == "" || dossier.Metadata["prompt_hash"] != s.prompts.Hash {
		// Policy freshness is mandatory even when the optional config hash is absent.
		return s.recoveryCancel(ctx, workflow.ID, "config_changed", s.recoveryCancelWorkflow("superseded", "config_changed"))
	}
	explicitValue := dossier.Metadata["profile_explicit"]
	if explicitValue != "true" && explicitValue != "false" {
		return s.recoveryCancel(ctx, workflow.ID, "profile_explicit_invalid", s.recoveryCancelWorkflow("superseded", "profile_explicit_invalid"))
	}
	profileName, profileCfg, err := s.profileFor(ctx, project, dossier.Metadata["requested_profile"], currentConfig.Profile, mr.Labels, explicitValue == "true")
	if err != nil {
		return err
	}
	profile := review.Profile{Name: profileName, Config: profileCfg}
	if workflow.Profile != profileName || dossier.Metadata["profile_hash"] == "" || dossier.Metadata["profile_hash"] != s.reviewProfileHash(profile) {
		// The profile (including referenced user models, #299) changed while
		// the process was down; record a visible reason like identity changes.
		return s.recoveryCancel(ctx, workflow.ID, "profile_hash_changed", s.recoveryCancelWorkflow("profile_hash_changed", "profile_hash_changed"))
	}
	if dossier.RepositorySkillProvenance != nil && dossier.RepositorySkillProvenance.Mode == "repository" {
		lister, _ := s.store.(repositorySkillGrantLister)
		if err := repositorySkillProvenanceCurrent(ctx, lister, project, s.reviewDefaults, &dossier, dossier.RepositorySkillProvenance); err != nil {
			return s.recoveryCancel(ctx, workflow.ID, "repository_skill_changed", s.cancelRepositorySkillWorkflow)
		}
	}
	repositoryDeny, err := repositorySkillDenyFromEvidence(dossier.IgnorePolicy.Patterns, dossier.RepositorySkillExcluded)
	if err != nil {
		return s.recoveryCancel(ctx, workflow.ID, "repository_skill_deny_invalid", s.cancelRepositorySkillWorkflow)
	}
	execution, err := s.resolveReviewExecution(ctx, project, token, profile, dossier.MR.TargetBranch, repositoryDeny)
	if err != nil {
		if dossier.RepositorySkillProvenance != nil && dossier.RepositorySkillProvenance.Mode == "repository" {
			return s.recoveryCancel(ctx, workflow.ID, "repository_skill_resolve_failed", s.cancelRepositorySkillWorkflow)
		}
		return err
	}
	defer execution.cleanupRepositorySkills()
	if !s.matchesReviewExecution(project.ID, dossier.Metadata["execution_identity"], execution.digest) {
		return s.recoveryCancel(ctx, workflow.ID, "execution_identity_changed", s.recoveryCancelWorkflow("execution_identity_changed", "execution_identity_changed"))
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
		return s.recoveryCancel(ctx, workflow.ID, "workspace_identity_changed", s.recoveryCancelWorkflow("superseded", "workspace_identity_changed"))
	}
	_, indexEligible, err := s.reviewIndexState(ctx, project.ID, version.StartSHA, currentConfig.Settings.Ignore)
	if err != nil {
		return err
	}
	if dossier.WorkspaceProvenance.MCPEligible && !indexEligible {
		return s.recoveryCancel(ctx, workflow.ID, "index_ineligible", s.recoveryCancelThenSettle(s.cancelWorkspaceWorkflow, "index_ineligible"))
	}
	index := dossierDiffIndex(dossier.DiffVersion, &dossier)
	resolved := dossier
	resolved.HeadDir = headDir
	resolved.BaseDir = baseDir
	executor, err := review.NewExecutor(review.ExecutorDeps{
		Dossier:                  &resolved,
		Profile:                  profile,
		Graph:                    review.GraphConfig{AllowedProviders: s.graphProviders()},
		Blocks:                   execution.blocks,
		Clients:                  s.snapshotClients(),
		Prompts:                  s.prompts,
		AgentRun:                 execution.agentRunner(s.agentRun),
		Snapshot:                 engineSnapshot(s.engine),
		Tools:                    execution.tools,
		Skills:                   execution.skills,
		Locate:                   locateFunc(index),
		Cost:                     s.cost,
		Outbound:                 outboundSender(ws),
		RepositorySkillsVerified: execution.repositorySkillsVerified(),
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
	if err := sendOutbound(ctx, ws, func(context.Context) error { return nil }); err != nil {
		return err
	}
	if superseded {
		s.logger.Info("recovery observed cancelled workflow", "workflow_id", workflow.ID, "cause", "superseded_during_run")
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
			if errors.Is(err, workspace.ErrOutboundInvalidated) || errors.Is(err, errDiscussionUnknown) {
				return err
			}
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
		if cfg.Settings.PostFailureNote || cfg.Settings.CommitStatus {
			publishErr = s.publishFailure(ctx, workflow.ID, snapshot)
		} else {
			publishErr = s.skipPublication(ctx, workflow.ID, failureNoteDisabledReason)
		}
		s.settleReviewStatus(ctx, project.ID, *workflow.MRIID, record.Generation, 0,
			dossier.DiffVersion.HeadSHA, profileName, reviewStatusFailed, "", noteID, reviewAwardFail)
		return publishErr
	case dag.WorkflowCancelled:
		cause := snapshot.CancelReason
		if cause == "" {
			cause = "unknown"
		}
		s.logger.Info("recovery observed cancelled workflow", "workflow_id", workflow.ID, "cause", cause)
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

// failurePublicationEnabled reads the persisted configuration snapshot for a
// failed workflow. It reports whether any failure output is enabled, so a
// retry does not skip a commit status when only the failure note is disabled.
func (s *ReviewService) failurePublicationEnabled(ctx context.Context, workflowID string) (bool, error) {
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
	settings := s.resolveConfig(projectRow, dossier.RepoDocs).Settings
	return settings.PostFailureNote || settings.CommitStatus, nil
}

// PublishPending retries terminal workflows whose publication state is still
// pending. It is safe to call at startup and after a crash.
func (s *ReviewService) PublishPending(ctx context.Context) error {
	var errs []error
	// Workflow creation order is immutable, even when a pending row is retried.
	for round := 0; round < 3; round++ {
		progress := false
		for _, status := range []string{string(dag.WorkflowCancelled), string(dag.WorkflowFailed), string(dag.WorkflowCompleted)} {
			cursor := ""
			for {
				if err := ctx.Err(); err != nil {
					return errors.Join(append(errs, err)...)
				}
				workflows, next, err := s.store.ListWorkflows(ctx, store.WorkflowFilter{
					Status: status, PublicationState: store.WorkflowPublicationPending,
				}, cursor, 50)
				if err != nil {
					return errors.Join(append(errs, err)...)
				}
				for _, workflow := range workflows {
					if err := ctx.Err(); err != nil {
						return errors.Join(append(errs, err)...)
					}
					dossier, err := s.store.GetWorkflowDossier(ctx, workflow.ID)
					if errors.Is(err, store.ErrNotFound) {
						continue
					}
					if err != nil {
						errs = append(errs, err)
						continue
					}
					item := store.PendingPublication{WorkflowID: workflow.ID, JobID: dossier.JobID,
						ProjectID: workflow.ProjectID, HeadSHA: workflow.HeadSHA,
						Generation: dossier.Generation, Status: workflow.Status, UpdatedAt: dossier.UpdatedAt}
					if workflow.MRIID != nil {
						item.MRIID = *workflow.MRIID
					}
					advanced, err := s.publishPendingItem(ctx, item)
					if err != nil {
						errs = append(errs, err)
					} else if advanced {
						progress = true
					}
				}
				if next == "" || next == cursor {
					break
				}
				cursor = next
			}
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
		if s.publisher != nil {
			if rejected, unknown, err := s.publisher.rejectRepositorySkillPublication(ctx, s.store, item.WorkflowID); err != nil {
				return false, err
			} else if rejected {
				return !unknown, nil
			}
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
	ctx, unlock, err := s.publisher.lockWorkflow(ctx, item.WorkflowID)
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
	if wf == nil || wf.ProjectID != item.ProjectID || wf.Status != item.Status || wf.PublicationState != store.WorkflowPublicationPending {
		return true, nil
	}
	if rejected, unknown, err := s.publisher.rejectRepositorySkillPublication(ctx, s.store, item.WorkflowID); rejected || err != nil {
		if err != nil {
			return false, err
		}
		if unknown {
			return false, errDiscussionUnknown
		}
		return true, nil
	}
	switch item.Status {
	case string(dag.WorkflowFailed):
		record, recordErr := s.store.GetWorkflowDossier(ctx, item.WorkflowID)
		if recordErr != nil {
			return false, recordErr
		}
		if !review.DossierHasNoRepositorySkillCapability(record.Data) {
			rejected, unknown, err := s.publisher.rejectRepositorySkillPublication(ctx, s.store, item.WorkflowID)
			if err != nil {
				return false, err
			}
			if unknown {
				return false, errDiscussionUnknown
			}
			if rejected {
				return true, nil
			}
		}
		var dossier review.Dossier
		if json.Unmarshal(record.Data, &dossier) != nil || !validPublicationPolicy(&dossier) {
			if skipErr := s.skipPublication(ctx, item.WorkflowID, publicationPolicyReason(&dossier)); skipErr != nil {
				return false, skipErr
			}
			return true, nil
		}
		wantsPublication, configErr := s.failurePublicationEnabled(ctx, item.WorkflowID)
		if configErr != nil {
			return false, configErr
		}
		if !wantsPublication {
			if skipErr := s.skipPublication(ctx, item.WorkflowID, failureNoteDisabledReason); skipErr != nil {
				return false, skipErr
			}
			return true, nil
		}
		err = s.publisher.publishFailureLocked(ctx, item.WorkflowID, reviewExecutionFailedReason)
	case string(dag.WorkflowCompleted):
		if _, _, err := s.startRepublishStatus(ctx, item.WorkflowID); err != nil {
			return false, err
		}
		err = s.publisher.publishLocked(ctx, item.WorkflowID)
	default:
		return false, nil
	}
	if err != nil {
		if IsPermanentJob(err) && !errors.Is(err, errDiscussionUnknown) {
			if item.Status == string(dag.WorkflowCompleted) {
				if settleErr := s.settlePendingCompleted(ctx, item); settleErr != nil {
					return false, settleErr
				}
			}
			return true, nil
		}
		return false, err
	}
	if item.Status == string(dag.WorkflowCompleted) {
		if err := s.settlePendingCompleted(ctx, item); err != nil {
			return false, err
		}
	}
	return true, nil
}

// A pending completed workflow can finish publication without running its job
// again. Preserve the generation fence when settling its status and reaction.
func (s *ReviewService) settlePendingCompleted(ctx context.Context, item store.PendingPublication) error {
	unlock, err := s.lockPublicationOutcome(ctx, item.WorkflowID)
	if err != nil {
		return err
	}
	defer unlock()
	wf, err := s.store.GetWorkflow(ctx, item.WorkflowID)
	if err != nil {
		return err
	}
	record, err := s.store.GetWorkflowDossier(ctx, item.WorkflowID)
	if err != nil {
		return err
	}
	if wf == nil || wf.ProjectID != item.ProjectID || wf.MRIID == nil || *wf.MRIID != item.MRIID ||
		wf.Status != string(dag.WorkflowCompleted) || record == nil || record.Generation != item.Generation {
		return nil
	}
	latest, err := s.store.LatestReviewWorkflow(ctx, item.ProjectID, item.MRIID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if latest == nil || latest.ID != wf.ID {
		return nil
	}
	control, err := s.store.GetReviewControl(ctx, item.ProjectID, item.MRIID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if control.Cancelled || control.Generation != item.Generation || !strings.EqualFold(control.HeadSHA, item.HeadSHA) {
		return nil
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		if errors.Is(err, review.ErrInvalidDossier) && record.PublicationState == store.WorkflowPublicationSkipped {
			return nil
		}
		return fmt.Errorf("app: decode publication dossier: %w", err)
	}
	if !strings.EqualFold(dossier.DiffVersion.HeadSHA, item.HeadSHA) {
		return nil
	}
	state, reaction, detail := completedPublicationStatus(wf.Verdict, record.PublicationState)
	if state == "" {
		return nil
	}
	current, err := s.store.GetReviewStatus(ctx, item.ProjectID, item.MRIID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	attempt := 0
	alreadySettled := current != nil && current.Generation == item.Generation && current.State == state && current.Detail == detail
	if current != nil && !alreadySettled && current.Generation == item.Generation && terminalReviewStatus(current.State) {
		attempt = current.Attempt + 1
	}
	if !alreadySettled {
		written, err := s.store.SetReviewStatus(ctx, store.ReviewStatusGoal{
			ProjectID: item.ProjectID, MRIID: item.MRIID, Generation: item.Generation,
			Attempt: attempt, HeadSHA: item.HeadSHA, Profile: wf.Profile, State: state, Detail: detail,
		})
		if errors.Is(err, store.ErrSuperseded) || (err == nil && !written) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	if noteID := dossierNoteID(dossier); noteID > 0 {
		_, err = s.store.SetReviewReaction(ctx, store.ReviewReactionGoal{
			ProjectID: item.ProjectID, MRIID: item.MRIID, NoteID: noteID,
			Generation: item.Generation, Desired: reaction,
		})
		if !errors.Is(err, store.ErrSuperseded) {
			return err
		}
	}
	return nil
}

// The caller holds the workspace read lock. Share the publisher's lock order so
// a republish cannot change the stored outcome while status is being settled.
func (s *ReviewService) lockPublicationOutcome(ctx context.Context, workflowID string) (func(), error) {
	mrUnlock, err := s.publisher.lockReviewMR(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	unlock, err := s.publisher.locks.lock(ctx, workflowID)
	if err != nil {
		mrUnlock()
		return nil, err
	}
	return func() { unlock(); mrUnlock() }, nil
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
		case "renamed", "copied":
			diff.RenamedFile = true
		}
		out = append(out, diff)
	}
	return out
}

func dossierDiffIndex(version gitlab.Version, d *review.Dossier) *gitlab.DiffIndex {
	index := gitlab.BuildDiffIndex(version, dossierDiffs(d))
	paths := append([]string(nil), d.Ignored...)
	for _, omitted := range d.DiffSummary.Omitted {
		paths = append(paths, omitted.Path)
	}
	unknown := d.DiffSummary.Truncated || len(d.DiffSummary.Omitted) > 0 || len(d.Ignored) > 0
	for _, file := range d.Files {
		if file.OldPath == "" && (file.Status == "renamed" || file.Status == "copied") {
			unknown = true
		}
	}
	if unknown {
		// A missing old or omitted path could alias any included file.
		for _, file := range d.Files {
			paths = append(paths, file.Path, file.OldPath)
		}
	}
	index.WithholdPaths(paths)
	return index
}
