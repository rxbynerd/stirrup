// Package version exposes the build's version label and git commit, both of
// which are injected at link time via -ldflags. Centralising these in the
// zero-dependency types module lets every binary in the workspace
// (stirrup, stirrup-eval, future tools) share one source of truth for the
// "what version am I running?" question.
package version

// Set at build time via:
//
//	-ldflags "-X github.com/rxbynerd/stirrup/types/version.version=<tag> \
//	          -X github.com/rxbynerd/stirrup/types/version.commit=<short-sha>"
//
// The defaults below are deliberately printable so a `dev` build still
// produces a sensible --version line without any ldflag plumbing.
var (
	version = "dev"
	commit  = ""
)

// Version returns the human-readable build label: a semver tag like "v1.2.3"
// for tagged releases, "main" for main-branch builds, or "dev" for local
// builds where no ldflag was supplied.
func Version() string { return version }

// Commit returns the build's git short SHA when injected at link time, or
// the empty string for local builds.
func Commit() string { return commit }

// Full returns "<version> (<short-sha>)" when both fields are populated,
// falling back to just <version> when no commit is injected. This is the
// preferred string for human-facing --version output.
func Full() string {
	if commit == "" {
		return version
	}
	return version + " (" + commit + ")"
}
