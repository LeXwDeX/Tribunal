// Command tribunalctl is a thin HTTP client for the M0 management API.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultURL     = "http://localhost:8080"
	defaultTimeout = 30 * time.Second
	maxResponse    = 4 << 20
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr, nil); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "tribunalctl:", err)
		os.Exit(1)
	}
}

type globals struct {
	serviceURL  string
	apiTokenEnv string
	timeout     time.Duration
	apiToken    string
	httpClient  *http.Client
	stdout      io.Writer
	stderr      io.Writer
	getenv      func(string) string
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, client *http.Client) error {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	g := &globals{stdout: stdout, stderr: stderr, getenv: getenv}

	global := flag.NewFlagSet("tribunalctl", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() {
		commandHelp.usage(stderr, "tribunalctl")
		global.PrintDefaults()
	}
	global.StringVar(&g.serviceURL, "url", defaultURL, "service base URL")
	global.StringVar(&g.apiTokenEnv, "api-token-env", "REVIEW_ADMIN_TOKEN", "environment variable holding the admin token")
	global.DurationVar(&g.timeout, "timeout", defaultTimeout, "HTTP timeout")
	if err := global.Parse(args); err != nil {
		return err
	}
	if node, path, ok := helpRequest(commandHelp, global.Args()); ok {
		if node == commandHelp {
			global.Usage()
		} else {
			node.usage(stderr, path)
		}
		return flag.ErrHelp
	}
	if err := g.prepare(getenv, client); err != nil {
		return err
	}
	rest := global.Args()
	if len(rest) == 0 {
		global.Usage()
		return errors.New("projects or users subcommand is required")
	}
	switch rest[0] {
	case "projects":
		return g.runProjects(ctx, rest[1:])
	case "users":
		return g.runUsers(ctx, rest[1:])
	default:
		global.Usage()
		return fmt.Errorf("unknown subcommand %q", rest[0])
	}
}

// helpNode describes one command level for -h, --help and help handling.
type helpNode struct {
	args     string
	actions  []string
	children map[string]*helpNode
}

func (n *helpNode) usage(w io.Writer, path string) {
	if n.args != "" {
		fmt.Fprintf(w, "Usage: %s %s\n", path, n.args)
	} else {
		fmt.Fprintf(w, "Usage: %s\n", path)
	}
	if len(n.actions) > 0 {
		fmt.Fprintln(w, "Actions:", strings.Join(n.actions, ", "))
	}
}

// helpRequest returns the command level targeted by -h, --help or help.
// Unknown command words are not help so they still fail as before.
func helpRequest(root *helpNode, args []string) (*helpNode, string, bool) {
	node, path := root, "tribunalctl"
	for _, arg := range args {
		switch arg {
		case "-h", "--help":
			return node, path, true
		case "help":
			// A bare help word only acts as help where a subcommand is expected,
			// so positional values such as "projects get help" keep failing.
			if len(node.children) > 0 {
				return node, path, true
			}
			continue
		}
		child, ok := node.children[arg]
		if !ok {
			if len(node.children) == 0 {
				continue // leaf command: skip its positional arguments
			}
			return nil, "", false
		}
		node = child
		path += " " + arg
	}
	return nil, "", false
}

var commandHelp = &helpNode{
	args:    "[--url URL] [--api-token-env NAME] <projects|users> <action> [options]",
	actions: []string{"projects", "users"},
	children: map[string]*helpNode{
		"projects": {
			args:    "<action> [options]",
			actions: []string{"register", "list", "get", "all", "retry-onboarding", "grants", "residues", "webhook"},
			children: map[string]*helpNode{
				"register":         {args: "--project ID --token-env NAME [--profile PROFILE] [--auto-webhook] [--settings JSON]"},
				"list":             {},
				"get":              {args: "<project ID>"},
				"retry-onboarding": {args: "<project ID>"},
				"all":              {},
				"grants": {
					args:    "<action> [options]",
					actions: []string{"list", "set", "delete"},
					children: map[string]*helpNode{
						"list":   {args: "<project ID>"},
						"set":    {args: "<project ID> <user ID> <read|manage>"},
						"delete": {args: "<project ID> <user ID>"},
					},
				},
				"residues": {
					args:    "<action> [options]",
					actions: []string{"list (default)", "clean"},
					children: map[string]*helpNode{
						"clean": {args: "--yes"},
					},
				},
				"webhook": {
					args:    "<action> <project ID>",
					actions: []string{"enable", "rebuild", "disable"},
					children: map[string]*helpNode{
						"enable":  {args: "<project ID>"},
						"rebuild": {args: "<project ID>"},
						"disable": {args: "<project ID>"},
					},
				},
			},
		},
		"users": {
			args:    "<action> [options]",
			actions: []string{"list", "disable", "enable", "revoke"},
			children: map[string]*helpNode{
				"list":    {},
				"disable": {args: "<user ID>"},
				"enable":  {args: "<user ID>"},
				"revoke":  {args: "<user ID>"},
			},
		},
	},
}

func (g *globals) prepare(getenv func(string) string, client *http.Client) error {
	normalized, err := validateServiceURL(g.serviceURL)
	if err != nil {
		return err
	}
	g.serviceURL = normalized
	if strings.TrimSpace(g.apiTokenEnv) == "" || !envNameRE.MatchString(g.apiTokenEnv) {
		return errors.New("--api-token-env must be a valid environment variable name")
	}
	g.apiToken = getenv(g.apiTokenEnv)
	if strings.TrimSpace(g.apiToken) == "" {
		return fmt.Errorf("environment variable %s is not set or empty", g.apiTokenEnv)
	}
	if g.timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	if client == nil {
		client = &http.Client{}
	}
	copied := *client
	copied.Timeout = g.timeout
	copied.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	g.httpClient = &copied
	return nil
}

func validateServiceURL(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", errors.New("--url must be an absolute http(s) URL without credentials, query, or fragment")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		(u.Scheme != "http" && u.Scheme != "https") ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("--url must be an absolute http(s) URL without credentials, query, or fragment")
	}
	return strings.TrimRight(raw, "/"), nil
}

