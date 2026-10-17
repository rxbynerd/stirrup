package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// respondWhenAllRequested answers every tool_result_request only once n of
// them are on the wire, so the calls are provably in flight together.
func respondWhenAllRequested(t *testing.T, tr *asyncTestTransport, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var requests []types.HarnessEvent
		for _, e := range tr.Events() {
			if e.Type == "tool_result_request" {
				requests = append(requests, e)
			}
		}
		if len(requests) == n {
			for _, r := range requests {
				tr.FireControl(types.ControlEvent{Type: "tool_result_response", RequestID: r.RequestID, Content: "ok"})
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("fewer than %d tool_result_request events emitted", n)
}

func decodeTraceContext(t *testing.T, c *types.TraceContext) oteltrace.SpanContext {
	t.Helper()
	if c == nil {
		t.Fatal("tool_result_request carries no trace context")
	}
	carrier := propagation.MapCarrier{"traceparent": c.Traceparent, "tracestate": c.Tracestate}
	return oteltrace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), carrier))
}

func twoAsyncCallsProvider() *scriptedProvider {
	return &scriptedProvider{turns: [][]types.StreamEvent{
		{
			{Type: "tool_call", ID: "tc_a", Name: "async_echo", Input: map[string]any{}},
			{Type: "tool_call", ID: "tc_b", Name: "async_echo", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use"},
		},
		{
			{Type: "text_delta", Text: "done"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}}
}

func TestAsyncToolResultRequest_CarriesItsExecuteToolSpan(t *testing.T) {
	loop, exporter := otelLoop(t, nil)
	loop.Tools.(*tool.Registry).Register(asyncEchoTool())
	loop.Provider = twoAsyncCallsProvider()
	tr := newAsyncTestTransport()
	loop.Transport = tr

	go respondWhenAllRequested(t, tr, 2)
	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	spans := exporter.GetSpans()
	root := spansNamed(spans, "invoke_agent stirrup")
	if len(root) != 1 {
		t.Fatalf("found %d root spans, want 1", len(root))
	}
	spanByCallID := map[string]tracetest.SpanStub{}
	for _, s := range spansNamed(spans, "execute_tool async_echo") {
		id, _ := spanAttr(s, "gen_ai.tool.call.id")
		spanByCallID[id] = s
	}

	seen := map[oteltrace.SpanID]string{}
	for _, e := range tr.Events() {
		if e.Type != "tool_result_request" {
			continue
		}
		sc := decodeTraceContext(t, e.TraceContext)
		want, ok := spanByCallID[e.ToolUseID]
		if !ok {
			t.Fatalf("no execute_tool span recorded for %s", e.ToolUseID)
		}
		if sc.TraceID() != root[0].SpanContext.TraceID() {
			t.Errorf("%s: trace ID = %s, want the run's %s", e.ToolUseID, sc.TraceID(), root[0].SpanContext.TraceID())
		}
		if sc.SpanID() != want.SpanContext.SpanID() {
			t.Errorf("%s: span ID = %s, want its execute_tool span %s", e.ToolUseID, sc.SpanID(), want.SpanContext.SpanID())
		}
		if !sc.IsSampled() {
			t.Errorf("%s: carrier is unsampled, want the recorded span's sampled flag", e.ToolUseID)
		}
		if other, dup := seen[sc.SpanID()]; dup {
			t.Errorf("%s and %s carry the same span ID %s", other, e.ToolUseID, sc.SpanID())
		}
		seen[sc.SpanID()] = e.ToolUseID
	}
	if len(seen) != 2 {
		t.Fatalf("got %d tool_result_request carriers, want 2", len(seen))
	}
}

func TestAsyncToolResultRequest_NoCarrierWithoutSpan(t *testing.T) {
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, asyncEchoTool())
	call := types.ToolCall{ID: "tc_plain", Name: "async_echo", Input: json.RawMessage(`{}`)}

	go respondWhenAllRequested(t, tr, 1)
	if output, ok := loop.dispatchToolCall(context.Background(), call); !ok {
		t.Fatalf("dispatch failed: %q", output)
	}
	for _, e := range tr.Events() {
		if e.Type == "tool_result_request" && e.TraceContext != nil {
			t.Errorf("noop-traced call carries trace context %+v, want none", e.TraceContext)
		}
	}
}
