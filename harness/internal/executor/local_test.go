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
	must := require.New(t)
	dir := t.TempDir()
	// On macOS, t.TempDir() returns /var/... but EvalSymlinks resolves to
	// /private/var/.... Resolve here so test assertions match.
	dir, err := filepath.EvalSymlinks(dir)
	must.NoError(err, "EvalSymlinks")
	exec, err := NewLocalExecutor(dir)
	must.NoError(err, "NewLocalExecutor")
	return exec, dir
}

func TestNewLocalExecutor_RejectsMissingAndFileWorkspaces(t *testing.T) {
	t.Run("nonexistent directory", func(t *testing.T) {
		must := require.New(t)
		_, err := NewLocalExecutor("/nonexistent/path/that/does/not/exist")
		must.Error(err)
	})

	t.Run("regular file", func(t *testing.T) {
		must := require.New(t)
		f := filepath.Join(t.TempDir(), "file.txt")
		_ = os.WriteFile(f, []byte("x"), 0o644)
		_, err := NewLocalExecutor(f)
		must.Error(err)
	})
}

func TestResolvePath_Basic(t *testing.T) {
	must := require.New(t)
	exec, dir := newTestExecutor(t)

	resolved, err := exec.ResolvePath("foo.txt")
	must.NoError(err)
	must.Equal(filepath.Join(dir, "foo.txt"), resolved)
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
			must := require.New(t)
			_, err := exec.ResolvePath(tc.path)
			must.Error(err)
		})
	}
}

func TestResolvePath_AbsoluteOutsideWorkspace(t *testing.T) {
	must := require.New(t)
	exec, _ := newTestExecutor(t)
	_, err := exec.ResolvePath("/etc/passwd")
	must.Error(err)
}

func TestResolvePath_SymlinkEscapesWorkspace(t *testing.T) {
	must := require.New(t)
	exec, dir := newTestExecutor(t)

	// Create a symlink inside the workspace that points to /tmp (outside).
	link := filepath.Join(dir, "escape_link")
	must.NoError(os.Symlink("/tmp", link))

	// Attempting to resolve a path through the symlink should fail because
	// the resolved target (/tmp/...) is outside the workspace.
	_, err := exec.ResolvePath("escape_link/somefile")
	must.ErrorContains(err, "escapes workspace")

	// The symlink target itself should also be rejected.
	_, err = exec.ResolvePath("escape_link")
	must.Error(err, "symlink target outside workspace")
}

func TestResolvePath_AbsoluteInsideWorkspace(t *testing.T) {
	must := require.New(t)
	exec, dir := newTestExecutor(t)

	// Create a file inside the workspace.
	testFile := filepath.Join(dir, "inner.txt")
	_ = os.WriteFile(testFile, []byte("hi"), 0o644)

	// An absolute path that falls inside the workspace should be accepted.
	resolved, err := exec.ResolvePath(testFile)
	must.NoError(err)
	must.Equal(testFile, resolved)
}

func TestReadFile_ReturnsContent(t *testing.T) {
	must := require.New(t)
	exec, dir := newTestExecutor(t)
	content := "hello world"
	_ = os.WriteFile(filepath.Join(dir, "test.txt"), []byte(content), 0o644)

	got, err := exec.ReadFile(context.Background(), "test.txt")
	must.NoError(err)
	must.Equal(content, got)
}

func TestReadFile_RejectsDirectory(t *testing.T) {
	must := require.New(t)
	exec, dir := newTestExecutor(t)
	_ = os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)

	_, err := exec.ReadFile(context.Background(), "subdir")
	must.Error(err)
}

func TestReadFile_TooLarge(t *testing.T) {
	must := require.New(t)
	exec, dir := newTestExecutor(t)
	// Create a file just over the limit.
	f, _ := os.Create(filepath.Join(dir, "big.bin"))
	_ = f.Truncate(maxFileSize + 1)
	_ = f.Close()

	_, err := exec.ReadFile(context.Background(), "big.bin")
	must.ErrorContains(err, "too large")
}

func TestWriteFile_CreatesParentDirectories(t *testing.T) {
	must := require.New(t)
	exec, dir := newTestExecutor(t)

	err := exec.WriteFile(context.Background(), "sub/dir/test.txt", "content")
	must.NoError(err)

	data, _ := os.ReadFile(filepath.Join(dir, "sub", "dir", "test.txt"))
	must.Equal("content", string(data))
}

