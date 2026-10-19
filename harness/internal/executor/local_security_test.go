package executor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/security"
)

// TestLocalExecutor_FileSizeLimitExceeded_EmitsSecurityEvent writes a file
// larger than the executor's 10 MB limit and confirms that ReadFile both
// returns an error and emits a file_size_limit_exceeded security event
// into the SecurityLogger's sink.
func TestLocalExecutor_FileSizeLimitExceeded_EmitsSecurityEvent(t *testing.T) {
	exec, dir := newTestExecutor(t)

	// Write a sparse file just over the 10 MB limit so the test is fast
	// and produces no real I/O bytes. os.Truncate creates a sparse file
	// that os.Stat reports at the requested size.
	path := filepath.Join(dir, "big.bin")
	f, err := os.Create(path)
	require.NoError(t, err, "create big file")
	err = f.Truncate(maxFileSize + 1)
	_ = f.Close()
	require.NoError(t, err, "truncate big file")

	var secBuf bytes.Buffer
	exec.Security = security.NewSecurityLogger(&secBuf, "exec-test-1")

	_, readErr := exec.ReadFile(context.Background(), "big.bin")
	require.Error(t, readErr, "expected ReadFile to fail for oversize file")

	require.Contains(t, secBuf.String(), `"event":"file_size_limit_exceeded"`,
		"expected file_size_limit_exceeded event in security log")
}

// TestLocalExecutor_OutputTruncated_EmitsSecurityEvent runs a command that
// produces output exceeding the 1 MB cap and confirms that Exec emits an
// output_truncated security event.
func TestLocalExecutor_OutputTruncated_EmitsSecurityEvent(t *testing.T) {
	exec, _ := newTestExecutor(t)

	var secBuf bytes.Buffer
	exec.Security = security.NewSecurityLogger(&secBuf, "exec-test-2")

	// Generate >1 MB of stdout deterministically. `head -c` reading from
	// /dev/zero is fast and portable across BSD/GNU.
	const cmd = "head -c 1100000 /dev/zero"
	result, err := exec.Exec(context.Background(), cmd, 30*time.Second)
	require.NoError(t, err)
	require.NotNil(t, result, "expected non-nil ExecResult")

	require.Contains(t, secBuf.String(), `"event":"output_truncated"`,
		"expected output_truncated event in security log")
}
