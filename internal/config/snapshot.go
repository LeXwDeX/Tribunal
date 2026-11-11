package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
	"time"

	"github.com/LeXwDeX/tribunal/internal/llm/effort"
	"go.yaml.in/yaml/v3"
)

// Origin identifies the configuration layer that supplied an effective value.
type Origin string

const (
	// OriginDefault is the built-in default document compiled into the binary.
	OriginDefault Origin = "default"
	// OriginBase is one of the --config files.
	OriginBase Origin = "base"
	// OriginOverlay is the --overlay file.
	OriginOverlay Origin = "overlay"
)

// Source is one configuration file with the document exactly as written. Root
// keeps ${ENV} references unexpanded so a reload never rewrites its input.
type Source struct {
	// Origin is the layer this source belongs to: base or overlay.
	Origin Origin
	// Path is the file path used to read the source. It is never returned by
	// the HTTP API.
	Path string
	// Revision is the hex SHA-256 of the bytes that were read. A missing
	// overlay has an empty revision.
	Revision string
	// Root is the parsed YAML mapping, or nil for an empty or missing file.
	Root *yaml.Node
	// Missing is true when an optional overlay file does not exist.
	Missing bool
}

// Snapshot is the merged file configuration plus where each value came from.
// It is a display snapshot: the running services keep using the Config loaded
// at startup until a restart.
type Snapshot struct {
	Config *Config
	// Sources lists every file layer in load order (base files, then overlay).
	Sources []Source
	// Origins maps a dotted configuration path (array items use [n]) to the
	// layer that supplied the effective value.
	Origins map[string]Origin
	// BaseRevision is the SHA-256 over each base file revision in load order.
	BaseRevision string
	// OverlayRevision is the SHA-256 of the overlay file, empty when absent.
	OverlayRevision string
	// OverlayPresent reports whether a configured overlay file exists.
	OverlayPresent bool
	// Warnings mirrors Config.Warnings for display.
	Warnings []string
}

// State is the last-good display snapshot and the outcome of the latest
// reload attempt. Snapshot is never nil for a running server.
type State struct {
	Snapshot    *Snapshot
	LastError   string
	LastErrorAt time.Time
	UpdatedAt   time.Time
}

// LoadSnapshot merges the built-in defaults, the base files in order, and an
// optional overlay, then records per-key origins. An empty overlayPath disables
// the overlay. A configured but missing overlay is treated as an empty
// document; other overlay read errors fail the load.
func LoadSnapshot(basePaths []string, overlayPath string) (*Snapshot, error) {
	defaultRoot, err := parseYAML([]byte(defaultConfigYAML), "<defaults>")
	if err != nil {
		return nil, err
	}

	layers := []originLayer{{origin: OriginDefault, paths: collectPaths(defaultRoot)}}
	sources := make([]Source, 0, len(basePaths)+1)
	baseRevisions := make([]string, 0, len(basePaths))
	for _, path := range basePaths {
		root, revision, err := loadSourceFile(path)
		if err != nil {
			return nil, err
		}
		sources = append(sources, Source{Origin: OriginBase, Path: path, Revision: revision, Root: root})
		layers = append(layers, originLayer{origin: OriginBase, paths: collectPaths(root)})
		baseRevisions = append(baseRevisions, revision)
	}

	overlayRevision := ""
	overlayPresent := false
	if overlayPath != "" {
		root, revision, missing, err := loadOverlayFile(overlayPath)
		if err != nil {
			return nil, err
		}
		overlayPresent = !missing
		overlayRevision = revision
		if root != nil {
			// Reject overlay-only structural problems with the source name
			// before merging. Expand a copy first so typed fields may still use
			// ${ENV}; type errors survive merging because the overlay is the
			// highest layer, so reporting them here is accurate.
			checkRoot := cloneNode(root)
			var envErrs []error
			expandEnv(checkRoot, &envErrs)
			if len(envErrs) > 0 {
				return nil, errors.Join(envErrs...)
			}
			if err := decodeStrict(checkRoot, &Config{}); err != nil {
				return nil, fmt.Errorf("config %s: %w", overlayPath, err)
			}
		}
		sources = append(sources, Source{
			Origin: OriginOverlay, Path: overlayPath, Revision: revision, Root: root, Missing: missing,
		})
		layers = append(layers, originLayer{origin: OriginOverlay, paths: collectPaths(root)})
	}

	merged := cloneNode(defaultRoot)
	for _, source := range sources {
		if source.Root == nil {
			continue
		}
		merged = mergeNode(merged, cloneNode(source.Root))
	}
	if merged == nil {
		return nil, errors.New("config: empty configuration")
	}

	var envErrs []error
	expandEnv(merged, &envErrs)
	if len(envErrs) > 0 {
		return nil, errors.Join(envErrs...)
	}
	applyProfileAndProviderDefaults(merged)
	if err := checkDuplicateKeys(merged); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	cfg := &Config{}
	if err := decodeStrict(merged, cfg); err != nil {
		return nil, err
	}
	cfg.Warnings = normalizeProfiles(cfg)
	if err := checkProviderEffort(cfg); err != nil {
		return nil, err
	}
	appendProviderWarnings(cfg)

	return &Snapshot{
		Config:          cfg,
		Sources:         sources,
		Origins:         buildOrigins(layers, merged),
		BaseRevision:    combineRevisions(baseRevisions),
		OverlayRevision: overlayRevision,
		OverlayPresent:  overlayPresent,
		Warnings:        append([]string(nil), cfg.Warnings...),
	}, nil
}