func (g *globals) runProjects(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("projects action is required")
	}
	switch args[0] {
	case "register":
		return g.register(ctx, args[1:])
	case "list":
		if len(args) != 1 {
			return errors.New("projects list takes no arguments")
		}
		return g.listProjects(ctx)
	case "get":
		if len(args) != 2 {
			return errors.New("projects get requires exactly one project ID")
		}
		id, err := parsePositiveID(args[1], "project ID")
		if err != nil {
			return err
		}
		raw, err := g.request(ctx, http.MethodGet, "/api/v1/projects/"+strconv.FormatInt(id, 10), nil)
		if err != nil {
			return err
		}
		return writeRaw(g.stdout, raw)
	case "all":
		if len(args) != 1 {
			return errors.New("projects all takes no arguments")
		}
		return g.listAllProjects(ctx)
	case "retry-onboarding":
		if len(args) != 2 {
			return errors.New("projects retry-onboarding requires exactly one project ID")
		}
		id, err := parsePositiveID(args[1], "project ID")
		if err != nil {
			return err
		}
		raw, err := g.request(ctx, http.MethodPost,
			"/api/v1/admin/projects/"+strconv.FormatInt(id, 10)+"/onboarding/retry", nil)
		if err != nil {
			return err
		}
		return writeRaw(g.stdout, raw)
	case "grants":
		return g.runGrants(ctx, args[1:])
	case "residues":
		return g.runProjectResidues(ctx, args[1:])
	case "webhook":
		return g.projectWebhook(ctx, args[1:])
	default:
		return fmt.Errorf("unknown projects action %q", args[0])
	}
}

// projectWebhook enables, rebuilds or disables the service-managed hook.
func (g *globals) projectWebhook(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("projects webhook action is required")
	}
	if len(args) != 2 {
		return fmt.Errorf("projects webhook %s requires exactly one project ID", args[0])
	}
	id, err := parsePositiveID(args[1], "project ID")
	if err != nil {
		return err
	}
	var method, path string
	switch args[0] {
	case "enable":
		method, path = http.MethodPost, "/api/v1/projects/"+strconv.FormatInt(id, 10)+"/webhook"
	case "rebuild":
		method, path = http.MethodPost, "/api/v1/projects/"+strconv.FormatInt(id, 10)+"/webhook/rebuild"
	case "disable":
		method, path = http.MethodDelete, "/api/v1/projects/"+strconv.FormatInt(id, 10)+"/webhook"
	default:
		return fmt.Errorf("unknown projects webhook action %q", args[0])
	}
	raw, err := g.request(ctx, method, path, nil)
	if err != nil {
		return err
	}
	return writeRaw(g.stdout, raw)
}

// runProjectResidues lists leftover local project data, or removes the part
// that provably belongs to an unregistered project after an explicit --yes.
func (g *globals) runProjectResidues(ctx context.Context, args []string) error {
	if len(args) == 0 {
		raw, err := g.request(ctx, http.MethodGet, "/api/v1/admin/projects/residues", nil)
		if err != nil {
			return err
		}
		return writeRaw(g.stdout, raw)
	}
	switch args[0] {
	case "clean":
		fs := flag.NewFlagSet("tribunalctl projects residues clean", flag.ContinueOnError)
		fs.SetOutput(g.stderr)
		yes := fs.Bool("yes", false, "remove residue that belongs to an unregistered project")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() > 0 {
			return errors.New("projects residues clean takes no positional arguments")
		}
		if !*yes {
			return errors.New("projects residues clean requires --yes")
		}
		raw, err := g.request(ctx, http.MethodPost,
			"/api/v1/admin/projects/residues/cleanup", map[string]any{"confirm": true})
		if err != nil {
			return err
		}
		return writeRaw(g.stdout, raw)
	default:
		return fmt.Errorf("unknown projects residues action %q", args[0])
	}
}

