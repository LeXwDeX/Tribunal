package gitlab

import (
	"strconv"
	"strings"
	"time"
)

// FileDiff is one changed file from the merge request diff endpoint.
type FileDiff struct {
	OldPath       string
	NewPath       string
	Diff          string
	NewFile       bool
	RenamedFile   bool
	DeletedFile   bool
	GeneratedFile bool
	Collapsed     bool
	TooLarge      bool
	// Collection reports the diff version scope of the whole list. It is set
	// on the first entry only, when the collector could read the merge
	// request's reported changed-path count.
	Collection *DiffCollection
}

// DiffCollection is the merge request diff version scope reported by GitLab.
// Total is the number of changed paths GitLab reports for the version; when
// Truncated is set, the returned diff list is a subset of that scope.
type DiffCollection struct {
	State     string
	RealSize  string
	Total     int
	Truncated bool
}

// ParseChangedFileCount parses a GitLab real_size or changes_count value. A
// trailing "+" marks a count that GitLab truncated at its own limit.
func ParseChangedFileCount(value string) (count int, truncated bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if strings.HasSuffix(value, "+") {
		truncated = true
		value = strings.TrimSpace(strings.TrimSuffix(value, "+"))
	}
	count, err := strconv.Atoi(value)
	if err != nil || count < 0 {
		return 0, false
	}
	return count, truncated
}

// Version is a merge request diff version. The three SHAs are required for
// inline discussion positions. State and RealSize report the completeness of
// the diff collection GitLab stored for this version.
type Version struct {
	ID       int64
	BaseSHA  string
	StartSHA string
	HeadSHA  string
	State    string
	RealSize string
}

// Pipeline is the pipeline projection used in the review dossier.
type Pipeline struct {
	ID     int64
	IID    int64
	Status string
	Ref    string
	SHA    string
	WebURL string
}

// Job is a CI job belonging to a pipeline.
type Job struct {
	ID            int64
	Name          string
	Stage         string
	Status        string
	AllowFailure  bool
	FailureReason string
	WebURL        string
}

// Issue is an issue that closes when the merge request is merged.
type Issue struct {
	IID         int64
	Title       string
	State       string
	Description string
	Labels      []string
	WebURL      string
}

// Note is a merge request note (or discussion note).
type Note struct {
	ID             int64
	Body           string
	AuthorID       int64
	AuthorUsername string
	System         bool
	Resolvable     bool
	Resolved       bool
	Position       *Position
	CreatedAt      *time.Time
}

// Discussion is a merge request discussion thread.
type Discussion struct {
	ID             string
	IndividualNote bool
	Notes          []Note
}

// Award is an emoji reaction on a note.
type Award struct {
	ID       int64
	Name     string
	UserID   int64
	Username string
}

// CommitStatus is the payload written for a commit status.
type CommitStatus struct {
	State       string
	Name        string
	TargetURL   string
	Description string
}

// NewDiscussion is a body plus an optional inline position.
type NewDiscussion struct {
	Body     string
	Position *Position
}

// Side selects which side of a diff a line number refers to.
type Side string

const (
	SideNew Side = "new"
	SideOld Side = "old"
)

// Position is the GitLab discussion position payload.
type Position struct {
	BaseSHA  string
	StartSHA string
	HeadSHA  string
	OldPath  string
	NewPath  string
	OldLine  int
	NewLine  int
}
