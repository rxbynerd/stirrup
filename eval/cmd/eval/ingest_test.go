package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval/lakehouse"
	"github.com/rxbynerd/stirrup/types"
	tracereader "github.com/rxbynerd/stirrup/types/trace"
)

// writeTraceFile writes lines to a JSONL trace file under dir and returns
// the path. Kept local so ingest tests do not depend on the harness
// package's own emitter (an internal boundary per CLAUDE.md).
func writeTraceFile(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	must := require.New(t)
	path := filepath.Join(dir, name)
	content := ""
	for _, line := range lines {
		content += line + "\n"
	}
	must.NoError(os.WriteFile(path, []byte(content), 0o644), "write %s", path)
	return path
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	must := require.New(t)
	b, err := json.Marshal(v)
	must.NoError(err)
	return string(b)
}

// TestIngestFile_StreamingFullRun pins that a run_started + N
// turn_records + run_finished file lands one traces/<id>.json plus one
// recordings/<id>.json on disk.
func TestIngestFile_StreamingFullRun(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = store.Close() }()

	traces, recordings, skipped, err := ingestFile(context.Background(), store, tracePath, false)
	must.NoError(err)
	must.Equal(1, traces, "trace count")
	must.Equal(1, recordings, "recording count")
	must.Equal(0, skipped, "skipped count")

	must.FileExists(filepath.Join(storeDir, "traces", "run-1.json"))
	must.FileExists(filepath.Join(storeDir, "recordings", "run-1.json"))

	gotTraces, err := store.QueryTraces(context.Background(), types.TraceFilter{})
	must.NoError(err)
	must.Len(gotTraces, 1)
	must.Equal("run-1", gotTraces[0].ID)
	must.Equal("success", gotTraces[0].Outcome)
}

// TestIngestFile_LegacySingleBlob pins that a legacy JSONL trace file (one
// RunTrace per line, no kind discriminator) ingests as traces only — no
// recordings/ entry, since the legacy shape has no transcript content.
func TestIngestFile_LegacySingleBlob(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	tracePath := writeTraceFile(t, dir, "legacy.jsonl", []string{
		mustMarshal(t, types.RunTrace{ID: "old-1", Outcome: "success", Turns: 3}),
		mustMarshal(t, types.RunTrace{ID: "old-2", Outcome: "error", Turns: 1}),
	})

	store, err := lakehouse.NewFileStore(t.TempDir())
	must.NoError(err)
	defer func() { _ = store.Close() }()

	traces, recordings, skipped, err := ingestFile(context.Background(), store, tracePath, false)
	must.NoError(err)
	must.Equal(2, traces, "trace count")
	must.Equal(0, recordings, "recording count")
	must.Equal(0, skipped, "skipped count")

	gotTraces, err := store.QueryTraces(context.Background(), types.TraceFilter{})
	must.NoError(err)
	must.Len(gotTraces, 2)
}

// TestIngestFile_PartialStream pins that a streaming trace missing its
// run_finished event is ingested as a recording with
// FinalOutcome.Outcome=="interrupted" by default, or skipped entirely when
// --skip-partial is set.
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
	})

	t.Run("default keeps the run as interrupted", func(t *testing.T) {
		must := require.New(t)
		store, err := lakehouse.NewFileStore(t.TempDir())
		must.NoError(err)
		defer func() { _ = store.Close() }()

		traces, recordings, skipped, err := ingestFile(context.Background(), store, tracePath, false)
		must.NoError(err)
		must.Equal(1, traces, "trace count")
		must.Equal(1, recordings, "recording count")
		must.Equal(0, skipped, "skipped count")

		gotTraces, err := store.QueryTraces(context.Background(), types.TraceFilter{})
		must.NoError(err)
		must.Len(gotTraces, 1)
		must.Equal("interrupted", gotTraces[0].Outcome)
	})

	t.Run("skip-partial drops the run", func(t *testing.T) {
		must := require.New(t)
		store, err := lakehouse.NewFileStore(t.TempDir())
		must.NoError(err)
		defer func() { _ = store.Close() }()

		traces, recordings, skipped, err := ingestFile(context.Background(), store, tracePath, true)
		must.NoError(err)
		must.Equal(0, traces, "trace count")
		must.Equal(0, recordings, "recording count")
		must.Equal(1, skipped, "skipped count")
	})
}

