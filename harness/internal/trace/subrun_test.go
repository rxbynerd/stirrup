package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
	emitter.RecordHookExecution(types.HookExecution{Phase: "preRun"})
	emitter.RecordCommandOutput(types.CommandOutputRecord{RunID: "run-p/sub-1", ParentRunID: "run-p", ToolUseID: "fwd"})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	events := readEvents(t, buf.Bytes())
	type tags struct{ runID, parentRunID string }
	check := func(kind EventKind, want []tags) {
		t.Helper()
		got := eventsOfKind(events, kind)
		if len(got) != len(want) {
			t.Fatalf("%s: got %d events, want %d", kind, len(got), len(want))
		}
		for i, ev := range got {
			if ev.RunID != want[i].runID || ev.ParentRunID != want[i].parentRunID {
				t.Errorf("%s[%d] tags = (%q, %q), want (%q, %q)", kind, i, ev.RunID, ev.ParentRunID, want[i].runID, want[i].parentRunID)
			}
		}
	}
	own := tags{"run-p", ""}
	child := tags{"run-p/sub-1", "run-p"}
	check(EventKindTurnRecord, []tags{own, child})
	check(EventKindToolCallRecord, []tags{own, child})
	check(EventKindHookRecord, []tags{own})
	check(EventKindCommandOutputRecord, []tags{child})
}

func TestJSONLTraceEmitter_SubRunLines(t *testing.T) {
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

			if got := strings.Contains(buf.String(), "secret://CHILD_KEY"); got != tc.wantRawKey {
				t.Errorf("raw child key on disk = %v, want %v:\n%s", got, tc.wantRawKey, buf.String())
			}
			events := readEvents(t, buf.Bytes())
			started := pickEvent(t, events, EventKindSubRunStarted)
			if started.RunID != "run-p/sub-1" || started.ParentRunID != "run-p" {
				t.Errorf("subrun_started ids = (%q, %q)", started.RunID, started.ParentRunID)
			}
			if started.Config == nil || started.Config.Mode != "research" {
				t.Errorf("subrun_started config = %+v, want the child's config", started.Config)
			}
			if started.StartedAt == nil || !started.StartedAt.Equal(startedAt) {
				t.Errorf("subrun_started startedAt = %v, want %v", started.StartedAt, startedAt)
			}
			finished := pickEvent(t, events, EventKindSubRunFinished)
			if finished.RunID != "run-p/sub-1" || finished.ParentRunID != "run-p" {
				t.Errorf("subrun_finished ids = (%q, %q)", finished.RunID, finished.ParentRunID)
			}
			if finished.SubRun == nil || *finished.SubRun != summary {
				t.Errorf("subrun_finished summary = %+v, want %+v", finished.SubRun, summary)
			}
			if finished.Trace != nil {
				t.Errorf("subrun_finished carries a full trace; want the summary only")
			}
		})
	}
}

