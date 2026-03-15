package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestExecutor(t *testing.T) (*LocalExecutor, string) {
	t.Helper()
	dir := t.TempDir()
	// On macOS, t.TempDir() returns /var/... but EvalSymlinks resolves to
	// /private/var/.... Resolve here so test assertions match.
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	exec, err := NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}
	return exec, dir
}

func TestNewLocalExecutor_InvalidPaths(t *testing.T) {
	_, err := NewLocalExecutor("/nonexistent/path/that/does/not/exist")
	if err == nil {
		t.Fatal("expected error for nonexistent workspace")
	}

	// File, not directory.
	f := filepath.Join(t.TempDir(), "file.txt")
	os.WriteFile(f, []byte("x"), 0o644)
	_, err = NewLocalExecutor(f)
	if err == nil {
		t.Fatal("expected error for file workspace")
	}
}

func TestResolvePath_Basic(t *testing.T) {
	exec, dir := newTestExecutor(t)

	resolved, err := exec.ResolvePath("foo.txt")
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	if resolved != filepath.Join(dir, "foo.txt") {
		t.Errorf("got %q, want %q", resolved, filepath.Join(dir, "foo.txt"))
	}
}

func TestResolvePath_TraversalRejected(t *testing.T) {
	exec, _ := newTestExecutor(t)

	cases := []string{
		"../etc/passwd",
		"foo/../../etc/passwd",
		"../../../tmp/evil",
	}
	for _, p := range cases {
		_, err := exec.ResolvePath(p)
		if err == nil {
			t.Errorf("expected error for path traversal %q", p)
		}
	}
}

func TestResolvePath_AbsoluteOutsideWorkspace(t *testing.T) {
	exec, _ := newTestExecutor(t)
	_, err := exec.ResolvePath("/etc/passwd")
	if err == nil {
		t.Fatal("expected error for absolute path outside workspace")
	}
}

func TestResolvePath_SymlinkEscapesWorkspace(t *testing.T) {
	exec, dir := newTestExecutor(t)

	// Create a symlink inside the workspace that points to /tmp (outside).
	link := filepath.Join(dir, "escape_link")
	if err := os.Symlink("/tmp", link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// Attempting to resolve a path through the symlink should fail because
	// the resolved target (/tmp/...) is outside the workspace.
	_, err := exec.ResolvePath("escape_link/somefile")
	if err == nil {
		t.Fatal("expected error for symlink escaping workspace")
	}
	if !strings.Contains(err.Error(), "escapes workspace") {
		t.Errorf("error should mention workspace escape, got: %v", err)
	}

	// The symlink target itself should also be rejected.
	_, err = exec.ResolvePath("escape_link")
	if err == nil {
		t.Fatal("expected error for symlink target outside workspace")
	}
}

func TestResolvePath_AbsoluteInsideWorkspace(t *testing.T) {
	exec, dir := newTestExecutor(t)

	// Create a file inside the workspace.
	testFile := filepath.Join(dir, "inner.txt")
	os.WriteFile(testFile, []byte("hi"), 0o644)

	// An absolute path that falls inside the workspace should be accepted.
	resolved, err := exec.ResolvePath(testFile)
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	if resolved != testFile {
		t.Errorf("got %q, want %q", resolved, testFile)
	}
}

func TestReadFile(t *testing.T) {
	exec, dir := newTestExecutor(t)
	content := "hello world"
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte(content), 0o644)

	got, err := exec.ReadFile(context.Background(), "test.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got != content {
		t.Errorf("got %q, want %q", got, content)
	}
}

func TestReadFile_Directory(t *testing.T) {
	exec, dir := newTestExecutor(t)
	os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)

	_, err := exec.ReadFile(context.Background(), "subdir")
	if err == nil {
		t.Fatal("expected error reading directory as file")
	}
}

