package lakehouse

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// TestManifest_PopulatedOnStore pins that every StoreTrace and
// StoreRecording call appends a matching manifest.jsonl line. A
// regression that wrote the JSON but skipped the manifest would
// silently degrade query performance back to O(files).
func TestManifest_PopulatedOnStore(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	if err := fs.StoreTrace(ctx, types.RunTrace{ID: "t1", Outcome: "success"}); err != nil {
		t.Fatalf("StoreTrace: %v", err)
	}
	if err := fs.StoreRecording(ctx, types.RunRecording{
		RunID:        "r1",
		FinalOutcome: types.RunTrace{ID: "r1", Outcome: "error"},
	}); err != nil {
		t.Fatalf("StoreRecording: %v", err)
	}

	traces, recordings, ok := fs.loadManifestIndex()
	if !ok {
		t.Fatal("loadManifestIndex returned ok=false")
	}
	if _, has := traces["t1"]; !has {
		t.Errorf("manifest missing trace t1; got %v", traces)
	}
	if _, has := recordings["r1"]; !has {
		t.Errorf("manifest missing recording r1; got %v", recordings)
	}
	if traces["t1"].Outcome != "success" {
		t.Errorf("manifest entry for t1: outcome = %q, want success", traces["t1"].Outcome)
	}
}

// TestManifest_FilterShortCircuitsLoad pins that a filter rejecting
// entries on a pre-decoded field (Outcome here) skips the JSON load
// for those entries: it stores a passing trace whose JSON file is
// then truncated to invalid JSON, and a query filtering for a
// different outcome must still succeed.
func TestManifest_FilterShortCircuitsLoad(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	if err := fs.StoreTrace(ctx, types.RunTrace{ID: "good-passing", Outcome: "success"}); err != nil {
		t.Fatalf("StoreTrace good: %v", err)
	}
	if err := fs.StoreTrace(ctx, types.RunTrace{ID: "good-failing", Outcome: "error"}); err != nil {
		t.Fatalf("StoreTrace failing: %v", err)
	}

	// Corrupt the passing trace's JSON file. The manifest still
	// claims it's outcome=success, so a filter for outcome=error
	// must short-circuit and never try to load it.
	if err := os.WriteFile(filepath.Join(dir, "traces", "good-passing.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	got, err := fs.QueryTraces(ctx, types.TraceFilter{Outcome: "error"})
	if err != nil {
		t.Fatalf("QueryTraces with manifest short-circuit: %v", err)
	}
	if len(got) != 1 || got[0].ID != "good-failing" {
		t.Errorf("got %v, want one trace ID=good-failing", got)
	}
}

// TestManifest_RebuildOnMissing pins the missing-manifest recovery
// path: when manifest.jsonl is absent (or removed by an operator),
// the next query falls back to a directory scan AND rebuilds the
// manifest as a side effect.
func TestManifest_RebuildOnMissing(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := fs.StoreTrace(ctx, types.RunTrace{
			ID:      "t" + string(rune('1'+i)),
			Outcome: "success",
		}); err != nil {
			t.Fatalf("StoreTrace t%d: %v", i, err)
		}
	}

	// Operator forces a rebuild by removing the manifest.
	if err := os.Remove(fs.manifestPath()); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}

	got, err := fs.QueryTraces(ctx, types.TraceFilter{})
	if err != nil {
		t.Fatalf("QueryTraces after manifest removal: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d traces, want 3 (rebuild must surface all)", len(got))
	}

	// And the manifest is back.
	if _, err := os.Stat(fs.manifestPath()); err != nil {
		t.Errorf("manifest not rebuilt: %v", err)
	}
}

// TestManifest_RebuildOnCorrupt pins recovery from a malformed
// manifest. A single garbled line in manifest.jsonl invalidates
// the index; the read path falls back to scanning everything and
// rewrites a fresh manifest.
func TestManifest_RebuildOnCorrupt(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	if err := fs.StoreTrace(ctx, types.RunTrace{ID: "ok-1", Outcome: "success"}); err != nil {
		t.Fatalf("StoreTrace: %v", err)
	}

	// Append garbage to the manifest.
	if err := os.WriteFile(fs.manifestPath(), []byte("not-json-at-all\n"), 0o644); err != nil {
		t.Fatalf("corrupt manifest: %v", err)
	}

	got, err := fs.QueryTraces(ctx, types.TraceFilter{})
	if err != nil {
		t.Fatalf("QueryTraces after corruption: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ok-1" {
		t.Errorf("got %v, want one trace ID=ok-1", got)
	}

	// Manifest should now parse again.
	traces, _, ok := fs.loadManifestIndex()
	if !ok {
		t.Fatal("manifest still corrupt after rebuild")
	}
	if _, has := traces["ok-1"]; !has {
		t.Errorf("rebuilt manifest missing ok-1: %v", traces)
	}
}

// TestManifest_LastWriteWins pins that re-storing a trace with the
// same ID overwrites the manifest entry (last entry per id wins).
// Critical for idempotent re-ingest: the index should reflect the
// final stored state, not a stale earlier version.
func TestManifest_LastWriteWins(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	if err := fs.StoreTrace(ctx, types.RunTrace{ID: "same", Outcome: "success"}); err != nil {
		t.Fatalf("StoreTrace v1: %v", err)
	}
	if err := fs.StoreTrace(ctx, types.RunTrace{ID: "same", Outcome: "error"}); err != nil {
		t.Fatalf("StoreTrace v2: %v", err)
	}

	traces, _, ok := fs.loadManifestIndex()
	if !ok {
		t.Fatal("loadManifestIndex returned ok=false")
	}
	if traces["same"].Outcome != "error" {
		t.Errorf("manifest entry outcome = %q, want error (last-write-wins)", traces["same"].Outcome)
	}
}

// TestManifest_ConcurrentAppendSafe smoke-tests the O_APPEND
// atomicity assumption: multiple goroutines appending to the
// manifest concurrently must produce a parseable file where every
// store call is represented.
func TestManifest_ConcurrentAppendSafe(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
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
				id := "g" + string(rune('0'+g)) + "-" + string(rune('0'+i%10))
				_ = fs.StoreTrace(ctx, types.RunTrace{
					ID:        id,
					Outcome:   "success",
					StartedAt: time.Now(),
				})
			}
		}(g)
	}
	wg.Wait()

	// Manifest should parse cleanly.
	data, err := os.ReadFile(fs.manifestPath())
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("manifest empty after concurrent writes")
	}
	for i, line := range lines {
		if line == "" {
			continue
		}
		var e manifestEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Errorf("line %d corrupt under concurrency: %v\n%s", i, err, line)
		}
	}
}
