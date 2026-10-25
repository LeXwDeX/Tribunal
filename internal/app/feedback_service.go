package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/secret"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/workspace"
)

const (
	defaultFeedbackInterval = 6 * time.Hour
	feedbackWindow          = 14 * 24 * time.Hour
	feedbackPageLimit       = 100
	feedbackMaxPages        = 5
	mergedFeedbackWindow    = 3 * 24 * time.Hour
)

// feedbackDiff is the minimal line-level git capability F8 needs. It stays
// separate from GitOps so the shared interface does not grow for one caller.
type feedbackDiff interface {
	FileDiff(ctx context.Context, dir, from, to, path string) (*repo.FileDiff, error)
}

// FeedbackService implements F8: periodic award and resolve collection.
type FeedbackService struct {
	store     ReviewStore
	gitlab    GitLabClient
	cipher    *secret.Cipher
	git       GitOps
	diff      feedbackDiff
	workspace *workspace.Manager
	logger    *slog.Logger
	now       func() time.Time
}

// FeedbackServiceConfig configures FeedbackService.
type FeedbackServiceConfig struct {
	Store     ReviewStore
	GitLab    GitLabClient
	Cipher    *secret.Cipher
	Git       GitOps
	Workspace *workspace.Manager
	Logger    *slog.Logger
	Now       func() time.Time
}

// NewFeedbackService builds the F8 collector.
func NewFeedbackService(cfg FeedbackServiceConfig) (*FeedbackService, error) {
	if cfg.Store == nil || cfg.GitLab == nil || cfg.Cipher == nil || cfg.Workspace == nil {
		return nil, fmt.Errorf("app: feedback service dependencies are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	var diff feedbackDiff
	if cfg.Git != nil {
		diff, _ = cfg.Git.(feedbackDiff)
	}
	return &FeedbackService{
		store:     cfg.Store,
		gitlab:    cfg.GitLab,
		cipher:    cfg.Cipher,
		git:       cfg.Git,
		diff:      diff,
		workspace: cfg.Workspace,
		logger:    logger,
		now:       now,
	}, nil
}

// Start runs the collector immediately and then on every interval until ctx is
// cancelled. It returns when the context is cancelled.
func (s *FeedbackService) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultFeedbackInterval
	}
	s.RunOnce(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RunOnce(ctx)
		}
	}
}

// RunOnce performs one collection pass.
func (s *FeedbackService) RunOnce(ctx context.Context) {
	projects := make(map[int64]*store.Project)
	tokens := make(map[int64]string)
	seen := 0
	cutoff := s.now().UTC().Add(-feedbackWindow)
	cursor := ""
	for page := 0; page < feedbackMaxPages; page++ {
		workflows, next, err := s.store.ListWorkflows(ctx, store.WorkflowFilter{Kind: "review", Status: "completed", Since: cutoff}, cursor, feedbackPageLimit)
		if err != nil {
			s.logger.Warn("feedback workflow lookup failed")
			return
		}
		for _, workflow := range workflows {
			if err := ctx.Err(); err != nil {
				return
			}
			seen++
			s.collectWorkflow(ctx, workflow, projects, tokens)
		}
		if len(workflows) < feedbackPageLimit || next == "" {
			break
		}
		cursor = next
	}
	if seen > 0 {
		s.logger.Info("feedback collection finished", "workflows", seen)
	}
}

func (s *FeedbackService) collectWorkflow(ctx context.Context, workflow store.Workflow, projects map[int64]*store.Project, tokens map[int64]string) {
	if workflow.MRIID == nil {
		return
	}
	mrIID := *workflow.MRIID
	project, token, ok := s.project(ctx, workflow.ProjectID, projects, tokens)
	if !ok {
		return
	}
	mr, err := s.gitlab.GetMergeRequest(ctx, token, project.ID, mrIID)
	if err != nil {
		return
	}
	if mr == nil || !s.eligibleMR(mr) {
		return
	}
	publications, err := s.store.ListPublications(ctx, workflow.ID)
	if err != nil {
		return
	}
	for _, publication := range publications {
		switch publication.Kind {
		case summaryPublishKind:
			noteID, err := strconv.ParseInt(publication.GitlabID, 10, 64)
			if err != nil || noteID <= 0 {
				continue
			}
			s.collectAwards(ctx, token, publication, mrIID, noteID)
		case discussionPublishKind:
			s.collectDiscussion(ctx, token, project, publication, workflow, mr, mrIID)
		}
	}
}

