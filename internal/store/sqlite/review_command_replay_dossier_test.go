package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

type legacyDossierProof struct {
	workflowID, jobID, kind, workflowTrigger                     string
	workflowProject, dossierProject, mrIID                       int64
	generation, version, schema                                  int
	dossierProjectID, dossierMRIID                               int64
	dossierTrigger, noteID, head                                 string
	badHash, badFingerprint, badGzip, orphan                     bool
	badJSON, duplicateMetadata, duplicateRoot, oversized, badJob bool
}

func insertLegacyDossierProof(t *testing.T, db *sql.DB, p legacyDossierProof) {
	t.Helper()
	const stamp = "2026-10-22T12:00:00.000000000Z"
	const configHash, promptHash = "config", "prompt"
	data, err := json.Marshal(map[string]any{
		"schema_version": p.schema,
		"project":        map[string]any{"id": p.dossierProjectID},
		"mr":             map[string]any{"iid": p.dossierMRIID},
		"diff_version":   map[string]any{"HeadSHA": p.head},
		"metadata":       map[string]string{"trigger": p.dossierTrigger, "note_id": p.noteID, "config_hash": configHash, "prompt_hash": promptHash},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.badJSON {
		data = []byte(`{"schema_version":`)
	}
	if p.duplicateMetadata {
		data = bytes.Replace(data, []byte(`"metadata":{`), []byte(`"metadata":{"note_id":"999",`), 1)
	}
	if p.duplicateRoot {
		data = bytes.Replace(data, []byte(`{`), []byte(`{"Project":{"id":43},`), 1)
	}
	if p.oversized {
		data = []byte(`{"padding":"` + strings.Repeat("x", maxWorkflowDossierBytes) + `"}`)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	proof := sha256.Sum256([]byte(hash + "\n" + configHash + "\n" + promptHash))
	fingerprint := hex.EncodeToString(proof[:])
	if p.badFingerprint {
		fingerprint = "incorrect"
	}
	if !p.orphan {
		if _, err := db.Exec(`INSERT INTO workflows (id, kind, project_id, mr_iid, head_sha, fingerprint, trigger, status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'completed', ?)`, p.workflowID, p.kind, p.workflowProject, p.mrIID,
			reviewHead("a"), fingerprint, p.workflowTrigger, stamp); err != nil {
			t.Fatalf("insert workflow: %v", err)
		}
	}
	if p.badJob {
		insertLegacyReplayJob(t, db, p.jobID, "workspace.onboard", "workspace.onboard:42", `{}`, "done")
	}
	compressed, err := compressDossier(data)
	if err != nil {
		t.Fatal(err)
	}
	if p.badGzip {
		compressed = []byte("not gzip")
	}
	if p.badHash {
		hash = "incorrect"
	}
	var jobID any
	if p.jobID != "" {
		jobID = p.jobID
	}
	if _, err := db.Exec(`INSERT INTO workflow_dossiers
		(workflow_id, project_id, job_id, generation, version, hash, data, publication_state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'published', ?, ?)`, p.workflowID, p.dossierProject, jobID,
		p.generation, p.version, hash, compressed, stamp, stamp); err != nil {
		t.Fatalf("insert dossier: %v", err)
	}
}

func TestCommandReplayDossierProofAndIsolation(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyReplayProject(t, legacy, 43)
	head := reviewHead("a")
	insertLegacyReplayControl(t, legacy, 42, 7, 2, head, true)
	insertLegacyReplayReaction(t, legacy, 42, 7, 555, 2)
	// A later, unexecuted cancel shares the source note and reaction. The
	// dossier proves the earlier run and prevents reaction-only cancel proof.
	insertLegacyReplayJob(t, legacy, "queued-cancel", "review.command", "review.command:42:555",
		legacyCommandPayload(42, 7, 555, "cancel"), "queued")
	valid := legacyDossierProof{workflowID: "valid", jobID: "pruned-run", kind: "review", workflowTrigger: "command",
		workflowProject: 42, dossierProject: 42, mrIID: 7, generation: 1, version: 1, schema: 1,
		dossierProjectID: 42, dossierMRIID: 7, dossierTrigger: "command", noteID: "555", head: head}
	insertLegacyDossierProof(t, legacy, valid)
	detached := valid
	detached.workflowID, detached.jobID, detached.mrIID, detached.dossierMRIID, detached.noteID = "detached", "", 8, 8, "556"
	insertLegacyDossierProof(t, legacy, detached)
	for i, tc := range []struct {
		name   string
		mutate func(*legacyDossierProof)
	}{
		{"wrong workflow trigger", func(p *legacyDossierProof) { p.workflowTrigger = "api" }},
		{"wrong dossier trigger", func(p *legacyDossierProof) { p.dossierTrigger = "api" }},
		{"onboarding workflow", func(p *legacyDossierProof) { p.kind = "onboarding" }},
		{"owner mismatch", func(p *legacyDossierProof) { p.dossierProject = 43 }},
		{"unknown owner", func(p *legacyDossierProof) { p.dossierProject = 0 }},
		{"snapshot project mismatch", func(p *legacyDossierProof) { p.dossierProjectID = 43 }},
		{"snapshot mr mismatch", func(p *legacyDossierProof) { p.dossierMRIID = 8 }},
		{"snapshot head mismatch", func(p *legacyDossierProof) { p.head = reviewHead("b") }},
		{"orphan dossier", func(p *legacyDossierProof) { p.orphan = true }},
		{"unknown generation", func(p *legacyDossierProof) { p.generation = 0 }},
		{"unknown version", func(p *legacyDossierProof) { p.version = 2 }},
		{"unknown schema", func(p *legacyDossierProof) { p.schema = 2 }},
		{"wrong hash", func(p *legacyDossierProof) { p.badHash = true }},
		{"wrong fingerprint", func(p *legacyDossierProof) { p.badFingerprint = true }},
		{"corrupt gzip", func(p *legacyDossierProof) { p.badGzip = true }},
		{"invalid json", func(p *legacyDossierProof) { p.badJSON = true }},
		{"duplicate metadata key", func(p *legacyDossierProof) { p.duplicateMetadata = true }},
		{"ambiguous root key", func(p *legacyDossierProof) { p.duplicateRoot = true }},
		{"oversized dossier", func(p *legacyDossierProof) { p.oversized = true }},
		{"conflicting linked job", func(p *legacyDossierProof) { p.badJob = true }},
		{"missing note", func(p *legacyDossierProof) { p.noteID = "" }},
		{"zero note", func(p *legacyDossierProof) { p.noteID = "0" }},
		{"noncanonical note", func(p *legacyDossierProof) { p.noteID = "0" + p.noteID }},
		{"negative note", func(p *legacyDossierProof) { p.noteID = "-555" }},
		{"overflow note", func(p *legacyDossierProof) { p.noteID = "9223372036854775808" }},
	} {
		p := valid
		p.workflowID, p.jobID, p.noteID = fmt.Sprintf("invalid-%d", i), fmt.Sprintf("invalid-job-%d", i), fmt.Sprint(600+i)
		tc.mutate(&p)
		insertLegacyDossierProof(t, legacy, p)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("upgrade with damaged dossiers: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_command_replays`); got != 2 {
		t.Fatalf("identities = %d, want only two proven runs", got)
	}
	for _, tc := range []struct {
		mrIID, noteID int64
		jobID         string
	}{{7, 555, "pruned-run"}, {8, 556, ""}} {
		result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, tc.mrIID, tc.noteID, "run", head))
		if err != nil || !result.Duplicate || result.Generation != 0 || result.JobID != tc.jobID {
			t.Errorf("proven run %d/%d = %+v, %v", tc.mrIID, tc.noteID, result, err)
		}
	}
	uncertain, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 555, "cancel", ""))
	if !errors.Is(err, store.ErrLegacyCommandUncertain) || uncertain.Duplicate || uncertain.Generation != 0 {
		t.Fatalf("unproven cancel = %+v, %v", uncertain, err)
	}
	for _, tc := range []struct {
		projectID, mrIID, noteID int64
		kind, head               string
	}{{42, 8, 555, "run", head}, {43, 7, 555, "run", head}} {
		result, err := s.EnqueueReviewLatest(ctx, commandReplay(tc.projectID, tc.mrIID, tc.noteID, tc.kind, tc.head))
		if err != nil || result.Duplicate || result.Generation == 0 {
			t.Errorf("unproven %d/%d/%d %s = %+v, %v", tc.projectID, tc.mrIID, tc.noteID, tc.kind, result, err)
		}
	}
}

