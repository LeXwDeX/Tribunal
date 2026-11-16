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
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

const (
	// reviewStatusMarkerPrefix identifies the single review note maintained by
	// this service. It uses the summary marker so the status worker and the
	// publisher both find and edit the same MR note (#451).
	reviewStatusMarkerPrefix = review.SummaryMarkerPrefix
	reviewStatusMarkerLine   = reviewStatusMarkerPrefix + " v2 -->"
	// reviewLegacyStatusMarkerPrefix is the pre-#452 status note marker. Rows
	// upgraded from an older database keep updating that note instead of
	// taking over the published summary (#453) until the merged note carries
	// the final body and cleanup removes it (#475).
	reviewLegacyStatusMarkerPrefix = "<!-- gitlab-ce-review:status"
	reviewLegacyStatusMarkerLine   = reviewLegacyStatusMarkerPrefix + " v1 -->"
	reviewStatusDetailRunes        = 160

	// reviewStatusTableHeader and reviewStatusTableDivider render the single
	// status note as a deterministic Markdown table (#475). parseStatusPhase
	// matches the header exactly, so keep the two in sync.
	reviewStatusTableHeader  = "| 阶段 | 状态 | 代次 | 提交 | 预计 | 详情 |"
	reviewStatusTableDivider = "| --- | --- | --- | --- | --- | --- |"

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
	DeleteMRNote(ctx context.Context, token string, projectID, iid, noteID int64) error
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
	// FinalBody is the rendered review summary. When set, the status note
	// carries it below the lifecycle lines, so the MR has one review note.
	FinalBody string
	// LegacyMarker keeps a pre-upgrade row on the old status-marker note. Its
	// published summary lives in a separate note that this writer must not
	// find or edit.
	LegacyMarker bool
	// CleanupLegacy removes the bot's legacy status note once the merged note
	// carries the final body, so the MR keeps exactly one review note (#475).
	CleanupLegacy bool
}

// statusReporter writes the durable status and reaction goals to GitLab. It
// returns upstream errors so a job can retry; callers that must not fail a
// review log the error instead.
type statusReporter struct {
	gitlab   statusGitLab
	logger   *slog.Logger
	outbound func(context.Context, func(context.Context) error) error
	durable  bool
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

func (r *statusReporter) send(ctx context.Context, call func(context.Context) error) error {
	if r.outbound == nil {
		return call(ctx)
	}
	return r.outbound(ctx, call)
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
	marker := reviewStatusMarkerPrefix
	if update.LegacyMarker {
		marker = reviewLegacyStatusMarkerPrefix
	}
	notes, err := r.gitlab.ListMRNotes(ctx, token, projectID, mrIID)
	if err != nil {
		return 0, "", fmt.Errorf("app: status note lookup: %w", err)
	}
	noteID := int64(0)
	noteHash := hash
	for _, note := range notes {
		if !strings.HasPrefix(note.Body, marker) {
			continue
		}
		if note.AuthorID != user.ID {
			continue
		}
		noteID = note.ID
		if note.Body == body {
			break
		}
		// A queued acknowledgement may race a worker that already started the
		// review. Never move a visible status backwards.
		if !r.durable && !statusMayRegress(note.Body, update.State) {
			noteHash = sha256Hex(note.Body)
			break
		}
		err := r.send(ctx, func(ctx context.Context) error {
			return r.gitlab.UpdateMRNote(ctx, token, projectID, mrIID, note.ID, body)
		})
		if err == nil {
			break
		}
		if errors.Is(err, workspace.ErrOutboundInvalidated) {
			return 0, "", err
		}
		if !gitlab.IsNotFound(err) {
			return 0, "", fmt.Errorf("app: status note update: %w", err)
		}
		// The note was deleted between list and update. Recreate it below.
		noteID = 0
		break
	}
	if noteID == 0 {
		var created *gitlab.Note
		err = r.send(ctx, func(ctx context.Context) error {
			var err error
			created, err = r.gitlab.CreateMRNote(ctx, token, projectID, mrIID, body)
			return err
		})
		if err != nil {
			return 0, "", fmt.Errorf("app: status note creation: %w", err)
		}
		if created == nil || created.ID <= 0 {
			return 0, "", errors.New("app: status note creation returned no note")
		}
		noteID = created.ID
	}
	// Cleanup runs on every converged write, including the identical-body and
	// refused-regression early paths, so a legacy note cannot survive a restart
	// after the merged note is already current (#475).
	if update.CleanupLegacy {
		if err := r.deleteLegacyStatusNotes(ctx, projectID, mrIID, token, notes, user.ID); err != nil {
			return 0, "", err
		}
	}
	return noteID, noteHash, nil
}

// deleteLegacyStatusNotes removes the bot's own pre-#452 status notes. Foreign
// notes and the merged note never match both the author and marker filters.
func (r *statusReporter) deleteLegacyStatusNotes(ctx context.Context, projectID, mrIID int64, token string, notes []gitlab.Note, authorID int64) error {
	for _, note := range notes {
		if note.AuthorID != authorID || !strings.HasPrefix(note.Body, reviewLegacyStatusMarkerPrefix) {
			continue
		}
		noteID := note.ID
		err := r.send(ctx, func(ctx context.Context) error {
			return r.gitlab.DeleteMRNote(ctx, token, projectID, mrIID, noteID)
		})
		if err == nil || gitlab.IsNotFound(err) {
			continue
		}
		if errors.Is(err, workspace.ErrOutboundInvalidated) {
			return err
		}
		return fmt.Errorf("app: legacy status note delete: %w", err)
	}
	return nil
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
		if err := r.send(ctx, func(ctx context.Context) error {
			return r.gitlab.DeleteNoteAward(ctx, token, projectID, mrIID, noteID, award.ID)
		}); err != nil {
			if errors.Is(err, workspace.ErrOutboundInvalidated) {
				return err
			}
			if gitlab.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("app: reaction removal: %w", err)
		}
	}
	if desired == reviewAwardNone || haveDesired {
		return nil
	}
	if err := r.send(ctx, func(ctx context.Context) error {
		_, err := r.gitlab.AddNoteAward(ctx, token, projectID, mrIID, noteID, desired)
		return err
	}); err != nil {
		if errors.Is(err, workspace.ErrOutboundInvalidated) {
			return err
		}
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
	case "进行中", "已完成", "失败":
		return false
	// Bodies written before the phase split stored the lifecycle state.
	case reviewStatusRunning, reviewStatusPublishing,
		reviewStatusPass, reviewStatusBlocking, reviewStatusIncomplete,
		reviewStatusFailed, reviewStatusCancelled, reviewStatusStale:
		return false
	}
	return true
}