func TestWriteFile_TooLarge(t *testing.T) {
	must := require.New(t)
	exec, _ := newTestExecutor(t)
	bigContent := strings.Repeat("x", maxFileSize+1)
	err := exec.WriteFile(context.Background(), "big.txt", bigContent)
	must.Error(err)
}

func TestWriteFile_TraversalRejected(t *testing.T) {
	must := require.New(t)
	exec, _ := newTestExecutor(t)
	err := exec.WriteFile(context.Background(), "../escape.txt", "bad")
	must.Error(err)
}

func TestListDirectory_ListsFilesAndSlashSuffixedDirs(t *testing.T) {
	must := require.New(t)
	exec, dir := newTestExecutor(t)
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte(""), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)

	entries, err := exec.ListDirectory(context.Background(), ".")
	must.NoError(err)

	must.Contains(entries, "a.txt")
	must.Contains(entries, "subdir/", "directories should have a trailing slash")
}

func TestExec_ZeroExitCapturesStdout(t *testing.T) {
	must := require.New(t)
	exec, _ := newTestExecutor(t)

	result, err := exec.Exec(context.Background(), "echo hello", 0)
	must.NoError(err)
	must.Equal(0, result.ExitCode)
	must.Equal("hello", strings.TrimSpace(result.Stdout))
}

func TestExec_NonZeroExit(t *testing.T) {
	must := require.New(t)
	exec, _ := newTestExecutor(t)

	result, err := exec.Exec(context.Background(), "exit 42", 0)
	must.NoError(err)
	must.Equal(42, result.ExitCode)
}

func TestExec_Timeout(t *testing.T) {
	must := require.New(t)
	exec, _ := newTestExecutor(t)

	_, err := exec.Exec(context.Background(), "sleep 60", 100*time.Millisecond)
	must.ErrorContains(err, "timed out")
	// A genuine deadline expiry must satisfy errors.Is against the shared
	// executor sentinel, not a match on the formatted text.
	must.ErrorIs(err, ErrTimeout)
}

