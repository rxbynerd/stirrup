//go:build unix

package judge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunGit_KillsHangingChildrenAtDeadline(t *testing.T) {
	requireGit(t)
	bin := tempDir(t)
	pidFile := filepath.Join(bin, "child.pid")
	writeFiles(t, bin, map[string]string{"git": "#!/bin/sh\nsleep 15 &\necho $! > " + pidFile + "\nsleep 15\n"})
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := runGit(ctx, bin, nil, nil, "status")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("runGit returned after %v; the 1s deadline was not honoured", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline error", err)
	}

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("reading child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("git's background child outlived the deadline")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
