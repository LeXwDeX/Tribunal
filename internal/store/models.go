package store

import (
	"context"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
)

// ModelParams contains only non-credential provider settings.
type ModelParams struct {
	MaxTokens             int                     `json:"max_tokens"`
	ContextWindow         int                     `json:"context_window"`
	Temperature           *float64                `json:"temperature"`
	Reasoning             *config.ReasoningConfig `json:"reasoning"`
	Thinking              *config.ThinkingConfig  `json:"thinking"`
	ForcedToolChoice      string                  `json:"forced_tool_choice"`
	StrictRoleAlternation bool                    `json:"strict_role_alternation"`
	MaxConcurrency        int                     `json:"max_concurrency"`
	RPM                   int                     `json:"rpm"`
	// Capability declarations (omitempty keeps existing stored records and API
	// responses byte-identical when unset).
	AllowReasoningEffortNone bool   `json:"allow_reasoning_effort_none,omitempty"`
	PreserveReasoningContent bool   `json:"preserve_reasoning_content,omitempty"`
	OutputConfigEffort       bool   `json:"output_config_effort,omitempty"`
	ResponsesParams          string `json:"responses_params,omitempty"`
}

// ParamUpdate marks one nullable parameter present in a merge request. Set is
// false when the key is absent; Set with a nil Value clears the stored value.
type ParamUpdate[T any] struct {
	Set   bool
	Value *T
}

// ModelParamsPatch names the parameter keys present in a merge request. Nil
// pointers leave the stored value unchanged; explicit nulls are represented by
// ParamUpdate with a nil Value.
type ModelParamsPatch struct {
	MaxTokens                *int
	ContextWindow            *int
	Temperature              ParamUpdate[float64]
	Reasoning                ParamUpdate[config.ReasoningConfig]
	Thinking                 ParamUpdate[config.ThinkingConfig]
	ForcedToolChoice         *string
	StrictRoleAlternation    *bool
	MaxConcurrency           *int
	RPM                      *int
	AllowReasoningEffortNone *bool
	PreserveReasoningContent *bool
	OutputConfigEffort       *bool
	ResponsesParams          *string
}

// Apply merges the patch into base. Keys absent from the patch keep their
// stored value; an explicit null clears a nullable parameter.
func (p ModelParamsPatch) Apply(base ModelParams) ModelParams {
	if p.MaxTokens != nil {
		base.MaxTokens = *p.MaxTokens
	}
	if p.ContextWindow != nil {
		base.ContextWindow = *p.ContextWindow
	}
	if p.Temperature.Set {
		base.Temperature = p.Temperature.Value
	}
	if p.Reasoning.Set {
		base.Reasoning = p.Reasoning.Value
	}
	if p.Thinking.Set {
		base.Thinking = p.Thinking.Value
	}
	if p.ForcedToolChoice != nil {
		base.ForcedToolChoice = *p.ForcedToolChoice
	}
	if p.StrictRoleAlternation != nil {
		base.StrictRoleAlternation = *p.StrictRoleAlternation
	}
	if p.MaxConcurrency != nil {
		base.MaxConcurrency = *p.MaxConcurrency
	}
	if p.RPM != nil {
		base.RPM = *p.RPM
	}
	if p.AllowReasoningEffortNone != nil {
		base.AllowReasoningEffortNone = *p.AllowReasoningEffortNone
	}
	if p.PreserveReasoningContent != nil {
		base.PreserveReasoningContent = *p.PreserveReasoningContent
	}
	if p.OutputConfigEffort != nil {
		base.OutputConfigEffort = *p.OutputConfigEffort
	}
	if p.ResponsesParams != nil {
		base.ResponsesParams = *p.ResponsesParams
	}
	return base
}