// collectAwards records reactions on the summary note. A summary describes the
// whole review, so it stays workflow-level and is never linked to one finding.
func (s *FeedbackService) collectAwards(ctx context.Context, token string, publication store.Publication, mrIID, noteID int64) {
	awards, err := s.gitlab.ListNoteAwards(ctx, token, publication.ProjectID, mrIID, noteID)
	if err != nil {
		return
	}
	for _, award := range awards {
		signal := ""
		switch award.Name {
		case "thumbsup":
			signal = "thumbsup"
		case "thumbsdown":
			signal = "thumbsdown"
		default:
			continue
		}
		userID := award.UserID
		if _, err := s.store.UpsertFeedback(ctx, store.Feedback{
			ProjectID:     publication.ProjectID,
			MRIID:         mrIID,
			PublicationID: publication.ID,
			Signal:        signal,
			GitlabUserID:  &userID,
			ObservedAt:    s.now().UTC(),
		}); err != nil {
			s.logger.Warn("feedback upsert failed", "publication_id", publication.ID)
		}
	}
}

func (s *FeedbackService) collectDiscussion(ctx context.Context, token string, project *store.Project, publication store.Publication, workflow store.Workflow, mr *gitlab.MergeRequest, mrIID int64) {
	discussions, err := s.gitlab.ListDiscussions(ctx, token, publication.ProjectID, mrIID)
	if err != nil {
		return
	}
	var notes []gitlab.Note
	for _, discussion := range discussions {
		if discussion.ID != publication.GitlabID {
			continue
		}
		notes = discussion.Notes
		break
	}
	if len(notes) == 0 {
		return
	}
	// The publisher creates the thread with the finding note; later notes are
	// reviewer replies. Their reactions describe the discussion, not approval
	// of the published finding.
	publisherNote := notes[0]
	findingID := ""
	if finding := s.publicationFinding(ctx, workflow, publication); finding != nil {
		findingID = finding.ID
	}
	resolved := false
	for _, note := range notes {
		if note.Resolved {
			resolved = true
		}
	}
	s.collectNoteAwards(ctx, token, publication, publisherNote.ID, findingID)
	if !resolved {
		return
	}
	signal := s.resolveSignal(ctx, token, project, publication, workflow, mr, mrIID)
	if signal == "" {
		return
	}
	if _, err := s.store.UpsertFeedback(ctx, store.Feedback{
		ProjectID:     publication.ProjectID,
		MRIID:         mrIID,
		PublicationID: publication.ID,
		FindingID:     findingID,
		Signal:        signal,
		ObservedAt:    s.now().UTC(),
	}); err != nil {
		s.logger.Warn("feedback upsert failed", "publication_id", publication.ID)
	}
}

// publicationFinding resolves the exact persisted finding a discussion
// publication announced. The workflow, project and merge request must all
// agree, so a publish key reused by another review cannot steal a signal.
func (s *FeedbackService) publicationFinding(ctx context.Context, workflow store.Workflow, publication store.Publication) *store.Finding {
	if publication.PublishKey == "" || publication.WorkflowID != workflow.ID {
		return nil
	}
	if publication.ProjectID != workflow.ProjectID || workflow.MRIID == nil || publication.MRIID != *workflow.MRIID {
		return nil
	}
	findings, err := s.store.ListFindings(ctx, store.FindingFilter{WorkflowID: workflow.ID})
	if err != nil {
		return nil
	}
	for i := range findings {
		if findings[i].PublishKey == publication.PublishKey {
			return &findings[i]
		}
	}
	return nil
}

func (s *FeedbackService) collectNoteAwards(ctx context.Context, token string, publication store.Publication, noteID int64, findingID string) {
	awards, err := s.gitlab.ListNoteAwards(ctx, token, publication.ProjectID, publication.MRIID, noteID)
	if err != nil {
		return
	}
	for _, award := range awards {
		signal := ""
		switch award.Name {
		case "thumbsup":
			signal = "thumbsup"
		case "thumbsdown":
			signal = "thumbsdown"
		default:
			continue
		}
		userID := award.UserID
		if _, err := s.store.UpsertFeedback(ctx, store.Feedback{
			ProjectID:     publication.ProjectID,
			MRIID:         publication.MRIID,
			PublicationID: publication.ID,
			FindingID:     findingID,
			Signal:        signal,
			GitlabUserID:  &userID,
			ObservedAt:    s.now().UTC(),
		}); err != nil {
			s.logger.Warn("feedback upsert failed", "publication_id", publication.ID)
		}
	}
}

