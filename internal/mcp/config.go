// Package mcp connects explicitly allowlisted MCP tools to agents.
//
// It speaks the official MCP Go SDK, reads Claude-compatible mcpServers
// configuration, and keeps stdio servers in a per-workspace pool. It never
// spawns commands chosen by a model: commands, URLs and index commands come
// from administrator configuration only.
package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// MaxConfigBytes bounds one MCP configuration document.
	MaxConfigBytes = 1 << 20
	// DefaultCallTimeout bounds one tool call when x-review.call_timeout is
	// absent.
	DefaultCallTimeout = 60 * time.Second
	// DefaultIdleTimeout closes an idle server after ten minutes.
	DefaultIdleTimeout = 10 * time.Minute
	// DefaultMaxOutputChars bounds one tool result in characters.
	DefaultMaxOutputChars = 20000
	// MaxOutputChars is the largest accepted per-call output limit.
	MaxOutputChars = 1 << 20

	maxServers      = 64
	maxArgs         = 128
	maxEnv          = 128
	maxHeaders      = 64
	maxAllowlist    = 64
	maxIndexArgs    = 128
	maxNameLen      = 8192
	maxFieldLen     = 8192
	minCallTimeout  = time.Millisecond
	maxCallTimeout  = time.Hour
	minIdleTimeout  = time.Millisecond
	maxIdleTimeout  = 24 * time.Hour
	minOutputChars  = 1
	maxServerOutput = MaxOutputChars
)

var (
	// ErrInvalidConfig reports an MCP configuration that failed validation.
	// Its text never contains configured secret values.
	ErrInvalidConfig = errors.New("mcp: invalid config")

	serverNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	envNameRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	headerNameRE = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]+$")
	envRefRE     = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

// Config is the Claude-compatible document shared by platform and project
// scopes. Merge gives project entries precedence by name.
type Config struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// ServerConfig is one mcpServers entry plus the x-review platform extension.
// Type is "stdio" (the default when Command is set) or "http" (when URL is
// set).
type ServerConfig struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	XReview XReviewConfig     `json:"x-review,omitempty"`
}

// XReviewConfig is the platform extension. All fields are optional.
type XReviewConfig struct {
	IndexCommand   []string `json:"index_command,omitempty"`
	EnvAllowlist   []string `json:"env_allowlist,omitempty"`
	Home           string   `json:"home,omitempty"`
	CallTimeout    Duration `json:"call_timeout,omitempty"`
	MaxOutputChars int      `json:"max_output_chars,omitempty"`
	IdleTimeout    Duration `json:"idle_timeout,omitempty"`
}

// Duration is a JSON duration string such as "60s". Zero selects the default.
type Duration time.Duration

// MarshalJSON writes the duration as a Go duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts a duration string. Numbers and unknown units are
// rejected without echoing the input.
func (d *Duration) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*d = 0
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return errors.New("duration must be a string")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return errors.New("invalid duration")
	}
	if parsed < 0 {
		return errors.New("duration must not be negative")
	}
	*d = Duration(parsed)
	return nil
}

// String implements fmt.Stringer.
func (d Duration) String() string { return time.Duration(d).String() }

// Duration returns the standard library value.
func (d Duration) AsDuration() time.Duration { return time.Duration(d) }

// Parse reads a strict, bounded Claude-compatible configuration. ${NAME}
// references are expanded from the process environment; a missing variable is
// an error. denied names (project tokens and provider keys) are never expanded.
// Unknown fields, trailing data and oversized documents are rejected.
func Parse(data []byte, denied ...string) (Config, error) {
	return ParseWithEnv(data, os.LookupEnv, denied...)
}

// ParseWithEnv is Parse with an explicit environment lookup, used by callers
// and tests that must not consult the process environment directly.
func ParseWithEnv(data []byte, lookup func(string) (string, bool), denied ...string) (Config, error) {
	cfg, err := parseRaw(data)
	if err != nil {
		return Config{}, err
	}
	if len(cfg.MCPServers) == 0 {
		return cfg, nil
	}
	if len(cfg.MCPServers) > maxServers {
		return Config{}, fmt.Errorf("%w: too many servers (max %d)", ErrInvalidConfig, maxServers)
	}
	denied = cleanDenied(denied)
	names := make([]string, 0, len(cfg.MCPServers))
	for name := range cfg.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := Config{MCPServers: make(map[string]ServerConfig, len(cfg.MCPServers))}
	for _, name := range names {
		server, err := expandServer(name, cfg.MCPServers[name], lookup, denied)
		if err != nil {
			return Config{}, err
		}
		server = server.withDefaults()
		if err := server.Validate(name); err != nil {
			return Config{}, err
		}
		out.MCPServers[name] = server
	}
	return out, nil
}

