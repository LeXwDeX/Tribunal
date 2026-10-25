package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func systemPlatform() *config.Config {
	return &config.Config{
		Providers: map[string]config.ProviderConfig{
			"claude": {
				Protocol: "anthropic", BaseURL: "https://api.anthropic.com",
				Model: "claude-sonnet", APIKeyEnv: "ANTHROPIC_API_KEY",
				MaxConcurrency: 4, RPM: 60,
			},
			"unused": {Protocol: "openai-chat", BaseURL: "https://llm.example.com", Model: "m"},
		},
		Profiles: map[string]config.ProfileConfig{"default": {}, "deep": {}},
		Secrets:  config.SecretsConfig{MasterKeyEnv: "REVIEW_MASTER_KEY"},
		Storage: config.StorageConfig{
			Driver: "sqlite",
			DSN:    "file:/srv/review.db?_pragma=password(hunter2)",
		},
		Auth: config.AuthConfig{
			BootstrapAdmins: []string{"alice"},
			OAuth:           config.OAuthConfig{ClientIDEnv: "GITLAB_CLIENT_ID", ClientSecretEnv: "GITLAB_CLIENT_SECRET"},
		},
		MCP:  config.MCPConfig{Config: "/etc/review/mcp.json"},
		Jobs: config.JobsConfig{Workers: 4},
		Blocks: map[string]config.BlockBinding{
			"review": {MCPTools: []string{"mcp__github__search"}, Skills: []string{"go-review"}},
		},
	}
}

func TestProvidersHideKeysAndReportStats(t *testing.T) {
	fake := newFakeReviewStore()
	fake.stats = store.Stats{
		ByModel: []store.ModelStats{
			{Provider: "claude", Model: "claude-sonnet", Calls: 10, Errors: 2, P50LatencyMS: ptr(120.0), ErrorRate: ptr(0.2)},
			{Provider: "claude", Model: "claude-haiku", Calls: 4, Errors: 1, P50LatencyMS: ptr(80.0), ErrorRate: ptr(0.25)},
		},
	}
	env := newM6Env(t, m6Options{store: fake, platform: systemPlatform()})
	client := env.loginAs("code-admin")

	response := client.request(http.MethodGet, "/api/v1/providers", nil, nil)
	assertStatus(t, response, http.StatusOK)
	text := string(response.body)
	for _, forbidden := range []string{"ANTHROPIC_API_KEY", "api_key_env", "secret"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("providers leaked %q: %s", forbidden, text)
		}
	}
	body := env.decode(t, response)
	providers := body["providers"].([]any)
	if len(providers) != 2 {
		t.Fatalf("providers = %v", providers)
	}
	first := providers[0].(map[string]any)
	if first["name"] != "claude" || first["protocol"] != "anthropic" {
		t.Fatalf("provider = %v", first)
	}
	stats := first["stats_24h"].(map[string]any)
	if stats["calls"] != float64(14) || stats["errors"] != float64(3) {
		t.Fatalf("stats_24h = %v", stats)
	}
	rate := stats["error_rate"].(float64)
	if rate < 0 || rate > 1 {
		t.Fatalf("error_rate outside [0,1]: %v", rate)
	}
	if rate != 3.0/14.0 {
		t.Fatalf("error_rate = %v", rate)
	}
	if stats["p50_latency_ms"] != float64(120) {
		t.Fatalf("p50 = %v", stats["p50_latency_ms"])
	}
	second := providers[1].(map[string]any)
	unusedStats := second["stats_24h"].(map[string]any)
	if unusedStats["calls"] != float64(0) || unusedStats["error_rate"] != nil || unusedStats["p50_latency_ms"] != nil {
		t.Fatalf("unused provider stats = %v", unusedStats)
	}

	// The 24h window ends at now and starts 24h earlier.
	if fake.lastStats.From.IsZero() || fake.lastStats.To.IsZero() {
		t.Fatalf("stats filter = %+v", fake.lastStats)
	}
	if fake.lastStats.To.Sub(fake.lastStats.From).Hours() != 24 {
		t.Fatalf("stats window = %v", fake.lastStats.To.Sub(fake.lastStats.From))
	}
}

