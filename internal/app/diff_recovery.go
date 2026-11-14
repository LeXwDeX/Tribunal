package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
)

const (
	// maxRecoveredFileBytes bounds one side of a recovered file body. GitLab
	// withholds a diff when the patch exceeds its diff size limit; a larger
	// body is not worth inlining into a review prompt.
	maxRecoveredFileBytes = 512 << 10
	// maxRecoveredReviewBytes bounds the total recovered bodies per review.
	maxRecoveredReviewBytes = 2 << 20
	// maxRecoveredFiles bounds the number of files recovered per review.
	maxRecoveredFiles = 20
	// maxRecoveredDiffBytes bounds one synthesized unified diff.
	maxRecoveredDiffBytes = 1 << 20
)

// applyDiffScope annotates the diff list with the merge request diff version
// scope, recovers bodies GitLab withheld for too_large or collapsed files, and
// rejects an empty list that GitLab reports as non-empty. A list without a
// reported scope cannot tell a complete collection from a GitLab-side
// truncation, so the collector always records what GitLab reported.
func (s *ReviewService) applyDiffScope(ctx context.Context, projectID int64, token string, version *gitlab.Version, changesCount string, diffs []gitlab.FileDiff) error {
	if version == nil {
		return errors.New("app: diff version is required for diff collection")
	}
	collection := diffCollectionScope(version, changesCount, len(diffs))
	if len(diffs) == 0 {
		if collection.Total > 0 || collection.Truncated {
			return fmt.Errorf("app: GitLab returned no diffs for a merge request with changed files (state %s)",
				safeDiffState(collection.State))
		}
		return nil
	}
	diffs[0].Collection = collection
	recoveredFiles, recoveredBytes := 0, 0
	for i := range diffs {
		if recoveredFiles >= maxRecoveredFiles || recoveredBytes >= maxRecoveredReviewBytes {
			break
		}
		diff := &diffs[i]
		if (!diff.TooLarge && !diff.Collapsed) || strings.TrimSpace(diff.Diff) != "" {
			continue
		}
		body, ok := s.recoverWithheldDiff(ctx, token, projectID, version, *diff)
		if !ok || recoveredBytes+len(body) > maxRecoveredReviewBytes {
			continue
		}
		diff.Diff = body
		// The body was recovered, so the file is reviewable content now.
		diff.TooLarge = false
		diff.Collapsed = false
		recoveredFiles++
		recoveredBytes += len(body)
	}
	return nil
}

// diffCollectionScope reports how much of the changed-path scope GitLab
// delivered. RealSize is the version's own count; changes_count is the merge
// request fallback when the version does not carry one.
func diffCollectionScope(version *gitlab.Version, changesCount string, provided int) *gitlab.DiffCollection {
	collection := &gitlab.DiffCollection{
		State:    strings.TrimSpace(version.State),
		RealSize: strings.TrimSpace(version.RealSize),
	}
	count, truncated := gitlab.ParseChangedFileCount(collection.RealSize)
	if count == 0 && !truncated {
		count, truncated = gitlab.ParseChangedFileCount(changesCount)
	}
	if count < provided {
		// GitLab cannot have reported fewer paths than it delivered; keep the
		// snapshot valid by trusting the larger, observed scope.
		count = provided
	}
	collection.Total = count
	collection.Truncated = truncated || count > provided || diffStateTruncated(collection.State)
	return collection
}

// diffStateTruncated reports whether a diff version state means the stored
// diffs are not a complete view of the change.
func diffStateTruncated(state string) bool {
	state = strings.TrimSpace(state)
	return state != "" && state != "collected"
}

