package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// ScopePlatform marks an extension that applies to every project.
	ScopePlatform = "platform"
	// ScopeProject marks an extension that applies to one project.
	ScopeProject = "project"
)

// MCPServer is one stored Claude Code mcpServers entry. Config is the raw
// server object; the store never inspects or rewrites it.
type MCPServer struct {
	ID        string          `json:"id"`
	Scope     string          `json:"scope"`
	ProjectID *int64          `json:"project_id,omitempty"`
	Name      string          `json:"name"`
	Config    json.RawMessage `json:"config"`
	Enabled   bool            `json:"enabled"`
	// Builtin names the bundled default this row was created from; empty for
	// rows an administrator created.
	Builtin   string    `json:"builtin,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MCPServerUpdate replaces only the fields that are set. A nil field keeps
// the stored value.
type MCPServerUpdate struct {
	Name   *string
	Config json.RawMessage
	// MergeConfig, when set, combines the submitted config with the value read
	// inside the update transaction. It closes the gap between a caller that
	// restored secret placeholders from an earlier read and a concurrent write.
	MergeConfig func(submitted, current json.RawMessage) json.RawMessage
	Enabled     *bool
}

// MCPServerImport is one named entry of an atomic mcpServers import.
type MCPServerImport struct {
	Name   string
	Config json.RawMessage
}

// Skill is one registered skill directory. Uploaded archives keep an empty
// Dir; registered directories carry their absolute server path.
type Skill struct {
	ID          string `json:"id"`
	Scope       string `json:"scope"`
	ProjectID   *int64 `json:"project_id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Dir         string `json:"dir,omitempty"`
	Enabled     bool   `json:"enabled"`
	// Builtin names the bundled skill this row was installed from; empty for
	// uploaded or registered skills.
	Builtin   string    `json:"builtin,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// BlockBinding grants MCP tools and skills to one review block. Platform
// bindings use project ID 0 in the schema; the nil ProjectID is the API view.
type BlockBinding struct {
	Scope     string   `json:"scope"`
	ProjectID *int64   `json:"project_id,omitempty"`
	Block     string   `json:"block"`
	MCPTools  []string `json:"mcp_tools"`
	Skills    []string `json:"skills"`
	// Revision is the content digest of the binding; it is not stored.
	Revision string `json:"revision"`
}

// BlockBindingAbsentRevision marks a conditional write that expects no stored
// row. A content digest never equals this value.
const BlockBindingAbsentRevision = "absent"

// BlockBindingRevision returns a stable digest of the binding content. The
// scope, project ID, block, MCP tools and skills cover the complete grant, so
// changing any listed item invalidates the previous revision. Nil and empty
// arrays have the same digest; array order and duplicates are preserved.
func BlockBindingRevision(scope string, projectID *int64, block string, mcpTools, skills []string) string {
	project := int64(0)
	if projectID != nil {
		project = *projectID
	}
	var b strings.Builder
	b.WriteString("block-binding/v1\n")
	writeRevisionPart(&b, strings.TrimSpace(scope))
	writeRevisionPart(&b, strconv.FormatInt(project, 10))
	writeRevisionPart(&b, strings.TrimSpace(block))
	b.WriteString(strconv.Itoa(len(mcpTools)))
	b.WriteByte('\n')
	for _, tool := range mcpTools {
		writeRevisionPart(&b, tool)
	}
	b.WriteString(strconv.Itoa(len(skills)))
	b.WriteByte('\n')
	for _, skill := range skills {
		writeRevisionPart(&b, skill)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// ValidBlockBindingRevision reports whether revision is the absent marker or a
// digest this package produces. Values are case-sensitive.
func ValidBlockBindingRevision(revision string) bool {
	if revision == BlockBindingAbsentRevision {
		return true
	}
	if len(revision) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(revision); i++ {
		c := revision[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func writeRevisionPart(b *strings.Builder, value string) {
	b.WriteString(strconv.Itoa(len(value)))
	b.WriteByte(':')
	b.WriteString(value)
	b.WriteByte('\n')
}

// ValidateExtensionScope checks the shared scope/project_id selector. An
// empty scope means "all scopes" only when required is false.
func ValidateExtensionScope(scope string, projectID *int64, required bool) error {
	switch scope {
	case "":
		if required {
			return errors.New("scope is required")
		}
		if projectID != nil {
			return errors.New("project_id requires project scope")
		}
		return nil
	case ScopePlatform:
		if projectID != nil {
			return errors.New("project_id must be omitted for platform scope")
		}
		return nil
	case ScopeProject:
		if projectID == nil {
			return errors.New("project_id is required for project scope")
		}
		if *projectID <= 0 {
			return errors.New("project_id must be positive")
		}
		return nil
	default:
		return fmt.Errorf("scope must be %s or %s", ScopePlatform, ScopeProject)
	}
}

// Extension default outcomes. A recorded key is never processed again.
const (
	ExtensionDefaultApplied = "applied"
	ExtensionDefaultSkipped = "skipped"
)

// ExtensionDefault is one ledger row of the platform default extension
// seeder: a registration, a block grant or a block snapshot. Detail is a
// bounded reason or the JSON binding the defaults last wrote.
type ExtensionDefault struct {
	Key       string    `json:"key"`
	Outcome   string    `json:"outcome"`
	Detail    string    `json:"detail,omitempty"`
	AppliedAt time.Time `json:"applied_at"`
}
