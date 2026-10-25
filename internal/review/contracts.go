package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/invopop/jsonschema"
)

// Severity is the normalized finding severity used by code and contracts.
type Severity string

const (
	SeverityP0   Severity = "P0"
	SeverityP1   Severity = "P1"
	SeverityP2   Severity = "P2"
	SeverityInfo Severity = "info"
)

// Confidence is the proposer's confidence.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

var severityRank = map[Severity]int{
	SeverityP0:   0,
	SeverityP1:   1,
	SeverityP2:   2,
	SeverityInfo: 3,
}

// Valid reports whether s is one of P0, P1, P2, or info.
func (s Severity) Valid() bool {
	_, ok := severityRank[s]
	return ok
}

// Higher reports whether s is more severe than other.
func (s Severity) Higher(other Severity) bool {
	a, okA := severityRank[s]
	b, okB := severityRank[other]
	return okA && okB && a < b
}

const (
	MaxTitleRunes      = 120
	MaxPathBytes       = 1024
	MaxBodyRunes       = 8000
	MaxEvidenceRunes   = 8000
	MaxSuggestionRunes = 8000
	MaxSummaryRunes    = 3000
	MaxReasonRunes     = 4000
	MaxFindings        = 200
	DefaultMaxPackages = 6
	HardMaxPackages    = 100
	DefaultMaxFiles    = 200
	HardMaxFiles       = 200
	HardMaxExtends     = 2
)

// ValidationError identifies the failing contract field.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// FindingOut is one proposer finding.
type FindingOut struct {
	Path       string     `json:"path" jsonschema:"maxLength=1024"`
	Line       int        `json:"line,omitempty" jsonschema:"minimum=0"`
	Side       string     `json:"side,omitempty" jsonschema:"enum=new,enum=old"`
	Severity   Severity   `json:"severity" jsonschema:"enum=P0,enum=P1,enum=P2,enum=info"`
	Category   string     `json:"category" jsonschema:"pattern=^[a-z][a-z0-9_-]{0,31}$"`
	Title      string     `json:"title" jsonschema:"minLength=1,maxLength=120"`
	Body       string     `json:"body" jsonschema:"minLength=1,maxLength=8000,description=Structured Markdown with short paragraphs, lists, fenced code blocks for code or commands, and inline code for identifiers."`
	Suggestion string     `json:"suggestion,omitempty" jsonschema:"maxLength=8000,description=Structured Markdown with fenced code blocks for code or commands and inline code for identifiers."`
	Evidence   string     `json:"evidence" jsonschema:"minLength=1,maxLength=8000"`
	Confidence Confidence `json:"confidence" jsonschema:"enum=high,enum=medium,enum=low"`
}

// CodeRef is a concrete code location used as verification evidence.
type CodeRef struct {
	Path      string `json:"path" jsonschema:"minLength=1,maxLength=1024"`
	StartLine int    `json:"start_line" jsonschema:"minimum=1"`
	EndLine   int    `json:"end_line" jsonschema:"minimum=1"`
	Root      string `json:"root,omitempty" jsonschema:"enum=head,enum=base"`
	Note      string `json:"note" jsonschema:"minLength=1,maxLength=2000"`
}

// PlanOut is the agent plan contract.
type PlanOut struct {
	Risk          string     `json:"risk" jsonschema:"enum=low,enum=medium,enum=high"`
	RiskReasons   []string   `json:"risk_reasons" jsonschema:"minItems=1,maxItems=20"`
	Packages      []Package  `json:"packages" jsonschema:"minItems=1,maxItems=100"`
	RunReasoner   bool       `json:"run_reasoner"`
	ChallengeOnly []Severity `json:"challenge_only" jsonschema:"minItems=1,maxItems=4,enum=P0,enum=P1,enum=P2,enum=info"`
}

// Package is one planned review package.
type Package struct {
	ID       string   `json:"id" jsonschema:"pattern=^[a-z0-9_-]{1,32}$"`
	Blocks   []string `json:"blocks" jsonschema:"minItems=1,maxItems=2,enum=code-review,enum=review"`
	Files    []string `json:"files" jsonschema:"minItems=1,maxItems=200"`
	Focus    string   `json:"focus" jsonschema:"minLength=1,maxLength=2000"`
	Provider string   `json:"provider,omitempty"`
}

// JSONSchemaExtend fixes the package id pattern that contains a comma.
func (Package) JSONSchemaExtend(schema *jsonschema.Schema) {
	if schema != nil && schema.Properties != nil {
		if property, ok := schema.Properties.Get("id"); ok {
			property.Pattern = packageIDRE.String()
		}
	}
}

// PlanPolicy is the profile-authorized context for a plan.
type PlanPolicy struct {
	AllowedBlocks      []string
	AllowedProviders   []string
	ChangedFiles       []string
	MaxPackages        int
	MaxFilesPerPackage int
	AllowReasoner      bool
}

// JSONSchemaExtend fixes patterns that contain commas; invopop/jsonschema
// treats commas in struct tags as option separators.
func (FindingOut) JSONSchemaExtend(schema *jsonschema.Schema) {
	if schema != nil && schema.Properties != nil {
		if property, ok := schema.Properties.Get("category"); ok {
			property.Pattern = categoryRE.String()
		}
	}
}

