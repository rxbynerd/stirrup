package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval/lakehouse"
	"github.com/rxbynerd/stirrup/types"
)

// TestIngestFile_SubRunsStayInsideParentRecording pins that a trace
// carrying sub-agent records ingests as one parent recording: the
// interrupted-run summary counts only the parent's turns, the children
// are kept in SubRuns, and no child is stored as a run of its own.
func TestIngestFile_SubRunsStayInsideParentRecording(t *testing.T) {
	dir := t.TempDir()
	turn := func(runID, parentRunID string, n int) string {
		ev := map[string]any{"kind": "turn_record", "runId": runID, "turn": n, "modelOutput": []types.ContentBlock{{Type: "text", Text: runID}}}
		if parentRunID != "" {
			ev["parentRunId"] = parentRunID
		}
		return mustMarshal(t, ev)
	}
	tracePath := writeTraceFile(t, dir, "run-p.jsonl", []string{
		mustMarshal(t, map[string]any{"kind": "run_started", "schemaVersion": "2", "runId": "run-p", "config": types.RunConfig{RunID: "run-p"}}),
		mustMarshal(t, map[string]any{"kind": "subrun_started", "runId": "run-p/sub-1", "parentRunId": "run-p", "config": types.RunConfig{RunID: "run-p/sub-1"}}),
		turn("run-p/sub-1", "run-p", 0),
		turn("run-p/sub-1", "run-p", 1),
		mustMarshal(t, map[string]any{"kind": "subrun_finished", "runId": "run-p/sub-1", "parentRunId": "run-p", "subRun": types.SubRunSummary{RunID: "run-p/sub-1", ParentRunID: "run-p", Outcome: "success", Turns: 2}}),
		turn("run-p", "", 0),
	})

	storeDir := t.TempDir()
	store, err := lakehouse.NewFileStore(storeDir)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	traces, recordings, skipped, err := ingestFile(context.Background(), store, tracePath, false)
	require.NoError(t, err)
	require.Equal(t, 1, traces, "ingested traces")
	require.Equal(t, 1, recordings, "ingested recordings")
	require.Equal(t, 0, skipped, "skipped lines")

	for _, sub := range []string{"traces", "recordings"} {
		t.Run(sub, func(t *testing.T) {
			entries, err := os.ReadDir(filepath.Join(storeDir, sub))
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, "run-p.json", entries[0].Name())
		})
	}

	gotTraces, err := store.QueryTraces(context.Background(), types.TraceFilter{})
	require.NoError(t, err)
	require.Len(t, gotTraces, 1, "traces = %+v", gotTraces)
	require.Equal(t, 1, gotTraces[0].Turns, "parent turns in summary")
	require.Equal(t, "interrupted", gotTraces[0].Outcome)

	recs, err := store.QueryRecordings(context.Background(), types.TraceFilter{})
	require.NoError(t, err)
	require.Len(t, recs, 1, "recordings = %+v", recs)
	require.Len(t, recs[0].Turns, 1, "parent turns in recording")
	require.Len(t, recs[0].SubRuns, 1, "sub-runs in recording")

	sub := recs[0].SubRuns[0]
	require.Equal(t, "run-p/sub-1", sub.RunID, "sub-run = %+v", sub)
	require.Len(t, sub.Turns, 2, "sub-run turns")
	require.NotNil(t, sub.Summary, "sub-run summary")
}

// TestSummariseFailingTurn_UsesParentsLastTurn pins that the mined
// task description quotes the parent's last turn even when a sub-run
// recorded later turns.
func TestSummariseFailingTurn_UsesParentsLastTurn(t *testing.T) {
	rec := types.RunRecording{
		RunID: "run-p",
		Turns: []types.TurnRecord{
			{Turn: 4, RunID: "run-p", ModelOutput: []types.ContentBlock{{Type: "text", Text: "parent gave up"}}},
		},
		SubRuns: []types.SubRunRecording{{
			RunID:       "run-p/sub-1",
			ParentRunID: "run-p",
			Turns: []types.TurnRecord{
				{Turn: 9, RunID: "run-p/sub-1", ParentRunID: "run-p", ModelOutput: []types.ContentBlock{{Type: "text", Text: "child text"}}},
			},
		}},
	}
	summary := summariseFailingTurn(rec)
	require.Contains(t, summary, "last turn: 4")
	require.Contains(t, summary, "parent gave up")
	require.NotContains(t, summary, "child text", "summary quotes a sub-run turn")
}
