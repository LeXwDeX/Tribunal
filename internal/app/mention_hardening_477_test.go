package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// TestMentionDetectionRejectsOversizedNoteWithoutWork pins the ingress hard
// limit: any commenter can post a huge note without the webhook secret, so a
// note above the limit must short-circuit before any scan or allocation.
func TestMentionDetectionRejectsOversizedNoteWithoutWork(t *testing.T) {
	f := newMentionWebhookFixture(t, []string{"tribunal-bot"})
	body := strings.Repeat("\n", 5<<20) + "@tribunal-bot"
	if len(body) <= mentionNoteLimit {
		t.Fatalf("test body is not oversized: %d", len(body))
	}
	if f.service.mentionsBot(body) {
		t.Fatal("oversized note triggered a mention")
	}
	start := time.Now()
	if f.service.mentionsBot(body) {
		t.Fatal("oversized note triggered a mention")
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("oversized note scan took %s, want a bounded short-circuit", elapsed)
	}
	allocs := testing.AllocsPerRun(5, func() {
		f.service.mentionsBot(body)
	})
	if allocs > 1 {
		t.Fatalf("oversized note allocated %.1f objects per run", allocs)
	}
}

// TestMentionDetectionBoundsPathologicalBackticks reproduces the quadratic
// backtick-run cost with a body above the limit; the cap must win before the
// scanner runs.
func TestMentionDetectionBoundsPathologicalBackticks(t *testing.T) {
	f := newMentionWebhookFixture(t, []string{"tribunal-bot"})
	var body strings.Builder
	for n := 1; body.Len() < 4<<20; n++ {
		body.WriteString(strings.Repeat("`", n))
		body.WriteByte('x')
	}
	note := body.String()
	if len(note) <= mentionNoteLimit {
		t.Fatalf("test body is not oversized: %d", len(note))
	}
	start := time.Now()
	if f.service.mentionsBot(note) {
		t.Fatal("pathological note triggered a mention")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("oversized backtick note took %s, want a bounded short-circuit", elapsed)
	}
}

// TestMentionDetectionWithinLimitStillMatches keeps the cap from rejecting
// ordinary large notes.
func TestMentionDetectionWithinLimitStillMatches(t *testing.T) {
	f := newMentionWebhookFixture(t, []string{"tribunal-bot"})
	padding := strings.Repeat("a", mentionNoteLimit-len(" @tribunal-bot"))
	if !f.service.mentionsBot(padding + " @tribunal-bot") {
		t.Fatal("mention near the size limit was not detected")
	}
}

// TestMentionDetectionScansBacktickRunsWithinLimit is a regression guard for
// the single-pass pairing: increasing run lengths with no matching closer used
// to scan to the end of the note for every opener.
func TestMentionDetectionScansBacktickRunsWithinLimit(t *testing.T) {
	f := newMentionWebhookFixture(t, []string{"tribunal-bot"})
	var body strings.Builder
	for n := 1; body.Len() < 96<<10; n++ {
		body.WriteString(strings.Repeat("`", n))
		body.WriteByte('x')
	}
	note := body.String()
	if len(note) > mentionNoteLimit {
		t.Fatalf("test body exceeds the limit: %d", len(note))
	}
	start := time.Now()
	if f.service.mentionsBot(note) {
		t.Fatal("unmatched backtick runs triggered a mention")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("within-limit backtick scan took %s, want linear work", elapsed)
	}
}

// TestMentionUsernameBoundaryUsesGitLabCharset covers '.', '-', '_' as valid
// GitLab username characters. A trailing dot at a sentence end therefore
// belongs to the username potential and must not match.
func TestMentionUsernameBoundaryUsesGitLabCharset(t *testing.T) {
	f := newMentionWebhookFixture(t, []string{"tribunal"})
	for _, body := range []string{
		"@tribunal.helper",
		"@tribunal-helper",
		"@tribunal_helper",
		"@tribunal.",
		"@tribunalx",
		"x@tribunal",
	} {
		if f.service.mentionsBot(body) {
			t.Errorf("body %q matched a longer username", body)
		}
	}
	for _, body := range []string{
		"@tribunal",
		"@tribunal ",
		"@tribunal,",
		"@tribunal!",
		"(@tribunal)",
		"请 @tribunal 看一下",
	} {
		if !f.service.mentionsBot(body) {
			t.Errorf("body %q did not match the configured username", body)
		}
	}
}

