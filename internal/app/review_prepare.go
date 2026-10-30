package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func (s *ReviewService) incrementalRange(ctx context.Context, projectID int64, payload reviewRunPayload, control *store.ReviewControl, head string, files []review.FileChange) *review.IncrementalRange {
	if payload.Trigger != "mr_update" || payload.Full {
		return &review.IncrementalRange{Head: head, Full: true, Reason: "full review"}
	}
	last, err := s.store.LastPublishedHead(ctx, projectID, control.MRIID)
	if err != nil || last == "" || strings.EqualFold(last, head) {
		return &review.IncrementalRange{Head: head, Full: true, Reason: "no previous reviewed head"}
	}
	// A previous published P0/P1 may live in a file this push did not touch.
	// The incremental scope cannot recheck it, so fall back to the full MR.
	severe, err := s.store.HasPublishedSevereFinding(ctx, projectID, control.MRIID)
	if err != nil {
		return &review.IncrementalRange{Head: head, LastHead: last, Full: true, Reason: "previous finding lookup failed"}
	}
	if severe {
		return &review.IncrementalRange{Head: head, LastHead: last, Full: true, Reason: "previous severe finding was not rechecked"}
	}
	ws := s.workspace.Get(projectID)
	if ws == nil {
		return nil
	}
	ancestor, err := s.git.IsAncestor(ctx, ws.MirrorDir(), last, head)
	if err != nil {
		s.logger.Warn("incremental ancestry check failed", "project_id", projectID, "mr_iid", control.MRIID)
		return &review.IncrementalRange{Head: head, LastHead: last, Full: true, Reason: "ancestry check failed"}
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	scope, err := review.ComputeIncrementalScope(review.IncrementalInput{
		LastHead:   last,
		Head:       head,
		MRFiles:    paths,
		IsAncestor: ancestor,
		LoadChanged: func(from, to string) ([]string, error) {
			return s.git.ChangedFiles(ctx, ws.MirrorDir(), from, to)
		},
	})
	if err != nil {
		return &review.IncrementalRange{Head: head, LastHead: last, Full: true, Reason: "incremental scope failed"}
	}
	if len(scope.Files) == 0 {
		return &review.IncrementalRange{Head: head, LastHead: last, Full: true, Reason: "empty incremental scope"}
	}
	return &review.IncrementalRange{
		LastHead: last,
		Head:     head,
		Files:    scope.Files,
		Full:     scope.Full,
		Reason:   scope.Reason,
	}
}

// prepareRun fetches the MR head and creates a detached worktree keyed by the
// retried job ID. The caller holds the workspace read lock through cleanup.
func (s *ReviewService) prepareRun(ctx context.Context, project *store.Project, token string, mrIID int64, runID, head string) (string, func(), error) {
	ws := s.workspace.Get(project.ID)
	if ws == nil {
		return "", nil, PermanentJob(fmt.Errorf("workspace is unavailable"))
	}
	fail := func(err error) (string, func(), error) {
		return "", nil, err
	}
	remote, err := currentProjectRemote(ctx, s.gitlab, project, token)
	if err != nil {
		return fail(err)
	}
	if err := ensureReviewRoot(ws.Root); err != nil {
		return fail(err)
	}
	auth := repo.Auth{Token: token}
	ref := fmt.Sprintf("refs/merge-requests/%d/head", mrIID)
	refspec := fmt.Sprintf("+%s:%s", ref, ref)
	resolved, err := fetchProjectRef(ctx, ws, s.git, remote, auth, refspec, ref)
	if err != nil {
		return fail(err)
	}
	if !strings.EqualFold(resolved, head) {
		// The mirror may lag behind the API; retry and re-check the generation.
		return fail(fmt.Errorf("app: merge request ref does not match the job head"))
	}

	runsDir := filepath.Join(ws.Root, "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		return fail(fmt.Errorf("app: create run directory: %w", err))
	}
	if info, err := os.Lstat(runsDir); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fail(fmt.Errorf("app: run directory is unsafe"))
	}
	runDir := filepath.Join(runsDir, runID)
	switch info, err := os.Lstat(runDir); {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fail(fmt.Errorf("app: run path is not a directory"))
		}
		current, resolveErr := s.git.Resolve(ctx, runDir, "HEAD")
		if resolveErr == nil && strings.EqualFold(current, head) {
			break
		}
		// A missing or interrupted worktree is rebuilt from the mirror.
		if err := removeRunDir(runDir); err != nil {
			return fail(err)
		}
		if err := s.git.AddWorktree(ctx, ws.MirrorDir(), runDir, head); err != nil {
			return fail(err)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := s.git.AddWorktree(ctx, ws.MirrorDir(), runDir, head); err != nil {
			return fail(err)
		}
	default:
		return fail(err)
	}

	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := s.git.RemoveWorktree(cleanupCtx, ws.MirrorDir(), runDir); err != nil {
			s.logger.Warn("run worktree removal failed", "project_id", project.ID, "run_id", runID)
		}
		_ = os.RemoveAll(runDir)
	}
	return runDir, cleanup, nil
}

