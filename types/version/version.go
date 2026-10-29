// Package version exposes the build's version label and git commit,
// injected at link time via -ldflags, so every binary in the workspace
// shares one source of truth.
package version

import (
	"regexp"
	"strings"
)

// Set at build time via:
//
//	-ldflags "-X github.com/rxbynerd/stirrup/types/version.version=<tag> \
//	          -X github.com/rxbynerd/stirrup/types/version.commit=<short-sha>"
var (
	version = "dev"
	commit  = ""
)

// semverPattern is the Semantic Versioning 2.0.0 grammar from semver.org.
var semverPattern = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
	`(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
	`(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

// Version returns the build label: a semver tag, "main", or "dev".
func Version() string { return version }

// Semver returns the build label as SemverOf reads it.
func Semver() string { return SemverOf(version) }

// SemverOf returns a build label as a Semantic Versioning 2.0.0 string
// without the tag's leading "v", or empty when the label is not a
// version, as for "main" and "dev" builds.
func SemverOf(label string) string {
	v := strings.TrimPrefix(label, "v")
	if !semverPattern.MatchString(v) {
		return ""
	}
	return v
}

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
