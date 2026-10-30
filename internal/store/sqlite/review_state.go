package sqlite

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

const (
	reviewRunKind            = "review.run"
	reviewCancelKind         = "review.cancel"
	reviewCandidateKind      = "review.candidate"
	defaultReviewMaxAttempts = 3
	maxWorkflowDossierBytes  = 1 << 20 // uncompressed JSON ceiling
	maxPublicationErrorRunes = 500
)

var reviewHeadRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// GetReviewControl returns the durable per-MR review intent.
func (s *Store) GetReviewControl(ctx context.Context, projectID, mrIID int64) (*store.ReviewControl, error) {
	if projectID <= 0 || mrIID <= 0 {
		return nil, errors.New("sqlite: review control project id and mr iid are required")
	}
	var (
		control    store.ReviewControl
		cancelled  int
		updatedRaw string
	)
	err := s.db.QueryRowContext(ctx, `SELECT project_id, mr_iid, generation, head_sha,
		cancelled, candidate_seq, barrier_seq, updated_at
		FROM review_controls WHERE project_id = ? AND mr_iid = ?`,
		projectID, mrIID).Scan(&control.ProjectID, &control.MRIID, &control.Generation,
		&control.HeadSHA, &cancelled, &control.CandidateSeq, &control.BarrierSeq, &updatedRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: review control for project %d MR %d", store.ErrNotFound, projectID, mrIID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get review control: %w", err)
	}
	control.Cancelled = cancelled != 0
	if control.UpdatedAt, err = parseTime(updatedRaw); err != nil {
		return nil, err
	}
	return &control, nil
}

// RecordReviewDelivery records one webhook delivery and, when command is not
// nil, atomically updates review_controls and enqueues the run or cancel job.
// A replayed (project_id, uuid) returns the original job ID with
// duplicate=true and changes neither the control generation nor the queue.
//
// The generation is assigned inside the transaction and written into the job
// payload. Caller payload must not include generation/head_sha conflicts for
// the command kind; see store.ReviewCommand.
func (s *Store) RecordReviewDelivery(ctx context.Context, d store.Delivery, command *store.ReviewCommand) (store.ReviewEnqueueResult, error) {
	if d.UUID == "" {
		return store.ReviewEnqueueResult{}, errors.New("sqlite: delivery uuid is required")
	}
	if d.ProjectID <= 0 {
		return store.ReviewEnqueueResult{}, errors.New("sqlite: delivery project id is required")
	}
	if d.Event == "" {
		return store.ReviewEnqueueResult{}, errors.New("sqlite: delivery event is required")
	}
	switch d.Outcome {
	case store.DeliveryEnqueued:
		if command == nil {
			return store.ReviewEnqueueResult{}, errors.New("sqlite: enqueued delivery requires a review command")
		}
	case store.DeliveryIgnored, store.DeliveryRejected:
		if command != nil {
			return store.ReviewEnqueueResult{}, errors.New("sqlite: ignored or rejected delivery must not enqueue a review command")
		}
	default:
		return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: invalid delivery outcome %q", d.Outcome)
	}
	if command != nil && command.ProjectID != d.ProjectID {
		return store.ReviewEnqueueResult{}, errors.New("sqlite: delivery and review command project ids differ")
	}

	now := s.now().UTC()
	if d.ReceivedAt.IsZero() {
		d.ReceivedAt = now
	} else {
		d.ReceivedAt = d.ReceivedAt.UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: begin review delivery: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `INSERT INTO webhook_deliveries
		(project_id, uuid, event, outcome, reason, job_id, received_at)
		VALUES (?, ?, ?, ?, ?, NULL, ?)
		ON CONFLICT (project_id, uuid) DO NOTHING`,
		d.ProjectID, d.UUID, d.Event, d.Outcome, nullString(d.Reason), formatTime(d.ReceivedAt))
	if err != nil {
		return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: insert delivery: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: insert delivery: %w", err)
	}
	if inserted == 0 {
		var existing sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT job_id FROM webhook_deliveries
			WHERE project_id = ? AND uuid = ?`, d.ProjectID, d.UUID).Scan(&existing); err != nil {
			return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: read delivery: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: commit delivery: %w", err)
		}
		return store.ReviewEnqueueResult{JobID: existing.String, Duplicate: true}, nil
	}

	result := store.ReviewEnqueueResult{}
	if command != nil {
		jobID, generation, duplicate, err := s.enqueueReviewTx(ctx, tx, *command, now)
		if err != nil {
			return store.ReviewEnqueueResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE webhook_deliveries SET job_id = ?
			WHERE project_id = ? AND uuid = ?`, jobID, d.ProjectID, d.UUID); err != nil {
			return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: attach delivery job: %w", err)
		}
		result.JobID = jobID
		result.Generation = generation
		result.Duplicate = duplicate
	}
	if err := tx.Commit(); err != nil {
		return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: commit review delivery: %w", err)
	}
	return result, nil
}

// EnqueueReviewLatest enqueues an authenticated review.run or review.cancel
// without a webhook delivery. A queued job with the same key is replaced with
// the newest payload; a running job is never modified, and its attempt fence
// stays monotonic. The generation is assigned in the same transaction.
//
// An authenticated command with a source note (ReviewCommand.NoteID and
// Command) is also recorded by identity in that transaction. A replay of the
// same identity returns the original job ID with Duplicate=true and changes
// neither the control nor the queue.
func (s *Store) EnqueueReviewLatest(ctx context.Context, command store.ReviewCommand) (store.ReviewEnqueueResult, error) {
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: begin review enqueue: %w", err)
	}
	defer tx.Rollback()

	jobID, generation, duplicate, err := s.enqueueReviewTx(ctx, tx, command, now)
	if err != nil {
		return store.ReviewEnqueueResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return store.ReviewEnqueueResult{}, fmt.Errorf("sqlite: commit review enqueue: %w", err)
	}
	return store.ReviewEnqueueResult{JobID: jobID, Generation: generation, Duplicate: duplicate}, nil
}

