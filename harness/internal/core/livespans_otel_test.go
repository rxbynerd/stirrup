package core

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
)

// otelLoop returns a test loop recording through an in-memory OTel
// emitter, with loop spans on the emitter's tracer as in production.
func otelLoop(t *testing.T, prov *mockProvider) (*AgenticLoop, *tracetest.InMemoryExporter) {
	t.Helper()
	emitter, exporter := newInMemoryOTelEmitter(t)
	loop := buildTestLoop(prov)
	loop.Trace = emitter
	loop.Tracer = emitter.Tracer()
	return loop, exporter
}

func spansNamed(spans tracetest.SpanStubs, name string) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// chatSpansAtTurn returns the chat spans whose turn.number attribute equals turn.
func chatSpansAtTurn(spans tracetest.SpanStubs, turn string) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for _, s := range spansNamed(spans, "chat claude-sonnet-4-6") {
		if got, _ := spanAttr(s, "turn.number"); got == turn {
			out = append(out, s)
		}
	}
	return out
}

func spanAttr(s tracetest.SpanStub, key string) (string, bool) {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.String(), true
		}
	}
	return "", false
}

func requireParent(t *testing.T, child, parent tracetest.SpanStub) {
	t.Helper()
	require.Equal(t, parent.SpanContext.SpanID(), child.Parent.SpanID(), "%s: parent span ID (want the %s span)", child.Name, parent.Name)
}

func TestLoop_OTelChatSpanPerRequest(t *testing.T) {
	loop, exporter := otelLoop(t, nil)
	loop.Provider = &scriptedProvider{turns: [][]types.StreamEvent{
		{
			{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use", InputTokens: 10, OutputTokens: 3},
		},
		{
			{Type: "text_delta", Text: "done"},
			{Type: "message_complete", StopReason: "end_turn", InputTokens: 12, OutputTokens: 1},
		},
	}}
	_, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)

	spans := exporter.GetSpans()
	assert.Empty(t, spansNamed(spans, "run"), "legacy run spans")
	root := spansNamed(spans, "invoke_agent stirrup")
	require.Len(t, root, 1, "invoke_agent stirrup spans")
	chats := spansNamed(spans, "chat claude-sonnet-4-6")
	require.Len(t, chats, 2, "chat spans")
	streams := spansNamed(spans, "provider.stream")
	require.Len(t, streams, 2, "provider.stream spans")
	for i, chat := range chats {
		assert.Equal(t, oteltrace.SpanKindClient, chat.SpanKind, "chat %d span kind", i)
		requireParent(t, chat, root[0])
		requireParent(t, streams[i], chat)
		turnNumber, _ := spanAttr(chat, "turn.number")
		assert.Equal(t, []string{"0", "1"}[i], turnNumber, "chat %d turn.number", i)
	}
	toolCalls, _ := spanAttr(chats[0], "turn.tool_calls")
	assert.Equal(t, "1", toolCalls, "turn.tool_calls")
	tools := spansNamed(spans, "execute_tool test_tool")
	require.Len(t, tools, 1, "execute_tool spans")
	toolTurn, _ := spanAttr(tools[0], "turn.number")
	assert.Equal(t, "0", toolTurn, "tool span turn.number (the emitting chat span's turn)")
	inputTokens, _ := spanAttr(chats[1], "gen_ai.usage.input_tokens")
	assert.Equal(t, "12", inputTokens, "second chat gen_ai.usage.input_tokens")
	for _, s := range spans {
		assert.False(t, strings.HasPrefix(s.Name, "turn["), "legacy turn span %q exported", s.Name)
	}
}

func TestLoop_OTelOverflowRetryGetsItsOwnChatSpan(t *testing.T) {
	loop, prov, _, _ := buildOverflowLoop(t, overflowStep{err: promptTooLong()})
	emitter, exporter := newInMemoryOTelEmitter(t)
	loop.Trace = emitter
	loop.Tracer = emitter.Tracer()
	config := buildTestConfig()
	config.ContextStrategy.MaxTokens = 200_000

	result, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "success", result.Outcome, "prerequisite: run outcome")
	require.Len(t, prov.requests, 5, "prerequisite: provider requests")

	turn3 := chatSpansAtTurn(exporter.GetSpans(), "3")
	require.Len(t, turn3, 2, "turn 3 chat spans (rejected request and retry)")
	rejectedErrType, _ := spanAttr(turn3[0], "error.type")
	assert.Equal(t, "context_overflow", rejectedErrType, "rejected request error.type")
	_, retryHasErr := spanAttr(turn3[1], "error.type")
	assert.False(t, retryHasErr, "successful retry carries error.type")
}

