package provider

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
	tracereader "github.com/rxbynerd/stirrup/types/trace"
)

func textTurn(runID, parentRunID, text string) types.TurnRecord {
	return types.TurnRecord{
		RunID:       runID,
		ParentRunID: parentRunID,
		ModelOutput: []types.ContentBlock{{Type: "text", Text: text}},
	}
}

// replayTexts streams every turn rp holds and returns each turn's text.
func replayTexts(t *testing.T, rp *ReplayProvider) []string {
	t.Helper()
	var texts []string
	for {
		ch, err := rp.Stream(context.Background(), types.StreamParams{})
		if err != nil {
			return texts
		}
		for _, ev := range collectEvents(t, ch) {
			if ev.Type == "text_delta" {
				texts = append(texts, ev.Text)
			}
		}
	}
}

func TestReplayProvider_SkipsSubAgentTurns(t *testing.T) {
	turns := []types.TurnRecord{
		textTurn("run-p/sub-1", "run-p", "child 0"),
		textTurn("run-p", "", "parent 0"),
		textTurn("run-p/sub-1", "run-p", "child 1"),
		textTurn("run-p", "", "parent 1"),
	}
	got := replayTexts(t, NewReplayProvider(turns))
	want := []string{"parent 0", "parent 1"}
	require.Equal(t, want, got)
}

func TestReplayProvider_ReplaysOneSubRunsTurns(t *testing.T) {
	turns := []types.TurnRecord{
		textTurn("run-p/sub-1", "run-p", "child 0"),
		textTurn("run-p/sub-1", "run-p", "child 1"),
	}
	got := replayTexts(t, NewReplayProvider(turns))
	want := []string{"child 0", "child 1"}
	require.Equal(t, want, got)
}

func TestReplayProvider_ReplaysEveryTopLevelRun(t *testing.T) {
	turns := []types.TurnRecord{
		textTurn("run-a", "", "a0"),
		textTurn("run-a/sub-1", "run-a", "child"),
		textTurn("run-a", "", "a1"),
		textTurn("run-b", "", "b0"),
	}
	got := replayTexts(t, NewReplayProvider(turns))
	want := []string{"a0", "a1", "b0"}
	require.Equal(t, want, got)
}

func TestReplayProvider_UntaggedTurnsReplayInOrder(t *testing.T) {
	turns := []types.TurnRecord{
		textTurn("", "", "first"),
		textTurn("", "", "second"),
	}
	got := replayTexts(t, NewReplayProvider(turns))
	want := []string{"first", "second"}
	require.Equal(t, want, got)
}

// TestReplayProvider_RecordingWithSubAgentsReplaysParentOnly writes a
// parent and an interleaved sub-agent through the JSONL emitter, reads
// the recording back, and replays its Turns: only the parent's outputs
// may come back.
func TestReplayProvider_RecordingWithSubAgentsReplaysParentOnly(t *testing.T) {
	var buf bytes.Buffer
	parent := trace.NewJSONLTraceEmitter(&buf, false)
	parent.Start("run-p", nil)
	child := trace.NewNestedJSONLEmitter(parent, "run-p", context.Background())
	child.Start("run-p/sub-1", nil)

	record := func(e trace.TraceEmitter, turn int, text string) {
		e.RecordTurnRecord(types.TurnRecord{Turn: turn, ModelOutput: []types.ContentBlock{{Type: "text", Text: text}}})
	}
	record(child, 0, "child 0")
	record(parent, 0, "parent 0")
	record(child, 1, "child 1")
	_, err := child.Finish(context.Background(), "success")
	require.NoError(t, err)
	record(parent, 1, "parent 1")
	_, err = parent.Finish(context.Background(), "success")
	require.NoError(t, err)

	rec, err := tracereader.NewReader(bytes.NewReader(buf.Bytes())).ReadRecording()
	require.NoError(t, err, "ReadRecording")
	got := replayTexts(t, NewReplayProvider(rec.Turns))
	want := []string{"parent 0", "parent 1"}
	require.Equal(t, want, got)
}