// enqueueReviewTx validates the command, increments review_controls, builds the
// job payload with the assigned generation, and enqueues it. All writes share
// the caller's transaction. A duplicate command identity returns the original
// job ID with generation zero and writes nothing.
func (s *Store) enqueueReviewTx(ctx context.Context, tx *sql.Tx, command store.ReviewCommand, now time.Time) (string, int64, bool, error) {
	if command.ProjectID <= 0 {
		return "", 0, false, errors.New("sqlite: review command project id is required")
	}
	if command.MRIID <= 0 {
		return "", 0, false, errors.New("sqlite: review command mr iid is required")
	}
	head, err := normalizeReviewHead(command.HeadSHA, command.Cancel)
	if err != nil {
		return "", 0, false, err
	}
	if err := requireProjectTx(ctx, tx, command.ProjectID); err != nil {
		return "", 0, false, err
	}
	identity, replay, err := commandReplayIdentity(command)
	if err != nil {
		return "", 0, false, err
	}
	if replay {
		jobID, duplicate, err := reserveCommandReplayTx(ctx, tx, command, identity, now)
		if err != nil {
			return "", 0, false, err
		}
		if duplicate {
			return jobID, 0, true, nil
		}
	}

	generation, err := bumpReviewControlTx(ctx, tx, command, head, now)
	if err != nil {
		return "", 0, false, err
	}
	control, err := getReviewControlTx(ctx, tx, command.ProjectID, command.MRIID)
	if err != nil {
		return "", 0, false, err
	}
	if err := s.recordReviewSideEffectsTx(ctx, tx, command, control, generation, now); err != nil {
		return "", 0, false, err
	}
	if command.MaxAttempts == 0 {
		command.MaxAttempts = defaultReviewMaxAttempts
	}
	payload, err := reviewJobPayload(command, head, generation)
	if err != nil {
		return "", 0, false, err
	}
	kind, key := reviewJobKindKey(command)
	job := store.Job{
		Kind:        kind,
		Key:         key,
		Payload:     payload,
		MaxAttempts: command.MaxAttempts,
	}
	if err := s.prepareJob(&job, now); err != nil {
		return "", 0, false, err
	}
	jobID, _, err := enqueueTx(ctx, tx, &job, true)
	if err != nil {
		return "", 0, false, err
	}
	if replay {
		if err := attachCommandReplayTx(ctx, tx, command, identity, jobID); err != nil {
			return "", 0, false, err
		}
	}
	return jobID, generation, false, nil
}

const (
	commandReplayBackfillBatch = 500
	// Legacy review payloads passed numbers through float64; their reactions
	// also used the rounded note ID, so 2^53 can identify a different note.
	maxReplayExactNoteID = 1<<53 - 1
)

// commandReplayNoteKey is one source note on one merge request.
type commandReplayNoteKey struct {
	projectID int64
	mrIID     int64
	noteID    int64
}

// commandReplayNoteState accumulates every retained kind that can be
// attributed to one source note before the one-time backfill decides.
type commandReplayNoteState struct {
	kinds              map[string]bool   // all retained command kinds and review evidence
	candidates         map[string]bool   // kind -> reaction proof allowed for runnable/failed jobs
	reviewIDs          map[string]string // kind -> downstream review job ID, if known
	hasReaction        bool
	reactionGeneration int64
	hasControl         bool
	controlGeneration  int64
	cancelled          bool
}

func newCommandReplayNoteState() *commandReplayNoteState {
	return &commandReplayNoteState{
		kinds:      make(map[string]bool),
		candidates: make(map[string]bool),
		reviewIDs:  make(map[string]string),
	}
}

// proven reports whether the enqueue of this kind committed.
// Kind-specific review evidence is direct proof. For runnable/failed jobs, a
// reaction only proves a kind when the note has one known kind and the
// reaction cannot belong to a different current intent.
func (st *commandReplayNoteState) proven(kind string, allowReaction bool) bool {
	if _, ok := st.reviewIDs[kind]; ok {
		return true
	}
	if !allowReaction || !st.hasReaction || len(st.kinds) != 1 || !st.kinds[kind] {
		return false
	}
	// An older reaction may belong to an unrelated, pruned command of the
	// opposite kind. Only a reaction at the current control generation, with a
	// matching control intent, proves this exact kind.
	if !st.hasControl || st.reactionGeneration != st.controlGeneration {
		return false
	}
	controlKind := "run"
	if st.cancelled {
		controlKind = "cancel"
	}
	return controlKind == kind
}

type commandReplayJobPayload struct {
	ProjectID int64  `json:"project_id"`
	MRIID     int64  `json:"mr_iid"`
	NoteID    int64  `json:"note_id"`
	Trigger   string `json:"trigger"`
	Command   string `json:"command"`
}

// backfillReviewCommandReplays backfills identities for commands accepted
// before migration 0020. The marker and identities commit together. A command
// job alone does not prove enqueue; a matching downstream job or a bound
// command-triggered review dossier does, even if both jobs have been pruned.
//
// All retained command jobs are candidates. A done or cancelled command can
// have been denied, ignored, or stopped before enqueue, so it requires direct
// kind-specific job or dossier evidence. For queued/running/failed candidates, a
// reaction can also prove the kind under the existing strict generation and
// intent checks. All retained command states, kind-specific review jobs, and
// proven dossier runs count as known kinds, so a shared reaction cannot prove
// either of two kinds.
// Ambiguous or pruned history is left unbackfilled instead of inventing a kind.
func backfillReviewCommandReplays(ctx context.Context, db *sql.DB, now time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin command replay backfill: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO review_command_replay_backfill
		(id, completed_at) VALUES (1, ?) ON CONFLICT (id) DO NOTHING`, formatTime(now))
	if err != nil {
		return fmt.Errorf("sqlite: reserve command replay backfill: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: reserve command replay backfill: %w", err)
	}
	if inserted == 0 {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqlite: commit command replay backfill: %w", err)
		}
		return nil
	}

	states, err := listCommandReplayCandidatesTx(ctx, tx)
	if err != nil {
		return err
	}
	if err := loadCommandReplayDossiersTx(ctx, tx, states); err != nil {
		return err
	}
	if len(states) == 0 {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqlite: commit command replay backfill: %w", err)
		}
		return nil
	}
	if err := loadCommandReplayEvidenceTx(ctx, tx, states); err != nil {
		return err
	}
	for noteKey, state := range states {
		for kind, allowReaction := range state.candidates {
			if !state.proven(kind, allowReaction) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO review_command_replays
				(project_id, mr_iid, note_id, command, job_id, created_at)
				VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (project_id, mr_iid, note_id, command) DO NOTHING`,
				noteKey.projectID, noteKey.mrIID, noteKey.noteID, kind, state.reviewIDs[kind], formatTime(now)); err != nil {
				return fmt.Errorf("sqlite: backfill command replay: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit command replay backfill: %w", err)
	}
	return nil
}

