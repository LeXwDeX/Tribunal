package review

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
)

// DossierSchemaVersion is the persisted materials-snapshot version.
const DossierSchemaVersion = 1

// ErrInvalidDossier reports a dossier that cannot be persisted or replayed.
var ErrInvalidDossier = errors.New("review: invalid dossier")

// dossierSHA1RE and dossierSHA256RE accept the object IDs GitLab may return.
var (
	dossierSHA1RE   = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	dossierSHA256RE = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	secretKeyRE     = regexp.MustCompile(`(?i)(^|[._-])(token|secret|password|passwd|api[._-]?key|apikey|private[._-]?key|authorization|credential|bearer|access[._-]?key)([._-]|$)`)
)

// ProjectRef identifies the GitLab project without credentials.
type ProjectRef struct {
	ID     int64  `json:"id"`
	Path   string `json:"path"`
	WebURL string `json:"web_url,omitempty"`
}

// MRMeta is the sanitized merge-request metadata used by prompts.
type MRMeta struct {
	IID          int64    `json:"iid"`
	Title        string   `json:"title"`
	Description  string   `json:"description,omitempty"`
	Author       string   `json:"author,omitempty"`
	SourceBranch string   `json:"source_branch"`
	TargetBranch string   `json:"target_branch"`
	Draft        bool     `json:"draft,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	WebURL       string   `json:"web_url,omitempty"`
}

// FileChange is one changed file. Diff must already be sanitized by the
// caller; the dossier never reads credentials or environment values.
type FileChange struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"` // added | modified | deleted | renamed | copied
	Diff      string `json:"diff,omitempty"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Generated bool   `json:"generated,omitempty"`
}

// OmittedFile records one changed path that is absent from the materials
// snapshot. Reason is a stable reason code owned by the app collector.
type OmittedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// DiffSummary is the counts-only view of the MR diff index. Files counts
// paths included in the snapshot, not paths that were reviewed; callers must
// not use it as an "actually reviewed" count.
type DiffSummary struct {
	Files     int  `json:"files"`
	Additions int  `json:"additions"`
	Deletions int  `json:"deletions"`
	Truncated bool `json:"truncated,omitempty"`
	// Total is the number of changed paths seen before ignore rules.
	// Zero means the field is absent (older snapshots).
	Total int `json:"total,omitempty"`
	// Omitted lists paths missing from the snapshot and why. Ignored paths
	// are tracked separately in Dossier.Ignored and are not coverage gaps.
	Omitted []OmittedFile `json:"omitted,omitempty"`
}

// JobSummary is one failed CI job shown to the planner.
type JobSummary struct {
	Name          string `json:"name"`
	Stage         string `json:"stage,omitempty"`
	Status        string `json:"status"`
	FailureReason string `json:"failure_reason,omitempty"`
	WebURL        string `json:"web_url,omitempty"`
}

// PipelineSummary is the latest pipeline projection.
type PipelineSummary struct {
	ID         int64        `json:"id"`
	Status     string       `json:"status"`
	Ref        string       `json:"ref,omitempty"`
	SHA        string       `json:"sha,omitempty"`
	WebURL     string       `json:"web_url,omitempty"`
	FailedJobs []JobSummary `json:"failed_jobs,omitempty"`
}

// IssueRef is an issue closed by the merge request.
type IssueRef struct {
	IID    int64  `json:"iid"`
	Title  string `json:"title"`
	State  string `json:"state,omitempty"`
	WebURL string `json:"web_url,omitempty"`
}

// RepoDocs carries target-branch documents read by the app layer.
type RepoDocs struct {
	GitLabReviewConfig string `json:"gitlab_review_config,omitempty"`
	AGENTS             string `json:"agents,omitempty"`
	CLAUDE             string `json:"claude,omitempty"`
}

// IncrementalRange records the incremental review scope.
type IncrementalRange struct {
	LastHead string   `json:"last_head"`
	Head     string   `json:"head"`
	Files    []string `json:"files,omitempty"`
	Full     bool     `json:"full"`
	Reason   string   `json:"reason,omitempty"`
}

// Dossier is the sanitized, replayable materials snapshot for one workflow.
//
// SECURITY: callers must remove project tokens, provider keys, MCP secrets,
// and any other credentials before constructing a Dossier. ValidateDossier
// rejects secret-looking JSON keys as a second line of defense, but it does
// not inspect free-form document text.
type Dossier struct {
	SchemaVersion int               `json:"schema_version"`
	Project       ProjectRef        `json:"project"`
	MR            MRMeta            `json:"mr"`
	DiffVersion   gitlab.Version    `json:"diff_version"`
	DiffSummary   DiffSummary       `json:"diff_summary"`
	Files         []FileChange      `json:"files"`
	Ignored       []string          `json:"ignored,omitempty"`
	Pipeline      *PipelineSummary  `json:"pipeline,omitempty"`
	Issues        []IssueRef        `json:"issues,omitempty"`
	RepoDocs      RepoDocs          `json:"repo_docs,omitempty"`
	ContextDocs   string            `json:"context_docs,omitempty"`
	Language      string            `json:"language,omitempty"`
	CustomRules   []string          `json:"custom_rules,omitempty"`
	Incremental   *IncrementalRange `json:"incremental,omitempty"`
	PriorFindings []FindingOut      `json:"prior_findings,omitempty"`
	HeadDir       string            `json:"head_dir,omitempty"`
	BaseDir       string            `json:"base_dir,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	GeneratedAt   time.Time         `json:"generated_at"`
}