func TestProviderTestRequiresOKBoolean(t *testing.T) {
	env := newM6Env(t, m6Options{platform: systemPlatform()})
	env.llm.result = ProviderTestResult{Model: "claude-sonnet", LatencyMS: 42}
	client := env.loginAs("code-admin")

	success := client.request(http.MethodPost, "/api/v1/providers/claude/test", nil, nil)
	assertStatus(t, success, http.StatusOK)
	body := env.decode(t, success)
	if body["ok"] != true || body["model"] != "claude-sonnet" || body["latency_ms"] != float64(42) {
		t.Fatalf("success body = %v", body)
	}
	if len(env.llm.names) != 1 || env.llm.names[0] != "claude" {
		t.Fatalf("tester names = %v", env.llm.names)
	}

	env.llm.err = ioError("dial tcp 10.0.0.1:443: connection refused")
	failure := client.request(http.MethodPost, "/api/v1/providers/claude/test", nil, nil)
	assertStatus(t, failure, http.StatusOK)
	failureBody := env.decode(t, failure)
	if failureBody["ok"] != false {
		t.Fatalf("failure body = %v", failureBody)
	}
	if failureBody["message"] == nil {
		t.Fatalf("failure has no message: %v", failureBody)
	}
	if strings.Contains(string(failure.body), "10.0.0.1") {
		t.Fatalf("failure leaked provider details: %s", failure.body)
	}

	unknown := client.request(http.MethodPost, "/api/v1/providers/missing/test", nil, nil)
	assertStatus(t, unknown, http.StatusNotFound)
}

type ioError string

func (e ioError) Error() string { return string(e) }

func TestStatsQualityByProviderJSON(t *testing.T) {
	fake := newFakeReviewStore()
	fake.stats = store.Stats{QualityByProvider: []store.ProviderQualityStats{{
		Provider: "claude", Proposed: 4, Reviewed: 2, Valid: 1, Invalid: 1,
		Accuracy: ptr(0.5), Overturned: 3, OverturnedRatio: ptr(0.75),
		Refuted: 4, RefutedReviewed: 3, FalseRefuted: 2, FalseKillRate: ptr(2.0 / 3.0),
	}}}
	env := newM6Env(t, m6Options{store: fake, platform: systemPlatform()})
	client := env.businessClient()

	response := client.request(http.MethodGet, "/api/v1/stats", nil, nil)
	assertStatus(t, response, http.StatusOK)
	rows := env.decode(t, response)["quality_by_provider"].([]any)
	if len(rows) != 1 {
		t.Fatalf("quality_by_provider = %v", rows)
	}
	row := rows[0].(map[string]any)
	for field, want := range map[string]float64{
		"proposed": 4, "reviewed": 2, "valid": 1, "invalid": 1, "accuracy": 0.5,
		"overturned": 3, "overturned_ratio": 0.75, "refuted": 4,
		"refuted_reviewed": 3, "false_refuted": 2, "false_kill_rate": 2.0 / 3.0,
	} {
		if got := row[field]; got != want {
			t.Fatalf("%s = %v, want %v in %v", field, got, want, row)
		}
	}
	if row["provider"] != "claude" {
		t.Fatalf("provider = %v", row["provider"])
	}
}