func TestLoop_OTelWindowFillingResponseEndsItsChatSpan(t *testing.T) {
	loop, prov, _, _ := buildOverflowLoop(t, overflowStepsByKind()["context window stop"])
	emitter, exporter := newInMemoryOTelEmitter(t)
	loop.Trace = emitter
	loop.Tracer = emitter.Tracer()
	config := buildTestConfig()
	config.ContextStrategy.MaxTokens = 200_000

	result, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "success", result.Outcome, "prerequisite: run outcome")
	require.Len(t, prov.requests, 5, "prerequisite: provider requests")

	turn3 := chatSpansAtTurn(exporter.GetSpans(), "3")
	require.Len(t, turn3, 2, "turn 3 chat spans (window-filling response and retry)")
	filled := turn3[0]
	filledErrType, _ := spanAttr(filled, "error.type")
	assert.Equal(t, "context_overflow", filledErrType, "window-filling request error.type")
	_, abandoned := spanAttr(filled, "stirrup.span.abandoned")
	assert.False(t, abandoned, "window-filling request span was abandoned rather than ended by the loop")
	outputTokens, _ := spanAttr(filled, "gen_ai.usage.output_tokens")
	assert.Equal(t, "40", outputTokens, "window-filling request gen_ai.usage.output_tokens")
	_, retryHasErr := spanAttr(turn3[1], "error.type")
	assert.False(t, retryHasErr, "successful retry carries error.type")
}

