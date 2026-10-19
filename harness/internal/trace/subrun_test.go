package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
	tracereader "github.com/rxbynerd/stirrup/types/trace"
)

// subRunRecordingEmitter is a recordingEmitter that also captures
// SubRunRecorder calls.
type subRunRecordingEmitter struct {
	recordingEmitter
	subMu       sync.Mutex
	subStarts   []subRunStart
	subFinishes []types.SubRunSummary
}

type subRunStart struct {
	runID, parentRunID string
	startedAt          time.Time
	config             *types.RunConfig
}

func (r *subRunRecordingEmitter) RecordSubRunStarted(runID, parentRunID string, startedAt time.Time, config *types.RunConfig) {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	r.subStarts = append(r.subStarts, subRunStart{runID, parentRunID, startedAt, config})
}

func (r *subRunRecordingEmitter) RecordSubRunFinished(summary types.SubRunSummary) {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	r.subFinishes = append(r.subFinishes, summary)
}

func eventsOfKind(events []Event, kind EventKind) []Event {
	var out []Event
	for _, ev := range events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func TestJSONLTraceEmitter_TagsPerRunRecords(t *testing.T) {
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)
	emitter.Start("run-p", nil)

	emitter.RecordTurnRecord(types.TurnRecord{Turn: 0})
	emitter.RecordTurnRecord(types.TurnRecord{Turn: 0, RunID: "run-p/sub-1", ParentRunID: "run-p"})
	emitter.RecordToolCall(types.ToolCallTrace{ID: "own", Name: "read_file"})
	emitter.RecordToolCall(types.ToolCallTrace{ID: "fwd", Name: "read_file", RunID: "run-p/sub-1", ParentRunID: "run-p"})
	emitter.RecordHookExecution(context.Background(), types.HookExecution{Phase: "preRun"})
	emitter.RecordCommandOutput(types.CommandOutputRecord{RunID: "run-p/sub-1", ParentRunID: "run-p", ToolUseID: "fwd"})
	_, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)

	events := readEvents(t, buf.Bytes())
	type tags struct{ runID, parentRunID string }
	check := func(kind EventKind, want []tags) {
		t.Helper()
		got := eventsOfKind(events, kind)
		require.Len(t, got, len(want), "%s events", kind)
		for i, ev := range got {
			gotTags := tags{ev.RunID, ev.ParentRunID}
			require.Equal(t, want[i], gotTags, "%s[%d]", kind, i)
		}
	}
	own := tags{"run-p", ""}
	child := tags{"run-p/sub-1", "run-p"}
	check(EventKindTurnRecord, []tags{own, child})
	check(EventKindToolCallRecord, []tags{own, child})
	check(EventKindHookRecord, []tags{own})
	check(EventKindCommandOutputRecord, []tags{child})
}

func TestJSONLTraceEmitter_SubRunEvents_ShapeAndRedaction(t *testing.T) {
	childConfig := &types.RunConfig{
		RunID:    "run-p/sub-1",
		Mode:     "research",
		Provider: types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://CHILD_KEY"},
	}
	startedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	summary := types.SubRunSummary{
		RunID:       "run-p/sub-1",
		ParentRunID: "run-p",
		Outcome:     "success",
		Turns:       3,
		TokenUsage:  types.TokenUsage{Input: 30, Output: 6},
		StartedAt:   startedAt,
		CompletedAt: startedAt.Add(2 * time.Second),
		DurationMs:  2000,
	}

	for _, tc := range []struct {
		name              string
		redactionDisabled bool
		wantRawKey        bool
	}{
		{"redacted", false, false},
		{"debug build", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			emitter := NewJSONLTraceEmitter(&buf, tc.redactionDisabled)
			emitter.Start("run-p", nil)
			emitter.RecordSubRunStarted("run-p/sub-1", "run-p", startedAt, childConfig)
			emitter.RecordSubRunFinished(summary)

			require.Equal(t, tc.wantRawKey, strings.Contains(buf.String(), "secret://CHILD_KEY"),
				"raw child key on disk:\n%s", buf.String())

			events := readEvents(t, buf.Bytes())
			started := pickEvent(t, events, EventKindSubRunStarted)
			require.Equal(t, "run-p/sub-1", started.RunID)
			require.Equal(t, "run-p", started.ParentRunID)
			require.NotNil(t, started.Config, "subrun_started should carry the child's config")
			require.Equal(t, "research", started.Config.Mode)
			require.NotNil(t, started.StartedAt)
			require.True(t, started.StartedAt.Equal(startedAt), "subrun_started startedAt = %v, want %v", *started.StartedAt, startedAt)

			finished := pickEvent(t, events, EventKindSubRunFinished)
			require.Equal(t, "run-p/sub-1", finished.RunID)
			require.Equal(t, "run-p", finished.ParentRunID)
			require.NotNil(t, finished.SubRun)
			require.Equal(t, summary, *finished.SubRun)
			require.Nil(t, finished.Trace, "subrun_finished should carry the summary only")
		})
	}
}