// DossierInput is the explicit, already-sanitized input to BuildDossier.
type DossierInput struct {
	Project       ProjectRef
	MR            MRMeta
	DiffVersion   gitlab.Version
	DiffSummary   DiffSummary
	Files         []FileChange
	Ignored       []string
	Pipeline      *PipelineSummary
	Issues        []IssueRef
	RepoDocs      RepoDocs
	ContextDocs   string
	Language      string
	CustomRules   []string
	Incremental   *IncrementalRange
	PriorFindings []FindingOut
	HeadDir       string
	BaseDir       string
	Metadata      map[string]string
	GeneratedAt   time.Time
}

// BuildDossier validates and snapshots explicit inputs. It performs no I/O
// and never collects credentials.
func BuildDossier(in DossierInput) (*Dossier, error) {
	generatedAt := in.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = time.Now().UTC()
	} else {
		generatedAt = generatedAt.UTC()
	}
	var files []FileChange
	if in.Files != nil {
		files = make([]FileChange, len(in.Files))
		copy(files, in.Files)
	}
	d := &Dossier{
		SchemaVersion: DossierSchemaVersion,
		Project:       in.Project,
		MR:            in.MR,
		DiffVersion:   in.DiffVersion,
		DiffSummary:   in.DiffSummary,
		Files:         files,
		Ignored:       append([]string(nil), in.Ignored...),
		Issues:        append([]IssueRef(nil), in.Issues...),
		RepoDocs:      in.RepoDocs,
		ContextDocs:   in.ContextDocs,
		Language:      in.Language,
		CustomRules:   append([]string(nil), in.CustomRules...),
		PriorFindings: append([]FindingOut(nil), in.PriorFindings...),
		HeadDir:       in.HeadDir,
		BaseDir:       in.BaseDir,
		Metadata:      cloneMetadata(in.Metadata),
		GeneratedAt:   generatedAt,
	}
	d.MR.Labels = append([]string(nil), in.MR.Labels...)
	if in.Pipeline != nil {
		pipeline := *in.Pipeline
		pipeline.FailedJobs = append([]JobSummary(nil), in.Pipeline.FailedJobs...)
		d.Pipeline = &pipeline
	}
	if in.Incremental != nil {
		incremental := *in.Incremental
		incremental.Files = append([]string(nil), in.Incremental.Files...)
		d.Incremental = &incremental
	}
	if err := ValidateDossier(d); err != nil {
		return nil, err
	}
	return d, nil
}

func cloneMetadata(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func dossierErr(field, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalidDossier, field, fmt.Sprintf(format, args...))
}

func validObjectID(value string) bool {
	return dossierSHA1RE.MatchString(value) || dossierSHA256RE.MatchString(value)
}

