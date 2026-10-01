//go:build unix

package judge

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroup runs cmd in its own process group and makes
// cancellation kill the whole group, so no child git spawns outlives the
// deadline.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
