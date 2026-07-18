// Package version exposes the build's version label and git commit,
// injected at link time via -ldflags, so every binary in the workspace
// shares one source of truth.
package version

// Set at build time via:
//
//	-ldflags "-X github.com/rxbynerd/stirrup/types/version.version=<tag> \
//	          -X github.com/rxbynerd/stirrup/types/version.commit=<short-sha>"
var (
	version = "dev"
	commit  = ""
)

// Version returns the build label: a semver tag, "main", or "dev".
func Version() string { return version }

// Commit returns the build's git short SHA, or empty for local builds.
func Commit() string { return commit }

// Full returns "<version> (<short-sha>)", or just <version> when commit
// is empty.
func Full() string {
	if commit == "" {
		return version
	}
	return version + " (" + commit + ")"
}