func TestNestedJSONLEmitter_ReportsSubRunLifecycle(t *testing.T) {
	root := &subRunRecordingEmitter{}
	child := NewNestedJSONLEmitter(root, "run-p", context.Background())
	config := &types.RunConfig{Mode: "research"}
	child.Start("run-p/sub-1", config)
	child.RecordTurn(types.TurnTrace{Turn: 0, Tokens: types.TokenUsage{Input: 7, Output: 3}})
	child.RecordTurn(types.TurnTrace{Turn: 1, Tokens: types.TokenUsage{Input: 5, Output: 1}})

	grandchild := NewNestedJSONLEmitter(child, "run-p/sub-1", context.Background())
	grandchild.Start("run-p/sub-1/sub-1", nil)
	_, err := grandchild.Finish(context.Background(), "error")
	require.NoError(t, err, "grandchild Finish")
	runTrace, err := child.Finish(context.Background(), "success")
	require.NoError(t, err, "child Finish")

	require.Empty(t, root.starts, "parent Start should not be called")
	require.Empty(t, root.finishes, "parent Finish should not be called")

	var gotStarts []string
	for _, s := range root.subStarts {
		gotStarts = append(gotStarts, s.runID+"|"+s.parentRunID)
	}
	require.Equal(t, []string{"run-p/sub-1|run-p", "run-p/sub-1/sub-1|run-p/sub-1"}, gotStarts)
	require.Same(t, config, root.subStarts[0].config, "sub-run start should forward the child config")

	require.Len(t, root.subFinishes, 2)
	grandchildSummary := root.subFinishes[0]
	require.Equal(t, "run-p/sub-1/sub-1", grandchildSummary.RunID)
	require.Equal(t, "run-p/sub-1", grandchildSummary.ParentRunID)
	require.Equal(t, "error", grandchildSummary.Outcome)

	want := types.SubRunSummary{
		RunID:       "run-p/sub-1",
		ParentRunID: "run-p",
		Outcome:     "success",
		Turns:       2,
		TokenUsage:  types.TokenUsage{Input: 12, Output: 4},
		StartedAt:   runTrace.StartedAt,
		CompletedAt: runTrace.CompletedAt,
		DurationMs:  runTrace.CompletedAt.Sub(runTrace.StartedAt).Milliseconds(),
	}
	require.Equal(t, want, root.subFinishes[1], "child summary")
}

