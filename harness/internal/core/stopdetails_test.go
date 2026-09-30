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
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if runTrace.Outcome != "refusal" {
		t.Errorf("Outcome = %q, want refusal", runTrace.Outcome)
	}
	if runTrace.StopDetails == nil || *runTrace.StopDetails != want {
		t.Errorf("RunTrace.StopDetails = %+v, want %+v", runTrace.StopDetails, want)
	}

	var finished struct {
		Kind  string         `json:"kind"`
		Trace types.RunTrace `json:"trace"`
	}
	lines := strings.Split(strings.TrimSpace(traceBuf.String()), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &finished); err != nil {
		t.Fatalf("decode run_finished line: %v", err)
	}
	if finished.Kind != "run_finished" {
		t.Fatalf("last trace line kind = %q, want run_finished", finished.Kind)
	}
	if finished.Trace.StopDetails == nil || *finished.Trace.StopDetails != want {
		t.Errorf("persisted StopDetails = %+v, want %+v", finished.Trace.StopDetails, want)
	}

	refusals := logRecords(t, logBuf, "provider refused to respond")
	if len(refusals) != 1 {
		t.Fatalf("refusal log records = %d, want 1; logs:\n%s", len(refusals), logBuf.String())
	}
	if rec := refusals[0]; rec["level"] != "WARN" || rec["category"] != "cyber" || rec["explanation"] != want.Explanation {
		t.Errorf("refusal log = %v, want WARN with category and explanation", rec)
	}

	if category, _ := providerStreamAttr(exporter, "stop.category"); category != "cyber" {
		t.Errorf("provider.stream stop.category = %q, want cyber", category)
	}
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
			loop := buildTestLoop(&mockProvider{events: refusalEvents(tc.details)})
			logBuf, exporter := observedLoop(t, loop)

			runTrace, err := loop.Run(context.Background(), buildTestConfig())
			if err != nil {
				t.Fatalf("Run() error: %v", err)
			}
			if runTrace.Outcome != "refusal" {
				t.Errorf("Outcome = %q, want refusal", runTrace.Outcome)
			}
			refusals := logRecords(t, logBuf, "provider refused to respond")
			if len(refusals) != 1 {
				t.Fatalf("refusal log records = %d, want 1; logs:\n%s", len(refusals), logBuf.String())
			}
			if rec := refusals[0]; rec["level"] != "WARN" || rec["category"] != "" || rec["explanation"] != tc.wantExplanation {
				t.Errorf("refusal log = %v, want WARN with empty category and explanation %q", rec, tc.wantExplanation)
			}
			if value, ok := providerStreamAttr(exporter, "stop.category"); ok {
				t.Errorf("provider.stream stop.category = %q, want the attribute absent", value)
			}
		})
	}
}

func TestLoop_ContextWindowExceededPassesThrough(t *testing.T) {
	loop := buildTestLoop(&mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "Partial answer"},
		{Type: "message_complete", StopReason: "model_context_window_exceeded"},
	}})
	logBuf, _ := observedLoop(t, loop)

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "model_context_window_exceeded" {
		t.Errorf("Outcome = %q, want model_context_window_exceeded", runTrace.Outcome)
	}
	if runTrace.StopDetails != nil {
		t.Errorf("StopDetails = %+v, want nil", runTrace.StopDetails)
	}
	if n := len(logRecords(t, logBuf, "provider refused to respond")); n != 0 {
		t.Errorf("refusal log records = %d, want 0", n)
	}
}

// TestLoop_StreamErrorAfterPartialText feeds the loop the event sequence
// the Anthropic adapter emits for an SSE error event that follows streamed
// text (pinned adapter-side by TestSSE_ErrorEventAfterPartialText).
func TestLoop_StreamErrorAfterPartialText(t *testing.T) {
	loop := buildTestLoop(&mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "Looking at the"},
		{Type: "error", Error: errors.New("anthropic API stream error (overloaded_error): Overloaded")},
	}})
	logBuf, _ := observedLoop(t, loop)

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "error" {
		t.Errorf("Outcome = %q, want error", runTrace.Outcome)
	}
	if runTrace.StopDetails != nil {
		t.Errorf("StopDetails = %+v, want nil", runTrace.StopDetails)
	}
	failures := logRecords(t, logBuf, "provider stream failed")
	if len(failures) != 1 {
		t.Fatalf("provider stream failed records = %d, want 1; logs:\n%s", len(failures), logBuf.String())
	}
	if msg, _ := failures[0]["error"].(string); !strings.Contains(msg, "overloaded_error") {
		t.Errorf("logged error = %q, want the provider error type", msg)
	}
	if n := len(logRecords(t, logBuf, "provider returned empty stop reason")); n != 0 {
		t.Errorf("empty stop reason warnings = %d, want 0", n)
	}
}

func TestLoop_StopDetailsOnTurnTrace(t *testing.T) {
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

	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	turns, _ := emitter.snapshot()
	if len(turns) != 2 {
		t.Fatalf("recorded %d turns, want 2", len(turns))
	}
	if turns[0].StopDetails != nil {
		t.Errorf("tool_use turn StopDetails = %+v, want nil", turns[0].StopDetails)
	}
	if turns[1].StopReason != "refusal" || turns[1].StopDetails == nil || *turns[1].StopDetails != want {
		t.Errorf("refusal turn = %+v, want StopReason refusal with %+v", turns[1], want)
	}
	if emitter.stopDetails == nil || *emitter.stopDetails != want {
		t.Errorf("recorded run StopDetails = %+v, want %+v", emitter.stopDetails, want)
	}
}

