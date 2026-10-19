package executor

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func newTestExecutor(t *testing.T) (*LocalExecutor, string) {
	t.Helper()
	dir := t.TempDir()
	// On macOS, t.TempDir() returns /var/... but EvalSymlinks resolves to
	// /private/var/.... Resolve here so test assertions match.
	dir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err, "EvalSymlinks")
	exec, err := NewLocalExecutor(dir)
	require.NoError(t, err, "NewLocalExecutor")
	return exec, dir
}

func TestNewLocalExecutor_RejectsMissingAndFileWorkspaces(t *testing.T) {
	t.Run("nonexistent directory", func(t *testing.T) {
		_, err := NewLocalExecutor("/nonexistent/path/that/does/not/exist")
		require.Error(t, err)
	})

	t.Run("regular file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "file.txt")
		_ = os.WriteFile(f, []byte("x"), 0o644)
		_, err := NewLocalExecutor(f)
		require.Error(t, err)
	})
}

func TestResolvePath_Basic(t *testing.T) {
	exec, dir := newTestExecutor(t)

	resolved, err := exec.ResolvePath("foo.txt")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "foo.txt"), resolved)
}

func TestResolvePath_TraversalRejected(t *testing.T) {
	exec, _ := newTestExecutor(t)

	cases := []struct {
		name string
		path string
	}{
		{name: "parent directory", path: "../etc/passwd"},
		{name: "nested parent directory", path: "foo/../../etc/passwd"},
		{name: "repeated parent directories", path: "../../../tmp/evil"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := exec.ResolvePath(tc.path)
			require.Error(t, err)
		})
	}
}

func TestResolvePath_AbsoluteOutsideWorkspace(t *testing.T) {
	exec, _ := newTestExecutor(t)
	_, err := exec.ResolvePath("/etc/passwd")
	require.Error(t, err)
}

func TestResolvePath_SymlinkEscapesWorkspace(t *testing.T) {
	exec, dir := newTestExecutor(t)

	// Create a symlink inside the workspace that points to /tmp (outside).
	link := filepath.Join(dir, "escape_link")
	require.NoError(t, os.Symlink("/tmp", link))

	// Attempting to resolve a path through the symlink should fail because
	// the resolved target (/tmp/...) is outside the workspace.
	_, err := exec.ResolvePath("escape_link/somefile")
	require.ErrorContains(t, err, "escapes workspace")

	// The symlink target itself should also be rejected.
	_, err = exec.ResolvePath("escape_link")
	require.Error(t, err, "symlink target outside workspace")
}

func TestResolvePath_AbsoluteInsideWorkspace(t *testing.T) {
	exec, dir := newTestExecutor(t)

	// Create a file inside the workspace.
	testFile := filepath.Join(dir, "inner.txt")
	_ = os.WriteFile(testFile, []byte("hi"), 0o644)

	// An absolute path that falls inside the workspace should be accepted.
	resolved, err := exec.ResolvePath(testFile)
	require.NoError(t, err)
	require.Equal(t, testFile, resolved)
}

func TestReadFile_ReturnsContent(t *testing.T) {
	exec, dir := newTestExecutor(t)
	content := "hello world"
	_ = os.WriteFile(filepath.Join(dir, "test.txt"), []byte(content), 0o644)

	got, err := exec.ReadFile(context.Background(), "test.txt")
	require.NoError(t, err)
	require.Equal(t, content, got)
}

func TestReadFile_RejectsDirectory(t *testing.T) {
	exec, dir := newTestExecutor(t)
	_ = os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)

	_, err := exec.ReadFile(context.Background(), "subdir")
	require.Error(t, err)
}

func TestReadFile_TooLarge(t *testing.T) {
	exec, dir := newTestExecutor(t)
	// Create a file just over the limit.
	f, _ := os.Create(filepath.Join(dir, "big.bin"))
	_ = f.Truncate(maxFileSize + 1)
	_ = f.Close()

	_, err := exec.ReadFile(context.Background(), "big.bin")
	require.ErrorContains(t, err, "too large")
}