// listCommandReplayCandidatesTx streams each retained job once. Valid
// downstream jobs prove their own kind, including when the command was pruned.
// Only compact identity state is retained; raw payloads are not accumulated.
func listCommandReplayCandidatesTx(ctx context.Context, tx *sql.Tx) (map[commandReplayNoteKey]*commandReplayNoteState, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, COALESCE("key", ''), payload, state FROM jobs
		WHERE kind IN ('review.command', 'review.run', 'review.cancel')`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list legacy review jobs: %w", err)
	}
	states := make(map[commandReplayNoteKey]*commandReplayNoteState)
	for rows.Next() {
		var jobID, jobKind, key, jobState string
		var raw []byte
		if err := rows.Scan(&jobID, &jobKind, &key, &raw, &jobState); err != nil {
			rows.Close()
			return nil, fmt.Errorf("sqlite: scan legacy review job: %w", err)
		}
		var payload commandReplayJobPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			continue
		}
		if payload.ProjectID <= 0 || payload.MRIID <= 0 || payload.NoteID <= 0 {
			continue
		}
		if jobKind != "review.command" && payload.NoteID > maxReplayExactNoteID {
			continue
		}
		kind := ""
		if jobKind == "review.command" {
			kind = strings.ToLower(strings.TrimSpace(payload.Command))
			if (kind != "run" && kind != "cancel") || key != commandReplayJobKey(payload.ProjectID, payload.NoteID) {
				continue
			}
		} else {
			kind = "run"
			prefix := "review:"
			if jobKind == reviewCancelKind {
				kind = "cancel"
				prefix = "review.cancel:"
			}
			expectedKey := prefix + strconv.FormatInt(payload.ProjectID, 10) + ":" + strconv.FormatInt(payload.MRIID, 10)
			if key != expectedKey || payload.Trigger != "command" {
				continue
			}
		}
		if !canonicalReplayJob(raw, jobKind == "review.command") {
			continue
		}
		noteKey := commandReplayNoteKey{payload.ProjectID, payload.MRIID, payload.NoteID}
		if states[noteKey] == nil {
			states[noteKey] = newCommandReplayNoteState()
		}
		state := states[noteKey]
		state.kinds[kind] = true
		if jobKind != "review.command" {
			state.reviewIDs[kind] = jobID
			if _, exists := state.candidates[kind]; !exists {
				state.candidates[kind] = false
			}
		} else if jobState == "queued" || jobState == "running" || jobState == "failed" {
			state.candidates[kind] = payload.NoteID <= maxReplayExactNoteID
		} else if jobState == "done" || jobState == "cancelled" {
			if _, exists := state.candidates[kind]; !exists {
				state.candidates[kind] = false
			}
		}
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("sqlite: close legacy review jobs: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate legacy review jobs: %w", err)
	}
	return states, nil
}

func commandReplayJobKey(projectID, noteID int64) string {
	return "review.command:" + strconv.FormatInt(projectID, 10) + ":" + strconv.FormatInt(noteID, 10)
}

// replayObject rejects ambiguous duplicate keys in provenance objects.
func replayObject(raw []byte) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	fields := make(map[string]json.RawMessage)
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		folded := strings.ToLower(key)
		if err != nil || !ok || seen[folded] {
			return nil, false
		}
		seen[folded] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		fields[key] = value
	}
	_, err = decoder.Token()
	return fields, err == nil
}

func canonicalReplayJob(raw []byte, command bool) bool {
	fields, valid := replayObject(raw)
	if !valid || fields["project_id"] == nil || fields["mr_iid"] == nil || fields["note_id"] == nil {
		return false
	}
	if command {
		return fields["command"] != nil
	}
	return fields["trigger"] != nil
}

// loadCommandReplayDossiersTx uses only a workflow-bound, fingerprint-matched
// review snapshot. A dossier proves a run, never a cancel. Orphan reservations,
// damaged snapshots, and conflicting job mappings cannot prove an identity.
func loadCommandReplayDossiersTx(ctx context.Context, tx *sql.Tx, states map[commandReplayNoteKey]*commandReplayNoteState) error {
	rows, err := tx.QueryContext(ctx, `SELECT w.project_id, w.mr_iid, COALESCE(w.head_sha, ''),
		COALESCE(w.fingerprint, ''), COALESCE(d.job_id, ''), d.hash, d.data,
		COALESCE(j.id, ''), COALESCE(j.kind, ''), COALESCE(j."key", ''), COALESCE(j.payload, '')
		FROM workflows w JOIN workflow_dossiers d ON d.workflow_id = w.id
		LEFT JOIN jobs j ON j.id = d.job_id
		WHERE w.kind = 'review' AND w.trigger = 'command'
		  AND w.project_id > 0 AND w.mr_iid > 0 AND d.project_id = w.project_id
		  AND d.generation > 0 AND d.version = 1`)
	if err != nil {
		return fmt.Errorf("sqlite: list legacy command dossiers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var projectID, mrIID int64
		var head, fingerprint, jobID, hash, linkedID, linkedKind, linkedKey string
		var compressed, linkedPayload []byte
		if err := rows.Scan(&projectID, &mrIID, &head, &fingerprint, &jobID, &hash, &compressed,
			&linkedID, &linkedKind, &linkedKey, &linkedPayload); err != nil {
			return fmt.Errorf("sqlite: scan legacy command dossier: %w", err)
		}
		data, err := decompressDossier(compressed)
		if err != nil {
			continue
		}
		snapshot, valid := replayObject(data)
		if !valid {
			continue
		}
		project, projectOK := replayObject(snapshot["project"])
		mr, mrOK := replayObject(snapshot["mr"])
		diff, diffOK := replayObject(snapshot["diff_version"])
		_, metadataOK := replayObject(snapshot["metadata"])
		var schema int
		var dossierProjectID, dossierMRIID int64
		var dossierHead string
		var metadata map[string]string
		if !projectOK || !mrOK || !diffOK || !metadataOK ||
			json.Unmarshal(snapshot["schema_version"], &schema) != nil || schema != 1 ||
			json.Unmarshal(project["id"], &dossierProjectID) != nil || json.Unmarshal(mr["iid"], &dossierMRIID) != nil ||
			json.Unmarshal(diff["HeadSHA"], &dossierHead) != nil || json.Unmarshal(snapshot["metadata"], &metadata) != nil ||
			dossierProjectID != projectID || dossierMRIID != mrIID || !reviewHeadRE.MatchString(head) ||
			!strings.EqualFold(dossierHead, head) || metadata["trigger"] != "command" ||
			metadata["config_hash"] == "" || metadata["prompt_hash"] == "" {
			continue
		}
		noteID, err := strconv.ParseInt(metadata["note_id"], 10, 64)
		if err != nil || noteID <= 0 || noteID > maxReplayExactNoteID || strconv.FormatInt(noteID, 10) != metadata["note_id"] {
			continue
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != hash {
			continue
		}
		// The workflow fingerprint was made from the dossier hash and the two
		// hashes in its metadata when the review workflow was created.
		bound := sha256.Sum256([]byte(hash + "\n" + metadata["config_hash"] + "\n" + metadata["prompt_hash"]))
		if hex.EncodeToString(bound[:]) != fingerprint {
			continue
		}
		if linkedID != "" {
			var payload commandReplayJobPayload
			if linkedKind != reviewRunKind || linkedKey != "review:"+strconv.FormatInt(projectID, 10)+":"+strconv.FormatInt(mrIID, 10) ||
				!canonicalReplayJob(linkedPayload, false) || json.Unmarshal(linkedPayload, &payload) != nil || payload.ProjectID != projectID || payload.MRIID != mrIID ||
				payload.NoteID != noteID || payload.Trigger != "command" {
				continue
			}
		}
		key := commandReplayNoteKey{projectID, mrIID, noteID}
		if states[key] == nil {
			states[key] = newCommandReplayNoteState()
		}
		state := states[key]
		state.kinds["run"] = true
		if _, exists := state.reviewIDs["run"]; !exists {
			state.reviewIDs["run"] = jobID
		}
		if _, exists := state.candidates["run"]; !exists {
			state.candidates["run"] = false
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: iterate legacy command dossiers: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("sqlite: close legacy command dossiers: %w", err)
	}
	return nil
}

// loadCommandReplayEvidenceTx loads reactions and controls only for candidates
// eligible for reaction proof. Job evidence was collected in the streaming scan.
func loadCommandReplayEvidenceTx(ctx context.Context, tx *sql.Tx, states map[commandReplayNoteKey]*commandReplayNoteState) error {
	groups := make(map[[2]int64][]commandReplayNoteKey)
	for key := range states {
		for _, allowReaction := range states[key].candidates {
			if allowReaction {
				groupKey := [2]int64{key.projectID, key.mrIID}
				groups[groupKey] = append(groups[groupKey], key)
				break
			}
		}
	}
	for groupKey, notes := range groups {
		projectID, mrIID := groupKey[0], groupKey[1]
		byNote := make(map[int64]*commandReplayNoteState, len(notes))
		for _, noteKey := range notes {
			byNote[noteKey.noteID] = states[noteKey]
		}
		noteIDs := make([]int64, 0, len(byNote))
		for noteID := range byNote {
			noteIDs = append(noteIDs, noteID)
		}
		for start := 0; start < len(noteIDs); start += commandReplayBackfillBatch {
			end := start + commandReplayBackfillBatch
			if end > len(noteIDs) {
				end = len(noteIDs)
			}
			batch := noteIDs[start:end]
			args := make([]any, 0, len(batch)+2)
			args = append(args, projectID, mrIID)
			for _, noteID := range batch {
				args = append(args, noteID)
			}
			rows, err := tx.QueryContext(ctx, `SELECT note_id, generation FROM review_reactions
				WHERE project_id = ? AND mr_iid = ? AND note_id IN (`+sqlPlaceholders(len(batch))+`)`,
				args...)
			if err != nil {
				return fmt.Errorf("sqlite: list legacy review reactions: %w", err)
			}
			for rows.Next() {
				var noteID, generation int64
				if err := rows.Scan(&noteID, &generation); err != nil {
					rows.Close()
					return fmt.Errorf("sqlite: scan legacy review reaction: %w", err)
				}
				if state := byNote[noteID]; state != nil {
					state.hasReaction = true
					state.reactionGeneration = generation
				}
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("sqlite: close legacy review reactions: %w", err)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("sqlite: iterate legacy review reactions: %w", err)
			}
		}

		var (
			generation int64
			cancelled  int
		)
		switch err := tx.QueryRowContext(ctx, `SELECT generation, cancelled FROM review_controls
			WHERE project_id = ? AND mr_iid = ?`, projectID, mrIID).Scan(&generation, &cancelled); {
		case err == nil:
			for _, state := range byNote {
				state.hasControl = true
				state.controlGeneration = generation
				state.cancelled = cancelled != 0
			}
		case errors.Is(err, sql.ErrNoRows):
		default:
			return fmt.Errorf("sqlite: read legacy review control: %w", err)
		}
	}
	return nil
}

// commandReplayIdentity normalizes the authenticated source-note identity.
// Producers without any identity fields keep the old behavior. An explicit but
// incomplete or mismatched identity is rejected instead of silently falling
// back to a non-idempotent enqueue.
func commandReplayIdentity(command store.ReviewCommand) (string, bool, error) {
	if command.NoteID < 0 {
		return "", false, errors.New("sqlite: review command note id must not be negative")
	}
	kind := strings.ToLower(strings.TrimSpace(command.Command))
	hasNote := command.NoteID > 0
	hasCommand := kind != ""
	if !hasNote && !hasCommand {
		return "", false, nil
	}
	if !hasNote || !hasCommand {
		return "", false, errors.New("sqlite: review command identity requires both note id and command")
	}
	switch kind {
	case "run":
		if command.Cancel {
			return "", false, errors.New("sqlite: review command kind run conflicts with cancel")
		}
	case "cancel":
		if !command.Cancel {
			return "", false, errors.New("sqlite: review command kind cancel requires cancel")
		}
	default:
		return "", false, fmt.Errorf("sqlite: unknown review command kind %q", kind)
	}
	if err := validateCommandPayloadNoteID(command.Payload, command.NoteID); err != nil {
		return "", false, err
	}
	return kind, true, nil
}

// validateCommandPayloadNoteID rejects an explicit command whose payload
// names a different note. A producer without identity skips this check, so
// webhook and manual paths with a payload note_id keep their old behavior.
func validateCommandPayloadNoteID(raw json.RawMessage, noteID int64) error {
	payload := bytes.TrimSpace(raw)
	if len(payload) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return fmt.Errorf("sqlite: review command payload must be a JSON object: %w", err)
	}
	noteRaw, ok := fields["note_id"]
	if !ok {
		return nil
	}
	var payloadNoteID int64
	if err := json.Unmarshal(noteRaw, &payloadNoteID); err != nil {
		return errors.New("sqlite: review command payload note_id must be an integer")
	}
	if payloadNoteID != noteID {
		return errors.New("sqlite: review command payload note_id does not match the identity")
	}
	return nil
}

// reserveCommandReplayTx claims one command identity in the caller's
// transaction. It reports duplicate=true with the original job ID when the
// identity already exists.
func reserveCommandReplayTx(ctx context.Context, tx *sql.Tx, command store.ReviewCommand, identity string, now time.Time) (string, bool, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO review_command_replays
		(project_id, mr_iid, note_id, command, job_id, created_at)
		VALUES (?, ?, ?, ?, '', ?)
		ON CONFLICT (project_id, mr_iid, note_id, command) DO NOTHING`,
		command.ProjectID, command.MRIID, command.NoteID, identity, formatTime(now))
	if err != nil {
		return "", false, fmt.Errorf("sqlite: reserve command replay: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return "", false, fmt.Errorf("sqlite: reserve command replay: %w", err)
	}
	if inserted == 1 {
		return "", false, nil
	}
	var jobID string
	if err := tx.QueryRowContext(ctx, `SELECT job_id FROM review_command_replays
		WHERE project_id = ? AND mr_iid = ? AND note_id = ? AND command = ?`,
		command.ProjectID, command.MRIID, command.NoteID, identity).Scan(&jobID); err != nil {
		return "", false, fmt.Errorf("sqlite: read command replay: %w", err)
	}
	return jobID, true, nil
}

