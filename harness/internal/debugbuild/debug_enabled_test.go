//go:build stirrupdebug

package debugbuild

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDebugBuild_EnabledUnderTag is the companion to
// debug_disabled_test.go's TestDebugBuild_DisabledByDefault: built only
// under -tags stirrupdebug, it asserts DebugBuildEnabled() == true so a
// future change that accidentally inverts the two files' return values
// is caught under either build configuration.
func TestDebugBuild_EnabledUnderTag(t *testing.T) {
	must := require.New(t)
	must.True(DebugBuildEnabled(), "DebugBuildEnabled() in a build with -tags stirrupdebug")
}

// TestVersionSuffix_MarksDebugBuild guards the --version marker: a debug
// build must never present as a plain release version string.
func TestVersionSuffix_MarksDebugBuild(t *testing.T) {
	must := require.New(t)
	must.Equal("+debug", VersionSuffix(), "VersionSuffix() in a build with -tags stirrupdebug")
}
