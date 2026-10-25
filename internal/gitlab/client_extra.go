package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	sdk "gitlab.com/gitlab-org/api/client-go"
)

const (
	maxListPages   = 1000
	pageSize       = 100
	defaultStatus  = "ai-review"
	maxDescription = 140
	maxAwardName   = 100
)

func positiveIDs(ids ...int64) bool {
	for _, id := range ids {
		if id <= 0 {
			return false
		}
	}
	return true
}

func validSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func validateRepoPath(path string) error {
	if path == "" || strings.TrimSpace(path) != path || strings.HasPrefix(path, "/") ||
		strings.ContainsAny(path, "\\?#") {
		return errors.New("gitlab: invalid repository path")
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return errors.New("gitlab: invalid repository path")
		}
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("gitlab: invalid repository path")
		}
	}
	return nil
}

func validateRef(ref string) error {
	if ref == "" || len(ref) > 255 || strings.TrimSpace(ref) != ref ||
		strings.HasPrefix(ref, "-") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".lock") ||
		strings.Contains(ref, "..") || strings.ContainsAny(ref, " ~^:?*[\\") {
		return errors.New("gitlab: invalid ref")
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f {
			return errors.New("gitlab: invalid ref")
		}
	}
	return nil
}

func validTargetURL(raw string) bool {
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	return err == nil && u.User == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

func validatePosition(p *Position) error {
	if p == nil {
		return errors.New("gitlab: position is required")
	}
	if !validSHA(p.BaseSHA) || !validSHA(p.StartSHA) || !validSHA(p.HeadSHA) {
		return errors.New("gitlab: invalid position SHAs")
	}
	if err := validateRepoPath(p.OldPath); err != nil {
		return errors.New("gitlab: invalid position path")
	}
	if err := validateRepoPath(p.NewPath); err != nil {
		return errors.New("gitlab: invalid position path")
	}
	if p.OldLine < 0 || p.NewLine < 0 || (p.OldLine == 0 && p.NewLine == 0) {
		return errors.New("gitlab: invalid position line")
	}
	return nil
}

// listPages exhausts GitLab pagination. The caller owns the timeout context
// and a single concurrency permit for the whole list.
func listPages[T any](ctx context.Context, op string, fetch func(page int64) ([]*T, *sdk.Response, error)) ([]*T, error) {
	var out []*T
	page := int64(1)
	for calls := 0; ; calls++ {
		if calls >= maxListPages {
			return nil, fmt.Errorf("gitlab: %s pagination limit exceeded", op)
		}
		items, resp, err := fetch(page)
		if err != nil {
			return nil, mapError(ctx, op, resp, err)
		}
		out = append(out, items...)
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		if resp.NextPage <= page {
			return nil, fmt.Errorf("gitlab: %s invalid pagination", op)
		}
		page = resp.NextPage
	}
}

func noteFromSDK(note *sdk.Note) *Note {
	if note == nil {
		return nil
	}
	out := &Note{
		ID:             note.ID,
		Body:           note.Body,
		AuthorID:       note.Author.ID,
		AuthorUsername: note.Author.Username,
		System:         note.System,
		Resolvable:     note.Resolvable,
		Resolved:       note.Resolved,
		CreatedAt:      note.CreatedAt,
	}
	out.Position = positionFromSDK(note.Position)
	return out
}

func positionFromSDK(p *sdk.NotePosition) *Position {
	if p == nil {
		return nil
	}
	return &Position{
		BaseSHA:  p.BaseSHA,
		StartSHA: p.StartSHA,
		HeadSHA:  p.HeadSHA,
		OldPath:  p.OldPath,
		NewPath:  p.NewPath,
		OldLine:  int(p.OldLine),
		NewLine:  int(p.NewLine),
	}
}

func discussionFromSDK(d *sdk.Discussion) Discussion {
	out := Discussion{ID: d.ID, IndividualNote: d.IndividualNote}
	for _, note := range d.Notes {
		if mapped := noteFromSDK(note); mapped != nil {
			out.Notes = append(out.Notes, *mapped)
		}
	}
	return out
}

// ListMRDiffs returns every changed file. Pages are exhausted.
func (c *Client) ListMRDiffs(ctx context.Context, token string, projectID, iid int64) ([]FileDiff, error) {
	if !positiveIDs(projectID, iid) {
		return nil, errors.New("gitlab: invalid project or merge request ID")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	items, err := listPages(reqCtx, "list_mr_diffs", func(page int64) ([]*sdk.MergeRequestDiff, *sdk.Response, error) {
		return client.MergeRequests.ListMergeRequestDiffs(projectID, iid, &sdk.ListMergeRequestDiffsOptions{
			ListOptions: sdk.ListOptions{Page: page, PerPage: pageSize},
		}, sdk.WithContext(reqCtx))
	})
	if err != nil {
		return nil, err
	}
	out := make([]FileDiff, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		out = append(out, FileDiff{
			OldPath:       item.OldPath,
			NewPath:       item.NewPath,
			Diff:          item.Diff,
			NewFile:       item.NewFile,
			RenamedFile:   item.RenamedFile,
			DeletedFile:   item.DeletedFile,
			GeneratedFile: item.GeneratedFile,
			Collapsed:     item.Collapsed,
			TooLarge:      item.TooLarge,
		})
	}
	return out, nil
}

// LatestMRVersion returns the version with the highest ID across all pages.
func (c *Client) LatestMRVersion(ctx context.Context, token string, projectID, iid int64) (*Version, error) {
	if !positiveIDs(projectID, iid) {
		return nil, errors.New("gitlab: invalid project or merge request ID")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	items, err := listPages(reqCtx, "list_mr_versions", func(page int64) ([]*sdk.MergeRequestDiffVersion, *sdk.Response, error) {
		return client.MergeRequests.GetMergeRequestDiffVersions(projectID, iid, &sdk.GetMergeRequestDiffVersionsOptions{
			ListOptions: sdk.ListOptions{Page: page, PerPage: pageSize},
		}, sdk.WithContext(reqCtx))
	})
	if err != nil {
		return nil, err
	}
	var latest *sdk.MergeRequestDiffVersion
	for _, item := range items {
		if item != nil && (latest == nil || item.ID > latest.ID) {
			latest = item
		}
	}
	if latest == nil || latest.ID <= 0 ||
		!validSHA(latest.BaseCommitSHA) || !validSHA(latest.StartCommitSHA) || !validSHA(latest.HeadCommitSHA) {
		return nil, &Error{Op: "get_mr_version"}
	}
	return &Version{
		ID:       latest.ID,
		BaseSHA:  latest.BaseCommitSHA,
		StartSHA: latest.StartCommitSHA,
		HeadSHA:  latest.HeadCommitSHA,
	}, nil
}

// ListMRPipelines returns every pipeline attached to the merge request.
func (c *Client) ListMRPipelines(ctx context.Context, token string, projectID, iid int64) ([]Pipeline, error) {
	if !positiveIDs(projectID, iid) {
		return nil, errors.New("gitlab: invalid project or merge request ID")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	items, err := listPages(reqCtx, "list_mr_pipelines", func(page int64) ([]*sdk.PipelineInfo, *sdk.Response, error) {
		return client.MergeRequests.ListMergeRequestPipelines(projectID, iid,
			sdk.WithContext(reqCtx), sdk.WithOffsetPaginationParameters(page))
	})
	if err != nil {
		return nil, err
	}
	out := make([]Pipeline, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		out = append(out, Pipeline{
			ID:     item.ID,
			IID:    item.IID,
			Status: item.Status,
			Ref:    item.Ref,
			SHA:    item.SHA,
			WebURL: item.WebURL,
		})
	}
	return out, nil
}

// ListPipelineJobs returns every job in a pipeline.
func (c *Client) ListPipelineJobs(ctx context.Context, token string, projectID, pipelineID int64) ([]Job, error) {
	if !positiveIDs(projectID, pipelineID) {
		return nil, errors.New("gitlab: invalid project or pipeline ID")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	items, err := listPages(reqCtx, "list_pipeline_jobs", func(page int64) ([]*sdk.Job, *sdk.Response, error) {
		return client.Jobs.ListPipelineJobs(projectID, pipelineID, &sdk.ListJobsOptions{
			ListOptions: sdk.ListOptions{Page: page, PerPage: pageSize},
		}, sdk.WithContext(reqCtx))
	})
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		out = append(out, Job{
			ID:            item.ID,
			Name:          item.Name,
			Stage:         item.Stage,
			Status:        item.Status,
			AllowFailure:  item.AllowFailure,
			FailureReason: item.FailureReason,
			WebURL:        item.WebURL,
		})
	}
	return out, nil
}

// ListClosingIssues returns issues that merge will close.
func (c *Client) ListClosingIssues(ctx context.Context, token string, projectID, iid int64) ([]Issue, error) {
	if !positiveIDs(projectID, iid) {
		return nil, errors.New("gitlab: invalid project or merge request ID")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	items, err := listPages(reqCtx, "list_closing_issues", func(page int64) ([]*sdk.Issue, *sdk.Response, error) {
		return client.MergeRequests.GetIssuesClosedOnMerge(projectID, iid, &sdk.GetIssuesClosedOnMergeOptions{
			ListOptions: sdk.ListOptions{Page: page, PerPage: pageSize},
		}, sdk.WithContext(reqCtx))
	})
	if err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		out = append(out, Issue{
			IID:         item.IID,
			Title:       item.Title,
			State:       item.State,
			Description: item.Description,
			Labels:      append([]string(nil), item.Labels...),
			WebURL:      item.WebURL,
		})
	}
	return out, nil
}

// GetRawFile reads a file from the given ref, normally the target branch.
func (c *Client) GetRawFile(ctx context.Context, token string, projectID int64, path, ref string) ([]byte, error) {
	if projectID <= 0 {
		return nil, errors.New("gitlab: invalid project ID")
	}
	if err := validateRepoPath(path); err != nil {
		return nil, err
	}
	if err := validateRef(ref); err != nil {
		return nil, err
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	body, resp, err := client.RepositoryFiles.GetRawFile(projectID, path, &sdk.GetRawFileOptions{
		Ref: &ref,
	}, sdk.WithContext(reqCtx))
	if err != nil {
		return nil, mapError(reqCtx, "get_raw_file", resp, err)
	}
	return body, nil
}

// ListMRNotes returns every merge request note.
func (c *Client) ListMRNotes(ctx context.Context, token string, projectID, iid int64) ([]Note, error) {
	if !positiveIDs(projectID, iid) {
		return nil, errors.New("gitlab: invalid project or merge request ID")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	items, err := listPages(reqCtx, "list_mr_notes", func(page int64) ([]*sdk.Note, *sdk.Response, error) {
		return client.Notes.ListMergeRequestNotes(projectID, iid, &sdk.ListMergeRequestNotesOptions{
			ListOptions: sdk.ListOptions{Page: page, PerPage: pageSize},
		}, sdk.WithContext(reqCtx))
	})
	if err != nil {
		return nil, err
	}
	out := make([]Note, 0, len(items))
	for _, item := range items {
		if mapped := noteFromSDK(item); mapped != nil {
			out = append(out, *mapped)
		}
	}
	return out, nil
}

// CreateMRNote creates a merge request summary note.
func (c *Client) CreateMRNote(ctx context.Context, token string, projectID, iid int64, body string) (*Note, error) {
	if !positiveIDs(projectID, iid) {
		return nil, errors.New("gitlab: invalid project or merge request ID")
	}
	if strings.TrimSpace(body) == "" {
		return nil, errors.New("gitlab: note body is required")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	note, resp, err := client.Notes.CreateMergeRequestNote(projectID, iid, &sdk.CreateMergeRequestNoteOptions{
		Body: &body,
	}, sdk.WithContext(reqCtx))
	if err != nil {
		return nil, mapError(reqCtx, "create_note", resp, err)
	}
	mapped := noteFromSDK(note)
	if mapped == nil || mapped.ID <= 0 {
		return nil, &Error{Op: "create_note"}
	}
	return mapped, nil
}

// UpdateMRNote updates an existing merge request note in place.
func (c *Client) UpdateMRNote(ctx context.Context, token string, projectID, iid, noteID int64, body string) error {
	if !positiveIDs(projectID, iid, noteID) {
		return errors.New("gitlab: invalid project, merge request, or note ID")
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("gitlab: note body is required")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return err
	}
	defer done()

	_, resp, err := client.Notes.UpdateMergeRequestNote(projectID, iid, noteID, &sdk.UpdateMergeRequestNoteOptions{
		Body: &body,
	}, sdk.WithContext(reqCtx))
	if err != nil {
		return mapError(reqCtx, "update_note", resp, err)
	}
	return nil
}

// ListDiscussions returns every merge request discussion and its notes.
func (c *Client) ListDiscussions(ctx context.Context, token string, projectID, iid int64) ([]Discussion, error) {
	if !positiveIDs(projectID, iid) {
		return nil, errors.New("gitlab: invalid project or merge request ID")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	items, err := listPages(reqCtx, "list_discussions", func(page int64) ([]*sdk.Discussion, *sdk.Response, error) {
		return client.Discussions.ListMergeRequestDiscussions(projectID, iid, &sdk.ListMergeRequestDiscussionsOptions{
			ListOptions: sdk.ListOptions{Page: page, PerPage: pageSize},
		}, sdk.WithContext(reqCtx))
	})
	if err != nil {
		return nil, err
	}
	out := make([]Discussion, 0, len(items))
	for _, item := range items {
		if item != nil {
			out = append(out, discussionFromSDK(item))
		}
	}
	return out, nil
}

// CreateDiscussion creates an MR note, optionally anchored to a diff line.
func (c *Client) CreateDiscussion(ctx context.Context, token string, projectID, iid int64, d NewDiscussion) (*Discussion, error) {
	if !positiveIDs(projectID, iid) {
		return nil, errors.New("gitlab: invalid project or merge request ID")
	}
	if strings.TrimSpace(d.Body) == "" {
		return nil, errors.New("gitlab: discussion body is required")
	}
	opts := &sdk.CreateMergeRequestDiscussionOptions{Body: &d.Body}
	if d.Position != nil {
		position := *d.Position
		if position.OldPath == "" {
			position.OldPath = position.NewPath
		}
		if position.NewPath == "" {
			position.NewPath = position.OldPath
		}
		if err := validatePosition(&position); err != nil {
			return nil, err
		}
		positionType := "text"
		opts.Position = &sdk.PositionOptions{
			BaseSHA:      &position.BaseSHA,
			StartSHA:     &position.StartSHA,
			HeadSHA:      &position.HeadSHA,
			OldPath:      &position.OldPath,
			NewPath:      &position.NewPath,
			PositionType: &positionType,
		}
		// GitLab rejects a zero line as an invalid line_code. An added line has
		// no old line and a deleted line has no new line, so omit the absent side.
		if position.OldLine > 0 {
			opts.Position.OldLine = int64Ptr(position.OldLine)
		}
		if position.NewLine > 0 {
			opts.Position.NewLine = int64Ptr(position.NewLine)
		}
	}

	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	out, resp, err := client.Discussions.CreateMergeRequestDiscussion(projectID, iid, opts, sdk.WithContext(reqCtx))
	if err != nil {
		return nil, mapError(reqCtx, "create_discussion", resp, err)
	}
	if out == nil || out.ID == "" {
		return nil, &Error{Op: "create_discussion"}
	}
	mapped := discussionFromSDK(out)
	return &mapped, nil
}

// SetCommitStatus writes the ai-review status for a commit SHA.
func (c *Client) SetCommitStatus(ctx context.Context, token string, projectID int64, sha string, st CommitStatus) error {
	if projectID <= 0 {
		return errors.New("gitlab: invalid project ID")
	}
	if !validSHA(sha) {
		return errors.New("gitlab: invalid commit SHA")
	}
	if st.State != "success" && st.State != "failed" {
		return errors.New("gitlab: invalid commit status state")
	}
	name := st.Name
	if name == "" {
		name = defaultStatus
	}
	if len(name) > 255 {
		return errors.New("gitlab: invalid commit status name")
	}
	if utf8.RuneCountInString(st.Description) > maxDescription {
		return errors.New("gitlab: commit status description is too long")
	}
	if !validTargetURL(st.TargetURL) {
		return errors.New("gitlab: invalid commit status target URL")
	}
	state := sdk.BuildStateValue(st.State)
	options := &sdk.SetCommitStatusOptions{
		State: state,
		Name:  &name,
	}
	if st.TargetURL != "" {
		options.TargetURL = &st.TargetURL
	}
	if st.Description != "" {
		options.Description = &st.Description
	}

	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return err
	}
	defer done()

	_, resp, err := client.Commits.SetCommitStatus(projectID, sha, options, sdk.WithContext(reqCtx))
	if err != nil {
		return mapError(reqCtx, "set_commit_status", resp, err)
	}
	return nil
}

// ListNoteAwards returns emoji reactions on a merge request note.
func (c *Client) ListNoteAwards(ctx context.Context, token string, projectID, iid, noteID int64) ([]Award, error) {
	if !positiveIDs(projectID, iid, noteID) {
		return nil, errors.New("gitlab: invalid project, merge request, or note ID")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	items, err := listPages(reqCtx, "list_note_awards", func(page int64) ([]*sdk.AwardEmoji, *sdk.Response, error) {
		return client.AwardEmoji.ListMergeRequestAwardEmojiOnNote(projectID, iid, noteID, &sdk.ListAwardEmojiOptions{
			ListOptions: sdk.ListOptions{Page: page, PerPage: pageSize},
		}, sdk.WithContext(reqCtx))
	})
	if err != nil {
		return nil, err
	}
	out := make([]Award, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		out = append(out, awardFromSDK(item))
	}
	return out, nil
}

// AddNoteAward adds one emoji reaction to a merge request note.
func (c *Client) AddNoteAward(ctx context.Context, token string, projectID, iid, noteID int64, name string) (*Award, error) {
	if !positiveIDs(projectID, iid, noteID) {
		return nil, errors.New("gitlab: invalid project, merge request, or note ID")
	}
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxAwardName {
		return nil, errors.New("gitlab: invalid award name")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return nil, err
	}
	defer done()

	out, resp, err := client.AwardEmoji.CreateMergeRequestAwardEmojiOnNote(projectID, iid, noteID, &sdk.CreateAwardEmojiOptions{
		Name: name,
	}, sdk.WithContext(reqCtx))
	if err != nil {
		return nil, mapError(reqCtx, "add_note_award", resp, err)
	}
	if out == nil {
		return nil, &Error{Op: "add_note_award"}
	}
	mapped := awardFromSDK(out)
	return &mapped, nil
}

// DeleteNoteAward deletes one emoji reaction from a merge request note.
func (c *Client) DeleteNoteAward(ctx context.Context, token string, projectID, iid, noteID, awardID int64) error {
	if !positiveIDs(projectID, iid, noteID, awardID) {
		return errors.New("gitlab: invalid project, merge request, note, or award ID")
	}
	reqCtx, client, done, err := c.prepare(ctx, token)
	if err != nil {
		return err
	}
	defer done()

	resp, err := client.AwardEmoji.DeleteMergeRequestAwardEmojiOnNote(projectID, iid, noteID, awardID, sdk.WithContext(reqCtx))
	if err != nil {
		return mapError(reqCtx, "delete_note_award", resp, err)
	}
	return nil
}

func awardFromSDK(item *sdk.AwardEmoji) Award {
	return Award{
		ID:       item.ID,
		Name:     item.Name,
		UserID:   item.User.ID,
		Username: item.User.Username,
	}
}

func int64Ptr(value int) *int64 {
	v := int64(value)
	return &v
}
