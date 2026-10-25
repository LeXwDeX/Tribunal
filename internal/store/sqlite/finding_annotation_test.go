package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func annotationFinding(id string) store.Finding {
	return store.Finding{
		ID:             id,
		OriginNode:     "a",
		OriginIndex:    0,
		OriginProvider: "claude",
		Severity:       "P1",
		Category:       "logic",
		Title:          id,
		Body:           "body-" + id,
	}
}

func findingByID(t *testing.T, findings []store.Finding, id string) store.Finding {
	t.Helper()
	for _, finding := range findings {
		if finding.ID == id {
			return finding
		}
	}
	t.Fatalf("finding %s not in %+v", id, findings)
	return store.Finding{}
}

func assertAnnotatedFinding(t *testing.T, finding store.Finding, annotation, annotatedBy string, annotatedAt time.Time) {
	t.Helper()
	if finding.Annotation != annotation || finding.AnnotatedBy != annotatedBy {
		t.Fatalf("annotation/by = %q/%q, want %q/%q", finding.Annotation, finding.AnnotatedBy, annotation, annotatedBy)
	}
	if finding.AnnotatedAt == nil || !finding.AnnotatedAt.Equal(annotatedAt) {
		t.Fatalf("annotated_at = %v, want %v", finding.AnnotatedAt, annotatedAt)
	}
}

