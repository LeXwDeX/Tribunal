package config

import (
	"strings"
	"testing"
	"time"
)

func TestLimitsDefaults(t *testing.T) {
	setRequiredEnv(t)
	cfg := loadConfig(t, writeConfig(t, "config.yaml", validGitLab))
	want := LimitsConfig{RegisterPerMinute: 10, ValidatePerMinute: 30, PrecheckCacheTTL: 45 * time.Second,
		StreamMaxConcurrent: 64, StreamMaxPerUser: 4}
	if cfg.Limits != want {
		t.Fatalf("limits = %+v, want %+v", cfg.Limits, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestLimitsValidationRejectsInvalidValues(t *testing.T) {
	setRequiredEnv(t)
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "register zero",
			body: "limits:\n  register_per_minute: 0\n  validate_per_minute: 30\n  precheck_cache_ttl: 45s\n",
			want: "limits.register_per_minute must be positive",
		},
		{
			name: "register negative",
			body: "limits:\n  register_per_minute: -1\n  validate_per_minute: 30\n  precheck_cache_ttl: 45s\n",
			want: "limits.register_per_minute must be positive",
		},
		{
			name: "validate zero",
			body: "limits:\n  register_per_minute: 10\n  validate_per_minute: 0\n  precheck_cache_ttl: 45s\n",
			want: "limits.validate_per_minute must be positive",
		},
		{
			name: "validate negative",
			body: "limits:\n  register_per_minute: 10\n  validate_per_minute: -5\n  precheck_cache_ttl: 45s\n",
			want: "limits.validate_per_minute must be positive",
		},
		{
			name: "negative cache TTL",
			body: "limits:\n  register_per_minute: 10\n  validate_per_minute: 30\n  precheck_cache_ttl: -1s\n",
			want: "limits.precheck_cache_ttl must not be negative",
		},
		{
			name: "negative stream total",
			body: "limits:\n  register_per_minute: 10\n  validate_per_minute: 30\n  precheck_cache_ttl: 45s\n  stream_max_concurrent: -1\n",
			want: "limits.stream_max_concurrent must not be negative",
		},
		{
			name: "negative stream per user",
			body: "limits:\n  register_per_minute: 10\n  validate_per_minute: 30\n  precheck_cache_ttl: 45s\n  stream_max_per_user: -1\n",
			want: "limits.stream_max_per_user must not be negative",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadConfig(t, writeConfig(t, "config.yaml", validGitLab+tc.body))
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLimitsPrecheckCacheTTLZeroDisablesCaching(t *testing.T) {
	setRequiredEnv(t)
	cfg := loadConfig(t, writeConfig(t, "config.yaml", validGitLab+
		"limits:\n  register_per_minute: 10\n  validate_per_minute: 30\n  precheck_cache_ttl: 0s\n"))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if cfg.Limits.PrecheckCacheTTL != 0 {
		t.Fatalf("precheck_cache_ttl = %v, want 0", cfg.Limits.PrecheckCacheTTL)
	}
	if cfg.Limits.RegisterPerMinute != 10 || cfg.Limits.ValidatePerMinute != 30 {
		t.Fatalf("limits = %+v, want 10/30", cfg.Limits)
	}
}
