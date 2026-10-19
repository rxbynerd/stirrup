//go:build !stirrupdebug

package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateDebugBuildFlags_RejectsDebugOnReleaseBuild and its sibling
// below guard the CLI-layer half of the load-bearing security property: a
// release binary, one built without -tags stirrupdebug (every artifact that
// `just build` and the release workflow produce), must hard-error on --debug
// and --trace-wire rather than silently ignoring them. This file is tag-gated
// !stirrupdebug, so it runs only against that build configuration; the
// companion harness_debug_enabled_test.go asserts the opposite under
// -tags stirrupdebug.
func TestValidateDebugBuildFlags_RejectsDebugOnReleaseBuild(t *testing.T) {
	for _, flagName := range debugBuildOnlyFlags {
		t.Run(flagName, func(t *testing.T) {
			cmd := newTestHarnessCommand()
			require.NoError(t, cmd.Flags().Set(flagName, "true"))

			err := validateDebugBuildFlags(cmd, nil)
			require.Error(t, err)
			require.Equal(t, exitUsage, classifyExitCode(err))
			require.ErrorContains(t, err, "--"+flagName)
			require.ErrorContains(t, err, "stirrupdebug", "error should name the fix (-tags stirrupdebug)")
		})
	}
}

// TestValidateDebugBuildFlags_NoOpWhenFlagsUnset asserts a release build
// that never touches --debug/--trace-wire proceeds normally — the gate
// only fires when a flag was explicitly Changed(), not merely because it
// exists in the closed set.
func TestValidateDebugBuildFlags_NoOpWhenFlagsUnset(t *testing.T) {
	cmd := newTestHarnessCommand()

	err := validateDebugBuildFlags(cmd, nil)
	require.NoError(t, err, "--debug and --trace-wire are unset")
}
