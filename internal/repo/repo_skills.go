package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	repositorySkillsRoot      = ".claude/skills"
	maxRepositorySkillEntries = 4096
	maxRepositorySkillFile    = 1 << 20
	maxRepositorySkillsBytes  = 64 << 20
	maxRepositoryTreeOutput   = 4 << 20
)

var errRepositorySkills = errors.New("repo: repository skill tree is unavailable or unsafe")

type repositorySkillEntry struct {
	path string
	oid  string
	size int64
}

// ResolveCommitRef returns the direct commit object named by a full ref.
// It does not authenticate the ref; callers that fetch into a mutable ref must
// hold the mirror lock from the fetch until this method returns.
func (g *Git) ResolveCommitRef(ctx context.Context, dir, ref string) (string, error) {
	if err := validateAbsPath(dir); err != nil {
		return "", err
	}
	if err := validateRef(ref); err != nil {
		return "", err
	}
	if err := requireExistingDir(dir); err != nil {
		return "", err
	}
	out, err := g.runSkillBounded(ctx, dir, 256, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return "", err
	}
	sha := strings.ToLower(strings.TrimSpace(string(out)))
	if ValidateSHA(sha) != nil {
		return "", errRepositorySkills
	}
	commit, err := g.runSkillBounded(ctx, dir, 256, "rev-parse", "--verify", "--quiet", sha+"^{commit}")
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(strings.TrimSpace(string(commit)), sha) {
		return "", errRepositorySkills
	}
	return sha, nil
}

// ReadRepositorySkillConfig reads the review configuration from the same
// pinned commit as repository skills. A missing file is not an error.
func (g *Git) ReadRepositorySkillConfig(ctx context.Context, dir, sha string) ([]byte, bool, error) {
	if err := validateAbsPath(dir); err != nil {
		return nil, false, err
	}
	if err := ValidateSHA(sha); err != nil {
		return nil, false, err
	}
	if err := requireExistingDir(dir); err != nil {
		return nil, false, err
	}
	if err := g.verifySkillCommit(ctx, dir, sha); err != nil {
		return nil, false, err
	}
	const configPath = ".gitlab-review.yml"
	out, err := g.runSkillBounded(ctx, dir, 1024, "ls-tree", "-z", sha, "--", configPath)
	if err != nil {
		return nil, false, err
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	fields, name, ok := parseSkillTreeLine(out)
	if !ok || name != configPath || len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" || ValidateSHA(fields[2]) != nil {
		return nil, false, errRepositorySkills
	}
	sizeOut, err := g.runSkillBounded(ctx, dir, 256, "cat-file", "-s", fields[2])
	if err != nil {
		return nil, false, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeOut)), 10, 64)
	if err != nil || size < 0 || size > maxRepositorySkillFile {
		return nil, false, errRepositorySkills
	}
	data, err := g.runSkillBounded(ctx, dir, int(size)+1, "cat-file", "blob", fields[2])
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) != size {
		return nil, false, errRepositorySkills
	}
	return data, true, nil
}

func (g *Git) verifySkillCommit(ctx context.Context, dir, sha string) error {
	resolved, err := g.runSkillBounded(ctx, dir, 256, "rev-parse", "--verify", "--quiet", sha+"^{commit}")
	if err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(string(resolved)), sha) {
		return errRepositorySkills
	}
	return nil
}