// CodeReviewOut is the code-review node contract.
type CodeReviewOut struct {
	Summary  string       `json:"summary" jsonschema:"minLength=1,maxLength=3000"`
	Findings []FindingOut `json:"findings" jsonschema:"maxItems=200"`
}

// ReviewOut is a standards or intent review channel.
type ReviewOut struct {
	Verdict        string       `json:"verdict" jsonschema:"enum=PASS,enum=BLOCKING"`
	EvidenceStatus string       `json:"evidence_status" jsonschema:"enum=sufficient,enum=insufficient"`
	Findings       []FindingOut `json:"findings" jsonschema:"maxItems=200"`
	EvidenceGaps   []string     `json:"evidence_gaps" jsonschema:"maxItems=100"`
	Summary        string       `json:"summary" jsonschema:"minLength=1,maxLength=3000"`
}

// ReviewGateOut is the deterministic gate over the two review channels.
type ReviewGateOut struct {
	Verdict  string  `json:"verdict" jsonschema:"enum=PASS,enum=BLOCKING"`
	Accepted []Ref   `json:"accepted" jsonschema:"maxItems=500"`
	Rebutted []Rebut `json:"rebutted" jsonschema:"maxItems=500"`
	Summary  string  `json:"summary" jsonschema:"minLength=1,maxLength=3000"`
}

// Ref identifies one finding in a node output.
type Ref struct {
	Node  string `json:"node" jsonschema:"minLength=1"`
	Index int    `json:"index" jsonschema:"minimum=0"`
}

// Rebut rejects one source finding with a reason.
type Rebut struct {
	Ref
	Reason string `json:"reason" jsonschema:"minLength=1,maxLength=4000"`
}

// ReasonerOut is the design-reasoning node contract.
type ReasonerOut struct {
	ResultType string            `json:"result_type" jsonschema:"enum=CLEAN,enum=FINDINGS,enum=INSUFFICIENT_SUBJECT"`
	Findings   []ReasonerFinding `json:"findings" jsonschema:"maxItems=200"`
}

// ReasonerFinding keeps the reasoner's native severity names.
type ReasonerFinding struct {
	Severity    string     `json:"severity" jsonschema:"enum=critical,enum=notable,enum=informational"`
	Dimension   string     `json:"dimension" jsonschema:"minLength=1,maxLength=64"`
	Finding     string     `json:"finding" jsonschema:"minLength=1,maxLength=2000"`
	Where       string     `json:"where" jsonschema:"minLength=1,maxLength=2000"`
	Why         string     `json:"why" jsonschema:"minLength=1,maxLength=4000"`
	Assumptions []string   `json:"assumptions" jsonschema:"maxItems=50"`
	Confidence  Confidence `json:"confidence" jsonschema:"enum=high,enum=medium,enum=low"`
	Path        string     `json:"path,omitempty" jsonschema:"maxLength=1024"`
	Line        int        `json:"line,omitempty" jsonschema:"minimum=0"`
}

// VerifyOut is one challenger verdict.
type VerifyOut struct {
	Status            string    `json:"status" jsonschema:"enum=CONFIRMED,enum=REFUTED,enum=UNCERTAIN"`
	Reason            string    `json:"reason" jsonschema:"minLength=1,maxLength=4000"`
	Evidence          []CodeRef `json:"evidence" jsonschema:"maxItems=20"`
	SuggestedSeverity Severity  `json:"suggested_severity,omitempty" jsonschema:"enum=P0,enum=P1,enum=P2,enum=info"`
}

// SynthesizeOut is the final agent decision contract.
type SynthesizeOut struct {
	Verdict        string          `json:"verdict" jsonschema:"enum=PASS,enum=BLOCKING"`
	Summary        string          `json:"summary" jsonschema:"minLength=1,maxLength=3000"`
	Decisions      []Decision      `json:"decisions" jsonschema:"maxItems=500"`
	NewFindings    []FindingOut    `json:"new_findings" jsonschema:"maxItems=20"`
	ExtendRequests []ExtendRequest `json:"extend_requests" jsonschema:"maxItems=2"`
}

// Decision is the agent's explicit decision for one grouped finding.
type Decision struct {
	FindingID string   `json:"finding_id" jsonschema:"minLength=1"`
	Action    string   `json:"action" jsonschema:"enum=publish,enum=drop,enum=mark,enum=downgrade"`
	Severity  Severity `json:"severity,omitempty" jsonschema:"enum=P0,enum=P1,enum=P2,enum=info"`
	Reason    string   `json:"reason" jsonschema:"minLength=1,maxLength=4000"`
}

// ExtendRequest asks the engine for one extra challenge or review node.
//
// NewFindingIndex is an extension to the docs04 contract. It is a zero-based
// index into SynthesizeOut.NewFindings. A verify request uses exactly one of
// FindingID (an existing group) or NewFindingIndex (a new main finding).
type ExtendRequest struct {
	Kind            string   `json:"kind" jsonschema:"enum=verify,enum=review"`
	FindingID       string   `json:"finding_id,omitempty"`
	NewFindingIndex *int     `json:"new_finding_index,omitempty" jsonschema:"minimum=0"`
	Files           []string `json:"files,omitempty" jsonschema:"maxItems=200"`
	Focus           string   `json:"focus" jsonschema:"minLength=1,maxLength=2000"`
}

