package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/eval/lakehouse"
	"github.com/rxbynerd/stirrup/types"
)

// writeTraceFile writes lines to a JSONL trace file under dir and
// returns the path. Test helper kept local so the ingest tests do
// not depend on the harness package's own emitter (an internal
// boundary per CLAUDE.md).
func writeTraceFile(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	content := ""
	for _, line := range lines {
		content += line + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestIngestFile_StreamingFullRun pins the happy-path streaming
// ingest: a run_started + N turn_records + run_finished file lands
// one traces/<id>.json plus one recordings/<id>.json on disk.
func TestIngestFile_StreamingFullRun(t *testing.T) {
	dir := t.TempDir()
	tracePath := writeTraceFile(t, dir, "run-1.jsonl", []string{
		mustMarshal(t, map[string]any{
			"kind":      "run_started",
			"runId":     "run-1",
			"startedAt": time.Now(),
			"config":    types.RunConfig{RunID: "run-1", Mode: "execution"},
		}),
		mustMarshal(t, map[string]any{
			"kind":        "turn_record",
			"turn":        1,
			"modelInput":  types.ModelInput{Model: "claude-3-5"},
			"modelOutput": []types.ContentBlock{{Type: "text", Text: "hi"}},
		}),
		mustMarshal(t, map[string]any{
			"kind":  "run_finished",
			"trace": types.RunTrace{ID: "run-1", Turns: 1, Outcome: "success"},
		}),
	})

	storeDir := t.TempDir()
	store, err := lakehouse.NewFileStore(storeDir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	traces, recordings, skipped, err := ingestFile(context.Background(), store, tracePath, false)
	if err != nil {
		t.Fatalf("ingestFile: %v", err)
	}
	if traces != 1 || recordings != 1 || skipped != 0 {
		t.Errorf("counts: traces=%d recordings=%d skipped=%d; want 1/1/0", traces, recordings, skipped)
	}

	if _, err := os.Stat(filepath.Join(storeDir, "traces", "run-1.json")); err != nil {
		t.Errorf("traces/run-1.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(storeDir, "recordings", "run-1.json")); err != nil {
		t.Errorf("recordings/run-1.json missing: %v", err)
	}

	gotTraces, err := store.QueryTraces(context.Background(), types.TraceFilter{})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(gotTraces) != 1 || gotTraces[0].ID != "run-1" || gotTraces[0].Outcome != "success" {
		t.Errorf("trace = %+v, want one with ID=run-1 Outcome=success", gotTraces)
	}
}

// TestIngestFile_LegacySingleBlob pins backward-compat: a pre-#270
// JSONL trace file (one RunTrace per line, no kind discriminator)
// ingests as traces only — no recordings/ entry is produced because
// the legacy shape has no transcript content.
func TestIngestFile_LegacySingleBlob(t *testing.T) {
	dir := t.TempDir()
	tracePath := writeTraceFile(t, dir, "legacy.jsonl", []string{
		mustMarshal(t, types.RunTrace{ID: "old-1", Outcome: "success", Turns: 3}),
		mustMarshal(t, types.RunTrace{ID: "old-2", Outcome: "error", Turns: 1}),
	})

	storeDir := t.TempDir()
	store, err := lakehouse.NewFileStore(storeDir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	traces, recordings, skipped, err := ingestFile(context.Background(), store, tracePath, false)
	if err != nil {
		t.Fatalf("ingestFile: %v", err)
	}
	if traces != 2 || recordings != 0 || skipped != 0 {
		t.Errorf("counts: traces=%d recordings=%d skipped=%d; want 2/0/0", traces, recordings, skipped)
	}

	gotTraces, err := store.QueryTraces(context.Background(), types.TraceFilter{})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(gotTraces) != 2 {
		t.Fatalf("got %d traces, want 2", len(gotTraces))
	}
}

// TestIngestFile_PartialStream pins the interrupted-run path: a
// streaming trace missing its run_finished event is ingested as a
// recording with FinalOutcome.Outcome=="interrupted" by default,
// or skipped entirely when --skip-partial is set.
func TestIngestFile_PartialStream(t *testing.T) {
	dir := t.TempDir()
	tracePath := writeTraceFile(t, dir, "partial.jsonl", []string{
		mustMarshal(t, map[string]any{
			"kind":      "run_started",
			"runId":     "partial-run",
			"startedAt": time.Now(),
			"config":    types.RunConfig{RunID: "partial-run"},
		}),
		mustMarshal(t, map[string]any{
			"kind":        "turn_record",
			"turn":        1,
			"modelOutput": []types.ContentBlock{{Type: "text", Text: "only"}},
		}),
		// no run_finished event
	})

	storeDir := t.TempDir()
	store, err := lakehouse.NewFileStore(storeDir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	traces, recordings, skipped, err := ingestFile(context.Background(), store, tracePath, false)
	if err != nil {
		t.Fatalf("ingestFile (default): %v", err)
	}
	if traces != 1 || recordings != 1 || skipped != 0 {
		t.Errorf("default counts: traces=%d recordings=%d skipped=%d; want 1/1/0", traces, recordings, skipped)
	}

	gotTraces, err := store.QueryTraces(context.Background(), types.TraceFilter{})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(gotTraces) != 1 || gotTraces[0].Outcome != "interrupted" {
		t.Errorf("trace = %+v, want one with Outcome=interrupted", gotTraces)
	}

	// Re-ingest with --skip-partial against a fresh store: nothing
	// should land.
	storeDir2 := t.TempDir()
	store2, err := lakehouse.NewFileStore(storeDir2)
	if err != nil {
		t.Fatalf("NewFileStore 2: %v", err)
	}
	defer func() { _ = store2.Close() }()
	traces2, recordings2, skipped2, err := ingestFile(context.Background(), store2, tracePath, true)
	if err != nil {
		t.Fatalf("ingestFile (skip-partial): %v", err)
	}
	if traces2 != 0 || recordings2 != 0 || skipped2 != 1 {
		t.Errorf("skip-partial counts: traces=%d recordings=%d skipped=%d; want 0/0/1", traces2, recordings2, skipped2)
	}
}

// TestIngestFile_Idempotent pins that re-ingesting the same file
// is a no-op in net effect — the resulting on-disk state is
// identical to a single ingest. Together with the atomic-write
// pattern from #267 this guarantees no torn files even on
// concurrent retry.
func TestIngestFile_Idempotent(t *testing.T) {
	dir := t.TempDir()
	tracePath := writeTraceFile(t, dir, "run-i.jsonl", []string{
		mustMarshal(t, map[string]any{
			"kind":      "run_started",
			"runId":     "run-i",
			"startedAt": time.Now(),
			"config":    types.RunConfig{RunID: "run-i"},
		}),
		mustMarshal(t, map[string]any{
			"kind":  "run_finished",
			"trace": types.RunTrace{ID: "run-i", Outcome: "success"},
		}),
	})

	storeDir := t.TempDir()
	store, err := lakehouse.NewFileStore(storeDir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	for i := 0; i < 3; i++ {
		if _, _, _, err := ingestFile(context.Background(), store, tracePath, false); err != nil {
			t.Fatalf("ingestFile iter %d: %v", i, err)
		}
	}
	gotTraces, err := store.QueryTraces(context.Background(), types.TraceFilter{})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(gotTraces) != 1 || gotTraces[0].ID != "run-i" {
		t.Errorf("after 3 ingests: got %d traces, want 1 (last-write-wins)", len(gotTraces))
	}
}

// TestDetectFormat covers the three cases the format peeker has to
// distinguish: streaming events (line carries a kind key), legacy
// single-blob (line lacks kind), and empty file. Mixed-format
// invocation works at the per-file level — each --trace argument is
// detected independently.
func TestDetectFormat(t *testing.T) {
	dir := t.TempDir()

	streaming := writeTraceFile(t, dir, "stream.jsonl", []string{
		mustMarshal(t, map[string]any{"kind": "run_started", "runId": "x"}),
	})
	legacy := writeTraceFile(t, dir, "legacy.jsonl", []string{
		mustMarshal(t, types.RunTrace{ID: "x"}),
	})
	empty := writeTraceFile(t, dir, "empty.jsonl", nil)

	cases := []struct {
		path string
		want traceFormat
	}{
		{streaming, formatStreaming},
		{legacy, formatLegacy},
		{empty, formatEmpty},
	}
	for _, tc := range cases {
		_, got, err := detectFormat(tc.path)
		if err != nil {
			t.Errorf("detectFormat(%s): err=%v", filepath.Base(tc.path), err)
			continue
		}
		if got != tc.want {
			t.Errorf("detectFormat(%s) = %v, want %v", filepath.Base(tc.path), got, tc.want)
		}
	}
}
