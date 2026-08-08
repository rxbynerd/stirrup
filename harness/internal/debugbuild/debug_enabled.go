//go:build stirrupdebug

// Package debugbuild is the single compile-time gate for --debug and
// --trace-wire. Exactly one of this file pair compiles into any binary, so
// DebugBuildEnabled is a property of the build rather than a tamperable
// runtime flag. See docs/security.md#debug-builds.
package debugbuild

// DebugBuildEnabled reports whether this binary was compiled with -tags
// stirrupdebug. Every debug-only behaviour must gate its point of effect on
// this, not merely on a CLI flag.
func DebugBuildEnabled() bool { return true }

// VersionSuffix marks --version output so a debug binary is never mistaken
// for a release build, following semver build-metadata syntax.
func VersionSuffix() string { return "+debug" }
