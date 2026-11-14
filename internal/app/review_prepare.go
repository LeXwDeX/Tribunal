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
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func (s *ReviewService) incrementalRange(ctx context.Context, projectID int64, payload reviewRunPayload, control *store.ReviewControl, head string, files []review.FileChange) *review.IncrementalRange {
	if (payload.Trigger != "mr_update" && payload.Trigger != catchUpTrigger) || payload.Full {
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
	profileName := s.profileNameFor(project, "", cfg.Profile, inputs.MR.Labels, false)
	fingerprint := sha256Hex(strings.Join([]string{
		inputs.Version.HeadSHA, reviewConfigHash(cfg), profileName,
	}, "\n"))
	verdict, statusDetail, summaryText, incompleteReason, warnings := emptyReviewOutcome(inputs, cfg, summary, ignored)
	summaryInput := review.SummaryInput{
		Verdict:          verdict,
		Summary:          summaryText,
		IncompleteReason: incompleteReason,
		Fingerprint:      fingerprint,
		HeadSHA:          inputs.Version.HeadSHA,
		Profile:          profileName,
		Warnings:         warnings,
		Generated:        summary.Generated,
		GeneratedPaths:   summary.GeneratedPaths,
	}
	if summary.Truncated || len(summary.Omitted) > 0 {
		total := summary.Total
		if total == 0 {
			total = summary.Files + len(summary.Omitted) + len(inputs.Diffs)
		}
		reason := fmt.Sprintf("omitted %d of %d changed file(s) (included=%d, ignored=%d, generated=%d)",
			len(summary.Omitted), total, summary.Files, len(ignored), summary.Generated)
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
		statusDetail = store.StatusDetailSnapshotOmission
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
	var status *gitlab.CommitStatus
	if summaryInput.Verdict == review.VerdictIncomplete && cfg.Settings.CommitStatus {
		status = &gitlab.CommitStatus{State: "failed", Name: "ai-review", Description: statusDescription(review.VerdictIncomplete, nil)}
	}
	// Summary and status share the MR lock and recheck the generation and head
	// before each request. A control change can still race an in-flight request.
	if err := s.publisher.publishStandaloneReviewLocked(ctx, project.ID, control.MRIID, control.Generation, inputs.Version.HeadSHA, token, fingerprint, body, status); err != nil {
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
		inputs.Version.HeadSHA, "", state, statusDetail, noteID, reaction)
	return nil
}

// emptyReviewOutcome classifies a review with no reviewable file. The settled
// status keeps a recognizable detail so the console can list the run and show
// why it ended this way.
func emptyReviewOutcome(inputs *reviewInputs, cfg effectiveReviewConfig, summary review.DiffSummary, ignored []string) (verdict, statusDetail, summaryText, incompleteReason string, warnings []string) {
	verdict = review.VerdictPass
	statusDetail = store.StatusDetailNoReviewable
	summaryText = "无需评审：所有变更文件均被忽略规则过滤。"
	warnings = append([]string(nil), cfg.Warnings...)
	if len(inputs.Diffs) == 0 {
		return review.VerdictIncomplete, store.StatusDetailEmptyDiff,
			"没有可评审的变更：MR 的 diff 为空。", "empty diff",
			append(warnings, "no changed files in merge request")
	}
	if summary.Generated > 0 && summary.Files == 0 && len(summary.Omitted) == 0 {
		if len(ignored) > 0 {
			summaryText = "无需评审：变更文件均为生成文件或被忽略规则过滤。"
		} else {
			summaryText = "无需评审：变更文件均为生成文件。"
		}
		warnings = append(warnings, fmt.Sprintf(
			"no reviewable files: %d generated file(s) were skipped", summary.Generated))
		return verdict, statusDetail, summaryText, "", warnings
	}
	return verdict, statusDetail, summaryText, "", append(warnings, "no reviewable files after ignore rules")
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
	// GitLab reports the changed-path count of the diff version. When the
	// collector recorded it, it is authoritative over the page we received.
	if collection := diffCollection(diffs); collection != nil {
		if collection.Total > 0 {
			summary.Total = collection.Total
		}
		if collection.Truncated {
			summary.Truncated = true
			warnings = append(warnings, fmt.Sprintf(
				"review coverage gap: GitLab diff collection is incomplete (state=%s, real_size=%s): %d of %d changed file(s) were provided; the remaining files were not reviewed",
				safeDiffState(collection.State), safeCountLabel(collection.RealSize),
				len(diffs), max(summary.Total, len(diffs))))
		}
	}
	generated := make([]string, 0)
	lines := 0
	omittedByFiles := 0
	omittedByLines := 0
	omit := func(filePath, reason string) {
		summary.Truncated = true
		summary.Omitted = append(summary.Omitted, review.OmittedFile{Path: filePath, Reason: reason})
	}
	// omitDiff also records the pre-rename path so withholding can follow it.
	omitDiff := func(filePath, reason string, diff gitlab.FileDiff) {
		omit(filePath, reason)
		if old := strings.TrimSpace(diff.OldPath); old != filePath && validRepoPath(old) {
			summary.Omitted[len(summary.Omitted)-1].OldPath = old
		}
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
		if diff.RenamedFile && (diff.OldPath == "" || diff.NewPath == "") {
			omit(filePath, "missing_rename_path")
			warnings = append(warnings, fmt.Sprintf("review coverage gap: rename paths are incomplete for %s", filePath))
			continue
		}
		if diff.GeneratedFile {
			// Generated files are not hand-written source: planning and
			// reviewing them would spend packages on machine output. The
			// counts and paths stay visible as a warning.
			summary.Generated++
			generated = append(generated, filePath)
			continue
		}
		if maxFiles > 0 && len(files) >= maxFiles {
			omittedByFiles++
			omitDiff(filePath, "max_files", diff)
			continue
		}
		if diff.TooLarge || diff.Collapsed {
			// GitLab withheld the body and the collector could not recover it.
			// Record the path and reason so the omission is visible; never
			// present it as reviewed content.
			cause := "too_large"
			if diff.Collapsed {
				cause = "collapsed"
			}
			omitDiff(filePath, cause, diff)
			warnings = append(warnings, fmt.Sprintf(
				"review coverage gap: GitLab withheld the diff for %s (%s); the file was not reviewed", filePath, cause))
			continue
		}
		additions, deletions := countDiffLines(diff.Diff)
		if maxDiffLines > 0 && lines+additions+deletions > maxDiffLines {
			omittedByLines++
			omitDiff(filePath, "max_diff_lines", diff)
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
	if len(generated) > 0 {
		sort.Strings(generated)
		recorded := generated
		if len(recorded) > review.MaxGeneratedPaths {
			recorded = recorded[:review.MaxGeneratedPaths]
		}
		summary.GeneratedPaths = append([]string(nil), recorded...)
		warnings = append(warnings, fmt.Sprintf(
			"review skipped %d generated file(s), which were not reviewed: %s",
			len(generated), boundedFilePathList(generated, 12)))
	}
	sort.Strings(ignored)
	return files, ignored, summary, warnings
}

// boundedFilePathList renders a bounded, safe path list for warnings.
func boundedFilePathList(paths []string, max int) string {
	if len(paths) == 0 {
		return ""
	}
	if max <= 0 || max > len(paths) {
		max = len(paths)
	}
	shown := make([]string, 0, max)
	for _, filePath := range paths[:max] {
		shown = append(shown, truncateRunes(filePath, 120))
	}
	out := strings.Join(shown, ", ")
	if len(paths) > max {
		out += fmt.Sprintf(", ... (+%d more)", len(paths)-max)
	}
	return out
}

// ignoredOldPaths returns the pre-rename paths of ignored files and whether
// that metadata is complete (no ignored rename or copy lacks a side).
func ignoredOldPaths(diffs []gitlab.FileDiff, ignores []string) ([]string, bool) {
	matcher := newIgnoreMatcher(ignores)
	out := make([]string, 0)
	complete := true
	for _, diff := range diffs {
		filePath := strings.TrimSpace(diff.NewPath)
		if filePath == "" {
			filePath = strings.TrimSpace(diff.OldPath)
		}
		old := strings.TrimSpace(diff.OldPath)
		if !validRepoPath(filePath) || (old != "" && !validRepoPath(old)) {
			continue
		}
		if !matcher.MatchPath(filePath) && !matcher.MatchPath(old) {
			continue
		}
		if diff.RenamedFile && (old == "" || strings.TrimSpace(diff.NewPath) == "") {
			complete = false
		}
		if old != "" && old != filePath {
			out = append(out, old)
		}
	}
	sort.Strings(out)
	return out, complete
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

var diffHunkHeaderRE = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@`)

// countDiffLines counts added and deleted lines conservatively. Hunk header
// line counts are not trusted: incomplete, overflowed, or malformed diffs
// must not hide content from max_diff_lines. File headers (--- / +++) are
// ignored only outside hunks, when they form a pair and the next line is
// structural. Hunk content may legitimately start with the same prefix, so
// counting inside a hunk stays conservative.
func countDiffLines(diff string) (int, int) {
	lines := strings.Split(diff, "\n")
	additions, deletions := 0, 0
	inHunk := false
	headerAllowed := true
	skipFileHeaderTail := false
	for i, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		if skipFileHeaderTail {
			skipFileHeaderTail = false
			if strings.HasPrefix(line, "+++ ") {
				headerAllowed = false
				continue
			}
		}
		if strings.HasPrefix(line, "diff --git ") {
			inHunk = false
			headerAllowed = true
			continue
		}
		if strings.HasPrefix(line, "@@ ") {
			inHunk = diffHunkHeaderRE.MatchString(line)
			headerAllowed = false
			continue
		}
		if line == "" {
			inHunk = false
			continue
		}
		if isDiffCountMetadata(line) {
			continue
		}
		if !inHunk && headerAllowed && isFileHeaderPair(lines, i) {
			skipFileHeaderTail = true
			headerAllowed = false
			continue
		}
		headerAllowed = false
		switch line[0] {
		case '\\', ' ':
			continue
		case '+':
			additions++
		case '-':
			deletions++
		default:
			inHunk = false
		}
	}
	return additions, deletions
}

func isDiffCountMetadata(line string) bool {
	for _, prefix := range []string{
		"index ", "new file mode ", "deleted file mode ", "old mode ", "new mode ",
		"similarity index ", "dissimilarity index ", "rename from ", "rename to ",
		"copy from ", "copy to ", "Binary files ", "GIT binary patch",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func isFileHeaderPair(lines []string, index int) bool {
	if !strings.HasPrefix(strings.TrimSuffix(lines[index], "\r"), "--- ") {
		return false
	}
	if index+1 >= len(lines) || !strings.HasPrefix(strings.TrimSuffix(lines[index+1], "\r"), "+++ ") {
		return false
	}
	if index+2 >= len(lines) {
		return true
	}
	next := strings.TrimSuffix(lines[index+2], "\r")
	return next == "" || strings.HasPrefix(next, "@@ ") ||
		strings.HasPrefix(next, "diff --git ") || strings.HasPrefix(next, "Binary files ") ||
		strings.HasPrefix(next, "GIT binary patch")
}
