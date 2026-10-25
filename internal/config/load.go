package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

const defaultConfigYAML = `
server:
  listen: ":8080"
  public_url: ""
  read_timeout: 15s
  write_timeout: 60s
  trusted_proxies: []
gitlab:
  url: ""
  request_timeout: 30s
  max_concurrency: 16
  bot_usernames: []
auth:
  oauth: {}
  bootstrap_admins: []
  session_ttl: 168h
  bootstrap_token_env: REVIEW_ADMIN_TOKEN
storage:
  driver: sqlite
  dsn: "file:data/review.db"
  llm_call_retention: 720h
  delivery_retention: 336h
  job_retention: 168h
  context_versions: 5
secrets:
  master_key_env: REVIEW_MASTER_KEY
workspace:
  root: "data/workspaces"
  git_bin: "git"
  context_refresh:
    changed_files: 200
    max_age: 168h
  run_cleanup_after: 1h
jobs:
  workers: 4
  lease: 5m
engine:
  max_concurrent_nodes: 8
  default_node_timeout: 10m
providers: {}
profiles:
  default:
    plan:
      mode: code
      package_tokens: 60000
    propose:
      - block: code-review
    on_uncertain: mark
    synthesize:
      mode: code
    budget:
      max_nodes: 30
      max_extends: 0
      deadline: 20m
      max_tokens: 1500000
mcp: {}
skills:
  dirs: []
blocks: {}
review_defaults:
  language: "zh-CN"
  skip_draft: true
  target_branches: []
  labels_include: []
  labels_exclude: []
  ignore:
    - "**/*.lock"
    - "**/vendor/**"
    - "**/node_modules/**"
    - "**/*.pb.go"
    - "**/*_generated.*"
    - "**/*.min.js"
  max_files: 200
  max_diff_lines: 8000
  commit_status: false
  post_failure_note: true
  show_model_attribution: false
  allow_repo_config: true
  allow_repo_skills: false
  profile_by_label: {}
  custom_rules: []
`

const profileDefaultsYAML = `
plan:
  mode: code
  package_tokens: 60000
on_uncertain: mark
synthesize:
  mode: code
budget:
  max_nodes: 30
  max_extends: 0
  deadline: 20m
  max_tokens: 1500000
`

const providerDefaultsYAML = `
max_concurrency: 4
`

var (
	envRefRE      = regexp.MustCompile(`\$\{([^}]*)\}`)
	backtickValue = regexp.MustCompile("`[^`]*`")
)

// Load merges YAML configuration files in order, expands ${ENV_NAME} in parsed
// string scalars, and applies defaults. It does not validate required
// environment-backed values; call Config.Validate for that.
func Load(paths ...string) (*Config, error) {
	root, err := parseYAML([]byte(defaultConfigYAML), "<defaults>")
	if err != nil {
		return nil, err
	}

	for _, path := range paths {
		fileRoot, err := loadFile(path)
		if err != nil {
			return nil, err
		}
		if fileRoot == nil {
			continue
		}
		if root == nil {
			root = fileRoot
		} else {
			root = mergeNode(root, fileRoot)
		}
	}
	if root == nil {
		return nil, errors.New("config: empty configuration")
	}

	var envErrs []error
	expandEnv(root, &envErrs)
	if len(envErrs) > 0 {
		return nil, errors.Join(envErrs...)
	}
	applyProfileAndProviderDefaults(root)
	if err := checkDuplicateKeys(root); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	cfg := &Config{}
	if err := decodeStrict(root, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func loadFile(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return parseYAML(data, path)
}

func parseYAML(data []byte, source string) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, fmt.Errorf("parse config %s: %s", source, sanitizeYAMLError(err))
	}

	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("config %s: multiple YAML documents are not supported", source)
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config %s: %s", source, sanitizeYAMLError(err))
	}

	if len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return nil, nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config %s: top-level value must be a mapping", source)
	}
	if err := checkDuplicateKeys(root); err != nil {
		return nil, fmt.Errorf("config %s: %w", source, err)
	}
	return root, nil
}

func mergeNode(dst, src *yaml.Node) *yaml.Node {
	if dst == nil {
		return src
	}
	if src == nil {
		return dst
	}
	if dst.Kind == yaml.DocumentNode && src.Kind == yaml.DocumentNode {
		if len(dst.Content) > 0 && len(src.Content) > 0 {
			dst.Content[0] = mergeNode(dst.Content[0], src.Content[0])
		}
		return dst
	}
	if dst.Kind != yaml.MappingNode || src.Kind != yaml.MappingNode {
		return src
	}
	for i := 0; i+1 < len(src.Content); i += 2 {
		srcKey, srcValue := src.Content[i], src.Content[i+1]
		merged := false
		for j := 0; j+1 < len(dst.Content); j += 2 {
			if dst.Content[j].Value == srcKey.Value {
				dst.Content[j+1] = mergeNode(dst.Content[j+1], srcValue)
				merged = true
				break
			}
		}
		if !merged {
			dst.Content = append(dst.Content, srcKey, srcValue)
		}
	}
	return dst
}

