package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
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
	_ = os.WriteFile(f, []byte("x"), 0o644)
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
	_ = os.WriteFile(testFile, []byte("hi"), 0o644)

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
	_ = os.WriteFile(filepath.Join(dir, "test.txt"), []byte(content), 0o644)

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
	_ = os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)

	_, err := exec.ReadFile(context.Background(), "subdir")
	if err == nil {
		t.Fatal("expected error reading directory as file")
	}
}

func TestReadFile_TooLarge(t *testing.T) {
	exec, dir := newTestExecutor(t)
	// Create a file just over the limit.
	f, _ := os.Create(filepath.Join(dir, "big.bin"))
	_ = f.Truncate(maxFileSize + 1)
	_ = f.Close()

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
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte(""), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)

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
	// A genuine deadline expiry must satisfy errors.Is against the shared
	// executor sentinel (#468: hook.isTimeoutErr keys off this instead of
	// matching the formatted text).
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v, want errors.Is(err, ErrTimeout)", err)
	}
}

// TestExec_CancelledContext_NotErrTimeout is the #469 regression: a
// context cancellation that is NOT a deadline expiry (e.g. a SIGTERM-driven
// parent-context cancel) must not be reported as a timeout, and it must not
// satisfy errors.Is(err, ErrTimeout) — even though a large configured
// timeout (60s) is in play, only 200ms actually elapsed. Output already
// captured before the cancel must still be returned (matches local's
// pre-existing partial-output behaviour; container.go and k8s_execcore.go
// previously discarded it — see #473).
func TestExec_CancelledContext_NotErrTimeout(t *testing.T) {
	exec, _ := newTestExecutor(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	result, err := exec.Exec(ctx, "echo started; sleep 30", 60*time.Second)
	if err == nil {
		t.Fatal("expected an error from the cancelled command")
	}
	if errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v, must not satisfy errors.Is(err, ErrTimeout): the command was cancelled after 200ms, not deadline-expired after the configured 60s", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(err, context.Canceled)", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, must not claim the command timed out", err)
	}
	if result == nil || !strings.Contains(result.Stdout, "started") {
		t.Errorf("result = %+v, want Stdout to preserve output captured before the cancel", result)
	}
}