func cleanDenied(denied []string) []string {
	out := make([]string, 0, len(denied))
	for _, name := range denied {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func parseRaw(data []byte) (Config, error) {
	if len(data) == 0 {
		return Config{}, fmt.Errorf("%w: empty document", ErrInvalidConfig)
	}
	if len(data) > MaxConfigBytes {
		return Config{}, fmt.Errorf("%w: document exceeds %d bytes", ErrInvalidConfig, MaxConfigBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var raw struct {
		MCPServers *map[string]ServerConfig `json:"mcpServers"`
	}
	if err := dec.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("%w: %s", ErrInvalidConfig, safeJSONError(err))
	}
	if raw.MCPServers == nil {
		return Config{}, fmt.Errorf("%w: mcpServers is required", ErrInvalidConfig)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, fmt.Errorf("%w: trailing JSON data", ErrInvalidConfig)
		}
		return Config{}, fmt.Errorf("%w: trailing JSON data", ErrInvalidConfig)
	}
	return Config{MCPServers: *raw.MCPServers}, nil
}

// DecodeServer reads one strict, bounded mcpServers entry. It rejects unknown
// fields and trailing data but does not expand ${NAME} references, so stored
// configuration keeps its templates.
func DecodeServer(data []byte) (ServerConfig, error) {
	if len(data) == 0 || len(data) > MaxConfigBytes {
		return ServerConfig{}, fmt.Errorf("%w: invalid server document size", ErrInvalidConfig)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cfg ServerConfig
	if err := dec.Decode(&cfg); err != nil {
		return ServerConfig{}, fmt.Errorf("%w: %s", ErrInvalidConfig, safeJSONError(err))
	}
	if err := ensureJSONEOF(dec); err != nil {
		return ServerConfig{}, err
	}
	return cfg, nil
}

// ParseServer reads one mcpServers entry, as posted by the extension API. It
// applies environment expansion, defaults and validation.
func ParseServer(data []byte, denied ...string) (ServerConfig, error) {
	cfg, err := DecodeServer(data)
	if err != nil {
		return ServerConfig{}, err
	}
	cfg, err = expandServer("", cfg, os.LookupEnv, cleanDenied(denied))
	if err != nil {
		return ServerConfig{}, err
	}
	cfg = cfg.withDefaults()
	if err := cfg.Validate(""); err != nil {
		return ServerConfig{}, err
	}
	return cfg, nil
}

// ValidateServerJSON validates one stored entry without expanding environment
// references or reading the process environment. Unknown fields, invalid
// values and references to denied or sensitive variables are rejected.
func ValidateServerJSON(name string, data []byte, denied ...string) error {
	cfg, err := DecodeServer(data)
	if err != nil {
		return err
	}
	cfg = cfg.withDefaults()
	if name != "" {
		if err := ValidateServerName(name); err != nil {
			return err
		}
	}
	if err := cfg.Validate(name); err != nil {
		return err
	}
	denied = cleanDenied(denied)
	for _, key := range sortedKeys(cfg.Env) {
		if nameDenied(key, denied) {
			return fmt.Errorf("%w: server config: env[%s]: environment variable is not allowed", ErrInvalidConfig, key)
		}
	}
	return validateTemplateRefs(cfg, denied)
}

// ValidateServerName checks the stable server identifier used in tool names.
func ValidateServerName(name string) error {
	if !serverNameRE.MatchString(name) {
		return fmt.Errorf("%w: invalid server name", ErrInvalidConfig)
	}
	if strings.Contains(name, "__") {
		return fmt.Errorf("%w: server name must not contain \"__\"", ErrInvalidConfig)
	}
	return nil
}

// LoadFile reads a bounded MCP configuration file.
func LoadFile(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("mcp: read config %s: %w", path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxConfigBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("mcp: read config %s: %w", path, err)
	}
	if len(data) > MaxConfigBytes {
		return Config{}, fmt.Errorf("%w: document exceeds %d bytes", ErrInvalidConfig, MaxConfigBytes)
	}
	return Parse(data)
}

// Merge combines platform and project configurations. A project server with
// the same name replaces the platform entry completely; nested fields are not
// merged.
func Merge(platform, project Config) Config {
	out := Config{MCPServers: make(map[string]ServerConfig, len(platform.MCPServers)+len(project.MCPServers))}
	for name, server := range platform.MCPServers {
		out.MCPServers[name] = cloneServer(server)
	}
	for name, server := range project.MCPServers {
		out.MCPServers[name] = cloneServer(server)
	}
	return out
}

func cloneServer(cfg ServerConfig) ServerConfig {
	cfg.Args = append([]string(nil), cfg.Args...)
	cfg.XReview.IndexCommand = append([]string(nil), cfg.XReview.IndexCommand...)
	cfg.XReview.EnvAllowlist = append([]string(nil), cfg.XReview.EnvAllowlist...)
	cfg.Env = cloneMap(cfg.Env)
	cfg.Headers = cloneMap(cfg.Headers)
	return cfg
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (cfg ServerConfig) withDefaults() ServerConfig {
	if cfg.XReview.CallTimeout <= 0 {
		cfg.XReview.CallTimeout = Duration(DefaultCallTimeout)
	}
	if cfg.XReview.IdleTimeout <= 0 {
		cfg.XReview.IdleTimeout = Duration(DefaultIdleTimeout)
	}
	if cfg.XReview.MaxOutputChars == 0 {
		cfg.XReview.MaxOutputChars = DefaultMaxOutputChars
	}
	return cfg
}

func expandServer(name string, cfg ServerConfig, lookup func(string) (string, bool), denied []string) (ServerConfig, error) {
	var err error
	where := "server " + name
	if name == "" {
		where = "server config"
	}
	expand := func(field, value string) (string, error) {
		out, err := expandValue(value, lookup, denied)
		if err != nil {
			return "", fmt.Errorf("%w: %s: %s: %w", ErrInvalidConfig, where, field, err)
		}
		return out, nil
	}
	if cfg.Command, err = expand("command", cfg.Command); err != nil {
		return ServerConfig{}, err
	}
	if cfg.URL, err = expand("url", cfg.URL); err != nil {
		return ServerConfig{}, err
	}
	for i := range cfg.Args {
		if cfg.Args[i], err = expand(fmt.Sprintf("args[%d]", i), cfg.Args[i]); err != nil {
			return ServerConfig{}, err
		}
	}
	for i := range cfg.XReview.IndexCommand {
		if cfg.XReview.IndexCommand[i], err = expand(fmt.Sprintf("x-review.index_command[%d]", i), cfg.XReview.IndexCommand[i]); err != nil {
			return ServerConfig{}, err
		}
	}
	if cfg.XReview.Home, err = expand("x-review.home", cfg.XReview.Home); err != nil {
		return ServerConfig{}, err
	}
	if err := expandMap(cfg.Env, "env", where, lookup, denied); err != nil {
		return ServerConfig{}, err
	}
	if err := expandMap(cfg.Headers, "headers", where, lookup, denied); err != nil {
		return ServerConfig{}, err
	}
	return cfg, nil
}

func expandMap(in map[string]string, field, where string, lookup func(string) (string, bool), denied []string) error {
	for key, value := range in {
		out, err := expandValue(value, lookup, denied)
		if err != nil {
			return fmt.Errorf("%w: %s: %s[%s]: %w", ErrInvalidConfig, where, field, key, err)
		}
		in[key] = out
	}
	return nil
}

func expandValue(value string, lookup func(string) (string, bool), denied []string) (string, error) {
	if !strings.Contains(value, "${") {
		return value, nil
	}
	if lookup == nil {
		return "", errors.New("environment lookup is unavailable")
	}
	var firstErr error
	out := envRefRE.ReplaceAllStringFunc(value, func(ref string) string {
		name := ref[2 : len(ref)-1]
		if dangerousName(name) || sensitiveName(name) || nameDenied(name, denied) {
			if firstErr == nil {
				firstErr = fmt.Errorf("%w: environment variable %s is not allowed", ErrSensitiveEnv, name)
			}
			return ref
		}
		replacement, ok := lookup(name)
		if !ok {
			if firstErr == nil {
				firstErr = fmt.Errorf("environment variable %s is not set", name)
			}
			return ref
		}
		return replacement
	})
	if strings.Contains(out, "${") && firstErr == nil {
		firstErr = errors.New("invalid environment variable reference")
	}
	return out, firstErr
}

// validateTemplateRefs rejects sensitive or malformed ${NAME} references
// without resolving them. Error messages name the variable, never its value.
func validateTemplateRefs(cfg ServerConfig, denied []string) error {
	check := func(field, value string) error {
		if !strings.Contains(value, "${") {
			return nil
		}
		var firstErr error
		out := envRefRE.ReplaceAllStringFunc(value, func(ref string) string {
			name := ref[2 : len(ref)-1]
			if dangerousName(name) || sensitiveName(name) || nameDenied(name, denied) {
				if firstErr == nil {
					firstErr = fmt.Errorf("%w: environment variable %s is not allowed", ErrSensitiveEnv, name)
				}
				return ref
			}
			return ""
		})
		if strings.Contains(out, "${") && firstErr == nil {
			firstErr = errors.New("invalid environment variable reference")
		}
		if firstErr != nil {
			return fmt.Errorf("%w: server config: %s: %w", ErrInvalidConfig, field, firstErr)
		}
		return nil
	}
	if err := check("command", cfg.Command); err != nil {
		return err
	}
	if err := check("url", cfg.URL); err != nil {
		return err
	}
	for i, arg := range cfg.Args {
		if err := check(fmt.Sprintf("args[%d]", i), arg); err != nil {
			return err
		}
	}
	for i, arg := range cfg.XReview.IndexCommand {
		if err := check(fmt.Sprintf("x-review.index_command[%d]", i), arg); err != nil {
			return err
		}
	}
	if err := check("x-review.home", cfg.XReview.Home); err != nil {
		return err
	}
	for _, key := range sortedKeys(cfg.Env) {
		if err := check("env["+key+"]", cfg.Env[key]); err != nil {
			return err
		}
	}
	for _, key := range sortedKeys(cfg.Headers) {
		if err := check("headers["+key+"]", cfg.Headers[key]); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Validate checks one named server. An empty name validates the shape only,
// as used by ParseServer.
func (cfg ServerConfig) Validate(name string) error {
	if name != "" {
		if err := ValidateServerName(name); err != nil {
			return err
		}
	}
	kind := strings.ToLower(strings.TrimSpace(cfg.Type))
	if kind == "" {
		switch {
		case cfg.Command != "":
			kind = "stdio"
		case cfg.URL != "":
			kind = "http"
		default:
			return fmt.Errorf("%w: server %q needs command or url", ErrInvalidConfig, name)
		}
	}
	if kind != "stdio" && kind != "http" {
		return fmt.Errorf("%w: server %q has unsupported type", ErrInvalidConfig, name)
	}
	if strings.ContainsRune(cfg.Command, 0) || strings.ContainsAny(cfg.Command, "\r\n") {
		return fmt.Errorf("%w: server %q has an invalid command", ErrInvalidConfig, name)
	}
	if len(cfg.Command) > maxFieldLen {
		return fmt.Errorf("%w: server %q command is too long", ErrInvalidConfig, name)
	}
	if len(cfg.Args) > maxArgs || len(cfg.XReview.IndexCommand) > maxIndexArgs {
		return fmt.Errorf("%w: server %q has too many arguments", ErrInvalidConfig, name)
	}
	if len(cfg.XReview.IndexCommand) > 0 && strings.TrimSpace(cfg.XReview.IndexCommand[0]) == "" {
		return fmt.Errorf("%w: server %q has an empty index command", ErrInvalidConfig, name)
	}
	for _, arg := range append(append([]string(nil), cfg.Args...), cfg.XReview.IndexCommand...) {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("%w: server %q has an argument containing NUL", ErrInvalidConfig, name)
		}
		if len(arg) > maxNameLen {
			return fmt.Errorf("%w: server %q has an argument that is too long", ErrInvalidConfig, name)
		}
	}
	switch kind {
	case "stdio":
		if cfg.Command == "" {
			return fmt.Errorf("%w: server %q needs a command", ErrInvalidConfig, name)
		}
		if cfg.URL != "" || len(cfg.Headers) > 0 {
			return fmt.Errorf("%w: stdio server %q cannot set url or headers", ErrInvalidConfig, name)
		}
	case "http":
		if cfg.Command != "" || len(cfg.Args) > 0 || len(cfg.Env) > 0 {
			return fmt.Errorf("%w: http server %q cannot set command, args or env", ErrInvalidConfig, name)
		}
		if err := validateURL(cfg.URL); err != nil {
			return fmt.Errorf("%w: server %q: %s", ErrInvalidConfig, name, err)
		}
	}
	if len(cfg.Env) > maxEnv {
		return fmt.Errorf("%w: server %q has too many environment variables", ErrInvalidConfig, name)
	}
	envKeys := make([]string, 0, len(cfg.Env))
	for key := range cfg.Env {
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	for _, key := range envKeys {
		if !envNameRE.MatchString(key) {
			return fmt.Errorf("%w: server %q has an invalid environment variable name", ErrInvalidConfig, name)
		}
		if strings.EqualFold(key, "HOME") || dangerousName(key) {
			return fmt.Errorf("%w: server %q may not set environment variable %s", ErrInvalidConfig, name, key)
		}
		if strings.ContainsRune(cfg.Env[key], 0) {
			return fmt.Errorf("%w: server %q has an environment value containing NUL", ErrInvalidConfig, name)
		}
		if len(cfg.Env[key]) > maxFieldLen {
			return fmt.Errorf("%w: server %q has an environment value that is too long", ErrInvalidConfig, name)
		}
	}
	if len(cfg.Headers) > maxHeaders {
		return fmt.Errorf("%w: server %q has too many headers", ErrInvalidConfig, name)
	}
	headerKeys := make([]string, 0, len(cfg.Headers))
	for key := range cfg.Headers {
		headerKeys = append(headerKeys, key)
	}
	sort.Strings(headerKeys)
	for _, key := range headerKeys {
		if !headerNameRE.MatchString(key) {
			return fmt.Errorf("%w: server %q has an invalid header name", ErrInvalidConfig, name)
		}
		if strings.ContainsAny(cfg.Headers[key], "\x00\r\n") {
			return fmt.Errorf("%w: server %q has a header value containing a control character", ErrInvalidConfig, name)
		}
		if len(cfg.Headers[key]) > maxFieldLen {
			return fmt.Errorf("%w: server %q has a header value that is too long", ErrInvalidConfig, name)
		}
	}
	if len(cfg.XReview.EnvAllowlist) > maxAllowlist {
		return fmt.Errorf("%w: server %q has too many allowlisted variables", ErrInvalidConfig, name)
	}
	seenAllow := make(map[string]bool, len(cfg.XReview.EnvAllowlist))
	for _, envName := range cfg.XReview.EnvAllowlist {
		if !envNameRE.MatchString(envName) {
			return fmt.Errorf("%w: server %q has an invalid allowlisted variable", ErrInvalidConfig, name)
		}
		if strings.Contains(envName, "${") {
			return fmt.Errorf("%w: server %q has an invalid allowlisted variable", ErrInvalidConfig, name)
		}
		if seenAllow[envName] {
			return fmt.Errorf("%w: server %q has a duplicate allowlisted variable", ErrInvalidConfig, name)
		}
		seenAllow[envName] = true
	}
	if strings.ContainsRune(cfg.XReview.Home, 0) || strings.ContainsAny(cfg.XReview.Home, "\r\n") || len(cfg.XReview.Home) > 4096 {
		return fmt.Errorf("%w: server %q has an invalid home", ErrInvalidConfig, name)
	}
	if cfg.XReview.CallTimeout != 0 {
		d := cfg.XReview.CallTimeout.AsDuration()
		if d < minCallTimeout || d > maxCallTimeout {
			return fmt.Errorf("%w: server %q call_timeout is out of range", ErrInvalidConfig, name)
		}
	}
	if cfg.XReview.IdleTimeout != 0 {
		d := cfg.XReview.IdleTimeout.AsDuration()
		if d < minIdleTimeout || d > maxIdleTimeout {
			return fmt.Errorf("%w: server %q idle_timeout is out of range", ErrInvalidConfig, name)
		}
	}
	if cfg.XReview.MaxOutputChars != 0 {
		if cfg.XReview.MaxOutputChars < minOutputChars || cfg.XReview.MaxOutputChars > maxServerOutput {
			return fmt.Errorf("%w: server %q max_output_chars is out of range", ErrInvalidConfig, name)
		}
	}
	return nil
}

func validateURL(raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	if strings.ContainsAny(raw, "\x00\r\n\t") {
		return errors.New("url contains a control character")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return errors.New("invalid url")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("url scheme must be http or https")
	}
	if parsed.Host == "" {
		return errors.New("url host is required")
	}
	if parsed.User != nil {
		return errors.New("url must not contain user information")
	}
	if parsed.Fragment != "" {
		return errors.New("url must not contain a fragment")
	}
	return nil
}

// ValidateServerConfig validates a named config exactly as Parse would.
func ValidateServerConfig(name string, cfg ServerConfig) error {
	return cfg.withDefaults().Validate(name)
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON data", ErrInvalidConfig)
	}
	return nil
}

func safeJSONError(err error) string {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Sprintf("invalid JSON at byte %d", syntax.Offset)
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		if typeErr.Field != "" {
			return "invalid JSON type for " + typeErr.Field
		}
		return "invalid JSON type"
	}
	if strings.HasPrefix(err.Error(), "json: unknown field ") {
		return err.Error()
	}
	return "invalid JSON"
}

func fingerprint(cfg ServerConfig) string {
	data, err := json.Marshal(cfg.withDefaults())
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}
