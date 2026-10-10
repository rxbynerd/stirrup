package trace

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata golden files")

// eventLines renders each value as one JSONL line.
func eventLines(t *testing.T, events ...map[string]any) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, ev := range events {
		buf.Write(mustMarshal(t, ev))
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func readRecordingFrom(t *testing.T, body []byte) *types.RunRecording {
	t.Helper()
	rec, err := NewReader(bytes.NewReader(body), WithLogger(discardLogger())).ReadRecording()
	if err != nil {
		t.Fatalf("ReadRecording: %v", err)
	}
	return rec
}

func turnLine(runID, parentRunID string, turn int, text string) map[string]any {
	ev := map[string]any{
		"kind":        "turn_record",
		"turn":        turn,
		"modelOutput": []types.ContentBlock{{Type: "text", Text: text}},
	}
	if runID != "" {
		ev["runId"] = runID
	}
	if parentRunID != "" {
		ev["parentRunId"] = parentRunID
	}
	return ev
}

func turnTexts(turns []types.TurnRecord) []string {
	out := make([]string, len(turns))
	for i, turn := range turns {
		if len(turn.ModelOutput) > 0 {
			out[i] = turn.ModelOutput[0].Text
		}
	}
	return out
}

// TestReadRecording_LegacyFixtureMatchesGolden pins that a schema-1
// trace whose sub-agent records carry no run IDs reads exactly as it
// did before sub-run grouping: every turn, parent and child, lands in
// Turns in file order and SubRuns stays empty.
func TestReadRecording_LegacyFixtureMatchesGolden(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "legacy_v1_subagent.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rec := readRecordingFrom(t, body)
	got, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	goldenPath := filepath.Join("testdata", "legacy_v1_subagent.golden.json")
	if *updateGolden {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("legacy recording differs from golden:\n got: %s\nwant: %s", got, want)
	}
	if len(rec.SubRuns) != 0 {
		t.Errorf("SubRuns = %d, want 0 for a trace without run IDs", len(rec.SubRuns))
	}
}

// TestReadRecording_GroupsSubRuns pins that a parent with two
// interleaved children rebuilds three distinct histories: the parent's
// turns in Turns, and each child's turns, config, and outcome in its
// own SubRuns entry, in order of first appearance.
func TestReadRecording_GroupsSubRuns(t *testing.T) {
	const parent = "run-p"
	const childA = "run-p/sub-1"
	const childB = "run-p/sub-2"
	body := eventLines(t,
		map[string]any{"kind": "run_started", "schemaVersion": "2", "runId": parent, "config": types.RunConfig{RunID: parent, Mode: "execution"}},
		map[string]any{"kind": "subrun_started", "runId": childA, "parentRunId": parent, "config": types.RunConfig{RunID: childA, Mode: "research"}},
		turnLine(childA, parent, 0, "a0"),
		map[string]any{"kind": "subrun_started", "runId": childB, "parentRunId": parent, "config": types.RunConfig{RunID: childB, Mode: "review"}},
		turnLine(childB, parent, 0, "b0"),
		turnLine(childA, parent, 1, "a1"),
		map[string]any{"kind": "subrun_finished", "runId": childA, "parentRunId": parent, "subRun": types.SubRunSummary{RunID: childA, ParentRunID: parent, Outcome: "success", Turns: 2, TokenUsage: types.TokenUsage{Input: 10, Output: 2}, DurationMs: 50}},
		turnLine(childB, parent, 1, "b1"),
		map[string]any{"kind": "subrun_finished", "runId": childB, "parentRunId": parent, "subRun": types.SubRunSummary{RunID: childB, ParentRunID: parent, Outcome: "max_turns", Turns: 2}},
		turnLine(parent, "", 0, "p0"),
		turnLine(parent, "", 1, "p1"),
		map[string]any{"kind": "run_finished", "trace": types.RunTrace{ID: parent, Turns: 2, Outcome: "success"}},
	)

	rec := readRecordingFrom(t, body)
	if got := turnTexts(rec.Turns); !slices.Equal(got, []string{"p0", "p1"}) {
		t.Errorf("parent Turns = %v, want [p0 p1]", got)
	}
	for _, turn := range rec.Turns {
		if turn.RunID != parent || turn.ParentRunID != "" {
			t.Errorf("parent turn tags = (%q, %q), want (%q, \"\")", turn.RunID, turn.ParentRunID, parent)
		}
	}
	if len(rec.SubRuns) != 2 {
		t.Fatalf("SubRuns = %d, want 2", len(rec.SubRuns))
	}
	want := []struct {
		runID, mode, outcome string
		texts                []string
	}{
		{childA, "research", "success", []string{"a0", "a1"}},
		{childB, "review", "max_turns", []string{"b0", "b1"}},
	}
	for i, w := range want {
		sub := rec.SubRuns[i]
		if sub.RunID != w.runID || sub.ParentRunID != parent {
			t.Errorf("SubRuns[%d] ids = (%q, %q), want (%q, %q)", i, sub.RunID, sub.ParentRunID, w.runID, parent)
		}
		if sub.Config.Mode != w.mode {
			t.Errorf("SubRuns[%d].Config.Mode = %q, want %q", i, sub.Config.Mode, w.mode)
		}
		if got := turnTexts(sub.Turns); !slices.Equal(got, w.texts) {
			t.Errorf("SubRuns[%d] turns = %v, want %v", i, got, w.texts)
		}
		for _, turn := range sub.Turns {
			if turn.RunID != w.runID || turn.ParentRunID != parent {
				t.Errorf("SubRuns[%d] turn tags = (%q, %q), want (%q, %q)", i, turn.RunID, turn.ParentRunID, w.runID, parent)
			}
		}
		if sub.Summary == nil || sub.Summary.Outcome != w.outcome {
			t.Errorf("SubRuns[%d].Summary = %+v, want outcome %q", i, sub.Summary, w.outcome)
		}
	}
	if rec.SubRuns[0].Summary.TokenUsage.Input != 10 || rec.SubRuns[0].Summary.DurationMs != 50 {
		t.Errorf("SubRuns[0].Summary = %+v, want tokens and duration preserved", rec.SubRuns[0].Summary)
	}
}

// TestReadRecording_SubRunWithoutLifecycleEvents pins that child
// records are grouped by run ID even when the subrun_started line is
// missing, and that a child the trace never finished has a nil Summary.
func TestReadRecording_SubRunWithoutLifecycleEvents(t *testing.T) {
	body := eventLines(t,
		map[string]any{"kind": "run_started", "runId": "run-p"},
		turnLine("run-p/sub-1", "run-p", 0, "c0"),
		turnLine("run-p", "", 0, "p0"),
	)
	rec := readRecordingFrom(t, body)
	if got := turnTexts(rec.Turns); !slices.Equal(got, []string{"p0"}) {
		t.Errorf("parent Turns = %v, want [p0]", got)
	}
	if len(rec.SubRuns) != 1 || rec.SubRuns[0].RunID != "run-p/sub-1" {
		t.Fatalf("SubRuns = %+v, want one entry for run-p/sub-1", rec.SubRuns)
	}
	if rec.SubRuns[0].Summary != nil {
		t.Errorf("Summary = %+v, want nil for an unfinished child", rec.SubRuns[0].Summary)
	}
	if got := turnTexts(rec.SubRuns[0].Turns); !slices.Equal(got, []string{"c0"}) {
		t.Errorf("child turns = %v, want [c0]", got)
	}
}

// TestReadRecording_TopLevelRecordsStayInTurns pins that records with
// no parent run stay in Turns even when their run ID differs from the
// recording's: a file whose run_started line was lost, and a file
// holding a follow-up run after the primary, both keep every top-level
// turn.
func TestReadRecording_TopLevelRecordsStayInTurns(t *testing.T) {
	t.Run("missing run_started", func(t *testing.T) {
		body := eventLines(t,
			turnLine("run-p", "", 0, "p0"),
			turnLine("run-p/sub-1", "run-p", 0, "c0"),
			turnLine("run-p", "", 1, "p1"),
		)
		rec := readRecordingFrom(t, body)
		if got := turnTexts(rec.Turns); !slices.Equal(got, []string{"p0", "p1"}) {
			t.Errorf("Turns = %v, want [p0 p1]", got)
		}
		if len(rec.SubRuns) != 1 {
			t.Errorf("SubRuns = %d, want 1", len(rec.SubRuns))
		}
	})
	t.Run("follow-up run", func(t *testing.T) {
		body := eventLines(t,
			map[string]any{"kind": "run_started", "runId": "run-1"},
			turnLine("run-1", "", 0, "first"),
			map[string]any{"kind": "run_finished", "trace": types.RunTrace{ID: "run-1"}},
			map[string]any{"kind": "run_started", "runId": "run-2"},
			turnLine("run-2", "", 0, "second"),
		)
		rec := readRecordingFrom(t, body)
		if got := turnTexts(rec.Turns); !slices.Equal(got, []string{"first", "second"}) {
			t.Errorf("Turns = %v, want [first second]", got)
		}
		if len(rec.SubRuns) != 0 {
			t.Errorf("SubRuns = %d, want 0", len(rec.SubRuns))
		}
	})
}

// TestReadRecording_SynthesisesSubRunCommandOutputs pins that a
// sub-agent's run_command call without a command_output_record gets a
// synthesised record naming the child run, not the parent.
func TestReadRecording_SynthesisesSubRunCommandOutputs(t *testing.T) {
	child := turnLine("run-p/sub-1", "run-p", 0, "c0")
	child["toolCalls"] = []types.ToolCallRecord{{ID: "call-1", Name: "run_command", Output: "ok", Success: true}}
	body := eventLines(t,
		map[string]any{"kind": "run_started", "runId": "run-p"},
		child,
	)
	rec := readRecordingFrom(t, body)
	if len(rec.CommandOutputs) != 1 {
		t.Fatalf("CommandOutputs = %d, want 1", len(rec.CommandOutputs))
	}
	got := rec.CommandOutputs[0]
	if got.RunID != "run-p/sub-1" || got.ParentRunID != "run-p" || got.ToolUseID != "call-1" {
		t.Errorf("synthesised record = (%q, %q, %q), want (run-p/sub-1, run-p, call-1)", got.RunID, got.ParentRunID, got.ToolUseID)
	}
}

// TestReader_NextSkipsSubRunFinished pins that Next yields only the
// recorded run's run_finished trace, never a child's outcome.
func TestReader_NextSkipsSubRunFinished(t *testing.T) {
	body := eventLines(t,
		map[string]any{"kind": "run_started", "runId": "run-p"},
		map[string]any{"kind": "subrun_finished", "runId": "run-p/sub-1", "parentRunId": "run-p", "subRun": types.SubRunSummary{RunID: "run-p/sub-1", Outcome: "error"}},
		map[string]any{"kind": "run_finished", "trace": types.RunTrace{ID: "run-p", Outcome: "success"}},
	)
	got, err := NewReader(bytes.NewReader(body), WithLogger(discardLogger())).All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || got[0].ID != "run-p" {
		t.Fatalf("All = %+v, want only the parent trace", got)
	}
}