// mergeMissing adds keys from src that dst does not define. Existing dst
// values, including explicit zero and false, are preserved.
func mergeMissing(dst, src *yaml.Node) *yaml.Node {
	if dst == nil {
		return src
	}
	if src == nil || dst.Kind != yaml.MappingNode || src.Kind != yaml.MappingNode {
		return dst
	}
	for i := 0; i+1 < len(src.Content); i += 2 {
		srcKey, srcValue := src.Content[i], src.Content[i+1]
		found := false
		for j := 0; j+1 < len(dst.Content); j += 2 {
			if dst.Content[j].Value == srcKey.Value {
				dst.Content[j+1] = mergeMissing(dst.Content[j+1], srcValue)
				found = true
				break
			}
		}
		if !found {
			dst.Content = append(dst.Content, srcKey, srcValue)
		}
	}
	return dst
}

// applyProfileAndProviderDefaults merges defaults into YAML mappings before
// decoding. Explicit zeros stay zero and fail validation instead of being
// silently replaced.
func applyProfileAndProviderDefaults(root *yaml.Node) {
	profiles := mappingValue(root, "profiles")
	if profiles != nil && profiles.Kind == yaml.MappingNode {
		defaults, err := parseYAML([]byte(profileDefaultsYAML), "<profile-defaults>")
		if err == nil {
			for i := 1; i < len(profiles.Content); i += 2 {
				profiles.Content[i] = mergeMissing(profiles.Content[i], defaults)
			}
		}
	}
	providers := mappingValue(root, "providers")
	if providers != nil && providers.Kind == yaml.MappingNode {
		defaults, err := parseYAML([]byte(providerDefaultsYAML), "<provider-defaults>")
		if err == nil {
			for i := 1; i < len(providers.Content); i += 2 {
				providers.Content[i] = mergeMissing(providers.Content[i], defaults)
			}
		}
	}
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func checkDuplicateKeys(root *yaml.Node) error {
	visited := make(map[*yaml.Node]bool)
	var walk func(*yaml.Node) error
	walk = func(node *yaml.Node) error {
		if node == nil || visited[node] {
			return nil
		}
		visited[node] = true
		if node.Kind == yaml.MappingNode {
			seen := make(map[string]int, len(node.Content)/2)
			for i := 0; i+1 < len(node.Content); i += 2 {
				key := node.Content[i]
				id := fmt.Sprintf("%d:%s", key.Kind, key.Value)
				if line, ok := seen[id]; ok {
					return fmt.Errorf("line %d: mapping key %q already defined at line %d", key.Line, key.Value, line)
				}
				seen[id] = key.Line
			}
		}
		for _, child := range node.Content {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(root)
}

func expandEnv(node *yaml.Node, errs *[]error) {
	if node == nil {
		return
	}
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" && strings.Contains(node.Value, "${") {
		node.Value = envRefRE.ReplaceAllStringFunc(node.Value, func(ref string) string {
			name := ref[2 : len(ref)-1]
			if !envNameRE.MatchString(name) {
				*errs = append(*errs, fmt.Errorf("line %d: invalid environment variable reference", node.Line))
				return ref
			}
			value, ok := os.LookupEnv(name)
			if !ok {
				*errs = append(*errs, fmt.Errorf("line %d: environment variable %s is not set", node.Line, name))
				return ref
			}
			return value
		})
	}
	for _, child := range node.Content {
		expandEnv(child, errs)
	}
}

func decodeStrict(root *yaml.Node, cfg *Config) error {
	data, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("encode merged config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return fmt.Errorf("decode config: %s", sanitizeYAMLError(err))
	}
	return nil
}

func sanitizeYAMLError(err error) string {
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) {
		msgs := make([]string, len(typeErr.Errors))
		for i, msg := range typeErr.Errors {
			msgs[i] = redactYAMLValues(msg)
		}
		return strings.Join(msgs, "; ")
	}
	return redactYAMLValues(err.Error())
}

func redactYAMLValues(s string) string {
	return backtickValue.ReplaceAllString(s, "`<redacted>`")
}