func TestStatsKeepsUndefinedValuesNull(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake, platform: systemPlatform()})
	client := env.businessClient()

	response := client.request(http.MethodGet, "/api/v1/stats?from=2026-10-01T00:00:00Z&to=2026-10-21T00:00:00Z&project_id=42", nil, nil)
	assertStatus(t, response, http.StatusOK)
	var body map[string]any
	if err := json.Unmarshal(response.body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, field := range []string{"blocking_ratio", "overturned_ratio", "avg_duration_ms", "p95_duration_ms"} {
		value, present := body[field]
		if !present || value != nil {
			t.Fatalf("%s = %v, want null", field, value)
		}
	}
	if body["duration_samples"] != float64(0) {
		t.Fatalf("duration_samples = %v, want 0", body["duration_samples"])
	}
	for _, field := range []string{"trend", "by_provider", "by_block", "quality_by_provider"} {
		value, present := body[field]
		if !present || value == nil {
			t.Fatalf("%s = %v, want an empty array", field, value)
		}
		if values, ok := value.([]any); !ok || len(values) != 0 {
			t.Fatalf("%s = %v, want an empty array", field, value)
		}
	}
	if body["by_project"] == nil || body["by_model"] == nil || body["decisions"] == nil {
		t.Fatalf("collections must be arrays/objects: %v", body)
	}
	if len(body["by_project"].([]any)) != 0 || len(body["by_model"].([]any)) != 0 {
		t.Fatalf("collections = %v", body)
	}
	if fake.lastStats.ProjectID == nil || *fake.lastStats.ProjectID != 42 {
		t.Fatalf("project filter = %+v", fake.lastStats)
	}

	invalid := client.request(http.MethodGet, "/api/v1/stats?from=2026-10-21T00:00:00Z&to=2026-10-01T00:00:00Z", nil, nil)
	assertStatus(t, invalid, http.StatusBadRequest)
}

func TestStatsFeedbackJSON(t *testing.T) {
	cases := []struct {
		name       string
		feedback   *store.FeedbackTotals
		wantField  bool
		wantTotal  float64
		want       map[string]float64
		wantByRole map[string][]float64
	}{
		{
			name: "counts",
			feedback: &store.FeedbackTotals{
				Total:    3,
				BySignal: map[string]int64{"thumbsup": 2, "thumbsdown": 1},
				ByProvider: []store.FeedbackProviderStats{
					{
						Provider: "anthropic", Role: "proposer", Total: 2,
						BySignal: map[string]int64{"thumbsup": 2},
					},
					{
						Provider: "openai", Role: "challenger", Total: 1,
						BySignal: map[string]int64{"thumbsdown": 1},
					},
				},
			},
			wantField: true,
			wantTotal: 3,
			want:      map[string]float64{"thumbsup": 2, "thumbsdown": 1},
			wantByRole: map[string][]float64{
				"anthropic/proposer": {2, 2},
				"openai/challenger":  {1, 1},
			},
		},
		{
			name:      "known empty",
			feedback:  &store.FeedbackTotals{},
			wantField: true,
			wantTotal: 0,
			want:      map[string]float64{},
		},
		{
			name:      "missing",
			feedback:  nil,
			wantField: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeReviewStore()
			fake.stats = store.Stats{Feedback: tc.feedback}
			env := newM6Env(t, m6Options{store: fake, platform: systemPlatform()})
			client := env.businessClient()

			response := client.request(http.MethodGet, "/api/v1/stats?from=2026-10-01T00:00:00Z&to=2026-10-21T00:00:00Z&project_id=42", nil, nil)
			assertStatus(t, response, http.StatusOK)
			body := env.decode(t, response)
			feedback, present := body["feedback"]
			if present != tc.wantField {
				t.Fatalf("feedback present = %v, want %v: %v", present, tc.wantField, body)
			}
			if !tc.wantField {
				return
			}
			view := feedback.(map[string]any)
			if view["total"] != tc.wantTotal {
				t.Fatalf("feedback.total = %v, want %v", view["total"], tc.wantTotal)
			}
			bySignal := view["by_signal"].(map[string]any)
			if len(bySignal) != len(tc.want) {
				t.Fatalf("feedback.by_signal = %v, want %v", bySignal, tc.want)
			}
			for signal, count := range tc.want {
				if bySignal[signal] != count {
					t.Fatalf("feedback.by_signal[%s] = %v, want %v", signal, bySignal[signal], count)
				}
			}
			byProvider, ok := view["by_provider"].([]any)
			if !ok {
				t.Fatalf("feedback.by_provider = %v, want an array", view["by_provider"])
			}
			if len(byProvider) != len(tc.wantByRole) {
				t.Fatalf("feedback.by_provider = %v, want %d records", byProvider, len(tc.wantByRole))
			}
			for _, raw := range byProvider {
				entry := raw.(map[string]any)
				key := entry["provider"].(string) + "/" + entry["role"].(string)
				want, ok := tc.wantByRole[key]
				if !ok {
					t.Fatalf("unexpected by_provider record %s: %v", key, byProvider)
				}
				if entry["total"] != want[0] {
					t.Fatalf("by_provider[%s].total = %v, want %v", key, entry["total"], want[0])
				}
				signals := entry["by_signal"].(map[string]any)
				if len(signals) != 1 {
					t.Fatalf("by_provider[%s].by_signal = %v", key, signals)
				}
				for _, count := range signals {
					if count != want[1] {
						t.Fatalf("by_provider[%s].by_signal = %v, want %v", key, signals, want[1])
					}
				}
			}
		})
	}
}