func parseStatusPhase(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != reviewStatusTableHeader {
			continue
		}
		for _, row := range lines[i+1:] {
			row = strings.TrimSpace(row)
			if row == "" || !strings.HasPrefix(row, "|") {
				break
			}
			fields := strings.Split(row, "|")
			if len(fields) < 3 {
				continue
			}
			cell := strings.TrimSpace(fields[1])
			if cell == "" || strings.Trim(cell, "-") == "" {
				continue // divider row
			}
			return cell
		}
		break
	}
	// Bodies written before #475 stored the phase as a bullet. Keep parsing
	// them so the monotonic queued acknowledgement check still works across
	// the upgrade.
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
// hidden marker so repeated reports of the same state are no-ops. The lifecycle
// fields are a Markdown table (#475).
func reviewStatusBody(update reviewStatusUpdate) string {
	var b strings.Builder
	if update.LegacyMarker {
		b.WriteString(reviewLegacyStatusMarkerLine)
	} else {
		b.WriteString(reviewStatusMarkerLine)
	}
	b.WriteByte('\n')
	b.WriteString("**AI 评审：")
	b.WriteString(reviewStatusLabel(update.State))
	b.WriteString("**\n\n")
	b.WriteString(reviewStatusTableHeader)
	b.WriteByte('\n')
	b.WriteString(reviewStatusTableDivider)
	b.WriteByte('\n')
	b.WriteString("| ")
	b.WriteString(reviewStatusPhase(update.State))
	b.WriteString(" | `")
	b.WriteString(sanitizeStatusDetail(update.State))
	b.WriteString("` | ")
	b.WriteString(statusGenerationCell(update.Generation))
	b.WriteString(" | ")
	b.WriteString(statusHeadCell(update.HeadSHA))
	b.WriteString(" | ")
	b.WriteString(statusEstimateCell(update.EstimateMinutes))
	b.WriteString(" | ")
	b.WriteString(statusDetailCell(update.Detail))
	b.WriteString(" |\n")
	if body := strings.TrimSpace(update.FinalBody); body != "" && !update.LegacyMarker {
		b.WriteString("\n")
		b.WriteString(stripSummaryMarker(body))
		b.WriteString("\n")
	}
	return gitlab.NeutralizeQuickActions(b.String())
}

func statusGenerationCell(generation int64) string {
	if generation <= 0 {
		return "-"
	}
	return "`" + strconv.FormatInt(generation, 10) + "`"
}

func statusHeadCell(head string) string {
	head = strings.TrimSpace(head)
	if head == "" {
		return "-"
	}
	return "`" + shortSHA(head) + "`"
}

func statusEstimateCell(minutes int) string {
	if minutes <= 0 {
		return "-"
	}
	return "约 " + strconv.Itoa(minutes) + " 分钟"
}

func statusDetailCell(detail string) string {
	detail = sanitizeStatusDetail(detail)
	if detail == "" {
		return "-"
	}
	// A raw pipe would split the table cell.
	return strings.ReplaceAll(detail, "|", `\|`)
}

// reviewStatusPhase maps a lifecycle state to the four visible phases.
func reviewStatusPhase(state string) string {
	switch state {
	case reviewStatusReceived, reviewStatusQueued:
		return "开始"
	case reviewStatusRunning, reviewStatusPublishing:
		return "进行中"
	case reviewStatusPass, reviewStatusBlocking, reviewStatusIncomplete:
		return "已完成"
	case reviewStatusFailed, reviewStatusCancelled, reviewStatusStale:
		return "失败"
	default:
		return sanitizeStatusDetail(state)
	}
}

// stripSummaryMarker removes the comment's summary marker line, if present.
// The status note keeps only its own first-line marker for recovery. A body
// without a newline after the marker keeps the text after the prefix.
func stripSummaryMarker(body string) string {
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, review.SummaryMarkerPrefix) {
		return body
	}
	if idx := strings.IndexByte(body, '\n'); idx >= 0 {
		return strings.TrimSpace(body[idx+1:])
	}
	return strings.TrimSpace(body[len(review.SummaryMarkerPrefix):])
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
