package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/health"
)

func TestRunHealthcheck_MarkerPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthy")
	require.NoError(t, health.WriteProbe(path), "health.WriteProbe should create the marker")

	_, err := executeRootCmd(t, []string{"healthcheck", "--file=" + path})
	require.NoError(t, err)
}

func TestRunHealthcheck_MarkerAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := executeRootCmd(t, []string{"healthcheck", "--file=" + path})
	require.Error(t, err)
	require.Equal(t, 1, classifyExitCode(err), "absent marker should map to the default precondition exit code")
}

func TestRunHealthcheck_MarkerUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced when running as root")
	}

	path := filepath.Join(t.TempDir(), "healthy")
	require.NoError(t, health.WriteProbe(path), "health.WriteProbe should create the marker")
	require.NoError(t, os.Chmod(path, 0o000), "os.Chmod should remove the marker's read permission")

	_, err := executeRootCmd(t, []string{"healthcheck", "--file=" + path})
	require.Error(t, err)

	var ee *exitError
	require.ErrorAs(t, err, &ee, "unreadable marker should surface as an exitError")
	require.Equal(t, exitIO, ee.code, "unreadable marker should exit with the I/O error code")
}