// OnboardingOut is the project context contract.
type OnboardingOut struct {
	Overview     string   `json:"overview" jsonschema:"minLength=1,maxLength=4000"`
	Architecture string   `json:"architecture" jsonschema:"minLength=1,maxLength=8000"`
	Modules      []Module `json:"modules" jsonschema:"maxItems=200"`
	Conventions  []string `json:"conventions" jsonschema:"maxItems=100"`
	BuildAndTest []string `json:"build_and_test" jsonschema:"maxItems=100"`
	RiskHotspots []string `json:"risk_hotspots" jsonschema:"maxItems=100"`
	ReviewFocus  []string `json:"review_focus" jsonschema:"maxItems=100"`
}

// Module is one repository module in the onboarding result.
type Module struct {
	Path    string `json:"path" jsonschema:"minLength=1,maxLength=1024"`
	Purpose string `json:"purpose" jsonschema:"minLength=1,maxLength=1000"`
}

var (
	categoryRE   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	packageIDRE  = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	controlChars = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)
)

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func checkText(field, value string, max int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return invalid(field, "must not be empty")
	}
	if runeLen(value) > max {
		return invalid(field, "must be at most %d characters", max)
	}
	if controlChars.MatchString(value) {
		return invalid(field, "contains control characters")
	}
	return nil
}

func checkOneLine(field, value string, max int, required bool) error {
	if err := checkText(field, value, max, required); err != nil {
		return err
	}
	if strings.ContainsAny(value, "\r\n") {
		return invalid(field, "must be one line")
	}
	return nil
}

func checkRepoPath(field, value string, required bool) error {
	if value == "" {
		if required {
			return invalid(field, "must not be empty")
		}
		return nil
	}
	if len(value) > MaxPathBytes {
		return invalid(field, "must be at most %d bytes", MaxPathBytes)
	}
	if strings.ContainsAny(value, "\\\x00\r\n") {
		return invalid(field, "must be a clean repository-relative path")
	}
	if strings.HasPrefix(value, "/") {
		return invalid(field, "must be repository-relative")
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == "." || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return invalid(field, "must be a clean repository-relative path")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return invalid(field, "must be a clean repository-relative path")
		}
	}
	return nil
}

func (f FindingOut) Validate() error {
	if err := checkRepoPath("path", f.Path, false); err != nil {
		return err
	}
	if f.Line < 0 {
		return invalid("line", "must be zero or positive")
	}
	if f.Line > 0 && f.Path == "" {
		return invalid("path", "is required when line is set")
	}
	if f.Side != "" && f.Side != "new" && f.Side != "old" {
		return invalid("side", "must be new or old")
	}
	if !f.Severity.Valid() {
		return invalid("severity", "must be P0, P1, P2, or info")
	}
	if !categoryRE.MatchString(f.Category) {
		return invalid("category", "must match %s", categoryRE.String())
	}
	if err := checkOneLine("title", f.Title, MaxTitleRunes, true); err != nil {
		return err
	}
	if err := checkText("body", f.Body, MaxBodyRunes, true); err != nil {
		return err
	}
	if err := checkText("suggestion", f.Suggestion, MaxSuggestionRunes, false); err != nil {
		return err
	}
	if err := checkText("evidence", f.Evidence, MaxEvidenceRunes, true); err != nil {
		return err
	}
	switch f.Confidence {
	case ConfidenceHigh, ConfidenceMedium, ConfidenceLow:
	default:
		return invalid("confidence", "must be high, medium, or low")
	}
	return nil
}

func (r CodeRef) Validate() error {
	if err := checkRepoPath("path", r.Path, true); err != nil {
		return err
	}
	if r.StartLine <= 0 {
		return invalid("start_line", "must be positive")
	}
	if r.EndLine < r.StartLine {
		return invalid("end_line", "must not be before start_line")
	}
	if r.Root != "" && r.Root != "head" && r.Root != "base" {
		return invalid("root", "must be head or base")
	}
	if err := checkText("note", r.Note, 2000, true); err != nil {
		return err
	}
	return nil
}

// HasValidCodeEvidence reports whether at least one evidence reference is a
// syntactically valid code location. Non-empty prose alone is not a
// refutation. Snapshot-backed callers must use ValidCodeEvidence instead.
func HasValidCodeEvidence(refs []CodeRef) bool {
	return ValidCodeEvidence(refs, nil)
}

