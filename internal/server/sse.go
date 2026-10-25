package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

const streamHeartbeatInterval = 15 * time.Second

type streamNodeEvent struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Attempt int    `json:"attempt"`
}

type streamFindingEvent struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
}

type streamWorkflowEvent struct {
	Status           string `json:"status"`
	Verdict          string `json:"verdict,omitempty"`
	PublicationState string `json:"publication_state,omitempty"`
	PublicationError string `json:"publication_error,omitempty"`
}

type streamPublishedEvent struct {
	Kind     string `json:"kind"`
	GitlabID string `json:"gitlab_id,omitempty"`
}

type streamNode struct {
	status  string
	attempt int
}

type streamPublication struct {
	kind     string
	gitlabID string
}

// streamSnapshot is a comparable projection of one workflow. The stream polls
// the store because no pub/sub bus exists in the single-process deployment;
// two snapshots are diffed per tick and flushes happen immediately.
type streamSnapshot struct {
	projectID        int64
	status           string
	verdict          string
	publicationState string
	publicationError string
	nodeOrder        []string
	nodes            map[string]streamNode
	findingOrder     []string
	findings         map[string]string
	publicationOrder []string
	publications     map[string]streamPublication
}

// terminal means both the workflow execution and its GitLab publication have
// settled. A pending publication keeps the stream open after the workflow
// itself completes, so clients see the final publication state instead of
// closing on execution alone.
func (snap *streamSnapshot) terminal() bool {
	switch snap.status {
	case "completed", "failed", "cancelled":
	default:
		return false
	}
	switch snap.publicationState {
	case "", store.WorkflowPublicationPublished, store.WorkflowPublicationSkipped,
		store.WorkflowPublicationFailed:
		return true
	default:
		return false
	}
}