// EqualRuntime reports whether two loaded configurations describe the same
// running service. Warnings are display-only and do not require a restart;
// RawProfiles still count because stored overrides are merged into them.
func EqualRuntime(left, right *Config) bool {
	if left == nil || right == nil {
		return left == right
	}
	a, b := *left, *right
	a.Warnings, b.Warnings = nil, nil
	return reflect.DeepEqual(a, b)
}

// loadSourceFile reads and parses one required base file.
func loadSourceFile(path string) (*yaml.Node, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read config %s: %w", path, err)
	}
	root, err := parseYAML(data, path)
	if err != nil {
		return nil, "", err
	}
	return root, revisionOf(data), nil
}

// loadOverlayFile reads and parses the optional overlay. A missing file is an
// empty document, not an error.
func loadOverlayFile(path string) (*yaml.Node, string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", true, nil
		}
		return nil, "", false, fmt.Errorf("read config %s: %w", path, err)
	}
	root, err := parseYAML(data, path)
	if err != nil {
		return nil, "", false, err
	}
	return root, revisionOf(data), false, nil
}

func revisionOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// combineRevisions makes one stable revision for the ordered base files.
func combineRevisions(revisions []string) string {
	if len(revisions) == 0 {
		return ""
	}
	digest := sha256.New()
	for _, revision := range revisions {
		_, _ = io.WriteString(digest, revision)
		_, _ = io.WriteString(digest, "\n")
	}
	return hex.EncodeToString(digest.Sum(nil))
}

type originLayer struct {
	origin Origin
	paths  map[string]bool
}

// buildOrigins walks the merged document and attributes every path to the
// highest-precedence layer that defines it. Paths injected by the profile and
// provider defaults are not present in any layer and report OriginDefault.
func buildOrigins(layers []originLayer, merged *yaml.Node) map[string]Origin {
	origins := make(map[string]Origin)
	for path := range collectPaths(merged) {
		origin := OriginDefault
		for i := len(layers) - 1; i >= 0; i-- {
			if layers[i].paths[path] {
				origin = layers[i].origin
				break
			}
		}
		origins[path] = origin
	}
	return origins
}

// collectPaths records every mapping key, sequence item, and scalar path.
func collectPaths(root *yaml.Node) map[string]bool {
	paths := make(map[string]bool)
	walkPaths(root, "", paths, make(map[*yaml.Node]bool))
	return paths
}

