package trace

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadReplay_KeepsOnlyWhatAReplayServes pins that the replay read
// groups turns into the run's own and each sub-run's like ReadRecording,
// keeps each turn's identity, stop reason and model output, and drops
// the run config, model input and tool calls.
func TestReadReplay_KeepsOnlyWhatAReplayServes(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	parentTurn := turnLine("run-p", "", 1, "parent")
	parentTurn["stopReason"] = "max_tokens"
	parentTurn["modelInput"] = map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "prompt"}}}}}
	parentTurn["toolCalls"] = []any{map[string]any{"id": "c1", "name": "read_file", "output": "contents"}}
	body := eventLines(t,
		map[string]any{"kind": "run_started", "runId": "run-p", "config": map[string]any{"mode": "execution"}},
		parentTurn,
		turnLine("run-p/sub-1", "run-p", 1, "child"),
		turnLine("run-p", "", 2, "parent again"),
	)

	rec, err := NewReader(bytes.NewReader(body), WithLogger(discardLogger())).ReadReplay(context.Background())
	must.NoError(err)

	check.Equal("run-p", rec.RunID)
	check.Empty(rec.Config.Mode, "the run config is not kept")
	must.Len(rec.Turns, 2)
	first := rec.Turns[0]
	check.Equal(1, first.Turn)
	check.Equal("run-p", first.RunID)
	check.Equal("max_tokens", first.StopReason)
	must.Len(first.ModelOutput, 1)
	check.Equal("parent", first.ModelOutput[0].Text)
	check.Empty(first.ModelInput.Messages, "model input is not kept")
	check.Empty(first.ToolCalls, "tool calls are not kept")
	check.Equal("parent again", rec.Turns[1].ModelOutput[0].Text)

	must.Len(rec.SubRuns, 1)
	check.Equal("run-p/sub-1", rec.SubRuns[0].RunID)
	check.Equal("run-p", rec.SubRuns[0].ParentRunID)
	must.Len(rec.SubRuns[0].Turns, 1)
	check.Equal("child", rec.SubRuns[0].Turns[0].ModelOutput[0].Text)
}

// TestReadReplay_CountsMalformedLines pins that a line which is not JSON,
// or whose kept fields do not decode, is counted rather than kept.
func TestReadReplay_CountsMalformedLines(t *testing.T) {
	must := require.New(t)
	body := append(eventLines(t, turnLine("", "", 1, "kept")), []byte("not json\n{\"kind\":\"turn_record\",\"modelOutput\":\"not a list\"}\n")...)
	reader := NewReader(bytes.NewReader(body), WithLogger(discardLogger()))

	rec, err := reader.ReadReplay(context.Background())

	must.NoError(err)
	must.Len(rec.Turns, 1)
	must.Equal(2, reader.MalformedLines())
}

// TestReadReplay_StopsOnCancelledContext pins that a cancelled context
// ends the read with its error.
func TestReadReplay_StopsOnCancelledContext(t *testing.T) {
	must := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := eventLines(t, turnLine("", "", 1, "never read"))

	_, err := NewReader(bytes.NewReader(body), WithLogger(discardLogger())).ReadReplay(ctx)

	must.ErrorIs(err, context.Canceled)
}

// TestReadReplay_RegistersStartedSubRunsWithTheirPrompt pins that a
// sub-run is registered from its subrun_started event, with its prompt,
// even when it recorded no turns.
func TestReadReplay_RegistersStartedSubRunsWithTheirPrompt(t *testing.T) {
	must := require.New(t)
	body := eventLines(t,
		map[string]any{"kind": "run_started", "runId": "run-p"},
		map[string]any{"kind": "subrun_started", "runId": "run-p/sub-1", "parentRunId": "run-p", "config": map[string]any{"prompt": "List files."}},
	)

	rec, err := NewReader(bytes.NewReader(body), WithLogger(discardLogger())).ReadReplay(context.Background())

	must.NoError(err)
	must.Len(rec.SubRuns, 1)
	must.Equal("run-p/sub-1", rec.SubRuns[0].RunID)
	must.Equal("run-p", rec.SubRuns[0].ParentRunID)
	must.Equal("List files.", rec.SubRuns[0].Config.Prompt)
	must.Empty(rec.SubRuns[0].Turns)
}

// TestReadReplay_RefusesTooManyModelOutputBlocks pins that a recording
// holding more model output blocks than the cap fails the read, since
// decoded blocks cost far more memory than their bytes on disk.
func TestReadReplay_RefusesTooManyModelOutputBlocks(t *testing.T) {
	must := require.New(t)
	blocks := make([]any, MaxReplayContentBlocks+1)
	for i := range blocks {
		blocks[i] = map[string]any{}
	}
	body := eventLines(t, map[string]any{"kind": "turn_record", "turn": 1, "modelOutput": blocks})

	_, err := NewReader(bytes.NewReader(body), WithLogger(discardLogger())).ReadReplay(context.Background())

	must.ErrorContains(err, "more than 131072 model output blocks")
}