func (p Package) Validate() error {
	if !packageIDRE.MatchString(p.ID) {
		return invalid("id", "must match %s", packageIDRE.String())
	}
	if p.Blocks == nil {
		return invalid("blocks", "is required (use [] only if intentionally empty)")
	}
	if len(p.Blocks) == 0 || len(p.Blocks) > 2 {
		return invalid("blocks", "must contain 1 to 2 entries")
	}
	seenBlocks := make(map[string]struct{}, len(p.Blocks))
	for i, block := range p.Blocks {
		if block != "code-review" && block != "review" {
			return invalid(fmt.Sprintf("blocks[%d]", i), "must be code-review or review")
		}
		if _, ok := seenBlocks[block]; ok {
			return invalid(fmt.Sprintf("blocks[%d]", i), "duplicate block")
		}
		seenBlocks[block] = struct{}{}
	}
	if p.Files == nil {
		return invalid("files", "is required (use [] only if intentionally empty)")
	}
	if len(p.Files) == 0 || len(p.Files) > HardMaxFiles {
		return invalid("files", "must contain 1 to %d entries", HardMaxFiles)
	}
	seenFiles := make(map[string]struct{}, len(p.Files))
	for i, file := range p.Files {
		if err := checkRepoPath(fmt.Sprintf("files[%d]", i), file, true); err != nil {
			return err
		}
		if _, ok := seenFiles[file]; ok {
			return invalid(fmt.Sprintf("files[%d]", i), "duplicate file")
		}
		seenFiles[file] = struct{}{}
	}
	if err := checkText("focus", p.Focus, 2000, true); err != nil {
		return err
	}
	if p.Provider != "" {
		if err := checkOneLine("provider", p.Provider, 128, true); err != nil {
			return err
		}
	}
	return nil
}

func (p PlanOut) Validate() error {
	switch p.Risk {
	case "low", "medium", "high":
	default:
		return invalid("risk", "must be low, medium, or high")
	}
	if p.RiskReasons == nil {
		return invalid("risk_reasons", "is required")
	}
	if len(p.RiskReasons) == 0 || len(p.RiskReasons) > 20 {
		return invalid("risk_reasons", "must contain 1 to 20 entries")
	}
	for i, reason := range p.RiskReasons {
		if err := checkText(fmt.Sprintf("risk_reasons[%d]", i), reason, 1000, true); err != nil {
			return err
		}
	}
	if p.Packages == nil {
		return invalid("packages", "is required")
	}
	if len(p.Packages) == 0 || len(p.Packages) > HardMaxPackages {
		return invalid("packages", "must contain 1 to %d entries", HardMaxPackages)
	}
	seen := make(map[string]struct{}, len(p.Packages))
	for i, pkg := range p.Packages {
		if err := pkg.Validate(); err != nil {
			return wrapField(fmt.Sprintf("packages[%d]", i), err)
		}
		if _, ok := seen[pkg.ID]; ok {
			return invalid(fmt.Sprintf("packages[%d].id", i), "duplicate package id")
		}
		seen[pkg.ID] = struct{}{}
	}
	if p.ChallengeOnly == nil {
		return invalid("challenge_only", "is required")
	}
	if len(p.ChallengeOnly) == 0 || len(p.ChallengeOnly) > 4 {
		return invalid("challenge_only", "must contain 1 to 4 severities")
	}
	seenSeverity := make(map[Severity]struct{}, len(p.ChallengeOnly))
	for i, severity := range p.ChallengeOnly {
		if !severity.Valid() {
			return invalid(fmt.Sprintf("challenge_only[%d]", i), "invalid severity")
		}
		if _, ok := seenSeverity[severity]; ok {
			return invalid(fmt.Sprintf("challenge_only[%d]", i), "duplicate severity")
		}
		seenSeverity[severity] = struct{}{}
	}
	return nil
}

// ValidatePolicy checks the plan against profile-authorized blocks,
// providers, changed files, and hard budget bounds.
func (p PlanOut) ValidatePolicy(policy PlanPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if policy.MaxPackages < 0 || policy.MaxPackages > HardMaxPackages {
		return invalid("max_packages", "must be between 0 and %d", HardMaxPackages)
	}
	if policy.MaxFilesPerPackage < 0 || policy.MaxFilesPerPackage > HardMaxFiles {
		return invalid("max_files_per_package", "must be between 0 and %d", HardMaxFiles)
	}
	maxPackages := policy.MaxPackages
	if maxPackages == 0 {
		maxPackages = DefaultMaxPackages
	}
	if len(p.Packages) > maxPackages {
		return invalid("packages", "exceeds max_packages %d", maxPackages)
	}
	maxFiles := policy.MaxFilesPerPackage
	if maxFiles == 0 {
		maxFiles = DefaultMaxFiles
	}
	blocks := stringSet(policy.AllowedBlocks)
	providers := stringSet(policy.AllowedProviders)
	changed := stringSet(policy.ChangedFiles)
	for i, pkg := range p.Packages {
		if len(pkg.Files) > maxFiles {
			return invalid(fmt.Sprintf("packages[%d].files", i), "exceeds max files per package %d", maxFiles)
		}
		for j, block := range pkg.Blocks {
			if _, ok := blocks[block]; !ok {
				return invalid(fmt.Sprintf("packages[%d].blocks[%d]", i, j), "block %q is not authorized", block)
			}
		}
		for j, file := range pkg.Files {
			if _, ok := changed[file]; !ok {
				return invalid(fmt.Sprintf("packages[%d].files[%d]", i, j), "file is not in the changed set")
			}
		}
		if pkg.Provider != "" {
			if _, ok := providers[pkg.Provider]; !ok {
				return invalid(fmt.Sprintf("packages[%d].provider", i), "provider %q is not authorized", pkg.Provider)
			}
		}
	}
	if p.RunReasoner && !policy.AllowReasoner {
		return invalid("run_reasoner", "reasoner is not authorized by the profile")
	}
	return nil
}

