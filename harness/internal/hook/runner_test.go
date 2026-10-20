package hook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

// fakeExecCall records one Exec invocation for assertions on dispatch
// order and the timeout ExecRunner resolved for it.
type fakeExecCall struct {
	command string
	timeout time.Duration
}

// mockExecutor implements executor.Executor for hook.Runner unit tests.
// Only Exec and Capabilities are exercised; the file-I/O methods error
// if called.
type mockExecutor struct {
	caps     executor.ExecutorCapabilities
	execFunc func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error)
	calls    []fakeExecCall
}

func newMockExecutor() *mockExecutor {
	return &mockExecutor{
		caps: executor.ExecutorCapabilities{
			CanRead: true, CanWrite: true, CanExec: true, CanNetwork: true,
			MaxTimeout: 30 * time.Minute,
		},
	}
}

func (f *mockExecutor) ReadFile(context.Context, string) (string, error) {
	return "", errors.New("mockExecutor: ReadFile not implemented")
}

func (f *mockExecutor) WriteFile(context.Context, string, string) error {
	return errors.New("mockExecutor: WriteFile not implemented")
}

func (f *mockExecutor) ListDirectory(context.Context, string) ([]string, error) {
	return nil, errors.New("mockExecutor: ListDirectory not implemented")
}

func (f *mockExecutor) ResolvePath(relativePath string) (string, error) {
	return relativePath, nil
}

func (f *mockExecutor) Capabilities() executor.ExecutorCapabilities {
	return f.caps
}

func (f *mockExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
	f.calls = append(f.calls, fakeExecCall{command: command, timeout: timeout})
	if f.execFunc != nil {
		return f.execFunc(ctx, command, timeout)
	}
	return &executor.ExecResult{ExitCode: 0}, nil
}

func succeedingHook(name string) types.HookConfig {
	return types.HookConfig{Name: name, Command: "echo " + name}
}

// TestExecRunner_ImplementsRunner is a compile-time satisfaction guard.
func TestExecRunner_ImplementsRunner(t *testing.T) {
	var _ Runner = (*ExecRunner)(nil)
}

func TestExecRunner_RunPre_Ordering(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	exec := newMockExecutor()
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{
			succeedingHook("first"), succeedingHook("second"), succeedingHook("third"),
		}},
		Exec: exec,
	}

	results, err := r.RunPre(context.Background())
	must.NoError(err)
	must.Len(results, 3)

	wantOrder := []string{"echo first", "echo second", "echo third"}
	for i, call := range exec.calls {
		check.Equal(wantOrder[i], call.command, "call[%d].command", i)
	}
	for i, res := range results {
		check.Equal(PhasePreRun, res.Phase, "results[%d].Phase", i)
		check.Equal(i, res.Index, "results[%d].Index", i)
		check.False(res.Skipped, "results[%d] must not be skipped", i)
		check.Empty(res.Error, "results[%d] must not fail", i)
	}
}

func TestExecRunner_RunPre_FatalFailureSkipsRemaining(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	exec := newMockExecutor()
	exec.execFunc = func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
		if command == "false" {
			return &executor.ExecResult{ExitCode: 1}, nil
		}
		return &executor.ExecResult{ExitCode: 0}, nil
	}
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{
			{Name: "ok", Command: "true"},
			{Name: "boom", Command: "false"},
			{Name: "never-runs", Command: "true"},
		}},
		Exec: exec,
	}

	results, err := r.RunPre(context.Background())
	must.ErrorContains(err, "boom", "error must name the failing hook")
	must.Len(exec.calls, 2, "dispatch must stop after the fatal failure")
	must.Len(results, 3, "skipped entries must still be recorded, Index-aligned")

	check.False(results[0].Skipped, "results[0] must have succeeded")
	check.Empty(results[0].Error, "results[0] must have succeeded")
	check.False(results[1].Skipped, "results[1] must record the failure, not a skip")
	check.NotEmpty(results[1].Error, "results[1] must record the failure, not a skip")
	check.True(results[2].Skipped, "results[2] must be skipped after the fatal failure")
	check.Equal("never-runs", results[2].Name, "results[2] must keep Index alignment")
}

