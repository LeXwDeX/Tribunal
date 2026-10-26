package config

import (
	"reflect"
	"testing"
)

var wantDefaultSkills = []DefaultSkill{{Name: "code-review", Blocks: []string{"code-review", "review"}}}

func TestDefaultExtensionsShipCodeReviewSkill(t *testing.T) {
	setRequiredEnv(t)
	cfg := loadConfig(t, writeConfig(t, "config.yaml", validGitLab))
	if !reflect.DeepEqual(cfg.DefaultExtensions.Skills, wantDefaultSkills) {
		t.Fatalf("built-in default skills = %+v, want %+v", cfg.DefaultExtensions.Skills, wantDefaultSkills)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestShippedConfigsDeclareDefaultSkill(t *testing.T) {
	setDockerConfigEnv(t)
	setRequiredEnv(t)
	for _, path := range []string{"../../config/server.example.yaml", dockerConfigPath} {
		cfg := loadConfig(t, path)
		if !reflect.DeepEqual(cfg.DefaultExtensions.Skills, wantDefaultSkills) {
			t.Errorf("%s default skills = %+v", path, cfg.DefaultExtensions.Skills)
		}
	}
}

func TestDefaultExtensionsCanBeDisabled(t *testing.T) {
	setRequiredEnv(t)
	cfg := loadConfig(t, writeConfig(t, "config.yaml", validGitLab+"default_extensions:\n  skills: []\n"))
	if len(cfg.DefaultExtensions.Skills) != 0 {
		t.Fatalf("skills = %+v, want disabled", cfg.DefaultExtensions.Skills)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestValidateDefaultExtensions(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"unknown built-in", "default_extensions:\n  skills:\n    - name: nope\n      blocks: [review]\n", "default_extensions.skills[0].name"},
		{"unknown block", "default_extensions:\n  skills:\n    - name: code-review\n      blocks: [plan]\n", "default_extensions.skills[0].blocks[0]"},
		{"duplicate skill", "default_extensions:\n  skills:\n    - name: code-review\n    - name: code-review\n", "default_extensions.skills[1].name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			cfg := loadConfig(t, writeConfig(t, "config.yaml", validGitLab+tt.body))
			requireErrorContains(t, cfg.Validate(), tt.want)
		})
	}
}
