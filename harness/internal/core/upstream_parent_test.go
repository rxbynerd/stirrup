package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

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
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := oteltrace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatal(err)
	}
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

	if _, err := loop.Run(upstreamCtx(t, true), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	spans := exporter.GetSpans()
	root := spansNamed(spans, "invoke_agent stirrup")
	if len(root) != 1 {
		t.Fatalf("found %d root spans, want 1", len(root))
	}
	if root[0].Parent.SpanID() != parent.SpanID() || root[0].Parent.TraceID() != parent.TraceID() {
		t.Errorf("run span parent = %s/%s, want the upstream %s/%s", root[0].Parent.TraceID(), root[0].Parent.SpanID(), parent.TraceID(), parent.SpanID())
	}
	for _, s := range spans {
		if s.SpanContext.TraceID() != parent.TraceID() {
			t.Errorf("%s: trace ID = %s, want the upstream trace %s", s.Name, s.SpanContext.TraceID(), parent.TraceID())
		}
	}
	chats := spansNamed(spans, "chat claude-sonnet-4-6")
	if len(chats) != 1 {
		t.Fatalf("found %d chat spans, want 1", len(chats))
	}
	requireParent(t, chats[0], root[0])

	started := eventsOfType(tp, "run_started")
	if len(started) != 1 || started[0].TraceReference == nil {
		t.Fatalf("got %d run_started events, want 1 with a reference", len(started))
	}
	if ref := started[0].TraceReference; ref.TraceID != parent.TraceID().String() || ref.RunSpanID != root[0].SpanContext.SpanID().String() || !ref.Sampled {
		t.Errorf("run_started reference = %+v, want the upstream trace and the run span", *ref)
	}
}

func TestLoop_UnsampledUpstreamParentSuppressesExport(t *testing.T) {
	loop, exporter := otelLoop(t, endTurnProvider())
	tp := &recordingTransport{}
	loop.Transport = tp

	rt, err := loop.Run(upstreamCtx(t, false), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if spans := exporter.GetSpans(); len(spans) != 0 {
		t.Errorf("exported %d spans under an unsampled parent, want none: %v", len(spans), spanNames(spans))
	}
	started := eventsOfType(tp, "run_started")
	if len(started) != 1 || started[0].TraceReference == nil {
		t.Fatalf("got %d run_started events, want 1 with a reference", len(started))
	}
	ref := started[0].TraceReference
	if ref.Sampled {
		t.Error("run_started reports sampled: true under an unsampled parent")
	}
	if ref.TraceID != upstreamParent(t, false).TraceID().String() {
		t.Errorf("run_started trace ID = %s, want the upstream trace", ref.TraceID)
	}
	if rt.TraceReference == nil || rt.TraceReference.Sampled {
		t.Errorf("RunTrace.TraceReference = %+v, want an unsampled reference", rt.TraceReference)
	}
}

func TestLoop_SubAgentRunIgnoresCallerSpan(t *testing.T) {
	for name, configure := range map[string]func(*AgenticLoop){
		"parent run ID set":    func(l *AgenticLoop) { l.ParentRunID = "parent-run" },
		"trace context preset": func(l *AgenticLoop) { l.TraceContext = context.Background() },
	} {
		t.Run(name, func(t *testing.T) {
			loop, exporter := otelLoop(t, endTurnProvider())
			configure(loop)
			if _, err := loop.Run(upstreamCtx(t, true), buildTestConfig()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			root := spansNamed(exporter.GetSpans(), "invoke_agent stirrup")
			if len(root) != 1 {
				t.Fatalf("found %d root spans, want 1", len(root))
			}
			if root[0].Parent.IsValid() {
				t.Errorf("run span parented on %s, want a new root", root[0].Parent.SpanID())
			}
		})
	}
}

func TestSpawnSubAgent_UnderUpstreamParentNestsUnderSpawnTool(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	emitter := trace.NewOTelTraceEmitterForTest(tp)

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

	if _, err := parentLoop.Run(upstreamCtx(t, true), parentConfig); err != nil {
		t.Fatalf("Run: %v", err)
	}

	spans := exporter.GetSpans()
	one := func(name string) tracetest.SpanStub {
		t.Helper()
		found := spansNamed(spans, name)
		if len(found) != 1 {
			t.Fatalf("found %d %q spans, want 1 (spans=%v)", len(found), name, spanNames(spans))
		}
		return found[0]
	}
	root := one("invoke_agent stirrup")
	spawn := one("execute_tool spawn_agent")
	sub := one("invoke_agent stirrup.subagent")
	if root.Parent.SpanID() != upstreamParent(t, true).SpanID() {
		t.Errorf("run span parent = %s, want the upstream span", root.Parent.SpanID())
	}
	requireParent(t, spawn, root)
	requireParent(t, sub, spawn)
	if sub.SpanContext.TraceID() != root.SpanContext.TraceID() {
		t.Errorf("sub-agent trace ID = %s, want the run's %s", sub.SpanContext.TraceID(), root.SpanContext.TraceID())
	}
}

func TestRunFollowUpLoop_FollowUpStartsNewTraceAfterParentedRun(t *testing.T) {
	loop, exporter := otelLoop(t, endTurnProvider())
	tp := &recordingControlTransport{}
	loop.Transport = tp
	loop.ensureControlRouting()
	config := buildTestConfig()
	parent := upstreamParent(t, true)

	if _, err := loop.Run(upstreamCtx(t, true), config); err != nil {
		t.Fatalf("Run: %v", err)
	}

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
		t.Fatal("follow-up run did not complete")
	}
	cancel()
	<-done

	roots := spansNamed(exporter.GetSpans(), "invoke_agent stirrup")
	if len(roots) != 2 {
		t.Fatalf("found %d root spans, want one per run", len(roots))
	}
	if roots[0].Parent.SpanID() != parent.SpanID() {
		t.Errorf("primary run parent = %s, want the upstream span", roots[0].Parent.SpanID())
	}
	if roots[1].Parent.IsValid() {
		t.Errorf("follow-up run parented on %s, want a new root", roots[1].Parent.SpanID())
	}
	if roots[1].SpanContext.TraceID() == parent.TraceID() {
		t.Error("follow-up run joined the upstream trace")
	}
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
	if _, err := loop.Run(upstreamCtx(t, true), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, e := range tr.Events() {
		if e.Type != "tool_result_request" {
			continue
		}
		sc := decodeTraceContext(t, e.TraceContext)
		if sc.TraceID() != parent.TraceID() || sc.SpanID() != parent.SpanID() {
			t.Errorf("carrier = %s/%s, want the upstream parent %s/%s", sc.TraceID(), sc.SpanID(), parent.TraceID(), parent.SpanID())
		}
		return
	}
	t.Fatal("no tool_result_request emitted")
}

func TestLoop_UpstreamParentCorrelatesLogsWithoutOTelSpans(t *testing.T) {
	var stderr bytes.Buffer
	scope := observability.NewRunScope("")
	loop := buildTestLoop(endTurnProvider())
	loop.Logger = observability.NewScopedLoggerWithExport(scope, slog.LevelInfo, &stderr, nil, nil)
	loop.LogScope = scope
	parent := upstreamParent(t, true)

	if _, err := loop.Run(upstreamCtx(t, true), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var records int
	sc := bufio.NewScanner(&stderr)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		records++
		if rec["trace_id"] != parent.TraceID().String() || rec["span_id"] != parent.SpanID().String() {
			t.Errorf("record %q correlates with %v/%v, want the upstream parent", rec["msg"], rec["trace_id"], rec["span_id"])
		}
	}
	if records == 0 {
		t.Fatal("run logged no records")
	}
}
