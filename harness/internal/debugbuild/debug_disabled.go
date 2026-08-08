//go:build !stirrupdebug

package debugbuild

// DebugBuildEnabled is the release-build implementation, always false:
// neither `just build` nor the release workflow passes -tags, so --debug
// and --trace-wire hard-error at startup rather than silently no-op'ing.
func DebugBuildEnabled() bool { return false }

// VersionSuffix is empty on a release build, leaving --version unchanged.
func VersionSuffix() string { return "" }
