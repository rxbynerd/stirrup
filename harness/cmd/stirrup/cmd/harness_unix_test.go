//go:build unix

package cmd

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestReadPromptFile_SpecialFile pins the !IsRegular() guard on
// --prompt-file: named pipes (FIFOs) report info.Size()==0 from
// os.Stat, so without the guard an unwritten FIFO would block
// io.ReadAll forever, and a FIFO pre-loaded with >10 MiB would slip
// the size cap. The build tag scopes the test to platforms that
// support syscall.Mkfifo.
func TestReadPromptFile_SpecialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	_, err := readPromptFile(path)
	if err == nil {
		t.Fatal("expected error for non-regular file, got nil (this would mean readPromptFile blocked on the FIFO)")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("expected non-regular-file error, got: %v", err)
	}
}
