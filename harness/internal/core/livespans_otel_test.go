package core

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
	if child.Parent.SpanID() != parent.SpanContext.SpanID() {
		t.Errorf("%s: parent span ID = %s, want %s (%s)", child.Name, child.Parent.SpanID(), parent.SpanContext.SpanID(), parent.Name)
	}
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
	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	spans := exporter.GetSpans()
	if n := len(spansNamed(spans, "run")); n != 0 {
		t.Errorf("found %d legacy run spans", n)
	}
	root := spansNamed(spans, "invoke_agent stirrup")
	if len(root) != 1 {
		t.Fatalf("found %d root spans, want 1", len(root))
	}
	chats := spansNamed(spans, "chat claude-sonnet-4-6")
	if len(chats) != 2 {
		t.Fatalf("found %d chat spans, want 2", len(chats))
	}
	streams := spansNamed(spans, "provider.stream")
	if len(streams) != 2 {
		t.Fatalf("found %d provider.stream spans, want 2", len(streams))
	}
	for i, chat := range chats {
		if chat.SpanKind != oteltrace.SpanKindClient {
			t.Errorf("chat %d kind = %v, want client", i, chat.SpanKind)
		}
		requireParent(t, chat, root[0])
		requireParent(t, streams[i], chat)
		if got, _ := spanAttr(chat, "turn.number"); got != []string{"0", "1"}[i] {
			t.Errorf("chat %d turn.number = %s", i, got)
		}
	}
	if got, _ := spanAttr(chats[0], "turn.tool_calls"); got != "1" {
		t.Errorf("turn.tool_calls = %s, want 1", got)
	}
	tools := spansNamed(spans, "execute_tool test_tool")
	if len(tools) != 1 {
		t.Fatalf("found %d execute_tool spans, want 1", len(tools))
	}
	if got, _ := spanAttr(tools[0], "turn.number"); got != "0" {
		t.Errorf("tool span turn.number = %s, want the emitting chat span's 0", got)
	}
	if got, _ := spanAttr(chats[1], "gen_ai.usage.input_tokens"); got != "12" {
		t.Errorf("second chat input tokens = %s, want 12", got)
	}
	for _, s := range spans {
		if len(s.Name) > 4 && s.Name[:5] == "turn[" {
			t.Errorf("legacy turn span %q exported", s.Name)
		}
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != "success" || len(prov.requests) != 5 {
		t.Fatalf("prerequisite: outcome %q after %d requests, want success after 5", result.Outcome, len(prov.requests))
	}

	var turn3 []tracetest.SpanStub
	for _, s := range spansNamed(exporter.GetSpans(), "chat claude-sonnet-4-6") {
		if got, _ := spanAttr(s, "turn.number"); got == "3" {
			turn3 = append(turn3, s)
		}
	}
	if len(turn3) != 2 {
		t.Fatalf("turn 3 has %d chat spans, want 2 (rejected request and retry)", len(turn3))
	}
	if got, _ := spanAttr(turn3[0], "error.type"); got != "context_overflow" {
		t.Errorf("rejected request error.type = %q, want context_overflow", got)
	}
	if _, ok := spanAttr(turn3[1], "error.type"); ok {
		t.Error("successful retry carries error.type")
	}
}

