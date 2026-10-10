package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// TestRunWithConfig_FirstSignalKeepsPostRunWorkPastCloseGrace delivers
// the first signal mid-turn while a postRun hook outlasts
// shutdownCloseGrace. The hook must finish and the JSONL trace must
// record run_finished: the loop's executor and trace emitter stay open
// until Run returns, because only a second signal arms the watchdog.
func TestRunWithConfig_FirstSignalKeepsPostRunWorkPastCloseGrace(t *testing.T) {
	interrupts := make(chan func(), 1)
	prevHandler, prevGrace := setupSignalHandler, shutdownCloseGrace
	setupSignalHandler = func(interrupt, _ func()) { interrupts <- interrupt }
	shutdownCloseGrace = 10 * time.Millisecond
	t.Cleanup(func() { setupSignalHandler, shutdownCloseGrace = prevHandler, prevGrace })

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// The server notices the client hanging up only once the body
		// has been read.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case interrupt := <-interrupts:
			interrupt()
		default:
		}
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer srv.Close()

	config := runOutcomeTestConfig(t, srv.URL)
	tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
	config.TraceEmitter = types.TraceEmitterConfig{Type: "jsonl", FilePath: tracePath}
	config.Hooks = &types.HooksConfig{
		PostRun: []types.HookConfig{{Name: "slow", Command: "sleep 0.5 && touch hook-finished"}},
	}

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	if runErr != nil && strings.Contains(runErr.Error(), "finish trace") {
		t.Fatalf("runWithConfig error = %v, want no trace persistence failure", runErr)
	}
	if result := decodeStirrupResult(t, stdout); result.Outcome != "cancelled" {
		t.Errorf("STIRRUP_RESULT outcome = %q, want cancelled", result.Outcome)
	}
	if _, err := os.Stat(filepath.Join(config.Executor.Workspace, "hook-finished")); err != nil {
		t.Errorf("postRun hook did not finish after the first signal: %v", err)
	}
	traceBytes, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if !strings.Contains(string(traceBytes), `"run_finished"`) {
		t.Errorf("trace file has no run_finished event; the trace emitter was closed before Finish:\n%s", traceBytes)
	}
}
