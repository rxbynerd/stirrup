package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	for _, sub := range []string{"traces", "recordings"} {
		entries, err := os.ReadDir(filepath.Join(storeDir, sub))
		if err != nil {
			t.Fatalf("read %s: %v", sub, err)
		}
		if len(entries) != 1 || entries[0].Name() != "run-p.json" {
			t.Errorf("%s entries = %v, want only run-p.json", sub, entries)
		}
	}

	gotTraces, err := store.QueryTraces(context.Background(), types.TraceFilter{})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(gotTraces) != 1 || gotTraces[0].Turns != 1 || gotTraces[0].Outcome != "interrupted" {
		t.Errorf("traces = %+v, want one interrupted run with 1 parent turn", gotTraces)
	}
	recs, err := store.QueryRecordings(context.Background(), types.TraceFilter{})
	if err != nil {
		t.Fatalf("QueryRecordings: %v", err)
	}
	if len(recs) != 1 || len(recs[0].Turns) != 1 || len(recs[0].SubRuns) != 1 {
		t.Fatalf("recordings = %+v, want one with 1 turn and 1 sub-run", recs)
	}
	if sub := recs[0].SubRuns[0]; sub.RunID != "run-p/sub-1" || len(sub.Turns) != 2 || sub.Summary == nil {
		t.Errorf("sub-run = %+v, want run-p/sub-1 with 2 turns and a summary", sub)
	}
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
	got := summariseFailingTurn(rec)
	if !strings.Contains(got, "last turn: 4") || !strings.Contains(got, "parent gave up") {
		t.Errorf("summary = %q, want the parent's last turn", got)
	}
	if strings.Contains(got, "child text") {
		t.Errorf("summary = %q, quotes a sub-run turn", got)
	}
}