func TestWriteFile_CreatesParentDirectories(t *testing.T) {
	exec, dir := newTestExecutor(t)

	err := exec.WriteFile(context.Background(), "sub/dir/test.txt", "content")
	require.NoError(t, err)

	data, _ := os.ReadFile(filepath.Join(dir, "sub", "dir", "test.txt"))
	require.Equal(t, "content", string(data))
}

func TestWriteFile_TooLarge(t *testing.T) {
	exec, _ := newTestExecutor(t)
	bigContent := strings.Repeat("x", maxFileSize+1)
	err := exec.WriteFile(context.Background(), "big.txt", bigContent)
	require.Error(t, err)
}

func TestWriteFile_TraversalRejected(t *testing.T) {
	exec, _ := newTestExecutor(t)
	err := exec.WriteFile(context.Background(), "../escape.txt", "bad")
	require.Error(t, err)
}

func TestListDirectory_ListsFilesAndSlashSuffixedDirs(t *testing.T) {
	exec, dir := newTestExecutor(t)
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte(""), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)

	entries, err := exec.ListDirectory(context.Background(), ".")
	require.NoError(t, err)

	require.Contains(t, entries, "a.txt")
	require.Contains(t, entries, "subdir/", "directories should have a trailing slash")
}

func TestExec_ZeroExitCapturesStdout(t *testing.T) {
	exec, _ := newTestExecutor(t)

	result, err := exec.Exec(context.Background(), "echo hello", 0)
	require.NoError(t, err)
	require.Equal(t, 0, result.ExitCode)
	require.Equal(t, "hello", strings.TrimSpace(result.Stdout))
}

func TestExec_NonZeroExit(t *testing.T) {
	exec, _ := newTestExecutor(t)

	result, err := exec.Exec(context.Background(), "exit 42", 0)
	require.NoError(t, err)
	require.Equal(t, 42, result.ExitCode)
}

func TestExec_Timeout(t *testing.T) {
	exec, _ := newTestExecutor(t)

	_, err := exec.Exec(context.Background(), "sleep 60", 100*time.Millisecond)
	require.ErrorContains(t, err, "timed out")
	// A genuine deadline expiry must satisfy errors.Is against the shared
	// executor sentinel, not a match on the formatted text.
	require.ErrorIs(t, err, ErrTimeout)
}

// TestExec_CancelledContext_NotErrTimeout verifies that a context
// cancellation that is NOT a deadline expiry (e.g. a SIGTERM-driven
// parent-context cancel) must not be reported as a timeout, and must not
// satisfy errors.Is(err, ErrTimeout) — even though a large configured
// timeout (60s) is in play, only 200ms actually elapsed. Output already
// captured before the cancel must still be returned.
func TestExec_CancelledContext_NotErrTimeout(t *testing.T) {
	exec, _ := newTestExecutor(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	result, err := exec.Exec(ctx, "echo started; sleep 30", 60*time.Second)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrTimeout, "cancelled after 200ms, not deadline-expired after the configured 60s")
	require.ErrorIs(t, err, context.Canceled)
	require.NotContains(t, err.Error(), "timed out")
	require.NotNil(t, result)
	require.Contains(t, result.Stdout, "started", "output captured before the cancel should be preserved")
}

// TestExec_KillsOrphanedGrandchildPromptly verifies that a compound
// command ("cmd1; sleep N; cmd2") — whose later stages run as children the
// shell forks and waits on, not an exec-replaced process — does not block
// Exec on ctx cancellation: without cmd.WaitDelay, killing only the direct
// "sh" child leaves the still-running "sleep" grandchild holding the
// stdout pipe open. A single-command "sleep N" (see TestExec_Timeout) does
// not reproduce this: sh execs directly into it with no fork.
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

	require.Error(t, err)
	// Bounded well under the 30s grandchild sleep; generous enough to
	// be non-flaky (200ms cancel delay + shortCommandKillGrace + CI
	// scheduling slack), but nowhere near the un-fixed ~30s.
	require.LessOrEqual(t, elapsed, 5*time.Second, "orphaned grandchild must not block Exec after cancellation")
}

