// Package health provides Kubernetes health probe helpers for the stirrup harness.
package health

import "os"

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