func attachCommandReplayTx(ctx context.Context, tx *sql.Tx, command store.ReviewCommand, identity, jobID string) error {
	res, err := tx.ExecContext(ctx, `UPDATE review_command_replays SET job_id = ?
		WHERE project_id = ? AND mr_iid = ? AND note_id = ? AND command = ?`,
		jobID, command.ProjectID, command.MRIID, command.NoteID, identity)
	if err != nil {
		return fmt.Errorf("sqlite: attach command replay job: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: attach command replay job: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("sqlite: command replay identity changed")
	}
	return nil
}

// reviewCommandSideEffects parses the fields the queue owns for side effects.
// The queued command payload keeps its existing shape; no new key is required.
type reviewCommandSideEffects struct {
	NoteID  int64  `json:"note_id"`
	Profile string `json:"profile"`
}

func (s *Store) recordReviewSideEffectsTx(ctx context.Context, tx *sql.Tx, command store.ReviewCommand, control *store.ReviewControl, generation int64, now time.Time) error {
	var meta reviewCommandSideEffects
	if raw := bytes.TrimSpace(command.Payload); len(raw) > 0 {
		if err := json.Unmarshal(raw, &meta); err != nil {
			return fmt.Errorf("sqlite: review payload must be a JSON object: %w", err)
		}
	}
	goal := store.ReviewStatusGoal{
		ProjectID:  command.ProjectID,
		MRIID:      command.MRIID,
		Generation: generation,
		HeadSHA:    control.HeadSHA,
		Profile:    meta.Profile,
		State:      store.ReviewStatusQueued,
	}
	if _, err := s.setReviewStatusTx(ctx, tx, goal, now); err != nil {
		return err
	}
	if _, err := s.clearReviewReactionsTx(ctx, tx, command.ProjectID, command.MRIID, generation, true, now); err != nil {
		return err
	}
	if meta.NoteID > 0 {
		if _, err := s.setReviewReactionTx(ctx, tx, store.ReviewReactionGoal{
			ProjectID:  command.ProjectID,
			MRIID:      command.MRIID,
			NoteID:     meta.NoteID,
			Generation: generation,
			Desired:    store.ReviewReactionEyes,
		}, now); err != nil {
			return err
		}
	}
	return nil
}

func bumpReviewControlTx(ctx context.Context, tx *sql.Tx, command store.ReviewCommand, head string, now time.Time) (int64, error) {
	cancelled := 0
	if command.Cancel {
		cancelled = 1
	}
	var generation int64
	err := tx.QueryRowContext(ctx, `INSERT INTO review_controls
		(project_id, mr_iid, generation, head_sha, cancelled, candidate_seq, barrier_seq, updated_at)
		VALUES (?, ?, 1, ?, ?, 0, 0, ?)
		ON CONFLICT (project_id, mr_iid) DO UPDATE SET
			generation = review_controls.generation + 1,
			head_sha = CASE WHEN excluded.cancelled = 1
				THEN review_controls.head_sha ELSE excluded.head_sha END,
			cancelled = excluded.cancelled,
			barrier_seq = review_controls.candidate_seq,
			updated_at = excluded.updated_at
		RETURNING generation`,
		command.ProjectID, command.MRIID, head, cancelled, formatTime(now)).Scan(&generation)
	if err != nil {
		return 0, fmt.Errorf("sqlite: update review control: %w", err)
	}
	return generation, nil
}

func normalizeReviewHead(raw string, cancel bool) (string, error) {
	head := strings.ToLower(strings.TrimSpace(raw))
	if cancel {
		if head != "" {
			return "", errors.New("sqlite: review cancel must not carry a head sha")
		}
		return "", nil
	}
	if !reviewHeadRE.MatchString(head) {
		return "", errors.New("sqlite: review head sha must be 40 or 64 hex characters")
	}
	return head, nil
}

func reviewJobKindKey(command store.ReviewCommand) (string, string) {
	if command.Cancel {
		return reviewCancelKind,
			"review.cancel:" + strconv.FormatInt(command.ProjectID, 10) + ":" + strconv.FormatInt(command.MRIID, 10)
	}
	return reviewRunKind,
		"review:" + strconv.FormatInt(command.ProjectID, 10) + ":" + strconv.FormatInt(command.MRIID, 10)
}

func reviewJobPayload(command store.ReviewCommand, head string, generation int64) (json.RawMessage, error) {
	fields := make(map[string]any)
	if raw := bytes.TrimSpace(command.Payload); len(raw) > 0 {
		// Keep JSON numbers exact. Note IDs are int64 and can exceed 2^53,
		// where the default float64 conversion could target another note.
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&fields); err != nil {
			return nil, fmt.Errorf("sqlite: review payload must be a JSON object: %w", err)
		}
		if fields == nil {
			fields = make(map[string]any)
		}
		if _, err := decoder.Token(); err != io.EOF {
			return nil, errors.New("sqlite: review payload must be a single JSON object")
		}
	}
	if _, ok := fields["generation"]; ok {
		return nil, errors.New("sqlite: review payload must not contain generation; the store assigns it")
	}
	fields["project_id"] = command.ProjectID
	fields["mr_iid"] = command.MRIID
	fields["generation"] = generation
	if command.Cancel {
		delete(fields, "head_sha")
	} else {
		fields["head_sha"] = head
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("sqlite: encode review payload: %w", err)
	}
	return encoded, nil
}