func TestNestedJSONLEmitter_ReportsSubRunLifecycle(t *testing.T) {
	root := &subRunRecordingEmitter{}
	child := NewNestedJSONLEmitter(root, "run-p")
	config := &types.RunConfig{Mode: "research"}
	child.Start("run-p/sub-1", config)
	child.RecordTurn(types.TurnTrace{Turn: 0, Tokens: types.TokenUsage{Input: 7, Output: 3}})
	child.RecordTurn(types.TurnTrace{Turn: 1, Tokens: types.TokenUsage{Input: 5, Output: 1}})

	grandchild := NewNestedJSONLEmitter(child, "run-p/sub-1")
	grandchild.Start("run-p/sub-1/sub-1", nil)
	if _, err := grandchild.Finish(context.Background(), "error"); err != nil {
		t.Fatalf("grandchild Finish: %v", err)
	}
	runTrace, err := child.Finish(context.Background(), "success")
	if err != nil {
		t.Fatalf("child Finish: %v", err)
	}

	if len(root.starts) != 0 || len(root.finishes) != 0 {
		t.Errorf("parent Start/Finish called: starts=%d finishes=%d", len(root.starts), len(root.finishes))
	}
	wantStarts := []string{"run-p/sub-1|run-p", "run-p/sub-1/sub-1|run-p/sub-1"}
	var gotStarts []string
	for _, s := range root.subStarts {
		gotStarts = append(gotStarts, s.runID+"|"+s.parentRunID)
	}
	if !slices.Equal(gotStarts, wantStarts) {
		t.Errorf("sub-run starts = %v, want %v", gotStarts, wantStarts)
	}
	if root.subStarts[0].config != config {
		t.Errorf("sub-run start config not forwarded")
	}
	if len(root.subFinishes) != 2 {
		t.Fatalf("sub-run finishes = %d, want 2", len(root.subFinishes))
	}
	if g := root.subFinishes[0]; g.RunID != "run-p/sub-1/sub-1" || g.ParentRunID != "run-p/sub-1" || g.Outcome != "error" {
		t.Errorf("grandchild summary = %+v", g)
	}
	got := root.subFinishes[1]
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
	if got != want {
		t.Errorf("child summary = %+v, want %+v", got, want)
	}
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

	childA := NewNestedJSONLEmitter(parent, "run-p")
	childA.Start("run-p/sub-1", &types.RunConfig{RunID: "run-p/sub-1", Mode: "research"})
	turn(childA, 0, "a0", "a-call-0")
	childB := NewNestedJSONLEmitter(parent, "run-p")
	childB.Start("run-p/sub-2", &types.RunConfig{RunID: "run-p/sub-2", Mode: "review"})
	turn(childB, 0, "b0", "b-call-0")
	turn(childA, 1, "a1", "a-call-1")
	if _, err := childA.Finish(context.Background(), "success"); err != nil {
		t.Fatal(err)
	}
	turn(parent, 0, "p0", "p-call-0")
	turn(childB, 1, "b1", "b-call-1")
	if _, err := childB.Finish(context.Background(), "max_turns"); err != nil {
		t.Fatal(err)
	}
	turn(parent, 1, "p1", "p-call-1")
	if _, err := parent.Finish(context.Background(), "success"); err != nil {
		t.Fatal(err)
	}

	events := readEvents(t, buf.Bytes())
	if v := pickEvent(t, events, EventKindRunStarted).SchemaVersion; v != "2" {
		t.Errorf("schemaVersion = %q, want 2", v)
	}
	for _, kind := range []EventKind{EventKindTurnRecord, EventKindToolCallRecord} {
		for _, ev := range eventsOfKind(events, kind) {
			if ev.RunID == "" {
				t.Errorf("%s line without runId: %+v", kind, ev)
			}
			if (ev.RunID == "run-p") != (ev.ParentRunID == "") {
				t.Errorf("%s tags = (%q, %q): only the parent's lines may omit parentRunId", kind, ev.RunID, ev.ParentRunID)
			}
		}
	}

	rec, err := tracereader.NewReader(bytes.NewReader(buf.Bytes())).ReadRecording()
	if err != nil {
		t.Fatalf("ReadRecording: %v", err)
	}
	if rec.RunID != "run-p" || rec.FinalOutcome.Outcome != "success" {
		t.Errorf("recording = (%q, %q), want (run-p, success)", rec.RunID, rec.FinalOutcome.Outcome)
	}
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
	if !slices.Equal(texts, []string{"p0", "p1"}) || !slices.Equal(calls, []string{"p-call-0", "p-call-1"}) {
		t.Errorf("parent history = %v %v", texts, calls)
	}
	if len(rec.SubRuns) != 2 {
		t.Fatalf("SubRuns = %d, want 2", len(rec.SubRuns))
	}
	for i, want := range []struct {
		runID, mode, outcome string
		texts, calls         []string
	}{
		{"run-p/sub-1", "research", "success", []string{"a0", "a1"}, []string{"a-call-0", "a-call-1"}},
		{"run-p/sub-2", "review", "max_turns", []string{"b0", "b1"}, []string{"b-call-0", "b-call-1"}},
	} {
		sub := rec.SubRuns[i]
		texts, calls := history(sub.Turns)
		if sub.RunID != want.runID || sub.ParentRunID != "run-p" || sub.Config.Mode != want.mode {
			t.Errorf("SubRuns[%d] = (%q, %q, %q), want (%q, run-p, %q)", i, sub.RunID, sub.ParentRunID, sub.Config.Mode, want.runID, want.mode)
		}
		if !slices.Equal(texts, want.texts) || !slices.Equal(calls, want.calls) {
			t.Errorf("SubRuns[%d] history = %v %v, want %v %v", i, texts, calls, want.texts, want.calls)
		}
		if sub.Summary == nil || sub.Summary.Outcome != want.outcome || sub.Summary.Turns != 2 || sub.Summary.TokenUsage.Input != 20 {
			t.Errorf("SubRuns[%d].Summary = %+v, want outcome %q, 2 turns, 20 input tokens", i, sub.Summary, want.outcome)
		}
	}

	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"subRuns":[`) {
		t.Errorf("recording JSON has no subRuns field: %s", raw)
	}
}