// ReadRepositorySkills returns only non-ignored regular files from a pinned
// commit's .claude/skills tree. Paths in the result are relative to that tree.
// The caller must first authenticate the project and verify that sha is the
// fetched target-branch tip. deny must include the effective ignore policy and
// any additional excluded paths, such as the other side of an ignored rename.
// This method does not authorize or mount the returned files.
func (g *Git) ReadRepositorySkills(ctx context.Context, dir, sha string, deny func(string) bool) (map[string][]byte, error) {
	if err := validateAbsPath(dir); err != nil {
		return nil, err
	}
	if err := ValidateSHA(sha); err != nil {
		return nil, err
	}
	if deny == nil {
		return nil, errRepositorySkills
	}
	if err := requireExistingDir(dir); err != nil {
		return nil, err
	}
	if err := g.verifySkillCommit(ctx, dir, sha); err != nil {
		return nil, err
	}
	result := make(map[string][]byte)
	if deniedRepositorySkillPath(deny, repositorySkillsRoot) {
		return result, nil
	}
	// Verify both ancestors. A symlink or gitlink at an ancestor must not be
	// mistaken for an absent tree by ls-tree's pathspec traversal.
	for _, ancestor := range []string{".claude", repositorySkillsRoot} {
		out, err := g.runSkillBounded(ctx, dir, maxRepositoryTreeOutput, "ls-tree", "-z", sha, "--", ancestor)
		if err != nil {
			return nil, err
		}
		if len(out) == 0 {
			return result, nil
		}
		fields, name, ok := parseSkillTreeLine(out)
		if !ok || name != ancestor || len(fields) != 3 || fields[0] != "040000" || fields[1] != "tree" || ValidateSHA(fields[2]) != nil {
			return nil, errRepositorySkills
		}
	}
	out, err := g.runSkillBounded(ctx, dir, maxRepositoryTreeOutput, "ls-tree", "-r", "-t", "-z", sha, "--", repositorySkillsRoot+"/")
	if err != nil {
		return nil, err
	}
	var entries []repositorySkillEntry
	var entryCount int
	for len(out) > 0 {
		index := bytes.IndexByte(out, 0)
		if index < 0 {
			return nil, errRepositorySkills
		}
		line := out[:index+1]
		out = out[index+1:]
		fields, name, ok := parseSkillTreeLine(line)
		if ok && (name == ".claude" || name == repositorySkillsRoot) {
			continue
		}
		if !ok || !safeRepositorySkillPath(name) || !strings.HasPrefix(name, repositorySkillsRoot+"/") {
			return nil, errRepositorySkills
		}
		entryCount++
		if entryCount > maxRepositorySkillEntries {
			return nil, errRepositorySkills
		}
		if deniedRepositorySkillPath(deny, name) {
			continue
		}
		if len(fields) != 3 || ValidateSHA(fields[2]) != nil {
			return nil, errRepositorySkills
		}
		if fields[0] == "040000" && fields[1] == "tree" {
			continue
		}
		if (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" {
			return nil, errRepositorySkills
		}
		entries = append(entries, repositorySkillEntry{path: strings.TrimPrefix(name, repositorySkillsRoot+"/"), oid: fields[2]})
	}
	if len(entries) == 0 {
		return result, nil
	}
	var objectIDs strings.Builder
	for _, entry := range entries {
		objectIDs.WriteString(entry.oid)
		objectIDs.WriteByte('\n')
	}
	ids := objectIDs.String()
	metadata, err := g.runSkillBoundedInput(ctx, dir, maxRepositoryTreeOutput, ids, "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	if err != nil {
		return nil, err
	}
	var total int64
	for i := range entries {
		line, rest, ok := bytes.Cut(metadata, []byte{'\n'})
		if !ok {
			return nil, errRepositorySkills
		}
		metadata = rest
		size, ok := skillObjectSize(line, entries[i].oid)
		if !ok || size > maxRepositorySkillFile {
			return nil, errRepositorySkills
		}
		total += size
		if total > maxRepositorySkillsBytes {
			return nil, errRepositorySkills
		}
		entries[i].size = size
	}
	if len(metadata) != 0 {
		return nil, errRepositorySkills
	}
	objects, err := g.runSkillBoundedInput(ctx, dir, maxRepositorySkillsBytes+maxRepositoryTreeOutput, ids, "cat-file", "--batch=%(objectname) %(objecttype) %(objectsize)")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		line, rest, ok := bytes.Cut(objects, []byte{'\n'})
		size, valid := skillObjectSize(line, entry.oid)
		if !ok || !valid || size != entry.size || int64(len(rest)) < size+1 || rest[size] != '\n' {
			return nil, errRepositorySkills
		}
		result[entry.path] = rest[:size:size]
		objects = rest[size+1:]
	}
	if len(objects) != 0 {
		return nil, errRepositorySkills
	}
	return result, nil
}

func skillObjectSize(line []byte, oid string) (int64, bool) {
	fields := strings.Fields(string(line))
	if len(fields) != 3 || !strings.EqualFold(fields[0], oid) || fields[1] != "blob" {
		return 0, false
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	return size, err == nil && size >= 0
}

func deniedRepositorySkillPath(deny func(string) bool, name string) bool {
	parts := strings.Split(name, "/")
	for i := range parts {
		if deny(strings.Join(parts[:i+1], "/")) {
			return true
		}
	}
	return false
}

func safeRepositorySkillPath(name string) bool {
	if validateRepoPath(name) != nil || !utf8.ValidString(name) || len(name) > len(repositorySkillsRoot)+1+512 || strings.ContainsRune(name, ':') {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".git") {
			return false
		}
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func parseSkillTreeLine(line []byte) ([]string, string, bool) {
	if len(line) == 0 || line[len(line)-1] != 0 || bytes.IndexByte(line[:len(line)-1], 0) >= 0 {
		return nil, "", false
	}
	metadata, name, ok := bytes.Cut(line[:len(line)-1], []byte{'\t'})
	return strings.Fields(string(metadata)), string(name), ok
}

type skillOutputBuffer struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
}

func (b *skillOutputBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buf.Len() {
		b.exceeded = true
		return 0, errRepositorySkills
	}
	return b.buf.Write(p)
}

func (g *Git) runSkillBounded(ctx context.Context, dir string, limit int, args ...string) ([]byte, error) {
	return g.runSkillBoundedInput(ctx, dir, limit, "", args...)
}

func (g *Git) runSkillBoundedInput(ctx context.Context, dir string, limit int, input string, args ...string) ([]byte, error) {
	env, err := g.buildEnv(Auth{})
	if err != nil {
		return nil, errRepositorySkills
	}
	full := append([]string{"-C", dir, "--no-replace-objects"}, globalArgs()...)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, g.bin(), full...)
	cmd.Env = env
	cmd.WaitDelay = gitWaitDelay
	cmd.Stdin = strings.NewReader(input)
	out := &skillOutputBuffer{limit: limit}
	cmd.Stdout, cmd.Stderr = out, io.Discard
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("repo: repository skill tree: %w", ctx.Err())
	}
	if err != nil || out.exceeded {
		return nil, errRepositorySkills
	}
	return out.buf.Bytes(), nil
}