func TestExecRunner_ContinueOnError_DispatchContinuesAndPhaseSucceeds(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	exec := newMockExecutor()
	exec.execFunc = func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
		if command == "false" {
			return &executor.ExecResult{ExitCode: 1}, nil
		}
		return &executor.ExecResult{ExitCode: 0}, nil
	}
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{
			{Name: "soft-fail", Command: "false", ContinueOnError: true},
			{Name: "still-runs", Command: "true"},
		}},
		Exec: exec,
	}

	results, err := r.RunPre(context.Background())
	must.NoError(err, "only a continueOnError hook failed")
	must.Len(exec.calls, 2, "continueOnError must not stop the phase")

	check.True(results[0].ContinuedOnError, "results[0] must record ContinuedOnError")
	check.NotEmpty(results[0].Error, "results[0] must record the failure")
	check.False(results[1].Skipped, "results[1] must not be skipped")
}

// TestExecRunner_RunPre_WrappedErrTimeoutIsTimedOut pins that isTimeoutErr
// classifies a wrapped executor.ErrTimeout as a timeout via errors.Is, not
// by matching the formatted text.
func TestExecRunner_RunPre_WrappedErrTimeoutIsTimedOut(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	exec := newMockExecutor()
	exec.execFunc = func(_ context.Context, _ string, timeout time.Duration) (*executor.ExecResult, error) {
		return &executor.ExecResult{ExitCode: -1}, fmt.Errorf("%w after %s: %w", executor.ErrTimeout, timeout, context.DeadlineExceeded)
	}
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{{Name: "slow", Command: "sleep 999", TimeoutSeconds: 1}}},
		Exec:  exec,
	}

	results, err := r.RunPre(context.Background())
	must.Error(err, "a timed-out fatal hook must fail the phase")
	must.Len(results, 1)

	check.True(results[0].TimedOut, "results[0] must be marked as timed out")
	check.Contains(results[0].Error, "timed out", "results[0].Error must mention the timeout")
}

// TestExecRunner_BareDeadlineExceededNotTimedOut pins that a bare, unwrapped
// context.DeadlineExceeded, which does not wrap executor.ErrTimeout, is NOT
// classified as a timeout.
func TestExecRunner_BareDeadlineExceededNotTimedOut(t *testing.T) {
	must := require.New(t)
	exec := newMockExecutor()
	exec.execFunc = func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
		return nil, context.DeadlineExceeded
	}
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{{Name: "slow", Command: "sleep 999"}}},
		Exec:  exec,
	}

	results, err := r.RunPre(context.Background())
	must.Error(err)
	must.False(results[0].TimedOut, "a bare context.DeadlineExceeded does not wrap executor.ErrTimeout")
}

// TestExecRunner_CancelledContext_NotTimedOut pins that a hook killed by
// a parent-context cancellation (e.g. a SIGTERM-driven shutdown) is
// recorded as a failure but NOT as a timeout, and its error text must
// not claim the hook "timed out".
func TestExecRunner_CancelledContext_NotTimedOut(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	exec := newMockExecutor()
	exec.execFunc = func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
		return &executor.ExecResult{ExitCode: -1}, fmt.Errorf("command cancelled: %w", context.Canceled)
	}
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{{Name: "slow", Command: "sleep 999", TimeoutSeconds: 120}}},
		Exec:  exec,
	}

	results, err := r.RunPre(context.Background())
	must.Error(err, "a cancelled fatal hook must fail the phase")
	must.Len(results, 1)

	check.False(results[0].TimedOut, "a parent-context cancel is not a timeout")
	check.NotEmpty(results[0].Error, "the cancellation must be surfaced")
	check.NotContains(results[0].Error, "timed out", "a cancelled hook must not claim it timed out")
}