func TestReadFile_TooLarge(t *testing.T) {
	exec, dir := newTestExecutor(t)
	// Create a file just over the limit.
	f, _ := os.Create(filepath.Join(dir, "big.bin"))
	f.Truncate(maxFileSize + 1)
	f.Close()

	_, err := exec.ReadFile(context.Background(), "big.bin")
	if err == nil {
		t.Fatal("expected error for oversized file")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWriteFile(t *testing.T) {
	exec, dir := newTestExecutor(t)

	err := exec.WriteFile(context.Background(), "sub/dir/test.txt", "content")
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "sub", "dir", "test.txt"))
	if string(data) != "content" {
		t.Errorf("got %q, want %q", string(data), "content")
	}
}

func TestWriteFile_TooLarge(t *testing.T) {
	exec, _ := newTestExecutor(t)
	bigContent := strings.Repeat("x", maxFileSize+1)
	err := exec.WriteFile(context.Background(), "big.txt", bigContent)
	if err == nil {
		t.Fatal("expected error for oversized content")
	}
}

func TestWriteFile_TraversalRejected(t *testing.T) {
	exec, _ := newTestExecutor(t)
	err := exec.WriteFile(context.Background(), "../escape.txt", "bad")
	if err == nil {
		t.Fatal("expected error for path traversal write")
	}
}

func TestListDirectory(t *testing.T) {
	exec, dir := newTestExecutor(t)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte(""), 0o644)
	os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)

	entries, err := exec.ListDirectory(context.Background(), ".")
	if err != nil {
		t.Fatalf("ListDirectory: %v", err)
	}

	found := map[string]bool{}
	for _, e := range entries {
		found[e] = true
	}
	if !found["a.txt"] {
		t.Error("missing a.txt")
	}
	if !found["subdir/"] {
		t.Error("missing subdir/ (should have trailing slash)")
	}
}

func TestExec_Basic(t *testing.T) {
	exec, _ := newTestExecutor(t)

	result, err := exec.Exec(context.Background(), "echo hello", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code: got %d, want 0", result.ExitCode)
	}
	if strings.TrimSpace(result.Stdout) != "hello" {
		t.Errorf("stdout: got %q, want %q", result.Stdout, "hello\n")
	}
}

func TestExec_NonZeroExit(t *testing.T) {
	exec, _ := newTestExecutor(t)

	result, err := exec.Exec(context.Background(), "exit 42", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 42 {
		t.Errorf("exit code: got %d, want 42", result.ExitCode)
	}
}

func TestExec_Timeout(t *testing.T) {
	exec, _ := newTestExecutor(t)

	_, err := exec.Exec(context.Background(), "sleep 60", 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestExec_MaxTimeoutCapped(t *testing.T) {
	exec, _ := newTestExecutor(t)

	// Passing a timeout exceeding max should be capped, not rejected.
	result, err := exec.Exec(context.Background(), "echo ok", 10*time.Minute)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "ok" {
		t.Errorf("stdout: got %q, want %q", result.Stdout, "ok\n")
	}
}

func TestExec_OutputTruncation(t *testing.T) {
	exec, dir := newTestExecutor(t)

	// Create a file larger than maxOutputSize and cat it.
	bigFile := filepath.Join(dir, "big.txt")
	data := strings.Repeat("A", maxOutputSize+1000)
	os.WriteFile(bigFile, []byte(data), 0o644)

	result, err := exec.Exec(context.Background(), "cat big.txt", 10*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.HasSuffix(result.Stdout, truncatedSuffix) {
		t.Errorf("expected truncation suffix on large output, got last 50 chars: %q", result.Stdout[len(result.Stdout)-50:])
	}
}

func TestExec_WorkingDirectory(t *testing.T) {
	exec, dir := newTestExecutor(t)

	result, err := exec.Exec(context.Background(), "pwd", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != dir {
		t.Errorf("working dir: got %q, want %q", strings.TrimSpace(result.Stdout), dir)
	}
}

func TestCapabilities(t *testing.T) {
	exec, _ := newTestExecutor(t)
	caps := exec.Capabilities()

	if !caps.CanRead || !caps.CanWrite || !caps.CanExec || !caps.CanNetwork {
		t.Error("local executor should have all capabilities enabled")
	}
	if caps.MaxTimeout != maxTimeout {
		t.Errorf("MaxTimeout: got %v, want %v", caps.MaxTimeout, maxTimeout)
	}
}