func TestCommandReplayDossierLinkedJobGenerationProvenance(t *testing.T) {
	head := reviewHead("a")
	otherHead := reviewHead("b")
	run := func(project, mr, note, generation int64, trigger, jobHead string) string {
		return fmt.Sprintf(`{"project_id":%d,"mr_iid":%d,"note_id":%d,"generation":%d,"head_sha":%q,"trigger":%q}`,
			project, mr, note, generation, jobHead, trigger)
	}
	tests := []struct {
		name, kind, key, payload string
		controlGeneration        int64
		noControl, noAttempts    bool
		controlRunning           bool
		want                     bool
	}{
		{name: "same generation and identity", payload: run(42, 7, 555, 1, "command", head), want: true},
		{name: "legacy identity without generation", payload: `{"project_id":42,"mr_iid":7,"note_id":555,"trigger":"command"}`, want: true},
		{name: "later command", payload: run(42, 7, 556, 2, "command", head), want: true},
		{name: "later command same note", payload: run(42, 7, 555, 2, "command", head), want: true},
		{name: "later manual", payload: fmt.Sprintf(`{"project_id":42,"mr_iid":7,"generation":2,"head_sha":%q,"trigger":"manual"}`, otherHead), want: true},
		{name: "replacement at current control", payload: run(42, 7, 556, 2, "command", head), controlGeneration: 2, controlRunning: true, want: true},
		{name: "replacement conflicts with current head", payload: run(42, 7, 556, 2, "command", otherHead), controlGeneration: 2, controlRunning: true},
		{name: "replacement conflicts with current cancel", payload: run(42, 7, 556, 2, "command", head), controlGeneration: 2},
		{name: "same generation different note", payload: run(42, 7, 556, 1, "command", head)},
		{name: "same generation different trigger", payload: run(42, 7, 555, 1, "manual", head)},
		{name: "same generation different head", payload: run(42, 7, 555, 1, "command", otherHead)},
		{name: "earlier generation", payload: run(42, 7, 556, 0, "command", head)},
		{name: "earlier generation same identity", payload: run(42, 7, 555, 0, "command", head)},
		{name: "missing generation", payload: `{"project_id":42,"mr_iid":7,"note_id":556,"trigger":"command"}`},
		{name: "future generation beyond control", payload: run(42, 7, 556, 4, "command", head)},
		{name: "missing control", payload: run(42, 7, 556, 2, "command", head), noControl: true},
		{name: "never attempted", payload: run(42, 7, 556, 2, "command", head), noAttempts: true},
		{name: "different project", payload: run(43, 7, 556, 2, "command", head)},
		{name: "different MR", payload: run(42, 8, 556, 2, "command", head)},
		{name: "different kind", kind: "review.cancel", payload: run(42, 7, 556, 2, "command", head)},
		{name: "different key", key: "review:42:8", payload: run(42, 7, 556, 2, "command", head)},
		{name: "missing head", payload: `{"project_id":42,"mr_iid":7,"note_id":556,"generation":2,"trigger":"command"}`},
		{name: "invalid head", payload: run(42, 7, 556, 2, "command", "bad")},
		{name: "missing trigger", payload: fmt.Sprintf(`{"project_id":42,"mr_iid":7,"note_id":556,"generation":2,"head_sha":%q}`, head)},
		{name: "empty trigger", payload: run(42, 7, 556, 2, "", head)},
		{name: "command without note", payload: fmt.Sprintf(`{"project_id":42,"mr_iid":7,"generation":2,"head_sha":%q,"trigger":"command"}`, head)},
		{name: "negative note on manual", payload: run(42, 7, -1, 2, "manual", head)},
		{name: "aliased note on manual", payload: fmt.Sprintf(`{"project_id":42,"mr_iid":7,"NOTE_ID":556,"generation":2,"head_sha":%q,"trigger":"manual"}`, head)},
		{name: "aliased zero note on manual", payload: fmt.Sprintf(`{"project_id":42,"mr_iid":7,"NOTE_ID":0,"generation":2,"head_sha":%q,"trigger":"manual"}`, head)},
		{name: "invalid generation type", payload: fmt.Sprintf(`{"project_id":42,"mr_iid":7,"note_id":556,"generation":"2","head_sha":%q,"trigger":"command"}`, head)},
		{name: "ambiguous generation", payload: fmt.Sprintf(`{"project_id":42,"mr_iid":7,"note_id":556,"generation":1,"GENERATION":2,"head_sha":%q,"trigger":"command"}`, head)},
		{name: "aliased generation", payload: fmt.Sprintf(`{"project_id":42,"mr_iid":7,"note_id":556,"GENERATION":2,"head_sha":%q,"trigger":"command"}`, head)},
		{name: "aliased generation same identity", payload: fmt.Sprintf(`{"project_id":42,"mr_iid":7,"note_id":555,"GENERATION":1,"head_sha":%q,"trigger":"command"}`, head)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			_, db := openLegacyCommandReplayDB(t)
			insertLegacyReplayProject(t, db, 42)
			if !tt.noControl {
				generation := tt.controlGeneration
				if generation == 0 {
					generation = 3
				}
				insertLegacyReplayControl(t, db, 42, 7, generation, head, !tt.controlRunning)
			}
			insertLegacyDossierProof(t, db, legacyDossierProof{
				workflowID: "workflow", jobID: "linked", kind: "review", workflowTrigger: "command",
				workflowProject: 42, dossierProject: 42, mrIID: 7, generation: 1, version: 1, schema: 1,
				dossierProjectID: 42, dossierMRIID: 7, dossierTrigger: "command", noteID: "555", head: head,
			})
			kind, key := tt.kind, tt.key
			if kind == "" {
				kind = "review.run"
			}
			if key == "" {
				key = "review:42:7"
			}
			insertLegacyReplayJob(t, db, "linked", kind, key, tt.payload, "queued")
			if tt.noAttempts {
				if _, err := db.Exec(`UPDATE jobs SET attempts = 0 WHERE id = 'linked'`); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			states := make(map[commandReplayNoteKey]*commandReplayNoteState)
			if err := loadCommandReplayDossiersTx(ctx, tx, states); err != nil {
				t.Fatal(err)
			}
			state := states[commandReplayNoteKey{42, 7, 555}]
			got := false
			if state != nil {
				_, got = state.reviewIDs["run"]
			}
			if got != tt.want {
				t.Errorf("original dossier proof = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCommandReplayDossierBackfillRollsBackAndRetries(t *testing.T) {
	ctx := context.Background()
	_, legacy := openLegacyCommandReplayDB(t)
	insertLegacyReplayProject(t, legacy, 42)
	insertLegacyDossierProof(t, legacy, legacyDossierProof{
		workflowID: "valid", jobID: "pruned-run", kind: "review", workflowTrigger: "command",
		workflowProject: 42, dossierProject: 42, mrIID: 7, generation: 1, version: 1, schema: 1,
		dossierProjectID: 42, dossierMRIID: 7, dossierTrigger: "command", noteID: "555", head: reviewHead("a"),
	})
	applyCommandReplaySchema(t, legacy)
	if _, err := legacy.Exec(`CREATE TRIGGER block_dossier_replay BEFORE INSERT ON review_command_replays
		BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := backfillReviewCommandReplays(ctx, legacy, newFakeClock().Now()); err == nil {
		t.Fatal("backfill ignored insertion failure")
	}
	for _, table := range []string{"review_command_replays", "review_command_replay_backfill"} {
		var count int
		if err := legacy.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s after failure = %d, %v", table, count, err)
		}
	}
	if _, err := legacy.Exec(`DROP TRIGGER block_dossier_replay`); err != nil {
		t.Fatal(err)
	}
	if err := backfillReviewCommandReplays(ctx, legacy, newFakeClock().Now()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := legacy.QueryRow(`SELECT COUNT(*) FROM review_command_replays WHERE project_id=42 AND mr_iid=7 AND note_id=555 AND command='run'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("identity after retry = %d, %v", count, err)
	}
}

func TestCommandReplayDossierBackfillRespectsMarkerAndProjectDeletion(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	insertLegacyReplayProject(t, legacy, 42)
	proof := legacyDossierProof{
		workflowID: "valid", jobID: "pruned-run", kind: "review", workflowTrigger: "command",
		workflowProject: 42, dossierProject: 42, mrIID: 7, generation: 1, version: 1, schema: 1,
		dossierProjectID: 42, dossierMRIID: 7, dossierTrigger: "command", noteID: "555", head: reviewHead("a"),
	}
	insertLegacyDossierProof(t, legacy, proof)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatal(err)
	}
	if got := commandReplayIdentityRows(t, s, 42, 7, 555, "run"); got != 1 {
		t.Fatalf("initial identity = %d", got)
	}
	if err := s.DeleteProject(ctx, 42); err != nil {
		t.Fatal(err)
	}
	insertLegacyReplayProject(t, s.db, 42)
	proof.workflowID, proof.jobID = "late", "late-run"
	insertLegacyDossierProof(t, s.db, proof)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := commandReplayIdentityRows(t, s, 42, 7, 555, "run"); got != 0 {
		t.Fatalf("deleted identity resurrected after reopen: %d", got)
	}
	result, err := s.EnqueueReviewLatest(ctx, commandReplay(42, 7, 555, "run", reviewHead("a")))
	if err != nil || result.Duplicate || result.Generation != 1 {
		t.Fatalf("new project command = %+v, %v", result, err)
	}
}

func TestCommandReplayRejectsRoundedLegacyNoteIdentity(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	insertLegacyReplayProject(t, legacy, 42)
	const sourceNote int64 = 1<<53 + 1
	head := reviewHead("a")
	// The pre-0020 reviewJobPayload decoded JSON into map[string]any before
	// adding project and MR, which converted the original note ID to float64.
	var fields map[string]any
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"trigger":"command","note_id":%d}`, sourceNote)), &fields); err != nil {
		t.Fatal(err)
	}
	fields["project_id"], fields["mr_iid"], fields["generation"], fields["head_sha"] = 42, 7, 1, head
	payload, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var decoded commandReplayJobPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.NoteID != sourceNote-1 {
		t.Fatalf("expected legacy map conversion to round %d; got %d", sourceNote, decoded.NoteID)
	}
	insertLegacyReplayJob(t, legacy, "rounded-run", "review.run", "review:42:7", string(payload), "done")
	fields["mr_iid"] = 11
	delete(fields, "head_sha")
	cancelPayload, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	insertLegacyReplayJob(t, legacy, "rounded-cancel", "review.cancel", "review.cancel:42:11", string(cancelPayload), "done")
	insertLegacyDossierProof(t, legacy, legacyDossierProof{
		workflowID: "rounded-dossier", jobID: "pruned-run", kind: "review", workflowTrigger: "command",
		workflowProject: 42, dossierProject: 42, mrIID: 8, generation: 1, version: 1, schema: 1,
		dossierProjectID: 42, dossierMRIID: 8, dossierTrigger: "command", noteID: strconv.FormatInt(decoded.NoteID, 10), head: head,
	})
	// The largest safe note still works. Even an exact larger command and
	// current reaction are ambiguous because the reaction may be rounded.
	insertLegacyReplayJob(t, legacy, "safe-run", "review.run", "review:42:10",
		legacyReviewPayload(42, 10, 1<<53-1, 1, head), "done")
	insertLegacyDossierProof(t, legacy, legacyDossierProof{
		workflowID: "safe-dossier", jobID: "pruned-safe-run", kind: "review", workflowTrigger: "command",
		workflowProject: 42, dossierProject: 42, mrIID: 12, generation: 1, version: 1, schema: 1,
		dossierProjectID: 42, dossierMRIID: 12, dossierTrigger: "command", noteID: strconv.FormatInt(1<<53-1, 10), head: head,
	})
	insertLegacyReplayControl(t, legacy, 42, 9, 1, head, false)
	insertLegacyReplayReaction(t, legacy, 42, 9, sourceNote, 1)
	insertLegacyReplayJob(t, legacy, "exact-command", "review.command", fmt.Sprintf("review.command:42:%d", sourceNote),
		legacyCommandPayload(42, 9, sourceNote, "run"), "queued")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, mrIID := range []int64{7, 8} {
		if got := commandReplayIdentityRows(t, s, 42, mrIID, decoded.NoteID, "run"); got != 0 {
			t.Errorf("MR %d: rounded note %d falsely backfilled", mrIID, decoded.NoteID)
		}
	}
	if got := commandReplayIdentityRows(t, s, 42, 11, decoded.NoteID, "cancel"); got != 0 {
		t.Errorf("rounded cancel note %d falsely backfilled", decoded.NoteID)
	}
	if got := commandReplayIdentityRows(t, s, 42, 10, 1<<53-1, "run"); got != 1 {
		t.Error("largest exactly representable downstream note was not backfilled")
	}
	if got := commandReplayIdentityRows(t, s, 42, 12, 1<<53-1, "run"); got != 1 {
		t.Error("largest exactly representable dossier note was not backfilled")
	}
	if got := commandReplayIdentityRows(t, s, 42, 9, sourceNote, "run"); got != 0 {
		t.Error("large command note was backfilled from an ambiguous reaction")
	}
}

func TestCommandReplayRejectsAmbiguousLegacyJobPayload(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	insertLegacyReplayProject(t, legacy, 42)
	head := reviewHead("a")
	insertLegacyReplayControl(t, legacy, 42, 7, 1, head, false)
	insertLegacyReplayReaction(t, legacy, 42, 7, 555, 1)
	insertLegacyReplayJob(t, legacy, "ambiguous-command", "review.command", "review.command:42:555",
		`{"project_id":42,"mr_iid":7,"note_id":554,"note_id":555,"command":"run"}`, "queued")
	insertLegacyReplayJob(t, legacy, "ambiguous-run", "review.run", "review:42:7",
		`{"project_id":42,"mr_iid":7,"note_id":554,"NOTE_ID":556,"trigger":"command"}`, "done")
	insertLegacyDossierProof(t, legacy, legacyDossierProof{
		workflowID: "ambiguous-link", jobID: "ambiguous-linked-run", kind: "review", workflowTrigger: "command",
		workflowProject: 42, dossierProject: 42, mrIID: 7, generation: 1, version: 1, schema: 1,
		dossierProjectID: 42, dossierMRIID: 7, dossierTrigger: "command", noteID: "557", head: head,
	})
	insertLegacyReplayJob(t, legacy, "ambiguous-linked-run", "review.run", "review:42:7",
		`{"project_id":42,"mr_iid":7,"note_id":554,"note_id":557,"trigger":"command"}`, "done")
	insertLegacyReplayJob(t, legacy, "valid-run", "review.run", "review:42:7",
		legacyReviewPayload(42, 7, 558, 1, head), "done")
	insertLegacyReplayJob(t, legacy, "aliased-run", "review.run", "review:42:7",
		`{"project_id":42,"mr_iid":7,"NOTE_ID":559,"trigger":"command"}`, "done")
	insertLegacyReplayReaction(t, legacy, 42, 7, 560, 1)
	insertLegacyReplayJob(t, legacy, "aliased-command", "review.command", "review.command:42:560",
		`{"project_id":42,"mr_iid":7,"NOTE_ID":560,"command":"run"}`, "queued")
	insertLegacyDossierProof(t, legacy, legacyDossierProof{
		workflowID: "aliased-link", jobID: "aliased-linked-run", kind: "review", workflowTrigger: "command",
		workflowProject: 42, dossierProject: 42, mrIID: 7, generation: 1, version: 1, schema: 1,
		dossierProjectID: 42, dossierMRIID: 7, dossierTrigger: "command", noteID: "561", head: head,
	})
	insertLegacyReplayJob(t, legacy, "aliased-linked-run", "review.run", "review:42:7",
		`{"project_id":42,"mr_iid":7,"NOTE_ID":561,"trigger":"command"}`, "done")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, noteID := range []int64{555, 556, 557, 559, 560, 561} {
		if got := commandReplayIdentityRows(t, s, 42, 7, noteID, "run"); got != 0 {
			t.Errorf("ambiguous job for note %d was backfilled", noteID)
		}
	}
	if got := commandReplayIdentityRows(t, s, 42, 7, 558, "run"); got != 1 {
		t.Error("unambiguous job was not backfilled")
	}
}

func TestCommandReplayRejectsReactionForRoundedHighNote(t *testing.T) {
	ctx := context.Background()
	path, legacy := openLegacyCommandReplayDB(t)
	insertLegacyReplayProject(t, legacy, 42)
	head := reviewHead("a")
	const roundedNote int64 = 1 << 53
	insertLegacyReplayControl(t, legacy, 42, 7, 1, head, false)
	// A run for note 2^53+1 can settle a reaction on 2^53 after its
	// downstream payload was rounded. That reaction cannot prove a new,
	// never-executed command for the latter note.
	insertLegacyReplayReaction(t, legacy, 42, 7, roundedNote, 1)
	insertLegacyReplayJob(t, legacy, "not-executed", "review.command", fmt.Sprintf("review.command:42:%d", roundedNote),
		legacyCommandPayload(42, 7, roundedNote, "run"), "queued")
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := commandReplayIdentityRows(t, s, 42, 7, roundedNote, "run"); got != 0 {
		t.Fatalf("reaction for rounded note %d falsely backfilled", roundedNote)
	}
}