func walkPaths(node *yaml.Node, path string, paths map[string]bool, stack map[*yaml.Node]bool) {
	if node == nil {
		return
	}
	if node.Kind == yaml.AliasNode {
		if stack[node] {
			return
		}
		stack[node] = true
		walkPaths(node.Alias, path, paths, stack)
		delete(stack, node)
		return
	}
	if path != "" {
		paths[path] = true
	}
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			walkPaths(node.Content[i+1], joinConfigPath(path, node.Content[i].Value), paths, stack)
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			walkPaths(child, indexPath(path, i), paths, stack)
		}
	}
}

func joinConfigPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

func indexPath(parent string, index int) string {
	return fmt.Sprintf("%s[%d]", parent, index)
}

// cloneNode deep-copies a YAML document. Anchors are preserved, and alias
// cycles cannot recurse forever.
func cloneNode(node *yaml.Node) *yaml.Node {
	return cloneNodeVisited(node, make(map[*yaml.Node]*yaml.Node))
}

func cloneNodeVisited(node *yaml.Node, visited map[*yaml.Node]*yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	if copy, ok := visited[node]; ok {
		return copy
	}
	copy := *node
	visited[node] = &copy
	copy.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		copy.Content[i] = cloneNodeVisited(child, visited)
	}
	copy.Alias = cloneNodeVisited(node.Alias, visited)
	return &copy
}

// checkProviderEffort rejects capability combinations Load cannot represent.
func checkProviderEffort(cfg *Config) error {
	for _, name := range sortedKeys(cfg.Providers) {
		provider := cfg.Providers[name]
		if provider.Reasoning == nil || effort.ValidCapability(provider.Reasoning.Effort, provider.AllowReasoningEffortNone) {
			continue
		}
		field := "providers." + name
		if provider.Reasoning.Effort == "none" {
			return fmt.Errorf("config: %s.reasoning.effort none requires allow_reasoning_effort_none", field)
		}
		return fmt.Errorf("config: %s.reasoning.effort must be low, medium, high or max", field)
	}
	return nil
}

// appendProviderWarnings records provider capability combinations Load cannot
// enforce. It only names fields, never values.
func appendProviderWarnings(cfg *Config) {
	for _, name := range sortedKeys(cfg.Providers) {
		provider := cfg.Providers[name]
		field := "providers." + name
		if provider.Protocol == "anthropic" && provider.Reasoning != nil && provider.Reasoning.Effort != "" && !provider.OutputConfigEffort {
			cfg.Warnings = append(cfg.Warnings, field+".reasoning.effort is not sent by anthropic without output_config_effort")
		}
		if provider.Protocol != "anthropic" && provider.OutputConfigEffort {
			cfg.Warnings = append(cfg.Warnings, field+".output_config_effort is only sent by anthropic")
		}
		if (provider.Protocol == "openai-chat" || provider.Protocol == "openai-responses") && provider.Thinking != nil && provider.Thinking.BudgetTokens != 0 {
			cfg.Warnings = append(cfg.Warnings, field+".thinking.budget_tokens is not sent by "+provider.Protocol)
		}
		if provider.Protocol != "openai-chat" && provider.Thinking != nil && provider.Thinking.Type != "" {
			cfg.Warnings = append(cfg.Warnings, field+".thinking.type is not sent by "+provider.Protocol)
		}
		if provider.Protocol != "openai-chat" && provider.PreserveReasoningContent {
			cfg.Warnings = append(cfg.Warnings, field+".preserve_reasoning_content is only honoured by openai-chat")
		}
		if provider.Protocol != "openai-responses" && provider.ResponsesParams != "" {
			cfg.Warnings = append(cfg.Warnings, field+".responses_params is only used by openai-responses")
		}
		if provider.Protocol == "anthropic" && provider.AllowReasoningEffortNone {
			cfg.Warnings = append(cfg.Warnings, field+".allow_reasoning_effort_none is only used by OpenAI protocols")
		}
	}
}
