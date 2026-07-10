package builtins

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

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

// TestRunCommandTool_TimeoutClampedTo300s pins the run_command tool's
// independent 300s (5 min) timeout ceiling. Issue #461 raised the shared
// executor.maxTimeout cap to 30 minutes so lifecycle hooks can run a cold
// `bundle install`, but the model-reachable run_command tool must keep
// its own tighter 300s clamp — the two are deliberately decoupled (see
// the maxTimeout doc comment in executor/local.go) so raising the
// executor cap does not silently hand the agent a longer exec budget.
func TestRunCommandTool_TimeoutClampedTo300s(t *testing.T) {
	var gotTimeout time.Duration
	exec := &mockExecutor{
		execFunc: func(_ context.Context, _ string, timeout time.Duration) (*executor.ExecResult, error) {
			gotTimeout = timeout
			return &executor.ExecResult{ExitCode: 0}, nil
		},
	}

	tl := RunCommandTool(exec)
	input := json.RawMessage(`{"command":"true","timeout":9000}`)
	if _, err := tl.StructuredHandler(context.Background(), input); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want := 300 * time.Second; gotTimeout != want {
		t.Errorf("Exec timeout = %v, want %v (tool-layer clamp must stay independent of the raised executor cap)", gotTimeout, want)
	}
}