func TestLoop_StopDetailsAbsentOnSuccess(t *testing.T) {
	loop := buildTestLoop(&mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "Done."},
		{Type: "message_complete", StopReason: "end_turn"},
	}})
	var traceBuf bytes.Buffer
	loop.Trace = trace.NewJSONLTraceEmitter(&traceBuf, false)

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("Outcome = %q, want success", runTrace.Outcome)
	}
	if runTrace.StopDetails != nil {
		t.Errorf("StopDetails = %+v, want nil", runTrace.StopDetails)
	}
	if strings.Contains(traceBuf.String(), "stopDetails") {
		t.Errorf("trace unexpectedly carries stopDetails:\n%s", traceBuf.String())
	}
}

// TestLoop_StopDetailsNotRecordedWhenRunEndsOnToolTurn pins that
// RunTrace.StopDetails reflects only a run that ended on a non-tool stop:
// details on a tool_use turn stay on that turn's TurnTrace.
func TestLoop_StopDetailsNotRecordedWhenRunEndsOnToolTurn(t *testing.T) {
	loop := buildTestLoop(nil)
	loop.Provider = &multiCallProvider{calls: [][]types.StreamEvent{{
		{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
		{Type: "message_complete", StopReason: "tool_use", StopDetails: &types.StopDetails{Type: "refusal"}},
	}}}
	emitter := &stopDetailsRecordingEmitter{}
	loop.Trace = emitter
	config := buildTestConfig()
	config.MaxTurns = 1

	if _, err := loop.Run(context.Background(), config); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if emitter.stopDetails != nil {
		t.Errorf("recorded run StopDetails = %+v, want nil", emitter.stopDetails)
	}
}

// TestLoop_StopDetailsFromEarlierVerificationAttemptNotRecorded pins that
// only the inner loop that ended the run contributes RunTrace.StopDetails.
// A refusal ends the run before verification, so the earlier attempt
// carries its details on an end_turn stop.
func TestLoop_StopDetailsFromEarlierVerificationAttemptNotRecorded(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("Outcome = %q, want success", runTrace.Outcome)
	}
	if emitter.stopDetails != nil {
		t.Errorf("recorded run StopDetails = %+v, want nil", emitter.stopDetails)
	}
	turns, _ := emitter.snapshot()
	if len(turns) != 2 || turns[0].StopDetails == nil {
		t.Errorf("turns = %+v, want the first turn to keep its StopDetails", turns)
	}
}

// TestLoop_StopDetailsClearedWhenOutcomeReclassified pins that a run whose
// outcome no longer reports the inner loop's provider stop carries no
// RunTrace.StopDetails, while the turn keeps its own.
func TestLoop_StopDetailsClearedWhenOutcomeReclassified(t *testing.T) {
	loop := buildTestLoop(&mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "Done."},
		{Type: "message_complete", StopReason: "end_turn", StopDetails: &types.StopDetails{Type: "refusal", Category: "bio"}},
	}})
	loop.Verifier = &failingVerifier{}
	emitter := &stopDetailsRecordingEmitter{}
	loop.Trace = emitter

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "verification_failed" {
		t.Errorf("Outcome = %q, want verification_failed", runTrace.Outcome)
	}
	if emitter.stopDetails != nil {
		t.Errorf("recorded run StopDetails = %+v, want nil", emitter.stopDetails)
	}
	turns, _ := emitter.snapshot()
	if len(turns) == 0 || turns[len(turns)-1].StopDetails == nil {
		t.Errorf("last turn StopDetails = nil, want the provider's details kept on the turn")
	}
}

func TestLoop_StopDetailsExplanationIsScrubbed(t *testing.T) {
	const raw = "declined: AKIAIOSFODNN7EXAMPLE"
	provided := &types.StopDetails{Type: "refusal", Explanation: raw}
	loop := buildTestLoop(&mockProvider{events: refusalEvents(provided)})
	logBuf, _ := observedLoop(t, loop)
	emitter := &stopDetailsRecordingEmitter{}
	loop.Trace = emitter

	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	scrubbed := func(where, got string) {
		t.Helper()
		if strings.Contains(got, "AKIA") || !strings.Contains(got, "[REDACTED]") {
			t.Errorf("%s explanation = %q, want the access key redacted", where, got)
		}
	}
	if emitter.stopDetails == nil {
		t.Fatal("recorded run StopDetails = nil, want scrubbed details")
	}
	scrubbed("RunTrace", emitter.stopDetails.Explanation)

	turns, _ := emitter.snapshot()
	if len(turns) != 1 || turns[0].StopDetails == nil {
		t.Fatalf("turns = %+v, want one turn with StopDetails", turns)
	}
	scrubbed("TurnTrace", turns[0].StopDetails.Explanation)

	refusals := logRecords(t, logBuf, "provider refused to respond")
	if len(refusals) != 1 {
		t.Fatalf("refusal log records = %d, want 1", len(refusals))
	}
	explanation, _ := refusals[0]["explanation"].(string)
	scrubbed("WARN log", explanation)

	if provided.Explanation != raw {
		t.Errorf("provider event explanation mutated to %q, want it untouched", provided.Explanation)
	}
}
