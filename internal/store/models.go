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
	Price                 *config.PriceConfig     `json:"price"`
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
	Price                    ParamUpdate[config.PriceConfig]
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
	if p.Price.Set {
		base.Price = p.Price.Value
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
	if p.Price.Set {
		fields = append(fields, "params.price")
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