func TestExecRunner_RunPre_TruncatesAndScrubsOutput(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	secret := "AKIAABCDEFGHIJKLMNOP" // matches the aws_access_key_id pattern
	longOutput := strings.Repeat("x", maxOutputTailBytes*2) + secret

	exec := newMockExecutor()
	exec.execFunc = func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
		return &executor.ExecResult{ExitCode: 0, Stdout: longOutput}, nil
	}
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{{Name: "chatty", Command: "true"}}},
		Exec:  exec,
	}

	results, err := r.RunPre(context.Background())
	must.NoError(err)

	check.True(results[0].Truncated, "output over the tail cap must set Truncated")
	check.LessOrEqual(len(results[0].OutputTail), maxOutputTailBytes, "OutputTail must be capped")
	check.NotContains(results[0].OutputTail, secret, "OutputTail must not leak the unscrubbed secret")
}

// TestExecRunner_RunPre_TruncationTrimsToRuneBoundary pins that a tail cut
// landing inside a multi-byte rune drops the partial bytes. The cut falls
// one byte into the three-byte "€" sign.
func TestExecRunner_RunPre_TruncationTrimsToRuneBoundary(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const euroSign = "€"
	stderrFiller := strings.Repeat("y", maxOutputTailBytes-3)

	exec := newMockExecutor()
	exec.execFunc = func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
		return &executor.ExecResult{ExitCode: 0, Stdout: euroSign, Stderr: stderrFiller}, nil
	}
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{{Name: "utf8-boundary", Command: "true"}}},
		Exec:  exec,
	}

	results, err := r.RunPre(context.Background())
	must.NoError(err)
	tail := results[0].OutputTail

	must.True(results[0].Truncated, "output over the tail cap must set Truncated")
	must.True(utf8.ValidString(tail), "OutputTail must be valid UTF-8, got %q", tail)
	check.False(strings.ContainsRune(tail, utf8.RuneError), "OutputTail must not contain U+FFFD, got %q", tail)
	check.Equal("\n"+stderrFiller, tail, "the partial euro sign bytes must be trimmed")
}

// TestExecRunner_RunPre_StderrOnlyOutputHasNoLeadingNewline pins that when a
// hook writes nothing to stdout, OutputTail is exactly the stderr text.
func TestExecRunner_RunPre_StderrOnlyOutputHasNoLeadingNewline(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	exec := newMockExecutor()
	exec.execFunc = func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
		return &executor.ExecResult{ExitCode: 0, Stderr: "warning: something"}, nil
	}
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{{Name: "stderr-only", Command: "true"}}},
		Exec:  exec,
	}

	results, err := r.RunPre(context.Background())
	must.NoError(err)

	check.Equal("warning: something", results[0].OutputTail, "stderr-only output must not get a leading newline")
	check.False(results[0].Truncated, "short output must not be truncated")
}

func TestExecRunner_RunPost_RunOnMatrix(t *testing.T) {
	cases := []struct {
		name        string
		runOn       string
		outcome     string
		wantSkipped bool
	}{
		{"unset runOn runs after success", "", "success", false},
		{"unset runOn runs after error", "", "error", false},
		{"always runs after success", "always", "success", false},
		{"always runs after error", "always", "error", false},
		{"success runs after success", "success", "success", false},
		{"success skips after error", "success", "error", true},
		{"failure skips after success", "failure", "success", true},
		{"failure runs after error", "failure", "error", false},
		{"failure runs after timeout", "failure", "timeout", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			exec := newMockExecutor()
			r := &ExecRunner{
				Hooks: &types.HooksConfig{PostRun: []types.HookConfig{{Name: "h", Command: "true", RunOn: tc.runOn}}},
				Exec:  exec,
			}
			results, err := r.RunPost(context.Background(), tc.outcome)
			must.NoError(err)
			must.Equal(tc.wantSkipped, results[0].Skipped)

			wantDispatched := !tc.wantSkipped
			must.Equal(wantDispatched, len(exec.calls) == 1, "dispatched must equal !Skipped")
		})
	}
}

