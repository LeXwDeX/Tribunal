package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
)

const (
	// reviewStatusMarkerPrefix identifies the single status note maintained by
	// this service. It is separate from the summary, finding, and reply markers.
	reviewStatusMarkerPrefix = "<!-- gitlab-ce-review:status"
	reviewStatusMarkerLine   = reviewStatusMarkerPrefix + " v1 -->"
	reviewStatusDetailRunes  = 160

	reviewStatusReceived   = "received"
	reviewStatusQueued     = store.ReviewStatusQueued
	reviewStatusRunning    = store.ReviewStatusRunning
	reviewStatusPublishing = store.ReviewStatusPublishing
	reviewStatusPass       = store.ReviewStatusPass
	reviewStatusBlocking   = store.ReviewStatusBlocking
	reviewStatusIncomplete = store.ReviewStatusIncomplete
	reviewStatusFailed     = store.ReviewStatusFailed
	reviewStatusCancelled  = store.ReviewStatusCancelled
	reviewStatusStale      = store.ReviewStatusStale

	reviewAwardEyes = store.ReviewReactionEyes
	reviewAwardPass = store.ReviewReactionPass
	reviewAwardFail = store.ReviewReactionFail
	reviewAwardNone = store.ReviewReactionNone
)

// errReactionNoteGone marks a source note (and its awards) deleted upstream.
// The desired reaction is settled instead of retried forever.
var errReactionNoteGone = errors.New("app: review reaction note is gone")

var (
	statusTokenRE = regexp.MustCompile(`(?i)\b(?:glpat|gloas|glrt|gldt|glcbt|glptt|glft)-[A-Za-z0-9_-]+`)
	statusURLRE   = regexp.MustCompile(`(?i)https?://[^\s]+`)
)

// statusGitLab is the GitLab surface needed to keep one status note and its
// command-source reactions up to date.
type statusGitLab interface {
	CurrentUser(ctx context.Context, token string) (*gitlab.User, error)
	ListMRNotes(ctx context.Context, token string, projectID, iid int64) ([]gitlab.Note, error)
	CreateMRNote(ctx context.Context, token string, projectID, iid int64, body string) (*gitlab.Note, error)
	UpdateMRNote(ctx context.Context, token string, projectID, iid, noteID int64, body string) error
	ListNoteAwards(ctx context.Context, token string, projectID, iid, noteID int64) ([]gitlab.Award, error)
	AddNoteAward(ctx context.Context, token string, projectID, iid, noteID int64, name string) (*gitlab.Award, error)
	DeleteNoteAward(ctx context.Context, token string, projectID, iid, noteID, awardID int64) error
}

// reviewStatusUpdate is one desired status note. Generation and estimate are
// display-only; the store fences the write by generation and head.
type reviewStatusUpdate struct {
	State           string
	Detail          string
	HeadSHA         string
	Generation      int64
	EstimateMinutes int
}

// statusReporter writes the durable status and reaction goals to GitLab. It
// returns upstream errors so a job can retry; callers that must not fail a
// review log the error instead.
type statusReporter struct {
	gitlab statusGitLab
	logger *slog.Logger
}