// eligibleMR reports whether the merge request can still produce feedback.
// Merged requests are collected only for three days after merging.
func (s *FeedbackService) eligibleMR(mr *gitlab.MergeRequest) bool {
	switch mr.State {
	case "opened":
		return true
	case "merged", "locked":
		if mr.MergedAt == nil {
			return false
		}
		return s.now().UTC().Sub(mr.MergedAt.UTC()) <= mergedFeedbackWindow
	default:
		return false
	}
}

// resolveSignal reports resolved_changed only when a zero-context diff proves
// that the published finding line was removed or replaced between the
// published head and the current head. It returns an empty signal when the
// evidence is missing or incomplete, so the caller records neither a positive
// nor a negative signal and can retry next pass.
func (s *FeedbackService) resolveSignal(ctx context.Context, token string, project *store.Project, publication store.Publication, workflow store.Workflow, mr *gitlab.MergeRequest, mrIID int64) string {
	if s.git == nil || s.diff == nil || s.workspace == nil || project == nil || mr == nil {
		return ""
	}
	if publication.ProjectID != project.ID || publication.MRIID != mrIID {
		return ""
	}
	if repo.ValidateSHA(workflow.HeadSHA) != nil || repo.ValidateSHA(mr.SHA) != nil {
		return ""
	}
	finding := s.publicationFinding(ctx, workflow, publication)
	if finding == nil || finding.Path == "" || finding.Line == nil || *finding.Line <= 0 {
		return ""
	}
	// Old-side lines belong to the base revision, which is not the published
	// head. Without a proven mapping they must stay unrecorded.
	if finding.Side != "" && finding.Side != "new" {
		return ""
	}
	if strings.EqualFold(workflow.HeadSHA, mr.SHA) {
		return "resolved_unchanged"
	}
	ws := s.workspace.Get(project.ID)
	if ws == nil {
		return ""
	}
	if !s.ensureHead(ctx, token, project, ws, mrIID, mr.SHA) {
		return ""
	}
	change, err := s.diff.FileDiff(ctx, ws.MirrorDir(), workflow.HeadSHA, mr.SHA, finding.Path)
	if err != nil {
		return ""
	}
	if change == nil {
		return "resolved_unchanged"
	}
	if change.Incomparable || change.Binary || change.OldPath != finding.Path {
		return ""
	}
	if change.Deleted {
		return "resolved_changed"
	}
	line := *finding.Line
	for _, hunk := range change.Hunks {
		if hunk.OldLines > 0 && line >= hunk.OldStart && line < hunk.OldStart+hunk.OldLines {
			return "resolved_changed"
		}
	}
	return "resolved_unchanged"
}

// ensureHead makes sha available in the project mirror. A missing object is
// fetched through the merge request ref; any failure leaves the caller without
// evidence instead of guessing.
func (s *FeedbackService) ensureHead(ctx context.Context, token string, project *store.Project, ws *workspace.Workspace, mrIID int64, sha string) bool {
	mirror := ws.MirrorDir()
	if _, err := s.git.Resolve(ctx, mirror, sha); err == nil {
		return true
	}
	if mrIID <= 0 {
		return false
	}
	remote := trimProjectWebURL(project.WebURL)
	if remote == "" {
		return false
	}
	auth := repo.Auth{Token: token}
	if err := s.git.EnsureMirror(ctx, mirror, remote, auth); err != nil {
		return false
	}
	ref := fmt.Sprintf("refs/merge-requests/%d/head", mrIID)
	if err := s.git.Fetch(ctx, mirror, auth, "+"+ref+":"+ref); err != nil {
		return false
	}
	resolved, err := s.git.Resolve(ctx, mirror, ref)
	return err == nil && strings.EqualFold(resolved, sha)
}

func (s *FeedbackService) project(ctx context.Context, projectID int64, projects map[int64]*store.Project, tokens map[int64]string) (*store.Project, string, bool) {
	if project, ok := projects[projectID]; ok {
		return project, tokens[projectID], project != nil
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil || project == nil || !project.Enabled {
		projects[projectID] = nil
		return nil, "", false
	}
	token, err := s.cipher.Decrypt(project.TokenEnc)
	if err != nil {
		projects[projectID] = nil
		return nil, "", false
	}
	projects[projectID] = project
	tokens[projectID] = string(token)
	return project, tokens[projectID], true
}
