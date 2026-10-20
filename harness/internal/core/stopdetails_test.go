package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// stopDetailsRecordingEmitter extends recordingTraceEmitter with the
// StopDetailsRecorder capability so tests can see what the loop records.
type stopDetailsRecordingEmitter struct {
	recordingTraceEmitter
	stopDetails *types.StopDetails
}

var _ trace.StopDetailsRecorder = (*stopDetailsRecordingEmitter)(nil)

func (e *stopDetailsRecordingEmitter) RecordStopDetails(details *types.StopDetails) {
	e.stopDetails = details
}

func (e *stopDetailsRecordingEmitter) Finish(_ context.Context, outcome string) (*types.RunTrace, error) {
	return &types.RunTrace{Outcome: outcome, StopDetails: e.stopDetails}, nil
}

// scriptedVerifier returns the next verdict from passes on each call and
// passes once the script is exhausted.
type scriptedVerifier struct {
	mu     sync.Mutex
	passes []bool
}

func (v *scriptedVerifier) Verify(_ context.Context, _ verifier.VerifyContext) (*types.VerificationResult, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	passed := true
	if len(v.passes) > 0 {
		passed, v.passes = v.passes[0], v.passes[1:]
	}
	return &types.VerificationResult{Passed: passed, Feedback: "verification check failed"}, nil
}

func refusalEvents(details *types.StopDetails) []types.StreamEvent {
	return []types.StreamEvent{
		{Type: "text_delta", Text: "I can't help with that."},
		{Type: "message_complete", StopReason: "refusal", StopDetails: details},
	}
}

// observedLoop wires a JSON log buffer and an in-memory span exporter onto
// loop so tests can assert on log records and provider.stream attributes.
func observedLoop(t *testing.T, loop *AgenticLoop) (*bytes.Buffer, *tracetest.InMemoryExporter) {
	t.Helper()
	var logBuf bytes.Buffer
	loop.Logger = slog.New(slog.NewJSONHandler(&logBuf, nil))
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	loop.Tracer = tp.Tracer("test")
	return &logBuf, exporter
}

// logRecords returns the JSON log records in buf whose msg equals msg.
func logRecords(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err == nil && rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// providerStreamAttr returns the value of key on the last provider.stream
// span and whether it was set.
func providerStreamAttr(exporter *tracetest.InMemoryExporter, key string) (string, bool) {
	var value string
	var found bool
	for _, s := range exporter.GetSpans() {
		if s.Name != "provider.stream" {
			continue
		}
		for _, attr := range s.Attributes {
			if string(attr.Key) == key {
				value, found = attr.Value.AsString(), true
			}
		}
	}
	return value, found
}

func TestLoop_RefusalRecordsStopDetails(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	want := types.StopDetails{
		Type:        "refusal",
		Category:    "cyber",
		Explanation: "This request was declined because it conflicts with Anthropic's Usage Policy.",
	}
	provided := want
	loop := buildTestLoop(&mockProvider{events: refusalEvents(&provided)})

	var traceBuf bytes.Buffer
	loop.Trace = trace.NewJSONLTraceEmitter(&traceBuf, false)
	logBuf, exporter := observedLoop(t, loop)

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	check.Equal("refusal", runTrace.Outcome)
	check.Equal(&want, runTrace.StopDetails)

	var finished struct {
		Kind  string         `json:"kind"`
		Trace types.RunTrace `json:"trace"`
	}
	lines := strings.Split(strings.TrimSpace(traceBuf.String()), "\n")
	err = json.Unmarshal([]byte(lines[len(lines)-1]), &finished)
	must.NoError(err, "decode run_finished line")
	must.Equal("run_finished", finished.Kind)
	check.Equal(&want, finished.Trace.StopDetails)

	refusals := logRecords(t, logBuf, "provider refused to respond")
	must.Len(refusals, 1, "logs:\n%s", logBuf.String())
	rec := refusals[0]
	check.Equal("WARN", rec["level"])
	check.Equal("cyber", rec["category"])
	check.Equal(want.Explanation, rec["explanation"])

	category, _ := providerStreamAttr(exporter, "stop.category")
	check.Equal("cyber", category)
}

func TestLoop_RefusalWithoutCategory(t *testing.T) {
	cases := []struct {
		name            string
		details         *types.StopDetails
		wantExplanation string
	}{
		{name: "nil details", details: nil},
		{
			name:            "empty category",
			details:         &types.StopDetails{Type: "refusal", Explanation: "Declined."},
			wantExplanation: "Declined.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			loop := buildTestLoop(&mockProvider{events: refusalEvents(tc.details)})
			logBuf, exporter := observedLoop(t, loop)

			runTrace, err := loop.Run(context.Background(), buildTestConfig())
			must.NoError(err)
			must.Equal("refusal", runTrace.Outcome)

			refusals := logRecords(t, logBuf, "provider refused to respond")
			must.Len(refusals, 1, "logs:\n%s", logBuf.String())
			rec := refusals[0]
			must.Equal("WARN", rec["level"])
			must.Empty(rec["category"])
			must.Equal(tc.wantExplanation, rec["explanation"])

			value, ok := providerStreamAttr(exporter, "stop.category")
			must.False(ok, "stop.category = %q, want the attribute absent", value)
		})
	}
}

