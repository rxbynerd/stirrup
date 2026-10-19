package health

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteProbe_CreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthy")

	require.NoError(t, WriteProbe(path))

	_, err := os.Stat(path)
	require.NoError(t, err, "probe file does not exist after WriteProbe")
}

// TestWriteProbe_MissingParentDirReturnsError pins the caller-visible
// contract runJob relies on: a write failure (e.g. a read-only or absent
// /tmp) is reported, not silently swallowed, so the caller can choose to
// warn and continue rather than crash the job.
func TestWriteProbe_MissingParentDirReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-parent", "healthy")

	require.Error(t, WriteProbe(path))
}

func TestWriteProbe_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthy")

	require.NoError(t, WriteProbe(path), "first WriteProbe() call")
	require.NoError(t, WriteProbe(path), "second WriteProbe() call")

	_, err := os.Stat(path)
	require.NoError(t, err, "probe file does not exist after second WriteProbe")
}

func TestRemoveProbe_RemovesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthy")

	require.NoError(t, WriteProbe(path))
	require.NoError(t, RemoveProbe(path))

	_, err := os.Stat(path)
	require.True(t, os.IsNotExist(err), "probe file still exists after RemoveProbe")
}

func TestRemoveProbe_NonexistentFileReturnsNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")

	require.NoError(t, RemoveProbe(path))
}

func TestCheckProbe_MarkerPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthy")

	require.NoError(t, WriteProbe(path))
	require.NoError(t, CheckProbe(path))
}

func TestCheckProbe_MarkerAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")

	err := CheckProbe(path)
	require.Error(t, err)
	require.True(t, os.IsNotExist(err), "CheckProbe() error = %v, want os.IsNotExist to be true", err)
}

func TestCheckProbe_MarkerUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced when running as root")
	}

	path := filepath.Join(t.TempDir(), "healthy")
	require.NoError(t, WriteProbe(path))
	require.NoError(t, os.Chmod(path, 0o000))

	err := CheckProbe(path)
	require.Error(t, err)
	require.False(t, os.IsNotExist(err), "CheckProbe() error = %v, want a permission error distinguishable from absent", err)
}