// ReviewWorkflowRequest is the atomic review-workflow creation input. The
// caller creates the job first, then calls CreateReviewWorkflow from the job
// handler. DossierData is sanitized JSON; DossierHash is optional and, when
// set, must equal the SHA-256 of the uncompressed data.
type ReviewWorkflowRequest struct {
	JobID              string
	Spec               dag.GraphSpec
	Meta               map[string]string
	DossierVersion     int
	DossierHash        string
	DossierData        json.RawMessage
	ExpectedGeneration int64
	ProjectID          int64
	MRIID              int64
	HeadSHA            string
}

// CreateReviewWorkflow durably stores the dossier and job mapping before
// appending dag.workflow.created. A retry for the same job returns the original
// workflow ID. A stale/cancelled review generation returns ErrSuperseded.
//
// Crash windows:
//   - dossier saved, append not committed: the next call reuses the reserved
//     workflow ID and appends again;
//   - append committed: the next call returns the existing workflow.
//
// The generation check happens before the dossier write. A push that lands
// between this check and the append is caught by the caller re-reading
// GetReviewControl immediately after CreateReviewWorkflow, and again before
// publication.
func (s *Store) CreateReviewWorkflow(ctx context.Context, req ReviewWorkflowRequest) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if strings.TrimSpace(req.JobID) == "" {
		return "", false, errors.New("sqlite: review workflow job id is required")
	}
	if req.ProjectID <= 0 || req.MRIID <= 0 {
		return "", false, errors.New("sqlite: review workflow project id and mr iid are required")
	}
	if req.ExpectedGeneration <= 0 {
		return "", false, errors.New("sqlite: review workflow expected generation is required")
	}
	head, err := normalizeReviewHead(req.HeadSHA, false)
	if err != nil {
		return "", false, fmt.Errorf("sqlite: review workflow: %w", err)
	}
	if req.DossierVersion <= 0 {
		return "", false, errors.New("sqlite: workflow dossier version is required")
	}
	data := bytes.TrimSpace(req.DossierData)
	if len(data) == 0 || !json.Valid(data) {
		return "", false, errors.New("sqlite: workflow dossier data must be valid JSON")
	}
	if len(data) > maxWorkflowDossierBytes {
		return "", false, fmt.Errorf("sqlite: workflow dossier exceeds %d bytes", maxWorkflowDossierBytes)
	}

	if existing, err := s.GetWorkflowByJob(ctx, req.JobID); err == nil {
		if existing.ProjectID != req.ProjectID {
			return "", false, fmt.Errorf("%w: workflow job belongs to another project", store.ErrConflict)
		}
		dossier, err := s.GetWorkflowDossier(ctx, existing.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return "", false, fmt.Errorf("%w: workflow %s for job %s", store.ErrIncompleteWorkflow, existing.ID, req.JobID)
			}
			return "", false, err
		}
		control, err := s.GetReviewControl(ctx, req.ProjectID, req.MRIID)
		if err != nil {
			return "", false, err
		}
		if control.Cancelled || control.Generation != req.ExpectedGeneration ||
			control.HeadSHA != head {
			return "", false, fmt.Errorf("%w: workflow %s for job %s has generation %d head %s",
				store.ErrSuperseded, existing.ID, req.JobID, dossier.Generation, existing.HeadSHA)
		}
		if dossier.Generation == req.ExpectedGeneration && strings.EqualFold(existing.HeadSHA, head) {
			return existing.ID, true, nil
		}
		// A queued job keeps its ID when a newer push replaces its payload.
		// The old mapping belongs to a stale generation; the current control
		// matches this request, so detach the mapping and create the current
		// workflow. The old workflow and dossier remain for audit.
		if err := s.DetachWorkflowJob(ctx, existing.ID, req.JobID); err != nil &&
			!errors.Is(err, store.ErrNotFound) {
			return "", false, err
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", false, err
	}

	control, err := s.GetReviewControl(ctx, req.ProjectID, req.MRIID)
	if err != nil {
		return "", false, err
	}
	if control.Cancelled || control.Generation != req.ExpectedGeneration || control.HeadSHA != head {
		return "", false, fmt.Errorf("%w: project %d MR %d generation %d head %s",
			store.ErrSuperseded, req.ProjectID, req.MRIID, req.ExpectedGeneration, head)
	}

	now := s.now().UTC()
	meta, err := reviewWorkflowMeta(req, head, now)
	if err != nil {
		return "", false, err
	}
	wfID, err := s.reserveReviewDossier(ctx, req, now)
	if err != nil {
		return "", false, err
	}
	if existing, err := s.GetWorkflow(ctx, wfID); err == nil {
		return existing.ID, true, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", false, err
	}

	payload, err := json.Marshal(dag.WorkflowCreatedPayload{
		WorkflowID: wfID,
		Spec:       req.Spec,
		Meta:       meta,
	})
	if err != nil {
		return "", false, fmt.Errorf("sqlite: encode workflow.created: %w", err)
	}
	event := dag.Event{Type: dag.EventWorkflowCreated, Payload: payload, At: now}
	if _, err := s.Append(ctx, wfID, 0, event); err != nil {
		// Keep the dossier: a retry reuses this workflow ID (crash-window
		// recovery), and deleting could remove a dossier another worker created.
		return "", false, err
	}
	return wfID, false, nil
}

