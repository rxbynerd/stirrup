package security

import "strings"

// ProtectedWriteTargets is a run's set of workspace paths that write tools
// may not target. The zero value protects nothing.
type ProtectedWriteTargets struct {
	targets []protectedWriteTarget
	resolve func(string) (string, error)
	root    string
}

type protectedWriteTarget struct {
	path     string
	resolved string
	dir      bool
}

// NewProtectedWriteTargets builds the set from workspace-relative entries.
// An entry ending in "/" protects that directory and every path beneath
// it; any other entry protects exactly that path. resolve, when non-nil,
// maps a path to the executor's canonical form so that absolute and
// symlinked spellings of a protected path also match.
func NewProtectedWriteTargets(entries []string, resolve func(string) (string, error)) ProtectedWriteTargets {
	p := ProtectedWriteTargets{resolve: resolve}
	if resolve != nil {
		if r, err := resolve("."); err == nil {
			p.root = normalizeGuardPath(r)
		}
	}
	for _, entry := range entries {
		slashed := strings.ReplaceAll(strings.TrimSpace(entry), `\`, "/")
		normalized := normalizeGuardPath(slashed)
		if normalized == "" {
			continue
		}
		target := protectedWriteTarget{
			path: normalized,
			dir:  strings.HasSuffix(slashed, "/"),
		}
		if resolve != nil {
			if r, err := resolve(normalized); err == nil {
				target.resolved = normalizeGuardPath(r)
			}
		}
		p.targets = append(p.targets, target)
	}
	return p
}

// Matches reports whether value names a protected path, ignoring case.
// Matching applies the guard's path normalisation ("./" prefix,
// backslashes, quotes, "..") and, when the set has a resolver, also
// compares canonical forms.
func (p ProtectedWriteTargets) Matches(value string) bool {
	if len(p.targets) == 0 {
		return false
	}
	normalized := normalizeGuardPath(value)
	if normalized == "" {
		return false
	}
	for _, t := range p.targets {
		if t.covers(normalized, t.path) {
			return true
		}
	}
	if !p.worthResolving(normalized) {
		return false
	}
	r, err := p.resolve(normalized)
	if err != nil {
		return false
	}
	resolved := normalizeGuardPath(r)
	for _, t := range p.targets {
		if t.resolved != "" && t.covers(resolved, t.resolved) {
			return true
		}
	}
	return false
}

// worthResolving skips candidates that lie outside the workspace on their
// face: resolving them would only repeat the executor's traversal
// rejection, and its security event, before dispatch.
func (p ProtectedWriteTargets) worthResolving(candidate string) bool {
	switch {
	case p.resolve == nil:
		return false
	case candidate == ".." || strings.HasPrefix(candidate, "../"):
		return false
	case strings.HasPrefix(candidate, "/"):
		return p.root != "" && (candidate == p.root || strings.HasPrefix(candidate, p.root+"/"))
	default:
		return true
	}
}

// covers compares case-insensitively because macOS and Windows workspaces
// usually sit on case-insensitive filesystems, where "agents.md" names the
// same file as "AGENTS.md".
func (t protectedWriteTarget) covers(candidate, target string) bool {
	return strings.EqualFold(candidate, target) ||
		(t.dir && strings.HasPrefix(strings.ToLower(candidate), strings.ToLower(target)+"/"))
}
