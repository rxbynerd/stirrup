package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// respondWhenAllRequested answers every tool_result_request only once n of
// them are on the wire, so the calls are provably in flight together.
// Callers run it on a goroutine, so it reports with assert rather than require.
func respondWhenAllRequested(t *testing.T, tr *asyncTestTransport, n int) {
	t.Helper()
	check := assert.New(t)
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
	check.Fail("fewer tool_result_request events than expected", "want %d", n)
}

func decodeTraceContext(t *testing.T, c *types.TraceContext) oteltrace.SpanContext {
	t.Helper()
	must := require.New(t)
	must.NotNil(c, "tool_result_request carries no trace context")
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
	must := require.New(t)
	loop, exporter := otelLoop(t, nil)
	loop.Tools.(*tool.Registry).Register(asyncEchoTool())
	loop.Provider = twoAsyncCallsProvider()
	tr := newAsyncTestTransport()
	loop.Transport = tr

	go respondWhenAllRequested(t, tr, 2)
	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err, "Run")

	spans := exporter.GetSpans()
	root := spansNamed(spans, "invoke_agent stirrup")
	must.Len(root, 1, "root spans")
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
		t.Run(e.ToolUseID, func(t *testing.T) {
			must := require.New(t)
			sc := decodeTraceContext(t, e.TraceContext)
			must.Contains(spanByCallID, e.ToolUseID, "no execute_tool span recorded")
			want := spanByCallID[e.ToolUseID]
			must.Equal(root[0].SpanContext.TraceID(), sc.TraceID(), "trace ID should be the run's")
			must.Equal(want.SpanContext.SpanID(), sc.SpanID(), "span ID should be its execute_tool span")
			must.True(sc.IsSampled(), "carrier should have the recorded span's sampled flag")
			other, dup := seen[sc.SpanID()]
			must.False(dup, "span ID is also carried by %s", other)
			seen[sc.SpanID()] = e.ToolUseID
		})
	}
	must.Len(seen, 2, "tool_result_request carriers")
}

func TestAsyncToolResultRequest_NoTraceContextWithoutSpan(t *testing.T) {
	must := require.New(t)
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, asyncEchoTool())
	call := types.ToolCall{ID: "tc_plain", Name: "async_echo", Input: json.RawMessage(`{}`)}

	go respondWhenAllRequested(t, tr, 1)
	output, ok := loop.dispatchToolCall(context.Background(), call)
	must.True(ok, "dispatch failed: %q", output)
	for _, e := range tr.Events() {
		if e.Type == "tool_result_request" {
			must.Nil(e.TraceContext, "noop-traced call should carry no trace context")
		}
	}
}