func reviewWorkflowMeta(req ReviewWorkflowRequest, head string, now time.Time) (map[string]string, error) {
	meta := make(map[string]string, len(req.Meta)+8)
	for key, value := range req.Meta {
		meta[key] = value
	}
	if kind := strings.TrimSpace(meta["kind"]); kind != "" && kind != "review" {
		return nil, fmt.Errorf("sqlite: review workflow metadata kind %q is invalid", kind)
	}
	meta["kind"] = "review"
	meta["project_id"] = strconv.FormatInt(req.ProjectID, 10)
	meta["mr_iid"] = strconv.FormatInt(req.MRIID, 10)
	meta["head_sha"] = head
	meta["generation"] = strconv.FormatInt(req.ExpectedGeneration, 10)
	for _, key := range []string{"trigger", "base_sha", "fingerprint", "config_hash", "prompt_hash"} {
		if strings.TrimSpace(meta[key]) == "" {
			return nil, fmt.Errorf("sqlite: review workflow metadata requires %s", key)
		}
	}
	if strings.TrimSpace(meta["started_at"]) == "" {
		meta["started_at"] = now.UTC().Format(time.RFC3339Nano)
	}
	return meta, nil
}

// reserveReviewDossier returns the workflow ID reserved for the job, forking a
// new ID when none exists yet, and upserts the dossier under that ID.
func (s *Store) reserveReviewDossier(ctx context.Context, req ReviewWorkflowRequest, now time.Time) (string, error) {
	wfID, err := s.dossierWorkflowIDByJob(ctx, req.JobID)
	if err != nil {
		return "", err
	}
	for attempt := 0; ; attempt++ {
		if wfID == "" {
			wfID, err = newID()
			if err != nil {
				return "", err
			}
		}
		_, err = s.SaveWorkflowDossier(ctx, store.WorkflowDossier{
			WorkflowID:       wfID,
			ProjectID:        req.ProjectID,
			JobID:            req.JobID,
			Generation:       req.ExpectedGeneration,
			Version:          req.DossierVersion,
			Hash:             req.DossierHash,
			Data:             req.DossierData,
			PublicationState: store.WorkflowPublicationPending,
		})
		if err == nil {
			return wfID, nil
		}
		if !errors.Is(err, store.ErrConflict) || attempt > 0 {
			return "", err
		}
		// Another worker reserved this job first. Reuse its workflow ID.
		existing, lookupErr := s.dossierWorkflowIDByJob(ctx, req.JobID)
		if lookupErr != nil {
			return "", lookupErr
		}
		if existing == "" {
			return "", err
		}
		wfID = existing
	}
}

func (s *Store) dossierWorkflowIDByJob(ctx context.Context, jobID string) (string, error) {
	var wfID string
	err := s.db.QueryRowContext(ctx,
		`SELECT workflow_id FROM workflow_dossiers WHERE job_id = ?`, jobID).Scan(&wfID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sqlite: look up workflow dossier by job: %w", err)
	}
	return wfID, nil
}

// GetWorkflowByJob resolves the durable job -> workflow mapping. A dossier with
// no workflow row means the previous create did not append; callers should call
// CreateReviewWorkflow again. Unknown jobs return ErrNotFound.
func (s *Store) GetWorkflowByJob(ctx context.Context, jobID string) (*store.Workflow, error) {
	if strings.TrimSpace(jobID) == "" {
		return nil, errors.New("sqlite: job id is required")
	}
	wf, err := scanWorkflow(s.db.QueryRowContext(ctx, `SELECT `+workflowColumns+`
		FROM workflows WHERE id = (SELECT workflow_id FROM workflow_dossiers WHERE job_id = ?)`,
		jobID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: workflow for job %s", store.ErrNotFound, jobID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get workflow by job: %w", err)
	}
	return wf, nil
}

