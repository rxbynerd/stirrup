package verifier

import (
	"context"
	"fmt"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

const (
	// defaultTestTimeout is the default timeout for test commands.
	defaultTestTimeout = 5 * time.Minute

	// maxFeedbackLen is the maximum length of test output included in
	// verification feedback. Longer output is truncated from the front
	// so the tail (which typically contains the summary/errors) is preserved.
	maxFeedbackLen = 4000
)

// commandExecutor is the subset of executor.Executor that TestRunnerVerifier
// needs. Defined locally so callers can supply a lightweight mock in tests
// without implementing the full Executor interface.
type commandExecutor interface {
	Exec(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error)
}

// TestRunnerVerifier runs a shell command (typically a test suite) and
// reports whether the command exits successfully. On failure the combined
// stdout+stderr output is included as feedback so the agent can diagnose
// and fix the problem.
type TestRunnerVerifier struct {
	command string
	timeout time.Duration
}

// NewTestRunnerVerifier creates a verifier that runs the given command.
// If timeout is zero the default of 5 minutes is used.
func NewTestRunnerVerifier(command string, timeout time.Duration) *TestRunnerVerifier {
	if timeout <= 0 {
		timeout = defaultTestTimeout
	}
	return &TestRunnerVerifier{
		command: command,
		timeout: timeout,
	}
}

// Verify runs the configured test command via the executor found in
// vc.Executor. The executor field is typed as any to avoid circular
// dependencies; Verify performs a type assertion to the local
// commandExecutor interface.
func (t *TestRunnerVerifier) Verify(ctx context.Context, vc VerifyContext) (*types.VerificationResult, error) {
	exec, ok := vc.Executor.(commandExecutor)
	if !ok {
		return nil, fmt.Errorf("test-runner verifier: executor does not implement Exec (got %T)", vc.Executor)
	}

	result, err := exec.Exec(ctx, t.command, t.timeout)
	if err != nil {
		return nil, fmt.Errorf("test-runner verifier: exec failed: %w", err)
	}

	if result.ExitCode == 0 {
		return &types.VerificationResult{
			Passed: true,
			Details: map[string]any{
				"command":  t.command,
				"exitCode": result.ExitCode,
			},
		}, nil
	}

	output := combineOutput(result.Stdout, result.Stderr)
	output = truncateFeedback(output, maxFeedbackLen)

	return &types.VerificationResult{
		Passed:   false,
		Feedback: fmt.Sprintf("Test command %q failed (exit code %d):\n%s", t.command, result.ExitCode, output),
		Details: map[string]any{
			"command":  t.command,
			"exitCode": result.ExitCode,
		},
	}, nil
}

// combineOutput merges stdout and stderr into a single string.
func combineOutput(stdout, stderr string) string {
	if stdout == "" {
		return stderr
	}
	if stderr == "" {
		return stdout
	}
	return stdout + "\n" + stderr
}

// truncateFeedback keeps the last maxLen bytes of s, since the end of test
// output (summary, error messages) is usually the most useful part.
func truncateFeedback(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return "[...truncated...]\n" + s[len(s)-maxLen:]
}
