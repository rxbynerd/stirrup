//go:build stirrupdebug

package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateDebugBuildFlags_AcceptsDebugOnDebugBuild is the companion
// to harness_debug_disabled_test.go's
// TestValidateDebugBuildFlags_RejectsDebugOnReleaseBuild: built only
// under -tags stirrupdebug, it asserts --debug and --trace-wire are
// accepted (no hard error) when the binary actually is a debug build.
func TestValidateDebugBuildFlags_AcceptsDebugOnDebugBuild(t *testing.T) {
	for _, flagName := range debugBuildOnlyFlags {
		t.Run(flagName, func(t *testing.T) {
			cmd := newTestHarnessCommand()
			require.NoError(t, cmd.Flags().Set(flagName, "true"))
			require.NoError(t, validateDebugBuildFlags(cmd, nil))
		})
	}
}
