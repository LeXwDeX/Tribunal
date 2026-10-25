package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func defaultCommandPayload(command string) commandPayload {
	return commandPayload{
		ProjectID:      42,
		MRIID:          7,
		NoteID:         555,
		AuthorID:       12,
		AuthorUsername: "alice",
		Command:        command,
		Profile:        "default",
		Trigger:        "command",
	}
}

type recordingCommandStore struct {
	CommandStore
	mu  sync.Mutex
	ids []string
	dup []bool
}

func (s *recordingCommandStore) EnqueueReviewLatest(ctx context.Context, command store.ReviewCommand) (store.ReviewEnqueueResult, error) {
	result, err := s.CommandStore.EnqueueReviewLatest(ctx, command)
	if err != nil {
		return result, err
	}
	s.mu.Lock()
	s.ids = append(s.ids, result.JobID)
	s.dup = append(s.dup, result.Duplicate)
	s.mu.Unlock()
	return result, nil
}

func TestCommandWorkerReplacesQueuedLatestPayload(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	cipher := newTestCipher(t)
	seedProject(t, db, cipher, 42, "whsec", "{}", "default")
	oldSHA := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)
	latestSHA := strings.Repeat("d", 40)
	oldPayload := mustJSON(map[string]any{
		"project_id": 42,
		"mr_iid":     7,
		"head_sha":   oldSHA,
		"profile":    "default",
		"trigger":    "command",
		"full":       false,
	})
	oldID, _, err := db.Enqueue(ctx, store.Job{
		Kind:        "review.run",
		Key:         reviewRunKey(42, 7),
		Payload:     oldPayload,
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("seed queue: %v", err)
	}
	rec := &recordingCommandStore{CommandStore: db}
	api := defaultFakeGitLab()
	worker, err := NewCommandWorker(CommandWorkerConfig{
		Store: rec, GitLab: api, Cipher: cipher, Profiles: testProfiles(),
	})
	if err != nil {
		t.Fatal(err)
	}

	full := defaultCommandPayload("run")
	full.Full = true
	full.NoteID = 1001
	api.mr.SHA = newSHA
	if err := worker.Handle(ctx, commandJob(t, full)); err != nil {
		t.Fatalf("full command: %v", err)
	}
	deep := defaultCommandPayload("run")
	deep.Full = false
	deep.Profile = "deep"
	deep.NoteID = 1002
	api.mr.SHA = latestSHA
	if err := worker.Handle(ctx, commandJob(t, deep)); err != nil {
		t.Fatalf("deep command: %v", err)
	}

	rec.mu.Lock()
	if len(rec.ids) != 2 || rec.ids[0] != oldID || rec.ids[1] != oldID {
		t.Fatalf("EnqueueLatest ids = %v, want [%s %s]", rec.ids, oldID, oldID)
	}
	if rec.dup[0] || rec.dup[1] {
		t.Fatalf("EnqueueLatest duplicates = %v", rec.dup)
	}
	rec.mu.Unlock()

	claimed, err := db.Claim(ctx, "runner", []string{"review.run"}, time.Hour)
	if err != nil || claimed == nil {
		t.Fatalf("Claim = %+v, %v", claimed, err)
	}
	if claimed.ID != oldID {
		t.Fatalf("claimed id = %s, want %s", claimed.ID, oldID)
	}
	var payload map[string]any
	if err := json.Unmarshal(claimed.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["head_sha"] != latestSHA || payload["profile"] != "deep" || payload["full"] != false {
		t.Fatalf("latest payload was not applied: %s", claimed.Payload)
	}
	if err := db.Complete(ctx, *claimed); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	extra, err := db.Claim(ctx, "runner", []string{"review.run"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if extra != nil {
		t.Fatalf("unexpected extra queued job: %+v", extra)
	}
}

func TestCommandWorkerPreservesRunningAndQueuesSuccessor(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	cipher := newTestCipher(t)
	seedProject(t, db, cipher, 42, "whsec", "{}", "default")
	oldPayload := mustJSON(map[string]any{
		"project_id": 42, "mr_iid": 7, "head_sha": strings.Repeat("a", 40),
		"profile": "default", "trigger": "command", "full": false,
	})
	oldID, _, err := db.Enqueue(ctx, store.Job{
		Kind: "review.run", Key: reviewRunKey(42, 7), Payload: oldPayload, MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	running, err := db.Claim(ctx, "runner", []string{"review.run"}, time.Hour)
	if err != nil || running == nil {
		t.Fatalf("Claim = %+v, %v", running, err)
	}
	if running.ID != oldID {
		t.Fatalf("running id = %s, want %s", running.ID, oldID)
	}
	rec := &recordingCommandStore{CommandStore: db}
	api := defaultFakeGitLab()
	api.mr.SHA = strings.Repeat("c", 40)
	worker, err := NewCommandWorker(CommandWorkerConfig{
		Store: rec, GitLab: api, Cipher: cipher, Profiles: testProfiles(),
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := defaultCommandPayload("run")
	payload.Full = true
	payload.NoteID = 2001
	if err := worker.Handle(ctx, commandJob(t, payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	rec.mu.Lock()
	if len(rec.ids) != 1 || rec.ids[0] == running.ID {
		t.Fatalf("successor ids = %v, running = %s", rec.ids, running.ID)
	}
	successorID := rec.ids[0]
	rec.mu.Unlock()

	stillRunning, err := db.GetJob(ctx, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillRunning.State != store.JobRunning || string(stillRunning.Payload) != string(oldPayload) {
		t.Fatalf("running job changed: state %s payload %s", stillRunning.State, stillRunning.Payload)
	}
	successor, err := db.GetJob(ctx, successorID)
	if err != nil {
		t.Fatal(err)
	}
	if successor.State != store.JobQueued {
		t.Fatalf("successor state = %s", successor.State)
	}
	var latest map[string]any
	if err := json.Unmarshal(successor.Payload, &latest); err != nil {
		t.Fatal(err)
	}
	if latest["head_sha"] != strings.Repeat("c", 40) || latest["full"] != true {
		t.Fatalf("successor payload = %s", successor.Payload)
	}
	if err := db.Complete(ctx, *running); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	next, err := db.Claim(ctx, "runner", []string{"review.run"}, time.Hour)
	if err != nil || next == nil {
		t.Fatalf("successor claim = %+v, %v", next, err)
	}
	if next.ID != successorID {
		t.Fatalf("claimed %s, want %s", next.ID, successorID)
	}
}

func TestCommandWorkerRejectsInvalidHeadSHAForRun(t *testing.T) {
	for _, sha := range []string{"", "not-a-sha", strings.Repeat("0", 40), strings.Repeat("z", 40)} {
		t.Run(fmt.Sprintf("%q", sha), func(t *testing.T) {
			db := openFakeStore(t, newTestCipher(t))
			api := defaultFakeGitLab()
			api.mr.SHA = sha
			worker := newCommandTestWorker(t, api, db, nil)
			err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("run")))
			if err == nil {
				t.Fatal("invalid head SHA was accepted")
			}
			if strings.Contains(err.Error(), "glpat") || strings.Contains(err.Error(), "https://") {
				t.Fatalf("error leaked details: %v", err)
			}
			if len(db.enqueued()) != 0 {
				t.Fatal("invalid head SHA was enqueued")
			}
		})
	}
}

func TestCommandWorkerCancelDoesNotRequireHeadSHA(t *testing.T) {
	db := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	api.mr.SHA = ""
	worker := newCommandTestWorker(t, api, db, nil)
	if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("cancel"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	jobs := db.enqueued()
	if len(jobs) != 1 || jobs[0].Kind != "review.cancel" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestCommandWorkerAllowsDeveloper(t *testing.T) {
	cipher := newTestCipher(t)
	fakeStore := openFakeStore(t, cipher)
	api := defaultFakeGitLab()
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	payload := defaultCommandPayload("run")
	payload.Full = true
	if err := worker.Handle(context.Background(), commandJob(t, payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	jobs := fakeStore.enqueued()
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	job := jobs[0]
	if job.Kind != "review.run" || job.Key != "review:42:7" {
		t.Fatalf("job = %+v", job)
	}
	var body map[string]any
	if err := json.Unmarshal(job.Payload, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"project_id": float64(42),
		"mr_iid":     float64(7),
		"head_sha":   sha40,
		"profile":    "default",
		"trigger":    "command",
		"full":       true,
	}
	for key, value := range want {
		if body[key] != value {
			t.Errorf("payload[%s] = %v, want %v", key, body[key], value)
		}
	}
	if api.callCount("GetMemberAccess") != 1 {
		t.Errorf("member access calls = %d", api.callCount("GetMemberAccess"))
	}
}

func TestCommandWorkerDeniesReporter(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	api.access = 20
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("run"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fakeStore.enqueued()) != 0 {
		t.Fatal("reporter was allowed to enqueue a review")
	}
	notes := api.noteSnapshot()
	if len(notes) != 1 {
		t.Fatalf("permission replies = %d, want 1", len(notes))
	}
	if !strings.Contains(notes[0].Body, "Developer") {
		t.Fatalf("permission reply = %q", notes[0].Body)
	}
	if !strings.HasPrefix(notes[0].Body, commandReplyPrefix(555)) {
		t.Fatalf("permission reply marker missing: %q", notes[0].Body)
	}
}

func TestCommandWorkerDeniedHelpGetsPermissionReply(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	api.access = 20
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload(commandHelp))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fakeStore.enqueued()) != 0 {
		t.Fatal("denied help enqueued a review")
	}
	notes := api.noteSnapshot()
	if len(notes) != 1 || !strings.Contains(notes[0].Body, "Developer") {
		t.Fatalf("permission reply = %+v", notes)
	}
	if strings.Contains(notes[0].Body, "可用命令") {
		t.Fatal("denied help received the command help text")
	}
}

func TestCommandWorkerHelpRepliesWithoutEnqueue(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	payload := defaultCommandPayload(commandHelp)
	if err := worker.Handle(context.Background(), commandJob(t, payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fakeStore.enqueued()) != 0 {
		t.Fatal("help enqueued a review job")
	}
	notes := api.noteSnapshot()
	if len(notes) != 1 {
		t.Fatalf("help replies = %d, want 1", len(notes))
	}
	body := notes[0].Body
	if !strings.HasPrefix(body, commandReplyPrefix(payload.NoteID)) {
		t.Fatalf("help marker missing: %q", body)
	}
	for _, command := range []string{"/review full", "/review cancel", "/review help"} {
		if !strings.Contains(body, command) {
			t.Errorf("help reply is missing %q: %q", command, body)
		}
	}
	if strings.Index(body, "`deep`") > strings.Index(body, "`default`") {
		t.Fatalf("profiles are not sorted: %q", body)
	}
	if strings.Contains(body, "alice") || strings.Contains(body, "run arbitrary") {
		t.Fatalf("help reply echoed note content: %q", body)
	}
	if api.callCount("ListMRNotes") != 1 || api.callCount("CreateMRNote") != 1 {
		t.Fatalf("reply calls = list %d create %d", api.callCount("ListMRNotes"), api.callCount("CreateMRNote"))
	}
}

func TestCommandWorkerReplyRetryDoesNotDuplicate(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	api.access = 20
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	job := commandJob(t, defaultCommandPayload("run"))
	for i := 0; i < 2; i++ {
		if err := worker.Handle(context.Background(), job); err != nil {
			t.Fatalf("Handle %d: %v", i, err)
		}
	}
	if notes := api.noteSnapshot(); len(notes) != 1 {
		t.Fatalf("retry created %d replies", len(notes))
	}
	if api.noteCreates != 1 || api.noteUpdates != 0 {
		t.Fatalf("reply writes = create %d update %d", api.noteCreates, api.noteUpdates)
	}
	if len(fakeStore.enqueued()) != 0 {
		t.Fatal("denied command enqueued a review")
	}
}

func TestCommandWorkerReplyUpdatesStaleBody(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	api.access = 20
	api.notes = []gitlab.Note{{ID: 7, AuthorID: api.user.ID, Body: commandReplyBody(555, "help", "stale")}}
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("run"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	notes := api.noteSnapshot()
	if len(notes) != 1 || api.noteUpdates != 1 || api.noteCreates != 0 {
		t.Fatalf("reply writes = notes %d create %d update %d", len(notes), api.noteCreates, api.noteUpdates)
	}
	if !strings.Contains(notes[0].Body, "Developer") {
		t.Fatalf("stale reply was not updated: %q", notes[0].Body)
	}
}

func TestCommandWorkerReplyFailuresRetry(t *testing.T) {
	t.Run("help note list", func(t *testing.T) {
		fakeStore := openFakeStore(t, newTestCipher(t))
		api := defaultFakeGitLab()
		api.listNotesErr = errors.New("transport glpat-seeded-token https://gitlab.example.com")
		worker := newCommandTestWorker(t, api, fakeStore, nil)
		err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload(commandHelp)))
		if err == nil {
			t.Fatal("expected retryable error")
		}
		for _, forbidden := range []string{"glpat", "https://"} {
			if strings.Contains(err.Error(), forbidden) {
				t.Fatalf("error leaked %q: %v", forbidden, err)
			}
		}
		if len(fakeStore.enqueued()) != 0 || len(api.noteSnapshot()) != 0 {
			t.Fatal("failed help reply mutated state")
		}
	})
	t.Run("help note create", func(t *testing.T) {
		fakeStore := openFakeStore(t, newTestCipher(t))
		api := defaultFakeGitLab()
		api.createNoteErr = errors.New("create failed")
		worker := newCommandTestWorker(t, api, fakeStore, nil)
		if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload(commandHelp))); err == nil {
			t.Fatal("expected retryable error")
		}
		if len(fakeStore.enqueued()) != 0 {
			t.Fatal("failed help reply enqueued a review")
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		fakeStore := openFakeStore(t, newTestCipher(t))
		api := defaultFakeGitLab()
		worker := newCommandTestWorker(t, api, fakeStore, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := worker.Handle(ctx, commandJob(t, defaultCommandPayload(commandHelp))); !errors.Is(err, context.Canceled) {
			t.Fatalf("Handle = %v, want context.Canceled", err)
		}
		if api.callCount("CurrentUser") != 0 || len(api.noteSnapshot()) != 0 {
			t.Fatal("canceled command reached GitLab")
		}
	})
}

func TestCommandWorkerIgnoresSelfBot(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	api.user = &gitlab.User{ID: 12, Username: "alice", Bot: true}
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("run"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fakeStore.enqueued()) != 0 {
		t.Fatal("self bot note was enqueued")
	}
	if api.callCount("GetMemberAccess") != 0 || api.callCount("GetMergeRequest") != 0 {
		t.Fatal("self bot note was not rejected before MR/member calls")
	}
	if len(api.noteSnapshot()) != 0 {
		t.Fatal("self bot note received a reply")
	}
}

func TestCommandWorkerIgnoresOwnUsername(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	api.user = &gitlab.User{ID: 99, Username: "alice"}
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("run"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fakeStore.enqueued()) != 0 {
		t.Fatal("note from the token's user was enqueued")
	}
}

func TestCommandWorkerIgnoresConfiguredBot(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	worker := newCommandTestWorker(t, api, fakeStore, []string{"ci-bot"})
	payload := defaultCommandPayload("run")
	payload.AuthorUsername = "ci-bot"
	if err := worker.Handle(context.Background(), commandJob(t, payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fakeStore.enqueued()) != 0 {
		t.Fatal("configured bot note was enqueued")
	}
	if api.callCount("CurrentUser") != 0 {
		t.Fatal("configured bot was not rejected before network lookup")
	}
}

func TestCommandWorkerIgnoresAuthorBotFlag(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	payload := defaultCommandPayload("run")
	payload.AuthorBot = true
	if err := worker.Handle(context.Background(), commandJob(t, payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fakeStore.enqueued()) != 0 {
		t.Fatal("author_bot note was enqueued")
	}
}

func TestCommandWorkerIgnoresClosedMR(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	api.mr.State = "merged"
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("run"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fakeStore.enqueued()) != 0 {
		t.Fatal("closed MR command was enqueued")
	}
	if api.callCount("GetMemberAccess") != 0 {
		t.Fatal("closed MR reached member validation")
	}
	if len(api.noteSnapshot()) != 0 {
		t.Fatal("closed MR received a reply")
	}
}

func TestCommandWorkerIgnoresMismatchedProject(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	api.mr.TargetProjectID = 99
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("run"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fakeStore.enqueued()) != 0 || len(api.noteSnapshot()) != 0 {
		t.Fatal("mismatched project received a review or reply")
	}
	if api.callCount("GetMemberAccess") != 0 {
		t.Fatal("mismatched project reached member validation")
	}
}

func TestCommandWorkerIgnoresMissingMR(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	api.mrErr = &gitlab.Error{Op: "get_mr", Status: 404}
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("run"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fakeStore.enqueued()) != 0 || len(api.noteSnapshot()) != 0 {
		t.Fatal("missing MR received a review or reply")
	}
}

func TestCommandWorkerCancel(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("cancel"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	jobs := fakeStore.enqueued()
	if len(jobs) != 1 || jobs[0].Kind != "review.cancel" || jobs[0].Key != "review.cancel:42:7" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestCommandWorkerMemberLookupOutcomes(t *testing.T) {
	t.Run("not found means no permission", func(t *testing.T) {
		fakeStore := openFakeStore(t, newTestCipher(t))
		api := defaultFakeGitLab()
		api.accessErr = &gitlab.Error{Op: "get_member", Status: 404}
		worker := newCommandTestWorker(t, api, fakeStore, nil)
		if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("run"))); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if len(fakeStore.enqueued()) != 0 {
			t.Fatal("unknown member was allowed")
		}
		if notes := api.noteSnapshot(); len(notes) != 1 || !strings.Contains(notes[0].Body, "Developer") {
			t.Fatalf("permission reply = %+v", notes)
		}
	})
	t.Run("upstream error retries", func(t *testing.T) {
		fakeStore := openFakeStore(t, newTestCipher(t))
		api := defaultFakeGitLab()
		api.accessErr = &gitlab.Error{Op: "get_member", Status: 500}
		worker := newCommandTestWorker(t, api, fakeStore, nil)
		if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("run"))); err == nil {
			t.Fatal("expected retryable error")
		}
		if len(fakeStore.enqueued()) != 0 {
			t.Fatal("job enqueued without access verification")
		}
		if len(api.noteSnapshot()) != 0 {
			t.Fatal("upstream failure was reported as a definitive denial")
		}
	})
}

func TestCommandWorkerMalformedPayloadIsIgnored(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	for _, job := range []*store.Job{
		nil,
		{Kind: "other", Payload: json.RawMessage(`{}`)},
		{Kind: "review.command", Payload: json.RawMessage(`{`)},
		{Kind: "review.command", Payload: json.RawMessage(`{"project_id":0}`)},
		{Kind: "review.command", Payload: json.RawMessage(`{"project_id":42,"mr_iid":7,"note_id":1,"author_id":2,"command":"nope"}`)},
	} {
		if err := worker.Handle(context.Background(), job); err != nil {
			t.Fatalf("Handle(%+v): %v", job, err)
		}
	}
	if len(fakeStore.enqueued()) != 0 {
		t.Fatal("malformed command enqueued a job")
	}
	if api.callCount("CurrentUser") != 0 {
		t.Fatal("malformed command reached GitLab")
	}
}

func TestCommandWorkerDisabledProject(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	fakeStore.project.Enabled = false
	api := defaultFakeGitLab()
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("run"))); err != nil {
		t.Fatal(err)
	}
	if len(fakeStore.enqueued()) != 0 || api.callCount("CurrentUser") != 0 {
		t.Fatal("disabled project command was processed")
	}
}

func TestCommandWorkerUnknownProfileFallsBack(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	fakeStore.project.Profile = "deep"
	api := defaultFakeGitLab()
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	payload := defaultCommandPayload("run")
	payload.Profile = "missing"
	if err := worker.Handle(context.Background(), commandJob(t, payload)); err != nil {
		t.Fatal(err)
	}
	jobs := fakeStore.enqueued()
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	var body map[string]any
	if err := json.Unmarshal(jobs[0].Payload, &body); err != nil {
		t.Fatal(err)
	}
	if body["profile"] != "deep" {
		t.Fatalf("profile = %v", body["profile"])
	}
}

func TestCommandWorkerEnqueueFailureRetries(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	fakeStore.enqueueErr = errors.New("queue unavailable")
	api := defaultFakeGitLab()
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("run"))); err == nil {
		t.Fatal("expected enqueue error")
	}
}

func TestCommandWorkerDuplicateEnqueueIsSuccess(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	fakeStore.duplicate = true
	api := defaultFakeGitLab()
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("run"))); err != nil {
		t.Fatal(err)
	}
}

func TestCommandWorkerErrorsDoNotLeakToken(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	api.mrErr = errors.New("transport includes glpat-seeded-token and https://gitlab.example.com")
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("run")))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, forbidden := range []string{"glpat-seeded-token", "https://"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("error leaked %q: %v", forbidden, err)
		}
	}
}

func TestLifecycle(t *testing.T) {
	lifecycle := &Lifecycle{}
	if lifecycle.Ready() {
		t.Fatal("lifecycle was ready before start")
	}
	lifecycle.MarkStarted()
	if !lifecycle.Ready() {
		t.Fatal("lifecycle is not ready after start")
	}
	lifecycle.MarkStopped()
	if lifecycle.Ready() {
		t.Fatal("lifecycle is ready after stop")
	}
}

func TestCommandWorkerRequiresDependencies(t *testing.T) {
	if _, err := NewCommandWorker(CommandWorkerConfig{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestHelpWebhookWorkerEndToEnd(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	cipher := newTestCipher(t)
	seedProject(t, db, cipher, 42, "whsec-test", "{}", "default")
	api := defaultFakeGitLab()
	webhook, err := NewWebhookService(WebhookServiceConfig{
		Store:          db,
		Cipher:         cipher,
		Profiles:       testProfiles(),
		ReviewDefaults: testDefaults(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := webhook.Handle(ctx, WebhookRequest{
		Event: "Note Hook",
		Token: "whsec-test",
		UUID:  uuid.NewString(),
		Body:  noteBody(42, 7, 777, 12, "alice", "/review help"),
	})
	if err != nil || !result.Accepted {
		t.Fatalf("webhook = %+v, err = %v", result, err)
	}
	if len(api.callList()) != 0 {
		t.Fatalf("webhook called GitLab: %v", api.callList())
	}
	command, err := db.GetJob(ctx, result.JobID)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewCommandWorker(CommandWorkerConfig{
		Store: db, GitLab: api, Cipher: cipher, Profiles: testProfiles(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Handle(ctx, &jobs.Job{ID: command.ID, Kind: command.Kind, Payload: command.Payload}); err != nil {
		t.Fatalf("worker: %v", err)
	}
	notes := api.noteSnapshot()
	if len(notes) != 1 || !strings.Contains(notes[0].Body, "/review full") {
		t.Fatalf("help replies = %+v", notes)
	}
	claimed, err := db.Claim(ctx, "help-e2e", []string{"review.run"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed != nil {
		t.Fatalf("help enqueued review.run: %+v", claimed)
	}
}

func TestCommandWorkerEnqueuesSourceNoteForStoreReceipt(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	job := commandJob(t, defaultCommandPayload("run"))
	for i := 0; i < 2; i++ {
		if err := worker.Handle(context.Background(), job); err != nil {
			t.Fatalf("Handle %d: %v", i, err)
		}
	}
	jobs := fakeStore.enqueued()
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want both retries to enqueue", len(jobs))
	}
	for _, enqueued := range jobs {
		var payload map[string]any
		if err := json.Unmarshal(enqueued.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["note_id"] != float64(555) {
			t.Fatalf("enqueued payload lost the source note: %v", payload)
		}
	}
	api.mu.Lock()
	adds := append([]string(nil), api.awardAdds...)
	api.mu.Unlock()
	if len(adds) != 0 {
		t.Fatalf("command worker wrote pre-enqueue reactions: %v", adds)
	}
}

func TestCommandWorkerCancelEnqueuesCancelNote(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	payload := defaultCommandPayload("cancel")
	payload.NoteID = 777
	if err := worker.Handle(context.Background(), commandJob(t, payload)); err != nil {
		t.Fatal(err)
	}
	jobs := fakeStore.enqueued()
	if len(jobs) != 1 || jobs[0].Kind != "review.cancel" {
		t.Fatalf("cancel jobs = %+v", jobs)
	}
	var enqueued map[string]any
	if err := json.Unmarshal(jobs[0].Payload, &enqueued); err != nil {
		t.Fatal(err)
	}
	if enqueued["note_id"] != float64(777) {
		t.Fatalf("cancel payload lost its note: %v", enqueued)
	}
	api.mu.Lock()
	adds := append([]string(nil), api.awardAdds...)
	api.mu.Unlock()
	if len(adds) != 0 {
		t.Fatalf("cancel worker wrote pre-enqueue reactions: %v", adds)
	}
}

func TestCommandWorkerDeniedDoesNotAddEyes(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	api.access = 20
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	job := commandJob(t, defaultCommandPayload("run"))
	for i := 0; i < 2; i++ {
		if err := worker.Handle(context.Background(), job); err != nil {
			t.Fatal(err)
		}
	}
	if len(api.awardAdds) != 0 {
		t.Fatalf("denied command added reactions: %v", api.awardAdds)
	}
}

func TestCommandWorkerEnqueueFailureDoesNotTouchReactions(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	fakeStore.enqueueErr = errors.New("queue unavailable")
	api := defaultFakeGitLab()
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload("run"))); err == nil {
		t.Fatal("enqueue failure was swallowed")
	}
	api.mu.Lock()
	adds := append([]string(nil), api.awardAdds...)
	api.mu.Unlock()
	if len(adds) != 0 {
		t.Fatalf("failed enqueue left reactions: %v", adds)
	}
	if jobs := fakeStore.enqueued(); len(jobs) != 0 {
		t.Fatalf("jobs = %d, want none", len(jobs))
	}
}

func TestCommandWorkerRecordsQueuedStatusAndReaction(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	cipher := newTestCipher(t)
	seedProject(t, db, cipher, 42, "whsec", "{}", "default")
	api := defaultFakeGitLab()
	worker, err := NewCommandWorker(CommandWorkerConfig{
		Store:    db,
		GitLab:   api,
		Cipher:   cipher,
		Profiles: testProfiles(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Handle(ctx, commandJob(t, defaultCommandPayload("run"))); err != nil {
		t.Fatal(err)
	}
	status, err := db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if status.State != reviewStatusQueued || status.Generation != 1 {
		t.Fatalf("queued status = %+v", status)
	}
	reaction, err := db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil || reaction.Desired != reviewAwardEyes {
		t.Fatalf("source-note reaction = %+v, %v", reaction, err)
	}
	if len(api.noteSnapshot()) != 0 {
		t.Fatalf("command worker wrote GitLab before the side-effect job: %+v", api.noteSnapshot())
	}
}
