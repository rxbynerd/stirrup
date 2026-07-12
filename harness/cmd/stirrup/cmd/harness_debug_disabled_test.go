//go:build !stirrupdebug

package cmd

import (
	"strings"
	"testing"
)

// TestValidateDebugBuildFlags_RejectsDebugOnReleaseBuild and its sibling
// below guard the CLI-layer half of the load-bearing security property
// (issues #219, #220): a release binary — one built without -tags
// stirrupdebug, which is every artifact `just build` / the release
// workflow produce — must hard-error on --debug and --trace-wire rather
// than silently ignoring them. This test file is itself tag-gated
// !stirrupdebug so it only runs against that build configuration; the
// companion harness_debug_enabled_test.go asserts the opposite under
// -tags stirrupdebug.
func TestValidateDebugBuildFlags_RejectsDebugOnReleaseBuild(t *testing.T) {
	for _, flagName := range debugBuildOnlyFlags {
		t.Run(flagName, func(t *testing.T) {
			cmd := newTestHarnessCommand()
			if err := cmd.Flags().Set(flagName, "true"); err != nil {
				t.Fatalf("set %s: %v", flagName, err)
			}
			err := validateDebugBuildFlags(cmd, nil)
			if err == nil {
				t.Fatalf("expected an error for --%s on a release build, got nil", flagName)
			}
			if got := classifyExitCode(err); got != exitUsage {
				t.Errorf("classifyExitCode = %d, want %d (usage)", got, exitUsage)
			}
			if !strings.Contains(err.Error(), "--"+flagName) {
				t.Errorf("error should name the offending flag %q, got: %v", flagName, err)
			}
			if !strings.Contains(err.Error(), "stirrupdebug") {
				t.Errorf("error should name the fix (-tags stirrupdebug), got: %v", err)
			}
		})
	}
}

// TestValidateDebugBuildFlags_NoOpWhenFlagsUnset asserts a release build
// that never touches --debug/--trace-wire proceeds normally — the gate
// only fires when a flag was explicitly Changed(), not merely because it
// exists in the closed set.
func TestValidateDebugBuildFlags_NoOpWhenFlagsUnset(t *testing.T) {
	cmd := newTestHarnessCommand()
	if err := validateDebugBuildFlags(cmd, nil); err != nil {
		t.Errorf("expected no error when --debug/--trace-wire are unset, got: %v", err)
	}
}
