package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
)

func upstreamParent(t *testing.T, sampled bool) oteltrace.SpanContext {
	t.Helper()
	traceID, err := oteltrace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := oteltrace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)
	cfg := oteltrace.SpanContextConfig{TraceID: traceID, SpanID: spanID, Remote: true}
	if sampled {
		cfg.TraceFlags = oteltrace.FlagsSampled
	}
	return oteltrace.NewSpanContext(cfg)
}

func upstreamCtx(t *testing.T, sampled bool) context.Context {
	t.Helper()
	return oteltrace.ContextWithRemoteSpanContext(context.Background(), upstreamParent(t, sampled))
}

func TestLoop_TopLevelRunAdoptsUpstreamParent(t *testing.T) {
	loop, exporter := otelLoop(t, endTurnProvider())
	tp := &recordingTransport{}
	loop.Transport = tp
	parent := upstreamParent(t, true)

	_, err := loop.Run(upstreamCtx(t, true), buildTestConfig())
	require.NoError(t, err)

	spans := exporter.GetSpans()
	root := spansNamed(spans, "invoke_agent stirrup")
	require.Len(t, root, 1)
	assert.Equal(t, parent.SpanID(), root[0].Parent.SpanID(), "run span parent span ID")
	assert.Equal(t, parent.TraceID(), root[0].Parent.TraceID(), "run span parent trace ID")
	for _, s := range spans {
		assert.Equal(t, parent.TraceID(), s.SpanContext.TraceID(), "%s trace ID", s.Name)
	}
	chats := spansNamed(spans, "chat claude-sonnet-4-6")
	require.Len(t, chats, 1)
	requireParent(t, chats[0], root[0])

	started := eventsOfType(tp, "run_started")
	require.Len(t, started, 1)
	require.NotNil(t, started[0].TraceReference)
	ref := started[0].TraceReference
	assert.Equal(t, parent.TraceID().String(), ref.TraceID, "run_started trace ID")
	assert.Equal(t, root[0].SpanContext.SpanID().String(), ref.RunSpanID, "run_started run span ID")
	assert.True(t, ref.Sampled, "run_started sampled")
}

func TestLoop_UnsampledUpstreamParentSuppressesExport(t *testing.T) {
	loop, exporter := otelLoop(t, endTurnProvider())
	tp := &recordingTransport{}
	loop.Transport = tp

	rt, err := loop.Run(upstreamCtx(t, false), buildTestConfig())
	require.NoError(t, err)

	spans := exporter.GetSpans()
	assert.Empty(t, spans, "spans exported under an unsampled parent: %v", spanNames(spans))

	started := eventsOfType(tp, "run_started")
	require.Len(t, started, 1)
	require.NotNil(t, started[0].TraceReference)
	ref := started[0].TraceReference
	assert.False(t, ref.Sampled, "run_started sampled under an unsampled parent")
	assert.Equal(t, upstreamParent(t, false).TraceID().String(), ref.TraceID, "run_started trace ID")

	require.NotNil(t, rt.TraceReference)
	assert.False(t, rt.TraceReference.Sampled, "RunTrace.TraceReference sampled")
}

func TestLoop_SubAgentRunIgnoresCallerSpan(t *testing.T) {
	for name, configure := range map[string]func(*AgenticLoop){
		"parent run ID set":    func(l *AgenticLoop) { l.ParentRunID = "parent-run" },
		"trace context preset": func(l *AgenticLoop) { l.TraceContext = context.Background() },
	} {
		t.Run(name, func(t *testing.T) {
			loop, exporter := otelLoop(t, endTurnProvider())
			configure(loop)

			_, err := loop.Run(upstreamCtx(t, true), buildTestConfig())
			require.NoError(t, err)

			root := spansNamed(exporter.GetSpans(), "invoke_agent stirrup")
			require.Len(t, root, 1)
			require.False(t, root[0].Parent.IsValid(), "run span parented on %s, want a new root", root[0].Parent.SpanID())
		})
	}
}

