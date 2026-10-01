//go:build !unix

package judge

import "os/exec"

// isolateProcessGroup is a no-op where process groups are unavailable;
// cmd.WaitDelay still bounds the wait for git's children.
func isolateProcessGroup(*exec.Cmd) {}