func TestStatsTrendJSON(t *testing.T) {
	fake := newFakeReviewStore()
	fake.stats = store.Stats{
		DurationSamples: 2,
		Trend: []store.DailyStats{
			{Day: "2026-10-20", Runs: 1, CostMicros: 5, DurationSamples: 1, AvgDurationMS: ptr(1000.0), P95DurationMS: ptr(1000.0)},
			{Day: "2026-10-21", Runs: 1, CostMicros: 0, DurationSamples: 0},
		},
		ByProject: []store.ProjectStats{{
			ProjectID: 42, Runs: 1, CostMicros: 5, DurationSamples: 1,
			AvgDurationMS: ptr(1000.0), P95DurationMS: ptr(1000.0),
		}},
		ByProvider: []store.ProviderStats{
			{
				Provider: "anthropic", Runs: 1, CostMicros: 5, DurationSamples: 1,
				AvgDurationMS: ptr(1000.0), P95DurationMS: ptr(1000.0),
				Trend: []store.DailyStats{{Day: "2026-10-20", Runs: 1, CostMicros: 5, DurationSamples: 1, AvgDurationMS: ptr(1000.0), P95DurationMS: ptr(1000.0)}},
			},
			{Provider: "openai", Runs: 1},
		},
		ByBlock: []store.BlockStats{{
			Block: "code-review", Runs: 1, CostMicros: 5, DurationSamples: 1,
			AvgDurationMS: ptr(1000.0), P95DurationMS: ptr(1000.0),
		}},
	}
	env := newM6Env(t, m6Options{store: fake, platform: systemPlatform()})
	client := env.businessClient()

	response := client.request(http.MethodGet, "/api/v1/stats?from=2026-10-01T00:00:00Z&to=2026-10-21T00:00:00Z&project_id=42", nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)

	if body["duration_samples"] != float64(2) {
		t.Fatalf("duration_samples = %v", body["duration_samples"])
	}
	trend := body["trend"].([]any)
	if len(trend) != 2 {
		t.Fatalf("trend = %v", trend)
	}
	first := trend[0].(map[string]any)
	if first["day"] != "2026-10-20" || first["runs"] != float64(1) || first["cost_micros"] != float64(5) ||
		first["duration_samples"] != float64(1) || first["avg_duration_ms"] != float64(1000) ||
		first["p95_duration_ms"] != float64(1000) {
		t.Fatalf("first trend = %v", first)
	}
	second := trend[1].(map[string]any)
	if second["day"] != "2026-10-21" || second["avg_duration_ms"] != nil || second["p95_duration_ms"] != nil {
		t.Fatalf("second trend = %v", second)
	}

	projects := body["by_project"].([]any)
	project := projects[0].(map[string]any)
	if project["duration_samples"] != float64(1) || project["avg_duration_ms"] != float64(1000) ||
		project["p95_duration_ms"] != float64(1000) {
		t.Fatalf("project stats = %v", project)
	}
	if project["trend"] == nil {
		t.Fatalf("project trend = nil, want a known empty array")
	}
	if values, ok := project["trend"].([]any); !ok || len(values) != 0 {
		t.Fatalf("project trend = %v, want []", project["trend"])
	}

	providers := body["by_provider"].([]any)
	if len(providers) != 2 {
		t.Fatalf("by_provider = %v", providers)
	}
	anthropic := providers[0].(map[string]any)
	if anthropic["provider"] != "anthropic" || anthropic["runs"] != float64(1) ||
		anthropic["cost_micros"] != float64(5) || anthropic["duration_samples"] != float64(1) ||
		anthropic["avg_duration_ms"] != float64(1000) || anthropic["p95_duration_ms"] != float64(1000) {
		t.Fatalf("anthropic = %v", anthropic)
	}
	if trend, ok := anthropic["trend"].([]any); !ok || len(trend) != 1 {
		t.Fatalf("anthropic trend = %v", anthropic["trend"])
	}
	openai := providers[1].(map[string]any)
	if openai["provider"] != "openai" || openai["avg_duration_ms"] != nil || openai["p95_duration_ms"] != nil {
		t.Fatalf("openai = %v", openai)
	}
	if values, ok := openai["trend"].([]any); !ok || len(values) != 0 {
		t.Fatalf("openai trend = %v, want []", openai["trend"])
	}

	blocks := body["by_block"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("by_block = %v", blocks)
	}
	block := blocks[0].(map[string]any)
	if block["block"] != "code-review" || block["runs"] != float64(1) ||
		block["avg_duration_ms"] != float64(1000) {
		t.Fatalf("block stats = %v", block)
	}
	if values, ok := block["trend"].([]any); !ok || len(values) != 0 {
		t.Fatalf("block trend = %v, want []", block["trend"])
	}
}