func TestExec_MaxTimeoutCapped(t *testing.T) {
	exec, _ := newTestExecutor(t)

	// Passing a timeout exceeding max should be capped, not rejected.
	result, err := exec.Exec(context.Background(), "echo ok", 10*time.Minute)
	require.NoError(t, err)
	require.Equal(t, "ok", strings.TrimSpace(result.Stdout))
}

func TestExec_OutputTruncation(t *testing.T) {
	exec, dir := newTestExecutor(t)

	// Create a file larger than maxOutputSize and cat it.
	bigFile := filepath.Join(dir, "big.txt")
	data := strings.Repeat("A", maxOutputSize+1000)
	_ = os.WriteFile(bigFile, []byte(data), 0o644)

	result, err := exec.Exec(context.Background(), "cat big.txt", 10*time.Second)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(result.Stdout, truncatedSuffix), "expected truncation suffix on large output")
	require.True(t, result.OutputTruncated, "expected ExecResult.OutputTruncated on capped output")
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
	require.NoError(t, err)
	payload := strings.TrimSuffix(result.Stdout, truncatedSuffix)
	require.Len(t, payload, maxOutputSize, "retained payload should be exactly the cap")
	require.True(t, strings.HasSuffix(result.Stdout, truncatedSuffix), "expected truncation suffix on over-cap output")
	require.Equal(t, 1, emitter.outputTruncCount, "OutputTruncated should fire once")
	require.Equal(t, totalBytes, emitter.lastOriginalSize, "OutputTruncated should report the true total, not the capped size")
}

func TestExecStream_PreservesOutputBeyondLegacyCap(t *testing.T) {
	exec, _ := newTestExecutor(t)
	var stdout, stderr strings.Builder
	result, err := exec.ExecStream(context.Background(), "yes x | head -c 2097152", 10*time.Second, &stdout, &stderr)
	require.NoError(t, err)
	require.Equal(t, 0, result.ExitCode)
	require.Equal(t, 2*1024*1024, stdout.Len())
	require.NotContains(t, stdout.String(), truncatedSuffix, "streaming output was legacy-truncated")
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
	_, err := exec.Exec(context.Background(), cmd, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, 1, emitter.outputTruncCount, "combined streams exceed the cap")
	require.Equal(t, 2*perStream, emitter.lastOriginalSize, "OutputTruncated should report the combined total")
}

// retainedLen returns the number of payload bytes a cappedWriter currently
// holds, excluding any appended truncation suffix.
func retainedLen(w *cappedWriter) int {
	return len(strings.TrimSuffix(w.result(), truncatedSuffix))
}

func TestCappedWriter_RetainsUpToCapAndCountsAll(t *testing.T) {
	t.Run("retains all bytes under the cap", func(t *testing.T) {
		w := newCappedWriter(100)
		n, err := w.Write([]byte("hello"))
		require.NoError(t, err)
		require.Equal(t, 5, n)
		require.Equal(t, 5, w.seen())
		require.Equal(t, "hello", w.result())
	})

	t.Run("exact-fit is not truncated", func(t *testing.T) {
		w := newCappedWriter(5)
		_, _ = w.Write([]byte("hello"))
		require.Equal(t, 5, w.seen())
		require.Equal(t, "hello", w.result(), "filling exactly to the cap must not append suffix")
	})

	t.Run("stops retaining past the cap but accepts and counts all bytes", func(t *testing.T) {
		w := newCappedWriter(4)
		// A single oversized write: the writer must report it fully consumed so
		// the producer never blocks, while retaining only the first limit bytes
		// and counting the true total.
		n, err := w.Write([]byte("abcdefghij"))
		require.NoError(t, err)
		require.Equal(t, 10, n)
		require.Equal(t, 10, w.seen())
		require.Equal(t, 4, retainedLen(w))
		require.Equal(t, "abcd"+truncatedSuffix, w.result())
	})

	t.Run("drops bytes across multiple writes once full", func(t *testing.T) {
		w := newCappedWriter(4)
		_, _ = w.Write([]byte("ab"))
		_, _ = w.Write([]byte("cd"))
		// Now full; further writes are accepted, counted, but discarded.
		n, err := w.Write([]byte("efgh"))
		require.NoError(t, err)
		require.Equal(t, 4, n)
		require.Equal(t, 8, w.seen())
		require.Equal(t, 4, retainedLen(w))
		require.Equal(t, "abcd"+truncatedSuffix, w.result(), "result must append truncation suffix once full")
	})
}

