package types

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const testGuidanceDigest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func TestValidateRunConfig_RepositoryGuidance_AcceptsValid(t *testing.T) {
	cases := []struct {
		name string
		cfg  *RepositoryGuidanceConfig
	}{
		{"advisory only", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md", "CLAUDE.md"}}},
		{"pinned lowercase digest", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}, PinnedSHA256: map[string]string{"AGENTS.md": testGuidanceDigest}}},
		{"pinned uppercase digest", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}, PinnedSHA256: map[string]string{"AGENTS.md": strings.ToUpper(testGuidanceDigest)}}},
		{"max bytes at cap", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}, MaxBytes: MaxRepositoryGuidanceMaxBytes}},
		{"max bytes of one", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}, MaxBytes: 1}},
		{"dotfile", &RepositoryGuidanceConfig{Files: []string{".cursorrules"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.RepositoryGuidance = tc.cfg
			require.NoError(t, ValidateRunConfig(c))
		})
	}
}

func TestValidateRunConfig_RepositoryGuidance_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *RepositoryGuidanceConfig
		executor string
		want     string
	}{
		{"empty files", &RepositoryGuidanceConfig{}, "", "must list at least one file"},
		{"empty entry", &RepositoryGuidanceConfig{Files: []string{""}}, "", "must not be empty"},
		{"absolute path", &RepositoryGuidanceConfig{Files: []string{"/etc/passwd"}}, "", "nested and absolute paths are not supported"},
		{"parent traversal", &RepositoryGuidanceConfig{Files: []string{"../AGENTS.md"}}, "", "nested and absolute paths are not supported"},
		{"dotdot", &RepositoryGuidanceConfig{Files: []string{".."}}, "", "must name a file"},
		{"nested path", &RepositoryGuidanceConfig{Files: []string{"docs/AGENTS.md"}}, "", "workspace root"},
		{"backslash path", &RepositoryGuidanceConfig{Files: []string{`docs\AGENTS.md`}}, "", "workspace root"},
		{"glob", &RepositoryGuidanceConfig{Files: []string{"*.md"}}, "", "globbing is not supported"},
		{"leading whitespace", &RepositoryGuidanceConfig{Files: []string{" AGENTS.md"}}, "", "whitespace"},
		{"control character", &RepositoryGuidanceConfig{Files: []string{"AGENTS\x00.md"}}, "", "control characters"},
		{"duplicate file", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md", "AGENTS.md"}}, "", "more than once"},
		{"too many files", &RepositoryGuidanceConfig{Files: manyGuidanceFiles(MaxRepositoryGuidanceFiles + 1)}, "", "repositoryGuidance.files must have <="},
		{"negative max bytes", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}, MaxBytes: -1}, "", "repositoryGuidance.maxBytes"},
		{"max bytes over cap", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}, MaxBytes: MaxRepositoryGuidanceMaxBytes + 1}, "", "repositoryGuidance.maxBytes"},
		{"pin for unlisted file", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}, PinnedSHA256: map[string]string{"CLAUDE.md": testGuidanceDigest}}, "", "must also appear in repositoryGuidance.files"},
		{"short pin", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}, PinnedSHA256: map[string]string{"AGENTS.md": "abc"}}, "", "64-character hex"},
		{"non-hex pin", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}, PinnedSHA256: map[string]string{"AGENTS.md": strings.Repeat("z", 64)}}, "", "64-character hex"},
		{"executor none", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}}, "none", `executor.type "none"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.Executor.Type = tc.executor
			c.RepositoryGuidance = tc.cfg
			require.ErrorContains(t, ValidateRunConfig(c), tc.want)
		})
	}
}

func TestRepositoryGuidanceConfig_EffectiveMaxBytes_DefaultsAndExplicit(t *testing.T) {
	cases := []struct {
		name string
		cfg  *RepositoryGuidanceConfig
		want int
	}{
		{"nil config uses default", nil, DefaultRepositoryGuidanceMaxBytes},
		{"zero max bytes uses default", &RepositoryGuidanceConfig{}, DefaultRepositoryGuidanceMaxBytes},
		{"explicit max bytes is returned", &RepositoryGuidanceConfig{MaxBytes: 100}, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.cfg.EffectiveMaxBytes())
		})
	}
}

func manyGuidanceFiles(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "GUIDE" + strings.Repeat("x", i) + ".md"
	}
	return out
}

// Guidance comes from workspace files the agent can already read, so it
// adds no Rule-of-Two leg in either tier.
func TestRepositoryGuidance_DoesNotChangeRuleOfTwoLegs(t *testing.T) {
	base := validConfig()
	base.Tools.BuiltIn = []string{"read_file", "run_command"}
	wantU, wantS, wantE := RuleOfTwoState(base)

	cases := []struct {
		name string
		cfg  *RepositoryGuidanceConfig
	}{
		{"advisory", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}}},
		{"pinned", &RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}, PinnedSHA256: map[string]string{"AGENTS.md": testGuidanceDigest}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.Tools.BuiltIn = []string{"read_file", "run_command"}
			c.RepositoryGuidance = tc.cfg
			u, s, e := RuleOfTwoState(c)
			require.Equal(t, wantU, u, "holdsUntrusted")
			require.Equal(t, wantS, s, "holdsSensitive")
			require.Equal(t, wantE, e, "canCommExternal")
		})
	}
}