// ValidatePlan is the named entry point for PlanOut.ValidatePolicy.
func ValidatePlan(p PlanOut, policy PlanPolicy) error { return p.ValidatePolicy(policy) }

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			set[value] = struct{}{}
		}
	}
	return set
}

func (c CodeReviewOut) Validate() error {
	if err := checkText("summary", c.Summary, MaxSummaryRunes, true); err != nil {
		return err
	}
	if c.Findings == nil {
		return invalid("findings", "is required")
	}
	if len(c.Findings) > MaxFindings {
		return invalid("findings", "must contain at most %d entries", MaxFindings)
	}
	for i, finding := range c.Findings {
		if err := finding.Validate(); err != nil {
			return wrapField(fmt.Sprintf("findings[%d]", i), err)
		}
	}
	return nil
}

func (r ReviewOut) Validate() error {
	switch r.Verdict {
	case "PASS", "BLOCKING":
	default:
		return invalid("verdict", "must be PASS or BLOCKING")
	}
	switch r.EvidenceStatus {
	case "sufficient", "insufficient":
	default:
		return invalid("evidence_status", "must be sufficient or insufficient")
	}
	if r.Findings == nil {
		return invalid("findings", "is required")
	}
	if len(r.Findings) > MaxFindings {
		return invalid("findings", "must contain at most %d entries", MaxFindings)
	}
	for i, finding := range r.Findings {
		if err := finding.Validate(); err != nil {
			return wrapField(fmt.Sprintf("findings[%d]", i), err)
		}
	}
	if r.EvidenceGaps == nil {
		return invalid("evidence_gaps", "is required")
	}
	if len(r.EvidenceGaps) > 100 {
		return invalid("evidence_gaps", "must contain at most 100 entries")
	}
	for i, gap := range r.EvidenceGaps {
		if err := checkText(fmt.Sprintf("evidence_gaps[%d]", i), gap, 2000, true); err != nil {
			return err
		}
	}
	return checkText("summary", r.Summary, MaxSummaryRunes, true)
}

func (r Ref) Validate() error {
	if err := checkOneLine("node", r.Node, 128, true); err != nil {
		return err
	}
	if r.Index < 0 {
		return invalid("index", "must be zero or positive")
	}
	return nil
}

func (r Rebut) Validate() error {
	if err := r.Ref.Validate(); err != nil {
		return err
	}
	return checkText("reason", r.Reason, MaxReasonRunes, true)
}

func (g ReviewGateOut) Validate() error {
	switch g.Verdict {
	case "PASS", "BLOCKING":
	default:
		return invalid("verdict", "must be PASS or BLOCKING")
	}
	if g.Accepted == nil {
		return invalid("accepted", "is required")
	}
	if g.Rebutted == nil {
		return invalid("rebutted", "is required")
	}
	if len(g.Accepted)+len(g.Rebutted) > 500 {
		return invalid("accepted", "combined references exceed 500")
	}
	for i, ref := range g.Accepted {
		if err := ref.Validate(); err != nil {
			return wrapField(fmt.Sprintf("accepted[%d]", i), err)
		}
	}
	for i, rebut := range g.Rebutted {
		if err := rebut.Validate(); err != nil {
			return wrapField(fmt.Sprintf("rebutted[%d]", i), err)
		}
	}
	return checkText("summary", g.Summary, MaxSummaryRunes, true)
}

// ValidateReviewGate requires accepted and rebutted refs to exactly cover the
// source finding refs. Duplicates and invented refs fail.
func ValidateReviewGate(out ReviewGateOut, source []Ref) error {
	if err := out.Validate(); err != nil {
		return err
	}
	want := make(map[Ref]struct{}, len(source))
	for i, ref := range source {
		if err := ref.Validate(); err != nil {
			return wrapField(fmt.Sprintf("source[%d]", i), err)
		}
		if _, ok := want[ref]; ok {
			return invalid(fmt.Sprintf("source[%d]", i), "duplicate source ref")
		}
		want[ref] = struct{}{}
	}
	seen := make(map[Ref]string, len(source))
	for i, ref := range out.Accepted {
		if _, ok := want[ref]; !ok {
			return invalid(fmt.Sprintf("accepted[%d]", i), "ref does not exist in source")
		}
		if _, ok := seen[ref]; ok {
			return invalid(fmt.Sprintf("accepted[%d]", i), "ref appears more than once")
		}
		seen[ref] = "accepted"
	}
	for i, rebut := range out.Rebutted {
		if _, ok := want[rebut.Ref]; !ok {
			return invalid(fmt.Sprintf("rebutted[%d]", i), "ref does not exist in source")
		}
		if _, ok := seen[rebut.Ref]; ok {
			return invalid(fmt.Sprintf("rebutted[%d]", i), "ref appears more than once")
		}
		seen[rebut.Ref] = "rebutted"
	}
	for ref := range want {
		if _, ok := seen[ref]; !ok {
			return invalid("accepted", "source ref %s[%d] is missing", ref.Node, ref.Index)
		}
	}
	return nil
}

