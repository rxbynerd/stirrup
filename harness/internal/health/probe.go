// Package health provides Kubernetes health probe helpers for the stirrup harness.
package health

import "os"

// LivenessMarker signals that the "stirrup job" process is alive: it is
// written once the harness connects to the control plane and removed only
// on process exit, so it stays present across both the assignment wait and
// task execution.
const LivenessMarker = "/tmp/healthy"

// ReadinessMarker signals that the process is idle and able to accept a
// task assignment. It is written alongside LivenessMarker but removed as
// soon as a task_assignment arrives, so it is present only during the
// assignment wait, not during execution.
const ReadinessMarker = "/tmp/ready"

// WriteProbe creates or touches a file at the given path, signalling liveness
// to a Kubernetes exec-based health probe.
func WriteProbe(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	return f.Close()
}

// RemoveProbe removes the probe file. Errors are returned but typically
// non-fatal during shutdown. A missing file is not treated as an error.
func RemoveProbe(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// CheckProbe reports whether the marker at path is present and readable.
// It opens rather than stats the file, so a permission error on a marker
// that does exist is distinguishable (via os.IsNotExist returning false)
// from the marker simply being absent.
func CheckProbe(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return f.Close()
}
