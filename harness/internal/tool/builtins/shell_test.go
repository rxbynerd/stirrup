package builtins

import "testing"

// TestFormatRunCommand_ExitCodeOnlyNoOutput locks the exact text for a command
// that produced no stdout or stderr but exited non-zero. The "[exit code: N]"
// line is written with a leading newline unconditionally, so an otherwise empty
// rendering still begins with "\n". This byte-for-byte shape is part of the
// pre-#231 text contract every provider must accept; a regression here would
// silently change the model-visible output for silent-but-failing commands.
func TestFormatRunCommand_ExitCodeOnlyNoOutput(t *testing.T) {
	got := formatRunCommand("", "", 2)
	const want = "\n[exit code: 2]"
	if got != want {
		t.Fatalf("formatRunCommand(\"\", \"\", 2) = %q, want %q", got, want)
	}
}
