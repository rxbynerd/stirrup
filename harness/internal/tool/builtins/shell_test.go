package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

// TestFormatRunCommand_ExitCodeOnlyNoOutput locks the exact text for a command
// that produced no stdout or stderr but exited non-zero. The "[exit code: N]"
// line is written with a leading newline unconditionally, so an otherwise empty
// rendering still begins with "\n"; a regression here would silently change
// the model-visible output for silent-but-failing commands.
func TestFormatRunCommand_ExitCodeOnlyNoOutput(t *testing.T) {
	must := require.New(t)
	want := "\n[exit code: 2]"
	must.Equal(want, formatRunCommand("", "", 2))
}

// TestRunCommandTool_TimeoutClampedTo300s pins the run_command tool's
// independent 300s (5 min) timeout ceiling, which stays decoupled from the
// shared (and larger) executor.maxTimeout cap — see the maxTimeout doc
// comment in executor/local.go — so raising the executor cap does not
// silently hand the model-reachable tool a longer exec budget.
func TestRunCommandTool_TimeoutClampedTo300s(t *testing.T) {
	must := require.New(t)
	var gotTimeout time.Duration
	exec := &mockExecutor{
		execFunc: func(_ context.Context, _ string, timeout time.Duration) (*executor.ExecResult, error) {
			gotTimeout = timeout
			return &executor.ExecResult{ExitCode: 0}, nil
		},
	}

	tl := RunCommandTool(exec)
	input := json.RawMessage(`{"command":"true","timeout":9000}`)
	_, err := tl.StructuredHandler(context.Background(), input)
	must.NoError(err)

	want := 300 * time.Second
	must.Equal(want, gotTimeout, "tool-layer clamp must stay independent of the raised executor cap")
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
	must := require.New(t)
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
	must.NoError(err, "a timeout must not be a hard error")

	t.Run("kind", func(t *testing.T) {
		must := require.New(t)
		must.Equal(kindCommandResult, got.Kind)
	})

	t.Run("structured payload", func(t *testing.T) {
		must := require.New(t)
		var structured commandResult
		must.NoError(json.Unmarshal(got.Structured, &structured))

		want := commandResult{
			Stdout:         "partial stdout before kill\n",
			Stderr:         "partial stderr before kill\n",
			ExitCode:       0,
			TimedOut:       true,
			TimeoutSeconds: 5,
		}
		must.Equal(want, structured)
	})

	t.Run("text fallback", func(t *testing.T) {
		must := require.New(t)
		must.Contains(got.Text, "partial stdout before kill", "text must preserve partial stdout")
		must.Contains(got.Text, "[timed out after 5s]", "text must carry an explicit timed-out marker")
	})
}

// TestRunCommandTool_NonTimeoutErrorStillPropagates is the counterpart to
// TestRunCommandTool_TimeoutReturnsPartialOutputAsSoftOutcome: an Exec error
// that does NOT wrap executor.ErrTimeout (e.g. a plain context cancellation,
// or any other exec failure) must still propagate as a hard tool error
// exactly as before. This guards against a classification that is too
// broad — errors.Is must exclude cancellation so a SIGTERM-driven shutdown
// isn't mistaken for a timed-out command's soft outcome.
func TestRunCommandTool_NonTimeoutErrorStillPropagates(t *testing.T) {
	must := require.New(t)
	cancelErr := fmt.Errorf("command cancelled: %w", context.Canceled)

	exec := &mockExecutor{
		execFunc: func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{Stdout: "some output"}, cancelErr
		},
	}

	tl := RunCommandTool(exec)
	input := json.RawMessage(`{"command":"true","timeout":5}`)
	got, err := tl.StructuredHandler(context.Background(), input)
	must.Error(err, "a non-timeout exec failure must be a hard error, got result %+v", got)
	must.ErrorIs(err, context.Canceled)
	must.NotErrorIs(err, executor.ErrTimeout)
}

func TestRunCommandTool_DescriptionStatesProcessTreeKillGuarantee(t *testing.T) {
	desc := RunCommandTool(&mockExecutor{}).Description

	for _, tc := range []struct {
		name string
		want string
	}{
		{name: "kills started processes", want: "the processes it started are killed"},
		{name: "mentions sigkill", want: "SIGKILL"},
		{name: "covers detached processes", want: "detached into their own session or process group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Contains(desc, tc.want)
		})
	}
}

// TestRunCommandTool_UncapturedOutputBoundedKeepsStderrAndExitCode checks
// that without a capture store run_command bounds its own text to the
// result budget while keeping the head and tail of both streams and the
// exit-code and timeout markers that follow them.
func TestRunCommandTool_UncapturedOutputBoundedKeepsStderrAndExitCode(t *testing.T) {
	must := require.New(t)
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

	tl := RunCommandToolWithStore(exec, nil, types.CommandOutputConfig{}, budget)
	got, err := tl.StructuredHandler(context.Background(), json.RawMessage(`{"command":"build","timeout":5}`))
	must.NoError(err)
	must.LessOrEqual(len(got.Text), budget, "text must fit the result budget")

	for _, tc := range []struct {
		name string
		want string
	}{
		{name: "stdout head", want: "STDOUT-HEAD"},
		{name: "stdout tail", want: "STDOUT-TAIL"},
		{name: "stdout truncation marker", want: "[run_command stdout truncated: "},
		{name: "stderr head", want: "\nSTDERR:\nSTDERR-HEAD"},
		{name: "stderr tail", want: "STDERR-TAIL"},
		{name: "stderr truncation marker", want: "[run_command stderr truncated: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Contains(got.Text, tc.want)
		})
	}

	must.True(strings.HasSuffix(got.Text, "STDERR-TAIL\n[exit code: 1]\n[timed out after 5s]"),
		"text must end with the stderr tail, then the exit-code and timeout markers")
	must.Nil(got.Structured, "truncated result must not keep its structured envelope")
	must.Empty(got.Kind, "truncated result must not keep its structured envelope")
	must.True(tl.SelfBoundedResult, "uncaptured run_command must report SelfBoundedResult")
}

// TestRunCommandTool_UncapturedSmallStderrKeptWhole checks that a short
// stderr is never elided and stdout receives the rest of the budget.
func TestRunCommandTool_UncapturedSmallStderrKeptWhole(t *testing.T) {
	must := require.New(t)
	const budget = 4 << 10
	got, truncated := boundCommandText(strings.Repeat("o", 1<<20), "error: build failed", 2, "", budget)

	must.True(truncated, "a 1 MiB stdout must be truncated")
	must.LessOrEqual(len(got), budget)
	must.True(strings.HasSuffix(got, "\nSTDERR:\nerror: build failed\n[exit code: 2]"),
		"stderr and exit code must survive truncation")
	must.GreaterOrEqual(len(got), budget-64, "stdout should use the space stderr left unused")
}

// TestElideMiddle_RespectsRuneBoundaries checks that elision never splits a
// multi-byte character.
func TestElideMiddle_RespectsRuneBoundaries(t *testing.T) {
	s := strings.Repeat("€", 10_000)
	for _, limit := range []int{10, 200, 201, 202, 1000} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			must := require.New(t)
			got := elideMiddle(s, limit, "stdout")
			must.LessOrEqual(len(got), limit)
			must.True(utf8.ValidString(got), "result must be valid UTF-8")
		})
	}
}
