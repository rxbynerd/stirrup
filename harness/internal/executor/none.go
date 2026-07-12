package executor

import (
	"context"
	"fmt"
	"time"
)

// NoneExecutor implements the Executor interface with no execution surface
// at all: every file I/O and command operation is unsupported. It is for
// MCP-only / server-side-tool runs where the harness needs no local
// filesystem or shell access — unlike the "api" executor, it does not even
// offer read access to a remote VCS host.
type NoneExecutor struct{}

// NewNoneExecutor creates an executor with no filesystem or shell surface.
// It takes no arguments: there are no secrets or credentials to resolve.
func NewNoneExecutor() *NoneExecutor {
	return &NoneExecutor{}
}

// ReadFile is not supported by the none executor.
func (n *NoneExecutor) ReadFile(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("none executor: read not supported")
}

// WriteFile is not supported by the none executor.
func (n *NoneExecutor) WriteFile(_ context.Context, _ string, _ string) error {
	return fmt.Errorf("none executor: write not supported")
}

// ListDirectory is not supported by the none executor.
func (n *NoneExecutor) ListDirectory(_ context.Context, _ string) ([]string, error) {
	return nil, fmt.Errorf("none executor: list not supported")
}

// Exec is not supported by the none executor.
func (n *NoneExecutor) Exec(_ context.Context, _ string, _ time.Duration) (*ExecResult, error) {
	return nil, fmt.Errorf("none executor: exec not supported")
}

// ResolvePath returns the path as-is since there is no local filesystem.
func (n *NoneExecutor) ResolvePath(path string) (string, error) {
	return path, nil
}

// Capabilities returns no capabilities at all: the none executor has no
// execution surface.
func (n *NoneExecutor) Capabilities() ExecutorCapabilities {
	return ExecutorCapabilities{
		CanRead:    false,
		CanWrite:   false,
		CanExec:    false,
		CanNetwork: false,
	}
}
