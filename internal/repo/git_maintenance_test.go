package repo

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestMergeRequestRefsListAndDelete covers the mirror ref maintenance used by
// the periodic workspace reclaim (#459): only the canonical MR head namespace is
// listed, deletion validates each ref, and an automatic gc succeeds afterwards.
func TestMergeRequestRefsListAndDelete(t *testing.T) {
	ctx := context.Background()
	source := initRepo(t)
	sha := commitFile(t, source, "a.txt", "one\n", "first")
	root := t.TempDir()
	mirror := filepath.Join(root, "mirror.git")
	g := &Git{}
	if err := g.EnsureMirror(ctx, mirror, "https://example.com/group/repo.git", Auth{}); err != nil {
		t.Fatal(err)
	}
	pushToMirror(t, source, mirror)
	gitCmd(t, mirror, "update-ref", "refs/merge-requests/12/head", sha)
	gitCmd(t, mirror, "update-ref", "refs/merge-requests/7/head", sha)
	gitCmd(t, mirror, "update-ref", "refs/unrelated/9/head", sha)

	refs, err := g.MergeRequestRefs(ctx, mirror)
	if err != nil {
		t.Fatalf("MergeRequestRefs: %v", err)
	}
	if len(refs) != 2 || refs[0].IID != 7 || refs[1].IID != 12 {
		t.Fatalf("merge request refs = %+v", refs)
	}
	if refs[0].Name != "refs/merge-requests/7/head" {
		t.Fatalf("ref name = %q", refs[0].Name)
	}

	removed, err := g.DeleteMergeRequestRefs(ctx, mirror, refs[:1])
	if err != nil || removed != 1 {
		t.Fatalf("DeleteMergeRequestRefs = %d, %v", removed, err)
	}
	remaining, err := g.MergeRequestRefs(ctx, mirror)
	if err != nil || len(remaining) != 1 || remaining[0].IID != 12 {
		t.Fatalf("remaining refs = %+v, %v", remaining, err)
	}
	if _, err := g.DeleteMergeRequestRefs(ctx, mirror, []MergeRequestRef{{IID: 1, Name: "refs/heads/main"}}); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("invalid ref deletion = %v, want ErrInvalidRef", err)
	}
	if err := g.GC(ctx, mirror); err != nil {
		t.Fatalf("GC: %v", err)
	}
	out := gitCmd(t, mirror, "for-each-ref", "--format=%(refname)", "refs/merge-requests/")
	if strings.Contains(out, "refs/merge-requests/7/head") {
		t.Fatalf("deleted ref still present: %s", out)
	}
	if !strings.Contains(out, "refs/merge-requests/12/head") {
		t.Fatalf("protected ref was removed: %s", out)
	}
}

func TestMergeRequestRefsRejectsMissingMirror(t *testing.T) {
	g := &Git{}
	missing := filepath.Join(t.TempDir(), "absent.git")
	if _, err := g.MergeRequestRefs(context.Background(), missing); err == nil {
		t.Fatal("MergeRequestRefs accepted a missing mirror")
	}
	if err := g.GC(context.Background(), missing); err == nil {
		t.Fatal("GC accepted a missing mirror")
	}
}