// Fields returns the changed parameter paths in sorted order without values.
func (p ModelParamsPatch) Fields() []string {
	fields := []string{}
	if p.AllowReasoningEffortNone != nil {
		fields = append(fields, "params.allow_reasoning_effort_none")
	}
	if p.ContextWindow != nil {
		fields = append(fields, "params.context_window")
	}
	if p.ForcedToolChoice != nil {
		fields = append(fields, "params.forced_tool_choice")
	}
	if p.MaxConcurrency != nil {
		fields = append(fields, "params.max_concurrency")
	}
	if p.MaxTokens != nil {
		fields = append(fields, "params.max_tokens")
	}
	if p.OutputConfigEffort != nil {
		fields = append(fields, "params.output_config_effort")
	}
	if p.PreserveReasoningContent != nil {
		fields = append(fields, "params.preserve_reasoning_content")
	}
	if p.Reasoning.Set {
		fields = append(fields, "params.reasoning")
	}
	if p.ResponsesParams != nil {
		fields = append(fields, "params.responses_params")
	}
	if p.RPM != nil {
		fields = append(fields, "params.rpm")
	}
	if p.StrictRoleAlternation != nil {
		fields = append(fields, "params.strict_role_alternation")
	}
	if p.Temperature.Set {
		fields = append(fields, "params.temperature")
	}
	if p.Thinking.Set {
		fields = append(fields, "params.thinking")
	}
	return fields
}

// ParamSource labels the layer that supplied an effective parameter.
const (
	ParamSourceModel   = "model"
	ParamSourceDefault = "default"
)

// MergeModelParams resolves a user model's effective parameters: an explicit
// stored value wins, an unset value inherits the platform default, and a value
// set by neither layer stays unset. A parameter is unset when it holds its
// zero value (0, false, an empty string or nil), so the result cannot express
// an explicit zero that overrides a non-zero default. Object parameters
// (reasoning, thinking) are inherited or replaced as a whole, matching the
// merge-request semantics.
//
// sources maps each flat parameter name that has a value to the layer that
// supplied it (ParamSourceModel or ParamSourceDefault); parameters unset in
// both layers are omitted.
func MergeModelParams(explicit, defaults ModelParams) (ModelParams, map[string]string) {
	merged := explicit
	sources := map[string]string{}
	sourceInt := func(name string, explicitValue, defaultValue int) int {
		switch {
		case explicitValue != 0:
			sources[name] = ParamSourceModel
			return explicitValue
		case defaultValue != 0:
			sources[name] = ParamSourceDefault
			return defaultValue
		default:
			return 0
		}
	}
	sourceBool := func(name string, explicitValue, defaultValue bool) bool {
		switch {
		case explicitValue:
			sources[name] = ParamSourceModel
			return true
		case defaultValue:
			sources[name] = ParamSourceDefault
			return true
		default:
			return false
		}
	}
	sourceString := func(name, explicitValue, defaultValue string) string {
		switch {
		case explicitValue != "":
			sources[name] = ParamSourceModel
			return explicitValue
		case defaultValue != "":
			sources[name] = ParamSourceDefault
			return defaultValue
		default:
			return ""
		}
	}
	merged.MaxTokens = sourceInt("max_tokens", explicit.MaxTokens, defaults.MaxTokens)
	merged.ContextWindow = sourceInt("context_window", explicit.ContextWindow, defaults.ContextWindow)
	merged.MaxConcurrency = sourceInt("max_concurrency", explicit.MaxConcurrency, defaults.MaxConcurrency)
	merged.RPM = sourceInt("rpm", explicit.RPM, defaults.RPM)
	switch {
	case explicit.Temperature != nil:
		sources["temperature"] = ParamSourceModel
	case defaults.Temperature != nil:
		merged.Temperature = defaults.Temperature
		sources["temperature"] = ParamSourceDefault
	}
	switch {
	case explicit.Reasoning != nil:
		sources["reasoning"] = ParamSourceModel
	case defaults.Reasoning != nil:
		merged.Reasoning = defaults.Reasoning
		sources["reasoning"] = ParamSourceDefault
	}
	switch {
	case explicit.Thinking != nil:
		sources["thinking"] = ParamSourceModel
	case defaults.Thinking != nil:
		merged.Thinking = defaults.Thinking
		sources["thinking"] = ParamSourceDefault
	}
	merged.ForcedToolChoice = sourceString("forced_tool_choice", explicit.ForcedToolChoice, defaults.ForcedToolChoice)
	merged.ResponsesParams = sourceString("responses_params", explicit.ResponsesParams, defaults.ResponsesParams)
	merged.StrictRoleAlternation = sourceBool("strict_role_alternation", explicit.StrictRoleAlternation, defaults.StrictRoleAlternation)
	merged.AllowReasoningEffortNone = sourceBool("allow_reasoning_effort_none", explicit.AllowReasoningEffortNone, defaults.AllowReasoningEffortNone)
	merged.PreserveReasoningContent = sourceBool("preserve_reasoning_content", explicit.PreserveReasoningContent, defaults.PreserveReasoningContent)
	merged.OutputConfigEffort = sourceBool("output_config_effort", explicit.OutputConfigEffort, defaults.OutputConfigEffort)
	return merged, sources
}