func newStatusReporter(api statusGitLab, logger *slog.Logger) *statusReporter {
	if api == nil {
		return nil
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &statusReporter{gitlab: api, logger: logger}
}

// writeStatus creates or updates the single marker note and returns the note
// ID and body hash for the applied row.
func (r *statusReporter) writeStatus(ctx context.Context, projectID, mrIID int64, token string, update reviewStatusUpdate) (int64, string, error) {
	if r == nil || r.gitlab == nil {
		return 0, "", errors.New("app: status reporter is not configured")
	}
	if projectID <= 0 || mrIID <= 0 || token == "" {
		return 0, "", errors.New("app: review status ids and token are required")
	}
	user, err := r.gitlab.CurrentUser(ctx, token)
	if err != nil {
		return 0, "", fmt.Errorf("app: status identity lookup: %w", err)
	}
	if user == nil || user.ID <= 0 {
		return 0, "", errors.New("app: status identity is unavailable")
	}
	body := reviewStatusBody(update)
	hash := sha256Hex(body)
	notes, err := r.gitlab.ListMRNotes(ctx, token, projectID, mrIID)
	if err != nil {
		return 0, "", fmt.Errorf("app: status note lookup: %w", err)
	}
	for _, note := range notes {
		if !strings.HasPrefix(note.Body, reviewStatusMarkerPrefix) {
			continue
		}
		if note.AuthorID != user.ID {
			continue
		}
		if note.Body == body {
			return note.ID, hash, nil
		}
		// A queued acknowledgement may race a worker that already started the
		// review. Never move a visible status backwards.
		if !statusMayRegress(note.Body, update.State) {
			return note.ID, hash, nil
		}
		err := r.gitlab.UpdateMRNote(ctx, token, projectID, mrIID, note.ID, body)
		if err == nil {
			return note.ID, hash, nil
		}
		if !gitlab.IsNotFound(err) {
			return 0, "", fmt.Errorf("app: status note update: %w", err)
		}
		// The note was deleted between list and update. Recreate it below.
		break
	}
	created, err := r.gitlab.CreateMRNote(ctx, token, projectID, mrIID, body)
	if err != nil {
		return 0, "", fmt.Errorf("app: status note creation: %w", err)
	}
	if created == nil || created.ID <= 0 {
		return 0, "", errors.New("app: status note creation returned no note")
	}
	return created.ID, hash, nil
}

// applyReaction converges the bot's own awards on one note to desired.
func (r *statusReporter) applyReaction(ctx context.Context, projectID, mrIID int64, token string, noteID int64, desired string) error {
	if r == nil || r.gitlab == nil {
		return errors.New("app: status reporter is not configured")
	}
	if projectID <= 0 || mrIID <= 0 || noteID <= 0 || token == "" {
		return errors.New("app: review reaction ids and token are required")
	}
	user, err := r.gitlab.CurrentUser(ctx, token)
	if err != nil {
		return fmt.Errorf("app: reaction identity lookup: %w", err)
	}
	if user == nil || user.ID <= 0 {
		return errors.New("app: reaction identity is unavailable")
	}
	awards, err := r.gitlab.ListNoteAwards(ctx, token, projectID, mrIID, noteID)
	if err != nil {
		if gitlab.IsNotFound(err) {
			return fmt.Errorf("%w: %v", errReactionNoteGone, err)
		}
		return fmt.Errorf("app: reaction lookup: %w", err)
	}
	remove := reactionAwardsToRemove(desired)
	haveDesired := false
	for _, award := range awards {
		if award.UserID != user.ID {
			continue
		}
		if desired != reviewAwardNone && award.Name == desired {
			haveDesired = true
			continue
		}
		if !remove[award.Name] {
			continue
		}
		if err := r.gitlab.DeleteNoteAward(ctx, token, projectID, mrIID, noteID, award.ID); err != nil {
			if gitlab.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("app: reaction removal: %w", err)
		}
	}
	if desired == reviewAwardNone || haveDesired {
		return nil
	}
	if _, err := r.gitlab.AddNoteAward(ctx, token, projectID, mrIID, noteID, desired); err != nil {
		// A concurrent retry may have added it already.
		if gitlab.StatusOf(err) == 409 {
			return nil
		}
		return fmt.Errorf("app: reaction creation: %w", err)
	}
	return nil
}

func reactionAwardsToRemove(desired string) map[string]bool {
	remove := map[string]bool{reviewAwardEyes: true}
	switch desired {
	case reviewAwardEyes:
		return map[string]bool{reviewAwardPass: true, reviewAwardFail: true}
	case reviewAwardPass:
		return map[string]bool{reviewAwardEyes: true, reviewAwardFail: true}
	case reviewAwardFail:
		return map[string]bool{reviewAwardEyes: true, reviewAwardPass: true}
	case reviewAwardNone:
		return map[string]bool{reviewAwardEyes: true, reviewAwardPass: true, reviewAwardFail: true}
	}
	return remove
}

// statusMayRegress reports whether nextState may replace the previous phase.
// Only the pre-run acknowledgements are monotonic; later phases intentionally
// allow a retry to report running again.
func statusMayRegress(previousBody, nextState string) bool {
	if nextState != reviewStatusReceived && nextState != reviewStatusQueued {
		return true
	}
	switch parseStatusPhase(previousBody) {
	case reviewStatusRunning, reviewStatusPublishing,
		reviewStatusPass, reviewStatusBlocking, reviewStatusIncomplete,
		reviewStatusFailed, reviewStatusCancelled, reviewStatusStale:
		return false
	}
	return true
}

func parseStatusPhase(body string) string {
	const prefix = "- 阶段：`"
	idx := strings.Index(body, prefix)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(prefix):]
	end := strings.IndexByte(rest, '`')
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// reviewStatusBody renders a deterministic status note. The first line is the
// hidden marker so repeated reports of the same state are no-ops.
func reviewStatusBody(update reviewStatusUpdate) string {
	var b strings.Builder
	b.WriteString(reviewStatusMarkerLine)
	b.WriteByte('\n')
	b.WriteString("**AI 评审：")
	b.WriteString(reviewStatusLabel(update.State))
	b.WriteString("**\n\n")
	b.WriteString("- 阶段：`")
	b.WriteString(sanitizeStatusDetail(update.State))
	b.WriteString("`\n")
	if update.Generation > 0 {
		b.WriteString("- 代次：`")
		b.WriteString(strconv.FormatInt(update.Generation, 10))
		b.WriteString("`\n")
	}
	if head := strings.TrimSpace(update.HeadSHA); head != "" {
		b.WriteString("- 提交：`")
		b.WriteString(shortSHA(head))
		b.WriteString("`\n")
	}
	if update.EstimateMinutes > 0 {
		b.WriteString("- 预计：约 ")
		b.WriteString(strconv.Itoa(update.EstimateMinutes))
		b.WriteString(" 分钟\n")
	}
	if detail := sanitizeStatusDetail(update.Detail); detail != "" {
		b.WriteString("- 详情：")
		b.WriteString(detail)
		b.WriteString("\n")
	}
	return b.String()
}

func reviewStatusLabel(state string) string {
	switch state {
	case reviewStatusReceived:
		return "已接收评审请求"
	case reviewStatusQueued:
		return "评审已入队"
	case reviewStatusRunning:
		return "评审运行中"
	case reviewStatusPublishing:
		return "正在发布评审结果"
	case reviewStatusPass:
		return "评审通过（PASS）"
	case reviewStatusBlocking:
		return "发现问题（BLOCKING）"
	case reviewStatusIncomplete:
		return "评审未完成（INCOMPLETE）"
	case reviewStatusFailed:
		return "评审失败"
	case reviewStatusCancelled:
		return "评审已取消"
	case reviewStatusStale:
		return "评审已过期"
	default:
		return sanitizeStatusDetail(state)
	}
}

// sanitizeStatusDetail bounds and redacts untrusted status text. Only a fixed
// set of internal reasons is expected, so URLs and token-like strings are
// replaced rather than risk leaking upstream details or credentials.
func sanitizeStatusDetail(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	value = statusTokenRE.ReplaceAllString(value, "[redacted]")
	value = statusURLRE.ReplaceAllString(value, "[url]")
	return truncateRunes(value, reviewStatusDetailRunes)
}

func shortSHA(value string) string {
	if len(value) > 7 {
		return value[:7]
	}
	return value
}