// TestExec_CancelledContext_NotErrTimeout verifies that a context
// cancellation that is NOT a deadline expiry (e.g. a SIGTERM-driven
// parent-context cancel) must not be reported as a timeout, and must not
// satisfy errors.Is(err, ErrTimeout) — even though a large configured
// timeout (60s) is in play, only 200ms actually elapsed. Output already
// captured before the cancel must still be returned.
func TestExec_CancelledContext_NotErrTimeout(t *testing.T) {
	must := require.New(t)
	exec, _ := newTestExecutor(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	result, err := exec.Exec(ctx, "echo started; sleep 30", 60*time.Second)
	must.Error(err)
	must.NotErrorIs(err, ErrTimeout, "cancelled after 200ms, not deadline-expired after the configured 60s")
	must.ErrorIs(err, context.Canceled)
	must.NotContains(err.Error(), "timed out")
	must.NotNil(result)
	must.Contains(result.Stdout, "started", "output captured before the cancel should be preserved")
}

// TestExec_KillsOrphanedGrandchildPromptly verifies that a compound
// command ("cmd1; sleep N; cmd2") — whose later stages run as children the
// shell forks and waits on, not an exec-replaced process — does not block
// Exec on ctx cancellation: without cmd.WaitDelay, killing only the direct
// "sh" child leaves the still-running "sleep" grandchild holding the
// stdout pipe open. A single-command "sleep N" (see TestExec_Timeout) does
// not reproduce this: sh execs directly into it with no fork.
func TestExec_KillsOrphanedGrandchildPromptly(t *testing.T) {
	must := require.New(t)
	exec, _ := newTestExecutor(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := exec.Exec(ctx, "echo start; sleep 30; echo finished", 60*time.Second)
	elapsed := time.Since(start)

	must.Error(err)
	// Bounded well under the 30s grandchild sleep; generous enough to
	// be non-flaky (200ms cancel delay + shortCommandKillGrace + CI
	// scheduling slack), but nowhere near the un-fixed ~30s.
	must.LessOrEqual(elapsed, 5*time.Second, "orphaned grandchild must not block Exec after cancellation")
}

func TestExec_MaxTimeoutCapped(t *testing.T) {
	must := require.New(t)
	exec, _ := newTestExecutor(t)

	// Passing a timeout exceeding max should be capped, not rejected.
	result, err := exec.Exec(context.Background(), "echo ok", 10*time.Minute)
	must.NoError(err)
	must.Equal("ok", strings.TrimSpace(result.Stdout))
}

func TestExec_OutputTruncation(t *testing.T) {
	must := require.New(t)
	exec, dir := newTestExecutor(t)

	// Create a file larger than maxOutputSize and cat it.
	bigFile := filepath.Join(dir, "big.txt")
	data := strings.Repeat("A", maxOutputSize+1000)
	_ = os.WriteFile(bigFile, []byte(data), 0o644)

	result, err := exec.Exec(context.Background(), "cat big.txt", 10*time.Second)
	must.NoError(err)
	must.True(strings.HasSuffix(result.Stdout, truncatedSuffix), "expected truncation suffix on large output")
	must.True(result.OutputTruncated, "expected ExecResult.OutputTruncated on capped output")
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
	must := require.New(t)
	exec, _ := newTestExecutor(t)

	emitter := &capturingEmitter{}
	exec.Security = emitter

	// 16 MB of stdout, well past the 1 MB cap.
	const totalBytes = 16777216
	result, err := exec.Exec(context.Background(), "head -c 16777216 /dev/zero", 30*time.Second)
	must.NoError(err)
	payload := strings.TrimSuffix(result.Stdout, truncatedSuffix)
	must.Len(payload, maxOutputSize, "retained payload should be exactly the cap")
	must.True(strings.HasSuffix(result.Stdout, truncatedSuffix), "expected truncation suffix on over-cap output")
	must.Equal(1, emitter.outputTruncCount, "OutputTruncated should fire once")
	must.Equal(totalBytes, emitter.lastOriginalSize, "OutputTruncated should report the true total, not the capped size")
}

func TestExecStream_PreservesOutputBeyondLegacyCap(t *testing.T) {
	must := require.New(t)
	exec, _ := newTestExecutor(t)
	var stdout, stderr strings.Builder
	result, err := exec.ExecStream(context.Background(), "yes x | head -c 2097152", 10*time.Second, &stdout, &stderr)
	must.NoError(err)
	must.Equal(0, result.ExitCode)
	must.Equal(2*1024*1024, stdout.Len())
	must.NotContains(stdout.String(), truncatedSuffix, "streaming output was legacy-truncated")
}

// TestExec_OutputTruncated_CombinedAcrossStreams locks in the trigger
// alignment: stdout and stderr each under the 1 MB cap but together over it
// must fire OutputTruncated, matching the container path's combined-size
// trigger. Neither stream is individually truncated, so a per-stream trigger
// would miss this.
func TestExec_OutputTruncated_CombinedAcrossStreams(t *testing.T) {
	must := require.New(t)
	exec, _ := newTestExecutor(t)

	emitter := &capturingEmitter{}
	exec.Security = emitter

	// 600 KB to stdout + 600 KB to stderr = 1.2 MB combined, each under 1 MB.
	const perStream = 600 * 1024
	cmd := "head -c 614400 /dev/zero; head -c 614400 /dev/zero 1>&2"
	_, err := exec.Exec(context.Background(), cmd, 30*time.Second)
	must.NoError(err)
	must.Equal(1, emitter.outputTruncCount, "combined streams exceed the cap")
	must.Equal(2*perStream, emitter.lastOriginalSize, "OutputTruncated should report the combined total")
}

// retainedLen returns the number of payload bytes a cappedWriter currently
// holds, excluding any appended truncation suffix.
func retainedLen(w *cappedWriter) int {
	return len(strings.TrimSuffix(w.result(), truncatedSuffix))
}

func TestCappedWriter_RetainsUpToCapAndCountsAll(t *testing.T) {
	t.Run("retains all bytes under the cap", func(t *testing.T) {
		must := require.New(t)
		w := newCappedWriter(100)
		n, err := w.Write([]byte("hello"))
		must.NoError(err)
		must.Equal(5, n)
		must.Equal(5, w.seen())
		must.Equal("hello", w.result())
	})

	t.Run("exact-fit is not truncated", func(t *testing.T) {
		must := require.New(t)
		w := newCappedWriter(5)
		_, _ = w.Write([]byte("hello"))
		must.Equal(5, w.seen())
		must.Equal("hello", w.result(), "filling exactly to the cap must not append suffix")
	})

	t.Run("stops retaining past the cap but accepts and counts all bytes", func(t *testing.T) {
		must := require.New(t)
		w := newCappedWriter(4)
		// A single oversized write: the writer must report it fully consumed so
		// the producer never blocks, while retaining only the first limit bytes
		// and counting the true total.
		n, err := w.Write([]byte("abcdefghij"))
		must.NoError(err)
		must.Equal(10, n)
		must.Equal(10, w.seen())
		must.Equal(4, retainedLen(w))
		must.Equal("abcd"+truncatedSuffix, w.result())
	})

	t.Run("drops bytes across multiple writes once full", func(t *testing.T) {
		must := require.New(t)
		w := newCappedWriter(4)
		_, _ = w.Write([]byte("ab"))
		_, _ = w.Write([]byte("cd"))
		// Now full; further writes are accepted, counted, but discarded.
		n, err := w.Write([]byte("efgh"))
		must.NoError(err)
		must.Equal(4, n)
		must.Equal(8, w.seen())
		must.Equal(4, retainedLen(w))
		must.Equal("abcd"+truncatedSuffix, w.result(), "result must append truncation suffix once full")
	})
}

func TestExec_WorkingDirectoryIsWorkspace(t *testing.T) {
	must := require.New(t)
	exec, dir := newTestExecutor(t)

	result, err := exec.Exec(context.Background(), "pwd", 0)
	must.NoError(err)
	must.Equal(dir, strings.TrimSpace(result.Stdout))
}

func TestExec_FiltersSecretEnvironment(t *testing.T) {
	must := require.New(t)
	t.Setenv("STIRRUP_TEST_SECRET", "super-secret")
	exec, _ := newTestExecutor(t)

	result, err := exec.Exec(context.Background(), "printf %s \"$STIRRUP_TEST_SECRET\"", 0)
	must.NoError(err)
	must.Empty(result.Stdout, "secret environment variable should be filtered")
}

// TestNewLocalExecutorWithConfig_RejectsAllowlist confirms that the local
// executor cannot be constructed when the operator asks for an egress
// allowlist. The local executor has no sandbox boundary so it could not
// enforce the allowlist even if asked; the harness should refuse the
// configuration up front rather than run unenforced.
func TestNewLocalExecutorWithConfig_RejectsAllowlist(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	must.NoError(err, "EvalSymlinks")

	_, err = NewLocalExecutorWithConfig(LocalExecutorConfig{
		Workspace: resolved,
		Network:   &types.NetworkConfig{Mode: "allowlist", Allowlist: []string{"example.com"}},
	})
	must.ErrorContains(err, "allowlist")
	must.ErrorContains(err, "container executor", "error should hint at the container executor")
}

// TestNewLocalExecutorWithConfig_AllowsNoneAndNil confirms that the
// allowlist gate does not block other valid configurations.
func TestNewLocalExecutorWithConfig_AllowsNoneAndNil(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	must.NoError(err, "EvalSymlinks")

	t.Run("nil network", func(t *testing.T) {
		must := require.New(t)
		_, err := NewLocalExecutorWithConfig(LocalExecutorConfig{Workspace: resolved})
		must.NoError(err)
	})

	t.Run("network mode none", func(t *testing.T) {
		must := require.New(t)
		_, err := NewLocalExecutorWithConfig(LocalExecutorConfig{
			Workspace: resolved,
			Network:   &types.NetworkConfig{Mode: "none"},
		})
		must.NoError(err)
	})
}

func TestLocalExecutor_CapabilitiesEnableAll(t *testing.T) {
	must := require.New(t)
	exec, _ := newTestExecutor(t)
	caps := exec.Capabilities()

	must.True(caps.CanRead, "CanRead should be enabled")
	must.True(caps.CanWrite, "CanWrite should be enabled")
	must.True(caps.CanExec, "CanExec should be enabled")
	must.True(caps.CanNetwork, "CanNetwork should be enabled")
	must.Equal(maxTimeout, caps.MaxTimeout)
}

// TestMaxTimeout_Is30Minutes pins the literal cap value itself. The other
// Capabilities() tests across local/container/k8s only compare
// caps.MaxTimeout against this same package-level maxTimeout constant,
// which would pass even if the value were silently changed; this test is
// the actual regression tripwire for the cap value.
func TestMaxTimeout_Is30Minutes(t *testing.T) {
	must := require.New(t)
	must.Equal(30*time.Minute, maxTimeout)
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
	must := require.New(t)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	must.FailNowf("pid file was not written", "path: %s", path)
	return 0
}

func requirePS(t *testing.T) {
	t.Helper()
	if _, err := osexec.LookPath("ps"); err != nil {
		t.Skip("ps not available")
	}
}

func TestExec_TimeoutKillsBackgroundGrandchild(t *testing.T) {
	must := require.New(t)
	exec, dir := newTestExecutor(t)

	// The detached grandchild holds no pipe, so only a process-group kill
	// stops it from writing the marker after the command timed out.
	command := "sh -c 'sleep 2; touch after-timeout' >/dev/null 2>&1 & sleep 600"
	_, err := exec.Exec(context.Background(), command, 500*time.Millisecond)
	must.ErrorIs(err, ErrTimeout)

	time.Sleep(3 * time.Second)
	must.NoFileExists(filepath.Join(dir, "after-timeout"), "background grandchild survived the timeout and wrote its marker")
}

func TestExec_TimeoutKillsProcessTree(t *testing.T) {
	must := require.New(t)
	requirePS(t)
	exec, dir := newTestExecutor(t)

	pidFile := filepath.Join(dir, "sleeper.pid")
	command := "sleep 600 & echo $! > sleeper.pid; wait"
	go func() { _, _ = exec.Exec(context.Background(), command, 1500*time.Millisecond) }()

	pid := readPid(t, pidFile)
	must.True(processRunning(t, pid), "sleeper %d should be running before the timeout", pid)

	must.Eventually(func() bool {
		return !processRunning(t, pid)
	}, 10*time.Second, 50*time.Millisecond, "sleeper %d still running after the command timed out", pid)
}

func TestExec_CancelKillsTermIgnoringProcessTree(t *testing.T) {
	must := require.New(t)
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
		must.Error(err, "Exec should return an error after cancellation")
		must.NotErrorIs(err, ErrTimeout, "want a plain cancellation, got %v", err)
	case <-time.After(10 * time.Second):
		must.Fail("Exec did not return after cancellation")
	}

	// Exec returns only after the group has been SIGKILLed.
	must.Eventually(func() bool {
		return !processRunning(t, pid)
	}, 2*time.Second, 50*time.Millisecond, "SIGTERM-ignoring process %d survived cancellation", pid)
}

func TestExec_NormalCompletionLeavesBackgroundProcessAndExitCode(t *testing.T) {
	must := require.New(t)
	exec, dir := newTestExecutor(t)

	result, err := exec.Exec(context.Background(), "(sleep 1; touch detached) >/dev/null 2>&1 & echo out; echo err >&2; exit 7", 10*time.Second)
	must.NoError(err)
	must.Equal(7, result.ExitCode)
	must.Equal("out\n", result.Stdout)
	must.Equal("err\n", result.Stderr)

	must.Eventually(func() bool {
		_, statErr := os.Stat(filepath.Join(dir, "detached"))
		return statErr == nil
	}, 5*time.Second, 50*time.Millisecond, "a command that finished normally must not kill the background processes it left behind")
}

type traversalCounter struct{ blocked int }

func (c *traversalCounter) PathTraversalBlocked(_, _ string)           { c.blocked++ }
func (c *traversalCounter) FileSizeLimitExceeded(_ string, _, _ int64) {}
func (c *traversalCounter) OutputTruncated(_ string, _, _ int)         {}

func TestResolvePathQuietly_RejectsEscapesWithoutSecurityEvent(t *testing.T) {
	must := require.New(t)
	exec, dir := newTestExecutor(t)
	counter := &traversalCounter{}
	exec.Security = counter
	must.NoError(os.Symlink(t.TempDir(), filepath.Join(dir, "escape_link")))

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
			must := require.New(t)
			_, err := exec.ResolvePathQuietly(tc.path)
			must.ErrorContains(err, "escapes workspace")
		})
	}
	must.Zero(counter.blocked, "ResolvePathQuietly must not emit path traversal events")

	_, err := exec.ResolvePath("escape_link/somefile")
	must.Error(err, "ResolvePath must reject the escaping symlink")
	must.Equal(1, counter.blocked, "ResolvePath should emit one path traversal event")
}