func TestFindingAnnotationMigrationPreservesLegacyFindings(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "annotations.db")
	clock := newFakeClock()

	st, err := Open(ctx, path, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustProject(t, st, 42)
	mustCreateWorkflow(t, st, "wf-migrate", reviewMeta(42, 1, "head"), clock.Now())
	if err := st.ReplaceFindings(ctx, "wf-migrate", []store.Finding{annotationFinding("f1")}); err != nil {
		t.Fatalf("ReplaceFindings: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Recreate a pre-feature database: rows remain, but the three annotation
	// columns and the migration record are removed.
	legacy, err := sql.Open("sqlite", withPragmas(path))
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	for _, column := range []string{"annotation", "annotated_by", "annotated_at"} {
		if _, err := legacy.Exec(`ALTER TABLE findings DROP COLUMN ` + column); err != nil {
			t.Fatalf("drop %s: %v", column, err)
		}
	}
	if _, err := legacy.Exec(`DELETE FROM schema_migrations WHERE version = 6`); err != nil {
		t.Fatalf("remove migration record: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	st, err = Open(ctx, path, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("reopen migrated db: %v", err)
	}
	defer st.Close()
	if got := rowCount(t, st, `SELECT COUNT(*) FROM schema_migrations`); got != 11 {
		t.Fatalf("schema_migrations = %d, want 10", got)
	}
	findings, err := st.ListFindings(ctx, store.FindingFilter{WorkflowID: "wf-migrate"})
	if err != nil {
		t.Fatalf("ListFindings after migration: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings after migration = %+v", findings)
	}
	legacyFinding := findings[0]
	if legacyFinding.Title != "f1" || legacyFinding.Annotation != "" || legacyFinding.AnnotatedBy != "" || legacyFinding.AnnotatedAt != nil {
		t.Fatalf("migrated finding = %+v", legacyFinding)
	}

	clock.Advance(time.Minute)
	at := clock.Now()
	updated, err := st.SetFindingAnnotation(ctx, "wf-migrate", "f1", store.FindingAnnotationValid, "alice")
	if err != nil {
		t.Fatalf("SetFindingAnnotation on migrated db: %v", err)
	}
	assertAnnotatedFinding(t, updated, store.FindingAnnotationValid, "alice", at)

	if err := st.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}
	st, err = Open(ctx, path, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("reopen annotated db: %v", err)
	}
	defer st.Close()
	reloaded, err := st.ListFindings(ctx, store.FindingFilter{WorkflowID: "wf-migrate"})
	if err != nil || len(reloaded) != 1 {
		t.Fatalf("reloaded findings = %+v, %v", reloaded, err)
	}
	assertAnnotatedFinding(t, reloaded[0], store.FindingAnnotationValid, "alice", at)
}

func TestFindingAnnotationSetClearAndScope(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	mustCreateWorkflow(t, st, "wf-a", reviewMeta(42, 1, "head-a"), clock.Now())
	mustCreateWorkflow(t, st, "wf-b", reviewMeta(42, 2, "head-b"), clock.Now())
	if err := st.ReplaceFindings(ctx, "wf-a", []store.Finding{annotationFinding("f-a")}); err != nil {
		t.Fatalf("ReplaceFindings wf-a: %v", err)
	}
	if err := st.ReplaceFindings(ctx, "wf-b", []store.Finding{annotationFinding("f-b")}); err != nil {
		t.Fatalf("ReplaceFindings wf-b: %v", err)
	}

	firstAt := clock.Now()
	finding, err := st.SetFindingAnnotation(ctx, "wf-a", "f-a", store.FindingAnnotationValid, "alice")
	if err != nil {
		t.Fatalf("SetFindingAnnotation valid: %v", err)
	}
	assertAnnotatedFinding(t, finding, store.FindingAnnotationValid, "alice", firstAt)
	if finding.Title != "f-a" || finding.Body != "body-f-a" || finding.Severity != "P1" {
		t.Fatalf("SetFindingAnnotation returned a partial finding: %+v", finding)
	}

	listed, err := st.ListFindings(ctx, store.FindingFilter{WorkflowID: "wf-a"})
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListFindings after valid = %+v, %v", listed, err)
	}
	assertAnnotatedFinding(t, listed[0], store.FindingAnnotationValid, "alice", firstAt)

	clock.Advance(time.Minute)
	secondAt := clock.Now()
	finding, err = st.SetFindingAnnotation(ctx, "wf-a", "f-a", store.FindingAnnotationInvalid, "bob")
	if err != nil {
		t.Fatalf("SetFindingAnnotation invalid: %v", err)
	}
	assertAnnotatedFinding(t, finding, store.FindingAnnotationInvalid, "bob", secondAt)

	if _, err := st.SetFindingAnnotation(ctx, "wf-a", "f-a", "maybe", "eve"); err == nil {
		t.Fatal("unsupported annotation accepted")
	}
	listed, err = st.ListFindings(ctx, store.FindingFilter{WorkflowID: "wf-a"})
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListFindings after rejected annotation = %+v, %v", listed, err)
	}
	assertAnnotatedFinding(t, listed[0], store.FindingAnnotationInvalid, "bob", secondAt)

	for _, testCase := range []struct {
		name       string
		workflowID string
		findingID  string
	}{
		{name: "wrong workflow", workflowID: "wf-b", findingID: "f-a"},
		{name: "missing workflow", workflowID: "wf-missing", findingID: "f-a"},
		{name: "missing finding", workflowID: "wf-a", findingID: "f-missing"},
	} {
		if _, err := st.SetFindingAnnotation(ctx, testCase.workflowID, testCase.findingID, store.FindingAnnotationUncertain, "eve"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s error = %v, want ErrNotFound", testCase.name, err)
		}
	}
	listed, err = st.ListFindings(ctx, store.FindingFilter{WorkflowID: "wf-b"})
	if err != nil || len(listed) != 1 || listed[0].Annotation != "" {
		t.Fatalf("wrong-workflow update changed wf-b = %+v, %v", listed, err)
	}

	clock.Advance(time.Minute)
	finding, err = st.SetFindingAnnotation(ctx, "wf-a", "f-a", store.FindingAnnotationNone, "bob")
	if err != nil {
		t.Fatalf("clear annotation: %v", err)
	}
	if finding.Annotation != "" || finding.AnnotatedBy != "" || finding.AnnotatedAt != nil {
		t.Fatalf("cleared finding = %+v", finding)
	}
	var (
		annotatedBy sql.NullString
		annotatedAt sql.NullString
	)
	if err := st.db.QueryRowContext(ctx,
		`SELECT annotated_by, annotated_at FROM findings WHERE id = 'f-a'`).Scan(&annotatedBy, &annotatedAt); err != nil {
		t.Fatalf("read cleared columns: %v", err)
	}
	if annotatedBy.Valid || annotatedAt.Valid {
		t.Fatalf("cleared columns are not null: by=%v at=%v", annotatedBy, annotatedAt)
	}
}

