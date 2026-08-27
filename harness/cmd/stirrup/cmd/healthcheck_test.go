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

	if _, err := executeRootCmd(t, []string{"healthcheck", "--file=" + path}); err != nil {
		t.Errorf("healthcheck = %v, want nil for a present marker", err)
	}
}

func TestRunHealthcheck_MarkerAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := executeRootCmd(t, []string{"healthcheck", "--file=" + path})
	if err == nil {
		t.Fatal("healthcheck = nil, want an error for a missing marker")
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

	_, err := executeRootCmd(t, []string{"healthcheck", "--file=" + path})
	if err == nil {
		t.Fatal("healthcheck = nil, want an error for an unreadable marker")
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != exitIO {
		t.Errorf("healthcheck error = %v, want it classified as an I/O error (exit %d)", err, exitIO)
	}
}
