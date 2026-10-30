package gitlab

import (
	"regexp"
	"strconv"
	"strings"
)

const maxIndexedLine = int64(1<<31 - 1)

// diffLine is the pair of line numbers for one line inside a hunk. A new line
// has oldLine zero; a deleted line has newLine zero.
type diffLine struct {
	oldLine int
	newLine int
}

type indexedFile struct {
	oldPath string
	newPath string
	byOld   map[int]diffLine
	byNew   map[int]diffLine
}

// DiffIndex maps (path, line, side) to a GitLab discussion position. Only
// lines inside a hunk are indexed; anything else must degrade to the summary.
// Paths are canonical per side: an old path answers only SideOld and a new
// path only SideNew. Cross-side rename aliases are unsafe because omitted
// diffs disappear before BuildDiffIndex, so an alias could route a comment to
// a different file. A nil entry records a withheld path and fails closed.
type DiffIndex struct {
	version  Version
	oldFiles map[string]*indexedFile
	newFiles map[string]*indexedFile
}

var hunkHeaderRE = regexp.MustCompile(`^@@ -([0-9]+)(,([0-9]+))? \+([0-9]+)(,([0-9]+))? @@`)

// BuildDiffIndex indexes new, deleted, and context lines. Collapsed, binary,
// oversized, or malformed diffs are skipped so a bad file cannot produce a
// position GitLab rejects or a panic.
func BuildDiffIndex(version Version, diffs []FileDiff) *DiffIndex {
	index := &DiffIndex{
		version:  version,
		oldFiles: make(map[string]*indexedFile),
		newFiles: make(map[string]*indexedFile),
	}
	if !validSHA(version.BaseSHA) || !validSHA(version.StartSHA) || !validSHA(version.HeadSHA) {
		return index
	}
	for _, diff := range diffs {
		oldPath, newPath := diffPaths(diff)
		if validateRepoPath(oldPath) != nil || validateRepoPath(newPath) != nil {
			continue
		}
		var file *indexedFile
		if !diff.Collapsed && !diff.TooLarge {
			file, _ = buildFileIndex(diff)
		}
		// Duplicate canonical paths keep the first diff, matching the previous
		// first-wins behavior. A nil entry is a withheld path.
		if _, exists := index.oldFiles[oldPath]; !exists {
			index.oldFiles[oldPath] = file
		}
		if _, exists := index.newFiles[newPath]; !exists {
			index.newFiles[newPath] = file
		}
	}
	return index
}

func diffPaths(diff FileDiff) (string, string) {
	oldPath, newPath := diff.OldPath, diff.NewPath
	if oldPath == "" {
		oldPath = newPath
	}
	if newPath == "" {
		newPath = oldPath
	}
	return oldPath, newPath
}