// safeDiffState bounds the GitLab version state for an error or warning.
func safeDiffState(state string) string {
	state = strings.ToLower(strings.TrimSpace(state))
	var b strings.Builder
	for _, r := range state {
		if b.Len() >= 32 {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// safeCountLabel bounds a reported count value for a warning.
func safeCountLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	for _, r := range value {
		if (r < '0' || r > '9') && r != '+' {
			return "unknown"
		}
	}
	return truncateRunes(value, 32)
}

// recoverWithheldDiff rebuilds a bounded unified diff from the repository
// bodies at the version start and head. It returns ok=false when a body is
// unavailable, binary, or over the recovery bounds; the caller then keeps the
// file as an explicit omission.
func (s *ReviewService) recoverWithheldDiff(ctx context.Context, token string, projectID int64, version *gitlab.Version, diff gitlab.FileDiff) (string, bool) {
	if s == nil || s.gitlab == nil {
		return "", false
	}
	headPath := strings.TrimSpace(diff.NewPath)
	if headPath == "" {
		headPath = strings.TrimSpace(diff.OldPath)
	}
	basePath := strings.TrimSpace(diff.OldPath)
	if basePath == "" {
		basePath = headPath
	}
	if !validRepoPath(headPath) || !validRepoPath(basePath) {
		return "", false
	}
	var oldContent, newContent []byte
	haveOld, haveNew := false, false
	if !diff.DeletedFile {
		if !shaRE.MatchString(version.HeadSHA) {
			return "", false
		}
		body, err := s.gitlab.GetRawFile(ctx, token, projectID, headPath, version.HeadSHA)
		if err != nil {
			return "", false
		}
		newContent, haveNew = body, true
	}
	if !diff.NewFile {
		if !shaRE.MatchString(version.StartSHA) {
			return "", false
		}
		body, err := s.gitlab.GetRawFile(ctx, token, projectID, basePath, version.StartSHA)
		if err != nil {
			return "", false
		}
		oldContent, haveOld = body, true
	}
	if !haveOld && !haveNew {
		return "", false
	}
	return buildRecoveredDiff(oldContent, newContent)
}

// buildRecoveredDiff renders the whole-file hunk with common prefix and
// suffix kept as context. It never truncates a body: a diff that cannot be
// represented within the bounds is reported as unavailable instead.
func buildRecoveredDiff(oldContent, newContent []byte) (string, bool) {
	if len(oldContent) > maxRecoveredFileBytes || len(newContent) > maxRecoveredFileBytes {
		return "", false
	}
	if bytes.IndexByte(oldContent, 0) >= 0 || bytes.IndexByte(newContent, 0) >= 0 {
		return "", false
	}
	if !utf8.Valid(oldContent) || !utf8.Valid(newContent) {
		return "", false
	}
	oldLines := splitContentLines(oldContent)
	newLines := splitContentLines(newContent)
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}
	removed := len(oldLines) - prefix - suffix
	added := len(newLines) - prefix - suffix
	if removed == 0 && added == 0 {
		return "", false
	}
	oldStart, newStart := 1, 1
	if len(oldLines) == 0 {
		oldStart = 0
	}
	if len(newLines) == 0 {
		newStart = 0
	}
	var b strings.Builder
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", oldStart, len(oldLines), newStart, len(newLines))
	for _, line := range oldLines[:prefix] {
		b.WriteString(" " + line + "\n")
	}
	for _, line := range oldLines[prefix : len(oldLines)-suffix] {
		b.WriteString("-" + line + "\n")
	}
	for _, line := range newLines[prefix : len(newLines)-suffix] {
		b.WriteString("+" + line + "\n")
	}
	for _, line := range oldLines[len(oldLines)-suffix:] {
		b.WriteString(" " + line + "\n")
	}
	if b.Len() > maxRecoveredDiffBytes {
		return "", false
	}
	return b.String(), true
}

func splitContentLines(content []byte) []string {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines
}

// diffCollection returns the scope metadata recorded on the first diff.
func diffCollection(diffs []gitlab.FileDiff) *gitlab.DiffCollection {
	if len(diffs) == 0 {
		return nil
	}
	return diffs[0].Collection
}
