package health

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteProbe_CreatesFile(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "healthy")

	must.NoError(WriteProbe(path))

	_, err := os.Stat(path)
	must.NoError(err, "probe file does not exist after WriteProbe")
}

// TestWriteProbe_MissingParentDirReturnsError pins the caller-visible
// contract runJob relies on: a write failure (e.g. a read-only or absent
// /tmp) is reported, not silently swallowed, so the caller can choose to
// warn and continue rather than crash the job.
func TestWriteProbe_MissingParentDirReturnsError(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "missing-parent", "healthy")

	must.Error(WriteProbe(path))
}

func TestWriteProbe_Idempotent(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "healthy")

	must.NoError(WriteProbe(path), "first WriteProbe() call")
	must.NoError(WriteProbe(path), "second WriteProbe() call")

	_, err := os.Stat(path)
	must.NoError(err, "probe file does not exist after second WriteProbe")
}

func TestRemoveProbe_RemovesFile(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "healthy")

	must.NoError(WriteProbe(path))
	must.NoError(RemoveProbe(path))

	_, err := os.Stat(path)
	must.True(os.IsNotExist(err), "probe file still exists after RemoveProbe")
}

func TestRemoveProbe_NonexistentFileReturnsNil(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "does-not-exist")

	must.NoError(RemoveProbe(path))
}

func TestCheckProbe_MarkerPresent(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "healthy")

	must.NoError(WriteProbe(path))
	must.NoError(CheckProbe(path))
}

func TestCheckProbe_MarkerAbsent(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "does-not-exist")

	err := CheckProbe(path)
	must.Error(err)
	must.True(os.IsNotExist(err), "CheckProbe() error = %v, want os.IsNotExist to be true", err)
}

func TestCheckProbe_MarkerUnreadable(t *testing.T) {
	must := require.New(t)
	if os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced when running as root")
	}

	path := filepath.Join(t.TempDir(), "healthy")
	must.NoError(WriteProbe(path))
	must.NoError(os.Chmod(path, 0o000))

	err := CheckProbe(path)
	must.Error(err)
	must.False(os.IsNotExist(err), "CheckProbe() error = %v, want a permission error distinguishable from absent", err)
}