// TestLoop_ContextWindowExceededWithNothingToCompact pins that a
// context-window stop on a request the forced compaction cannot shrink
// ends the run as context_exhausted, with the stop's tokens charged and
// no stop details or refusal log.
func TestLoop_ContextWindowExceededWithNothingToCompact(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	loop := buildTestLoop(&mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "Partial answer"},
		{Type: "message_complete", StopReason: "model_context_window_exceeded", InputTokens: 120, OutputTokens: 30},
	}})
	logBuf, _ := observedLoop(t, loop)

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	check.Equal("context_exhausted", runTrace.Outcome)
	check.Equal(30, runTrace.TokenUsage.Output, "the overflowing response's output tokens")
	check.Nil(runTrace.StopDetails)
	check.Empty(logRecords(t, logBuf, "provider refused to respond"))
}

// TestLoop_StreamErrorAfterPartialText feeds the loop the event sequence
// the Anthropic adapter emits for an SSE error event that follows streamed
// text (pinned adapter-side by TestSSE_ErrorEventSurfacesErrorType).
func TestLoop_StreamErrorAfterPartialText(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(&mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "Looking at the"},
		{Type: "error", Error: errors.New("anthropic API stream error (overloaded_error): Overloaded")},
	}})
	logBuf, _ := observedLoop(t, loop)

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	check.Equal("error", runTrace.Outcome)
	check.Nil(runTrace.StopDetails)

	failures := logRecords(t, logBuf, "provider stream failed")
	must.Len(failures, 1, "logs:\n%s", logBuf.String())
	msg, _ := failures[0]["error"].(string)
	check.Contains(msg, "overloaded_error", "logged error should carry the provider error type")
	check.Empty(logRecords(t, logBuf, "provider returned empty stop reason"))
}

func TestLoop_StopDetailsOnTurnTrace(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	want := types.StopDetails{Type: "refusal", Category: "bio"}
	provided := want
	loop := buildTestLoop(nil)
	loop.Provider = &multiCallProvider{calls: [][]types.StreamEvent{
		{
			{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use"},
		},
		refusalEvents(&provided),
	}}
	emitter := &stopDetailsRecordingEmitter{}
	loop.Trace = emitter

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	turns, _ := emitter.snapshot()
	must.Len(turns, 2)
	check.Nil(turns[0].StopDetails, "tool_use turn")
	check.Equal("refusal", turns[1].StopReason)
	check.Equal(&want, turns[1].StopDetails)
	check.Equal(&want, emitter.stopDetails, "recorded run StopDetails")
}

func TestLoop_StopDetailsAbsentOnSuccess(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(&mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "Done."},
		{Type: "message_complete", StopReason: "end_turn"},
	}})
	var traceBuf bytes.Buffer
	loop.Trace = trace.NewJSONLTraceEmitter(&traceBuf, false)

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	check.Equal("success", runTrace.Outcome)
	check.Nil(runTrace.StopDetails)
	check.NotContains(traceBuf.String(), "stopDetails", "trace should not carry stopDetails")
}

