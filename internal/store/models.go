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
type ModelStore interface {
	ListModels(context.Context) ([]Model, error)
	GetModel(context.Context, string) (*Model, error)
	CreateModel(context.Context, Model, string) (*Model, error)
	UpdateModel(context.Context, Model, int64, *string) (*Model, error)
	SoftDeleteModel(context.Context, string, int64, int64) error
	OpenModelCredential(context.Context, string) ([]byte, error)
}