func TestSpawnSubAgent_UnderUpstreamParentNestsUnderSpawnTool(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	emitter := trace.NewOTelTraceEmitterForTest(tp)
	parent := upstreamParent(t, true)

	parentLoop := buildTestLoop(nil)
	parentLoop.Trace = emitter
	parentLoop.Tracer = emitter.Tracer()
	parentLoop.Transport = &recordingTransport{}
	parentLoop.Provider = &multiCallProviderForOTel{calls: [][]types.StreamEvent{
		{
			{Type: "tool_call", ID: "tc_spawn", Name: "spawn_agent", Input: map[string]any{"prompt": "subtask"}},
			{Type: "message_complete", StopReason: "tool_use"},
		},
		{
			{Type: "text_delta", Text: "Done."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}}
	parentConfig := buildTestConfig()
	parentLoop.Tools.(*tool.Registry).Register(&tool.Tool{
		Name:        "spawn_agent",
		Description: "Spawn a sub-agent",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"]}`),
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			parentProv := parentLoop.Provider
			parentLoop.Provider = endTurnProvider()
			defer func() { parentLoop.Provider = parentProv }()
			result, err := SpawnSubAgent(ctx, parentLoop, parentConfig, SubAgentConfig{Prompt: "subtask", MaxTurns: 1})
			if err != nil {
				return "", err
			}
			out, _ := json.Marshal(result)
			return string(out), nil
		},
	})

	_, err := parentLoop.Run(upstreamCtx(t, true), parentConfig)
	require.NoError(t, err)

	spans := exporter.GetSpans()
	one := func(name string) tracetest.SpanStub {
		t.Helper()
		found := spansNamed(spans, name)
		require.Len(t, found, 1, "span %q (spans=%v)", name, spanNames(spans))
		return found[0]
	}
	root := one("invoke_agent stirrup")
	spawn := one("execute_tool spawn_agent")
	sub := one("invoke_agent stirrup.subagent")
	assert.Equal(t, parent.SpanID(), root.Parent.SpanID(), "run span parent span ID")
	requireParent(t, spawn, root)
	requireParent(t, sub, spawn)
	assert.Equal(t, root.SpanContext.TraceID(), sub.SpanContext.TraceID(), "sub-agent trace ID")
}

func TestRunFollowUpLoop_FollowUpStartsNewTraceAfterParentedRun(t *testing.T) {
	loop, exporter := otelLoop(t, endTurnProvider())
	tp := &recordingControlTransport{}
	loop.Transport = tp
	loop.ensureControlRouting()
	config := buildTestConfig()
	parent := upstreamParent(t, true)

	_, err := loop.Run(upstreamCtx(t, true), config)
	require.NoError(t, err)

	sessionCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunFollowUpLoop(sessionCtx, loop, config, 30, FollowUpOptions{
			OnRunComplete: func(*types.RunConfig, *types.RunTrace, error) { ran <- struct{}{} },
		})
	}()
	tp.FireControl(types.ControlEvent{Type: "user_response", UserResponse: "One more thing."})
	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		require.Fail(t, "follow-up run did not complete")
	}
	cancel()
	<-done

	roots := spansNamed(exporter.GetSpans(), "invoke_agent stirrup")
	require.Len(t, roots, 2, "one root span per run")
	assert.Equal(t, parent.SpanID(), roots[0].Parent.SpanID(), "primary run parent span ID")
	assert.False(t, roots[1].Parent.IsValid(), "follow-up run parented on %s, want a new root", roots[1].Parent.SpanID())
	assert.NotEqual(t, parent.TraceID(), roots[1].SpanContext.TraceID(), "follow-up run joined the upstream trace")
}

func TestAsyncToolResultRequest_ForwardsUpstreamParentWithoutOTelSpans(t *testing.T) {
	loop := buildTestLoop(nil)
	loop.Tools.(*tool.Registry).Register(asyncEchoTool())
	loop.Provider = &scriptedProvider{turns: [][]types.StreamEvent{
		{
			{Type: "tool_call", ID: "tc_fwd", Name: "async_echo", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use"},
		},
		{
			{Type: "text_delta", Text: "done"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}}
	tr := newAsyncTestTransport()
	loop.Transport = tr
	parent := upstreamParent(t, true)

	go respondWhenAllRequested(t, tr, 1)
	_, err := loop.Run(upstreamCtx(t, true), buildTestConfig())
	require.NoError(t, err)

	for _, e := range tr.Events() {
		if e.Type != "tool_result_request" {
			continue
		}
		sc := decodeTraceContext(t, e.TraceContext)
		assert.Equal(t, parent.TraceID(), sc.TraceID(), "carrier trace ID")
		assert.Equal(t, parent.SpanID(), sc.SpanID(), "carrier span ID")
		return
	}
	require.Fail(t, "no tool_result_request emitted")
}

func TestLoop_UpstreamParentCorrelatesLogsWithoutOTelSpans(t *testing.T) {
	var stderr bytes.Buffer
	scope := observability.NewRunScope("")
	loop := buildTestLoop(endTurnProvider())
	loop.Logger = observability.NewScopedLoggerWithExport(scope, slog.LevelInfo, &stderr, nil, nil)
	loop.LogScope = scope
	parent := upstreamParent(t, true)

	_, err := loop.Run(upstreamCtx(t, true), buildTestConfig())
	require.NoError(t, err)

	var records int
	sc := bufio.NewScanner(&stderr)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		records++
		assert.Equal(t, parent.TraceID().String(), rec["trace_id"], "record %q trace_id", rec["msg"])
		assert.Equal(t, parent.SpanID().String(), rec["span_id"], "record %q span_id", rec["msg"])
	}
	require.NotZero(t, records, "run logged no records")
}
