//go:build !unix

package executor

import (
	"os/exec"
	"time"
)

// Process groups are a Unix concept; elsewhere only the direct child is
// killed on cancellation.
func startInOwnProcessGroup(*exec.Cmd) {}

func killProcessGroup(*exec.Cmd, time.Duration) {}
