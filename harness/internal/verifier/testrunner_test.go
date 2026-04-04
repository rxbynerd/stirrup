package verifier

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// mockExecutor implements the commandExecutor interface for testing.
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

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Passed {
		t.Fatal("expected Passed to be true")
	}
	if result.Feedback != "" {
		t.Errorf("expected no feedback on pass, got %q", result.Feedback)
	}
	if mock.lastCommand != "go test ./..." {
		t.Errorf("expected command %q, got %q", "go test ./...", mock.lastCommand)
	}
	if mock.lastTimeout != 30*time.Second {
		t.Errorf("expected timeout %v, got %v", 30*time.Second, mock.lastTimeout)
	}
	// Verify details are populated.
	if result.Details["command"] != "go test ./..." {
		t.Errorf("expected details command, got %v", result.Details["command"])
	}
	if result.Details["exitCode"] != 0 {
		t.Errorf("expected details exitCode 0, got %v", result.Details["exitCode"])
	}
}

func TestTestRunnerVerifier_FailingTests(t *testing.T) {
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

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Passed {
		t.Fatal("expected Passed to be false")
	}
	if !strings.Contains(result.Feedback, "npm test") {
		t.Errorf("feedback should mention the command, got %q", result.Feedback)
	}
	if !strings.Contains(result.Feedback, "exit code 1") {
		t.Errorf("feedback should mention the exit code, got %q", result.Feedback)
	}
	if !strings.Contains(result.Feedback, "FAIL: TestSomething") {
		t.Errorf("feedback should include test output, got %q", result.Feedback)
	}
	if result.Details["exitCode"] != 1 {
		t.Errorf("expected details exitCode 1, got %v", result.Details["exitCode"])
	}
	// Default timeout should have been applied.
	if mock.lastTimeout != defaultTestTimeout {
		t.Errorf("expected default timeout %v, got %v", defaultTestTimeout, mock.lastTimeout)
	}
}

func TestTestRunnerVerifier_OutputTruncation(t *testing.T) {
	// Generate output larger than maxFeedbackLen.
	bigOutput := strings.Repeat("FAIL: line of test output\n", 300)
	if len(bigOutput) <= maxFeedbackLen {
		t.Fatal("test setup error: output should exceed maxFeedbackLen")
	}

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

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Passed {
		t.Fatal("expected Passed to be false")
	}
	// The feedback should be bounded in length. The overhead from the
	// "Test command ... failed ..." prefix and truncation marker means the
	// total will be somewhat larger than maxFeedbackLen, but the test
	// output portion itself must be capped.
	if !strings.Contains(result.Feedback, "[...truncated...]") {
		t.Error("expected truncation marker in feedback")
	}
	// The tail of the output should be preserved (most useful part).
	if !strings.HasSuffix(strings.TrimSpace(result.Feedback), "FAIL: line of test output") {
		t.Errorf("expected tail of output to be preserved, got %q", result.Feedback[len(result.Feedback)-80:])
	}
}

func TestTestRunnerVerifier_ExecError(t *testing.T) {
	mock := &mockExecutor{
		result: nil,
		err:    fmt.Errorf("command timed out"),
	}

	v := NewTestRunnerVerifier("go test ./...", time.Minute)
	_, err := v.Verify(context.Background(), VerifyContext{
		Executor: mock,
	})

	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "exec failed") {
		t.Errorf("error should mention exec failure, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "command timed out") {
		t.Errorf("error should wrap underlying cause, got %q", err.Error())
	}
}

func TestTestRunnerVerifier_InvalidExecutor(t *testing.T) {
	v := NewTestRunnerVerifier("go test ./...", time.Minute)

	// Pass a string instead of an executor.
	_, err := v.Verify(context.Background(), VerifyContext{
		Executor: "not an executor",
	})

	if err == nil {
		t.Fatal("expected error for invalid executor type")
	}
	if !strings.Contains(err.Error(), "does not implement Exec") {
		t.Errorf("error should mention missing Exec, got %q", err.Error())
	}
}

func TestTestRunnerVerifier_NilExecutor(t *testing.T) {
	v := NewTestRunnerVerifier("go test ./...", time.Minute)

	_, err := v.Verify(context.Background(), VerifyContext{
		Executor: nil,
	})

	if err == nil {
		t.Fatal("expected error for nil executor")
	}
}

func TestCombineOutput(t *testing.T) {
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
			got := combineOutput(tt.stdout, tt.stderr)
			if got != tt.want {
				t.Errorf("combineOutput(%q, %q) = %q, want %q", tt.stdout, tt.stderr, got, tt.want)
			}
		})
	}
}

func TestTruncateFeedback(t *testing.T) {
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
			got := truncateFeedback(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncateFeedback(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestNewTestRunnerVerifier_DefaultTimeout(t *testing.T) {
	v := NewTestRunnerVerifier("go test ./...", 0)
	if v.timeout != defaultTestTimeout {
		t.Errorf("expected default timeout %v, got %v", defaultTestTimeout, v.timeout)
	}
}

func TestNewTestRunnerVerifier_NegativeTimeout(t *testing.T) {
	v := NewTestRunnerVerifier("go test ./...", -1*time.Second)
	if v.timeout != defaultTestTimeout {
		t.Errorf("expected default timeout %v for negative input, got %v", defaultTestTimeout, v.timeout)
	}
}

// Verify that TestRunnerVerifier satisfies the Verifier interface.
var _ Verifier = (*TestRunnerVerifier)(nil)

// Verify that mockExecutor satisfies commandExecutor.
var _ commandExecutor = (*mockExecutor)(nil)

// Verify that passing test returns expected VerificationResult type.
func TestTestRunnerVerifier_ReturnsCorrectType(t *testing.T) {
	mock := &mockExecutor{
		result: &executor.ExecResult{ExitCode: 0},
	}
	v := NewTestRunnerVerifier("echo ok", time.Second)
	result, err := v.Verify(context.Background(), VerifyContext{Executor: mock})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}
