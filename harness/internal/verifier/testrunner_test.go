package verifier

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

type mockExecutor struct {
	result *executor.ExecResult
	err    error

	// Captured arguments from the last Exec call.
	lastCommand string
	lastTimeout time.Duration
}

func (m *mockExecutor) Exec(_ context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
	m.lastCommand = command
	m.lastTimeout = timeout
	return m.result, m.err
}

func TestTestRunnerVerifier_PassingTests(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{
		result: &executor.ExecResult{
			ExitCode: 0,
			Stdout:   "ok  \tgithub.com/example/pkg\t0.042s\n",
			Stderr:   "",
		},
	}

	v := NewTestRunnerVerifier("go test ./...", 30*time.Second)
	result, err := v.Verify(context.Background(), VerifyContext{
		Executor: mock,
	})

	must.NoError(err)
	must.True(result.Passed)
	must.Empty(result.Feedback)
	must.Equal("go test ./...", mock.lastCommand)
	must.Equal(30*time.Second, mock.lastTimeout)
	must.Equal("go test ./...", result.Details["command"])
	must.Equal(0, result.Details["exitCode"])
}

func TestTestRunnerVerifier_FailingTests(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{
		result: &executor.ExecResult{
			ExitCode: 1,
			Stdout:   "--- FAIL: TestSomething (0.00s)\n    expected 1, got 2\nFAIL\n",
			Stderr:   "exit status 1\n",
		},
	}

	v := NewTestRunnerVerifier("npm test", 0) // 0 triggers default timeout
	result, err := v.Verify(context.Background(), VerifyContext{
		Executor: mock,
	})

	must.NoError(err)
	must.False(result.Passed)
	must.Contains(result.Feedback, "npm test")
	must.Contains(result.Feedback, "exit code 1")
	must.Contains(result.Feedback, "FAIL: TestSomething")
	must.Equal(1, result.Details["exitCode"])
	must.Equal(defaultTestTimeout, mock.lastTimeout)
}

func TestTestRunnerVerifier_OutputTruncation(t *testing.T) {
	must := require.New(t)
	bigOutput := strings.Repeat("FAIL: line of test output\n", 300)
	must.Greater(len(bigOutput), maxFeedbackLen, "test setup error: output should exceed maxFeedbackLen")

	mock := &mockExecutor{
		result: &executor.ExecResult{
			ExitCode: 2,
			Stdout:   bigOutput,
			Stderr:   "",
		},
	}

	v := NewTestRunnerVerifier("make test", time.Minute)
	result, err := v.Verify(context.Background(), VerifyContext{
		Executor: mock,
	})

	must.NoError(err)
	must.False(result.Passed)
	must.Contains(result.Feedback, "[...truncated...]")
	must.True(strings.HasSuffix(strings.TrimSpace(result.Feedback), "FAIL: line of test output"), "feedback should keep the tail of the output")
}

func TestTestRunnerVerifier_ExecError(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{
		result: nil,
		err:    fmt.Errorf("command timed out"),
	}

	v := NewTestRunnerVerifier("go test ./...", time.Minute)
	_, err := v.Verify(context.Background(), VerifyContext{
		Executor: mock,
	})

	must.Error(err)
	must.ErrorContains(err, "exec failed")
	must.ErrorContains(err, "command timed out")
}

func TestTestRunnerVerifier_InvalidExecutor(t *testing.T) {
	must := require.New(t)
	v := NewTestRunnerVerifier("go test ./...", time.Minute)

	_, err := v.Verify(context.Background(), VerifyContext{
		Executor: "not an executor",
	})

	must.Error(err)
	must.ErrorContains(err, "does not implement Exec")
}

func TestTestRunnerVerifier_NilExecutor(t *testing.T) {
	must := require.New(t)
	v := NewTestRunnerVerifier("go test ./...", time.Minute)

	_, err := v.Verify(context.Background(), VerifyContext{
		Executor: nil,
	})

	must.Error(err)
}

func TestCombineOutput_JoinsStdoutAndStderr(t *testing.T) {
	tests := []struct {
		name           string
		stdout, stderr string
		want           string
	}{
		{"both empty", "", "", ""},
		{"stdout only", "out", "", "out"},
		{"stderr only", "", "err", "err"},
		{"both present", "out", "err", "out\nerr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			got := combineOutput(tt.stdout, tt.stderr)
			must.Equal(tt.want, got)
		})
	}
}

func TestTruncateFeedback_KeepsTailAndMarksCut(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"short enough", "hello", 10, "hello"},
		{"exact limit", "hello", 5, "hello"},
		{"needs truncation", "abcdefghij", 5, "[...truncated...]\nfghij"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			got := truncateFeedback(tt.input, tt.maxLen)
			must.Equal(tt.want, got)
		})
	}
}

func TestNewTestRunnerVerifier_DefaultTimeout(t *testing.T) {
	must := require.New(t)
	v := NewTestRunnerVerifier("go test ./...", 0)
	must.Equal(defaultTestTimeout, v.timeout)
}

func TestNewTestRunnerVerifier_NegativeTimeout(t *testing.T) {
	must := require.New(t)
	v := NewTestRunnerVerifier("go test ./...", -1*time.Second)
	must.Equal(defaultTestTimeout, v.timeout)
}

var _ Verifier = (*TestRunnerVerifier)(nil)

var _ commandExecutor = (*mockExecutor)(nil)

func TestTestRunnerVerifier_ReturnsVerificationResult(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{
		result: &executor.ExecResult{ExitCode: 0},
	}
	v := NewTestRunnerVerifier("echo ok", time.Second)
	result, err := v.Verify(context.Background(), VerifyContext{Executor: mock})
	must.NoError(err)
	var _ *types.VerificationResult = result //nolint:staticcheck // intentional compile-time type check
}
