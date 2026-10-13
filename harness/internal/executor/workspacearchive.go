package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrWorkspaceRootMissing is wrapped by ArchiveWorkspace when the workspace
// root directory does not exist inside the executor.
var ErrWorkspaceRootMissing = errors.New("workspace root does not exist")

// ErrArchiveTooLarge is wrapped when a workspace archive grows past the
// caller's byte cap.
var ErrArchiveTooLarge = errors.New("workspace archive exceeds size cap")

// ArchiveStats describes a completed ArchiveWorkspace call.
type ArchiveStats struct {
	// Bytes is the number of uncompressed tar bytes written.
	Bytes int64
}

// WorkspaceArchiver is the optional capability to stream the whole workspace
// as an uncompressed tar archive produced inside the executor's own
// boundary, never from the harness host's filesystem (unless the executor's
// workspace is a host directory). Entry names are relative to the workspace
// root, ownership is not meaningful, and symlinks are recorded as symlink
// entries rather than followed. Writing more than maxBytes fails with
// ErrArchiveTooLarge; a missing root fails with ErrWorkspaceRootMissing.
type WorkspaceArchiver interface {
	ArchiveWorkspace(ctx context.Context, w io.Writer, maxBytes int64) (ArchiveStats, error)
}

var (
	_ WorkspaceArchiver = (*LocalExecutor)(nil)
	_ WorkspaceArchiver = (*ContainerExecutor)(nil)
	_ WorkspaceArchiver = (*K8sExecutor)(nil)
	_ WorkspaceArchiver = (*AgentSandboxExecutor)(nil)
)

// workspaceRootMissingExit is the exit status workspaceArchiveArgv's script
// reserves for a missing root; tar itself exits 1 or 2 on failure.
const workspaceRootMissingExit = 66

// archiveStderrCap bounds the tar diagnostics kept for an error message.
const archiveStderrCap = 4 << 10

// workspaceArchiveArgv returns the argv that streams root as a tar archive
// on stdout through shell, passing root as a positional parameter so it is
// never interpolated into the script.
func workspaceArchiveArgv(shell, root string) []string {
	script := `[ -d "$1" ] || exit ` + strconv.Itoa(workspaceRootMissingExit) + `; exec tar -C "$1" -cf - .`
	return []string{shell, "-c", script, "sh", root}
}

// archiveViaExec runs a workspace archive command through run, which wires
// the command's stdout and stderr and reports its exit status (or an error
// when no status is available), and maps the result onto the
// WorkspaceArchiver error contract.
func archiveViaExec(root string, w io.Writer, maxBytes int64, run func(stdout, stderr io.Writer) (int, error)) (ArchiveStats, error) {
	out := &archiveCapWriter{w: w, max: maxBytes}
	stderr := &writeCapBuffer{limit: archiveStderrCap}
	code, err := run(out, stderr)
	stats := ArchiveStats{Bytes: out.written}
	if out.exceeded {
		return stats, fmt.Errorf("archive workspace %s: %w (%d bytes)", root, ErrArchiveTooLarge, maxBytes)
	}
	if err != nil {
		return stats, fmt.Errorf("archive workspace %s: %w", root, err)
	}
	switch code {
	case 0:
		return stats, nil
	case workspaceRootMissingExit:
		return stats, fmt.Errorf("archive workspace %s: %w", root, ErrWorkspaceRootMissing)
	}
	detail := strings.TrimSpace(stderr.String())
	if detail == "" {
		return stats, fmt.Errorf("archive workspace %s: tar exited with status %d", root, code)
	}
	return stats, fmt.Errorf("archive workspace %s: tar exited with status %d: %s", root, code, detail)
}

// archiveCapWriter forwards writes until max bytes have passed, then fails
// every write with ErrArchiveTooLarge so the producing stream aborts.
type archiveCapWriter struct {
	w        io.Writer
	max      int64
	written  int64
	exceeded bool
}

func (c *archiveCapWriter) Write(p []byte) (int, error) {
	if c.exceeded || c.written+int64(len(p)) > c.max {
		c.exceeded = true
		return 0, ErrArchiveTooLarge
	}
	n, err := c.w.Write(p)
	c.written += int64(n)
	return n, err
}

// ArchiveWorkspace streams the container's workspace with tar over the exec
// API, so the archive reflects the sandbox's view rather than the host bind
// mount's.
func (e *ContainerExecutor) ArchiveWorkspace(ctx context.Context, w io.Writer, maxBytes int64) (ArchiveStats, error) {
	return archiveViaExec(e.workspace, w, maxBytes, func(stdout, stderr io.Writer) (int, error) {
		result, err := e.execInContainerStream(ctx, workspaceArchiveArgv("sh", e.workspace), e.workspace, maxTimeout, stdout, stderr)
		if err != nil {
			return -1, err
		}
		return result.ExitCode, nil
	})
}

// ArchiveWorkspace streams the Pod's workspace with tar over pods/exec.
func (e *podExecCore) ArchiveWorkspace(ctx context.Context, w io.Writer, maxBytes int64) (ArchiveStats, error) {
	ctx, cancel := context.WithTimeout(ctx, maxTimeout)
	defer cancel()
	return archiveViaExec(k8sWorkspace, w, maxBytes, func(stdout, stderr io.Writer) (int, error) {
		err := e.streamExec(ctx, workspaceArchiveArgv("/bin/sh", k8sWorkspace), nil, stdout, stderr)
		if code, ok := extractExitCode(err); ok {
			return code, nil
		}
		return -1, err
	})
}
