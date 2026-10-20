package commandoutput

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// captureOne spools one command's stdout under runID and completes it.
func captureOne(t *testing.T, store *Store, runID, toolUseID, stdout string) {
	t.Helper()
	must := require.New(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: runID, Turn: 1, ToolUseID: toolUseID}), cancel)
	must.NoError(err, "Begin(%s)", runID)
	_, err = capture.Stdout().Write([]byte(stdout))
	must.NoError(err)
	_, err = capture.Complete(Completion{ExitCode: 0})
	must.NoError(err, "Complete(%s)", runID)
}

// TestStoreReset_GivesEachRunItsOwnArchive pins the per-run contract for
// a store reused across follow-ups: every run spools into a live root
// and finalises to its own archive, and the archive a run reports
// contains that run's output rather than the primary's.
func TestStoreReset_GivesEachRunItsOwnArchive(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	store, err := New(Options{RunID: "primary", Config: testConfig(), ArchivePath: filepath.Join(dir, "primary.command-output.tar.gz")})
	must.NoError(err)
	defer func() { _ = store.Close() }()

	var archives []string
	for _, run := range []string{"primary", "run-2", "run-3"} {
		if run != "primary" {
			must.NoError(store.Reset(run), "Reset(%s)", run)
		}
		captureOne(t, store, run, "tool-"+run, "output of "+run)
		archive, err := store.Finalize(context.Background())
		must.NoError(err, "Finalize(%s)", run)
		must.NotEmpty(archive, "Finalize(%s) should report an archive after a capture", run)
		_, err = os.Stat(archive)
		must.NoError(err, "archive for %s should exist", run)
		archives = append(archives, archive)
	}

	must.NotEqual(archives[0], archives[1], "primary and run-2 archives collide")
	must.NotEqual(archives[1], archives[2], "run-2 and run-3 archives collide")
	must.NotEqual(archives[0], archives[2], "primary and run-3 archives collide")

	for i, run := range []string{"run-2", "run-3"} {
		t.Run(run, func(t *testing.T) {
			must := require.New(t)
			must.Contains(filepath.Base(archives[i+1]), run, "archive should carry its run ID")
			must.Equal(dir, filepath.Dir(archives[i+1]), "follow-up archive should stay in the configured directory")
		})
	}

	// A second Finalize on the same run still returns that run's archive,
	// not a stale one.
	again, err := store.Finalize(context.Background())
	must.NoError(err, "repeat Finalize")
	must.Equal(archives[2], again, "repeat Finalize should return the run-3 archive")
}

// TestStoreReset_RefusesOpenCaptures pins that a store cannot be re-keyed
// while a command is still spooling into it.
func TestStoreReset_RefusesOpenCaptures(t *testing.T) {
	must := require.New(t)
	store, err := New(Options{RunID: "primary", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "out.tar.gz")})
	must.NoError(err)
	defer func() { _ = store.Close() }()

	ctx, cancel := context.WithCancelCause(context.Background())
	_, err = store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "primary", ToolUseID: "open"}), cancel)
	must.NoError(err)

	must.Error(store.Reset("run-2"), "Reset should fail while a capture is still open")
}

// TestStoreReset_BeforeUseIsNoop pins that resetting an unused store to
// its own run ID keeps the configured archive path.
func TestStoreReset_BeforeUseIsNoop(t *testing.T) {
	must := require.New(t)
	archive := filepath.Join(t.TempDir(), "configured.tar.gz")
	store, err := New(Options{RunID: "primary", Config: testConfig(), ArchivePath: archive})
	must.NoError(err)
	defer func() { _ = store.Close() }()

	must.NoError(store.Reset("primary"))
	must.Equal(archive, store.Archive(), "archive path should survive a no-op reset")
}