// reviewBaseIdentity is called while holding the workspace read lock. An
// unproven base is never passed to the sandbox or used to resume a workflow.
func (s *ReviewService) reviewBaseIdentity(ctx context.Context, projectID int64, startSHA string) (string, string, error) {
	ws := s.workspace.Get(projectID)
	if ws == nil {
		return "", "", nil
	}
	base := ws.BaseDir()
	info, err := os.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		return "", "absent", nil
	}
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", nil
	}
	state, err := s.store.GetWorkspace(ctx, projectID)
	if err != nil {
		return "", "", err
	}
	if state == nil {
		return "", "", nil
	}
	ownedHead := ws.BaseSHA()
	head, err := s.git.Resolve(ctx, base, "HEAD")
	if err != nil || !shaRE.MatchString(head) || !strings.EqualFold(head, ownedHead) || !strings.EqualFold(head, state.BaseSHA) || !strings.EqualFold(head, startSHA) {
		return "", "", nil
	}
	return base, sha256Hex(strings.Join([]string{base, strings.ToLower(head), state.IndexSHA, state.ContextSHA}, "\n")), nil
}

// ensureReviewRoot creates the project workspace root and rejects a symlinked
// root or parent before a git or worktree write can follow it. Deeper
// configured ancestors are trusted.
func ensureReviewRoot(root string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("app: workspace root is not absolute")
	}
	info, err := os.Lstat(root)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("app: workspace root is unsafe")
		}
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("app: inspect workspace root: %w", err)
	}
	parent := filepath.Dir(root)
	parentInfo, err := os.Lstat(parent)
	switch {
	case err == nil:
		if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
			return fmt.Errorf("app: workspace parent is unsafe")
		}
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return fmt.Errorf("app: create workspace parent: %w", err)
		}
		parentInfo, err = os.Lstat(parent)
		if err != nil || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
			return fmt.Errorf("app: workspace parent is unsafe")
		}
	default:
		return fmt.Errorf("app: inspect workspace parent: %w", err)
	}
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("app: create workspace root: %w", err)
	}
	info, err = os.Lstat(root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("app: workspace root is unsafe")
	}
	return nil
}

// publishEmpty publishes the "nothing to review" summary without a workflow.
func (s *ReviewService) publishEmpty(ctx context.Context, project *store.Project, token string, control *store.ReviewControl, inputs *reviewInputs, cfg effectiveReviewConfig, summary review.DiffSummary, ignored []string) error {
	return s.publishEmptyWithNote(ctx, project, token, control, 0, inputs, cfg, summary, ignored)
}