func (g *globals) runUsers(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("users action is required")
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("users list takes no arguments")
		}
		return g.listUsers(ctx)
	case "disable", "enable":
		if len(args) != 2 {
			return fmt.Errorf("users %s requires exactly one user ID", args[0])
		}
		id, err := parsePositiveID(args[1], "user ID")
		if err != nil {
			return err
		}
		raw, err := g.request(ctx, http.MethodPatch,
			"/api/v1/admin/users/"+strconv.FormatInt(id, 10),
			map[string]any{"disabled": args[0] == "disable"})
		if err != nil {
			return err
		}
		return writeRaw(g.stdout, raw)
	case "revoke":
		if len(args) != 2 {
			return errors.New("users revoke requires exactly one user ID")
		}
		id, err := parsePositiveID(args[1], "user ID")
		if err != nil {
			return err
		}
		_, err = g.request(ctx, http.MethodDelete,
			"/api/v1/admin/users/"+strconv.FormatInt(id, 10)+"/credentials", nil)
		return err
	default:
		return fmt.Errorf("unknown users action %q", args[0])
	}
}

func (g *globals) runGrants(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("projects grants action is required")
	}
	switch args[0] {
	case "list":
		if len(args) != 2 {
			return errors.New("projects grants list requires exactly one project ID")
		}
		projectID, err := parsePositiveID(args[1], "project ID")
		if err != nil {
			return err
		}
		raw, err := g.request(ctx, http.MethodGet,
			"/api/v1/admin/projects/"+strconv.FormatInt(projectID, 10)+"/grants", nil)
		if err != nil {
			return err
		}
		return writeRaw(g.stdout, raw)
	case "set":
		if len(args) != 4 {
			return errors.New("projects grants set requires <project ID> <user ID> <read|manage>")
		}
		projectID, err := parsePositiveID(args[1], "project ID")
		if err != nil {
			return err
		}
		userID, err := parsePositiveID(args[2], "user ID")
		if err != nil {
			return err
		}
		if args[3] != "read" && args[3] != "manage" {
			return errors.New("grant role must be read or manage")
		}
		raw, err := g.request(ctx, http.MethodPut,
			"/api/v1/admin/projects/"+strconv.FormatInt(projectID, 10)+"/grants/"+strconv.FormatInt(userID, 10),
			map[string]any{"role": args[3]})
		if err != nil {
			return err
		}
		return writeRaw(g.stdout, raw)
	case "delete":
		if len(args) != 3 {
			return errors.New("projects grants delete requires <project ID> <user ID>")
		}
		projectID, err := parsePositiveID(args[1], "project ID")
		if err != nil {
			return err
		}
		userID, err := parsePositiveID(args[2], "user ID")
		if err != nil {
			return err
		}
		_, err = g.request(ctx, http.MethodDelete,
			"/api/v1/admin/projects/"+strconv.FormatInt(projectID, 10)+"/grants/"+strconv.FormatInt(userID, 10), nil)
		return err
	default:
		return fmt.Errorf("unknown projects grants action %q", args[0])
	}
}

func parsePositiveID(raw, label string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", label)
	}
	return id, nil
}

