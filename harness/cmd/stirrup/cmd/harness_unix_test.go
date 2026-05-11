//go:build unix

package cmd

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestReadPromptFile_SpecialFile pins the !IsRegular() guard added
// for the --prompt-file security fix. Named pipes (FIFOs) report
// info.Size()==0 from os.Stat on both Linux and macOS, so without
// this guard:
//
//   - An unwritten FIFO would block io.ReadAll forever (the
//     harness hangs at startup with no diagnostic).
//   - A FIFO pre-loaded with >10 MiB would slip the size cap and
//     land entirely in cfg.Prompt — bypassing the documented
//     10 MiB bound.
//
// syscall.Mkfifo is preferred over os.Pipe() here because Mkfifo
// produces a real on-disk FIFO that we can hand to readPromptFile
// by path, which is exactly how a real operator would trip this.
// Linux + macOS + every BSD support Mkfifo; the build tag scopes
// the test to those platforms so Windows CI does not fail on the
// missing syscall.
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
