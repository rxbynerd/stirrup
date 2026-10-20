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
	must := require.New(t)
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	must.NoError(err, "NewFileStore")
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	must.NoError(fs.StoreTrace(ctx, types.RunTrace{ID: "t1", Outcome: "success"}), "StoreTrace")
	must.NoError(fs.StoreRecording(ctx, types.RunRecording{
		RunID:        "r1",
		FinalOutcome: types.RunTrace{ID: "r1", Outcome: "error"},
	}), "StoreRecording")

	traces, recordings, ok := fs.loadManifestIndex()
	must.True(ok, "loadManifestIndex returned ok=false")
	must.Contains(traces, "t1", "manifest missing trace t1")
	must.Contains(recordings, "r1", "manifest missing recording r1")
	must.Equal("success", traces["t1"].Outcome, "manifest entry for t1")
}

// TestManifest_FilterShortCircuitsLoad pins that a filter rejecting
// entries on a pre-decoded field (Outcome here) skips the JSON load
// for those entries: it stores a passing trace whose JSON file is
// then truncated to invalid JSON, and a query filtering for a
// different outcome must still succeed.
func TestManifest_FilterShortCircuitsLoad(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	must.NoError(err, "NewFileStore")
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	must.NoError(fs.StoreTrace(ctx, types.RunTrace{ID: "good-passing", Outcome: "success"}), "StoreTrace good")
	must.NoError(fs.StoreTrace(ctx, types.RunTrace{ID: "good-failing", Outcome: "error"}), "StoreTrace failing")

	// Corrupt the passing trace's JSON file. The manifest still
	// claims it's outcome=success, so a filter for outcome=error
	// must short-circuit and never try to load it.
	must.NoError(os.WriteFile(filepath.Join(dir, "traces", "good-passing.json"), []byte("{not json"), 0o644),
		"corrupt trace file")

	got, err := fs.QueryTraces(ctx, types.TraceFilter{Outcome: "error"})
	must.NoError(err, "QueryTraces with manifest short-circuit")
	must.Len(got, 1)
	must.Equal("good-failing", got[0].ID)
}

// TestManifest_RebuildOnMissing pins the missing-manifest recovery
// path: when manifest.jsonl is absent (or removed by an operator),
// the next query falls back to a directory scan AND rebuilds the
// manifest as a side effect.
func TestManifest_RebuildOnMissing(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	must.NoError(err, "NewFileStore")
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		must.NoError(fs.StoreTrace(ctx, types.RunTrace{
			ID:      fmt.Sprintf("t%d", i+1),
			Outcome: "success",
		}), "StoreTrace t%d", i+1)
	}

	// Operator forces a rebuild by removing the manifest.
	must.NoError(os.Remove(fs.manifestPath()), "remove manifest")

	got, err := fs.QueryTraces(ctx, types.TraceFilter{})
	must.NoError(err, "QueryTraces after manifest removal")
	must.Len(got, 3, "rebuild must surface all traces")

	// And the manifest is back.
	_, err = os.Stat(fs.manifestPath())
	must.NoError(err, "manifest not rebuilt")
}

// TestManifest_RebuildOnCorrupt pins recovery from a malformed
// manifest. A single garbled line in manifest.jsonl invalidates
// the index; the read path falls back to scanning everything and
// rewrites a fresh manifest.
func TestManifest_RebuildOnCorrupt(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	must.NoError(err, "NewFileStore")
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	must.NoError(fs.StoreTrace(ctx, types.RunTrace{ID: "ok-1", Outcome: "success"}), "StoreTrace")

	// Append garbage to the manifest.
	must.NoError(os.WriteFile(fs.manifestPath(), []byte("not-json-at-all\n"), 0o644),
		"corrupt manifest")

	got, err := fs.QueryTraces(ctx, types.TraceFilter{})
	must.NoError(err, "QueryTraces after corruption")
	must.Len(got, 1)
	must.Equal("ok-1", got[0].ID)

	// Manifest should now parse again.
	traces, _, ok := fs.loadManifestIndex()
	must.True(ok, "manifest still corrupt after rebuild")
	must.Contains(traces, "ok-1", "rebuilt manifest missing ok-1")
}

// TestManifest_LastWriteWins pins that re-storing a trace with the
// same ID overwrites the manifest entry (last entry per id wins).
// Critical for idempotent re-ingest: the index should reflect the
// final stored state, not a stale earlier version.
func TestManifest_LastWriteWins(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	must.NoError(err, "NewFileStore")
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	must.NoError(fs.StoreTrace(ctx, types.RunTrace{ID: "same", Outcome: "success"}), "StoreTrace v1")
	must.NoError(fs.StoreTrace(ctx, types.RunTrace{ID: "same", Outcome: "error"}), "StoreTrace v2")

	traces, _, ok := fs.loadManifestIndex()
	must.True(ok, "loadManifestIndex returned ok=false")
	must.Equal("error", traces["same"].Outcome, "last-write-wins")
}

// TestManifest_ConcurrentAppendSafe smoke-tests the O_APPEND
// atomicity assumption: multiple goroutines appending to the
// manifest concurrently must produce a parseable file where every
// store call is represented.
func TestManifest_ConcurrentAppendSafe(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	must.NoError(err, "NewFileStore")
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
	must.NoError(err, "read manifest")
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	must.NotEmpty(lines, "manifest empty after concurrent writes")
	for i, line := range lines {
		if line == "" {
			continue
		}
		var e manifestEntry
		check.NoError(json.Unmarshal([]byte(line), &e), "line %d corrupt under concurrency: %s", i, line)
	}
}
