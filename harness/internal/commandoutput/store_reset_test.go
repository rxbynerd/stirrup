package commandoutput

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// captureOne spools one command's stdout under runID and completes it.
func captureOne(t *testing.T, store *Store, runID, toolUseID, stdout string) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	capture, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: runID, Turn: 1, ToolUseID: toolUseID}), cancel)
	if err != nil {
		t.Fatalf("Begin(%s): %v", runID, err)
	}
	if _, err := capture.Stdout().Write([]byte(stdout)); err != nil {
		t.Fatal(err)
	}
	if _, err := capture.Complete(Completion{ExitCode: 0}); err != nil {
		t.Fatalf("Complete(%s): %v", runID, err)
	}
}

// TestStoreResetGivesEachRunItsOwnArchive pins the per-run contract for
// a store reused across follow-ups: every run spools into a live root
// and finalises to its own archive, and the archive a run reports
// contains that run's output rather than the primary's.
func TestStoreResetGivesEachRunItsOwnArchive(t *testing.T) {
	dir := t.TempDir()
	store, err := New(Options{RunID: "primary", Config: testConfig(), ArchivePath: filepath.Join(dir, "primary.command-output.tar.gz")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	var archives []string
	for _, run := range []string{"primary", "run-2", "run-3"} {
		if run != "primary" {
			if err := store.Reset(run); err != nil {
				t.Fatalf("Reset(%s): %v", run, err)
			}
		}
		captureOne(t, store, run, "tool-"+run, "output of "+run)
		archive, err := store.Finalize(context.Background())
		if err != nil {
			t.Fatalf("Finalize(%s): %v", run, err)
		}
		if archive == "" {
			t.Fatalf("Finalize(%s) reported no archive despite a capture", run)
		}
		if _, err := os.Stat(archive); err != nil {
			t.Fatalf("archive for %s missing: %v", run, err)
		}
		archives = append(archives, archive)
	}

	if archives[0] == archives[1] || archives[1] == archives[2] || archives[0] == archives[2] {
		t.Fatalf("archives collide: %v", archives)
	}
	for i, run := range []string{"run-2", "run-3"} {
		if !strings.Contains(filepath.Base(archives[i+1]), run) {
			t.Errorf("archive %q does not carry its run ID %q", archives[i+1], run)
		}
		if filepath.Dir(archives[i+1]) != dir {
			t.Errorf("follow-up archive %q left the configured directory %q", archives[i+1], dir)
		}
	}

	// A second Finalize on the same run still returns that run's archive,
	// not a stale one.
	again, err := store.Finalize(context.Background())
	if err != nil || again != archives[2] {
		t.Errorf("repeat Finalize = %q, %v; want %q", again, err, archives[2])
	}
}

// TestStoreResetRefusesOpenCaptures pins that a store cannot be re-keyed
// while a command is still spooling into it.
func TestStoreResetRefusesOpenCaptures(t *testing.T) {
	store, err := New(Options{RunID: "primary", Config: testConfig(), ArchivePath: filepath.Join(t.TempDir(), "out.tar.gz")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	ctx, cancel := context.WithCancelCause(context.Background())
	if _, err := store.Begin(tool.WithCallContext(ctx, tool.CallContext{RunID: "primary", ToolUseID: "open"}), cancel); err != nil {
		t.Fatal(err)
	}
	if err := store.Reset("run-2"); err == nil {
		t.Fatal("Reset succeeded with a capture still open")
	}
}

// TestStoreResetBeforeUseIsNoop pins that resetting an unused store to
// its own run ID keeps the configured archive path.
func TestStoreResetBeforeUseIsNoop(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "configured.tar.gz")
	store, err := New(Options{RunID: "primary", Config: testConfig(), ArchivePath: archive})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	if err := store.Reset("primary"); err != nil {
		t.Fatal(err)
	}
	if got := store.Archive(); got != archive {
		t.Errorf("archive path after no-op reset = %q, want %q", got, archive)
	}
}