func (s *ReviewService) publishEmptyWithNote(ctx context.Context, project *store.Project, token string, control *store.ReviewControl, noteID int64, inputs *reviewInputs, cfg effectiveReviewConfig, summary review.DiffSummary, ignored []string) error {
	if inputs == nil || inputs.MR == nil || inputs.Version == nil {
		return PermanentJob(errors.New("app: publish empty summary: incomplete review inputs"))
	}
	if s.publisher == nil {
		return errors.New("app: review publisher is not configured")
	}
	recheck := func() error {
		_, err := s.currentControl(ctx, project.ID, control.MRIID, control.Generation, inputs.Version.HeadSHA)
		return err
	}
	if err := recheck(); err != nil {
		return err
	}
	s.setReviewStatus(ctx, project.ID, control.MRIID, control.Generation, 0,
		inputs.Version.HeadSHA, "", reviewStatusPublishing, "")
	profileName, _ := s.profileFor(project, "", cfg.Profile, inputs.MR.Labels, false)
	fingerprint := sha256Hex(strings.Join([]string{
		inputs.Version.HeadSHA, reviewConfigHash(cfg), profileName,
	}, "\n"))
	summaryInput := review.SummaryInput{
		Verdict:     review.VerdictPass,
		Summary:     "无需评审：所有变更文件均被忽略规则过滤。",
		Fingerprint: fingerprint,
		HeadSHA:     inputs.Version.HeadSHA,
		Profile:     profileName,
		Warnings:    append(cfg.Warnings, "no reviewable files after ignore rules"),
	}
	if summary.Truncated || len(summary.Omitted) > 0 {
		total := summary.Total
		if total == 0 {
			total = summary.Files + len(summary.Omitted) + len(inputs.Diffs)
		}
		reason := fmt.Sprintf("omitted %d of %d changed file(s) (included=%d, ignored=%d)",
			len(summary.Omitted), total, summary.Files, len(ignored))
		if len(summary.Omitted) > 0 {
			reason += " before review:" + omittedPathList(summary.Omitted, 4)
			reasons := make(map[string]int, len(summary.Omitted))
			for _, omitted := range summary.Omitted {
				reasons[omitted.Reason]++
			}
			parts := make([]string, 0, len(reasons))
			for name, count := range reasons {
				parts = append(parts, fmt.Sprintf("%s=%d", name, count))
			}
			sort.Strings(parts)
			reason += "; reasons: " + strings.Join(parts, ", ")
		} else {
			reason += "; omitted paths were not recorded in this older snapshot"
		}
		summaryInput.Verdict = review.VerdictIncomplete
		summaryInput.Summary = "没有可评审的文件：材料快照遗漏了变更文件。"
		summaryInput.IncompleteReason = reason
		summaryInput.Warnings = append(cfg.Warnings,
			fmt.Sprintf("no reviewable files: %d changed file(s) omitted before review", len(summary.Omitted)))
	}
	body, err := review.RenderSummary(summaryInput)
	if err != nil {
		return PermanentJob(fmt.Errorf("render empty summary: %w", err))
	}
	// A push can land while the summary is rendered; never publish stale text.
	if err := recheck(); err != nil {
		return err
	}
	// The MR publication lock serializes writers, and the recheck inside
	// publishStandaloneSummaryLocked runs before each summary attempt. A control
	// change can still race an in-flight request.
	if err := s.publisher.publishStandaloneSummaryLocked(ctx, project.ID, control.MRIID, control.Generation, inputs.Version.HeadSHA, token, fingerprint, body); err != nil {
		state := reviewStatusFailed
		reaction := reviewAwardFail
		detail := "publication failed"
		if errors.Is(err, ErrStalePublication) {
			state = reviewStatusStale
			reaction = reviewAwardNone
			detail = "review is no longer current"
		}
		s.settleReviewStatus(ctx, project.ID, control.MRIID, control.Generation, 0,
			inputs.Version.HeadSHA, "", state, detail, noteID, reaction)
		return err
	}
	state, reaction := reviewStatusForVerdict(summaryInput.Verdict)
	s.settleReviewStatus(ctx, project.ID, control.MRIID, control.Generation, 0,
		inputs.Version.HeadSHA, "", state, "", noteID, reaction)
	return nil
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// removeRunDir clears an interrupted worktree before rebuilding it.
func removeRunDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("app: remove interrupted run worktree: %w", err)
	}
	return nil
}

func truncateRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max])
}

func newIgnoreMatcher(patterns []string) *review.IgnoreMatcher {
	return review.NewIgnoreMatcher(patterns)
}

