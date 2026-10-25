package review

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
)

const (
	testHeadSHA  = "1111111111111111111111111111111111111111"
	testBaseSHA  = "2222222222222222222222222222222222222222"
	testStartSHA = "3333333333333333333333333333333333333333"
	testLastSHA  = "4444444444444444444444444444444444444444"
)

func validDossier(t *testing.T) *Dossier {
	t.Helper()
	d, err := BuildDossier(DossierInput{
		Project: ProjectRef{ID: 42, Path: "group/project"},
		MR: MRMeta{
			IID:          7,
			Title:        "Add parser",
			SourceBranch: "feature/parser",
			TargetBranch: "main",
		},
		DiffVersion: gitlab.Version{BaseSHA: testBaseSHA, StartSHA: testStartSHA, HeadSHA: testHeadSHA},
		DiffSummary: DiffSummary{Files: 2, Additions: 12, Deletions: 3},
		Files: []FileChange{
			{Path: "a.go", Status: "modified", Diff: "@@ -1 +1 @@\n-a\n+b\n", Additions: 1, Deletions: 1},
			{Path: "b.go", Status: "added", Diff: "+package b\n", Additions: 1},
		},
		GeneratedAt: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("BuildDossier: %v", err)
	}
	return d
}

func TestDossierHashAndValidation(t *testing.T) {
	d := validDossier(t)
	first, err := DossierHash(d)
	if err != nil {
		t.Fatalf("DossierHash: %v", err)
	}
	if len(first) != 64 {
		t.Fatalf("hash length = %d, want 64", len(first))
	}
	second, err := DossierHash(d)
	if err != nil {
		t.Fatalf("DossierHash again: %v", err)
	}
	if first != second {
		t.Fatalf("hash is not deterministic")
	}
	changed := validDossier(t)
	changed.MR.Title = "Add parser and lexer"
	third, err := DossierHash(changed)
	if err != nil {
		t.Fatalf("DossierHash changed: %v", err)
	}
	if third == first {
		t.Fatalf("hash did not change after a material change")
	}
}

func TestDossierRejectsSecretKeysAndMissingFields(t *testing.T) {
	d := validDossier(t)
	d.Metadata = map[string]string{"api_token": "not-a-real-token"}
	if err := ValidateDossier(d); err == nil || !strings.Contains(err.Error(), "secret-looking") {
		t.Fatalf("expected secret key rejection, got %v", err)
	}
	missing := validDossier(t)
	missing.DiffVersion.HeadSHA = ""
	if err := ValidateDossier(missing); err == nil {
		t.Fatalf("expected missing head sha rejection")
	}
	emptyFiles := validDossier(t)
	emptyFiles.Files = nil
	if err := ValidateDossier(emptyFiles); err == nil {
		t.Fatalf("expected nil files rejection")
	}
	emptyFiles.Files = []FileChange{}
	if err := ValidateDossier(emptyFiles); err != nil {
		t.Fatalf("empty file list should be valid: %v", err)
	}
}

func TestComputeIncrementalScope(t *testing.T) {
	mr := []string{"a.go", "b.go", "dir/c.go"}
	scope, err := ComputeIncrementalScope(IncrementalInput{
		LastHead:     testLastSHA,
		Head:         testHeadSHA,
		MRFiles:      mr,
		IsAncestor:   true,
		ChangedFiles: []string{"a.go", "dir/c.go", "outside.go"},
	})
	if err != nil {
		t.Fatalf("ComputeIncrementalScope: %v", err)
	}
	if scope.Full {
		t.Fatalf("expected incremental scope")
	}
	if strings.Join(scope.Files, ",") != "a.go,dir/c.go" {
		t.Fatalf("files = %v", scope.Files)
	}

	full, err := ComputeIncrementalScope(IncrementalInput{
		LastHead:   testLastSHA,
		Head:       testHeadSHA,
		MRFiles:    mr,
		IsAncestor: false,
	})
	if err != nil {
		t.Fatalf("non-ancestor: %v", err)
	}
	if !full.Full || strings.Join(full.Files, ",") != "a.go,b.go,dir/c.go" {
		t.Fatalf("non-ancestor scope = %+v", full)
	}

	forced, err := ComputeIncrementalScope(IncrementalInput{
		LastHead:   testLastSHA,
		Head:       testHeadSHA,
		MRFiles:    mr,
		IsAncestor: true,
		ForceFull:  true,
	})
	if err != nil || !forced.Full {
		t.Fatalf("forced full scope = %+v, err=%v", forced, err)
	}

	if _, err := ComputeIncrementalScope(IncrementalInput{
		LastHead:   testLastSHA,
		Head:       testHeadSHA,
		MRFiles:    mr,
		IsAncestor: true,
	}); err == nil {
		t.Fatalf("expected an error without a changed-file source")
	}

	called := false
	loaded, err := ComputeIncrementalScope(IncrementalInput{
		LastHead:   testLastSHA,
		Head:       testHeadSHA,
		MRFiles:    mr,
		IsAncestor: true,
		LoadChanged: func(from, to string) ([]string, error) {
			called = true
			if from != testLastSHA || to != testHeadSHA {
				t.Fatalf("unexpected range %s..%s", from, to)
			}
			return []string{"b.go"}, nil
		},
	})
	if err != nil || !called || strings.Join(loaded.Files, ",") != "b.go" {
		t.Fatalf("LoadChanged scope = %+v, called=%v, err=%v", loaded, called, err)
	}
}

func TestDossierLegacyDiffSummaryStillFailsClosed(t *testing.T) {
	d := validDossier(t)
	d.DiffSummary.Truncated = true
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	summary := fields["diff_summary"].(map[string]any)
	delete(summary, "total")
	delete(summary, "omitted")
	legacyRaw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var legacy Dossier
	if err := json.Unmarshal(legacyRaw, &legacy); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDossier(&legacy); err != nil {
		t.Fatalf("legacy dossier is no longer valid: %v", err)
	}
	if !legacy.DiffSummary.Truncated || legacy.DiffSummary.Total != 0 || len(legacy.DiffSummary.Omitted) != 0 {
		t.Fatalf("legacy diff summary = %+v", legacy.DiffSummary)
	}
}
