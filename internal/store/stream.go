package store

// StreamSnapshot is the narrow projection a review stream polls. The workflow
// row is always returned; Nodes, Findings and Publications are only populated
// when the caller's revision differs from Revision. A nil slice therefore
// means "unchanged" and a non-nil slice (possibly empty) means "reload".
type StreamSnapshot struct {
	ProjectID        int64
	Status           string
	Verdict          string
	PublicationState string
	PublicationError string
	// Revision advances on every node, finding or publication write. Callers
	// pass the revision they already hold; -1 loads every collection.
	Revision     int64
	Nodes        []StreamNode
	Findings     []StreamFinding
	Publications []StreamPublication
}

// StreamNode is the node state the review stream emits.
type StreamNode struct {
	ID      string
	Status  string
	Attempt int
}

// StreamFinding is the finding state the review stream emits. Bodies are never
// loaded by the stream.
type StreamFinding struct {
	ID       string
	Decision string
}

// StreamPublication is the publication state the review stream emits.
type StreamPublication struct {
	ID       string
	Kind     string
	GitlabID string
}
