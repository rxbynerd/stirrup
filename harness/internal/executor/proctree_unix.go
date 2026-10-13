//go:build unix

package executor

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const groupPollInterval = 25 * time.Millisecond

// startInOwnProcessGroup makes cmd the leader of a new process group and
// replaces its context cancellation with SIGTERM to the whole group, so a
// cancelled command's children and grandchildren are signalled with it.
func startInOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
}

// killProcessGroup waits up to grace for the group led by cmd to exit after
// the SIGTERM sent on cancellation, then sends SIGKILL to whatever remains.
// It runs after cmd.Wait has returned, so the group's leader is already
// reaped and only descendants can remain.
func killProcessGroup(cmd *exec.Cmd, grace time.Duration) {
	if cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(groupPollInterval)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}