// TestMentionDetectionHandlesIndentedCodeAndEscapes covers the CommonMark
// deviations: 4-space indented code, code-span removal must not splice tokens
// together, and backslash-escaped backticks are literal text.
func TestMentionDetectionHandlesIndentedCodeAndEscapes(t *testing.T) {
	f := newMentionWebhookFixture(t, []string{"tribunal"})
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "four space indented code", body: "    @tribunal", want: false},
		{name: "tab indented code", body: "\t@tribunal", want: false},
		{name: "indented code after text", body: "text\n    @tribunal", want: false},
		{name: "inline code splice", body: "`@trib`x`unal`", want: false},
		{name: "escaped backticks", body: "\\`@tribunal\\`", want: true},
		{name: "blockquote is not code", body: "> @tribunal", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := f.service.mentionsBot(tt.body); got != tt.want {
				t.Fatalf("mentionsBot(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

// TestWebhookMentionIgnoresFindingMarkerNote adds the finding marker to the
// service-note defense: review findings may quote user text containing @names.
func TestWebhookMentionIgnoresFindingMarkerNote(t *testing.T) {
	f := newMentionWebhookFixture(t, []string{"tribunal-bot"})
	body := noteBodyWithDiscussion(42, 7, 555, 12, "review-bot", false,
		"<!-- gitlab-ce-review:finding key=abc -->\n@tribunal-bot", "disc-9")
	result, err := f.request("Note Hook", "whsec-test", body)
	if err != nil || result.Ignored != "service_note" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if jobs := f.jobs(); len(jobs) != 0 {
		t.Fatalf("finding note enqueued %d jobs", len(jobs))
	}
}

// TestWebhookMentionTrimsConfiguredBotUsernames pins config hygiene: a padded
// entry must still wake the bot and still be filtered as a bot author.
func TestWebhookMentionTrimsConfiguredBotUsernames(t *testing.T) {
	f := newMentionWebhookFixture(t, []string{"  tribunal-bot  "})
	result, err := f.request("Note Hook", "whsec-test",
		noteBodyWithDiscussion(42, 7, 555, 12, "alice", false, "@tribunal-bot please", "disc-9"))
	if err != nil || !result.Accepted {
		t.Fatalf("padded bot_usernames entry did not match: %+v, %v", result, err)
	}
	bot, err := f.request("Note Hook", "whsec-test", noteBody(42, 7, 556, 12, "tribunal-bot", "/review"))
	if err != nil || bot.Ignored != "bot_note" {
		t.Fatalf("padded bot username was not filtered: %+v, %v", bot, err)
	}
}

// TestCommandWorkerIgnoresTrimmedConfiguredBotUsername checks the worker uses
// trimmed names for its own bot-author defense.
func TestCommandWorkerIgnoresTrimmedConfiguredBotUsername(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	api.user = &gitlab.User{ID: 9, Username: "service-bot"}
	worker := newCommandTestWorker(t, api, fakeStore, []string{"  review-bot  "})
	payload := defaultCommandPayload(commandRun)
	payload.AuthorID = 77
	payload.AuthorUsername = "review-bot"
	payload.AuthorBot = false
	if err := worker.Handle(context.Background(), commandJob(t, payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fakeStore.enqueued()) != 0 || len(api.noteSnapshot()) != 0 {
		t.Fatal("padded bot username was not filtered")
	}
}

// TestCommandWorkerMentionRetryPreservesReplyBody pins the retry path: a
// duplicate command identity reports generation zero, which must not rewrite
// the accepted reply and erase its 代次 line.
func TestCommandWorkerMentionRetryPreservesReplyBody(t *testing.T) {
	ctx := context.Background()
	fakeStore := openFakeStore(t, newTestCipher(t))
	fakeStore.generation = 3
	api := defaultFakeGitLab()
	api.discussions = []gitlab.Discussion{{
		ID:    "disc-1",
		Notes: []gitlab.Note{{ID: 555, Body: "@review-bot", AuthorID: 12}},
	}}
	worker := newCommandTestWorker(t, api, fakeStore, []string{"review-bot"})
	job := commandJob(t, mentionCommandPayload("disc-1"))
	if err := worker.Handle(ctx, job); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	first := commandReplyNote(t, api, 555).Body
	if !strings.Contains(first, "代次 3") {
		t.Fatalf("first reply has no generation: %q", first)
	}
	fakeStore.generation = 0
	fakeStore.duplicate = true
	if err := worker.Handle(ctx, job); err != nil {
		t.Fatalf("retry Handle: %v", err)
	}
	if api.noteUpdates != 0 || api.discussionNoteAdds != 1 {
		t.Fatalf("retry writes = update %d add %d", api.noteUpdates, api.discussionNoteAdds)
	}
	if got := commandReplyNote(t, api, 555).Body; got != first {
		t.Fatalf("retry rewrote the reply: %q -> %q", first, got)
	}
}

// TestCommandWorkerExplicitDiscussionMissingUsesMarkerFallback covers a thread
// absent from ListDiscussions: the marker scan must still find the existing
// reply instead of appending a duplicate.
func TestCommandWorkerExplicitDiscussionMissingUsesMarkerFallback(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	fakeStore.generation = 1
	api := defaultFakeGitLab()
	api.notes = []gitlab.Note{{ID: 556, Body: commandReplyBody(555, "accepted", "stale"), AuthorID: api.user.ID}}
	worker := newCommandTestWorker(t, api, fakeStore, []string{"review-bot"})
	if err := worker.Handle(context.Background(), commandJob(t, mentionCommandPayload("gone"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if api.discussionNoteAdds != 0 || api.noteUpdates != 1 {
		t.Fatalf("reply writes = add %d update %d", api.discussionNoteAdds, api.noteUpdates)
	}
}

// TestCommandWorkerExplicitDiscussionGoneFallsBackToTopLevel covers a deleted
// thread: the 404 is distinguished from a transport failure and answered at
// the top level instead of retrying until quarantine.
func TestCommandWorkerExplicitDiscussionGoneFallsBackToTopLevel(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	fakeStore.generation = 1
	api := defaultFakeGitLab()
	api.addDiscussionErr = &gitlab.Error{Op: "add_discussion_note", Status: 404}
	worker := newCommandTestWorker(t, api, fakeStore, []string{"review-bot"})
	if err := worker.Handle(context.Background(), commandJob(t, mentionCommandPayload("gone"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if api.noteCreates != 1 || api.discussionNoteAdds != 0 {
		t.Fatalf("fallback writes = create %d add %d", api.noteCreates, api.discussionNoteAdds)
	}
	if reply := commandReplyNote(t, api, 555); !strings.Contains(reply.Body, "kind=accepted") {
		t.Fatalf("fallback reply = %q", reply.Body)
	}
}

// TestCommandWorkerThreadReplyNon404FailureRetries keeps transport failures
// retryable instead of silently moving the reply to the top level.
func TestCommandWorkerThreadReplyNon404FailureRetries(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	fakeStore.generation = 1
	api := defaultFakeGitLab()
	api.addDiscussionErr = errors.New("transport failed")
	worker := newCommandTestWorker(t, api, fakeStore, []string{"review-bot"})
	if err := worker.Handle(context.Background(), commandJob(t, mentionCommandPayload("disc-1"))); err == nil {
		t.Fatal("transport failure settled the job")
	}
	if api.noteCreates != 0 {
		t.Fatalf("transport failure appended %d top-level notes", api.noteCreates)
	}
}

// TestCommandWorkerMentionAppliesProjectFilters makes mention wake-ups obey
// the same draft/label/branch filters as automatic triggers; a filtered
// mention gets a thread reply and no review.
func TestCommandWorkerMentionAppliesProjectFilters(t *testing.T) {
	tests := []struct {
		name     string
		defaults config.ProjectSettings
		setup    func(*fakeGitLab)
		want     string
	}{
		{
			name:     "draft",
			defaults: config.ProjectSettings{SkipDraft: true},
			setup:    func(api *fakeGitLab) { api.mr.Draft = true },
			want:     "草稿",
		},
		{
			name:     "labels",
			defaults: config.ProjectSettings{LabelsInclude: []string{"review"}},
			setup:    func(api *fakeGitLab) { api.mr.Labels = []string{"other"} },
			want:     "标签",
		},
		{
			name:     "target branch",
			defaults: config.ProjectSettings{TargetBranches: []string{"main"}},
			setup:    func(api *fakeGitLab) { api.mr.TargetBranch = "release" },
			want:     "分支",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeStore := openFakeStore(t, newTestCipher(t))
			api := defaultFakeGitLab()
			tt.setup(api)
			api.discussions = []gitlab.Discussion{{
				ID:    "disc-1",
				Notes: []gitlab.Note{{ID: 555, Body: "@review-bot", AuthorID: 12}},
			}}
			worker := newCommandTestWorker(t, api, fakeStore, []string{"review-bot"})
			worker.reviewDefaults = tt.defaults
			if err := worker.Handle(context.Background(), commandJob(t, mentionCommandPayload("disc-1"))); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(fakeStore.enqueued()) != 0 {
				t.Fatal("filtered mention enqueued a review")
			}
			reply := commandReplyNote(t, api, 555)
			if !strings.Contains(reply.Body, "kind=filtered") || !strings.Contains(reply.Body, tt.want) {
				t.Fatalf("filtered reply = %q, want %q", reply.Body, tt.want)
			}
		})
	}
}

// TestCommandWorkerSlashCommandBypassesMentionFilters keeps `/review` an
// explicit override of the automatic filters.
func TestCommandWorkerSlashCommandBypassesMentionFilters(t *testing.T) {
	fakeStore := openFakeStore(t, newTestCipher(t))
	api := defaultFakeGitLab()
	api.mr.Draft = true
	worker := newCommandTestWorker(t, api, fakeStore, nil)
	worker.reviewDefaults = config.ProjectSettings{SkipDraft: true}
	if err := worker.Handle(context.Background(), commandJob(t, defaultCommandPayload(commandRun))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fakeStore.enqueued()) != 1 {
		t.Fatal("slash command was filtered like an automatic trigger")
	}
}

// TestReviewFilterReasonAppliesToMentionTrigger pins the shared filter switch.
func TestReviewFilterReasonAppliesToMentionTrigger(t *testing.T) {
	mr := &gitlab.MergeRequest{Draft: true, TargetBranch: "main"}
	project := &store.Project{DefaultBranch: "main"}
	settings := config.ProjectSettings{SkipDraft: true}
	if got := reviewFilterReason("mention", mr, project, settings); got != "draft" {
		t.Fatalf("reason = %q, want draft", got)
	}
}
