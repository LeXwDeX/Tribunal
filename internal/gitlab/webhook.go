package gitlab

import (
	"encoding/json"
	"fmt"
)

// Webhook is the common shape of GitLab project webhook payloads used at M0.
// Unknown fields are intentionally ignored: GitLab adds fields over time.
type Webhook struct {
	Event            string                  `json:"-"`
	UUID             string                  `json:"-"`
	Token            string                  `json:"-"`
	ObjectKind       string                  `json:"object_kind"`
	ProjectID        int64                   `json:"project_id"`
	Project          WebhookProject          `json:"project"`
	ObjectAttributes WebhookObjectAttributes `json:"object_attributes"`
	MergeRequest     WebhookMergeRequest     `json:"merge_request"`
	User             WebhookUser             `json:"user"`
	Labels           Labels                  `json:"labels"`
	Commits          []WebhookCommit         `json:"commits"`
	Ref              string                  `json:"ref"`
	Before           string                  `json:"before"`
	After            string                  `json:"after"`
	Changes          WebhookChanges          `json:"changes"`
}

// WebhookChange is one previous/current pair of a merge request update event.
type WebhookChange[T any] struct {
	Previous T `json:"previous"`
	Current  T `json:"current"`
}

// WebhookChanges reports the fields an update event changed. GitLab only
// includes entries that actually changed; a nil entry means "not reported".
type WebhookChanges struct {
	Draft        *WebhookChange[bool]   `json:"draft"`
	Labels       *WebhookChange[Labels] `json:"labels"`
	TargetBranch *WebhookChange[string] `json:"target_branch"`
}

// WebhookProject is the project portion of a webhook payload.
type WebhookProject struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
	DefaultBranch     string `json:"default_branch"`
}

// WebhookObjectAttributes covers merge request and note attributes.
type WebhookObjectAttributes struct {
	ID              int64         `json:"id"`
	IID             int64         `json:"iid"`
	TargetProjectID int64         `json:"target_project_id"`
	TargetBranch    string        `json:"target_branch"`
	SourceBranch    string        `json:"source_branch"`
	State           string        `json:"state"`
	Action          string        `json:"action"`
	OldRev          string        `json:"oldrev"`
	SHA             string        `json:"sha"`
	Note            string        `json:"note"`
	NoteableType    string        `json:"noteable_type"`
	NoteableIID     int64         `json:"noteable_iid"`
	AuthorID        int64         `json:"author_id"`
	LastCommit      WebhookCommit `json:"last_commit"`
	Labels          Labels        `json:"labels"`
	Draft           bool          `json:"draft"`
	WorkInProgress  bool          `json:"work_in_progress"`
	Title           string        `json:"title"`
}

// WebhookMergeRequest is the merge_request object included by MR and note hooks.
type WebhookMergeRequest struct {
	IID             int64         `json:"iid"`
	TargetProjectID int64         `json:"target_project_id"`
	TargetBranch    string        `json:"target_branch"`
	State           string        `json:"state"`
	LastCommit      WebhookCommit `json:"last_commit"`
	Labels          Labels        `json:"labels"`
	Draft           bool          `json:"draft"`
	WorkInProgress  bool          `json:"work_in_progress"`
	Title           string        `json:"title"`
}

// WebhookUser is the author/user object attached to a webhook.
type WebhookUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

// WebhookCommit is a commit reference in a push or merge request payload.
type WebhookCommit struct {
	ID string `json:"id"`
}

// Labels decodes GitLab labels given either as strings or as objects.
type Labels []string

// UnmarshalJSON accepts ["a"] and [{"title":"a"},{"name":"b"}].
func (l *Labels) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		if string(data) == "null" {
			*l = nil
			return nil
		}
		return err
	}
	labels := make([]string, 0, len(raw))
	for _, item := range raw {
		var text string
		if err := json.Unmarshal(item, &text); err == nil {
			if text != "" {
				labels = append(labels, text)
			}
			continue
		}
		var object struct {
			Title string `json:"title"`
			Name  string `json:"name"`
		}
		if err := json.Unmarshal(item, &object); err != nil {
			return fmt.Errorf("gitlab: invalid label value")
		}
		if object.Title != "" {
			labels = append(labels, object.Title)
		} else if object.Name != "" {
			labels = append(labels, object.Name)
		}
	}
	*l = labels
	return nil
}

// ParseWebhook decodes a payload and attaches trusted request metadata.
// It validates JSON syntax only; authorization and event support belong to app.
func ParseWebhook(event, uuid, token string, body []byte) (*Webhook, error) {
	var payload Webhook
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("gitlab: invalid webhook payload")
	}
	payload.Event = event
	payload.UUID = uuid
	payload.Token = token
	return &payload, nil
}
