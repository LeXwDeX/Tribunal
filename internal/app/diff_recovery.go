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
	// maxRecoveredRawFileBytes bounds one side of a recovered file body. It is
	// larger than gitlab.MaxRawFileBytes so recovery can rebuild a diff for a
	// body GitLab withheld as too_large, while still capping memory.
	maxRecoveredRawFileBytes = int64(gitlab.MaxRecoveryFileBytes)
	// maxRecoveredReviewBytes bounds the total recovered bodies per review.
	maxRecoveredReviewBytes = 8 << 20
	// maxRecoveredFiles bounds the number of recovery attempts per review,
	// successful or not, so withheld bodies cannot trigger one request each.
	maxRecoveredFiles = 20
	// maxRecoveredDiffBytes bounds one synthesized unified diff.
	maxRecoveredDiffBytes = 4 << 20
	// maxRecoveredFetchBytes bounds the total raw bytes read for recovery in one
	// review, so a stream of withheld files cannot fetch an unbounded volume.
	maxRecoveredFetchBytes = 32 << 20
	// maxResourceLineBytes bounds the longest single line in a recovered body.
	// A longer line is treated as a non-diffable resource (an embedded blob, a
	// base64 image) rather than review content.
	maxResourceLineBytes = 8 << 10
	// recoveryContextLines is the number of unchanged lines kept around each
	// recovered change block.
	recoveryContextLines = 3
	// maxLineDiffCells bounds the LCS table used to align a large changed
	// middle. Above it, the middle is emitted as one whole change block instead
	// of a minimal edit, keeping memory bounded without producing a wrong diff.
	maxLineDiffCells = 8 << 20
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
	recoveredBytes, fetchedBytes, attempts := 0, 0, 0
	for i := range diffs {
		if attempts >= maxRecoveredFiles || fetchedBytes >= maxRecoveredFetchBytes {
			break
		}
		diff := &diffs[i]
		if (!diff.TooLarge && !diff.Collapsed) || strings.TrimSpace(diff.Diff) != "" {
			continue
		}
		attempts++
		body, fetched, ok, resource := s.recoverWithheldDiff(ctx, token, projectID, version, *diff)
		fetchedBytes += fetched
		if resource {
			// The body was withheld but recovery identified it as a
			// non-diffable resource, so it is an explicit omission rather than
			// reviewable content. Leave the withheld flag set; the prepare step
			// records the undiffable_resource reason from this marker.
			diff.Undiffable = true
			continue
		}
		if !ok || recoveredBytes+len(body) > maxRecoveredReviewBytes {
			continue
		}
		diff.Diff = body
		// The body was recovered, so the file is reviewable content now.
		diff.TooLarge = false
		diff.Collapsed = false
		diff.Undiffable = false
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
// bodies at the version start and head. The int reports how many raw bytes were
// read so the caller can bound total fetch volume. ok=false leaves the file as
// an explicit omission; resource=true means the body was identified as a
// non-diffable resource rather than review content.
func (s *ReviewService) recoverWithheldDiff(ctx context.Context, token string, projectID int64, version *gitlab.Version, diff gitlab.FileDiff) (body string, fetched int, ok, resource bool) {
	if s == nil || s.gitlab == nil {
		return "", 0, false, false
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
		return "", 0, false, false
	}
	var oldContent, newContent []byte
	haveOld, haveNew := false, false
	if !diff.DeletedFile {
		if !shaRE.MatchString(version.HeadSHA) {
			return "", 0, false, false
		}
		body, err := s.gitlab.GetRawFileSized(ctx, token, projectID, headPath, version.HeadSHA, maxRecoveredRawFileBytes)
		fetched += len(body)
		if err != nil {
			return "", fetched, false, false
		}
		newContent, haveNew = body, true
	}
	if !diff.NewFile {
		if !shaRE.MatchString(version.StartSHA) {
			return "", fetched, false, false
		}
		body, err := s.gitlab.GetRawFileSized(ctx, token, projectID, basePath, version.StartSHA, maxRecoveredRawFileBytes)
		fetched += len(body)
		if err != nil {
			return "", fetched, false, false
		}
		oldContent, haveOld = body, true
	}
	if !haveOld && !haveNew {
		return "", fetched, false, false
	}
	diffBody, built, resource := buildRecoveredDiff(oldContent, newContent)
	return diffBody, fetched, built, resource
}

// buildRecoveredDiff renders a bounded unified diff between the two bodies. It
// keeps recoveryContextLines of context around every change block and emits
// one @@ hunk per block with exact line numbers, so inline comments land on the
// right line. It returns ok=false (an honest omission) when a body is over the
// recovery bound, binary, or the rendered diff is too large; resource=true when
// a body is a non-diffable single-line resource.
func buildRecoveredDiff(oldContent, newContent []byte) (diff string, ok, resource bool) {
	// Length precheck before any string/line copies: a body the client accepted
	// but is still over the recovery bound is an honest omission.
	if int64(len(oldContent)) > maxRecoveredRawFileBytes || int64(len(newContent)) > maxRecoveredRawFileBytes {
		return "", false, false
	}
	if bytes.IndexByte(oldContent, 0) >= 0 || bytes.IndexByte(newContent, 0) >= 0 {
		return "", false, false
	}
	if !utf8.Valid(oldContent) || !utf8.Valid(newContent) {
		return "", false, false
	}
	// A single line larger than maxResourceLineBytes is an embedded blob (for
	// example a base64 image); diffing it line-by-line is meaningless.
	if longestLineBytes(oldContent) > maxResourceLineBytes || longestLineBytes(newContent) > maxResourceLineBytes {
		return "", false, true
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
	if len(oldLines)-prefix-suffix == 0 && len(newLines)-prefix-suffix == 0 {
		return "", false, false
	}
	ops := buildLineOps(oldLines, newLines, prefix, suffix)
	rendered, ok := renderRecoveredDiff(ops, oldLines, newLines)
	if !ok {
		return "", false, false
	}
	return rendered, true, false
}

// longestLineBytes returns the byte length of the longest newline-delimited
// segment in content.
func longestLineBytes(content []byte) int {
	maxLen, start := 0, 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			if n := i - start; n > maxLen {
				maxLen = n
			}
			start = i + 1
		}
	}
	if n := len(content) - start; n > maxLen {
		maxLen = n
	}
	return maxLen
}

