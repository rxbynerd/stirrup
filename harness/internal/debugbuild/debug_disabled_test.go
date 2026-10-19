//go:build !stirrupdebug

package debugbuild

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDebugBuild_DisabledByDefault guards the load-bearing security
// property: a binary built without -tags stirrupdebug (every release
// artifact) must report DebugBuildEnabled() == false. This test is
// itself tag-gated !stirrupdebug so it only runs against the release
// build configuration — the companion debug_enabled_test.go asserts the
// opposite under -tags stirrupdebug.
func TestDebugBuild_DisabledByDefault(t *testing.T) {
	require.False(t, DebugBuildEnabled(),
		"DebugBuildEnabled() must be false in a build without -tags stirrupdebug")
}

// TestVersionSuffix_EmptyByDefault guards --version output: a release
// build's suffix must be empty so `stirrup --version` prints the plain
// version.
func TestVersionSuffix_EmptyByDefault(t *testing.T) {
	require.Empty(t, VersionSuffix(),
		"VersionSuffix() must be empty in a build without -tags stirrupdebug")
}
