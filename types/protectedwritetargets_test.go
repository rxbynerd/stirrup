package types

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateRunConfig_ProtectedWriteTargets_AcceptsValid(t *testing.T) {
	cases := []struct {
		name    string
		targets []string
	}{
		{"unset", nil},
		{"file", []string{"AGENTS.md"}},
		{"nested file", []string{"docs/AGENTS.md"}},
		{"directory", []string{".agents/", "config/policies/"}},
		{"dot prefix", []string{"./CLAUDE.md"}},
		{"backslash", []string{`config\policy.cedar`}},
		{"dotdot in name", []string{"notes..md"}},
		{"at cap", manyProtectedTargets(MaxProtectedWriteTargets)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.Tools.ProtectedWriteTargets = tc.targets
			require.NoError(t, ValidateRunConfig(c))
		})
	}
}

func TestValidateRunConfig_ProtectedWriteTargets_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name    string
		targets []string
		wantErr string
	}{
		{"empty", []string{""}, "must not be empty"},
		{"whitespace", []string{"AGENTS.md "}, "whitespace"},
		{"absolute", []string{"/etc/passwd"}, "must be relative"},
		{"absolute backslash", []string{`\workspace\AGENTS.md`}, "must be relative"},
		{"drive letter", []string{`C:\repo\AGENTS.md`}, "must be relative"},
		{"traversal", []string{"../AGENTS.md"}, `".." segments`},
		{"inner traversal", []string{"docs/../../AGENTS.md"}, `".." segments`},
		{"backslash traversal", []string{`docs\..\AGENTS.md`}, `".." segments`},
		{"root dot", []string{"."}, "not the workspace root"},
		{"root dot slash", []string{"./"}, "not the workspace root"},
		{"glob", []string{"docs/*.md"}, "globbing is not supported"},
		{"control character", []string{"AGENTS\x00.md"}, "control characters"},
		{"too many", manyProtectedTargets(MaxProtectedWriteTargets + 1), "tools.protectedWriteTargets must have <="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.Tools.ProtectedWriteTargets = tc.targets
			require.ErrorContains(t, ValidateRunConfig(c), tc.wantErr)
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
