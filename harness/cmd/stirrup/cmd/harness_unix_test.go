//go:build unix

package cmd

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReadPromptFile_RejectsNamedPipe pins the !IsRegular() guard on
// --prompt-file: named pipes (FIFOs) report info.Size()==0 from
// os.Stat, so without the guard an unwritten FIFO would block
// io.ReadAll forever, and a FIFO pre-loaded with >10 MiB would slip
// the size cap. The build tag scopes the test to platforms that
// support syscall.Mkfifo.
func TestReadPromptFile_RejectsNamedPipe(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "fifo")
	must.NoError(syscall.Mkfifo(path, 0o600), "mkfifo")

	_, err := readPromptFile(path)
	must.Error(err, "readPromptFile must reject the FIFO instead of blocking on it")
	must.ErrorContains(err, "not a regular file")
}
