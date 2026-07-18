package hook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	exec := newMockExecutor()
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{
			succeedingHook("first"), succeedingHook("second"), succeedingHook("third"),
		}},
		Exec: exec,
	}

	results, err := r.RunPre(context.Background())
	if err != nil {
		t.Fatalf("RunPre() error = %v, want nil", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	wantOrder := []string{"echo first", "echo second", "echo third"}
	for i, call := range exec.calls {
		if call.command != wantOrder[i] {
			t.Errorf("call[%d].command = %q, want %q", i, call.command, wantOrder[i])
		}
	}
	for i, res := range results {
		if res.Phase != PhasePreRun {
			t.Errorf("results[%d].Phase = %q, want %q", i, res.Phase, PhasePreRun)
		}
		if res.Index != i {
			t.Errorf("results[%d].Index = %d, want %d", i, res.Index, i)
		}
		if res.Skipped || res.Error != "" {
			t.Errorf("results[%d] unexpectedly failed/skipped: %+v", i, res)
		}
	}
}

func TestExecRunner_RunPre_FatalFailureSkipsRemaining(t *testing.T) {
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
	if err == nil {
		t.Fatal("RunPre() error = nil, want non-nil after a fatal hook failure")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error must name the failing hook, got: %v", err)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("Exec called %d times, want 2 (dispatch must stop after the fatal failure)", len(exec.calls))
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3 (skipped entries still recorded, Index-aligned)", len(results))
	}
	if results[0].Skipped || results[0].Error != "" {
		t.Errorf("results[0] must have succeeded: %+v", results[0])
	}
	if results[1].Skipped || results[1].Error == "" {
		t.Errorf("results[1] must record the failure, not a skip: %+v", results[1])
	}
	if !results[2].Skipped {
		t.Errorf("results[2] must be Skipped after the fatal failure: %+v", results[2])
	}
	if results[2].Name != "never-runs" {
		t.Errorf("results[2].Name = %q, want %q (Index alignment)", results[2].Name, "never-runs")
	}
}

func TestExecRunner_ContinueOnError_DispatchContinuesAndPhaseSucceeds(t *testing.T) {
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
	if err != nil {
		t.Fatalf("RunPre() error = %v, want nil (only continueOnError hook failed)", err)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("Exec called %d times, want 2 (continueOnError must not stop the phase)", len(exec.calls))
	}
	if !results[0].ContinuedOnError || results[0].Error == "" {
		t.Errorf("results[0] must record ContinuedOnError with an Error, got: %+v", results[0])
	}
	if results[1].Skipped {
		t.Errorf("results[1] must not be skipped: %+v", results[1])
	}
}

// TestExecRunner_TimedOut pins that isTimeoutErr classifies a wrapped
// executor.ErrTimeout as a timeout via errors.Is, not by matching the
// formatted text.
func TestExecRunner_TimedOut(t *testing.T) {
	exec := newMockExecutor()
	exec.execFunc = func(_ context.Context, _ string, timeout time.Duration) (*executor.ExecResult, error) {
		return &executor.ExecResult{ExitCode: -1}, fmt.Errorf("%w after %s: %w", executor.ErrTimeout, timeout, context.DeadlineExceeded)
	}
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{{Name: "slow", Command: "sleep 999", TimeoutSeconds: 1}}},
		Exec:  exec,
	}

	results, err := r.RunPre(context.Background())
	if err == nil {
		t.Fatal("RunPre() error = nil, want non-nil for a timed-out fatal hook")
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if !results[0].TimedOut {
		t.Errorf("results[0].TimedOut = false, want true")
	}
	if !strings.Contains(results[0].Error, "timed out") {
		t.Errorf("results[0].Error = %q, want it to mention the timeout", results[0].Error)
	}
}

// TestExecRunner_DeadlineExceededTimedOut pins that a bare, unwrapped
// context.DeadlineExceeded — not itself wrapping executor.ErrTimeout — is
// NOT classified as a timeout.
func TestExecRunner_DeadlineExceededTimedOut(t *testing.T) {
	exec := newMockExecutor()
	exec.execFunc = func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
		return nil, context.DeadlineExceeded
	}
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{{Name: "slow", Command: "sleep 999"}}},
		Exec:  exec,
	}

	results, err := r.RunPre(context.Background())
	if err == nil {
		t.Fatal("RunPre() error = nil, want non-nil")
	}
	if results[0].TimedOut {
		t.Errorf("results[0].TimedOut = true, want false: a bare context.DeadlineExceeded does not wrap executor.ErrTimeout")
	}
}

