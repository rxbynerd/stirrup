package types

import (
	"fmt"
	"path"
	"strings"
)

// MaxProtectedWriteTargets bounds ToolsConfig.ProtectedWriteTargets.
const MaxProtectedWriteTargets = 64

func validateProtectedWriteTargets(targets []string, errs *[]string) {
	if len(targets) > MaxProtectedWriteTargets {
		*errs = append(*errs, fmt.Sprintf("tools.protectedWriteTargets must have <= %d entries, got %d", MaxProtectedWriteTargets, len(targets)))
	}
	for i, t := range targets {
		if reason := protectedWriteTargetProblem(t); reason != "" {
			*errs = append(*errs, fmt.Sprintf("tools.protectedWriteTargets[%d] %q %s", i, t, reason))
		}
	}
}

// protectedWriteTargetProblem returns why p is not a valid protected write
// target, or "" when it is. Entries are matched literally against the
// workspace-relative path a write tool names.
func protectedWriteTargetProblem(p string) string {
	slashed := strings.ReplaceAll(p, `\`, "/")
	switch {
	case p == "":
		return "must not be empty"
	case strings.TrimSpace(p) != p:
		return "must not have leading or trailing whitespace"
	case strings.HasPrefix(slashed, "/") || hasDriveLetter(p):
		return "must be relative to the workspace root"
	case pathHasDotDotSegment(p):
		return `must not contain ".." segments`
	case path.Clean(slashed) == ".":
		return "must name a path inside the workspace, not the workspace root"
	case strings.ContainsAny(p, "*?["):
		return "must be a literal path; globbing is not supported"
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "must not contain control characters"
		}
	}
	return ""
}

func hasDriveLetter(p string) bool {
	return len(p) >= 2 && p[1] == ':' && (p[0] >= 'A' && p[0] <= 'Z' || p[0] >= 'a' && p[0] <= 'z')
}
