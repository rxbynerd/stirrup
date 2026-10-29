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
	must := require.New(t)
	turns := []types.TurnRecord{
		textTurn("run-p/sub-1", "run-p", "child 0"),
		textTurn("run-p", "", "parent 0"),
		textTurn("run-p/sub-1", "run-p", "child 1"),
		textTurn("run-p", "", "parent 1"),
	}
	got := replayTexts(t, NewReplayProvider(turns))
	want := []string{"parent 0", "parent 1"}
	must.Equal(want, got)
}

func TestReplayProvider_ReplaysOneSubRunsTurns(t *testing.T) {
	must := require.New(t)
	turns := []types.TurnRecord{
		textTurn("run-p/sub-1", "run-p", "child 0"),
		textTurn("run-p/sub-1", "run-p", "child 1"),
	}
	got := replayTexts(t, NewReplayProvider(turns))
	want := []string{"child 0", "child 1"}
	must.Equal(want, got)
}

func TestReplayProvider_ReplaysEveryTopLevelRun(t *testing.T) {
	must := require.New(t)
	turns := []types.TurnRecord{
		textTurn("run-a", "", "a0"),
		textTurn("run-a/sub-1", "run-a", "child"),
		textTurn("run-a", "", "a1"),
		textTurn("run-b", "", "b0"),
	}
	got := replayTexts(t, NewReplayProvider(turns))
	want := []string{"a0", "a1", "b0"}
	must.Equal(want, got)
}

func TestReplayProvider_UntaggedTurnsReplayInOrder(t *testing.T) {
	must := require.New(t)
	turns := []types.TurnRecord{
		textTurn("", "", "first"),
		textTurn("", "", "second"),
	}
	got := replayTexts(t, NewReplayProvider(turns))
	want := []string{"first", "second"}
	must.Equal(want, got)
}

// TestReplayProvider_RecordingWithSubAgentsReplaysParentOnly writes a
// parent and an interleaved sub-agent through the JSONL emitter, reads
// the recording back, and replays its Turns: only the parent's outputs
// may come back.
func TestReplayProvider_RecordingWithSubAgentsReplaysParentOnly(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	record(parent, 1, "parent 1")
	_, err = parent.Finish(context.Background(), "success")
	must.NoError(err)

	rec, err := tracereader.NewReader(bytes.NewReader(buf.Bytes())).ReadRecording()
	must.NoError(err, "ReadRecording")
	got := replayTexts(t, NewReplayProvider(rec.Turns))
	want := []string{"parent 0", "parent 1"}
	must.Equal(want, got)
}

// TestReplayProvider_ForSubRunServesTheSpawningRunsChild pins that a child
// is matched by its run ID relative to its parent, resolved against the
// recorded run of the turn served last, so a follow-up run's first child
// gets that run's sub-1 rather than the first run's.
func TestReplayProvider_ForSubRunServesTheSpawningRunsChild(t *testing.T) {
	must := require.New(t)
	rp := NewReplayProviderFromRecording(&types.RunRecording{
		Turns: []types.TurnRecord{textTurn("run-a", "", "a0"), textTurn("run-b", "", "b0")},
		SubRuns: []types.SubRunRecording{
			{RunID: "run-a/sub-1", Turns: []types.TurnRecord{textTurn("run-a/sub-1", "run-a", "a child")}},
			{RunID: "run-b/sub-1", Turns: []types.TurnRecord{textTurn("run-b/sub-1", "run-b", "b child")}},
		},
	})

	must.Equal([]string{"a0"}, nextTexts(t, rp))
	child, err := rp.ForSubRun("live-1", "live-1/sub-1")
	must.NoError(err)
	must.Equal([]string{"a child"}, replayTexts(t, child))

	must.Equal([]string{"b0"}, nextTexts(t, rp))
	child, err = rp.ForSubRun("live-2", "live-2/sub-1")
	must.NoError(err)
	must.Equal([]string{"b child"}, replayTexts(t, child))
}

// TestReplayProvider_ForSubRunWithoutRecordedTurnsFails pins that a child
// the recording holds no turns for gets an error naming it, and a
// provider that fails with the same error instead of serving anything.
func TestReplayProvider_ForSubRunWithoutRecordedTurnsFails(t *testing.T) {
	must := require.New(t)
	rp := NewReplayProviderFromRecording(&types.RunRecording{
		Turns: []types.TurnRecord{textTurn("run-a", "", "a0"), textTurn("run-a", "", "a1")},
	})
	_ = nextTexts(t, rp)

	child, err := rp.ForSubRun("live-1", "live-1/sub-1")
	must.ErrorContains(err, `no recorded turns for sub-agent "live-1/sub-1" (recorded run "run-a/sub-1")`)
	_, streamErr := child.Stream(context.Background(), types.StreamParams{})
	must.Equal(err, streamErr)
	must.Equal([]string{"a1"}, replayTexts(t, rp), "the parent's next turn is untouched")
}

// TestReplayProvider_RecordedSubRunRoundTrip writes a parent and a child
// through the JSONL emitters, reads the file with ReadReplay, and pins
// that the parent replays its own turns with their recorded stop reasons
// while the child replays its own.
func TestReplayProvider_RecordedSubRunRoundTrip(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	parent := trace.NewJSONLTraceEmitter(&buf, false)
	parent.Start("run-p", nil)
	child := trace.NewNestedJSONLEmitter(parent, "run-p", context.Background())
	child.Start("run-p/sub-1", nil)

	record := func(e trace.TraceEmitter, turn int, text, stopReason string) {
		e.RecordTurnRecord(types.TurnRecord{Turn: turn, StopReason: stopReason, ModelOutput: []types.ContentBlock{{Type: "text", Text: text}}})
	}
	record(parent, 0, "parent 0", "max_tokens")
	record(child, 0, "child 0", "end_turn")
	_, err := child.Finish(context.Background(), "success")
	must.NoError(err)
	record(parent, 1, "parent 1", "end_turn")
	_, err = parent.Finish(context.Background(), "success")
	must.NoError(err)

	rec, err := tracereader.NewReader(bytes.NewReader(buf.Bytes())).ReadReplay(context.Background())
	must.NoError(err)
	rp := NewReplayProviderFromRecording(rec)

	events := replayEvents(t, rp)
	must.Equal("max_tokens", events[len(events)-1].StopReason)
	sub, err := rp.ForSubRun("live", "live/sub-1")
	must.NoError(err)
	must.Equal([]string{"child 0"}, replayTexts(t, sub))
	must.Equal([]string{"parent 1"}, replayTexts(t, rp))
}

// nextTexts streams rp's next turn and returns its text.
func nextTexts(t *testing.T, rp *ReplayProvider) []string {
	t.Helper()
	must := require.New(t)
	ch, err := rp.Stream(context.Background(), types.StreamParams{})
	must.NoError(err)
	var texts []string
	for _, ev := range collectEvents(t, ch) {
		if ev.Type == "text_delta" {
			texts = append(texts, ev.Text)
		}
	}
	return texts
}
