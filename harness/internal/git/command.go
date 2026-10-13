package git

import (
	"regexp"
	"strings"
)

// CommandLine renders a git invocation for an Executor's `sh -c` carrying
// HardeningFlags, IdentityFlags and args. Every element is single-quoted so
// sh treats it as one literal word and never as syntax.
func CommandLine(args ...string) string {
	identity := IdentityFlags()
	parts := make([]string, 0, 1+len(HardeningFlags)+len(identity)+len(args))
	parts = append(parts, "git")
	for _, f := range HardeningFlags {
		parts = append(parts, shellQuote(f))
	}
	for _, f := range identity {
		parts = append(parts, shellQuote(f))
	}
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// ValidSHA reports whether s is a full lowercase SHA-1 or SHA-256 object
// name.
func ValidSHA(s string) bool {
	return shaPattern.MatchString(s)
}

// maxBranchNameLen bounds names accepted from the sandbox; git itself has
// no fixed limit, but a filesystem path component does.
const maxBranchNameLen = 255

// ValidBranchName reports whether name satisfies git's ref-name rules
// (git-check-ref-format) for a branch given without the refs/heads/
// prefix.
func ValidBranchName(name string) bool {
	if name == "" || len(name) > maxBranchNameLen || name == "@" {
		return false
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c == 0x7f {
			return false
		}
		switch c {
		case ' ', '~', '^', ':', '?', '*', '[', '\\':
			return false
		}
	}
	for _, component := range strings.Split(name, "/") {
		if strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}