func buildFileIndex(diff FileDiff) (*indexedFile, bool) {
	oldPath, newPath := diffPaths(diff)
	if validateRepoPath(oldPath) != nil || validateRepoPath(newPath) != nil || diff.Diff == "" {
		return nil, false
	}
	if isBinaryDiff(diff.Diff) {
		return nil, false
	}

	file := &indexedFile{
		oldPath: oldPath,
		newPath: newPath,
		byOld:   make(map[int]diffLine),
		byNew:   make(map[int]diffLine),
	}
	lines := strings.Split(diff.Diff, "\n")
	var (
		inHunk               bool
		seenHunk             bool
		oldLine, newLine     int
		oldRemain, newRemain int
	)
	for _, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		if inHunk {
			if strings.HasPrefix(line, `\`) {
				continue
			}
			if line == "" || line[0] == '@' {
				return nil, false
			}
			switch line[0] {
			case ' ':
				file.byOld[oldLine] = diffLine{oldLine: oldLine, newLine: newLine}
				file.byNew[newLine] = diffLine{oldLine: oldLine, newLine: newLine}
				oldLine++
				newLine++
				oldRemain--
				newRemain--
			case '-':
				file.byOld[oldLine] = diffLine{oldLine: oldLine}
				oldLine++
				oldRemain--
			case '+':
				file.byNew[newLine] = diffLine{newLine: newLine}
				newLine++
				newRemain--
			default:
				return nil, false
			}
			if oldRemain < 0 || newRemain < 0 {
				return nil, false
			}
			if oldRemain == 0 && newRemain == 0 {
				inHunk = false
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, `\`) {
			continue
		}
		if strings.HasPrefix(line, "@@") {
			match := hunkHeaderRE.FindStringSubmatch(line)
			if match == nil {
				return nil, false
			}
			oldStart, ok := parseIndexNumber(match[1])
			if !ok {
				return nil, false
			}
			oldCount, ok := parseIndexCount(match[3])
			if !ok {
				return nil, false
			}
			newStart, ok := parseIndexNumber(match[4])
			if !ok {
				return nil, false
			}
			newCount, ok := parseIndexCount(match[6])
			if !ok {
				return nil, false
			}
			if oldStart > maxIndexedLine-oldCount || newStart > maxIndexedLine-newCount {
				return nil, false
			}
			oldLine, newLine = int(oldStart), int(newStart)
			oldRemain, newRemain = int(oldCount), int(newCount)
			inHunk = oldRemain > 0 || newRemain > 0
			seenHunk = true
			continue
		}
		// Metadata and file headers are legal only before the first hunk.
		// After a hunk completes, +++/--- lines are overlong content and a
		// single-file FileDiff must not produce a position for them.
		if !seenHunk && isDiffMetadataLine(line) {
			continue
		}
		return nil, false
	}
	if inHunk {
		return nil, false
	}
	return file, true
}

func isDiffMetadataLine(line string) bool {
	for _, prefix := range []string{
		"diff --git ", "index ", "new file mode ", "deleted file mode ",
		"old mode ", "new mode ", "similarity index ", "dissimilarity index ",
		"rename from ", "rename to ", "copy from ", "copy to ",
		"--- ", "+++ ", "Binary files ", "GIT binary patch",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func parseIndexNumber(value string) (int64, bool) {
	n, err := strconv.ParseInt(value, 10, 32)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// parseIndexCount returns 1 for an omitted count (unified diff shorthand) and
// treats an explicitly empty capture as omitted.
func parseIndexCount(value string) (int64, bool) {
	if value == "" {
		return 1, true
	}
	return parseIndexNumber(value)
}

func isBinaryDiff(diff string) bool {
	for _, line := range strings.Split(diff, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		return strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch")
	}
	return false
}

// Locate maps a path and line to a position. Deleted lines are reachable only
// with SideOld, added lines only with SideNew, and context lines with both.
func (d *DiffIndex) Locate(path string, line int, side Side) (Position, bool) {
	if d == nil || line <= 0 || path == "" {
		return Position{}, false
	}
	if !validSHA(d.version.BaseSHA) || !validSHA(d.version.StartSHA) || !validSHA(d.version.HeadSHA) {
		return Position{}, false
	}
	var (
		file *indexedFile
		ref  diffLine
		ok   bool
	)
	switch side {
	case SideNew:
		file = d.newFiles[path]
		if file != nil {
			ref, ok = file.byNew[line]
		}
	case SideOld:
		file = d.oldFiles[path]
		if file != nil {
			ref, ok = file.byOld[line]
		}
	default:
		return Position{}, false
	}
	if file == nil || !ok {
		return Position{}, false
	}
	return Position{
		BaseSHA:  d.version.BaseSHA,
		StartSHA: d.version.StartSHA,
		HeadSHA:  d.version.HeadSHA,
		OldPath:  file.oldPath,
		NewPath:  file.newPath,
		OldLine:  ref.oldLine,
		NewLine:  ref.newLine,
	}, true
}
