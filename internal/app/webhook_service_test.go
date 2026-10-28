package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

type webhookFixture struct {
	t       *testing.T
	store   *sqlite.Store
	rec     *recordingDeliveryStore
	service *WebhookService
	project *store.Project
	secret  string
}

func newWebhookFixture(t *testing.T, settings string) *webhookFixture {
	t.Helper()
	st := openTestStore(t)
	cipher := newTestCipher(t)
	project := seedProject(t, st, cipher, 42, "whsec-test", settings, "default")
	rec := &recordingDeliveryStore{WebhookStore: st}
	service, err := NewWebhookService(WebhookServiceConfig{
		Store:          rec,
		Workspace:      workspace.NewManager(t.TempDir(), nil),
		Cipher:         cipher,
		Profiles:       testProfiles(),
		ReviewDefaults: testDefaults(),
	})
	if err != nil {
		t.Fatalf("NewWebhookService: %v", err)
	}
	return &webhookFixture{t: t, store: st, rec: rec, service: service, project: project, secret: "whsec-test"}
}

func (f *webhookFixture) send(event, eventUUID, token string, body []byte) (WebhookResult, error) {
	f.t.Helper()
	return f.service.Handle(context.Background(), WebhookRequest{
		Event: event,
		Token: token,
		UUID:  eventUUID,
		Body:  body,
	})
}

func (f *webhookFixture) request(event, token string, body []byte) (WebhookResult, error) {
	return f.send(event, uuid.NewString(), token, body)
}

func (f *webhookFixture) job(id string) *store.Job {
	f.t.Helper()
	job, err := f.store.GetJob(context.Background(), id)
	if err != nil {
		f.t.Fatalf("GetJob: %v", err)
	}
	return job
}

func (f *webhookFixture) records() []store.Delivery {
	f.rec.mu.Lock()
	defer f.rec.mu.Unlock()
	return append([]store.Delivery(nil), f.rec.records...)
}

func (f *webhookFixture) jobs() []*store.Job {
	f.rec.mu.Lock()
	defer f.rec.mu.Unlock()
	return append([]*store.Job(nil), f.rec.jobs...)
}

