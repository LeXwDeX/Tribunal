package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

// reviewSummary is the list item. store.Workflow already carries every field
// the console renders; duration_ms is derived from the timestamps.
type reviewSummary struct {
	store.Workflow
	DurationMS *int64 `json:"duration_ms,omitempty"`
}

type findingsSummary struct {
	Published  int            `json:"published"`
	Marked     int            `json:"marked"`
	Dropped    int            `json:"dropped"`
	BySeverity map[string]int `json:"by_severity"`
}

type reviewUsage struct {
	TokensIn   int64 `json:"tokens_in"`
	TokensOut  int64 `json:"tokens_out"`
	CostMicros int64 `json:"cost_micros"`
}

type publicationsSummary struct {
	SummaryNoteID *int64  `json:"summary_note_id,omitempty"`
	Discussions   int     `json:"discussions"`
	CommitStatus  *string `json:"commit_status,omitempty"`
}

type reviewNode struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Status     string   `json:"status"`
	DependsOn  []string `json:"depends_on,omitempty"`
	Provider   string   `json:"provider,omitempty"`
	Attempt    int      `json:"attempt"`
	DurationMS *int64   `json:"duration_ms,omitempty"`
}

type reviewDetail struct {
	store.Workflow
	Access          string              `json:"access"`
	MRTitle         *string             `json:"mr_title,omitempty"`
	DurationMS      *int64              `json:"duration_ms,omitempty"`
	Nodes           []reviewNode        `json:"nodes"`
	FindingsSummary findingsSummary     `json:"findings_summary"`
	Usage           reviewUsage         `json:"usage"`
	Publications    publicationsSummary `json:"publications"`
}

type graphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type nodeDetail struct {
	store.WorkflowNode
	DurationMS *int64    `json:"duration_ms,omitempty"`
	Usage      nodeUsage `json:"usage"`
}

type nodeUsage struct {
	TokensIn   int64 `json:"tokens_in"`
	TokensOut  int64 `json:"tokens_out"`
	CostMicros int64 `json:"cost_micros"`
}

