//go:build !stirrupdebug

package debugbuild

import "testing"

// TestDebugBuildDisabledByDefault guards the load-bearing security
// property: a binary built without -tags stirrupdebug (every release
// artifact) must report DebugBuildEnabled() == false. This test is
// itself tag-gated !stirrupdebug so it only runs against the release
// build configuration — the companion debug_enabled_test.go asserts the
// opposite under -tags stirrupdebug.
func TestDebugBuildDisabledByDefault(t *testing.T) {
	if DebugBuildEnabled() {
		t.Fatal("DebugBuildEnabled() == true in a build without -tags stirrupdebug")
	}
}

// TestVersionSuffixEmptyByDefault guards --version output: a release
// build's suffix must be empty so `stirrup --version` is byte-identical
// to its pre-debug-build form.
func TestVersionSuffixEmptyByDefault(t *testing.T) {
	if got := VersionSuffix(); got != "" {
		t.Fatalf("VersionSuffix() = %q, want \"\" in a build without -tags stirrupdebug", got)
	}
}