func (f ReasonerFinding) Validate() error {
	switch f.Severity {
	case "critical", "notable", "informational":
	default:
		return invalid("severity", "must be critical, notable, or informational")
	}
	if err := checkOneLine("dimension", f.Dimension, 64, true); err != nil {
		return err
	}
	if err := checkText("finding", f.Finding, 2000, true); err != nil {
		return err
	}
	if err := checkText("where", f.Where, 2000, true); err != nil {
		return err
	}
	if err := checkText("why", f.Why, 4000, true); err != nil {
		return err
	}
	if f.Assumptions == nil {
		return invalid("assumptions", "is required")
	}
	if len(f.Assumptions) > 50 {
		return invalid("assumptions", "must contain at most 50 entries")
	}
	for i, assumption := range f.Assumptions {
		if err := checkText(fmt.Sprintf("assumptions[%d]", i), assumption, 2000, true); err != nil {
			return err
		}
	}
	switch f.Confidence {
	case ConfidenceHigh, ConfidenceMedium, ConfidenceLow:
	default:
		return invalid("confidence", "must be high, medium, or low")
	}
	if err := checkRepoPath("path", f.Path, false); err != nil {
		return err
	}
	if f.Line < 0 {
		return invalid("line", "must be zero or positive")
	}
	if f.Line > 0 && f.Path == "" {
		return invalid("path", "is required when line is set")
	}
	return nil
}

func (r ReasonerOut) Validate() error {
	switch r.ResultType {
	case "CLEAN", "FINDINGS", "INSUFFICIENT_SUBJECT":
	default:
		return invalid("result_type", "must be CLEAN, FINDINGS, or INSUFFICIENT_SUBJECT")
	}
	if r.Findings == nil {
		return invalid("findings", "is required")
	}
	if len(r.Findings) > MaxFindings {
		return invalid("findings", "must contain at most %d entries", MaxFindings)
	}
	for i, finding := range r.Findings {
		if err := finding.Validate(); err != nil {
			return wrapField(fmt.Sprintf("findings[%d]", i), err)
		}
	}
	switch r.ResultType {
	case "CLEAN":
		if len(r.Findings) != 0 {
			return invalid("findings", "must be empty for CLEAN")
		}
	case "FINDINGS":
		if len(r.Findings) == 0 {
			return invalid("findings", "must not be empty for FINDINGS")
		}
	}
	return nil
}

func (v VerifyOut) Validate() error {
	switch v.Status {
	case "CONFIRMED", "REFUTED", "UNCERTAIN":
	default:
		return invalid("status", "must be CONFIRMED, REFUTED, or UNCERTAIN")
	}
	if err := checkText("reason", v.Reason, MaxReasonRunes, true); err != nil {
		return err
	}
	if v.Evidence == nil {
		return invalid("evidence", "is required")
	}
	if len(v.Evidence) > 20 {
		return invalid("evidence", "must contain at most 20 entries")
	}
	for i, ref := range v.Evidence {
		if err := ref.Validate(); err != nil {
			return wrapField(fmt.Sprintf("evidence[%d]", i), err)
		}
	}
	if v.SuggestedSeverity != "" && !v.SuggestedSeverity.Valid() {
		return invalid("suggested_severity", "invalid severity")
	}
	return nil
}

// NormalizeVerify downgrades REFUTED without a valid code location to
// UNCERTAIN. A non-empty arbitrary note is not enough.
func NormalizeVerify(out VerifyOut) VerifyOut {
	if out.Status == "REFUTED" && !HasValidCodeEvidence(out.Evidence) {
		out.Status = "UNCERTAIN"
	}
	return out
}

func (d Decision) Validate() error {
	if err := checkOneLine("finding_id", d.FindingID, 128, true); err != nil {
		return err
	}
	switch d.Action {
	case "publish", "drop", "mark", "downgrade":
	default:
		return invalid("action", "must be publish, drop, mark, or downgrade")
	}
	if err := checkText("reason", d.Reason, MaxReasonRunes, true); err != nil {
		return err
	}
	if d.Severity != "" && !d.Severity.Valid() {
		return invalid("severity", "invalid severity")
	}
	if d.Action == "downgrade" && d.Severity == "" {
		return invalid("severity", "is required for downgrade")
	}
	return nil
}

