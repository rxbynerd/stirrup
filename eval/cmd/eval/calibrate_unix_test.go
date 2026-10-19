//go:build unix

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCmdJudgeCalibrate_InterruptDuringLastJudgment_ExitsOneWithoutReport(t *testing.T) {
	endpoint := newJudgeEndpoint(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 24 {
			endpoint.srv.Config.Handler.ServeHTTP(w, r)
			return
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		http.Error(w, "interrupted", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	t.Setenv("CLI_JUDGE_KEY", "cli-secret")
	output := filepath.Join(t.TempDir(), "report.json")

	code, stdout, stderr := runCalibrate(t, "--golden", seedGoldenPath, "--judge-model", "m", "--judge-base-url", srv.URL,
		"--judge-api-key-ref", "secret://CLI_JUDGE_KEY", "--output", output)

	require.Equal(t, 1, code, "stderr:\n%s", stderr)
	require.Contains(t, stderr, "context canceled (after 23 judgments)")
	assert.Empty(t, stdout, "an interrupted calibration printed a report")
	assert.NoFileExists(t, output, "an interrupted calibration wrote the report")
}