func TestReplaceFindingsPreservesAnnotationOnSameID(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	mustCreateWorkflow(t, st, "wf-a", reviewMeta(42, 1, "head-a"), clock.Now())
	mustCreateWorkflow(t, st, "wf-b", reviewMeta(42, 2, "head-b"), clock.Now())
	if err := st.ReplaceFindings(ctx, "wf-a", []store.Finding{annotationFinding("f-a")}); err != nil {
		t.Fatalf("ReplaceFindings f-a: %v", err)
	}
	if err := st.ReplaceFindings(ctx, "wf-b", []store.Finding{annotationFinding("f-b")}); err != nil {
		t.Fatalf("ReplaceFindings f-b: %v", err)
	}
	at := clock.Now()
	if _, err := st.SetFindingAnnotation(ctx, "wf-a", "f-a", store.FindingAnnotationValid, "alice"); err != nil {
		t.Fatalf("SetFindingAnnotation f-a: %v", err)
	}
	if _, err := st.SetFindingAnnotation(ctx, "wf-b", "f-b", store.FindingAnnotationInvalid, "bob"); err != nil {
		t.Fatalf("SetFindingAnnotation f-b: %v", err)
	}

	producerAt := clock.Now().Add(time.Hour)
	producerFinding := annotationFinding("f-a")
	producerFinding.Title = "producer rewrite"
	producerFinding.Annotation = store.FindingAnnotationInvalid
	producerFinding.AnnotatedBy = "producer"
	producerFinding.AnnotatedAt = &producerAt
	newFinding := annotationFinding("f-new")
	newFinding.Annotation = store.FindingAnnotationValid
	newFinding.AnnotatedBy = "producer"

	if err := st.ReplaceFindings(ctx, "wf-a", []store.Finding{producerFinding, newFinding}); err != nil {
		t.Fatalf("ReplaceFindings with producer fields: %v", err)
	}
	findings, err := st.ListFindings(ctx, store.FindingFilter{WorkflowID: "wf-a"})
	if err != nil {
		t.Fatalf("ListFindings wf-a: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("wf-a findings = %+v", findings)
	}
	kept := findingByID(t, findings, "f-a")
	if kept.Title != "producer rewrite" {
		t.Fatalf("producer fields were not replaced: %+v", kept)
	}
	assertAnnotatedFinding(t, kept, store.FindingAnnotationValid, "alice", at)
	added := findingByID(t, findings, "f-new")
	if added.Annotation != "" || added.AnnotatedBy != "" || added.AnnotatedAt != nil {
		t.Fatalf("producer set human fields on a new finding: %+v", added)
	}
	other, err := st.ListFindings(ctx, store.FindingFilter{WorkflowID: "wf-b"})
	if err != nil || len(other) != 1 {
		t.Fatalf("ListFindings wf-b: %+v, %v", other, err)
	}
	assertAnnotatedFinding(t, other[0], store.FindingAnnotationInvalid, "bob", at)

	if err := st.ReplaceFindings(ctx, "wf-a", []store.Finding{newFinding}); err != nil {
		t.Fatalf("ReplaceFindings removing f-a: %v", err)
	}
	findings, err = st.ListFindings(ctx, store.FindingFilter{WorkflowID: "wf-a"})
	if err != nil || len(findings) != 1 || findings[0].ID != "f-new" {
		t.Fatalf("findings after removal = %+v, %v", findings, err)
	}

	clock.Advance(time.Minute)
	keptAt := clock.Now()
	if _, err := st.SetFindingAnnotation(ctx, "wf-a", "f-new", store.FindingAnnotationUncertain, "carol"); err != nil {
		t.Fatalf("SetFindingAnnotation f-new: %v", err)
	}
	if err := st.ReplaceFindings(ctx, "wf-a", []store.Finding{newFinding}); err != nil {
		t.Fatalf("ReplaceFindings preserving f-new: %v", err)
	}
	findings, err = st.ListFindings(ctx, store.FindingFilter{WorkflowID: "wf-a"})
	if err != nil || len(findings) != 1 {
		t.Fatalf("findings after same-ID replace = %+v, %v", findings, err)
	}
	assertAnnotatedFinding(t, findings[0], store.FindingAnnotationUncertain, "carol", keptAt)
}

func TestReplaceFindingsRejectsDuplicateAndCrossWorkflowIDsAtomically(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	mustCreateWorkflow(t, st, "wf-a", reviewMeta(42, 1, "head-a"), clock.Now())
	mustCreateWorkflow(t, st, "wf-b", reviewMeta(42, 2, "head-b"), clock.Now())
	if err := st.ReplaceFindings(ctx, "wf-a", []store.Finding{annotationFinding("f-a")}); err != nil {
		t.Fatalf("ReplaceFindings f-a: %v", err)
	}
	if err := st.ReplaceFindings(ctx, "wf-b", []store.Finding{annotationFinding("f-b")}); err != nil {
		t.Fatalf("ReplaceFindings f-b: %v", err)
	}
	at := clock.Now()
	if _, err := st.SetFindingAnnotation(ctx, "wf-a", "f-a", store.FindingAnnotationValid, "alice"); err != nil {
		t.Fatalf("SetFindingAnnotation f-a: %v", err)
	}
	if _, err := st.SetFindingAnnotation(ctx, "wf-b", "f-b", store.FindingAnnotationInvalid, "bob"); err != nil {
		t.Fatalf("SetFindingAnnotation f-b: %v", err)
	}

	duplicate := annotationFinding("f-a")
	duplicate.Title = "duplicate rewrite"
	if err := st.ReplaceFindings(ctx, "wf-a", []store.Finding{annotationFinding("f-a"), duplicate}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate IDs error = %v, want ErrConflict", err)
	}
	assertWorkflowFinding(t, st, "wf-a", "f-a", store.FindingAnnotationValid, "alice", at)
	assertWorkflowFinding(t, st, "wf-b", "f-b", store.FindingAnnotationInvalid, "bob", at)

	crossWorkflow := annotationFinding("f-b")
	crossWorkflow.Title = "cross-workflow overwrite"
	if err := st.ReplaceFindings(ctx, "wf-a", []store.Finding{crossWorkflow}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("cross-workflow ID error = %v, want ErrConflict", err)
	}
	assertWorkflowFinding(t, st, "wf-a", "f-a", store.FindingAnnotationValid, "alice", at)
	assertWorkflowFinding(t, st, "wf-b", "f-b", store.FindingAnnotationInvalid, "bob", at)

	mixed := []store.Finding{annotationFinding("f-new"), crossWorkflow}
	if err := st.ReplaceFindings(ctx, "wf-a", mixed); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("mixed replace error = %v, want ErrConflict", err)
	}
	findings, err := st.ListFindings(ctx, store.FindingFilter{WorkflowID: "wf-a"})
	if err != nil {
		t.Fatalf("ListFindings after rollback: %v", err)
	}
	if len(findings) != 1 || findings[0].ID != "f-a" {
		t.Fatalf("failed replace was not atomic: %+v", findings)
	}
}

func assertWorkflowFinding(t *testing.T, st *Store, workflowID, findingID, annotation, annotatedBy string, at time.Time) {
	t.Helper()
	findings, err := st.ListFindings(context.Background(), store.FindingFilter{WorkflowID: workflowID})
	if err != nil {
		t.Fatalf("ListFindings %s: %v", workflowID, err)
	}
	if len(findings) != 1 {
		t.Fatalf("%s findings = %+v, want exactly %s", workflowID, findings, findingID)
	}
	if findings[0].ID != findingID {
		t.Fatalf("%s finding ID = %s, want %s", workflowID, findings[0].ID, findingID)
	}
	assertAnnotatedFinding(t, findings[0], annotation, annotatedBy, at)
}
