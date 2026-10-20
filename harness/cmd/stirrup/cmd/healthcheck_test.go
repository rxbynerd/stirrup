package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/health"
)

func TestRunHealthcheck_MarkerPresent(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "healthy")
	must.NoError(health.WriteProbe(path), "health.WriteProbe should create the marker")

	_, err := executeRootCmd(t, []string{"healthcheck", "--file=" + path})
	must.NoError(err)
}

func TestRunHealthcheck_MarkerAbsent(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := executeRootCmd(t, []string{"healthcheck", "--file=" + path})
	must.Error(err)
	must.Equal(1, classifyExitCode(err), "absent marker should map to the default precondition exit code")
}

func TestRunHealthcheck_MarkerUnreadable(t *testing.T) {
	must := require.New(t)
	if os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced when running as root")
	}

	path := filepath.Join(t.TempDir(), "healthy")
	must.NoError(health.WriteProbe(path), "health.WriteProbe should create the marker")
	must.NoError(os.Chmod(path, 0o000), "os.Chmod should remove the marker's read permission")

	_, err := executeRootCmd(t, []string{"healthcheck", "--file=" + path})
	must.Error(err)

	var ee *exitError
	must.ErrorAs(err, &ee, "unreadable marker should surface as an exitError")
	must.Equal(exitIO, ee.code, "unreadable marker should exit with the I/O error code")
}