// ModelDefaults is the platform-level default parameter document applied to
// user models. Params uses the same zero-value-means-unset convention as a
// stored model.
type ModelDefaults struct {
	Params    ModelParams `json:"params"`
	Revision  int64       `json:"revision"`
	UpdatedBy int64       `json:"updated_by"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// ModelDefaultsStore stores the single platform-level defaults row used by
// user models. Config models are not affected; their defaults come from the
// base/overlay configuration.
type ModelDefaultsStore interface {
	// GetModelDefaults returns an empty document with revision 0 when no row
	// has been saved yet.
	GetModelDefaults(context.Context) (*ModelDefaults, error)
	// PutModelDefaults replaces the whole document at the given revision. A
	// different current revision is ErrStaleRevision.
	PutModelDefaults(ctx context.Context, params ModelParams, revision, userID int64) (*ModelDefaults, error)
	// DeleteModelDefaults clears the document at the given revision and keeps
	// the revision monotonic. A different current revision is ErrStaleRevision.
	DeleteModelDefaults(ctx context.Context, revision, userID int64) (*ModelDefaults, error)
}

// ModelEndpoint is the stored endpoint metadata needed to decide whether a
// saved credential may be reused. It does not parse the parameter JSON, so it
// remains readable when a record's parameters are corrupt.
type ModelEndpoint struct {
	Protocol  string
	BaseURL   string
	HasAPIKey bool
}

// Model is a public model record. Credentials are never included.
type Model struct {
	ID          string      `json:"id"`
	DisplayName string      `json:"display_name"`
	Protocol    string      `json:"protocol"`
	BaseURL     string      `json:"base_url"`
	Model       string      `json:"model"`
	Params      ModelParams `json:"params"`
	HasAPIKey   bool        `json:"has_api_key"`
	Unavailable bool        `json:"unavailable"`
	Revision    int64       `json:"revision"`
	CreatedBy   int64       `json:"created_by"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedBy   int64       `json:"updated_by"`
	UpdatedAt   time.Time   `json:"updated_at"`
	DeletedAt   *time.Time  `json:"deleted_at,omitempty"`
}

// ModelStore stores models without returning credentials in records. A nil
// update credential keeps the existing credential; an empty value clears it.
// UpdateModelParams merges only the parameter keys named by its patch, under
// the same revision check as UpdateModel.
type ModelStore interface {
	ListModels(context.Context) ([]Model, error)
	GetModel(context.Context, string) (*Model, error)
	GetModelEndpoint(context.Context, string) (ModelEndpoint, error)
	CreateModel(context.Context, Model, string) (*Model, error)
	UpdateModel(context.Context, Model, int64, *string) (*Model, error)
	UpdateModelParams(context.Context, string, int64, ModelParamsPatch, int64) (*Model, error)
	SoftDeleteModel(context.Context, string, int64, int64) error
	OpenModelCredential(context.Context, string) ([]byte, error)
}
