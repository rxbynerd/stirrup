package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/health"
)

func TestRunHealthcheck_MarkerPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthy")
	if err := health.WriteProbe(path); err != nil {
		t.Fatalf("health.WriteProbe() error: %v", err)
	}
	healthcheckFile = path
	t.Cleanup(func() { healthcheckFile = health.LivenessMarker })

	if err := runHealthcheck(healthcheckCmd, nil); err != nil {
		t.Errorf("runHealthcheck() = %v, want nil for a present marker", err)
	}
}

func TestRunHealthcheck_MarkerAbsent(t *testing.T) {
	healthcheckFile = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { healthcheckFile = health.LivenessMarker })

	err := runHealthcheck(healthcheckCmd, nil)
	if err == nil {
		t.Fatal("runHealthcheck() = nil, want an error for a missing marker")
	}
	if classifyExitCode(err) != 1 {
		t.Errorf("classifyExitCode(err) = %d, want 1 (default/precondition) for an absent marker", classifyExitCode(err))
	}
}

func TestRunHealthcheck_MarkerUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced when running as root")
	}

	path := filepath.Join(t.TempDir(), "healthy")
	if err := health.WriteProbe(path); err != nil {
		t.Fatalf("health.WriteProbe() error: %v", err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("os.Chmod() error: %v", err)
	}
	healthcheckFile = path
	t.Cleanup(func() { healthcheckFile = health.LivenessMarker })

	err := runHealthcheck(healthcheckCmd, nil)
	if err == nil {
		t.Fatal("runHealthcheck() = nil, want an error for an unreadable marker")
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != exitIO {
		t.Errorf("runHealthcheck() error = %v, want it classified as an I/O error (exit %d)", err, exitIO)
	}
}
