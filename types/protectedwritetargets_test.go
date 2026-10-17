package types

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateRunConfig_ProtectedWriteTargetsAcceptsValid(t *testing.T) {
	cases := map[string][]string{
		"unset":       nil,
		"file":        {"AGENTS.md"},
		"nested_file": {"docs/AGENTS.md"},
		"directory":   {".agents/", "config/policies/"},
		"dot_prefix":  {"./CLAUDE.md"},
		"backslash":   {`config\policy.cedar`},
		"dotdot_name": {"notes..md"},
		"at_cap":      manyProtectedTargets(MaxProtectedWriteTargets),
	}
	for name, targets := range cases {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			c.Tools.ProtectedWriteTargets = targets
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("expected valid config, got: %v", err)
			}
		})
	}
}

func TestValidateRunConfig_ProtectedWriteTargetsRejects(t *testing.T) {
	cases := []struct {
		name    string
		targets []string
		want    string
	}{
		{"empty", []string{""}, "must not be empty"},
		{"whitespace", []string{"AGENTS.md "}, "whitespace"},
		{"absolute", []string{"/etc/passwd"}, "must be relative"},
		{"absolute_backslash", []string{`\workspace\AGENTS.md`}, "must be relative"},
		{"drive_letter", []string{`C:\repo\AGENTS.md`}, "must be relative"},
		{"traversal", []string{"../AGENTS.md"}, `".." segments`},
		{"inner_traversal", []string{"docs/../../AGENTS.md"}, `".." segments`},
		{"backslash_traversal", []string{`docs\..\AGENTS.md`}, `".." segments`},
		{"root_dot", []string{"."}, "not the workspace root"},
		{"root_dot_slash", []string{"./"}, "not the workspace root"},
		{"glob", []string{"docs/*.md"}, "globbing is not supported"},
		{"control", []string{"AGENTS\x00.md"}, "control characters"},
		{"too_many", manyProtectedTargets(MaxProtectedWriteTargets + 1), "tools.protectedWriteTargets must have <="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.Tools.ProtectedWriteTargets = tc.targets
			err := ValidateRunConfig(c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got: %v", tc.want, err)
			}
		})
	}
}

func manyProtectedTargets(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("protected-%d.md", i)
	}
	return out
}
