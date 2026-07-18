// Package executor defines the Executor interface and implementations for
// performing file I/O and command execution within a workspace.
package executor

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrTimeout is the sentinel every Executor implementation's Exec method
// wraps (via %w) into the returned error when a command is killed because
// its per-call deadline elapsed. It is the load-bearing distinction between
// a genuine timeout and any other reason Exec's context ends — a
// SIGTERM-driven parent-context cancellation must NOT satisfy
// errors.Is(err, ErrTimeout). Callers (notably the hook runner) match on
// this sentinel rather than the error's formatted text. See
// docs/architecture.md for the cross-executor classification contract.
var ErrTimeout = errors.New("command timed out")

// classifyExecCtxErr builds the error an Executor's Exec method returns once
// its ctx is Done after a failed command: a genuine DeadlineExceeded wraps
// ErrTimeout, while any other ctx.Err() (cancellation) is reported as such,
// not a timeout. Shared by local.go, container.go, and k8s_execcore.go so
// all executors classify identically.
func classifyExecCtxErr(ctx context.Context, timeout time.Duration) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w after %s: %w", ErrTimeout, timeout, ctx.Err())
	}
	return fmt.Errorf("command cancelled: %w", ctx.Err())
}

// ExecResult holds the output of a command execution.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// ExecutorCapabilities describes what operations an executor supports.
type ExecutorCapabilities struct {
	CanRead    bool
	CanWrite   bool
	CanExec    bool
	CanNetwork bool
	MaxTimeout time.Duration
}

// Executor is the interface for performing file I/O and command execution
// within a sandboxed workspace. All paths are relative to the workspace root
// unless otherwise noted.
type Executor interface {
	ReadFile(ctx context.Context, path string) (string, error)
	WriteFile(ctx context.Context, path string, content string) error
	ListDirectory(ctx context.Context, path string) ([]string, error)
	Exec(ctx context.Context, command string, timeout time.Duration) (*ExecResult, error)
	ResolvePath(relativePath string) (string, error)
	Capabilities() ExecutorCapabilities
}
