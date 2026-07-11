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
// a genuine timeout and any other reason Exec's context ends — most
// importantly a SIGTERM-driven parent-context cancellation, which must NOT
// satisfy errors.Is(err, ErrTimeout). Callers (notably the hook runner,
// harness/internal/hook/runner.go) match on this sentinel instead of the
// error's formatted text, so a wording change in one executor can't
// silently break TimedOut classification downstream (#468).
//
// The container executor also routes its Docker Engine API deadlines
// through this sentinel (#S2): the short control-plane calls
// (create/start/stop/exec-create/exec-inspect) and the file I/O paths
// (ReadFile/WriteFile) have no caller-supplied timeout of their own, so
// they apply an internal one and classify a resulting ctx expiry the same
// way, rather than inventing a second, parallel timeout-detection
// mechanism — see container_api.go's classifyControlPlaneErr and
// container.go's classifyFileIOCtxErr.
var ErrTimeout = errors.New("command timed out")

// classifyExecCtxErr builds the error an Executor's Exec method returns once
// its ctx (a context.WithTimeout child of the caller's ctx) is Done after a
// failed command. context.DeadlineExceeded means the per-call timeout
// genuinely elapsed, so the result wraps both ErrTimeout and the underlying
// context error. Any other ctx.Err() (context.Canceled, or a custom cause
// propagated from an ancestor via context.WithCancelCause — e.g. the
// control plane's cancel or a SIGTERM-driven shutdown) is reported as a
// cancellation, not a timeout: previously local.go and container.go
// reported *any* ctx cancellation as "command timed out after <configured
// timeout>", corrupting HookExecution.TimedOut for a signal-killed hook and
// misleading operators triaging traces (#469). Shared by local.go,
// container.go, and k8s_execcore.go (and, via its embedded podExecCore, the
// k8s-sandbox executor) so all executors classify identically.
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
