package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

	if runErr != nil {
		require.NotContains(t, runErr.Error(), "finish trace", "runWithConfig should not fail on trace persistence")
	}
	result := decodeStirrupResult(t, stdout)
	assert.Equal(t, "cancelled", result.Outcome, "STIRRUP_RESULT outcome")

	_, err := os.Stat(filepath.Join(config.Executor.Workspace, "hook-finished"))
	require.NoError(t, err, "postRun hook should finish after the first signal")

	traceBytes, err := os.ReadFile(tracePath)
	require.NoError(t, err, "read trace file")
	assert.Contains(t, string(traceBytes), `"run_finished"`,
		"trace file should record run_finished; the trace emitter was closed before Finish")
}
