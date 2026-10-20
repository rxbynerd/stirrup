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
	must := require.New(t)
	traceID, err := oteltrace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	must.NoError(err)
	spanID, err := oteltrace.SpanIDFromHex("00f067aa0ba902b7")
	must.NoError(err)
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
	check := assert.New(t)
	must := require.New(t)
	loop, exporter := otelLoop(t, endTurnProvider())
	tp := &recordingTransport{}
	loop.Transport = tp
	parent := upstreamParent(t, true)

	_, err := loop.Run(upstreamCtx(t, true), buildTestConfig())
	must.NoError(err)

	spans := exporter.GetSpans()
	root := spansNamed(spans, "invoke_agent stirrup")
	must.Len(root, 1)
	check.Equal(parent.SpanID(), root[0].Parent.SpanID(), "run span parent span ID")
	check.Equal(parent.TraceID(), root[0].Parent.TraceID(), "run span parent trace ID")
	for _, s := range spans {
		check.Equal(parent.TraceID(), s.SpanContext.TraceID(), "%s trace ID", s.Name)
	}
	chats := spansNamed(spans, "chat claude-sonnet-4-6")
	must.Len(chats, 1)
	requireParent(t, chats[0], root[0])

	started := eventsOfType(tp, "run_started")
	must.Len(started, 1)
	must.NotNil(started[0].TraceReference)
	ref := started[0].TraceReference
	check.Equal(parent.TraceID().String(), ref.TraceID, "run_started trace ID")
	check.Equal(root[0].SpanContext.SpanID().String(), ref.RunSpanID, "run_started run span ID")
	check.True(ref.Sampled, "run_started sampled")
}

func TestLoop_UnsampledUpstreamParentSuppressesExport(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop, exporter := otelLoop(t, endTurnProvider())
	tp := &recordingTransport{}
	loop.Transport = tp

	rt, err := loop.Run(upstreamCtx(t, false), buildTestConfig())
	must.NoError(err)

	spans := exporter.GetSpans()
	check.Empty(spans, "spans exported under an unsampled parent: %v", spanNames(spans))

	started := eventsOfType(tp, "run_started")
	must.Len(started, 1)
	must.NotNil(started[0].TraceReference)
	ref := started[0].TraceReference
	check.False(ref.Sampled, "run_started sampled under an unsampled parent")
	check.Equal(upstreamParent(t, false).TraceID().String(), ref.TraceID, "run_started trace ID")

	must.NotNil(rt.TraceReference)
	check.False(rt.TraceReference.Sampled, "RunTrace.TraceReference sampled")
}

func TestLoop_SubAgentRunIgnoresCallerSpan(t *testing.T) {
	for name, configure := range map[string]func(*AgenticLoop){
		"parent run ID set":    func(l *AgenticLoop) { l.ParentRunID = "parent-run" },
		"trace context preset": func(l *AgenticLoop) { l.TraceContext = context.Background() },
	} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			loop, exporter := otelLoop(t, endTurnProvider())
			configure(loop)

			_, err := loop.Run(upstreamCtx(t, true), buildTestConfig())
			must.NoError(err)

			root := spansNamed(exporter.GetSpans(), "invoke_agent stirrup")
			must.Len(root, 1)
			must.False(root[0].Parent.IsValid(), "run span parented on %s, want a new root", root[0].Parent.SpanID())
		})
	}
}

func TestSpawnSubAgent_UnderUpstreamParentNestsUnderSpawnTool(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)

	spans := exporter.GetSpans()
	one := func(name string) tracetest.SpanStub {
		t.Helper()
		found := spansNamed(spans, name)
		must.Len(found, 1, "span %q (spans=%v)", name, spanNames(spans))
		return found[0]
	}
	root := one("invoke_agent stirrup")
	spawn := one("execute_tool spawn_agent")
	sub := one("invoke_agent stirrup.subagent")
	check.Equal(parent.SpanID(), root.Parent.SpanID(), "run span parent span ID")
	requireParent(t, spawn, root)
	requireParent(t, sub, spawn)
	check.Equal(root.SpanContext.TraceID(), sub.SpanContext.TraceID(), "sub-agent trace ID")
}

func TestRunFollowUpLoop_FollowUpStartsNewTraceAfterParentedRun(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	loop, exporter := otelLoop(t, endTurnProvider())
	tp := &recordingControlTransport{}
	loop.Transport = tp
	loop.ensureControlRouting()
	config := buildTestConfig()
	parent := upstreamParent(t, true)

	_, err := loop.Run(upstreamCtx(t, true), config)
	must.NoError(err)

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
		must.Fail("follow-up run did not complete")
	}
	cancel()
	<-done

	roots := spansNamed(exporter.GetSpans(), "invoke_agent stirrup")
	must.Len(roots, 2, "one root span per run")
	check.Equal(parent.SpanID(), roots[0].Parent.SpanID(), "primary run parent span ID")
	check.False(roots[1].Parent.IsValid(), "follow-up run parented on %s, want a new root", roots[1].Parent.SpanID())
	check.NotEqual(parent.TraceID(), roots[1].SpanContext.TraceID(), "follow-up run joined the upstream trace")
}

func TestAsyncToolResultRequest_ForwardsUpstreamParentWithoutOTelSpans(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)

	for _, e := range tr.Events() {
		if e.Type != "tool_result_request" {
			continue
		}
		sc := decodeTraceContext(t, e.TraceContext)
		check.Equal(parent.TraceID(), sc.TraceID(), "carrier trace ID")
		check.Equal(parent.SpanID(), sc.SpanID(), "carrier span ID")
		return
	}
	must.Fail("no tool_result_request emitted")
}

func TestLoop_UpstreamParentCorrelatesLogsWithoutOTelSpans(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var stderr bytes.Buffer
	scope := observability.NewRunScope("")
	loop := buildTestLoop(endTurnProvider())
	loop.Logger = observability.NewScopedLoggerWithExport(scope, slog.LevelInfo, &stderr, nil, nil)
	loop.LogScope = scope
	parent := upstreamParent(t, true)

	_, err := loop.Run(upstreamCtx(t, true), buildTestConfig())
	must.NoError(err)

	var records int
	sc := bufio.NewScanner(&stderr)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		records++
		check.Equal(parent.TraceID().String(), rec["trace_id"], "record %q trace_id", rec["msg"])
		check.Equal(parent.SpanID().String(), rec["span_id"], "record %q span_id", rec["msg"])
	}
	must.NotZero(records, "run logged no records")
}
