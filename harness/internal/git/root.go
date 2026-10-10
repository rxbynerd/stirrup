package git

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// Execer runs one shell command inside the sandbox, with the workspace
// root as its working directory. executor.Executor satisfies it.
type Execer interface {
	Exec(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error)
}

// WorkspaceRoot resolves the executor's working directory to its physical
// path inside the sandbox, the same form `git rev-parse --show-toplevel`
// reports, so the two can be compared to detect an enclosing repository.
func WorkspaceRoot(ctx context.Context, e Execer) (string, error) {
	res, err := e.Exec(ctx, "pwd -P", commandTimeout)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	if res == nil || res.ExitCode != 0 {
		return "", fmt.Errorf("resolve workspace root: %s", execFailure(res))
	}
	root := strings.TrimSuffix(res.Stdout, "\n")
	if root == "" || !strings.HasPrefix(root, "/") || strings.ContainsAny(root, "\n\r\x00") {
		return "", fmt.Errorf("resolve workspace root: pwd returned %q, not an absolute path", truncate(root))
	}
	return root, nil
}
