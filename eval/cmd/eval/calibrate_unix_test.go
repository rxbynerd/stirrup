//go:build unix

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestCmdJudgeCalibrate_InterruptDuringTheLastJudgmentExits1(t *testing.T) {
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

	if code != 1 || !strings.Contains(stderr, "context canceled (after 23 judgments)") {
		t.Fatalf("exit %d, want 1 naming the 23 judgments made\nstderr:\n%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("an interrupted calibration printed a report:\n%s", stdout)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Errorf("an interrupted calibration wrote %s (stat: %v)", output, err)
	}
}