func TestProfilesComeFromConfig(t *testing.T) {
	env := newM6Env(t, m6Options{platform: systemPlatform()})
	client := env.loginAs("code-admin")

	response := client.request(http.MethodGet, "/api/v1/profiles", nil, nil)
	assertStatus(t, response, http.StatusOK)
	profiles := env.decode(t, response)["profiles"].([]any)
	if len(profiles) != 2 {
		t.Fatalf("profiles = %v", profiles)
	}
	first := profiles[0].(map[string]any)
	if first["name"] != "deep" || first["config"] == nil {
		t.Fatalf("profile = %v", first)
	}
}

func TestSettingsMaskSecrets(t *testing.T) {
	env := newM6Env(t, m6Options{platform: systemPlatform()})
	client := env.loginAs("code-admin")

	response := client.request(http.MethodGet, "/api/v1/settings", nil, nil)
	assertStatus(t, response, http.StatusOK)
	text := string(response.body)
	for _, forbidden := range []string{
		"ANTHROPIC_API_KEY", "REVIEW_MASTER_KEY", "hunter2", "GITLAB_CLIENT_SECRET",
		"/srv/review.db", "file:",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("settings leaked %q: %s", forbidden, text)
		}
	}
	body := env.decode(t, response)
	providers := body["providers"].([]any)
	if len(providers) != 2 {
		t.Fatalf("providers = %v", providers)
	}
	if providers[0].(map[string]any)["api_key_configured"] != true {
		t.Fatalf("first provider = %v", providers[0])
	}
	if providers[1].(map[string]any)["api_key_configured"] != false {
		t.Fatalf("second provider = %v", providers[1])
	}
	secrets := body["secrets"].(map[string]any)
	if secrets["master_key_configured"] != true {
		t.Fatalf("secrets = %v", secrets)
	}
	auth := body["auth"].(map[string]any)
	if auth["oauth_configured"] != true {
		t.Fatalf("auth = %v", auth)
	}
	blocks := body["blocks"].(map[string]any)
	if blocks["review"] == nil {
		t.Fatalf("blocks = %v", blocks)
	}
}
