package config

import (
	"strings"
	"testing"
	"time"
)

// TestWorkspaceCapacitySettings covers the #459 quota fields: both default to
// disabled and reject negative values.
func TestWorkspaceCapacitySettings(t *testing.T) {
	setRequiredEnv(t)
	cfg := loadConfig(t, writeConfig(t, "config.yaml", validGitLab))
	if cfg.Workspace.MaxBytes != 0 || cfg.Workspace.ReclaimAfter != 0 {
		t.Fatalf("default capacity = %d bytes / %s, want both disabled",
			cfg.Workspace.MaxBytes, cfg.Workspace.ReclaimAfter)
	}

	cfg.Workspace.MaxBytes = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "workspace.max_bytes") {
		t.Fatalf("negative max_bytes error = %v", err)
	}
	cfg.Workspace.MaxBytes = 0
	cfg.Workspace.ReclaimAfter = -time.Hour
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "workspace.reclaim_after") {
		t.Fatalf("negative reclaim_after error = %v", err)
	}

	cfg.Workspace.MaxBytes = 1 << 30
	cfg.Workspace.ReclaimAfter = 30 * 24 * time.Hour
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid capacity settings: %v", err)
	}
}
