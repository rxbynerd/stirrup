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
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = store.Close() }()

	traces, recordings, skipped, err := ingestFile(context.Background(), store, tracePath, false)
	must.NoError(err)
	must.Equal(1, traces, "ingested traces")
	must.Equal(1, recordings, "ingested recordings")
	must.Equal(0, skipped, "skipped lines")

	for _, sub := range []string{"traces", "recordings"} {
		t.Run(sub, func(t *testing.T) {
			must := require.New(t)
			entries, err := os.ReadDir(filepath.Join(storeDir, sub))
			must.NoError(err)
			must.Len(entries, 1)
			must.Equal("run-p.json", entries[0].Name())
		})
	}

	gotTraces, err := store.QueryTraces(context.Background(), types.TraceFilter{})
	must.NoError(err)
	must.Len(gotTraces, 1, "traces = %+v", gotTraces)
	must.Equal(1, gotTraces[0].Turns, "parent turns in summary")
	must.Equal("interrupted", gotTraces[0].Outcome)

	recs, err := store.QueryRecordings(context.Background(), types.TraceFilter{})
	must.NoError(err)
	must.Len(recs, 1, "recordings = %+v", recs)
	must.Len(recs[0].Turns, 1, "parent turns in recording")
	must.Len(recs[0].SubRuns, 1, "sub-runs in recording")

	sub := recs[0].SubRuns[0]
	must.Equal("run-p/sub-1", sub.RunID, "sub-run = %+v", sub)
	must.Len(sub.Turns, 2, "sub-run turns")
	must.NotNil(sub.Summary, "sub-run summary")
}

// TestSummariseFailingTurn_UsesParentsLastTurn pins that the mined
// task description quotes the parent's last turn even when a sub-run
// recorded later turns.
func TestSummariseFailingTurn_UsesParentsLastTurn(t *testing.T) {
	must := require.New(t)
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
	must.Contains(summary, "last turn: 4")
	must.Contains(summary, "parent gave up")
	must.NotContains(summary, "child text", "summary quotes a sub-run turn")
}
