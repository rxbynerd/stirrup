//go:build stirrupdebug

// Package debugbuild is the single compile-time gate for the harness's
// debug-only behaviour (issues #219, #220): disabling trace/recording
// redaction via --debug and dumping raw provider wire traffic via
// --trace-wire. A release binary is built WITHOUT the stirrupdebug tag
// (see justfile / the release workflow), so DebugBuildEnabled always
// returns false there and the CLI hard-errors before either flag can
// have any effect — see docs/security.md#debug-builds.
//
// This file pair (debug_enabled.go / debug_disabled.go) is the same
// build-tag-gated split used elsewhere in the harness (e.g.
// harness_unix_test.go's "unix" tag): exactly one file compiles into any
// given binary, so DebugBuildEnabled's return value is a property of the
// build itself, not a runtime flag that could be tampered with.
package debugbuild

// DebugBuildEnabled reports whether this binary was compiled with
// -tags stirrupdebug. Every debug-only behaviour in the harness must
// gate its point of effect on this function (not merely on a CLI flag)
// so a release binary is physically incapable of disabling redaction or
// dumping unredacted wire traffic, even via a future non-flag code path.
func DebugBuildEnabled() bool { return true }

// VersionSuffix returns the marker appended to --version output so a
// debug binary is never mistaken for a release build. "+debug" follows
// semver build-metadata syntax.
func VersionSuffix() string { return "+debug" }