func TestLoop_OTelWindowFillingResponseEndsItsChatSpan(t *testing.T) {
	loop, prov, _, _ := buildOverflowLoop(t, overflowStepsByKind()["context window stop"])
	emitter, exporter := newInMemoryOTelEmitter(t)
	loop.Trace = emitter
	loop.Tracer = emitter.Tracer()
	config := buildTestConfig()
	config.ContextStrategy.MaxTokens = 200_000

	result, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != "success" || len(prov.requests) != 5 {
		t.Fatalf("prerequisite: outcome %q after %d requests, want success after 5", result.Outcome, len(prov.requests))
	}

	var turn3 []tracetest.SpanStub
	for _, s := range spansNamed(exporter.GetSpans(), "chat claude-sonnet-4-6") {
		if got, _ := spanAttr(s, "turn.number"); got == "3" {
			turn3 = append(turn3, s)
		}
	}
	if len(turn3) != 2 {
		t.Fatalf("turn 3 has %d chat spans, want 2 (window-filling response and retry)", len(turn3))
	}
	filled := turn3[0]
	if got, _ := spanAttr(filled, "error.type"); got != "context_overflow" {
		t.Errorf("window-filling request error.type = %q, want context_overflow", got)
	}
	if _, ok := spanAttr(filled, "stirrup.span.abandoned"); ok {
		t.Error("window-filling request span was abandoned rather than ended by the loop")
	}
	if got, _ := spanAttr(filled, "gen_ai.usage.output_tokens"); got != "40" {
		t.Errorf("window-filling request output tokens = %q, want 40", got)
	}
	if _, ok := spanAttr(turn3[1], "error.type"); ok {
		t.Error("successful retry carries error.type")
	}
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
	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	spans := exporter.GetSpans()
	root := spansNamed(spans, "invoke_agent stirrup")[0]
	slow := spansNamed(spans, "execute_tool slow_mutating_tool")
	fast := spansNamed(spans, "execute_tool test_tool")
	if len(slow) != 1 || len(fast) != 1 {
		t.Fatalf("found %d and %d execute_tool spans, want one per call", len(slow), len(fast))
	}
	for _, s := range spans {
		if strings.HasPrefix(s.Name, "tool.") {
			t.Errorf("legacy tool span %q exported", s.Name)
		}
	}
	requireParent(t, slow[0], root)
	requireParent(t, fast[0], root)
	perm := spansNamed(spans, "permission.check")
	if len(perm) != 1 {
		t.Fatalf("found %d permission.check spans, want 1", len(perm))
	}
	requireParent(t, perm[0], slow[0])
	if slow[0].EndTime.After(fast[0].StartTime) {
		t.Errorf("first call's span ends at %v, after the second call's span starts at %v", slow[0].EndTime, fast[0].StartTime)
	}
	if got, _ := spanAttr(slow[0], "gen_ai.tool.call.id"); got != "tc_0" {
		t.Errorf("gen_ai.tool.call.id = %q", got)
	}
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	durations := map[string]int64{}
	for _, call := range runTrace.ToolCalls {
		durations[call.ID] = call.DurationMs
	}
	if durations["tc_slow"] < 60 {
		t.Fatalf("prerequisite: slow call took %dms, want at least 60ms", durations["tc_slow"])
	}
	if durations["tc_fast"] >= 60 {
		t.Errorf("fast call recorded %dms, which includes the slow call after it", durations["tc_fast"])
	}
}

