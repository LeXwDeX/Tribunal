package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/errclass"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// reviewSummary is the list item. store.Workflow already carries every field
// the console renders; duration_ms is derived from the timestamps. MRTitle is
// the title persisted with the dossier. Nodes and FindingsSummary have the
// same shape and meaning as on the detail endpoint.
type reviewSummary struct {
	store.Workflow
	MRTitle         *string         `json:"mr_title,omitempty"`
	DurationMS      *int64          `json:"duration_ms,omitempty"`
	Nodes           []reviewNode    `json:"nodes"`
	FindingsSummary findingsSummary `json:"findings_summary"`
}

type findingsSummary struct {
	Published  int            `json:"published"`
	Marked     int            `json:"marked"`
	Dropped    int            `json:"dropped"`
	BySeverity map[string]int `json:"by_severity"`
}

type reviewUsage struct {
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`
}

type publicationsSummary struct {
	SummaryNoteID *int64  `json:"summary_note_id,omitempty"`
	Discussions   int     `json:"discussions"`
	CommitStatus  *string `json:"commit_status,omitempty"`
}

type reviewNode struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	Status          string   `json:"status"`
	DependsOn       []string `json:"depends_on,omitempty"`
	Provider        string   `json:"provider,omitempty"`
	Attempt         int      `json:"attempt"`
	DurationMS      *int64   `json:"duration_ms,omitempty"`
	UsageIncomplete bool     `json:"usage_incomplete"`
	// Reason is the durable failure class of a failed or skipped node:
	// executor_error, timeout, budget_exhausted, sandbox_denied,
	// required_node_failed, and so on. It stays empty for completed nodes and
	// when an older store has no event log.
	Reason string `json:"reason,omitempty"`
	// UsageInterruptions counts recovery restarts of this node. It comes from
	// the engine snapshot, not a stored column, and is omitted when zero.
	UsageInterruptions int `json:"usage_interruptions,omitempty"`
	// TokenFree marks a deterministic node that never calls a model.
	TokenFree bool `json:"token_free,omitempty"`
}

type reviewDetail struct {
	store.Workflow
	// UsageInterruptions is the largest per-node recovery restart count.
	UsageInterruptions int                 `json:"usage_interruptions,omitempty"`
	Access             string              `json:"access"`
	MRTitle            *string             `json:"mr_title,omitempty"`
	DurationMS         *int64              `json:"duration_ms,omitempty"`
	Nodes              []reviewNode        `json:"nodes"`
	FindingsSummary    findingsSummary     `json:"findings_summary"`
	Usage              reviewUsage         `json:"usage"`
	Publications       publicationsSummary `json:"publications"`
	// Summary, Warnings and Unresolved are the review-level diagnostics
	// persisted with the last completed synthesize node. They explain an
	// INCOMPLETE verdict that carries no workflow error.
	Summary    string                    `json:"summary,omitempty"`
	Warnings   []string                  `json:"warnings,omitempty"`
	Unresolved []review.UnresolvedSignal `json:"unresolved,omitempty"`
}

type graphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type nodeDetail struct {
	store.WorkflowNode
	DurationMS         *int64    `json:"duration_ms,omitempty"`
	Usage              nodeUsage `json:"usage"`
	UsageInterruptions int       `json:"usage_interruptions,omitempty"`
	// Reason is the durable failure class from the engine snapshot, or a
	// legacy fallback derived from the stored status.
	Reason string `json:"reason,omitempty"`
	// TokenFree marks a deterministic node that never calls a model.
	TokenFree bool `json:"token_free,omitempty"`
	// CallsPurged reports that the node spent tokens and finished before the
	// LLM call retention cutoff, so its audit rows were deleted.
	CallsPurged         bool `json:"calls_purged,omitempty"`
	CallsRetentionHours int  `json:"calls_retention_hours,omitempty"`
}

// workflowEventLoader is implemented by stores that keep the dag event log.
// The recovery interruption counters exist only in the projected engine view,
// so detail endpoints replay the log to read them.
type workflowEventLoader interface {
	Load(ctx context.Context, wfID string) ([]dag.Event, error)
}

// engineSnapshot projects the workflow's event log. It is best effort: a store
// without an event log, an unknown workflow or a projection error yields nil
// and the interruption fields are simply omitted.
func (s *Server) engineSnapshot(ctx context.Context, wfID string) *dag.WorkflowView {
	loader, ok := s.store.(workflowEventLoader)
	if !ok {
		return nil
	}
	events, err := loader.Load(ctx, wfID)
	if err != nil {
		s.logger.Warn("usage interruptions unavailable", "workflow_id", wfID, "error_class", errclass.Of(err))
		return nil
	}
	if len(events) == 0 {
		return nil
	}
	view, err := dag.Project(events)
	if err != nil {
		s.logger.Warn("usage interruptions unavailable", "workflow_id", wfID, "error_class", errclass.Of(err))
		return nil
	}
	return &view
}

type nodeUsage struct {
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`
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
		AccessUserID:     s.projectScopeUserID(r),
		GrantOnly:        strings.EqualFold(queryFilterValue(r, "access"), "mine"),
		MRIID:            mrIID,
		Kind:             "review", // reviews only; onboarding workflows are not reviews
		Status:           queryFilterValue(r, "status"),
		Verdict:          queryFilterValue(r, "verdict"),
		PublicationState: publicationState,
		Profile:          queryFilterValue(r, "profile"),
		Trigger:          queryFilterValue(r, "trigger"),
		Since:            since,
	}
	reviews, next, err := s.listReviewItems(r.Context(), filter, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": reviews, "next_cursor": next})
}