// DetachWorkflowJob clears the job -> workflow mapping of one dossier so a
// retried job can store a fresh dossier after its configuration or prompt
// bundle changed. The workflow and dossier stay for audit. A pending
// publication is marked skipped so stale inputs are never published. The
// update is atomic: concurrent detaches return ErrNotFound for the loser.
func (s *Store) DetachWorkflowJob(ctx context.Context, workflowID, jobID string) error {
	workflowID = strings.TrimSpace(workflowID)
	jobID = strings.TrimSpace(jobID)
	if workflowID == "" || jobID == "" {
		return errors.New("sqlite: workflow id and job id are required")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE workflow_dossiers
		SET job_id = NULL,
		    publication_state = CASE WHEN publication_state = ? THEN ? ELSE publication_state END,
		    publication_error = CASE WHEN publication_state = ? THEN ? ELSE publication_error END,
		    updated_at = ?
		WHERE workflow_id = ? AND job_id = ?`,
		store.WorkflowPublicationPending, store.WorkflowPublicationSkipped,
		store.WorkflowPublicationPending, "review superseded by a newer review",
		formatTime(s.now().UTC()), workflowID, jobID)
	if err != nil {
		return fmt.Errorf("sqlite: detach workflow job: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: detach workflow job: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("%w: workflow %s job %s", store.ErrNotFound, workflowID, jobID)
	}
	return nil
}

// SaveWorkflowDossier validates and stores one sanitized dossier. Data must be
// valid JSON of at most 1 MiB and is gzip-compressed at rest. An empty Hash is
// computed; a non-empty Hash must match. JobID is unique across dossiers.
func (s *Store) SaveWorkflowDossier(ctx context.Context, dossier store.WorkflowDossier) (*store.WorkflowDossier, error) {
	dossier, data, err := normalizeDossier(dossier)
	if err != nil {
		return nil, err
	}
	compressed, err := compressDossier(data)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	created := dossier.CreatedAt.UTC()
	if created.IsZero() {
		created = now
	}
	if dossier.ProjectID < 0 {
		return nil, errors.New("sqlite: invalid dossier project id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin save dossier: %w", err)
	}
	defer tx.Rollback()
	var current, linked int64
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT project_id FROM workflow_dossiers WHERE workflow_id = ?), 0),
		COALESCE((SELECT project_id FROM workflows WHERE id = ?), 0)`, dossier.WorkflowID, dossier.WorkflowID).Scan(&current, &linked); err != nil {
		return nil, fmt.Errorf("sqlite: check dossier owner: %w", err)
	}
	if (current > 0 && linked > 0 && current != linked) ||
		(dossier.ProjectID > 0 && ((current > 0 && dossier.ProjectID != current) || (linked > 0 && dossier.ProjectID != linked))) {
		return nil, fmt.Errorf("%w: dossier owner mismatch", store.ErrConflict)
	}
	if dossier.ProjectID == 0 {
		dossier.ProjectID = current
		if dossier.ProjectID == 0 {
			dossier.ProjectID = linked
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workflow_dossiers
		(workflow_id, project_id, job_id, generation, version, hash, data, publication_state,
		 publication_error, mr_title, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (workflow_id) DO UPDATE SET
			project_id = excluded.project_id,
			job_id = excluded.job_id,
			generation = excluded.generation,
			version = excluded.version,
			hash = excluded.hash,
			data = excluded.data,
			publication_state = excluded.publication_state,
			publication_error = excluded.publication_error,
			mr_title = excluded.mr_title,
			updated_at = excluded.updated_at`,
		dossier.WorkflowID, dossier.ProjectID, nullString(dossier.JobID), dossier.Generation,
		dossier.Version, dossier.Hash, compressed, dossier.PublicationState,
		dossier.PublicationError, dossierMRTitle(data), formatTime(created), formatTime(now))
	if err != nil {
		if isConstraintErr(err) {
			return nil, fmt.Errorf("%w: workflow dossier job %s is already bound", store.ErrConflict, dossier.JobID)
		}
		return nil, fmt.Errorf("sqlite: save workflow dossier: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit dossier: %w", err)
	}
	return s.GetWorkflowDossier(ctx, dossier.WorkflowID)
}

// GetWorkflowDossier returns one dossier with decompressed JSON.
func (s *Store) GetWorkflowDossier(ctx context.Context, workflowID string) (*store.WorkflowDossier, error) {
	if strings.TrimSpace(workflowID) == "" {
		return nil, errors.New("sqlite: workflow id is required")
	}
	var (
		dossier    store.WorkflowDossier
		jobID      sql.NullString
		compressed []byte
		createdRaw string
		updatedRaw string
	)
	err := s.db.QueryRowContext(ctx, `SELECT workflow_id, project_id, job_id, generation, version,
		hash, data, publication_state, publication_error, created_at, updated_at
		FROM workflow_dossiers WHERE workflow_id = ?`, workflowID).Scan(
		&dossier.WorkflowID, &dossier.ProjectID, &jobID, &dossier.Generation, &dossier.Version,
		&dossier.Hash, &compressed, &dossier.PublicationState, &dossier.PublicationError,
		&createdRaw, &updatedRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: workflow dossier %s", store.ErrNotFound, workflowID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get workflow dossier: %w", err)
	}
	dossier.JobID = jobID.String
	data, err := decompressDossier(compressed)
	if err != nil {
		return nil, err
	}
	dossier.Data = data
	if dossier.CreatedAt, err = parseTime(createdRaw); err != nil {
		return nil, err
	}
	if dossier.UpdatedAt, err = parseTime(updatedRaw); err != nil {
		return nil, err
	}
	return &dossier, nil
}

// dossierMRTitle returns the trimmed mr.title of a dossier, or "" when the
// dossier has none. It is stored beside the compressed data so list views can
// read titles without decompressing every dossier.
func dossierMRTitle(data []byte) string {
	var snapshot struct {
		MR struct {
			Title string `json:"title"`
		} `json:"mr"`
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return ""
	}
	return strings.TrimSpace(snapshot.MR.Title)
}

// ListWorkflowMRTitles returns the persisted MR titles of several workflows
// in one query. Workflows without a dossier or with an empty title are absent.
func (s *Store) ListWorkflowMRTitles(ctx context.Context, workflowIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(workflowIDs))
	if len(workflowIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT workflow_id, mr_title FROM workflow_dossiers
		 WHERE mr_title <> '' AND workflow_id IN (`+sqlPlaceholders(len(workflowIDs))+`)`,
		stringArgs(workflowIDs)...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list workflow mr titles: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, title string
		if err := rows.Scan(&id, &title); err != nil {
			return nil, fmt.Errorf("sqlite: scan workflow mr title: %w", err)
		}
		out[id] = title
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate workflow mr titles: %w", err)
	}
	return out, nil
}

func normalizeDossier(dossier store.WorkflowDossier) (store.WorkflowDossier, []byte, error) {
	dossier.WorkflowID = strings.TrimSpace(dossier.WorkflowID)
	dossier.JobID = strings.TrimSpace(dossier.JobID)
	if dossier.WorkflowID == "" {
		return dossier, nil, errors.New("sqlite: workflow dossier id is required")
	}
	if dossier.Version <= 0 {
		return dossier, nil, errors.New("sqlite: workflow dossier version must be positive")
	}
	data := bytes.TrimSpace(dossier.Data)
	if len(data) == 0 || !json.Valid(data) {
		return dossier, nil, errors.New("sqlite: workflow dossier data must be valid JSON")
	}
	if len(data) > maxWorkflowDossierBytes {
		return dossier, nil, fmt.Errorf("sqlite: workflow dossier exceeds %d bytes", maxWorkflowDossierBytes)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if dossier.Hash != "" && strings.ToLower(dossier.Hash) != hash {
		return dossier, nil, errors.New("sqlite: workflow dossier hash does not match data")
	}
	dossier.Hash = hash
	if dossier.PublicationState == "" {
		dossier.PublicationState = store.WorkflowPublicationPending
	}
	switch dossier.PublicationState {
	case store.WorkflowPublicationPending, store.WorkflowPublicationPublished,
		store.WorkflowPublicationSkipped, store.WorkflowPublicationFailed:
	default:
		return dossier, nil, fmt.Errorf("sqlite: invalid workflow publication state %q", dossier.PublicationState)
	}
	dossier.PublicationError = normalizePublicationError(dossier.PublicationState, dossier.PublicationError)
	return dossier, data, nil
}

// normalizePublicationError keeps a bounded reason for states that can carry
// one. A successful publication always clears the reason; pending keeps a
// retry reason so operators can see why the workflow is still unpublished.
func normalizePublicationError(state, reason string) string {
	if state == store.WorkflowPublicationPublished {
		return ""
	}
	return truncateRunes(strings.TrimSpace(reason), maxPublicationErrorRunes)
}

func truncateRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max])
}