func TestLoop_OTelChatSpanCarriesProviderEndpoint(t *testing.T) {
	loop, exporter := otelLoop(t, nil)
	loop.Provider = endpointProvider{&scriptedTurnProvider{scripts: []turnScript{{events: []types.StreamEvent{
		{Type: "text_delta", Text: "done"},
		{Type: "message_complete", StopReason: "end_turn"},
	}}}}}
	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	chats := spansNamed(exporter.GetSpans(), "chat claude-sonnet-4-6")
	if len(chats) != 1 {
		t.Fatalf("found %d chat spans, want 1", len(chats))
	}
	if got, _ := spanAttr(chats[0], "server.address"); got != "api.anthropic.com" {
		t.Errorf("server.address = %q", got)
	}
	if got, _ := spanAttr(chats[0], "server.port"); got != "443" {
		t.Errorf("server.port = %q", got)
	}
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
		if err == nil {
			t.Error("provider context finished without an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provider request was not cancelled with the run")
	}
	rt := <-result
	if rt.Outcome != "cancelled" {
		t.Errorf("outcome = %q, want cancelled", rt.Outcome)
	}
	chats := spansNamed(exporter.GetSpans(), "chat claude-sonnet-4-6")
	if len(chats) != 1 {
		t.Fatalf("found %d chat spans, want 1", len(chats))
	}
	if got, _ := spanAttr(chats[0], "error.type"); got != "cancelled" {
		t.Errorf("chat error.type = %q, want cancelled", got)
	}
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
			if _, err := loop.Run(context.Background(), config); err != nil {
				t.Fatalf("Run: %v", err)
			}
			chats := spansNamed(exporter.GetSpans(), "chat claude-sonnet-4-6")
			if len(chats) != 1 {
				t.Fatalf("found %d chat spans, want 1", len(chats))
			}
			got, _ := spanAttr(chats[0], "gen_ai.request.temperature")
			if got != tc.want {
				t.Errorf("gen_ai.request.temperature = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoop_OTelProviderRequestPastDeadlineIsTimeout(t *testing.T) {
	loop, exporter := otelLoop(t, nil)
	loop.Provider = &ctxBlockingProvider{entered: make(chan struct{}), done: make(chan error, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	rt, _ := loop.Run(ctx, buildTestConfig())
	if rt.Outcome != "timeout" {
		t.Fatalf("prerequisite: outcome = %q, want timeout", rt.Outcome)
	}
	chats := spansNamed(exporter.GetSpans(), "chat claude-sonnet-4-6")
	if len(chats) != 1 {
		t.Fatalf("found %d chat spans, want 1", len(chats))
	}
	if got, _ := spanAttr(chats[0], "error.type"); got != "timeout" {
		t.Errorf("chat error.type = %q, want timeout", got)
	}
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	root := spansNamed(exporter.GetSpans(), "invoke_agent stirrup")
	if len(root) != 1 {
		t.Fatalf("found %d root spans, want 1", len(root))
	}
	want := types.TraceReference{
		TraceID: root[0].SpanContext.TraceID().String(),
		SpanID:  root[0].SpanContext.SpanID().String(),
		Sampled: true,
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
	if len(started) != 1 {
		t.Fatalf("got %d run_started events, want 1", len(started))
	}
	if readyAt < 0 || startedAt <= readyAt || firstModelAt <= startedAt {
		t.Errorf("event order ready=%d run_started=%d first model event=%d, want ready before run_started before the first model event", readyAt, startedAt, firstModelAt)
	}
	if started[0].ID != config.RunID {
		t.Errorf("run_started ID = %q, want the run ID %q", started[0].ID, config.RunID)
	}
	if got := started[0].TraceReference; got == nil || *got != want {
		t.Errorf("run_started reference = %+v, want the exported root span %+v", got, want)
	}
	if rt.TraceReference == nil || *rt.TraceReference != want {
		t.Errorf("RunTrace.TraceReference = %+v, want %+v", rt.TraceReference, want)
	}

	done := eventsOfType(tp, "done")
	if len(done) != 1 || done[0].Trace == nil {
		t.Fatalf("got %d done events, want 1 with a summary", len(done))
	}
	if got := done[0].Trace.TraceReference; got == nil || *got != want {
		t.Errorf("done summary reference = %+v, want %+v", got, want)
	}
}

func TestLoop_RunStartedAbsentWithoutOTelSpan(t *testing.T) {
	loop := buildTestLoop(&mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "done"},
		{Type: "message_complete", StopReason: "end_turn"},
	}})
	tp := &recordingTransport{}
	loop.Transport = tp

	rt, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := eventsOfType(tp, "run_started"); len(got) != 0 {
		t.Errorf("JSONL-traced run emitted %d run_started events, want none", len(got))
	}
	if rt.TraceReference != nil {
		t.Errorf("RunTrace.TraceReference = %+v, want nil", rt.TraceReference)
	}
	for _, e := range tp.events {
		if e.TraceReference != nil || (e.Trace != nil && e.Trace.TraceReference != nil) {
			t.Errorf("%s event carries a trace reference without an OTel span", e.Type)
		}
	}
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

	if _, err := loop.Run(context.Background(), config); err != nil {
		t.Fatalf("Run: %v", err)
	}

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
		t.Fatal("follow-up run did not complete")
	}
	cancel()
	<-done

	started := eventsOfType(&tp.rec, "run_started")
	if len(started) != 2 {
		t.Fatalf("got %d run_started events, want one per run", len(started))
	}
	if started[0].ID != "test-run-1" || started[1].ID != followUpRunID {
		t.Errorf("run_started IDs = %q, %q, want test-run-1, %q", started[0].ID, started[1].ID, followUpRunID)
	}
	if started[0].TraceReference.TraceID == started[1].TraceReference.TraceID {
		t.Errorf("follow-up run reused trace ID %s", started[1].TraceReference.TraceID)
	}
}
