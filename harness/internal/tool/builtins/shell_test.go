package builtins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// TestRunCommandTool_TimeoutReturnsPartialOutputAsSoftOutcome pins #489's
// contract at the tool layer: an executor.Exec error that wraps
// executor.ErrTimeout must not surface as a hard tool error, since every
// executor implementation still returns whatever partial stdout/stderr it
// captured before the kill. The handler must classify the error via
// errors.Is (mirroring harness/internal/hook/runner.go's isTimeoutErr),
// report TimedOut in the structured payload, preserve the partial output,
// and make the timeout unambiguous in the Text fallback so a model reading
// only Text (not Structured) can still tell a timeout apart from a clean
// exit.
func TestRunCommandTool_TimeoutReturnsPartialOutputAsSoftOutcome(t *testing.T) {
	const timeout = 5 * time.Second
	execErr := fmt.Errorf("%w after %s: %w", executor.ErrTimeout, timeout, context.DeadlineExceeded)

	exec := &mockExecutor{
		execFunc: func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{
				Stdout: "partial stdout before kill\n",
				Stderr: "partial stderr before kill\n",
			}, execErr
		},
	}

	tl := RunCommandTool(exec)
	input := json.RawMessage(`{"command":"sleep 999","timeout":5}`)
	got, err := tl.StructuredHandler(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected hard error for a timeout: %v", err)
	}

	if got.Kind != kindCommandResult {
		t.Errorf("Kind = %q, want %q", got.Kind, kindCommandResult)
	}

	var structured commandResult
	if err := json.Unmarshal(got.Structured, &structured); err != nil {
		t.Fatalf("unmarshal structured result: %v", err)
	}
	if !structured.TimedOut {
		t.Errorf("structured.TimedOut = false, want true")
	}
	if structured.TimeoutSeconds != 5 {
		t.Errorf("structured.TimeoutSeconds = %d, want 5", structured.TimeoutSeconds)
	}
	if structured.Stdout != "partial stdout before kill\n" {
		t.Errorf("structured.Stdout = %q, want partial stdout preserved", structured.Stdout)
	}
	if structured.Stderr != "partial stderr before kill\n" {
		t.Errorf("structured.Stderr = %q, want partial stderr preserved", structured.Stderr)
	}

	if !strings.Contains(got.Text, "partial stdout before kill") {
		t.Errorf("Text = %q, want partial stdout preserved", got.Text)
	}
	if !strings.Contains(got.Text, "[timed out after 5s]") {
		t.Errorf("Text = %q, want an explicit timed-out marker", got.Text)
	}
}

// TestRunCommandTool_NonTimeoutErrorStillPropagates is the counterpart to
// TestRunCommandTool_TimeoutReturnsPartialOutputAsSoftOutcome: an Exec error
// that does NOT wrap executor.ErrTimeout (e.g. a plain context cancellation,
// or any other exec failure) must still propagate as a hard tool error
// exactly as before. This guards against a classification that is too
// broad — errors.Is must exclude cancellation so a SIGTERM-driven shutdown
// isn't mistaken for a timed-out command's soft outcome.
func TestRunCommandTool_NonTimeoutErrorStillPropagates(t *testing.T) {
	cancelErr := fmt.Errorf("command cancelled: %w", context.Canceled)

	exec := &mockExecutor{
		execFunc: func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{Stdout: "some output"}, cancelErr
		},
	}

	tl := RunCommandTool(exec)
	input := json.RawMessage(`{"command":"true","timeout":5}`)
	got, err := tl.StructuredHandler(context.Background(), input)
	if err == nil {
		t.Fatalf("expected an error for a non-timeout Exec failure, got nil (result: %+v)", got)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(err, context.Canceled)", err)
	}
	if errors.Is(err, executor.ErrTimeout) {
		t.Errorf("err = %v, must not satisfy errors.Is(err, executor.ErrTimeout)", err)
	}
}