// TestNestedJSONLEmitter_InterleavedChildrenRoundTrip drives a parent
// JSONL emitter and two interleaved sub-agents through the real writer
// and reader, and checks that every per-run line is tagged and that the
// reader rebuilds three distinct histories.
func TestNestedJSONLEmitter_InterleavedChildrenRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	parent := NewJSONLTraceEmitter(&buf, false)
	parent.Start("run-p", &types.RunConfig{RunID: "run-p", Mode: "execution"})

	turn := func(e TraceEmitter, n int, text, callID string) {
		e.RecordTurn(types.TurnTrace{Turn: n, Tokens: types.TokenUsage{Input: 10, Output: 1}})
		e.RecordToolCall(types.ToolCallTrace{ID: callID, Name: "read_file", Success: true})
		e.RecordTurnRecord(types.TurnRecord{
			Turn:        n,
			ModelOutput: []types.ContentBlock{{Type: "text", Text: text}},
			ToolCalls:   []types.ToolCallRecord{{ID: callID, Name: "read_file", Output: text + "-out", Success: true}},
		})
	}

	childA := NewNestedJSONLEmitter(parent, "run-p", context.Background())
	childA.Start("run-p/sub-1", &types.RunConfig{RunID: "run-p/sub-1", Mode: "research"})
	turn(childA, 0, "a0", "a-call-0")
	childB := NewNestedJSONLEmitter(parent, "run-p", context.Background())
	childB.Start("run-p/sub-2", &types.RunConfig{RunID: "run-p/sub-2", Mode: "review"})
	turn(childB, 0, "b0", "b-call-0")
	turn(childA, 1, "a1", "a-call-1")
	_, err := childA.Finish(context.Background(), "success")
	require.NoError(t, err)
	turn(parent, 0, "p0", "p-call-0")
	turn(childB, 1, "b1", "b-call-1")
	_, err = childB.Finish(context.Background(), "max_turns")
	require.NoError(t, err)
	turn(parent, 1, "p1", "p-call-1")
	_, err = parent.Finish(context.Background(), "success")
	require.NoError(t, err)

	events := readEvents(t, buf.Bytes())
	require.Equal(t, "2", pickEvent(t, events, EventKindRunStarted).SchemaVersion)
	for _, kind := range []EventKind{EventKindTurnRecord, EventKindToolCallRecord} {
		for _, ev := range eventsOfKind(events, kind) {
			require.NotEmpty(t, ev.RunID, "%s line without runId: %+v", kind, ev)
			if ev.RunID == "run-p" {
				require.Empty(t, ev.ParentRunID, "parent %s line should omit parentRunId", kind)
			} else {
				require.NotEmpty(t, ev.ParentRunID, "sub-run %s line should carry parentRunId", kind)
			}
		}
	}

	rec, err := tracereader.NewReader(bytes.NewReader(buf.Bytes())).ReadRecording()
	require.NoError(t, err)
	require.Equal(t, "run-p", rec.RunID)
	require.Equal(t, "success", rec.FinalOutcome.Outcome)

	history := func(turns []types.TurnRecord) (texts, calls []string) {
		for _, tr := range turns {
			texts = append(texts, tr.ModelOutput[0].Text)
			for _, call := range tr.ToolCalls {
				calls = append(calls, call.ID)
			}
		}
		return texts, calls
	}
	texts, calls := history(rec.Turns)
	require.Equal(t, []string{"p0", "p1"}, texts, "parent turn texts")
	require.Equal(t, []string{"p-call-0", "p-call-1"}, calls, "parent tool call ids")

	require.Len(t, rec.SubRuns, 2)
	for i, want := range []struct {
		name, runID, mode, outcome string
		texts, calls               []string
	}{
		{"research sub-run", "run-p/sub-1", "research", "success", []string{"a0", "a1"}, []string{"a-call-0", "a-call-1"}},
		{"review sub-run", "run-p/sub-2", "review", "max_turns", []string{"b0", "b1"}, []string{"b-call-0", "b-call-1"}},
	} {
		t.Run(want.name, func(t *testing.T) {
			sub := rec.SubRuns[i]
			require.Equal(t, want.runID, sub.RunID)
			require.Equal(t, "run-p", sub.ParentRunID)
			require.NotNil(t, sub.Config)
			require.Equal(t, want.mode, sub.Config.Mode)

			subTexts, subCalls := history(sub.Turns)
			require.Equal(t, want.texts, subTexts)
			require.Equal(t, want.calls, subCalls)

			require.NotNil(t, sub.Summary)
			require.Equal(t, want.outcome, sub.Summary.Outcome)
			require.Equal(t, 2, sub.Summary.Turns)
			require.Equal(t, 20, sub.Summary.TokenUsage.Input)
		})
	}

	raw, err := json.Marshal(rec)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"subRuns":[`, "recording JSON has no subRuns field: %s", raw)
}

func TestJSONLTraceEmitter_ConfigPromptIsScrubbed(t *testing.T) {
	const (
		secret = "sk-ant-api03-promptleak0123456789abcdef"
		prompt = "summarise the file; it said key "
	)
	config := &types.RunConfig{Mode: "research", Prompt: prompt + secret}

	for _, tc := range []struct {
		name              string
		redactionDisabled bool
		wantSecret        bool
	}{
		{"redacted", false, false},
		{"debug build", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			parent := NewJSONLTraceEmitter(&buf, tc.redactionDisabled)
			parent.Start("run-p", config)
			child := NewNestedJSONLEmitter(parent, "run-p", context.Background())
			child.Start("run-p/sub-1", config)
			_, err := child.Finish(context.Background(), "success")
			require.NoError(t, err, "child Finish")

			require.Equal(t, tc.wantSecret, strings.Contains(buf.String(), secret),
				"secret on disk:\n%s", buf.String())

			events := readEvents(t, buf.Bytes())
			for _, kind := range []EventKind{EventKindRunStarted, EventKindSubRunStarted} {
				started := pickEvent(t, events, kind)
				require.NotNil(t, started.Config, "%s config", kind)
				require.True(t, strings.HasPrefix(started.Config.Prompt, prompt),
					"%s prompt should keep its text apart from the secret, got %q", kind, started.Config.Prompt)
			}
		})
	}
}
