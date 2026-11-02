package review

import (
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
)

func verifyTestFinding() FindingOut {
	return FindingOut{
		Path:     "a.go",
		Line:     3,
		Side:     "new",
		Severity: SeverityP1,
		Category: "bug",
		Title:    "nil deref",
		Body:     "nil dereference",
		Evidence: "a.go:3",
	}
}

func TestVerifyTaskIncludesFailedJobTraces(t *testing.T) {
	d := validDossier(t)
	d.Pipeline = &PipelineSummary{
		Status: "failed",
		Ref:    "main",
		FailedJobs: []JobSummary{{
			ID: 9, Name: "unit", Status: "failed", FailureReason: "script_failure",
			Trace: "FAIL TestParser: panic at parser.go:42",
		}},
	}

	task := verifyTask(d, verifyTestFinding())
	for _, want := range []string{
		"FAIL TestParser: panic at parser.go:42",
		"可能已截断",
		"UNCERTAIN",
	} {
		if !strings.Contains(task, want) {
			t.Fatalf("verify task is missing %q:\n%s", want, task)
		}
	}
}

func TestVerifyTaskUsesLongerFenceWhenTraceContainsFence(t *testing.T) {
	d := validDossier(t)
	d.Pipeline = &PipelineSummary{
		Status: "failed",
		FailedJobs: []JobSummary{{
			ID: 9, Name: "unit", Status: "failed",
			Trace: "before\n```\nafter",
		}},
	}

	task := verifyTask(d, verifyTestFinding())
	if !strings.Contains(task, "````text") {
		t.Fatalf("verify task did not widen the fence:\n%s", task)
	}
	if strings.Contains(task, "\n```text") {
		t.Fatalf("verify task still uses the colliding fence:\n%s", task)
	}
}

func TestVerifyTaskTruncatesLongJobTrace(t *testing.T) {
	d := validDossier(t)
	d.Pipeline = &PipelineSummary{
		Status: "failed",
		FailedJobs: []JobSummary{{
			ID: 9, Name: "unit", Status: "failed",
			Trace: strings.Repeat("x", maxJobTracePromptBytes) + "TAIL-MARKER",
		}},
	}

	task := verifyTask(d, verifyTestFinding())
	if strings.Contains(task, "TAIL-MARKER") {
		t.Fatal("verify task kept trace bytes past the prompt limit")
	}
	if !strings.Contains(task, "日志片段已截断") {
		t.Fatalf("verify task has no trace truncation marker:\n%s", task)
	}
}

func TestBuildDossierKeepsJobTrace(t *testing.T) {
	d, err := BuildDossier(DossierInput{
		Project: ProjectRef{ID: 42, Path: "group/project"},
		MR: MRMeta{
			IID:          7,
			Title:        "Add parser",
			SourceBranch: "feature/parser",
			TargetBranch: "main",
		},
		DiffVersion:         gitlab.Version{BaseSHA: testBaseSHA, StartSHA: testStartSHA, HeadSHA: testHeadSHA},
		WorkspaceProvenance: &WorkspaceProvenance{Version: WorkspaceProvenanceVersion, TargetSHA: testStartSHA, IndexSHA: testStartSHA, MCPEligible: true},
		GeneratedAt:         time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC),
		Files:               []FileChange{},
		Pipeline: &PipelineSummary{
			Status: "failed",
			FailedJobs: []JobSummary{{
				ID: 9, Name: "unit", Status: "failed", Trace: "panic at main.go:3",
			}},
		},
	})
	if err != nil {
		t.Fatalf("BuildDossier: %v", err)
	}
	if len(d.Pipeline.FailedJobs) != 1 {
		t.Fatalf("failed jobs = %+v", d.Pipeline.FailedJobs)
	}
	job := d.Pipeline.FailedJobs[0]
	if job.ID != 9 || job.Trace != "panic at main.go:3" {
		t.Fatalf("job = %+v, want ID and trace preserved", job)
	}
}