func TestExec_WorkingDirectoryIsWorkspace(t *testing.T) {
	exec, dir := newTestExecutor(t)

	result, err := exec.Exec(context.Background(), "pwd", 0)
	require.NoError(t, err)
	require.Equal(t, dir, strings.TrimSpace(result.Stdout))
}

func TestExec_FiltersSecretEnvironment(t *testing.T) {
	t.Setenv("STIRRUP_TEST_SECRET", "super-secret")
	exec, _ := newTestExecutor(t)

	result, err := exec.Exec(context.Background(), "printf %s \"$STIRRUP_TEST_SECRET\"", 0)
	require.NoError(t, err)
	require.Empty(t, result.Stdout, "secret environment variable should be filtered")
}

// TestNewLocalExecutorWithConfig_RejectsAllowlist confirms that the local
// executor cannot be constructed when the operator asks for an egress
// allowlist. The local executor has no sandbox boundary so it could not
// enforce the allowlist even if asked; the harness should refuse the
// configuration up front rather than run unenforced.
func TestNewLocalExecutorWithConfig_RejectsAllowlist(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err, "EvalSymlinks")

	_, err = NewLocalExecutorWithConfig(LocalExecutorConfig{
		Workspace: resolved,
		Network:   &types.NetworkConfig{Mode: "allowlist", Allowlist: []string{"example.com"}},
	})
	require.ErrorContains(t, err, "allowlist")
	require.ErrorContains(t, err, "container executor", "error should hint at the container executor")
}

// TestNewLocalExecutorWithConfig_AllowsNoneAndNil confirms that the
// allowlist gate does not block other valid configurations.
func TestNewLocalExecutorWithConfig_AllowsNoneAndNil(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err, "EvalSymlinks")

	t.Run("nil network", func(t *testing.T) {
		_, err := NewLocalExecutorWithConfig(LocalExecutorConfig{Workspace: resolved})
		require.NoError(t, err)
	})

	t.Run("network mode none", func(t *testing.T) {
		_, err := NewLocalExecutorWithConfig(LocalExecutorConfig{
			Workspace: resolved,
			Network:   &types.NetworkConfig{Mode: "none"},
		})
		require.NoError(t, err)
	})
}

func TestLocalExecutor_CapabilitiesEnableAll(t *testing.T) {
	exec, _ := newTestExecutor(t)
	caps := exec.Capabilities()

	require.True(t, caps.CanRead, "CanRead should be enabled")
	require.True(t, caps.CanWrite, "CanWrite should be enabled")
	require.True(t, caps.CanExec, "CanExec should be enabled")
	require.True(t, caps.CanNetwork, "CanNetwork should be enabled")
	require.Equal(t, maxTimeout, caps.MaxTimeout)
}

// TestMaxTimeout_Is30Minutes pins the literal cap value itself. The other
// Capabilities() tests across local/container/k8s only compare
// caps.MaxTimeout against this same package-level maxTimeout constant,
// which would pass even if the value were silently changed; this test is
// the actual regression tripwire for the cap value.
func TestMaxTimeout_Is30Minutes(t *testing.T) {
	require.Equal(t, 30*time.Minute, maxTimeout)
}

// processRunning reports whether pid is a live, non-zombie process.
func processRunning(t *testing.T, pid int) bool {
	t.Helper()
	out, err := osexec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	stat := strings.TrimSpace(string(out))
	return stat != "" && !strings.HasPrefix(stat, "Z")
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.FailNowf(t, "pid file was not written", "path: %s", path)
	return 0
}

func requirePS(t *testing.T) {
	t.Helper()
	if _, err := osexec.LookPath("ps"); err != nil {
		t.Skip("ps not available")
	}
}