func TestWebhookProjectLookupAndAuthentication(t *testing.T) {
	body := mrBody(42, 7, nil)
	t.Run("unknown project", func(t *testing.T) {
		st := openTestStore(t)
		rec := &recordingDeliveryStore{WebhookStore: st, getErr: store.ErrNotFound}
		service, _ := NewWebhookService(WebhookServiceConfig{Store: rec, Workspace: workspace.NewManager(t.TempDir(), nil), Cipher: newTestCipher(t), Profiles: testProfiles(), ReviewDefaults: testDefaults()})
		result, err := service.Handle(context.Background(), WebhookRequest{
			Event: "Merge Request Hook", Token: "x", UUID: uuid.NewString(), Body: mrBody(999, 7, nil),
		})
		if err != nil || result.Ignored != "project_not_registered" || result.Accepted {
			t.Fatalf("result = %+v, err = %v", result, err)
		}
	})
	t.Run("disabled project", func(t *testing.T) {
		st := openTestStore(t)
		disabled := seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
		disabled.Enabled = false
		rec := &recordingDeliveryStore{WebhookStore: st, project: disabled}
		service, _ := NewWebhookService(WebhookServiceConfig{Store: rec, Workspace: workspace.NewManager(t.TempDir(), nil), Cipher: newTestCipher(t), Profiles: testProfiles(), ReviewDefaults: testDefaults()})
		result, _ := service.Handle(context.Background(), WebhookRequest{
			Event: "Merge Request Hook", Token: "whsec", UUID: uuid.NewString(), Body: mrBody(42, 7, nil),
		})
		if result.Ignored != "project_not_registered" {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("wrong secret", func(t *testing.T) {
		f := newWebhookFixture(t, "{}")
		result, err := f.request("Merge Request Hook", "wrong", body)
		if err != nil || !result.Unauthorized {
			t.Fatalf("result = %+v, err = %v", result, err)
		}
		if len(f.records()) != 0 {
			t.Fatal("rejected delivery was recorded")
		}
	})
	t.Run("missing secret", func(t *testing.T) {
		f := newWebhookFixture(t, "{}")
		result, _ := f.request("Merge Request Hook", "", body)
		if !result.Unauthorized {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("corrupt stored secret", func(t *testing.T) {
		st := openTestStore(t)
		project := seedProject(t, st, newTestCipher(t), 42, "whsec", "{}", "default")
		project.WebhookSecretEnc = []byte("corrupt")
		rec := &recordingDeliveryStore{WebhookStore: st, project: project}
		service, _ := NewWebhookService(WebhookServiceConfig{Store: rec, Workspace: workspace.NewManager(t.TempDir(), nil), Cipher: newTestCipher(t), Profiles: testProfiles(), ReviewDefaults: testDefaults()})
		result, _ := service.Handle(context.Background(), WebhookRequest{
			Event: "Merge Request Hook", Token: "whsec", UUID: uuid.NewString(), Body: body,
		})
		if !result.Unauthorized {
			t.Fatalf("result = %+v", result)
		}
	})
}

func TestWebhookEnvelopeValidation(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		uuid    string
		body    []byte
		token   string
		ignored string
		records int
	}{
		{
			name:    "missing uuid",
			event:   "Merge Request Hook",
			body:    mrBody(42, 7, nil),
			token:   "whsec-test",
			ignored: "missing_event_uuid",
		},
		{
			name:    "invalid uuid",
			event:   "Merge Request Hook",
			uuid:    "not-a-uuid",
			body:    mrBody(42, 7, nil),
			token:   "whsec-test",
			ignored: "invalid_event_uuid",
		},
		{
			name:    "malformed json",
			event:   "Merge Request Hook",
			uuid:    "00000000-0000-7000-8000-000000000001",
			body:    []byte(`{`),
			token:   "whsec-test",
			ignored: "bad_request",
		},
		{
			name:    "event mismatch",
			event:   "Push Hook",
			uuid:    "00000000-0000-7000-8000-000000000002",
			body:    mrBody(42, 7, nil),
			token:   "whsec-test",
			ignored: "event_mismatch",
			records: 1,
		},
		{
			name:    "unsupported event",
			event:   "Issue Hook",
			uuid:    "00000000-0000-7000-8000-000000000003",
			body:    mrBody(42, 7, nil),
			token:   "whsec-test",
			ignored: "unsupported_event",
			records: 1,
		},
		{
			name:    "missing object kind",
			event:   "Merge Request Hook",
			uuid:    "00000000-0000-7000-8000-000000000004",
			body:    []byte(`{"project":{"id":42}}`),
			token:   "whsec-test",
			ignored: "event_mismatch",
			records: 1,
		},
		{
			name:    "project root mismatch",
			event:   "Merge Request Hook",
			uuid:    "00000000-0000-7000-8000-000000000005",
			body:    []byte(`{"object_kind":"merge_request","project_id":43,"project":{"id":42}}`),
			token:   "whsec-test",
			ignored: "project_mismatch",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newWebhookFixture(t, "{}")
			result, err := f.send(tt.event, tt.uuid, tt.token, tt.body)
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if result.Ignored != tt.ignored {
				t.Fatalf("ignored = %q, want %q", result.Ignored, tt.ignored)
			}
			if len(f.records()) != tt.records {
				t.Errorf("records = %d, want %d", len(f.records()), tt.records)
			}
		})
	}
}

func TestWebhookMergeRequestDecisions(t *testing.T) {
	tests := []struct {
		name     string
		settings string
		mutate   func(map[string]any)
		accepted bool
		ignored  string
		kind     string
		key      string
		trigger  string
		full     bool
		wantHead string
		profile  string
	}{
		{name: "open", accepted: true, kind: "review.candidate", key: "review.candidate:42:7", trigger: "mr_open", full: true, wantHead: sha40, profile: "default"},
		{name: "immediate open", settings: `{"allow_repo_config":false}`, accepted: true, kind: "review.run", key: "review:42:7", trigger: "mr_open", full: true, wantHead: sha40, profile: "default"},
		{name: "reopen", mutate: func(b map[string]any) { patchAttr(b, "action", "reopen") }, accepted: true, kind: "review.candidate", key: "review.candidate:42:7", trigger: "mr_open", full: true},
		{name: "update", mutate: func(b map[string]any) { patchAttr(b, "action", "update"); patchAttr(b, "oldrev", sha40) }, accepted: true, kind: "review.candidate", key: "review.candidate:42:7", trigger: "mr_update", wantHead: sha40},
		{name: "update without oldrev", mutate: func(b map[string]any) { patchAttr(b, "action", "update") }, ignored: "missing_oldrev"},
		{name: "close", mutate: func(b map[string]any) { patchAttr(b, "action", "close") }, accepted: true, kind: "review.cancel", key: "review.cancel:42:7"},
		{name: "merge", mutate: func(b map[string]any) { patchAttr(b, "action", "merge") }, accepted: true, kind: "review.cancel", key: "review.cancel:42:7"},
		{name: "draft flag", settings: `{"allow_repo_config":false}`, mutate: func(b map[string]any) { patchAttr(b, "draft", true) }, ignored: "draft"},
		{name: "draft title", settings: `{"allow_repo_config":false}`, mutate: func(b map[string]any) { patchAttr(b, "title", "Draft: something") }, ignored: "draft"},
		{name: "wip title", settings: `{"allow_repo_config":false}`, mutate: func(b map[string]any) { patchAttr(b, "title", "WIP: something") }, ignored: "draft"},
		{name: "label excluded", settings: `{"labels_exclude":["no-ai"]}`, mutate: func(b map[string]any) {
			patchAttr(b, "labels", []string{"no-ai"})
		}, ignored: "labels_filtered"},
		{name: "label include miss", settings: `{"labels_include":["review"]}`, ignored: "labels_filtered"},
		{name: "label include hit", settings: `{"labels_include":["review"]}`, mutate: func(b map[string]any) {
			patchAttr(b, "labels", []string{"review"})
		}, accepted: true, kind: "review.candidate", key: "review.candidate:42:7", full: true},
		{name: "branch filtered", mutate: func(b map[string]any) {
			patchAttr(b, "target_branch", "feature")
			patchMR(b, "target_branch", "feature")
		}, ignored: "branch_filtered"},
		{name: "custom branch", settings: `{"target_branches":["release/*"]}`, mutate: func(b map[string]any) {
			patchAttr(b, "target_branch", "release/1.0")
			patchMR(b, "target_branch", "release/1.0")
		}, accepted: true, kind: "review.candidate", key: "review.candidate:42:7", full: true},
		{name: "target project mismatch", mutate: func(b map[string]any) {
			patchAttr(b, "target_project_id", 99)
		}, ignored: "project_mismatch"},
		{name: "missing head sha", mutate: func(b map[string]any) {
			patchAttr(b, "last_commit", map[string]any{"id": ""})
			patchMR(b, "last_commit", map[string]any{"id": ""})
		}, ignored: "missing_head_sha"},
		{name: "unknown action", mutate: func(b map[string]any) { patchAttr(b, "action", "approved") }, ignored: "unsupported_action"},
		{name: "closed state on open action", mutate: func(b map[string]any) {
			patchAttr(b, "state", "closed")
		}, ignored: "mr_closed"},
		{name: "missing iid", mutate: func(b map[string]any) {
			patchAttr(b, "iid", 0)
			patchMR(b, "iid", 0)
		}, ignored: "bad_request"},
		{name: "label profile", settings: `{"profile_by_label":{"deep-review":"deep"}}`, mutate: func(b map[string]any) {
			patchAttr(b, "labels", []string{"deep-review"})
		}, accepted: true, kind: "review.candidate", key: "review.candidate:42:7", profile: "deep", full: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newWebhookFixture(t, tt.settings)
			body := mrBody(42, 7, tt.mutate)
			result, err := f.request("Merge Request Hook", "whsec-test", body)
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if result.Accepted != tt.accepted {
				t.Fatalf("result = %+v, want accepted=%v", result, tt.accepted)
			}
			if tt.ignored != "" && result.Ignored != tt.ignored {
				t.Fatalf("ignored = %q, want %q", result.Ignored, tt.ignored)
			}
			if !tt.accepted {
				return
			}
			job := f.job(result.JobID)
			if job.Kind != tt.kind || job.Key != tt.key {
				t.Fatalf("job = %+v, want kind=%s key=%s", job, tt.kind, tt.key)
			}
			if tt.kind == "review.run" || tt.kind == "review.candidate" {
				var payload map[string]any
				if err := json.Unmarshal(job.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if tt.trigger != "" && payload["trigger"] != tt.trigger {
					t.Errorf("trigger = %v", payload["trigger"])
				}
				if tt.full != payload["full"] {
					t.Errorf("full = %v, want %v", payload["full"], tt.full)
				}
				if tt.wantHead != "" && payload["head_sha"] != tt.wantHead {
					t.Errorf("head_sha = %v", payload["head_sha"])
				}
				if tt.profile != "" && payload["profile"] != tt.profile {
					t.Errorf("profile = %v, want %s", payload["profile"], tt.profile)
				}
			}
		})
	}
}

func TestWebhookDraftSkipDisabled(t *testing.T) {
	f := newWebhookFixture(t, `{"skip_draft":false}`)
	body := mrBody(42, 7, func(b map[string]any) { patchAttr(b, "draft", true) })
	result, err := f.request("Merge Request Hook", "whsec-test", body)
	if err != nil || !result.Accepted {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestWebhookLatestQueuedPayloadWins(t *testing.T) {
	f := newWebhookFixture(t, "{}")
	firstSHA := strings.Repeat("a", 40)
	secondSHA := strings.Repeat("b", 40)
	first := mrBody(42, 7, func(b map[string]any) {
		patchAttr(b, "last_commit", map[string]any{"id": firstSHA})
	})
	firstResult, err := f.request("Merge Request Hook", "whsec-test", first)
	if err != nil || !firstResult.Accepted {
		t.Fatalf("first: %+v, %v", firstResult, err)
	}
	second := mrBody(42, 7, func(b map[string]any) {
		patchAttr(b, "action", "update")
		patchAttr(b, "oldrev", firstSHA)
		patchAttr(b, "last_commit", map[string]any{"id": secondSHA})
	})
	secondResult, err := f.request("Merge Request Hook", "whsec-test", second)
	if err != nil || !secondResult.Accepted {
		t.Fatalf("second: %+v, %v", secondResult, err)
	}
	if secondResult.JobID != firstResult.JobID {
		t.Fatalf("latest payload did not replace queued job: %s != %s", secondResult.JobID, firstResult.JobID)
	}
	job := f.job(firstResult.JobID)
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["head_sha"] != secondSHA || payload["trigger"] != "mr_update" {
		t.Fatalf("payload = %s", job.Payload)
	}
	if len(f.records()) != 2 {
		t.Fatalf("records = %d", len(f.records()))
	}
}

func TestWebhookDuplicateConcurrent(t *testing.T) {
	f := newWebhookFixture(t, "{}")
	eventUUID := uuid.NewString()
	body := mrBody(42, 7, nil)
	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan WebhookResult, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := f.send("Merge Request Hook", eventUUID, "whsec-test", body)
			results <- result
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	accepted, duplicates := 0, 0
	var jobID string
	for result := range results {
		if result.Accepted {
			accepted++
			jobID = result.JobID
		}
		if result.Ignored == "duplicate" {
			duplicates++
		}
	}
	if accepted != 1 || duplicates != workers-1 {
		t.Fatalf("accepted=%d duplicates=%d", accepted, duplicates)
	}
	if _, err := f.store.GetJob(context.Background(), jobID); err != nil {
		t.Fatalf("accepted job missing: %v", err)
	}
}

func TestWebhookNoteCommands(t *testing.T) {
	tests := []struct {
		name     string
		note     string
		mutate   func(map[string]any)
		accepted bool
		ignored  string
		command  string
		profile  string
		explicit *bool
		full     bool
	}{
		{name: "review", note: "/review", accepted: true, command: "run", profile: "default", explicit: boolPtr(false)},
		{name: "review full", note: "/review full", accepted: true, command: "run", profile: "default", explicit: boolPtr(false), full: true},
		{name: "review profile", note: "/review deep", accepted: true, command: "run", profile: "deep", explicit: boolPtr(true)},
		{name: "cancel", note: "/review cancel", accepted: true, command: "cancel"},
		{name: "help", note: "/review help", accepted: true, command: "help"},
		{name: "unknown args", note: "/review full extra", ignored: "unknown_command"},
		{name: "unknown profile", note: "/review nope", ignored: "unknown_profile"},
		{name: "not a command", note: "/reviewevil", ignored: "not_review_command"},
		{name: "bot", note: "/review", mutate: func(b map[string]any) {
			b["user"].(map[string]any)["bot"] = true
		}, ignored: "bot_note"},
		{name: "closed mr", note: "/review", mutate: func(b map[string]any) {
			b["merge_request"].(map[string]any)["state"] = "closed"
		}, ignored: "mr_closed"},
		{name: "non mr noteable", note: "/review", mutate: func(b map[string]any) {
			b["object_attributes"].(map[string]any)["noteable_type"] = "Issue"
		}, ignored: "unsupported_noteable"},
		{name: "edited note", note: "/review", mutate: func(b map[string]any) {
			b["object_attributes"].(map[string]any)["action"] = "update"
		}, ignored: "unsupported_action"},
		{name: "target mismatch", note: "/review", mutate: func(b map[string]any) {
			b["merge_request"].(map[string]any)["target_project_id"] = 99
		}, ignored: "project_mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newWebhookFixture(t, "{}")
			body := noteBody(42, 7, 555, 12, "alice", tt.note)
			if tt.mutate != nil {
				var generic map[string]any
				if err := json.Unmarshal(body, &generic); err != nil {
					t.Fatal(err)
				}
				tt.mutate(generic)
				body, _ = json.Marshal(generic)
			}
			result, err := f.request("Note Hook", "whsec-test", body)
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if result.Accepted != tt.accepted {
				t.Fatalf("result = %+v", result)
			}
			if tt.ignored != "" && result.Ignored != tt.ignored {
				t.Fatalf("ignored = %q, want %q", result.Ignored, tt.ignored)
			}
			if !tt.accepted {
				return
			}
			job := f.job(result.JobID)
			if job.Kind != "review.command" || job.Key != "review.command:42:555" {
				t.Fatalf("job = %+v", job)
			}
			var payload map[string]any
			if err := json.Unmarshal(job.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["command"] != tt.command {
				t.Errorf("command = %v", payload["command"])
			}
			if tt.profile != "" && payload["profile"] != tt.profile {
				t.Errorf("profile = %v", payload["profile"])
			}
			if payload["full"] != tt.full {
				t.Errorf("full = %v, want %v", payload["full"], tt.full)
			}
			if tt.explicit != nil && payload["profile_explicit"] != *tt.explicit {
				t.Errorf("profile_explicit = %v, want %v", payload["profile_explicit"], *tt.explicit)
			}
			if payload["author_id"] != float64(12) || payload["note_id"] != float64(555) {
				t.Errorf("payload = %v", payload)
			}
		})
	}
}

func TestWebhookHelpEnqueuesWithoutReply(t *testing.T) {
	f := newWebhookFixture(t, "{}")
	result, err := f.request("Note Hook", "whsec-test", noteBody(42, 7, 555, 12, "alice", "/review help"))
	if err != nil || !result.Accepted {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	job := f.job(result.JobID)
	if job.Kind != "review.command" || job.Key != "review.command:42:555" {
		t.Fatalf("job = %+v", job)
	}
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["command"] != "help" {
		t.Fatalf("command = %v", payload["command"])
	}
	if len(f.jobs()) != 1 {
		t.Fatalf("webhook enqueued %d jobs, want only review.command", len(f.jobs()))
	}
}

func TestWebhookIgnoresCommandReplyNote(t *testing.T) {
	f := newWebhookFixture(t, "{}")
	body := noteBody(42, 7, 9701, 9, "review-bot", commandReplyBody(555, "help", "should not loop"))
	result, err := f.request("Note Hook", "whsec-test", body)
	if err != nil {
		t.Fatal(err)
	}
	if result.Ignored != "command_reply" {
		t.Fatalf("ignored = %q", result.Ignored)
	}
	if len(f.jobs()) != 0 {
		t.Fatalf("reply note enqueued %d jobs", len(f.jobs()))
	}
}

func boolPtr(value bool) *bool { return &value }

func TestWebhookConfiguredBotUsername(t *testing.T) {
	f := newWebhookFixture(t, "{}")
	service, err := NewWebhookService(WebhookServiceConfig{
		Store:          f.rec,
		Workspace:      f.service.workspace,
		Cipher:         newTestCipher(t),
		Profiles:       testProfiles(),
		ReviewDefaults: testDefaults(),
		BotUsernames:   []string{"ci-bot"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Handle(context.Background(), WebhookRequest{
		Event: "Note Hook", Token: "whsec-test", UUID: uuid.NewString(),
		Body: noteBody(42, 7, 556, 12, "ci-bot", "/review"),
	})
	if err != nil || result.Ignored != "bot_note" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestWebhookPushDecisions(t *testing.T) {
	tests := []struct {
		name     string
		settings string
		ref      string
		after    string
		accepted bool
		ignored  string
	}{
		{name: "default branch", ref: "refs/heads/main", after: sha40, accepted: true},
		{name: "other branch", ref: "refs/heads/feature", after: sha40, ignored: "branch_filtered"},
		{name: "deleted branch", ref: "refs/heads/main", after: strings.Repeat("0", 40), ignored: "branch_deleted"},
		{name: "tag", ref: "refs/tags/v1", after: sha40, ignored: "unsupported_ref"},
		{name: "invalid sha", ref: "refs/heads/main", after: "not-a-sha", ignored: "bad_request"},
		{name: "custom branch", settings: `{"target_branches":["release/*"]}`, ref: "refs/heads/release/1.0", after: sha40, accepted: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newWebhookFixture(t, tt.settings)
			result, err := f.request("Push Hook", "whsec-test", pushBody(42, tt.ref, tt.after))
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if result.Accepted != tt.accepted {
				t.Fatalf("result = %+v", result)
			}
			if tt.ignored != "" && result.Ignored != tt.ignored {
				t.Fatalf("ignored = %q, want %q", result.Ignored, tt.ignored)
			}
			if tt.accepted {
				job := f.job(result.JobID)
				if job.Kind != "workspace.refresh" || job.Key != "ws:42" {
					t.Fatalf("job = %+v", job)
				}
				var payload map[string]any
				if err := json.Unmarshal(job.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if payload["after_sha"] != strings.ToLower(tt.after) {
					t.Errorf("payload = %v", payload)
				}
			}
		})
	}
}

func TestWebhookStorageFailure(t *testing.T) {
	f := newWebhookFixture(t, "{}")
	f.rec.recErr = errors.New("disk full")
	result, err := f.request("Merge Request Hook", "whsec-test", mrBody(42, 7, nil))
	if err == nil {
		t.Fatal("expected storage error")
	}
	if result.Accepted || result.Ignored != "storage_error" {
		t.Fatalf("result = %+v", result)
	}
}

func TestWebhookStorageFailureAllowsResendWithSameUUID(t *testing.T) {
	f := newWebhookFixture(t, "{}")
	eventUUID := uuid.NewString()
	body := mrBody(42, 7, nil)
	f.rec.recErr = errors.New("disk full")
	if _, err := f.send("Merge Request Hook", eventUUID, "whsec-test", body); err == nil {
		t.Fatal("expected storage error")
	}
	f.rec.recErr = nil
	result, err := f.send("Merge Request Hook", eventUUID, "whsec-test", body)
	if err != nil || !result.Accepted || result.JobID == "" {
		t.Fatalf("resend result = %+v, err = %v", result, err)
	}
}

func TestWebhookStoredSettingsInvalid(t *testing.T) {
	f := newWebhookFixture(t, `{"unknown_field":true}`)
	result, err := f.request("Merge Request Hook", "whsec-test", mrBody(42, 7, nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || result.Ignored != "invalid_settings" {
		t.Fatalf("result = %+v", result)
	}
}

func TestWebhookRejectsTargetProjectFromOtherProject(t *testing.T) {
	f := newWebhookFixture(t, "{}")
	body := mrBody(42, 7, func(b map[string]any) {
		patchAttr(b, "target_project_id", 99)
	})
	result, err := f.request("Merge Request Hook", "whsec-test", body)
	if err != nil || result.Ignored != "project_mismatch" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestWebhookServiceRequiresDependencies(t *testing.T) {
	if _, err := NewWebhookService(WebhookServiceConfig{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestWebhookInvalidCandidateDoesNotFallBackToGenericDelivery(t *testing.T) {
	f := newWebhookFixture(t, "{}")
	_, err := f.service.recordJob(context.Background(), 42, "Merge Request Hook", uuid.NewString(), &store.Job{
		Kind:        "review.candidate",
		Payload:     json.RawMessage(`{"project_id":0}`),
		MaxAttempts: 3,
	})
	if err == nil {
		t.Fatal("expected invalid candidate error")
	}
	if got := len(f.records()); got != 0 {
		t.Fatalf("invalid candidate wrote %d deliveries", got)
	}
}