// TestExecRunner_CancelledContext_NotTimedOut pins that a hook killed by
// a parent-context cancellation (e.g. a SIGTERM-driven shutdown) is
// recorded as a failure but NOT as a timeout, and its error text must
// not claim the hook "timed out".
func TestExecRunner_CancelledContext_NotTimedOut(t *testing.T) {
	exec := newMockExecutor()
	exec.execFunc = func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
		return &executor.ExecResult{ExitCode: -1}, fmt.Errorf("command cancelled: %w", context.Canceled)
	}
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{{Name: "slow", Command: "sleep 999", TimeoutSeconds: 120}}},
		Exec:  exec,
	}

	results, err := r.RunPre(context.Background())
	if err == nil {
		t.Fatal("RunPre() error = nil, want non-nil for a cancelled fatal hook")
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].TimedOut {
		t.Errorf("results[0].TimedOut = true, want false: a parent-context cancel is not a timeout")
	}
	if results[0].Error == "" {
		t.Error("results[0].Error is empty, want the cancellation surfaced")
	}
	if strings.Contains(results[0].Error, "timed out") {
		t.Errorf("results[0].Error = %q, must not claim the hook timed out (it was cancelled, not deadline-expired)", results[0].Error)
	}
}

func TestExecRunner_TruncationAndScrub(t *testing.T) {
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
	if err != nil {
		t.Fatalf("RunPre() error = %v, want nil", err)
	}
	if !results[0].Truncated {
		t.Error("results[0].Truncated = false, want true")
	}
	if len(results[0].OutputTail) > maxOutputTailBytes {
		t.Errorf("len(OutputTail) = %d, want <= %d", len(results[0].OutputTail), maxOutputTailBytes)
	}
	if strings.Contains(results[0].OutputTail, secret) {
		t.Errorf("OutputTail leaked the unscrubbed secret: %q", results[0].OutputTail)
	}
}

// TestExecRunner_TruncationTrimsUTF8RuneBoundary is a regression fixture
// for a byte-index tail cut that lands mid-rune: "€" encodes as 3 bytes,
// so combined = "€" + "\n" + 4093 "y"s cuts exactly 1 byte into "€",
// leaving a bare continuation-byte pair at the start before
// trimToRuneBoundary runs.
func TestExecRunner_TruncationTrimsUTF8RuneBoundary(t *testing.T) {
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
	if err != nil {
		t.Fatalf("RunPre() error = %v, want nil", err)
	}
	tail := results[0].OutputTail

	if !results[0].Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if !utf8.ValidString(tail) {
		t.Fatalf("OutputTail is not valid UTF-8: %q", tail)
	}
	if strings.ContainsRune(tail, utf8.RuneError) {
		t.Errorf("OutputTail contains U+FFFD (replacement character), want none introduced: %q", tail)
	}
	want := "\n" + stderrFiller
	if tail != want {
		t.Errorf("OutputTail = %q, want %q (the straddled euro sign's continuation bytes trimmed)", tail, want)
	}
}

// TestExecRunner_StderrOnlyOutput pins that when a hook writes nothing
// to stdout, OutputTail is exactly the stderr text with no spurious
// leading newline.
func TestExecRunner_StderrOnlyOutput(t *testing.T) {
	exec := newMockExecutor()
	exec.execFunc = func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
		return &executor.ExecResult{ExitCode: 0, Stderr: "warning: something"}, nil
	}
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{{Name: "stderr-only", Command: "true"}}},
		Exec:  exec,
	}

	results, err := r.RunPre(context.Background())
	if err != nil {
		t.Fatalf("RunPre() error = %v, want nil", err)
	}
	if results[0].OutputTail != "warning: something" {
		t.Errorf("OutputTail = %q, want %q (no leading newline for stderr-only output)", results[0].OutputTail, "warning: something")
	}
	if results[0].Truncated {
		t.Error("Truncated = true, want false for short output")
	}
}