type diffOpKind uint8

const (
	opEqual diffOpKind = iota
	opDelete
	opInsert
)

// diffOp is one line-level diff operation. oldIdx is the 0-based index into the
// old lines for opEqual and opDelete; newIdx is the 0-based index into the new
// lines for opEqual and opInsert.
type diffOp struct {
	kind   diffOpKind
	oldIdx int
	newIdx int
}

// buildLineOps returns a line-level diff for the whole file. The common prefix
// and suffix are emitted as equal ops; the middle is aligned with an LCS table
// when it fits maxLineDiffCells, otherwise it is emitted as a single whole
// change block. Either way the result is correct, only less minimal in the
// latter case.
func buildLineOps(oldLines, newLines []string, prefix, suffix int) []diffOp {
	ops := make([]diffOp, 0, len(oldLines)+len(newLines))
	for i := 0; i < prefix; i++ {
		ops = append(ops, diffOp{kind: opEqual, oldIdx: i, newIdx: i})
	}
	midOld := oldLines[prefix : len(oldLines)-suffix]
	midNew := newLines[prefix : len(newLines)-suffix]
	if int64(len(midOld))*int64(len(midNew)) <= maxLineDiffCells {
		ops = appendLCSEq(ops, midOld, midNew, prefix, prefix)
	} else {
		for i := range midOld {
			ops = append(ops, diffOp{kind: opDelete, oldIdx: prefix + i})
		}
		for i := range midNew {
			ops = append(ops, diffOp{kind: opInsert, newIdx: prefix + i})
		}
	}
	for i := 0; i < suffix; i++ {
		ops = append(ops, diffOp{kind: opEqual, oldIdx: len(oldLines) - suffix + i, newIdx: len(newLines) - suffix + i})
	}
	return ops
}

