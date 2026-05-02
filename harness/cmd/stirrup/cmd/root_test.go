package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// TestRootCmd_Version pins the format of the harness `--version` output.
// The wiring in root.go (`rootCmd.Version = version.Full()`) is exercised
// here so a refactor that drops or rewrites the link-time version plumbing
// fails this test rather than silently shipping a binary whose --version
// flag prints "stirrup " or panics.
//
// The eval CLI has the equivalent guard in TestRun_Version
// (eval/cmd/eval/main_test.go); this test is its harness counterpart.
func TestRootCmd_Version(t *testing.T) {
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetArgs([]string{"--version"})
	defer func() {
		// Reset shared-state mutations on the package-level rootCmd so
		// later tests in this package see a clean command. SetArgs(nil)
		// restores os.Args-based parsing; SetOut(nil) restores the cobra
		// default writer.
		rootCmd.SetOut(nil)
		rootCmd.SetArgs(nil)
	}()

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("rootCmd.Execute() returned error: %v", err)
	}

	out := buf.String()
	if !strings.HasPrefix(out, "stirrup version ") {
		t.Fatalf("unexpected --version output: %q (want prefix %q)", out, "stirrup version ")
	}
	// Default link-time version when no -ldflags injected is "dev".
	if !strings.Contains(out, "dev") {
		t.Fatalf("--version output %q should include the default link-time version %q", out, "dev")
	}
}
