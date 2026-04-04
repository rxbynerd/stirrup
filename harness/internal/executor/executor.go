// Package executor defines the Executor interface and implementations for
// performing file I/O and command execution within a workspace.
package executor

import (
	"context"
	"time"
)

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