// listAllProjects follows next_cursor over the minimal management list.
func (g *globals) listAllProjects(ctx context.Context) error {
	all := make([]json.RawMessage, 0)
	cursor := ""
	seen := map[string]bool{}
	for {
		path := "/api/v1/admin/projects"
		if cursor != "" {
			path += "?cursor=" + url.QueryEscape(cursor)
		}
		raw, err := g.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		var page struct {
			Projects   []json.RawMessage `json:"projects"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return errors.New("server returned an invalid admin project list")
		}
		all = append(all, page.Projects...)
		if page.NextCursor == "" {
			break
		}
		if seen[page.NextCursor] {
			return errors.New("server returned a repeating project cursor")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
	merged, err := json.Marshal(map[string]any{"projects": all})
	if err != nil {
		return errors.New("encode admin project list")
	}
	return writeRaw(g.stdout, merged)
}

func (g *globals) listUsers(ctx context.Context) error {
	all := make([]json.RawMessage, 0)
	cursor := ""
	seen := map[string]bool{}
	for {
		path := "/api/v1/admin/users"
		if cursor != "" {
			path += "?cursor=" + url.QueryEscape(cursor)
		}
		raw, err := g.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		var page struct {
			Users      []json.RawMessage `json:"users"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return errors.New("server returned an invalid user list")
		}
		all = append(all, page.Users...)
		if page.NextCursor == "" {
			break
		}
		if seen[page.NextCursor] {
			return errors.New("server returned a repeating user cursor")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
	merged, err := json.Marshal(map[string]any{"users": all})
	if err != nil {
		return errors.New("encode user list")
	}
	return writeRaw(g.stdout, merged)
}

// listProjects follows next_cursor so the CLI keeps its historical "list all
// projects" contract. An old server without next_cursor yields one page.
func (g *globals) listProjects(ctx context.Context) error {
	all := make([]json.RawMessage, 0)
	cursor := ""
	seen := map[string]bool{}
	for {
		path := "/api/v1/projects"
		if cursor != "" {
			path += "?cursor=" + url.QueryEscape(cursor)
		}
		raw, err := g.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		var page struct {
			Projects   []json.RawMessage `json:"projects"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return errors.New("server returned an invalid project list")
		}
		all = append(all, page.Projects...)
		if page.NextCursor == "" {
			break
		}
		if seen[page.NextCursor] {
			return errors.New("server returned a repeating project cursor")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
	merged, err := json.Marshal(map[string]any{"projects": all})
	if err != nil {
		return errors.New("encode project list")
	}
	return writeRaw(g.stdout, merged)
}

type registerRequest struct {
	Project     string          `json:"project"`
	Token       string          `json:"token"`
	Profile     string          `json:"profile,omitempty"`
	AutoWebhook bool            `json:"auto_webhook,omitempty"`
	Settings    json.RawMessage `json:"settings,omitempty"`
}

func (g *globals) register(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tribunalctl projects register", flag.ContinueOnError)
	fs.SetOutput(g.stderr)
	var (
		project     string
		tokenEnv    string
		profile     string
		autoWebhook bool
		settings    string
	)
	fs.StringVar(&project, "project", "", "GitLab project ID or namespace/path")
	fs.StringVar(&tokenEnv, "token-env", "", "environment variable holding the project token")
	fs.StringVar(&profile, "profile", "", "review profile")
	fs.BoolVar(&autoWebhook, "auto-webhook", false, "create the GitLab webhook")
	fs.StringVar(&settings, "settings", "", "JSON project settings")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("projects register takes no positional arguments")
	}
	if project == "" {
		return errors.New("--project is required")
	}
	if tokenEnv == "" || !envNameRE.MatchString(tokenEnv) {
		return errors.New("--token-env must be a valid environment variable name")
	}
	token := g.getenv(tokenEnv)
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("environment variable %s is not set or empty", tokenEnv)
	}
	var rawSettings json.RawMessage
	if settings != "" {
		if !json.Valid([]byte(settings)) {
			return errors.New("--settings must be valid JSON")
		}
		trimmed := bytes.TrimSpace([]byte(settings))
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return errors.New("--settings must be a JSON object")
		}
		rawSettings = json.RawMessage(trimmed)
	}
	body := registerRequest{
		Project:     project,
		Token:       token,
		Profile:     profile,
		AutoWebhook: autoWebhook,
		Settings:    rawSettings,
	}
	raw, err := g.request(ctx, http.MethodPost, "/api/v1/projects", body)
	if err != nil {
		return err
	}
	return writeRaw(g.stdout, raw)
}

func (g *globals) request(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, errors.New("encode request: invalid input")
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.serviceURL+path, reader)
	if err != nil {
		return nil, errors.New("build request: invalid URL")
	}
	req.Header.Set("Authorization", "Bearer "+g.apiToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %s", sanitizeTransportError(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, errors.New("read response: transport error")
	}
	if len(data) > maxResponse {
		return nil, errors.New("read response: response is too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, responseError(resp.StatusCode, data)
	}
	if len(data) == 0 {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid(data) {
		return nil, errors.New("server returned invalid JSON")
	}
	return json.RawMessage(data), nil
}

type errorEnvelope struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func responseError(status int, body []byte) error {
	var envelope errorEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Code != "" {
		message := fmt.Sprintf("server returned HTTP %d (%s)", status, envelope.Error.Code)
		if envelope.Error.RequestID != "" {
			message += " request_id=" + envelope.Error.RequestID
		}
		if envelope.Error.Message != "" {
			message += ": " + envelope.Error.Message
		}
		return errors.New(message)
	}
	// Do not echo arbitrary response text; it may contain upstream details.
	return fmt.Errorf("server returned HTTP %d", status)
}

func sanitizeTransportError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return "timeout"
	}
	return "transport error"
}

func writeRaw(w io.Writer, raw json.RawMessage) error {
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, raw, "", "  "); err != nil {
		return errors.New("server returned invalid JSON")
	}
	formatted.WriteByte('\n')
	_, err := w.Write(formatted.Bytes())
	return err
}
