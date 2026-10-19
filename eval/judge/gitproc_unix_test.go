//go:build unix

package judge

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunGit_KillsHangingChildrenAtDeadline(t *testing.T) {
	requireGit(t)
	bin := tempDir(t)
	pidFile := filepath.Join(bin, "child.pid")
	writeFiles(t, bin, map[string]string{"git": "#!/bin/sh\nsleep 15 &\necho $! > " + pidFile + "\nsleep 15\n"})
	require.NoError(t, os.Chmod(filepath.Join(bin, "git"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := runGit(ctx, bin, nil, nil, "status")
	elapsed := time.Since(start)
	assert.LessOrEqual(t, elapsed, 5*time.Second, "runGit returned after %v; the 1s deadline was not honoured", elapsed)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	data, err := os.ReadFile(pidFile)
	require.NoError(t, err, "reading child pid")
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)

	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			require.Fail(t, "git's background child outlived the deadline")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