func (e ExtendRequest) Validate() error {
	switch e.Kind {
	case "verify":
		if (e.FindingID == "") == (e.NewFindingIndex == nil) {
			return invalid("finding_id", "verify requires exactly one of finding_id or new_finding_index")
		}
	case "review":
		if e.FindingID != "" || e.NewFindingIndex != nil {
			return invalid("kind", "review does not accept finding selectors")
		}
	default:
		return invalid("kind", "must be verify or review")
	}
	if e.NewFindingIndex != nil && *e.NewFindingIndex < 0 {
		return invalid("new_finding_index", "must be zero or positive")
	}
	if err := checkText("focus", e.Focus, 2000, true); err != nil {
		return err
	}
	if e.Files != nil {
		if len(e.Files) > 200 {
			return invalid("files", "must contain at most 200 entries")
		}
		seen := make(map[string]struct{}, len(e.Files))
		for i, file := range e.Files {
			if err := checkRepoPath(fmt.Sprintf("files[%d]", i), file, true); err != nil {
				return err
			}
			if _, ok := seen[file]; ok {
				return invalid(fmt.Sprintf("files[%d]", i), "duplicate file")
			}
			seen[file] = struct{}{}
		}
	}
	return nil
}

func (s SynthesizeOut) Validate() error {
	switch s.Verdict {
	case "PASS", "BLOCKING":
	default:
		return invalid("verdict", "must be PASS or BLOCKING")
	}
	if err := checkText("summary", s.Summary, MaxSummaryRunes, true); err != nil {
		return err
	}
	if s.Decisions == nil {
		return invalid("decisions", "is required")
	}
	if len(s.Decisions) > 500 {
		return invalid("decisions", "must contain at most 500 entries")
	}
	for i, decision := range s.Decisions {
		if err := decision.Validate(); err != nil {
			return wrapField(fmt.Sprintf("decisions[%d]", i), err)
		}
	}
	if s.NewFindings == nil {
		return invalid("new_findings", "is required")
	}
	if len(s.NewFindings) > 20 {
		return invalid("new_findings", "must contain at most 20 entries")
	}
	for i, finding := range s.NewFindings {
		if err := finding.Validate(); err != nil {
			return wrapField(fmt.Sprintf("new_findings[%d]", i), err)
		}
	}
	if s.ExtendRequests == nil {
		return invalid("extend_requests", "is required")
	}
	if len(s.ExtendRequests) > HardMaxExtends {
		return invalid("extend_requests", "must contain at most %d entries", HardMaxExtends)
	}
	for i, request := range s.ExtendRequests {
		if err := request.Validate(); err != nil {
			return wrapField(fmt.Sprintf("extend_requests[%d]", i), err)
		}
	}
	return nil
}

func (m Module) Validate() error {
	if m.Path != "." {
		if err := checkRepoPath("path", m.Path, true); err != nil {
			return err
		}
	}
	return checkText("purpose", m.Purpose, 1000, true)
}