func (s *Server) handleListReviews(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	if !s.requireAccess(w, r) {
		return
	}
	projectID, err := queryInt64(r, "project_id")
	if err != nil {
		validationFailed(w, r, "project_id", err.Error())
		return
	}
	if projectID != nil {
		if _, ok := s.requireProjectAccess(w, r, *projectID, false); !ok {
			return
		}
	}
	publicationState, err := publicationStateFilter(r)
	if err != nil {
		validationFailed(w, r, "publication_state", err.Error())
		return
	}
	mrIID, err := queryInt64(r, "mr_iid")
	if err != nil {
		validationFailed(w, r, "mr_iid", err.Error())
		return
	}
	since, err := queryTime(r, "since")
	if err != nil {
		validationFailed(w, r, "since", err.Error())
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		validationFailed(w, r, "limit", err.Error())
		return
	}
	filter := store.WorkflowFilter{
		ProjectID:        projectID,
		AccessUserID:     accessUserIDPtr(currentUserID(r)),
		MRIID:            mrIID,
		Kind:             "review", // reviews only; onboarding workflows are not reviews
		Status:           queryFilterValue(r, "status"),
		Verdict:          queryFilterValue(r, "verdict"),
		PublicationState: publicationState,
		Profile:          queryFilterValue(r, "profile"),
		Trigger:          queryFilterValue(r, "trigger"),
		Since:            since,
	}
	workflows, next, err := s.store.ListWorkflows(r.Context(), filter, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	reviews := make([]reviewSummary, 0, len(workflows))
	for _, wf := range workflows {
		reviews = append(reviews, reviewSummary{Workflow: wf, DurationMS: workflowDuration(wf)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": reviews, "next_cursor": next})
}

// queryFilterValue bounds free-text filters so oversized values do not reach
// the database.
func queryFilterValue(r *http.Request, name string) string {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if len(value) > 64 {
		value = value[:64]
	}
	return value
}

// publicationStateFilter validates the optional publication filter. The set
// matches the persisted publication states; an unknown value is a request
// error instead of a silent empty result.
func publicationStateFilter(r *http.Request) (string, error) {
	value := strings.TrimSpace(r.URL.Query().Get("publication_state"))
	switch value {
	case "", store.WorkflowPublicationPending, store.WorkflowPublicationPublished,
		store.WorkflowPublicationSkipped, store.WorkflowPublicationFailed:
		return value, nil
	default:
		return "", errors.New("publication_state must be pending, published, skipped or failed")
	}
}

func workflowDuration(wf store.Workflow) *int64 {
	if wf.StartedAt == nil || wf.FinishedAt == nil {
		return nil
	}
	ms := wf.FinishedAt.Sub(*wf.StartedAt).Milliseconds()
	if ms < 0 {
		return nil
	}
	return &ms
}

func nodeDuration(node store.WorkflowNode) *int64 {
	if node.StartedAt == nil || node.FinishedAt == nil {
		return nil
	}
	ms := node.FinishedAt.Sub(*node.StartedAt).Milliseconds()
	if ms < 0 {
		return nil
	}
	return &ms
}

func (s *Server) handleTriggerReview(w http.ResponseWriter, r *http.Request) {
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	mrIID, err := parseID(r.PathValue("iid"))
	if err != nil {
		validationFailed(w, r, "iid", "iid must be a positive integer")
		return
	}
	if !s.requireReviews(w, r) {
		return
	}
	// The route guard already checked manage; keep the check here explicit so a
	// future route registration cannot skip it.
	if _, ok := s.requireProjectAccess(w, r, projectID, true); !ok {
		return
	}
	var request struct {
		Profile string `json:"profile,omitempty"`
		Full    bool   `json:"full,omitempty"`
	}
	if err := decodeOptionalJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	jobID, err := s.reviews.StartReview(r.Context(), projectID, mrIID, strings.TrimSpace(request.Profile), request.Full)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": jobID})
}

func (s *Server) handleGetReview(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	wf, err := s.store.GetWorkflow(r.Context(), r.PathValue("wf_id"))
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	nodes, err := s.store.GetWorkflowNodes(r.Context(), wf.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	findings, err := s.store.ListFindings(r.Context(), store.FindingFilter{WorkflowID: wf.ID})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	publications, err := s.store.ListPublications(r.Context(), wf.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	mrTitle, err := reviewMRTitle(r.Context(), s.store, wf.ID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	detail := reviewDetail{
		Workflow:        *wf,
		Access:          grantedAccess(r.Context()),
		MRTitle:         mrTitle,
		DurationMS:      workflowDuration(*wf),
		Nodes:           reviewNodes(nodes),
		FindingsSummary: summarizeFindings(findings),
		Usage: reviewUsage{
			TokensIn:   wf.TokensIn,
			TokensOut:  wf.TokensOut,
			CostMicros: wf.CostMicros,
		},
		Publications: summarizePublications(publications),
	}
	writeJSON(w, http.StatusOK, detail)
}

func reviewNodes(nodes []store.WorkflowNode) []reviewNode {
	out := make([]reviewNode, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, reviewNode{
			ID:         node.ID,
			Kind:       node.Kind,
			Status:     node.Status,
			DependsOn:  node.DependsOn,
			Provider:   node.Provider,
			Attempt:    node.Attempt,
			DurationMS: nodeDuration(node),
		})
	}
	return out
}

func summarizeFindings(findings []store.Finding) findingsSummary {
	summary := findingsSummary{
		BySeverity: map[string]int{"P0": 0, "P1": 0, "P2": 0, "info": 0},
	}
	for _, finding := range findings {
		switch finding.Decision {
		case "publish":
			summary.Published++
		case "mark":
			summary.Marked++
		case "drop":
			summary.Dropped++
		}
		severity := finding.FinalSeverity
		if severity == "" {
			severity = finding.Severity
		}
		if severity != "" {
			summary.BySeverity[severity]++
		}
	}
	return summary
}

// reviewMRTitle reads the title from the stored dossier snapshot. A workflow
// without a dossier has no title; it is never fetched from GitLab here.
func reviewMRTitle(ctx context.Context, st Store, workflowID string) (*string, error) {
	record, err := st.GetWorkflowDossier(ctx, workflowID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if record == nil || len(record.Data) == 0 {
		return nil, nil
	}
	var snapshot struct {
		MR struct {
			Title string `json:"title"`
		} `json:"mr"`
	}
	if err := json.Unmarshal(record.Data, &snapshot); err != nil {
		return nil, nil
	}
	title := strings.TrimSpace(snapshot.MR.Title)
	if title == "" {
		return nil, nil
	}
	return &title, nil
}

// summarizePublications reports only what the store actually knows. A commit
// status state is recorded in the publication key at publish time; a legacy
// key without a state stays unknown instead of being inferred from the verdict.
// Newest means the greatest UpdatedAt, then the greatest ID (UUIDv7). The
// result therefore does not depend on slice order or SQLite pagination.
func summarizePublications(publications []store.Publication) publicationsSummary {
	summary := publicationsSummary{}
	var latestStatus time.Time
	latestID := ""
	for _, publication := range publications {
		switch publication.Kind {
		case "summary":
			if summary.SummaryNoteID == nil {
				if id, err := strconv.ParseInt(publication.GitlabID, 10, 64); err == nil && id > 0 {
					summary.SummaryNoteID = &id
				}
			}
		case "discussion":
			summary.Discussions++
		case "commit_status":
			state, ok := commitStatusState(publication.PublishKey)
			if !ok {
				continue
			}
			newer := publication.UpdatedAt.After(latestStatus) ||
				(publication.UpdatedAt.Equal(latestStatus) && publication.ID > latestID)
			if summary.CommitStatus == nil || newer {
				value := state
				summary.CommitStatus = &value
				latestStatus = publication.UpdatedAt
				latestID = publication.ID
			}
		}
	}
	return summary
}

// commitStatusState parses the state suffix written by the publisher. The
// legacy key "status:<sha>" has no state and must not be guessed. A SHA that
// fails the repository object-ID policy is not a commit status key at all.
func commitStatusState(publishKey string) (string, bool) {
	rest, ok := strings.CutPrefix(publishKey, "status:")
	if !ok {
		return "", false
	}
	separator := strings.LastIndex(rest, ":")
	if separator <= 0 || separator == len(rest)-1 {
		return "", false
	}
	if repo.ValidateSHA(rest[:separator]) != nil {
		return "", false
	}
	state := rest[separator+1:]
	if state != "success" && state != "failed" {
		return "", false
	}
	return state, true
}

func (s *Server) handleReviewGraph(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	wfID := r.PathValue("wf_id")
	if _, err := s.store.GetWorkflow(r.Context(), wfID); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	nodes, err := s.store.GetWorkflowNodes(r.Context(), wfID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	graphNodes := reviewNodes(nodes)
	edges := make([]graphEdge, 0)
	for _, node := range nodes {
		for _, dependency := range node.DependsOn {
			edges = append(edges, graphEdge{From: dependency, To: node.ID})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": graphNodes, "edges": edges})
}

func (s *Server) handleReviewFindings(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		validationFailed(w, r, "limit", err.Error())
		return
	}
	wfID := r.PathValue("wf_id")
	if _, err := s.store.GetWorkflow(r.Context(), wfID); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	findings, err := s.store.ListFindings(r.Context(), store.FindingFilter{
		WorkflowID: wfID,
		Decision:   queryFilterValue(r, "decision"),
		Severity:   queryFilterValue(r, "severity"),
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	page, next, err := pageFindings(findings, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"findings": page, "next_cursor": next})
}

// handleSetFindingAnnotation stores one human label. The actor comes from the
// authenticated principal and the timestamp from the store; neither is read
// from the request body.
func (s *Server) handleSetFindingAnnotation(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	var request struct {
		Annotation *string `json:"annotation"`
	}
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if request.Annotation == nil {
		validationFailed(w, r, "annotation", "annotation is required")
		return
	}
	annotation := *request.Annotation
	if !store.ValidFindingAnnotation(annotation) {
		validationFailed(w, r, "annotation", "annotation must be valid, invalid, uncertain, or empty")
		return
	}
	actor := ""
	if principal, ok := principalFrom(r.Context()); ok {
		actor = principal.user.Username
	}
	finding, err := s.store.SetFindingAnnotation(r.Context(),
		r.PathValue("wf_id"), r.PathValue("finding_id"), annotation, actor)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, finding)
}

// pageFindings paginates the already-filtered finding list. Filters are
// applied by the store before this cursor window, so page size can never
// change which findings match.
func pageFindings(findings []store.Finding, cursor string, limit int) ([]store.Finding, string, error) {
	start := 0
	if cursor != "" {
		lastID, err := decodeFindingCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		start = -1
		for i := range findings {
			if findings[i].ID == lastID {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return nil, "", store.ErrInvalidCursor
		}
	}
	if start >= len(findings) {
		return []store.Finding{}, "", nil
	}
	end := start + limit
	next := ""
	if end < len(findings) {
		next = encodeFindingCursor(findings[end-1].ID)
	} else {
		end = len(findings)
	}
	return findings[start:end], next, nil
}

func (s *Server) handleReviewNode(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	wfID := r.PathValue("wf_id")
	// node_id is a single {node_id} segment: dots are preserved verbatim, so
	// IDs such as review.std.p1 match exactly.
	nodeID := r.PathValue("node_id")
	if nodeID == "" {
		validationFailed(w, r, "node_id", "node id is required")
		return
	}
	if _, err := s.store.GetWorkflow(r.Context(), wfID); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	nodes, err := s.store.GetWorkflowNodes(r.Context(), wfID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	for _, node := range nodes {
		if node.ID != nodeID {
			continue
		}
		writeJSON(w, http.StatusOK, nodeDetail{
			WorkflowNode: node,
			DurationMS:   nodeDuration(node),
			Usage: nodeUsage{
				TokensIn:   node.TokensIn,
				TokensOut:  node.TokensOut,
				CostMicros: node.CostMicros,
			},
		})
		return
	}
	writeError(w, r, http.StatusNotFound, "not_found", "node not found", nil)
}

func (s *Server) handleNodeCalls(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		validationFailed(w, r, "limit", err.Error())
		return
	}
	includePayload, err := queryBool(r, "include_payload")
	if err != nil {
		validationFailed(w, r, "include_payload", err.Error())
		return
	}
	wfID := r.PathValue("wf_id")
	if includePayload {
		workflow, ok := guardedWorkflow(r.Context())
		if !ok {
			writeError(w, r, http.StatusForbidden, "forbidden", "project manage role required", nil)
			return
		}
		if !s.requireManageGranted(w, r, workflow.ProjectID) {
			return
		}
	}
	nodeID := r.PathValue("node_id")
	if _, err := s.store.GetWorkflow(r.Context(), wfID); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	nodes, err := s.store.GetWorkflowNodes(r.Context(), wfID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	found := false
	for _, node := range nodes {
		if node.ID == nodeID {
			found = true
			break
		}
	}
	if !found {
		writeError(w, r, http.StatusNotFound, "not_found", "node not found", nil)
		return
	}
	calls, next, err := s.store.ListLLMCalls(r.Context(), store.LLMCallFilter{
		WorkflowID:     wfID,
		NodeID:         nodeID,
		Cursor:         r.URL.Query().Get("cursor"),
		Limit:          limit,
		IncludePayload: includePayload,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if calls == nil {
		calls = []store.LLMCall{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"calls": calls, "next_cursor": next})
}

func (s *Server) handleCancelReview(w http.ResponseWriter, r *http.Request) {
	if !s.requireReviews(w, r) {
		return
	}
	jobID, err := s.reviews.CancelReview(r.Context(), r.PathValue("wf_id"))
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": jobID})
}

func (s *Server) handleRepublishReview(w http.ResponseWriter, r *http.Request) {
	if !s.requireReviews(w, r) {
		return
	}
	jobID, err := s.reviews.RepublishReview(r.Context(), r.PathValue("wf_id"))
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": jobID})
}