// TestExecRunner_RunPost_CancelledCtxSurfacesError pins that RunPost handles
// an already-cancelled ctx by surfacing the resulting error on the
// HookExecution rather than panicking or hanging.
func TestExecRunner_RunPost_CancelledCtxSurfacesError(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	exec := newMockExecutor()
	exec.execFunc = func(ctx context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
		return nil, ctx.Err()
	}
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PostRun: []types.HookConfig{{Name: "artifact-submit", Command: "true"}}},
		Exec:  exec,
	}

	results, err := r.RunPost(ctx, "success")
	must.Error(err, "RunPost() must fail for a cancelled ctx")

	check.NotEmpty(results[0].Error, "the ctx-cancelled error must be surfaced")
	// A plain cancel (not a deadline) must not be classified as a timeout.
	check.False(results[0].TimedOut, "an explicitly cancelled ctx is not a timeout")
}

// TestExecRunner_RunPost_BudgetExpiryMidHook pins that a detached-ctx
// deadline expiring mid-hook surfaces as a TimedOut result, not a hang.
func TestExecRunner_RunPost_BudgetExpiryMidHook(t *testing.T) {
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond) // guarantee the deadline has passed

	exec := newMockExecutor()
	exec.execFunc = func(ctx context.Context, _ string, timeout time.Duration) (*executor.ExecResult, error) {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w after %s: %w", executor.ErrTimeout, timeout, ctx.Err())
		}
		return nil, ctx.Err()
	}
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PostRun: []types.HookConfig{{Name: "upload", Command: "true"}}},
		Exec:  exec,
	}

	results, err := r.RunPost(ctx, "success")
	must.Error(err)
	must.True(results[0].TimedOut, "an expired detached-ctx budget must be a timeout")
}

func TestExecRunner_CapabilityGuard_ClampsToMaxTimeout(t *testing.T) {
	must := require.New(t)
	exec := newMockExecutor()
	exec.caps.MaxTimeout = 10 * time.Second
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{{Name: "h", Command: "true", TimeoutSeconds: 1800}}},
		Exec:  exec,
	}

	_, err := r.RunPre(context.Background())
	must.NoError(err)
	must.Len(exec.calls, 1)
	must.Equal(exec.caps.MaxTimeout, exec.calls[0].timeout, "dispatched timeout must be clamped to Capabilities().MaxTimeout")
}

func TestExecRunner_EffectiveTimeoutDefaultsWhenUnset(t *testing.T) {
	must := require.New(t)
	exec := newMockExecutor()
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{{Name: "h", Command: "true"}}},
		Exec:  exec,
	}
	_, err := r.RunPre(context.Background())
	must.NoError(err)

	want := time.Duration(types.DefaultHookTimeoutSeconds) * time.Second
	must.Equal(want, exec.calls[0].timeout, "dispatched timeout must default to the hook default")
}

func TestExecRunner_RunPre_NilExecutorReturnsError(t *testing.T) {
	must := require.New(t)
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{{Name: "h", Command: "true"}}},
	}
	results, err := r.RunPre(context.Background())
	must.Error(err, "a runner with a nil Exec must be rejected as misconfigured")
	must.NotEmpty(results[0].Error, "the misconfiguration must be recorded on the hook")
}

func TestExecRunner_NilHooksConfig_ReturnsNilResults(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	r := &ExecRunner{Exec: newMockExecutor()}

	preResults, preErr := r.RunPre(context.Background())
	must.NoError(preErr, "RunPre() with nil Hooks must not error")
	check.Nil(preResults, "RunPre() with nil Hooks must return no results")

	postResults, postErr := r.RunPost(context.Background(), "success")
	must.NoError(postErr, "RunPost() with nil Hooks must not error")
	check.Nil(postResults, "RunPost() with nil Hooks must return no results")
}