// TestIngestFile_Idempotent pins that re-ingesting the same file is a no-op
// in net effect: the resulting on-disk state is identical to a single
// ingest.
func TestIngestFile_Idempotent(t *testing.T) {
	must := require.New(t)
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

	store, err := lakehouse.NewFileStore(t.TempDir())
	must.NoError(err)
	defer func() { _ = store.Close() }()

	for i := range 3 {
		_, _, _, err := ingestFile(context.Background(), store, tracePath, false)
		must.NoError(err, "ingest iteration %d", i)
	}

	gotTraces, err := store.QueryTraces(context.Background(), types.TraceFilter{})
	must.NoError(err)
	must.Len(gotTraces, 1, "re-ingest should leave one trace (last write wins)")
	must.Equal("run-i", gotTraces[0].ID)
}

// TestDetectFormat_ClassifiesStreamingLegacyAndEmpty covers the three cases
// the format peeker distinguishes: streaming events (line carries a kind
// key), legacy single-blob (line lacks kind), and empty file.
func TestDetectFormat_ClassifiesStreamingLegacyAndEmpty(t *testing.T) {
	dir := t.TempDir()

	streaming := writeTraceFile(t, dir, "stream.jsonl", []string{
		mustMarshal(t, map[string]any{"kind": "run_started", "runId": "x"}),
	})
	legacy := writeTraceFile(t, dir, "legacy.jsonl", []string{
		mustMarshal(t, types.RunTrace{ID: "x"}),
	})
	empty := writeTraceFile(t, dir, "empty.jsonl", nil)

	cases := []struct {
		name string
		path string
		want traceFormat
	}{
		{"streaming events", streaming, formatStreaming},
		{"legacy single blob", legacy, formatLegacy},
		{"empty file", empty, formatEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, got, err := detectFormat(tc.path)
			must.NoError(err)
			must.Equal(tc.want, got)
		})
	}
}

// TestIngestFile_ReportsSkippedLines pins that ingest tells the operator
// when oversized lines were dropped, and still stores the recording read
// around them.
func TestIngestFile_ReportsSkippedLines(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	oversized := mustMarshal(t, map[string]any{
		"kind":        "turn_record",
		"turn":        1,
		"modelOutput": []types.ContentBlock{{Type: "text", Text: strings.Repeat("x", tracereader.MaxLineBytes+8)}},
	})
	tracePath := writeTraceFile(t, dir, "big.jsonl", []string{
		mustMarshal(t, map[string]any{
			"kind":      "run_started",
			"runId":     "big-run",
			"startedAt": time.Now(),
			"config":    types.RunConfig{RunID: "big-run"},
		}),
		oversized,
		mustMarshal(t, map[string]any{"kind": "turn_record", "turn": 2}),
	})

	store, err := lakehouse.NewFileStore(t.TempDir())
	must.NoError(err)
	defer func() { _ = store.Close() }()

	pr, pw, err := os.Pipe()
	must.NoError(err)
	origStderr := os.Stderr
	os.Stderr = pw
	traces, recordings, _, ingestErr := ingestFile(context.Background(), store, tracePath, false)
	os.Stderr = origStderr
	_ = pw.Close()
	stderr, _ := io.ReadAll(pr)
	_ = pr.Close()

	must.NoError(ingestErr)
	must.Equal(1, traces, "trace count")
	must.Equal(1, recordings, "recording count")
	must.Contains(string(stderr), "big-run is a partial recording: skipped 1 line(s)")
}