func TestLoop_OTelOneToolSpanPerCall(t *testing.T) {
	loop, exporter := otelLoop(t, nil)
	loop.Permissions = permission.NewAllowAll()
	loop.Tools.(*tool.Registry).Register(&tool.Tool{
		Name:              "slow_mutating_tool",
		Description:       "sleeps then succeeds",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{}}`),
		WorkspaceMutating: true,
		Handler: func(context.Context, json.RawMessage) (string, error) {
			time.Sleep(20 * time.Millisecond)
			return "ok", nil
		},
	})
	loop.Provider = &scriptedProvider{turns: [][]types.StreamEvent{
		{
			{Type: "tool_call", ID: "tc_0", Name: "slow_mutating_tool", Input: map[string]any{}},
			{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use"},
		},
		{
			{Type: "text_delta", Text: "done"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}}
	_, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)

	spans := exporter.GetSpans()
	roots := spansNamed(spans, "invoke_agent stirrup")
	require.Len(t, roots, 1, "invoke_agent stirrup spans")
	root := roots[0]
	slow := spansNamed(spans, "execute_tool slow_mutating_tool")
	fast := spansNamed(spans, "execute_tool test_tool")
	require.Len(t, slow, 1, "execute_tool slow_mutating_tool spans")
	require.Len(t, fast, 1, "execute_tool test_tool spans")
	for _, s := range spans {
		assert.False(t, strings.HasPrefix(s.Name, "tool."), "legacy tool span %q exported", s.Name)
	}
	requireParent(t, slow[0], root)
	requireParent(t, fast[0], root)
	perm := spansNamed(spans, "permission.check")
	require.Len(t, perm, 1, "permission.check spans")
	requireParent(t, perm[0], slow[0])
	assert.False(t, slow[0].EndTime.After(fast[0].StartTime),
		"first call's span ends at %v, after the second call's span starts at %v", slow[0].EndTime, fast[0].StartTime)
	callID, _ := spanAttr(slow[0], "gen_ai.tool.call.id")
	assert.Equal(t, "tc_0", callID, "gen_ai.tool.call.id")
}

func TestLoop_ToolDurationExcludesLaterCalls(t *testing.T) {
	var traceBuf bytes.Buffer
	loop := buildTestLoop(nil)
	loop.Trace = trace.NewJSONLTraceEmitter(&traceBuf, false)
	loop.Tools.(*tool.Registry).Register(&tool.Tool{
		Name:        "slow_tool",
		Description: "sleeps then succeeds",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			time.Sleep(60 * time.Millisecond)
			return "ok", nil
		},
	})
	loop.Provider = &scriptedProvider{turns: [][]types.StreamEvent{
		{
			{Type: "tool_call", ID: "tc_fast", Name: "test_tool", Input: map[string]any{}},
			{Type: "tool_call", ID: "tc_slow", Name: "slow_tool", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use"},
		},
		{
			{Type: "text_delta", Text: "done"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}}
	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	durations := map[string]int64{}
	for _, call := range runTrace.ToolCalls {
		durations[call.ID] = call.DurationMs
	}
	require.GreaterOrEqual(t, durations["tc_slow"], int64(60), "prerequisite: slow call duration in ms")
	assert.Less(t, durations["tc_fast"], int64(60), "fast call duration in ms excludes the slow call after it")
}

func TestLoop_OTelChatSpanCarriesProviderEndpoint(t *testing.T) {
	loop, exporter := otelLoop(t, nil)
	loop.Provider = endpointProvider{&scriptedTurnProvider{scripts: []turnScript{{events: []types.StreamEvent{
		{Type: "text_delta", Text: "done"},
		{Type: "message_complete", StopReason: "end_turn"},
	}}}}}
	_, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	chats := spansNamed(exporter.GetSpans(), "chat claude-sonnet-4-6")
	require.Len(t, chats, 1, "chat spans")
	address, _ := spanAttr(chats[0], "server.address")
	assert.Equal(t, "api.anthropic.com", address, "server.address")
	port, _ := spanAttr(chats[0], "server.port")
	assert.Equal(t, "443", port, "server.port")
}

// ctxBlockingProvider blocks each Stream until its context is done, then
// reports what it saw.
type ctxBlockingProvider struct {
	entered chan struct{}
	done    chan error
}

func (p *ctxBlockingProvider) Stream(ctx context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	close(p.entered)
	<-ctx.Done()
	p.done <- ctx.Err()
	return nil, ctx.Err()
}

func TestLoop_OTelProviderRequestCancelledWithRun(t *testing.T) {
	loop, exporter := otelLoop(t, nil)
	prov := &ctxBlockingProvider{entered: make(chan struct{}), done: make(chan error, 1)}
	loop.Provider = prov

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan *types.RunTrace, 1)
	go func() {
		rt, _ := loop.Run(ctx, buildTestConfig())
		result <- rt
	}()

	<-prov.entered
	cancel()
	select {
	case err := <-prov.done:
		require.Error(t, err, "provider context finished without an error")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "provider request was not cancelled with the run")
	}
	rt := <-result
	assert.Equal(t, "cancelled", rt.Outcome, "run outcome")
	chats := spansNamed(exporter.GetSpans(), "chat claude-sonnet-4-6")
	require.Len(t, chats, 1, "chat spans")
	errType, _ := spanAttr(chats[0], "error.type")
	assert.Equal(t, "cancelled", errType, "chat error.type")
}

func TestLoop_OTelChatSpanTemperatureOnlyWhenSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		temp *float64
		want string
	}{
		{name: "loop fallback", temp: nil, want: ""},
		{name: "operator set", temp: types.Float64Ptr(0.7), want: "0.7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loop, exporter := otelLoop(t, nil)
			loop.Provider = &scriptedProvider{turns: [][]types.StreamEvent{{
				{Type: "text_delta", Text: "done"},
				{Type: "message_complete", StopReason: "end_turn"},
			}}}
			config := buildTestConfig()
			config.Temperature = tc.temp
			_, err := loop.Run(context.Background(), config)
			require.NoError(t, err)
			chats := spansNamed(exporter.GetSpans(), "chat claude-sonnet-4-6")
			require.Len(t, chats, 1, "chat spans")
			got, _ := spanAttr(chats[0], "gen_ai.request.temperature")
			assert.Equal(t, tc.want, got, "gen_ai.request.temperature")
		})
	}
}

func TestLoop_OTelProviderRequestPastDeadlineIsTimeout(t *testing.T) {
	loop, exporter := otelLoop(t, nil)
	loop.Provider = &ctxBlockingProvider{entered: make(chan struct{}), done: make(chan error, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	rt, _ := loop.Run(ctx, buildTestConfig())
	require.Equal(t, "timeout", rt.Outcome, "prerequisite: run outcome")
	chats := spansNamed(exporter.GetSpans(), "chat claude-sonnet-4-6")
	require.Len(t, chats, 1, "chat spans")
	errType, _ := spanAttr(chats[0], "error.type")
	require.Equal(t, "timeout", errType, "chat error.type")
}

func eventsOfType(tp *recordingTransport, typ string) []types.HarnessEvent {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	var out []types.HarnessEvent
	for _, e := range tp.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestLoop_RunStartedCarriesRunSpanReference(t *testing.T) {
	loop, exporter := otelLoop(t, &mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "done"},
		{Type: "message_complete", StopReason: "end_turn"},
	}})
	tp := &recordingTransport{}
	loop.Transport = tp
	loop.emitReady = true
	config := buildTestConfig()

	rt, err := loop.Run(context.Background(), config)
	require.NoError(t, err)

	root := spansNamed(exporter.GetSpans(), "invoke_agent stirrup")
	require.Len(t, root, 1, "invoke_agent stirrup spans")
	want := types.TraceReference{
		TraceID:   root[0].SpanContext.TraceID().String(),
		RunSpanID: root[0].SpanContext.SpanID().String(),
		Sampled:   true,
	}

	readyAt, startedAt, firstModelAt := -1, -1, -1
	var started []types.HarnessEvent
	for i, e := range tp.events {
		switch e.Type {
		case "ready":
			if readyAt < 0 {
				readyAt = i
			}
		case "run_started":
			startedAt = i
			started = append(started, e)
		case "text_delta", "tool_call":
			if firstModelAt < 0 {
				firstModelAt = i
			}
		}
	}
	require.Len(t, started, 1, "run_started events")
	assert.GreaterOrEqual(t, readyAt, 0, "ready event is emitted")
	assert.Greater(t, startedAt, readyAt, "run_started follows ready")
	assert.Greater(t, firstModelAt, startedAt, "first model event follows run_started")
	assert.Equal(t, config.RunID, started[0].ID, "run_started ID")
	require.NotNil(t, started[0].TraceReference, "run_started reference")
	assert.Equal(t, want, *started[0].TraceReference, "run_started reference")
	require.NotNil(t, rt.TraceReference, "RunTrace.TraceReference")
	assert.Equal(t, want, *rt.TraceReference, "RunTrace.TraceReference")

	done := eventsOfType(tp, "done")
	require.Len(t, done, 1, "done events")
	require.NotNil(t, done[0].Trace, "done summary")
	require.NotNil(t, done[0].Trace.TraceReference, "done summary reference")
	assert.Equal(t, want, *done[0].Trace.TraceReference, "done summary reference")
}

func TestLoop_RunStartedWithoutOTelSpanCarriesNoReference(t *testing.T) {
	loop := buildTestLoop(&mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "done"},
		{Type: "message_complete", StopReason: "end_turn"},
	}})
	tp := &recordingTransport{}
	loop.Transport = tp
	config := buildTestConfig()

	rt, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	started := eventsOfType(tp, "run_started")
	require.Len(t, started, 1, "run_started events")
	assert.Equal(t, config.RunID, started[0].ID, "run_started ID")
	assert.Equal(t, "run_started", tp.events[0].Type, "first event type")
	assert.Nil(t, rt.TraceReference, "RunTrace.TraceReference")
	for _, e := range tp.events {
		assert.Nil(t, e.TraceReference, "%s event reference", e.Type)
		if e.Trace != nil {
			assert.Nil(t, e.Trace.TraceReference, "%s event summary reference", e.Type)
		}
	}
}

func TestLoop_RunStartedIsFirstEventWithoutLoopReady(t *testing.T) {
	loop, _ := otelLoop(t, &mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "done"},
		{Type: "message_complete", StopReason: "end_turn"},
	}})
	tp := &recordingTransport{}
	loop.Transport = tp
	loop.emitReady = false
	config := buildTestConfig()

	_, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.NotEmpty(t, tp.events, "loop emitted events")
	first := tp.events[0]
	assert.Equal(t, "run_started", first.Type, "first event type")
	assert.Equal(t, config.RunID, first.ID, "first event ID")
	assert.NotNil(t, first.TraceReference, "first event reference")
}

// recordingControlTransport records emitted events and lets the test fire
// control events, so a follow-up run's events can be observed.
type recordingControlTransport struct {
	controllableTransport
	rec recordingTransport
}

func (t *recordingControlTransport) Emit(e types.HarnessEvent) error { return t.rec.Emit(e) }

func TestRunFollowUpLoop_EachRunStartsWithItsOwnReference(t *testing.T) {
	loop, _ := otelLoop(t, &mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "done"},
		{Type: "message_complete", StopReason: "end_turn"},
	}})
	tp := &recordingControlTransport{}
	loop.Transport = tp
	loop.ensureControlRouting()
	config := buildTestConfig()

	_, err := loop.Run(context.Background(), config)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunFollowUpLoop(ctx, loop, config, 30, FollowUpOptions{
			OnRunComplete: func(c *types.RunConfig, _ *types.RunTrace, _ error) { ran <- c.RunID },
		})
	}()
	tp.FireControl(types.ControlEvent{Type: "user_response", UserResponse: "And another thing."})
	var followUpRunID string
	select {
	case followUpRunID = <-ran:
	case <-time.After(3 * time.Second):
		require.FailNow(t, "follow-up run did not complete")
	}
	cancel()
	<-done

	started := eventsOfType(&tp.rec, "run_started")
	require.Len(t, started, 2, "run_started events, one per run")
	assert.Equal(t, "test-run-1", started[0].ID, "first run_started ID")
	assert.Equal(t, followUpRunID, started[1].ID, "follow-up run_started ID")
	require.NotNil(t, started[0].TraceReference, "first run_started reference")
	require.NotNil(t, started[1].TraceReference, "follow-up run_started reference")
	assert.NotEqual(t, started[0].TraceReference.TraceID, started[1].TraceReference.TraceID,
		"follow-up run should start its own trace")
}