// TestExec_KillsOrphanedGrandchildPromptly is a regression test for
// issue #461's finding #1 remediation: a compound command
// ("cmd1; sleep N; cmd2") runs its later stages as children the shell
// forks and waits on, not an exec-replaced process. Without
// cmd.WaitDelay, killing only the direct "sh" child on ctx cancellation
// leaves the still-running "sleep" grandchild holding the stdout pipe
// open, so Exec blocks until the grandchild exits on its own — the
// exact bug a manual end-to-end SIGTERM test surfaced (a postRun hook
// outlived its process-shutdown signal by the length of its own
// sleep). A single-command "sleep N" (see TestExec_Timeout) does not
// reproduce this: sh execs directly into it with no fork.
func TestExec_KillsOrphanedGrandchildPromptly(t *testing.T) {
	exec, _ := newTestExecutor(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := exec.Exec(ctx, "echo start; sleep 30; echo finished", 60*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from the cancelled command")
	}
	// Bounded well under the 30s grandchild sleep; generous enough to
	// be non-flaky (200ms cancel delay + shortCommandKillGrace + CI
	// scheduling slack), but nowhere near the un-fixed ~30s.
	if elapsed > 5*time.Second {
		t.Errorf("Exec took %v after ctx cancellation, want well under 5s (orphaned grandchild must not block Wait())", elapsed)
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
	_ = os.WriteFile(bigFile, []byte(data), 0o644)

	result, err := exec.Exec(context.Background(), "cat big.txt", 10*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.HasSuffix(result.Stdout, truncatedSuffix) {
		t.Errorf("expected truncation suffix on large output, got last 50 chars: %q", result.Stdout[len(result.Stdout)-50:])
	}
}

// capturingEmitter records the originalSize argument of the most recent
// OutputTruncated call so tests can assert the true pre-truncation byte count.
type capturingEmitter struct {
	outputTruncCount int
	lastOriginalSize int
}

func (e *capturingEmitter) PathTraversalBlocked(_, _ string)           {}
func (e *capturingEmitter) FileSizeLimitExceeded(_ string, _, _ int64) {}
func (e *capturingEmitter) OutputTruncated(_ string, originalSize, _ int) {
	e.outputTruncCount++
	e.lastOriginalSize = originalSize
}

var _ SecurityEventEmitter = (*capturingEmitter)(nil)

// TestExec_OutputStreamingBounded asserts that streaming far more than the cap
// does not retain more than maxOutputSize bytes of payload — peak memory is
// bounded, not proportional to total output — while the OutputTruncated event
// still reports the TRUE pre-truncation byte count, matching the container path.
func TestExec_OutputStreamingBounded(t *testing.T) {
	exec, _ := newTestExecutor(t)

	emitter := &capturingEmitter{}
	exec.Security = emitter

	// 16 MB of stdout, well past the 1 MB cap.
	const totalBytes = 16777216
	result, err := exec.Exec(context.Background(), "head -c 16777216 /dev/zero", 30*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	payload := strings.TrimSuffix(result.Stdout, truncatedSuffix)
	if len(payload) != maxOutputSize {
		t.Errorf("retained payload = %d bytes, want exactly cap %d", len(payload), maxOutputSize)
	}
	if !strings.HasSuffix(result.Stdout, truncatedSuffix) {
		t.Errorf("expected truncation suffix on over-cap output")
	}
	if emitter.outputTruncCount != 1 {
		t.Errorf("OutputTruncated fired %d times, want 1", emitter.outputTruncCount)
	}
	if emitter.lastOriginalSize != totalBytes {
		t.Errorf("OutputTruncated originalSize = %d, want true total %d (not the capped %d)",
			emitter.lastOriginalSize, totalBytes, maxOutputSize)
	}
}

// TestExec_OutputTruncated_CombinedAcrossStreams locks in the trigger
// alignment: stdout and stderr each under the 1 MB cap but together over it
// must fire OutputTruncated, matching the container path's combined-size
// trigger. Neither stream is individually truncated, so a per-stream trigger
// would miss this.
func TestExec_OutputTruncated_CombinedAcrossStreams(t *testing.T) {
	exec, _ := newTestExecutor(t)

	emitter := &capturingEmitter{}
	exec.Security = emitter

	// 600 KB to stdout + 600 KB to stderr = 1.2 MB combined, each under 1 MB.
	const perStream = 600 * 1024
	cmd := "head -c 614400 /dev/zero; head -c 614400 /dev/zero 1>&2"
	if _, err := exec.Exec(context.Background(), cmd, 30*time.Second); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if emitter.outputTruncCount != 1 {
		t.Fatalf("OutputTruncated fired %d times, want 1 (combined streams exceed cap)", emitter.outputTruncCount)
	}
	if want := 2 * perStream; emitter.lastOriginalSize != want {
		t.Errorf("OutputTruncated originalSize = %d, want combined total %d", emitter.lastOriginalSize, want)
	}
}

// retainedLen returns the number of payload bytes a cappedWriter currently
// holds, excluding any appended truncation suffix.
func retainedLen(w *cappedWriter) int {
	return len(strings.TrimSuffix(w.result(), truncatedSuffix))
}

func TestCappedWriter(t *testing.T) {
	t.Run("retains all bytes under the cap", func(t *testing.T) {
		w := newCappedWriter(100)
		n, err := w.Write([]byte("hello"))
		if err != nil || n != 5 {
			t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
		}
		if w.seen() != 5 {
			t.Errorf("seen = %d, want 5", w.seen())
		}
		if got := w.result(); got != "hello" {
			t.Errorf("result = %q, want %q", got, "hello")
		}
	})

	t.Run("exact-fit is not truncated", func(t *testing.T) {
		w := newCappedWriter(5)
		_, _ = w.Write([]byte("hello"))
		if w.seen() != 5 {
			t.Errorf("seen = %d, want 5", w.seen())
		}
		if got := w.result(); got != "hello" {
			t.Errorf("filling exactly to the cap must not append suffix: result = %q", got)
		}
	})

	t.Run("stops retaining past the cap but accepts and counts all bytes", func(t *testing.T) {
		w := newCappedWriter(4)
		// A single oversized write: the writer must report it fully consumed so
		// the producer never blocks, while retaining only the first limit bytes
		// and counting the true total.
		n, err := w.Write([]byte("abcdefghij"))
		if err != nil || n != 10 {
			t.Fatalf("Write = (%d, %v), want (10, nil)", n, err)
		}
		if w.seen() != 10 {
			t.Errorf("seen = %d, want true total 10", w.seen())
		}
		if rl := retainedLen(w); rl != 4 {
			t.Errorf("retained = %d, want cap 4", rl)
		}
		if got := w.result(); got != "abcd"+truncatedSuffix {
			t.Errorf("result = %q, want %q", got, "abcd"+truncatedSuffix)
		}
	})

	t.Run("drops bytes across multiple writes once full", func(t *testing.T) {
		w := newCappedWriter(4)
		_, _ = w.Write([]byte("ab"))
		_, _ = w.Write([]byte("cd"))
		// Now full; further writes are accepted, counted, but discarded.
		n, err := w.Write([]byte("efgh"))
		if err != nil || n != 4 {
			t.Fatalf("post-fill Write = (%d, %v), want (4, nil)", n, err)
		}
		if w.seen() != 8 {
			t.Errorf("seen = %d, want true total 8", w.seen())
		}
		if rl := retainedLen(w); rl != 4 {
			t.Errorf("retained = %d, want cap 4", rl)
		}
		if got := w.result(); got != "abcd"+truncatedSuffix {
			t.Errorf("result must append truncation suffix once full: got %q, want %q", got, "abcd"+truncatedSuffix)
		}
	})
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

func TestExec_FiltersSecretEnvironment(t *testing.T) {
	t.Setenv("STIRRUP_TEST_SECRET", "super-secret")
	exec, _ := newTestExecutor(t)

	result, err := exec.Exec(context.Background(), "printf %s \"$STIRRUP_TEST_SECRET\"", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "" {
		t.Fatalf("expected secret env to be filtered, got %q", result.Stdout)
	}
}

// TestNewLocalExecutorWithConfig_RejectsAllowlist confirms that the local
// executor cannot be constructed when the operator asks for an egress
// allowlist. The local executor has no sandbox boundary so it could not
// enforce the allowlist even if asked; the harness should refuse the
// configuration up front rather than run unenforced.
func TestNewLocalExecutorWithConfig_RejectsAllowlist(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	_, err = NewLocalExecutorWithConfig(LocalExecutorConfig{
		Workspace: resolved,
		Network:   &types.NetworkConfig{Mode: "allowlist", Allowlist: []string{"example.com"}},
	})
	if err == nil {
		t.Fatal("expected error for allowlist mode on local executor")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("error should mention allowlist, got: %v", err)
	}
	if !strings.Contains(err.Error(), "container executor") {
		t.Errorf("error should hint at container executor, got: %v", err)
	}
}

// TestNewLocalExecutorWithConfig_AllowsNoneAndNil confirms that the
// allowlist gate does not block other valid configurations.
func TestNewLocalExecutorWithConfig_AllowsNoneAndNil(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	if _, err := NewLocalExecutorWithConfig(LocalExecutorConfig{Workspace: resolved}); err != nil {
		t.Errorf("nil network: %v", err)
	}
	if _, err := NewLocalExecutorWithConfig(LocalExecutorConfig{
		Workspace: resolved,
		Network:   &types.NetworkConfig{Mode: "none"},
	}); err != nil {
		t.Errorf("mode=none: %v", err)
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

// TestMaxTimeout_Is30Minutes pins the literal cap value itself (issue
// #461 raised it from 5 to 30 minutes so a cold `bundle install` in a
// preRun hook has headroom). The other Capabilities() tests across
// local/container/k8s only compare caps.MaxTimeout against this same
// package-level maxTimeout constant, which would pass even if the raise
// were silently reverted; this test is the actual regression tripwire
// for the cap value.
func TestMaxTimeout_Is30Minutes(t *testing.T) {
	if maxTimeout != 30*time.Minute {
		t.Errorf("maxTimeout = %v, want 30m (issue #461 raised the shared executor cap from 5m)", maxTimeout)
	}
}