func TestExec_TimeoutKillsBackgroundGrandchild(t *testing.T) {
	exec, dir := newTestExecutor(t)

	// The detached grandchild holds no pipe, so only a process-group kill
	// stops it from writing the marker after the command timed out.
	command := "sh -c 'sleep 2; touch after-timeout' >/dev/null 2>&1 & sleep 600"
	_, err := exec.Exec(context.Background(), command, 500*time.Millisecond)
	require.ErrorIs(t, err, ErrTimeout)

	time.Sleep(3 * time.Second)
	require.NoFileExists(t, filepath.Join(dir, "after-timeout"), "background grandchild survived the timeout and wrote its marker")
}

func TestExec_TimeoutKillsProcessTree(t *testing.T) {
	requirePS(t)
	exec, dir := newTestExecutor(t)

	pidFile := filepath.Join(dir, "sleeper.pid")
	command := "sleep 600 & echo $! > sleeper.pid; wait"
	go func() { _, _ = exec.Exec(context.Background(), command, 1500*time.Millisecond) }()

	pid := readPid(t, pidFile)
	require.True(t, processRunning(t, pid), "sleeper %d should be running before the timeout", pid)

	require.Eventually(t, func() bool {
		return !processRunning(t, pid)
	}, 10*time.Second, 50*time.Millisecond, "sleeper %d still running after the command timed out", pid)
}

func TestExec_CancelKillsTermIgnoringProcessTree(t *testing.T) {
	requirePS(t)
	exec, dir := newTestExecutor(t)

	pidFile := filepath.Join(dir, "stubborn.pid")
	command := "trap '' TERM; sleep 600 & echo $! > stubborn.pid; wait"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := exec.Exec(ctx, command, 60*time.Second)
		done <- err
	}()

	pid := readPid(t, pidFile)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err, "Exec should return an error after cancellation")
		require.NotErrorIs(t, err, ErrTimeout, "want a plain cancellation, got %v", err)
	case <-time.After(10 * time.Second):
		require.Fail(t, "Exec did not return after cancellation")
	}

	// Exec returns only after the group has been SIGKILLed.
	require.Eventually(t, func() bool {
		return !processRunning(t, pid)
	}, 2*time.Second, 50*time.Millisecond, "SIGTERM-ignoring process %d survived cancellation", pid)
}

func TestExec_NormalCompletionLeavesBackgroundProcessAndExitCode(t *testing.T) {
	exec, dir := newTestExecutor(t)

	result, err := exec.Exec(context.Background(), "(sleep 1; touch detached) >/dev/null 2>&1 & echo out; echo err >&2; exit 7", 10*time.Second)
	require.NoError(t, err)
	require.Equal(t, 7, result.ExitCode)
	require.Equal(t, "out\n", result.Stdout)
	require.Equal(t, "err\n", result.Stderr)

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(filepath.Join(dir, "detached"))
		return statErr == nil
	}, 5*time.Second, 50*time.Millisecond, "a command that finished normally must not kill the background processes it left behind")
}

type traversalCounter struct{ blocked int }

func (c *traversalCounter) PathTraversalBlocked(_, _ string)           { c.blocked++ }
func (c *traversalCounter) FileSizeLimitExceeded(_ string, _, _ int64) {}
func (c *traversalCounter) OutputTruncated(_ string, _, _ int)         {}

func TestResolvePathQuietly_RejectsEscapesWithoutSecurityEvent(t *testing.T) {
	exec, dir := newTestExecutor(t)
	counter := &traversalCounter{}
	exec.Security = counter
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(dir, "escape_link")))

	tests := []struct {
		name string
		path string
	}{
		{name: "symlink escape", path: "escape_link/somefile"},
		{name: "relative escape", path: "../outside"},
		{name: "absolute path outside workspace", path: "/etc/passwd"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := exec.ResolvePathQuietly(tc.path)
			require.ErrorContains(t, err, "escapes workspace")
		})
	}
	require.Zero(t, counter.blocked, "ResolvePathQuietly must not emit path traversal events")

	_, err := exec.ResolvePath("escape_link/somefile")
	require.Error(t, err, "ResolvePath must reject the escaping symlink")
	require.Equal(t, 1, counter.blocked, "ResolvePath should emit one path traversal event")
}