func (s *Server) loadStreamSnapshot(ctx context.Context, workflowID string) (*streamSnapshot, error) {
	wf, err := s.store.GetWorkflow(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	nodes, err := s.store.GetWorkflowNodes(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	findings, err := s.store.ListFindings(ctx, store.FindingFilter{WorkflowID: workflowID})
	if err != nil {
		return nil, err
	}
	publications, err := s.store.ListPublications(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	snap := &streamSnapshot{
		projectID:        wf.ProjectID,
		status:           wf.Status,
		verdict:          wf.Verdict,
		publicationState: wf.PublicationState,
		publicationError: wf.PublicationError,
		nodes:            make(map[string]streamNode, len(nodes)),
		findings:         make(map[string]string, len(findings)),
		publications:     make(map[string]streamPublication, len(publications)),
	}
	for _, node := range nodes {
		if _, seen := snap.nodes[node.ID]; !seen {
			snap.nodeOrder = append(snap.nodeOrder, node.ID)
		}
		snap.nodes[node.ID] = streamNode{status: node.Status, attempt: node.Attempt}
	}
	for _, finding := range findings {
		if _, seen := snap.findings[finding.ID]; !seen {
			snap.findingOrder = append(snap.findingOrder, finding.ID)
		}
		snap.findings[finding.ID] = finding.Decision
	}
	for _, publication := range publications {
		if _, seen := snap.publications[publication.ID]; !seen {
			snap.publicationOrder = append(snap.publicationOrder, publication.ID)
		}
		snap.publications[publication.ID] = streamPublication{kind: publication.Kind, gitlabID: publication.GitlabID}
	}
	return snap, nil
}

type streamEvent struct {
	name  string
	value any
}

// diffStreamSnapshots reports only changes since previous. Initial state is not
// replayed: clients fetch the detail endpoint first, then open the stream.
func diffStreamSnapshots(previous, next *streamSnapshot) []streamEvent {
	events := make([]streamEvent, 0, 4)
	for _, id := range next.nodeOrder {
		value := next.nodes[id]
		old, seen := previous.nodes[id]
		if seen && old == value {
			continue
		}
		events = append(events, streamEvent{
			name:  "node",
			value: streamNodeEvent{ID: id, Status: value.status, Attempt: value.attempt},
		})
	}
	for _, id := range next.findingOrder {
		decision := next.findings[id]
		old, seen := previous.findings[id]
		if seen && old == decision {
			continue
		}
		events = append(events, streamEvent{
			name:  "finding",
			value: streamFindingEvent{ID: id, Decision: decision},
		})
	}
	for _, id := range next.publicationOrder {
		value := next.publications[id]
		old, seen := previous.publications[id]
		if seen && old == value {
			continue
		}
		events = append(events, streamEvent{
			name:  "published",
			value: streamPublishedEvent{Kind: value.kind, GitlabID: value.gitlabID},
		})
	}
	if previous.status != next.status || previous.verdict != next.verdict ||
		previous.publicationState != next.publicationState ||
		previous.publicationError != next.publicationError {
		events = append(events, streamEvent{
			name: "workflow",
			value: streamWorkflowEvent{
				Status:           next.status,
				Verdict:          next.verdict,
				PublicationState: next.publicationState,
				PublicationError: next.publicationError,
			},
		})
	}
	return events
}

func workflowStreamEvent(snapshot streamSnapshot) streamWorkflowEvent {
	return streamWorkflowEvent{
		Status:           snapshot.status,
		Verdict:          snapshot.verdict,
		PublicationState: snapshot.publicationState,
		PublicationError: snapshot.publicationError,
	}
}

// streamStillAuthorized repeats authentication and the project grant check.
// It never writes a response: the stream is already open, so the only safe
// outcome is closing it.
func (s *Server) streamStillAuthorized(r *http.Request, projectID int64) bool {
	value, ok := s.authenticate(r)
	if !ok || value.user.ID <= 0 || s.access == nil {
		return false
	}
	_, err := s.access.GetProjectAccess(r.Context(), value.user.ID, projectID)
	return err == nil
}

func writeSSEEvent(w io.Writer, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return err
	}
	return nil
}

// handleReviewStream serves GET /api/v1/reviews/{wf_id}/stream. Events are
// produced by polling the store every streamPoll interval (2s by default) and
// diffing snapshots; a comment line every 15s keeps proxies from closing an
// idle connection. The handler ends when the workflow reaches a terminal
// state, when the client disconnects, or when the workflow disappears. All
// work happens in the request goroutine, so no leaked goroutines remain.
func (s *Server) handleReviewStream(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	wfID := r.PathValue("wf_id")
	snapshot, err := s.loadStreamSnapshot(r.Context(), wfID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	controller := http.NewResponseController(w)
	// Clear the server write deadline for this long-lived response; the
	// request context and terminal workflow state end the stream.
	_ = controller.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, ": connected\n\n"); err != nil {
		return
	}
	if err := controller.Flush(); err != nil {
		return
	}
	if snapshot.terminal() {
		if err := writeSSEEvent(w, "workflow", workflowStreamEvent(*snapshot)); err != nil {
			return
		}
		_ = controller.Flush()
		return
	}

	interval := s.streamPoll
	if interval <= 0 {
		interval = streamPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	heartbeat := time.NewTicker(streamHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		case <-ticker.C:
			// Re-authenticate every poll: a revoked session or API token, a
			// disabled user, or a removed grant must close an open stream.
			if !s.streamStillAuthorized(r, snapshot.projectID) {
				return
			}
			next, err := s.loadStreamSnapshot(r.Context(), wfID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) || r.Context().Err() != nil {
					return
				}
				s.logger.Warn("review stream poll failed",
					"request_id", requestID(r.Context()), "workflow_id", wfID)
				return
			}
			for _, event := range diffStreamSnapshots(snapshot, next) {
				if err := writeSSEEvent(w, event.name, event.value); err != nil {
					return
				}
			}
			if err := controller.Flush(); err != nil {
				return
			}
			snapshot = next
			if snapshot.terminal() {
				return
			}
		}
	}
}
