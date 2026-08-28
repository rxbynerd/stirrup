// Package executor defines the Executor interface and implementations for
// performing file I/O and command execution within a workspace.
package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// TreeEntry is one file in an executor-served file tree. Path is relative to
// the root passed to ListTree and is always slash-separated. Size is the
// file's byte length, which lets callers skip oversized files without
// fetching them.
type TreeEntry struct {
	Path string
	Size int64
}

// TreeListing is the result of enumerating a workspace file tree. Truncated
// reports that the backing store capped the enumeration, so the entry list
// must be treated as incomplete.
type TreeListing struct {
	Entries   []TreeEntry
	Truncated bool
}

// TreeLister is the optional whole-tree enumeration capability. Executors
// whose workspace has no counterpart on the harness host's filesystem must
// implement it: without it, tools that enumerate files would have to walk
// ResolvePath's result, which for such an executor names a remote or virtual
// tree and would instead read the harness process's own filesystem.
type TreeLister interface {
	ListTree(ctx context.Context, root string) (TreeListing, error)
}

// HostPathWorkspace is the optional capability marking an executor whose
// workspace is a directory on the harness host's own filesystem, so a
// ResolvePath result may be dereferenced with host filesystem APIs
// (filepath.WalkDir, os.ReadFile). Executors whose workspace lives behind a
// sandbox or API boundary must not implement it, even when the workspace
// happens to be bind-mounted from the host: the sandbox boundary is the
// point, and searches on those executors must run inside the sandbox.
type HostPathWorkspace interface {
	HostWorkspaceRoot() string
}

// StreamingExecutor is the optional full-output execution capability used by
// run_command. Exec remains bounded for hooks, verifiers, git helpers, and
// legacy callers; production executors implement this interface to stream
// stdout/stderr directly into the harness-owned compliance store.
type StreamingExecutor interface {
	ExecStream(ctx context.Context, command string, timeout time.Duration, stdout, stderr io.Writer) (*ExecResult, error)
}