// buildReviewFiles filters diffs, applies ignore rules and bounds, and counts
// lines. Invalid repository paths are skipped rather than failing the review.
func buildReviewFiles(diffs []gitlab.FileDiff, ignores []string, maxFiles, maxDiffLines int) ([]review.FileChange, []string, review.DiffSummary, []string) {
	matcher := newIgnoreMatcher(ignores)
	files := make([]review.FileChange, 0, len(diffs))
	ignored := make([]string, 0)
	warnings := make([]string, 0)
	summary := review.DiffSummary{Total: len(diffs)}
	lines := 0
	omittedByFiles := 0
	omittedByLines := 0
	omit := func(filePath, reason string) {
		summary.Truncated = true
		summary.Omitted = append(summary.Omitted, review.OmittedFile{Path: filePath, Reason: reason})
	}
	for _, diff := range diffs {
		filePath := strings.TrimSpace(diff.NewPath)
		if filePath == "" {
			filePath = strings.TrimSpace(diff.OldPath)
		}
		if !validRepoPath(filePath) {
			label := invalidPathLabel(filePath)
			omit(label, "invalid_path")
			warnings = append(warnings, fmt.Sprintf(
				"review coverage gap: skipped a changed file with an invalid path (%s)", label))
			continue
		}
		if diff.OldPath != "" && !validRepoPath(diff.OldPath) {
			label := invalidPathLabel(diff.OldPath)
			omit(label, "invalid_path")
			warnings = append(warnings, fmt.Sprintf(
				"review coverage gap: skipped a changed file with an invalid path (%s)", label))
			continue
		}
		if matcher.MatchPath(filePath) || matcher.MatchPath(strings.TrimSpace(diff.OldPath)) {
			ignored = append(ignored, filePath)
			continue
		}
		if maxFiles > 0 && len(files) >= maxFiles {
			omittedByFiles++
			omit(filePath, "max_files")
			continue
		}
		if diff.TooLarge || diff.Collapsed {
			// GitLab withheld the body. Record the path and reason so the
			// coverage gap is visible; never present it as reviewed content.
			cause := "too_large"
			if diff.Collapsed {
				cause = "collapsed"
			}
			omit(filePath, cause)
			warnings = append(warnings, fmt.Sprintf(
				"review coverage gap: GitLab did not provide a diff for %s (%s)", filePath, cause))
			continue
		}
		additions, deletions := countDiffLines(diff.Diff)
		if maxDiffLines > 0 && lines > 0 && lines+additions+deletions > maxDiffLines {
			omittedByLines++
			omit(filePath, "max_diff_lines")
			continue
		}
		status := "modified"
		switch {
		case diff.NewFile:
			status = "added"
		case diff.DeletedFile:
			status = "deleted"
		case diff.RenamedFile:
			status = "renamed"
		}
		oldPath := diff.OldPath
		if !validRepoPath(oldPath) {
			oldPath = ""
		}
		files = append(files, review.FileChange{
			Path:      filePath,
			OldPath:   oldPath,
			Status:    status,
			Diff:      diff.Diff,
			Additions: additions,
			Deletions: deletions,
			Generated: diff.GeneratedFile,
		})
		lines += additions + deletions
		summary.Files++
		summary.Additions += additions
		summary.Deletions += deletions
	}
	if omittedByFiles > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"review truncated: %d changed file(s) omitted by max_files", omittedByFiles))
	}
	if omittedByLines > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"review truncated: %d changed file(s) omitted by max_diff_lines", omittedByLines))
	}
	sort.Strings(ignored)
	return files, ignored, summary, warnings
}

// invalidPathLabel turns an unsafe upstream path into a bounded repository-
// relative label. The label is diagnostic only and must never be sent to
// repository tools as a real path.
func invalidPathLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "<invalid-path/empty>"
	}
	escaped := url.PathEscape(truncateRunes(value, 200))
	escaped = strings.ReplaceAll(escaped, "..", "%2E%2E")
	switch escaped {
	case "", ".", "..":
		escaped = "%2E"
	}
	escaped = truncateRunes(escaped, review.MaxPathBytes-len("<invalid-path/>"))
	return "<invalid-path/" + escaped + ">"
}

// omittedPathList renders a bounded, safe list of omitted paths for summaries.
func omittedPathList(omitted []review.OmittedFile, max int) string {
	if len(omitted) == 0 {
		return ""
	}
	if max <= 0 || max > len(omitted) {
		max = len(omitted)
	}
	shown := make([]string, 0, max)
	for _, file := range omitted[:max] {
		shown = append(shown, truncateRunes(file.Path, 120))
	}
	out := " [" + strings.Join(shown, ", ")
	if len(omitted) > max {
		out += fmt.Sprintf(", ... (+%d more)", len(omitted)-max)
	}
	return out + "]"
}

func validRepoPath(value string) bool {
	if value == "" || len(value) > review.MaxPathBytes {
		return false
	}
	if strings.ContainsAny(value, "\\\x00\r\n") || strings.HasPrefix(value, "/") {
		return false
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func countDiffLines(diff string) (int, int) {
	additions, deletions := 0, 0
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			additions++
		case strings.HasPrefix(line, "-"):
			deletions++
		}
	}
	return additions, deletions
}
