package builtins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

// TestFormatRunCommand_ExitCodeOnlyNoOutput locks the exact text for a command
// that produced no stdout or stderr but exited non-zero. The "[exit code: N]"
// line is written with a leading newline unconditionally, so an otherwise empty
// rendering still begins with "\n"; a regression here would silently change
// the model-visible output for silent-but-failing commands.
func TestFormatRunCommand_ExitCodeOnlyNoOutput(t *testing.T) {
	got := formatRunCommand("", "", 2)
	const want = "\n[exit code: 2]"
	if got != want {
		t.Fatalf("formatRunCommand(\"\", \"\", 2) = %q, want %q", got, want)
	}
}

// TestRunCommandTool_TimeoutClampedTo300s pins the run_command tool's
// independent 300s (5 min) timeout ceiling, which stays decoupled from the
// shared (and larger) executor.maxTimeout cap — see the maxTimeout doc
// comment in executor/local.go — so raising the executor cap does not
// silently hand the model-reachable tool a longer exec budget.
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

// TestRunCommandTool_TimeoutReturnsPartialOutputAsSoftOutcome pins the
// tool-layer contract: an executor.Exec error that wraps executor.ErrTimeout
// must not surface as a hard tool error, since every executor implementation
// still returns whatever partial stdout/stderr it captured before the kill.
// The handler must classify the error via errors.Is (mirroring
// harness/internal/hook/runner.go's isTimeoutErr), report TimedOut in the
// structured payload, preserve the partial output, and make the timeout
// unambiguous in the Text fallback so a model reading only Text (not
// Structured) can still tell a timeout apart from a clean exit.
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
	want := commandResult{
		Stdout:         "partial stdout before kill\n",
		Stderr:         "partial stderr before kill\n",
		ExitCode:       0,
		TimedOut:       true,
		TimeoutSeconds: 5,
	}
	if structured != want {
		t.Errorf("structured mismatch\n got: %+v\nwant: %+v", structured, want)
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

func TestRunCommandTool_DescriptionStatesProcessTreeKillGuarantee(t *testing.T) {
	desc := RunCommandTool(&mockExecutor{}).Description
	for _, want := range []string{"the processes it started are killed", "SIGKILL", "detached into their own session or process group"} {
		if !strings.Contains(desc, want) {
			t.Errorf("run_command description missing %q: %s", want, desc)
		}
	}
}

// TestRunCommandTool_UncapturedOutputBoundedKeepsStderrAndExitCode checks
// that without a capture store run_command bounds its own text to the
// result budget while keeping the head and tail of both streams and the
// exit-code and timeout markers that follow them.
func TestRunCommandTool_UncapturedOutputBoundedKeepsStderrAndExitCode(t *testing.T) {
	const budget = 8 << 10
	execErr := fmt.Errorf("%w after 5s", executor.ErrTimeout)
	exec := &mockExecutor{
		execFunc: func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{
				Stdout:   "STDOUT-HEAD" + strings.Repeat("o", 3_000_000) + "STDOUT-TAIL",
				Stderr:   "STDERR-HEAD" + strings.Repeat("e", 200_000) + "STDERR-TAIL",
				ExitCode: 1,
			}, execErr
		},
	}

	got, err := RunCommandToolWithStore(exec, nil, types.CommandOutputConfig{}, budget).StructuredHandler(context.Background(), json.RawMessage(`{"command":"build","timeout":5}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Text) > budget {
		t.Fatalf("Text is %d bytes, want <= %d", len(got.Text), budget)
	}
	for _, want := range []string{
		"STDOUT-HEAD", "STDOUT-TAIL", "[run_command stdout truncated: ",
		"\nSTDERR:\nSTDERR-HEAD", "STDERR-TAIL", "[run_command stderr truncated: ",
	} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("Text is missing %q", want)
		}
	}
	if !strings.HasSuffix(got.Text, "STDERR-TAIL\n[exit code: 1]\n[timed out after 5s]") {
		t.Errorf("Text ends %q, want the stderr tail then the exit-code and timeout markers", got.Text[len(got.Text)-80:])
	}
	if got.Structured != nil || got.Kind != "" {
		t.Errorf("truncated result kept its structured envelope (kind %q)", got.Kind)
	}
	if !RunCommandToolWithStore(exec, nil, types.CommandOutputConfig{}, budget).SelfBoundedResult {
		t.Error("uncaptured run_command must report SelfBoundedResult")
	}
}

// TestRunCommandTool_UncapturedSmallStderrKeptWhole checks that a short
// stderr is never elided and stdout receives the rest of the budget.
func TestRunCommandTool_UncapturedSmallStderrKeptWhole(t *testing.T) {
	const budget = 4 << 10
	got, truncated := boundCommandText(strings.Repeat("o", 1<<20), "error: build failed", 2, "", budget)
	if !truncated || len(got) > budget {
		t.Fatalf("got %d bytes (truncated=%t), want <= %d and truncated", len(got), truncated, budget)
	}
	if !strings.HasSuffix(got, "\nSTDERR:\nerror: build failed\n[exit code: 2]") {
		t.Errorf("stderr or exit code lost: %q", got[len(got)-80:])
	}
	if len(got) < budget-64 {
		t.Errorf("got %d bytes, want stdout to use the space stderr left unused", len(got))
	}
}

// TestElideMiddle_RespectsRuneBoundaries checks that elision never splits a
// multi-byte character.
func TestElideMiddle_RespectsRuneBoundaries(t *testing.T) {
	s := strings.Repeat("€", 10_000)
	for _, limit := range []int{10, 200, 201, 202, 1000} {
		got := elideMiddle(s, limit, "stdout")
		if len(got) > limit {
			t.Errorf("limit %d: got %d bytes", limit, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("limit %d: result is not valid UTF-8", limit)
		}
	}
}
