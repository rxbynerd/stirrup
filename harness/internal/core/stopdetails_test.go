package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/harness/internal/trace"
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

func refusalEvents(details *types.StopDetails) []types.StreamEvent {
	return []types.StreamEvent{
		{Type: "text_delta", Text: "I can't help with that."},
		{Type: "message_complete", StopReason: "refusal", StopDetails: details},
	}
}

func TestLoop_RefusalRecordsStopDetails(t *testing.T) {
	details := &types.StopDetails{
		Type:        "refusal",
		Category:    "cyber",
		Explanation: "This request was declined because it conflicts with Anthropic's Usage Policy.",
	}
	loop := buildTestLoop(&mockProvider{events: refusalEvents(details)})

	var traceBuf, logBuf bytes.Buffer
	loop.Trace = trace.NewJSONLTraceEmitter(&traceBuf, false)
	loop.Logger = slog.New(slog.NewJSONHandler(&logBuf, nil))
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	loop.Tracer = tp.Tracer("test")

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if runTrace.Outcome != "refusal" {
		t.Errorf("Outcome = %q, want refusal", runTrace.Outcome)
	}
	if runTrace.StopDetails == nil || *runTrace.StopDetails != *details {
		t.Errorf("RunTrace.StopDetails = %+v, want %+v", runTrace.StopDetails, details)
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
	if finished.Trace.StopDetails == nil || *finished.Trace.StopDetails != *details {
		t.Errorf("persisted StopDetails = %+v, want %+v", finished.Trace.StopDetails, details)
	}

	var refusalLog map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err == nil && rec["msg"] == "provider refused to respond" {
			refusalLog = rec
		}
	}
	if refusalLog == nil {
		t.Fatalf("no refusal log record; logs:\n%s", logBuf.String())
	}
	if refusalLog["level"] != "WARN" || refusalLog["category"] != "cyber" || refusalLog["explanation"] != details.Explanation {
		t.Errorf("refusal log = %v, want WARN with category and explanation", refusalLog)
	}

	var category string
	for _, s := range exporter.GetSpans() {
		if s.Name != "provider.stream" {
			continue
		}
		for _, attr := range s.Attributes {
			if string(attr.Key) == "stop.category" {
				category = attr.Value.AsString()
			}
		}
	}
	if category != "cyber" {
		t.Errorf("provider.stream stop.category = %q, want cyber", category)
	}
}

func TestLoop_StopDetailsOnTurnTrace(t *testing.T) {
	details := &types.StopDetails{Type: "refusal", Category: "bio"}
	loop := buildTestLoop(nil)
	loop.Provider = &multiCallProvider{calls: [][]types.StreamEvent{
		{
			{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use"},
		},
		refusalEvents(details),
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
	if turns[1].StopReason != "refusal" || turns[1].StopDetails == nil || *turns[1].StopDetails != *details {
		t.Errorf("refusal turn = %+v, want StopReason refusal with %+v", turns[1], details)
	}
	if emitter.stopDetails == nil || *emitter.stopDetails != *details {
		t.Errorf("recorded run StopDetails = %+v, want %+v", emitter.stopDetails, details)
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

func TestLoop_StopDetailsExplanationIsScrubbed(t *testing.T) {
	loop := buildTestLoop(&mockProvider{events: refusalEvents(&types.StopDetails{
		Type:        "refusal",
		Explanation: "declined: AKIAIOSFODNN7EXAMPLE",
	})})

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.StopDetails == nil {
		t.Fatal("StopDetails = nil, want scrubbed details")
	}
	if got := runTrace.StopDetails.Explanation; strings.Contains(got, "AKIA") || !strings.Contains(got, "[REDACTED]") {
		t.Errorf("Explanation = %q, want the access key redacted", got)
	}
}
