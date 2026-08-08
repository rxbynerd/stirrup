//go:build stirrupdebug

package debugbuild

import "testing"

// TestDebugBuildEnabledUnderTag is the companion to
// debug_disabled_test.go's TestDebugBuildDisabledByDefault: built only
// under -tags stirrupdebug, it asserts DebugBuildEnabled() == true so a
// future change that accidentally inverts the two files' return values
// is caught under either build configuration.
func TestDebugBuildEnabledUnderTag(t *testing.T) {
	if !DebugBuildEnabled() {
		t.Fatal("DebugBuildEnabled() == false in a build with -tags stirrupdebug")
	}
}

// TestVersionSuffixMarksDebugBuild guards the --version marker: a debug
// build must never present as a plain release version string.
func TestVersionSuffixMarksDebugBuild(t *testing.T) {
	if got := VersionSuffix(); got != "+debug" {
		t.Fatalf("VersionSuffix() = %q, want \"+debug\" in a build with -tags stirrupdebug", got)
	}
}
