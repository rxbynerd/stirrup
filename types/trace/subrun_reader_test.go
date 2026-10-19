package trace

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

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
	require.NoError(t, err)
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
	require.NoError(t, err)
	rec := readRecordingFrom(t, body)
	got, err := json.MarshalIndent(rec, "", "  ")
	require.NoError(t, err)
	got = append(got, '\n')

	goldenPath := filepath.Join("testdata", "legacy_v1_subagent.golden.json")
	if *updateGolden {
		require.NoError(t, os.WriteFile(goldenPath, got, 0o644))
	}
	want, err := os.ReadFile(goldenPath)
	require.NoError(t, err)
	require.Equal(t, string(want), string(got), "legacy recording differs from %s", goldenPath)
	require.Empty(t, rec.SubRuns, "a trace without run IDs has no sub-runs")
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
	require.Equal(t, []string{"p0", "p1"}, turnTexts(rec.Turns))
	for i, turn := range rec.Turns {
		require.Equal(t, parent, turn.RunID, "Turns[%d] run ID", i)
		require.Empty(t, turn.ParentRunID, "Turns[%d] parent run ID", i)
	}

	require.Len(t, rec.SubRuns, 2)
	want := []struct {
		name, runID, mode, outcome string
		texts                      []string
	}{
		{"research sub-run", childA, "research", "success", []string{"a0", "a1"}},
		{"review sub-run", childB, "review", "max_turns", []string{"b0", "b1"}},
	}
	for i, w := range want {
		t.Run(w.name, func(t *testing.T) {
			sub := rec.SubRuns[i]
			require.Equal(t, w.runID, sub.RunID)
			require.Equal(t, parent, sub.ParentRunID)
			require.Equal(t, w.mode, sub.Config.Mode)
			require.Equal(t, w.texts, turnTexts(sub.Turns))
			for _, turn := range sub.Turns {
				require.Equal(t, w.runID, turn.RunID)
				require.Equal(t, parent, turn.ParentRunID)
			}
			require.NotNil(t, sub.Summary)
			require.Equal(t, w.outcome, sub.Summary.Outcome)
		})
	}

	require.Equal(t, 10, rec.SubRuns[0].Summary.TokenUsage.Input)
	require.EqualValues(t, 50, rec.SubRuns[0].Summary.DurationMs)
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
	require.Equal(t, []string{"p0"}, turnTexts(rec.Turns))
	require.Len(t, rec.SubRuns, 1)
	require.Equal(t, "run-p/sub-1", rec.SubRuns[0].RunID)
	require.Nil(t, rec.SubRuns[0].Summary, "an unfinished child has no summary")
	require.Equal(t, []string{"c0"}, turnTexts(rec.SubRuns[0].Turns))
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
		require.Equal(t, []string{"p0", "p1"}, turnTexts(rec.Turns))
		require.Len(t, rec.SubRuns, 1)
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
		require.Equal(t, []string{"first", "second"}, turnTexts(rec.Turns))
		require.Empty(t, rec.SubRuns)
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
	require.Len(t, rec.CommandOutputs, 1)
	got := rec.CommandOutputs[0]
	require.Equal(t, "run-p/sub-1", got.RunID)
	require.Equal(t, "run-p", got.ParentRunID)
	require.Equal(t, "call-1", got.ToolUseID)
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
	require.NoError(t, err)
	require.Len(t, got, 1, "only the parent trace is yielded")
	require.Equal(t, "run-p", got[0].ID)
}