// ValidateDossier checks the schema version, required fields, path shape, and
// secret-looking JSON keys.
func ValidateDossier(d *Dossier) error {
	if d == nil {
		return dossierErr("dossier", "must not be nil")
	}
	if d.SchemaVersion != DossierSchemaVersion {
		return dossierErr("schema_version", "must be %d", DossierSchemaVersion)
	}
	if d.Project.ID <= 0 {
		return dossierErr("project.id", "must be positive")
	}
	if strings.TrimSpace(d.Project.Path) == "" {
		return dossierErr("project.path", "must not be empty")
	}
	if d.MR.IID <= 0 {
		return dossierErr("mr.iid", "must be positive")
	}
	if strings.TrimSpace(d.MR.Title) == "" {
		return dossierErr("mr.title", "must not be empty")
	}
	if strings.TrimSpace(d.MR.SourceBranch) == "" {
		return dossierErr("mr.source_branch", "must not be empty")
	}
	if strings.TrimSpace(d.MR.TargetBranch) == "" {
		return dossierErr("mr.target_branch", "must not be empty")
	}
	if !validObjectID(d.DiffVersion.HeadSHA) {
		return dossierErr("diff_version.head_sha", "must be a 40 or 64 character object ID")
	}
	if d.DiffVersion.BaseSHA != "" && !validObjectID(d.DiffVersion.BaseSHA) {
		return dossierErr("diff_version.base_sha", "must be a 40 or 64 character object ID")
	}
	if d.DiffVersion.StartSHA != "" && !validObjectID(d.DiffVersion.StartSHA) {
		return dossierErr("diff_version.start_sha", "must be a 40 or 64 character object ID")
	}
	if d.Files == nil {
		return dossierErr("files", "is required (use [] for an empty change set)")
	}
	seen := make(map[string]struct{}, len(d.Files))
	for i, file := range d.Files {
		field := fmt.Sprintf("files[%d]", i)
		if err := checkRepoPath(field+".path", file.Path, true); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidDossier, err)
		}
		if _, ok := seen[file.Path]; ok {
			return dossierErr(field+".path", "duplicate file")
		}
		seen[file.Path] = struct{}{}
		switch file.Status {
		case "added", "new", "modified", "deleted", "renamed", "copied":
		default:
			return dossierErr(field+".status", "must be added, modified, deleted, renamed, or copied")
		}
		if file.OldPath != "" {
			if err := checkRepoPath(field+".old_path", file.OldPath, false); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidDossier, err)
			}
		}
		if file.Additions < 0 || file.Deletions < 0 {
			return dossierErr(field, "line counts must not be negative")
		}
	}
	for i, ignored := range d.Ignored {
		if err := checkRepoPath(fmt.Sprintf("ignored[%d]", i), ignored, true); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidDossier, err)
		}
	}
	if d.DiffSummary.Files < 0 || d.DiffSummary.Additions < 0 || d.DiffSummary.Deletions < 0 ||
		d.DiffSummary.Total < 0 {
		return dossierErr("diff_summary", "counts must not be negative")
	}
	if d.DiffSummary.Total > 0 && d.DiffSummary.Files > d.DiffSummary.Total {
		return dossierErr("diff_summary.files", "must not exceed total")
	}
	for i, omitted := range d.DiffSummary.Omitted {
		field := fmt.Sprintf("diff_summary.omitted[%d]", i)
		if err := checkRepoPath(field+".path", omitted.Path, true); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidDossier, err)
		}
		if err := checkOneLine(field+".reason", omitted.Reason, 64, true); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidDossier, err)
		}
	}
	if d.Pipeline != nil {
		if strings.TrimSpace(d.Pipeline.Status) == "" {
			return dossierErr("pipeline.status", "must not be empty")
		}
	}
	for i, issue := range d.Issues {
		if issue.IID <= 0 {
			return dossierErr(fmt.Sprintf("issues[%d].iid", i), "must be positive")
		}
		if strings.TrimSpace(issue.Title) == "" {
			return dossierErr(fmt.Sprintf("issues[%d].title", i), "must not be empty")
		}
	}
	if d.Incremental != nil {
		if d.Incremental.LastHead != "" && !validObjectID(d.Incremental.LastHead) {
			return dossierErr("incremental.last_head", "must be a 40 or 64 character object ID")
		}
		if !validObjectID(d.Incremental.Head) {
			return dossierErr("incremental.head", "must be a 40 or 64 character object ID")
		}
		seenFiles := make(map[string]struct{}, len(d.Incremental.Files))
		for i, file := range d.Incremental.Files {
			if err := checkRepoPath(fmt.Sprintf("incremental.files[%d]", i), file, true); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidDossier, err)
			}
			if _, ok := seenFiles[file]; ok {
				return dossierErr(fmt.Sprintf("incremental.files[%d]", i), "duplicate file")
			}
			seenFiles[file] = struct{}{}
		}
	}
	if err := checkOneLine("language", d.Language, 128, false); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidDossier, err)
	}
	for i, finding := range d.PriorFindings {
		if err := finding.Validate(); err != nil {
			return fmt.Errorf("%w: prior_findings[%d]: %v", ErrInvalidDossier, i, err)
		}
	}
	for key := range d.Metadata {
		if strings.TrimSpace(key) == "" {
			return dossierErr("metadata", "keys must not be empty")
		}
	}
	if d.GeneratedAt.IsZero() {
		return dossierErr("generated_at", "must not be zero")
	}
	if err := scanSecretKeys(d); err != nil {
		return err
	}
	return nil
}