// appendLCSEq appends the LCS-based line diff of a and b onto ops, offsetting
// the indices by oldOff/newOff so they stay absolute into the whole file.
func appendLCSEq(ops []diffOp, a, b []string, oldOff, newOff int) []diffOp {
	n, m := len(a), len(b)
	w := m + 1
	dp := make([]int32, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i*w+j] = dp[(i+1)*w+(j+1)] + 1
				continue
			}
			x, y := dp[(i+1)*w+j], dp[i*w+(j+1)]
			if x >= y {
				dp[i*w+j] = x
			} else {
				dp[i*w+j] = y
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{kind: opEqual, oldIdx: oldOff + i, newIdx: newOff + j})
			i++
			j++
		case dp[(i+1)*w+j] >= dp[i*w+(j+1)]:
			ops = append(ops, diffOp{kind: opDelete, oldIdx: oldOff + i})
			i++
		default:
			ops = append(ops, diffOp{kind: opInsert, newIdx: newOff + j})
			j++
		}
	}
	for i < n {
		ops = append(ops, diffOp{kind: opDelete, oldIdx: oldOff + i})
		i++
	}
	for j < m {
		ops = append(ops, diffOp{kind: opInsert, newIdx: newOff + j})
		j++
	}
	return ops
}

// renderRecoveredDiff groups the ops into hunks with recoveryContextLines of
// context on each side, merging change blocks the context would overlap, and
// renders each hunk with an exact @@ header.
func renderRecoveredDiff(ops []diffOp, oldLines, newLines []string) (string, bool) {
	type block struct{ start, end int } // op index range [start, end)
	var blocks []block
	for i := 0; i < len(ops); {
		if ops[i].kind == opEqual {
			i++
			continue
		}
		s := i
		for i < len(ops) && ops[i].kind != opEqual {
			i++
		}
		blocks = append(blocks, block{s, i})
	}
	if len(blocks) == 0 {
		return "", false
	}
	merged := make([]block, 0, len(blocks))
	for _, blk := range blocks {
		if len(merged) > 0 {
			last := &merged[len(merged)-1]
			if blk.start-last.end <= 2*recoveryContextLines {
				last.end = blk.end
				continue
			}
		}
		merged = append(merged, blk)
	}
	var b strings.Builder
	for _, blk := range merged {
		hs := blk.start - recoveryContextLines
		if hs < 0 {
			hs = 0
		}
		he := blk.end + recoveryContextLines
		if he > len(ops) {
			he = len(ops)
		}
		writeRecoveredHunk(&b, ops, oldLines, newLines, hs, he)
		if b.Len() > maxRecoveredDiffBytes {
			return "", false
		}
	}
	return b.String(), true
}

// writeRecoveredHunk renders ops[hs:he] as one @@ hunk with exact line counts
// and line numbers. A hunk with no old line keeps oldStart 0 (pure insertion);
// one with no new line keeps newStart 0 (pure deletion).
func writeRecoveredHunk(b *strings.Builder, ops []diffOp, oldLines, newLines []string, hs, he int) {
	oldStart, newStart := 0, 0
	oldCount, newCount := 0, 0
	haveOld, haveNew := false, false
	for i := hs; i < he; i++ {
		switch ops[i].kind {
		case opEqual:
			oldCount++
			newCount++
			if !haveOld {
				oldStart = ops[i].oldIdx + 1
				haveOld = true
			}
			if !haveNew {
				newStart = ops[i].newIdx + 1
				haveNew = true
			}
		case opDelete:
			oldCount++
			if !haveOld {
				oldStart = ops[i].oldIdx + 1
				haveOld = true
			}
		case opInsert:
			newCount++
			if !haveNew {
				newStart = ops[i].newIdx + 1
				haveNew = true
			}
		}
	}
	fmt.Fprintf(b, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
	for i := hs; i < he; i++ {
		switch ops[i].kind {
		case opEqual:
			b.WriteString(" " + oldLines[ops[i].oldIdx] + "\n")
		case opDelete:
			b.WriteString("-" + oldLines[ops[i].oldIdx] + "\n")
		case opInsert:
			b.WriteString("+" + newLines[ops[i].newIdx] + "\n")
		}
	}
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