// summarizeReviewPage adds titles, nodes and findings counts to one page of
// the list. The page is already access filtered; the batched reads cover exactly
// its workflow IDs, with one query per kind of data.
func (s *Server) summarizeReviewPage(ctx context.Context, workflows []store.Workflow) ([]reviewSummary, error) {
	reviews := make([]reviewSummary, 0, len(workflows))
	if len(workflows) == 0 {
		return reviews, nil
	}
	ids := make([]string, 0, len(workflows))
	for _, wf := range workflows {
		ids = append(ids, wf.ID)
	}
	nodesByWorkflow, err := s.store.ListWorkflowNodesByWorkflow(ctx, ids)
	if err != nil {
		return nil, err
	}
	counts, err := s.store.CountFindingsByWorkflow(ctx, ids)
	if err != nil {
		return nil, err
	}
	titles, err := s.store.ListWorkflowMRTitles(ctx, ids)
	if err != nil {
		return nil, err
	}
	summaries := make(map[string]*findingsSummary, len(workflows))
	for _, id := range ids {
		summary := newFindingsSummary()
		summaries[id] = &summary
	}
	for _, count := range counts {
		if summary, ok := summaries[count.WorkflowID]; ok {
			summary.add(count.Decision, count.Severity, count.Count)
		}
	}
	for _, wf := range workflows {
		var mrTitle *string
		if title, ok := titles[wf.ID]; ok && title != "" {
			mrTitle = &title
		}
		reviews = append(reviews, reviewSummary{
			Workflow:        wf,
			MRTitle:         mrTitle,
			DurationMS:      workflowDuration(wf),
			Nodes:           reviewNodes(nodesByWorkflow[wf.ID], nil),
			FindingsSummary: *summaries[wf.ID],
		})
	}
	return reviews, nil
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
	jobID, err := s.reviews.StartReview(s.dispatchContext(r, projectID), projectID, mrIID, strings.TrimSpace(request.Profile), request.Full)
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
	snapshot := s.engineSnapshot(r.Context(), wf.ID)
	summary, warnings, unresolved := reviewLevelDiagnostics(nodes)
	detail := reviewDetail{
		Workflow:        *wf,
		Access:          grantedAccess(r.Context()),
		MRTitle:         mrTitle,
		DurationMS:      workflowDuration(*wf),
		Nodes:           reviewNodes(nodes, snapshot),
		FindingsSummary: summarizeFindings(findings),
		Usage: reviewUsage{
			TokensIn:  wf.TokensIn,
			TokensOut: wf.TokensOut,
		},
		Publications: summarizePublications(publications),
		Summary:      summary,
		Warnings:     warnings,
		Unresolved:   unresolved,
	}
	if snapshot != nil {
		detail.UsageInterruptions = snapshot.UsageInterruptions
	}
	writeJSON(w, http.StatusOK, detail)
}

func reviewNodes(nodes []store.WorkflowNode, snapshot *dag.WorkflowView) []reviewNode {
	out := make([]reviewNode, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, reviewNode{
			UsageInterruptions: nodeInterruptions(snapshot, node.ID),
			ID:                 node.ID,
			Kind:               node.Kind,
			Status:             node.Status,
			DependsOn:          node.DependsOn,
			Provider:           node.Provider,
			Attempt:            node.Attempt,
			DurationMS:         nodeDuration(node),
			UsageIncomplete:    node.UsageIncomplete,
			Reason:             nodeReason(node, snapshot),
			TokenFree:          nodeTokenFree(node.ID, snapshot),
		})
	}
	return out
}