func TestExecRunner_RunPost_RunOnMatrix(t *testing.T) {
	cases := []struct {
		runOn       string
		outcome     string
		wantSkipped bool
	}{
		{"", "success", false},
		{"", "error", false},
		{"always", "success", false},
		{"always", "error", false},
		{"success", "success", false},
		{"success", "error", true},
		{"failure", "success", true},
		{"failure", "error", false},
		{"failure", "timeout", false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("runOn=%s/outcome=%s", tc.runOn, tc.outcome), func(t *testing.T) {
			exec := newMockExecutor()
			r := &ExecRunner{
				Hooks: &types.HooksConfig{PostRun: []types.HookConfig{{Name: "h", Command: "true", RunOn: tc.runOn}}},
				Exec:  exec,
			}
			results, err := r.RunPost(context.Background(), tc.outcome)
			if err != nil {
				t.Fatalf("RunPost() error = %v, want nil", err)
			}
			if results[0].Skipped != tc.wantSkipped {
				t.Errorf("Skipped = %v, want %v", results[0].Skipped, tc.wantSkipped)
			}
			gotDispatched := len(exec.calls) == 1
			if gotDispatched == tc.wantSkipped {
				t.Errorf("dispatched = %v, want dispatched == !Skipped", gotDispatched)
			}
		})
	}
}

// TestExecRunner_RunPost_DeadCtx pins that RunPost handles an
// already-cancelled ctx by surfacing the resulting error on the
// HookExecution rather than panicking or hanging.
func TestExecRunner_RunPost_DeadCtx(t *testing.T) {
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
	if err == nil {
		t.Fatal("RunPost() error = nil, want non-nil for a dead ctx")
	}
	if results[0].Error == "" {
		t.Error("results[0].Error is empty, want the ctx-cancelled error surfaced")
	}
	// A plain cancel (not a deadline) must not be classified as a timeout.
	if results[0].TimedOut {
		t.Error("results[0].TimedOut = true, want false: an explicitly cancelled ctx is not a timeout")
	}
}

// TestExecRunner_RunPost_BudgetExpiryMidHook pins that a detached-ctx
// deadline expiring mid-hook surfaces as a TimedOut result, not a hang.
func TestExecRunner_RunPost_BudgetExpiryMidHook(t *testing.T) {
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
	if err == nil {
		t.Fatal("RunPost() error = nil, want non-nil")
	}
	if !results[0].TimedOut {
		t.Errorf("results[0].TimedOut = false, want true for an expired detached-ctx budget")
	}
}

func TestExecRunner_CapabilityGuard_ClampsToMaxTimeout(t *testing.T) {
	exec := newMockExecutor()
	exec.caps.MaxTimeout = 10 * time.Second
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{{Name: "h", Command: "true", TimeoutSeconds: 1800}}},
		Exec:  exec,
	}

	if _, err := r.RunPre(context.Background()); err != nil {
		t.Fatalf("RunPre() error = %v, want nil", err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("Exec called %d times, want 1", len(exec.calls))
	}
	if exec.calls[0].timeout != exec.caps.MaxTimeout {
		t.Errorf("dispatched timeout = %v, want clamped to Capabilities().MaxTimeout = %v", exec.calls[0].timeout, exec.caps.MaxTimeout)
	}
}

func TestExecRunner_EffectiveTimeoutDefaultsWhenUnset(t *testing.T) {
	exec := newMockExecutor()
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{{Name: "h", Command: "true"}}},
		Exec:  exec,
	}
	if _, err := r.RunPre(context.Background()); err != nil {
		t.Fatalf("RunPre() error = %v, want nil", err)
	}
	want := time.Duration(types.DefaultHookTimeoutSeconds) * time.Second
	if exec.calls[0].timeout != want {
		t.Errorf("dispatched timeout = %v, want default %v", exec.calls[0].timeout, want)
	}
}

func TestExecRunner_NilExecutor(t *testing.T) {
	r := &ExecRunner{
		Hooks: &types.HooksConfig{PreRun: []types.HookConfig{{Name: "h", Command: "true"}}},
	}
	results, err := r.RunPre(context.Background())
	if err == nil {
		t.Fatal("RunPre() error = nil, want non-nil for a misconfigured (nil Exec) runner")
	}
	if results[0].Error == "" {
		t.Error("results[0].Error is empty, want a misconfiguration message")
	}
}

func TestExecRunner_NilHooksConfig(t *testing.T) {
	r := &ExecRunner{Exec: newMockExecutor()}
	preResults, preErr := r.RunPre(context.Background())
	if preErr != nil || preResults != nil {
		t.Errorf("RunPre() with nil Hooks = (%v, %v), want (nil, nil)", preResults, preErr)
	}
	postResults, postErr := r.RunPost(context.Background(), "success")
	if postErr != nil || postResults != nil {
		t.Errorf("RunPost() with nil Hooks = (%v, %v), want (nil, nil)", postResults, postErr)
	}
}