func (o OnboardingOut) Validate() error {
	if err := checkText("overview", o.Overview, 4000, true); err != nil {
		return err
	}
	if err := checkText("architecture", o.Architecture, 8000, true); err != nil {
		return err
	}
	if o.Modules == nil {
		return invalid("modules", "is required")
	}
	if len(o.Modules) > 200 {
		return invalid("modules", "must contain at most 200 entries")
	}
	for i, module := range o.Modules {
		if err := module.Validate(); err != nil {
			return wrapField(fmt.Sprintf("modules[%d]", i), err)
		}
	}
	for _, values := range []struct {
		name  string
		items []string
	}{
		{"conventions", o.Conventions},
		{"build_and_test", o.BuildAndTest},
		{"risk_hotspots", o.RiskHotspots},
		{"review_focus", o.ReviewFocus},
	} {
		if values.items == nil {
			return invalid(values.name, "is required")
		}
		if len(values.items) > 100 {
			return invalid(values.name, "must contain at most 100 entries")
		}
		for i, value := range values.items {
			if err := checkText(fmt.Sprintf("%s[%d]", values.name, i), value, 2000, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func wrapField(prefix string, err error) error {
	var validErr *ValidationError
	if errors.As(err, &validErr) {
		field := validErr.Field
		if field != "" {
			field = prefix + "." + field
		} else {
			field = prefix
		}
		return &ValidationError{Field: field, Message: validErr.Message}
	}
	return &ValidationError{Field: prefix, Message: err.Error()}
}

// Validator is implemented by every contract root.
type Validator interface {
	Validate() error
}

// DecodeStrict rejects unknown fields, trailing JSON, missing required
// fields, explicit nulls, invalid enums, and nil required arrays.
func DecodeStrict[T Validator](raw []byte) (T, error) {
	var zero T
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return zero, invalid("", "empty JSON")
	}
	if trimmed[0] != '{' {
		return zero, invalid("", "contract must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var out T
	if err := dec.Decode(&out); err != nil {
		return zero, fmt.Errorf("decode contract: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return zero, invalid("", "trailing JSON value")
		}
		return zero, fmt.Errorf("decode trailing JSON: %w", err)
	}
	if err := checkRequiredFields(trimmed, reflect.TypeOf(out)); err != nil {
		return zero, err
	}
	if err := out.Validate(); err != nil {
		return zero, err
	}
	return out, nil
}

// Contract kind names used by node executors.
const (
	ContractPlan            = "plan"
	ContractCodeReview      = "code-review"
	ContractReviewStandards = "review.standards"
	ContractReviewIntent    = "review.intent"
	ContractReview          = "review"
	ContractReviewGate      = "review.gate"
	ContractReasoner        = "reasoner"
	ContractVerify          = "verify"
	ContractSynthesize      = "synthesize"
	ContractOnboarding      = "onboarding"
)

type contractExample struct {
	newValue func() Validator
}

func contractExamples() map[string]contractExample {
	return map[string]contractExample{
		ContractPlan:            {func() Validator { return &PlanOut{} }},
		ContractCodeReview:      {func() Validator { return &CodeReviewOut{} }},
		ContractReviewStandards: {func() Validator { return &ReviewOut{} }},
		ContractReviewIntent:    {func() Validator { return &ReviewOut{} }},
		ContractReview:          {func() Validator { return &ReviewOut{} }},
		ContractReviewGate:      {func() Validator { return &ReviewGateOut{} }},
		ContractReasoner:        {func() Validator { return &ReasonerOut{} }},
		ContractVerify:          {func() Validator { return &VerifyOut{} }},
		ContractSynthesize:      {func() Validator { return &SynthesizeOut{} }},
		ContractOnboarding:      {func() Validator { return &OnboardingOut{} }},
	}
}

// DecodeContract decodes one node output by kind. It does not perform
// cross-node checks such as gate coverage or synthesis decisions; callers use
// ValidateReviewGate or ValidateSynthesis for those.
func DecodeContract(kind string, raw []byte) (any, error) {
	example, ok := contractExamples()[kind]
	if !ok {
		return nil, fmt.Errorf("unknown contract kind %q", kind)
	}
	switch kind {
	case ContractPlan:
		return DecodeStrict[*PlanOut](raw)
	case ContractCodeReview:
		return DecodeStrict[*CodeReviewOut](raw)
	case ContractReviewStandards, ContractReviewIntent, ContractReview:
		return DecodeStrict[*ReviewOut](raw)
	case ContractReviewGate:
		return DecodeStrict[*ReviewGateOut](raw)
	case ContractReasoner:
		return DecodeStrict[*ReasonerOut](raw)
	case ContractVerify:
		return DecodeStrict[*VerifyOut](raw)
	case ContractSynthesize:
		return DecodeStrict[*SynthesizeOut](raw)
	case ContractOnboarding:
		return DecodeStrict[*OnboardingOut](raw)
	default:
		_ = example
		return nil, fmt.Errorf("unknown contract kind %q", kind)
	}
}

// ContractSchema returns a JSON Schema for one node contract.
func ContractSchema(kind string) (json.RawMessage, error) {
	example, ok := contractExamples()[kind]
	if !ok {
		return nil, fmt.Errorf("unknown contract kind %q", kind)
	}
	reflector := &jsonschema.Reflector{AllowAdditionalProperties: false, ExpandedStruct: true}
	schema := reflector.Reflect(example.newValue())
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("marshal %s schema: %w", kind, err)
	}
	return json.RawMessage(raw), nil
}

// ContractSchemas returns all node schemas. Schemas are deterministic.
func ContractSchemas() (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(contractExamples()))
	for kind := range contractExamples() {
		raw, err := ContractSchema(kind)
		if err != nil {
			return nil, err
		}
		out[kind] = raw
	}
	return out, nil
}

func checkRequiredFields(raw []byte, typ reflect.Type) error {
	if typ == nil {
		return invalid("", "missing contract type")
	}
	return checkRequiredValue(raw, typ, "")
}

func checkRequiredValue(raw []byte, typ reflect.Type, field string) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if field == "" {
			return invalid("", "contract must not be null")
		}
		return invalid(field, "must not be null")
	}
	switch typ.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return invalid(field, "must be a JSON object")
		}
		return checkRequiredStruct(object, typ, field)
	case reflect.Slice:
		var array []json.RawMessage
		if err := json.Unmarshal(raw, &array); err != nil {
			return invalid(field, "must be a JSON array")
		}
		for i, item := range array {
			if err := checkRequiredValue(item, typ.Elem(), fmt.Sprintf("%s[%d]", field, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkRequiredStruct(object map[string]json.RawMessage, typ reflect.Type, prefix string) error {
	for i := 0; i < typ.NumField(); i++ {
		structField := typ.Field(i)
		tag := structField.Tag.Get("json")
		name, options := parseJSONTag(tag)
		if name == "-" {
			continue
		}
		fieldType := structField.Type
		for fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		if structField.Anonymous && name == "" && fieldType.Kind() == reflect.Struct {
			if err := checkRequiredStruct(object, fieldType, prefix); err != nil {
				return err
			}
			continue
		}
		if name == "" {
			name = structField.Name
		}
		pathName := name
		if prefix != "" {
			pathName = prefix + "." + name
		}
		value, present := object[name]
		if !present {
			if options["omitempty"] || options["omitzero"] {
				continue
			}
			return invalid(pathName, "is required")
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return invalid(pathName, "must not be null")
		}
		if err := checkRequiredValue(value, structField.Type, pathName); err != nil {
			return err
		}
	}
	return nil
}

func parseJSONTag(tag string) (string, map[string]bool) {
	parts := strings.Split(tag, ",")
	options := make(map[string]bool, len(parts))
	for _, option := range parts[1:] {
		options[option] = true
	}
	return parts[0], options
}