func nodeTokenFree(nodeID string, snapshot *dag.WorkflowView) bool {
	if snapshot == nil {
		return false
	}
	return snapshot.Nodes[nodeID].Spec.TokenFree
}

// nodeReason prefers the durable failure class projected from the event log.
// A store without an event log still yields the engine's fixed reasons for
// failed and skipped nodes, never the stored message.
func nodeReason(node store.WorkflowNode, snapshot *dag.WorkflowView) string {
	if snapshot != nil {
		if view, ok := snapshot.Nodes[node.ID]; ok && view.Reason != "" {
			return view.Reason
		}
	}
	switch dag.NodeStatus(node.Status) {
	case dag.NodeSkipped:
		switch node.Error {
		case "workflow token budget exhausted":
			return "budget_exhausted"
		case "queue wait exceeded node timeout":
			return "queue_timeout"
		}
		return "required_node_failed"
	case dag.NodeFailed:
		switch node.Error {
		case "workflow token budget exhausted":
			return "budget_exhausted"
		case "queue wait exceeded node timeout":
			return "queue_timeout"
		}
		return "executor_error"
	}
	return ""
}

// reviewLevelDiagnostics decodes the last completed synthesize output. The
// output is passed through SafeDiagnosticOutput so diagnostics copied by older
// versions from failed verify nodes are not echoed again.
func reviewLevelDiagnostics(nodes []store.WorkflowNode) (string, []string, []review.UnresolvedSignal) {
	for i := len(nodes) - 1; i >= 0; i-- {
		node := nodes[i]
		if node.Kind != review.KindSynthesize || node.Status != string(dag.NodeCompleted) {
			continue
		}
		safe, err := review.SafeDiagnosticOutput(node.Kind, node.Output, diagnosticWorkflow(nodes))
		if err != nil || len(safe) == 0 || string(safe) == "null" {
			return "", nil, nil
		}
		var result struct {
			Summary    string                    `json:"summary"`
			Warnings   []string                  `json:"warnings"`
			Unresolved []review.UnresolvedSignal `json:"unresolved"`
		}
		if err := json.Unmarshal(safe, &result); err != nil {
			return "", nil, nil
		}
		return result.Summary, result.Warnings, result.Unresolved
	}
	return "", nil, nil
}

// safeNodeError keeps the engine's fixed failure messages and collapses
// anything else, so a legacy row cannot leak an upstream body.
func safeNodeError(message string) string {
	switch message {
	case "", "node execution failed", "workflow token budget exhausted",
		"required node failed", "queue wait exceeded node timeout",
		"required dependency did not complete":
		return message
	default:
		return "node failed"
	}
}

func nodeInterruptions(snapshot *dag.WorkflowView, nodeID string) int {
	if snapshot == nil {
		return 0
	}
	return snapshot.Nodes[nodeID].UsageInterruptions
}

func newFindingsSummary() findingsSummary {
	return findingsSummary{
		BySeverity: map[string]int{"P0": 0, "P1": 0, "P2": 0, "info": 0},
	}
}

// add counts n findings with one decision and effective severity. Detail and
// list summaries both go through it so their numbers cannot drift apart.
func (summary *findingsSummary) add(decision, severity string, n int) {
	switch decision {
	case "publish":
		summary.Published += n
	case "mark":
		summary.Marked += n
	case "drop":
		summary.Dropped += n
	}
	if severity != "" {
		summary.BySeverity[severity] += n
	}
}

