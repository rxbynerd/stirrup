package types

import (
	"strings"
	"testing"
)

const testGuidanceDigest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func TestValidateRunConfig_RepositoryGuidanceAcceptsValid(t *testing.T) {
	cases := map[string]*RepositoryGuidanceConfig{
		"advisory_only": {Files: []string{"AGENTS.md", "CLAUDE.md"}},
		"pinned_lower":  {Files: []string{"AGENTS.md"}, PinnedSHA256: map[string]string{"AGENTS.md": testGuidanceDigest}},
		"pinned_upper":  {Files: []string{"AGENTS.md"}, PinnedSHA256: map[string]string{"AGENTS.md": strings.ToUpper(testGuidanceDigest)}},
		"max_bytes_cap": {Files: []string{"AGENTS.md"}, MaxBytes: MaxRepositoryGuidanceMaxBytes},
		"max_bytes_one": {Files: []string{"AGENTS.md"}, MaxBytes: 1},
		"dotfile":       {Files: []string{".cursorrules"}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			c.RepositoryGuidance = cfg
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("expected valid config, got: %v", err)
			}
		})
	}
}

func TestValidateRunConfig_RepositoryGuidanceRejects(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *RepositoryGuidanceConfig
		executor string
		want     string
	}{
		{"empty_files", &RepositoryGuidanceConfig{}, "", "must list at least one file"},
		{"empty_entry", &RepositoryGuidanceConfig{Files: []string{""}}, "", "must not be empty"},
		{"absolute", &RepositoryGuidanceConfig{Files: []string{"/etc/passwd"}}, "", "nested and absolute paths are not supported"},
		{"traversal", &RepositoryGuidanceConfig{Files: []string{"../AGENTS.md"}}, "", "nested and absolute paths are not supported"},
		{"dotdot", &RepositoryGuidanceConfig{Files: []string{".."}}, "", "must name a file"},
		{"nested", &RepositoryGuidanceConfig{Files: []string{"docs/AGENTS.md"}}, "", "workspace root"},
		{"backslash", &RepositoryGuidanceConfig{Files: []string{`docs\AGENTS.md`}}, "", "workspace root"},
		{"glob", &RepositoryGuidanceConfig{Files: []string{"*.md"}}, "", "globbing is not supported"},
		{"whitespace", &RepositoryGuidanceConfig{Files: []string{" AGENTS.md"}}, "", "whitespace"},
		{"control", &RepositoryGuidanceConfig{Files: []string{"AGENTS\x00.md"}}, "", "control characters"},
		{"duplicate", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md", "AGENTS.md"}}, "", "more than once"},
		{"too_many", &RepositoryGuidanceConfig{Files: manyGuidanceFiles(MaxRepositoryGuidanceFiles + 1)}, "", "repositoryGuidance.files must have <="},
		{"negative_max", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}, MaxBytes: -1}, "", "repositoryGuidance.maxBytes"},
		{"over_max", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}, MaxBytes: MaxRepositoryGuidanceMaxBytes + 1}, "", "repositoryGuidance.maxBytes"},
		{"pin_not_listed", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}, PinnedSHA256: map[string]string{"CLAUDE.md": testGuidanceDigest}}, "", "must also appear in repositoryGuidance.files"},
		{"pin_short", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}, PinnedSHA256: map[string]string{"AGENTS.md": "abc"}}, "", "64-character hex"},
		{"pin_not_hex", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}, PinnedSHA256: map[string]string{"AGENTS.md": strings.Repeat("z", 64)}}, "", "64-character hex"},
		{"none_executor", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}}, "none", `executor.type "none"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.Executor.Type = tc.executor
			c.RepositoryGuidance = tc.cfg
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("expected a validation error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestRepositoryGuidanceConfig_EffectiveMaxBytes(t *testing.T) {
	var nilCfg *RepositoryGuidanceConfig
	if got := nilCfg.EffectiveMaxBytes(); got != DefaultRepositoryGuidanceMaxBytes {
		t.Errorf("nil config: got %d, want %d", got, DefaultRepositoryGuidanceMaxBytes)
	}
	if got := (&RepositoryGuidanceConfig{}).EffectiveMaxBytes(); got != DefaultRepositoryGuidanceMaxBytes {
		t.Errorf("zero MaxBytes: got %d, want %d", got, DefaultRepositoryGuidanceMaxBytes)
	}
	if got := (&RepositoryGuidanceConfig{MaxBytes: 100}).EffectiveMaxBytes(); got != 100 {
		t.Errorf("explicit MaxBytes: got %d, want 100", got)
	}
}

func manyGuidanceFiles(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "GUIDE" + strings.Repeat("x", i) + ".md"
	}
	return out
}
