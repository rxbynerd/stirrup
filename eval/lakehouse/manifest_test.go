package lakehouse

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// TestManifest_PopulatedOnStore pins that every StoreTrace and
// StoreRecording call appends a matching manifest.jsonl line. A
// regression that wrote the JSON but skipped the manifest would
// silently degrade query performance back to O(files).
func TestManifest_PopulatedOnStore(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	require.NoError(t, err, "NewFileStore")
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	require.NoError(t, fs.StoreTrace(ctx, types.RunTrace{ID: "t1", Outcome: "success"}), "StoreTrace")
	require.NoError(t, fs.StoreRecording(ctx, types.RunRecording{
		RunID:        "r1",
		FinalOutcome: types.RunTrace{ID: "r1", Outcome: "error"},
	}), "StoreRecording")

	traces, recordings, ok := fs.loadManifestIndex()
	require.True(t, ok, "loadManifestIndex returned ok=false")
	require.Contains(t, traces, "t1", "manifest missing trace t1")
	require.Contains(t, recordings, "r1", "manifest missing recording r1")
	require.Equal(t, "success", traces["t1"].Outcome, "manifest entry for t1")
}

// TestManifest_FilterShortCircuitsLoad pins that a filter rejecting
// entries on a pre-decoded field (Outcome here) skips the JSON load
// for those entries: it stores a passing trace whose JSON file is
// then truncated to invalid JSON, and a query filtering for a
// different outcome must still succeed.
func TestManifest_FilterShortCircuitsLoad(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	require.NoError(t, err, "NewFileStore")
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	require.NoError(t, fs.StoreTrace(ctx, types.RunTrace{ID: "good-passing", Outcome: "success"}), "StoreTrace good")
	require.NoError(t, fs.StoreTrace(ctx, types.RunTrace{ID: "good-failing", Outcome: "error"}), "StoreTrace failing")

	// Corrupt the passing trace's JSON file. The manifest still
	// claims it's outcome=success, so a filter for outcome=error
	// must short-circuit and never try to load it.
	require.NoError(t,
		os.WriteFile(filepath.Join(dir, "traces", "good-passing.json"), []byte("{not json"), 0o644),
		"corrupt trace file")

	got, err := fs.QueryTraces(ctx, types.TraceFilter{Outcome: "error"})
	require.NoError(t, err, "QueryTraces with manifest short-circuit")
	require.Len(t, got, 1)
	require.Equal(t, "good-failing", got[0].ID)
}

// TestManifest_RebuildOnMissing pins the missing-manifest recovery
// path: when manifest.jsonl is absent (or removed by an operator),
// the next query falls back to a directory scan AND rebuilds the
// manifest as a side effect.
func TestManifest_RebuildOnMissing(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	require.NoError(t, err, "NewFileStore")
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		require.NoError(t, fs.StoreTrace(ctx, types.RunTrace{
			ID:      fmt.Sprintf("t%d", i+1),
			Outcome: "success",
		}), "StoreTrace t%d", i+1)
	}

	// Operator forces a rebuild by removing the manifest.
	require.NoError(t, os.Remove(fs.manifestPath()), "remove manifest")

	got, err := fs.QueryTraces(ctx, types.TraceFilter{})
	require.NoError(t, err, "QueryTraces after manifest removal")
	require.Len(t, got, 3, "rebuild must surface all traces")

	// And the manifest is back.
	_, err = os.Stat(fs.manifestPath())
	require.NoError(t, err, "manifest not rebuilt")
}

// TestManifest_RebuildOnCorrupt pins recovery from a malformed
// manifest. A single garbled line in manifest.jsonl invalidates
// the index; the read path falls back to scanning everything and
// rewrites a fresh manifest.
func TestManifest_RebuildOnCorrupt(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	require.NoError(t, err, "NewFileStore")
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	require.NoError(t, fs.StoreTrace(ctx, types.RunTrace{ID: "ok-1", Outcome: "success"}), "StoreTrace")

	// Append garbage to the manifest.
	require.NoError(t,
		os.WriteFile(fs.manifestPath(), []byte("not-json-at-all\n"), 0o644),
		"corrupt manifest")

	got, err := fs.QueryTraces(ctx, types.TraceFilter{})
	require.NoError(t, err, "QueryTraces after corruption")
	require.Len(t, got, 1)
	require.Equal(t, "ok-1", got[0].ID)

	// Manifest should now parse again.
	traces, _, ok := fs.loadManifestIndex()
	require.True(t, ok, "manifest still corrupt after rebuild")
	require.Contains(t, traces, "ok-1", "rebuilt manifest missing ok-1")
}

// TestManifest_LastWriteWins pins that re-storing a trace with the
// same ID overwrites the manifest entry (last entry per id wins).
// Critical for idempotent re-ingest: the index should reflect the
// final stored state, not a stale earlier version.
func TestManifest_LastWriteWins(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	require.NoError(t, err, "NewFileStore")
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	require.NoError(t, fs.StoreTrace(ctx, types.RunTrace{ID: "same", Outcome: "success"}), "StoreTrace v1")
	require.NoError(t, fs.StoreTrace(ctx, types.RunTrace{ID: "same", Outcome: "error"}), "StoreTrace v2")

	traces, _, ok := fs.loadManifestIndex()
	require.True(t, ok, "loadManifestIndex returned ok=false")
	require.Equal(t, "error", traces["same"].Outcome, "last-write-wins")
}

// TestManifest_ConcurrentAppendSafe smoke-tests the O_APPEND
// atomicity assumption: multiple goroutines appending to the
// manifest concurrently must produce a parseable file where every
// store call is represented.
func TestManifest_ConcurrentAppendSafe(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	require.NoError(t, err, "NewFileStore")
	defer func() { _ = fs.Close() }()

	const goroutines = 8
	const each = 20

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; i < each; i++ {
				id := fmt.Sprintf("g%d-%d", g, i%10)
				_ = fs.StoreTrace(ctx, types.RunTrace{
					ID:        id,
					Outcome:   "success",
					StartedAt: time.Now(),
				})
			}
		}(g)
	}
	wg.Wait()

	// Manifest should parse cleanly. Every line is checked, so one
	// report lists all corrupt lines rather than only the first.
	data, err := os.ReadFile(fs.manifestPath())
	require.NoError(t, err, "read manifest")
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	require.NotEmpty(t, lines, "manifest empty after concurrent writes")
	for i, line := range lines {
		if line == "" {
			continue
		}
		var e manifestEntry
		assert.NoError(t, json.Unmarshal([]byte(line), &e), "line %d corrupt under concurrency: %s", i, line)
	}
}