// scanSecretKeys walks the serialized dossier and rejects secret-looking keys.
func scanSecretKeys(d *Dossier) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("%w: marshal: %v", ErrInvalidDossier, err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("%w: inspect keys: %v", ErrInvalidDossier, err)
	}
	return walkSecretKeys("$", value)
}

func walkSecretKeys(path string, value any) error {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			field := path + "." + key
			if secretKeyRE.MatchString(key) {
				return dossierErr(field, "secret-looking keys are not allowed")
			}
			if err := walkSecretKeys(field, typed[key]); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range typed {
			if err := walkSecretKeys(fmt.Sprintf("%s[%d]", path, i), item); err != nil {
				return err
			}
		}
	}
	return nil
}

// MarshalDossier validates then marshals the snapshot deterministically.
func MarshalDossier(d *Dossier) ([]byte, error) {
	if err := ValidateDossier(d); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal: %v", ErrInvalidDossier, err)
	}
	return raw, nil
}

// DossierHash returns the SHA-256 of the canonical dossier JSON.
func DossierHash(d *Dossier) (string, error) {
	raw, err := MarshalDossier(d)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// IncrementalInput configures the pure incremental-scope helper. Git access
// stays in the caller: provide IsAncestor, ChangedFiles, or LoadChanged.
type IncrementalInput struct {
	LastHead     string
	Head         string
	MRFiles      []string
	IsAncestor   bool
	ForceFull    bool
	ChangedFiles []string
	LoadChanged  func(from, to string) ([]string, error)
}

// IncrementalScope is the pure result of the incremental-scope computation.
type IncrementalScope struct {
	Full   bool     `json:"full"`
	Files  []string `json:"files,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

// ComputeIncrementalScope intersects last_head..head files with the merge
// request's changed files. Any non-ancestor or missing last head is a full
// review. The helper never runs git.
func ComputeIncrementalScope(in IncrementalInput) (IncrementalScope, error) {
	full := func(reason string) (IncrementalScope, error) {
		return IncrementalScope{Full: true, Files: dedupePaths(in.MRFiles), Reason: reason}, nil
	}
	if strings.TrimSpace(in.Head) == "" {
		return IncrementalScope{}, dossierErr("head", "must not be empty")
	}
	if !validObjectID(in.Head) {
		return IncrementalScope{}, dossierErr("head", "must be an object ID")
	}
	if in.ForceFull {
		return full("forced full review")
	}
	if in.LastHead == "" || strings.EqualFold(in.LastHead, in.Head) {
		return full("no previous reviewed head")
	}
	if !validObjectID(in.LastHead) {
		return IncrementalScope{}, dossierErr("last_head", "must be an object ID")
	}
	if !in.IsAncestor {
		return full("previous head is not an ancestor of the current head")
	}
	var changed []string
	switch {
	case in.LoadChanged != nil:
		loaded, err := in.LoadChanged(in.LastHead, in.Head)
		if err != nil {
			return IncrementalScope{}, fmt.Errorf("review: load incremental files: %w", err)
		}
		changed = loaded
	case in.ChangedFiles != nil:
		changed = in.ChangedFiles
	default:
		return IncrementalScope{}, dossierErr("changed_files", "is required when the previous head is an ancestor")
	}
	mr := make(map[string]struct{}, len(in.MRFiles))
	for _, file := range in.MRFiles {
		if cleaned, ok := cleanScopePath(file); ok {
			mr[cleaned] = struct{}{}
		}
	}
	files := make([]string, 0, len(changed))
	seen := make(map[string]struct{}, len(changed))
	for _, file := range changed {
		cleaned, ok := cleanScopePath(file)
		if !ok {
			continue
		}
		if _, inMR := mr[cleaned]; !inMR {
			continue
		}
		if _, exists := seen[cleaned]; exists {
			continue
		}
		seen[cleaned] = struct{}{}
		files = append(files, cleaned)
	}
	sort.Strings(files)
	return IncrementalScope{Files: files}, nil
}

func cleanScopePath(value string) (string, bool) {
	if err := checkRepoPath("file", value, true); err != nil {
		return "", false
	}
	return value, true
}

func dedupePaths(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
