//go:build !stirrupdebug

package debugbuild

// DebugBuildEnabled reports whether this binary was compiled with
// -tags stirrupdebug. This is the release-build implementation, always
// false: release artifacts (built via `just build` / the release
// workflow, neither of which pass -tags) never satisfy this, so
// --debug and --trace-wire hard-error at startup instead of silently
// no-op'ing. See harness/cmd/stirrup/cmd/harness.go's PreRunE gate and
// docs/security.md#debug-builds.
func DebugBuildEnabled() bool { return false }

// VersionSuffix returns "" on a release build so `stirrup --version`
// output is unchanged from its pre-debug-build form.
func VersionSuffix() string { return "" }