func summarizeFindings(findings []store.Finding) findingsSummary {
	summary := newFindingsSummary()
	for _, finding := range findings {
		severity := finding.FinalSeverity
		if severity == "" {
			severity = finding.Severity
		}
		summary.add(finding.Decision, severity, 1)
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
	graphNodes := reviewNodes(nodes, s.engineSnapshot(r.Context(), wfID))
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
	nodes, err := s.store.GetWorkflowNodes(r.Context(), wfID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	wf := diagnosticWorkflow(nodes)
	for i := range findings {
		safeFindingDiagnostics(&findings[i], wf)
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
	nodes, err := s.store.GetWorkflowNodes(r.Context(), r.PathValue("wf_id"))
	if err != nil {
		s.writeStoreError(w, r, err)
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
	safeFindingDiagnostics(&finding, diagnosticWorkflow(nodes))
	writeJSON(w, http.StatusOK, finding)
}

func diagnosticWorkflow(nodes []store.WorkflowNode) dag.WorkflowView {
	wf := dag.WorkflowView{Nodes: make(map[string]dag.NodeView, len(nodes))}
	for _, node := range nodes {
		wf.Nodes[node.ID] = dag.NodeView{ID: node.ID, Status: dag.NodeStatus(node.Status), Error: node.Error, Spec: dag.NodeSpec{Kind: node.Kind, Params: node.Params}}
	}
	return wf
}

func safeFindingDiagnostics(finding *store.Finding, wf dag.WorkflowView) {
	challenge, reason, err := review.SafeFindingDiagnostics(finding.ID, finding.ChallengeEvidence, finding.DecisionReason, wf)
	if err != nil {
		finding.ChallengeEvidence = nil
		finding.DecisionReason = ""
		return
	}
	finding.ChallengeEvidence, finding.DecisionReason = challenge, reason
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
		output, err := review.SafeDiagnosticOutput(node.Kind, node.Output, diagnosticWorkflow(nodes))
		if err != nil {
			output = nil
		}
		node.Output = output
		node.Error = safeNodeError(node.Error)
		snapshot := s.engineSnapshot(r.Context(), wfID)
		purged, retentionHours := s.nodeCallsPurged(node)
		writeJSON(w, http.StatusOK, nodeDetail{
			WorkflowNode:        node,
			DurationMS:          nodeDuration(node),
			UsageInterruptions:  nodeInterruptions(snapshot, node.ID),
			Reason:              nodeReason(node, snapshot),
			TokenFree:           nodeTokenFree(node.ID, snapshot),
			CallsPurged:         purged,
			CallsRetentionHours: retentionHours,
			Usage: nodeUsage{
				TokensIn:  node.TokensIn,
				TokensOut: node.TokensOut,
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
	var target store.WorkflowNode
	for _, node := range nodes {
		if node.ID == nodeID {
			found = true
			target = node
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
	writeJSON(w, http.StatusOK, map[string]any{
		"calls":       calls,
		"next_cursor": next,
		"retention":   s.llmCallRetentionView(target, r.URL.Query().Get("cursor") == "", len(calls) > 0),
	})
}

// nodeCallsPurged reports whether the node's model-call details have passed
// the retention cutoff. It requires evidence of spend: an empty list on a code
// node is not a purge.
func (s *Server) nodeCallsPurged(node store.WorkflowNode) (bool, int) {
	if s.platform == nil || s.platform.Storage.LLMCallRetention <= 0 {
		return false, 0
	}
	retention := s.platform.Storage.LLMCallRetention
	spent := node.TokensIn > 0 || node.TokensOut > 0
	if !spent || node.FinishedAt == nil {
		return false, 0
	}
	cutoff := s.now().UTC().Add(-retention)
	return node.FinishedAt.Before(cutoff), int(retention.Hours())
}

// llmCallRetentionView tells the console why an empty call list may not mean a
// code node. A node with recorded token usage finished before the retention
// cutoff but has no stored calls: its audit rows were purged. The result is
// only asserted with evidence, never guessed from the node kind.
func (s *Server) llmCallRetentionView(node store.WorkflowNode, firstPage, hasCalls bool) map[string]any {
	view := map[string]any{"details_purged": false}
	if s.platform == nil || s.platform.Storage.LLMCallRetention <= 0 {
		return view
	}
	retention := s.platform.Storage.LLMCallRetention
	cutoff := s.now().UTC().Add(-retention)
	view["llm_call_retention_hours"] = int(retention.Hours())
	view["purge_before"] = cutoff.Format(time.RFC3339)
	purged, _ := s.nodeCallsPurged(node)
	if firstPage && !hasCalls && purged {
		view["details_purged"] = true
	}
	return view
}

func (s *Server) handleCancelReview(w http.ResponseWriter, r *http.Request) {
	if !s.requireReviews(w, r) {
		return
	}
	workflow, _ := guardedWorkflow(r.Context())
	var projectID int64
	if workflow != nil {
		projectID = workflow.ProjectID
	}
	jobID, err := s.reviews.CancelReview(s.dispatchContext(r, projectID), r.PathValue("wf_id"))
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
	workflow, _ := guardedWorkflow(r.Context())
	var projectID int64
	if workflow != nil {
		projectID = workflow.ProjectID
	}
	jobID, err := s.reviews.RepublishReview(s.dispatchContext(r, projectID), r.PathValue("wf_id"))
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": jobID})
}