func compressDossier(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("sqlite: compress workflow dossier: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("sqlite: compress workflow dossier: %w", err)
	}
	return buf.Bytes(), nil
}

func decompressDossier(compressed []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open workflow dossier: %w", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, maxWorkflowDossierBytes+1))
	if err != nil {
		return nil, fmt.Errorf("sqlite: read workflow dossier: %w", err)
	}
	if len(data) > maxWorkflowDossierBytes {
		return nil, fmt.Errorf("sqlite: workflow dossier exceeds %d bytes", maxWorkflowDossierBytes)
	}
	if !json.Valid(data) {
		return nil, errors.New("sqlite: stored workflow dossier is not valid JSON")
	}
	return data, nil
}

// HasPublishedSevereFinding reports whether incremental history is unsafe for
// one MR. A summary from a non-completed workflow, or from a completed workflow
// whose verdict is INCOMPLETE, is treated as unknown, so an empty summary
// cannot narrow the next review. Findings that were dropped (refuted) are
// excluded; published and marked findings count.
func (s *Store) HasPublishedSevereFinding(ctx context.Context, projectID, mrIID int64) (bool, error) {
	if projectID <= 0 || mrIID <= 0 {
		return false, errors.New("sqlite: project id and mr iid are required")
	}
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT CASE
			WHEN w.status <> 'completed' THEN 1
			WHEN w.verdict = 'INCOMPLETE' THEN 1
			WHEN EXISTS (
				SELECT 1 FROM findings f
				WHERE f.workflow_id = w.id
				  AND COALESCE(NULLIF(f.final_severity, ''), f.severity) IN ('P0', 'P1')
				  AND f.decision IN ('publish', 'mark', 'downgrade')
			) THEN 1
			ELSE 0
		END
		FROM publications p
		JOIN workflows w ON w.id = p.workflow_id
		WHERE p.project_id = ? AND p.mr_iid = ? AND p.kind = 'summary'
		ORDER BY p.updated_at DESC, p.id DESC LIMIT 1`, projectID, mrIID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlite: has published severe finding: %w", err)
	}
	return one != 0, nil
}

// SetWorkflowPublicationState updates the publish state and its bounded,
// caller-sanitized reason. A published row always clears the reason; pending,
// skipped, and failed rows keep the supplied reason.
func (s *Store) SetWorkflowPublicationState(ctx context.Context, workflowID, state, reason string) error {
	switch state {
	case store.WorkflowPublicationPending, store.WorkflowPublicationPublished,
		store.WorkflowPublicationSkipped, store.WorkflowPublicationFailed:
	default:
		return fmt.Errorf("sqlite: invalid workflow publication state %q", state)
	}
	reason = normalizePublicationError(state, reason)
	res, err := s.db.ExecContext(ctx, `UPDATE workflow_dossiers
		SET publication_state = ?, publication_error = ?, updated_at = ? WHERE workflow_id = ?`,
		state, reason, formatTime(s.now().UTC()), workflowID)
	if err != nil {
		return fmt.Errorf("sqlite: set workflow publication state: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sqlite: set workflow publication state: %w", err)
	} else if n != 1 {
		return fmt.Errorf("%w: workflow dossier %s", store.ErrNotFound, workflowID)
	}
	return nil
}

// SetWorkflowPublicationStateIfPending settles a still-pending publication.
// It never overwrites published, skipped, or failed state, so cancellation and
// publishing cannot race into one another.
func (s *Store) SetWorkflowPublicationStateIfPending(ctx context.Context, workflowID, reason string) error {
	reason = normalizePublicationError(store.WorkflowPublicationSkipped, reason)
	res, err := s.db.ExecContext(ctx, `UPDATE workflow_dossiers
		SET publication_state = ?, publication_error = ?, updated_at = ?
		WHERE workflow_id = ? AND publication_state = ?`,
		store.WorkflowPublicationSkipped, reason, formatTime(s.now().UTC()),
		workflowID, store.WorkflowPublicationPending)
	if err != nil {
		return fmt.Errorf("sqlite: settle pending publication: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: settle pending publication: %w", err)
	}
	if n == 1 {
		return nil
	}
	var found int
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM workflow_dossiers WHERE workflow_id = ?`, workflowID).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: workflow dossier %s", store.ErrNotFound, workflowID)
		}
		return fmt.Errorf("sqlite: check workflow dossier: %w", err)
	}
	return nil
}

// ListPendingPublications returns terminal workflows that still need their
// publish step. Completed rows publish the normal review; failed rows publish
// the failure summary; cancelled rows only need a local skip. Unfinished
// engine recovery stays in ListUnfinished.
func (s *Store) ListPendingPublications(ctx context.Context, limit int) ([]store.PendingPublication, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.workflow_id, d.job_id, w.project_id,
		w.mr_iid, w.head_sha, d.generation, w.status, d.updated_at
		FROM workflow_dossiers d JOIN workflows w ON w.id = d.workflow_id
		WHERE w.status IN (?, ?, ?) AND d.publication_state = ?
		ORDER BY (w.status != 'cancelled'), d.updated_at, d.workflow_id LIMIT ?`,
		string(dag.WorkflowCompleted), string(dag.WorkflowFailed), string(dag.WorkflowCancelled),
		store.WorkflowPublicationPending, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list pending publications: %w", err)
	}
	defer rows.Close()
	pending := make([]store.PendingPublication, 0)
	for rows.Next() {
		var (
			item       store.PendingPublication
			jobID      sql.NullString
			mrIID      sql.NullInt64
			headSHA    sql.NullString
			updatedRaw string
		)
		if err := rows.Scan(&item.WorkflowID, &jobID, &item.ProjectID, &mrIID,
			&headSHA, &item.Generation, &item.Status, &updatedRaw); err != nil {
			return nil, fmt.Errorf("sqlite: scan pending publication: %w", err)
		}
		item.JobID = jobID.String
		item.MRIID = mrIID.Int64
		item.HeadSHA = headSHA.String
		if item.UpdatedAt, err = parseTime(updatedRaw); err != nil {
			return nil, err
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate pending publications: %w", err)
	}
	return pending, nil
}
