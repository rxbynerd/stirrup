package core

import "github.com/rxbynerd/stirrup/harness/internal/security"

// buildWriteProtection combines the operator's protected write targets with
// the guidance files this run loaded. A missing advisory file is not
// protected: nothing was rendered from it.
func (l *AgenticLoop) buildWriteProtection() security.ProtectedWriteTargets {
	entries := append([]string(nil), l.ProtectedWriteTargets...)
	for _, d := range l.repositoryGuidance {
		if !d.Missing {
			entries = append(entries, d.Path)
		}
	}
	var resolve func(string) (string, error)
	if q, ok := l.Executor.(quietResolver); ok {
		resolve = q.ResolvePathQuietly
	} else if l.Executor != nil {
		resolve = l.Executor.ResolvePath
	}
	return security.NewProtectedWriteTargets(entries, resolve)
}

// quietResolver is an executor whose ResolvePath reports traversal attempts
// as security events. The guard only probes a candidate the tool dispatch
// resolves again, so it uses the quiet form and each attempt is reported
// once.
type quietResolver interface {
	ResolvePathQuietly(relativePath string) (string, error)
}