// TestLoop_StopDetailsNotRecordedWhenRunEndsOnToolTurn pins that
// RunTrace.StopDetails reflects only a run that ended on a non-tool stop:
// details on a tool_use turn stay on that turn's TurnTrace.
func TestLoop_StopDetailsNotRecordedWhenRunEndsOnToolTurn(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(nil)
	loop.Provider = &multiCallProvider{calls: [][]types.StreamEvent{{
		{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
		{Type: "message_complete", StopReason: "tool_use", StopDetails: &types.StopDetails{Type: "refusal"}},
	}}}
	emitter := &stopDetailsRecordingEmitter{}
	loop.Trace = emitter
	config := buildTestConfig()
	config.MaxTurns = 1

	_, err := loop.Run(context.Background(), config)
	must.NoError(err)
	check.Nil(emitter.stopDetails, "recorded run StopDetails")
}

// TestLoop_StopDetailsFromEarlierVerificationAttemptNotRecorded pins that
// only the inner loop that ended the run contributes RunTrace.StopDetails.
// A refusal ends the run before verification, so the earlier attempt
// carries its details on an end_turn stop.
func TestLoop_StopDetailsFromEarlierVerificationAttemptNotRecorded(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(nil)
	loop.Provider = &multiCallProvider{calls: [][]types.StreamEvent{
		{
			{Type: "text_delta", Text: "First attempt."},
			{Type: "message_complete", StopReason: "end_turn", StopDetails: &types.StopDetails{Type: "refusal", Category: "cyber"}},
		},
		{
			{Type: "text_delta", Text: "Second attempt."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}}
	loop.Verifier = &scriptedVerifier{passes: []bool{false, true}}
	emitter := &stopDetailsRecordingEmitter{}
	loop.Trace = emitter

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	check.Equal("success", runTrace.Outcome)
	check.Nil(emitter.stopDetails, "recorded run StopDetails")

	turns, _ := emitter.snapshot()
	must.Len(turns, 2)
	check.NotNil(turns[0].StopDetails, "first turn keeps its StopDetails")
}

// TestLoop_StopDetailsClearedWhenOutcomeReclassified pins that a run whose
// outcome no longer reports the inner loop's provider stop carries no
// RunTrace.StopDetails, while the turn keeps its own.
func TestLoop_StopDetailsClearedWhenOutcomeReclassified(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	loop := buildTestLoop(&mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "Done."},
		{Type: "message_complete", StopReason: "end_turn", StopDetails: &types.StopDetails{Type: "refusal", Category: "bio"}},
	}})
	loop.Verifier = &failingVerifier{}
	emitter := &stopDetailsRecordingEmitter{}
	loop.Trace = emitter

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	check.Equal("verification_failed", runTrace.Outcome)
	check.Nil(emitter.stopDetails, "recorded run StopDetails")

	turns, _ := emitter.snapshot()
	must.NotEmpty(turns)
	check.NotNil(turns[len(turns)-1].StopDetails, "last turn keeps the provider's details")
}

func TestLoop_StopDetailsExplanationIsScrubbed(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	const raw = "declined: AKIAIOSFODNN7EXAMPLE"
	provided := &types.StopDetails{Type: "refusal", Explanation: raw}
	loop := buildTestLoop(&mockProvider{events: refusalEvents(provided)})
	logBuf, _ := observedLoop(t, loop)
	emitter := &stopDetailsRecordingEmitter{}
	loop.Trace = emitter

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	scrubbed := func(where, got string) {
		t.Helper()
		check.NotContains(got, "AKIA", "%s explanation should not keep the access key", where)
		check.Contains(got, "[REDACTED]", "%s explanation should redact the access key", where)
	}
	must.NotNil(emitter.stopDetails, "recorded run StopDetails")
	scrubbed("RunTrace", emitter.stopDetails.Explanation)

	turns, _ := emitter.snapshot()
	must.Len(turns, 1)
	must.NotNil(turns[0].StopDetails, "turn StopDetails")
	scrubbed("TurnTrace", turns[0].StopDetails.Explanation)

	refusals := logRecords(t, logBuf, "provider refused to respond")
	must.Len(refusals, 1)
	explanation, _ := refusals[0]["explanation"].(string)
	scrubbed("WARN log", explanation)

	check.Equal(raw, provided.Explanation, "provider event explanation should not be mutated")
}
