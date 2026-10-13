package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"testing"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	tracepkg "github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

type exportedLog struct {
	message string
	traceID oteltrace.TraceID
	spanID  oteltrace.SpanID
}

// memoryLogExporter keeps the message and correlation IDs of every
// record the OTel logs SDK exports.
type memoryLogExporter struct {
	mu      sync.Mutex
	records []exportedLog
}

func (e *memoryLogExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range records {
		r := &records[i]
		e.records = append(e.records, exportedLog{
			message: r.Body().AsString(),
			traceID: r.TraceID(),
			spanID:  r.SpanID(),
		})
	}
	return nil
}

func (e *memoryLogExporter) Shutdown(context.Context) error   { return nil }
func (e *memoryLogExporter) ForceFlush(context.Context) error { return nil }

func (e *memoryLogExporter) all() []exportedLog {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]exportedLog(nil), e.records...)
}

// TestLoop_LogRecordsCorrelateWithRunAndRequestSpans pins log-to-trace
// correlation on both log sinks: every record carries the run's trace
// ID, per-request and per-tool records carry their chat or tool span,
// a sub-agent's records carry the sub-agent's spans, and a record
// logged without a context falls back to the run span.
func TestLoop_LogRecordsCorrelateWithRunAndRequestSpans(t *testing.T) {
	spanExporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spanExporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	emitter := tracepkg.NewOTelTraceEmitterForTest(tp)

	logExporter := &memoryLogExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logExporter)))
	t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })
	var stderr bytes.Buffer
	scope := observability.NewRunScope("")
	logger := observability.NewScopedLoggerWithExport(scope, slog.LevelInfo, &stderr, nil,
		otelslog.NewHandler("test", otelslog.WithLoggerProvider(lp)))

	parentProv := &multiCallProviderForOTel{
		calls: [][]types.StreamEvent{
			{
				{Type: "tool_call", ID: "tc_spawn_1", Name: "spawn_agent", Input: map[string]any{"prompt": "do a subtask"}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "Done."},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}
	childProv := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Sub-agent reply."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        "spawn_agent",
		Description: "Spawn a sub-agent",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"]}`),
	})

	parentLoop := &AgenticLoop{
		Provider:    parentProv,
		Router:      router.NewStaticRouter("anthropic", "claude-sonnet-4-6"),
		Prompt:      prompt.NewDefaultPromptBuilder(),
		Context:     contextpkg.NewSlidingWindowStrategy(),
		Tools:       registry,
		Edit:        edit.NewWholeFileStrategy(),
		Verifier:    verifier.NewNoneVerifier(),
		Permissions: permission.NewAllowAll(),
		Git:         git.NewNoneGitStrategy(),
		Transport:   transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}),
		Trace:       emitter,
		Tracer:      emitter.Tracer(),
		Metrics:     observability.NewNoopMetrics(),
		Logger:      logger,
		LogScope:    scope,
		GuardRail:   &phaseSpecificDenyGuard{denyPhase: "none"},
	}
	parentConfig := buildTestConfig()
	parentConfig.RunID = "parent-run-logs"

	registry.Resolve("spawn_agent").Handler = func(ctx context.Context, input json.RawMessage) (string, error) {
		var args struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return "", err
		}
		logger.Info("component record without context")
		origProv := parentLoop.Provider
		parentLoop.Provider = childProv
		defer func() { parentLoop.Provider = origProv }()
		result, err := SpawnSubAgent(ctx, parentLoop, parentConfig, SubAgentConfig{Prompt: args.Prompt, MaxTurns: 1})
		if err != nil {
			return "", err
		}
		out, _ := json.Marshal(result)
		return string(out), nil
	}

	if _, err := parentLoop.Run(context.Background(), parentConfig); err != nil {
		t.Fatalf("parent Run: %v", err)
	}

	spans := spanExporter.GetSpans()
	one := func(name string) tracetest.SpanStub {
		t.Helper()
		found := spansNamed(spans, name)
		if len(found) != 1 {
			t.Fatalf("found %d %q spans, want 1", len(found), name)
		}
		return found[0]
	}
	root := one("invoke_agent stirrup")
	spawn := one("execute_tool spawn_agent")
	sub := one("invoke_agent stirrup.subagent")
	var parentChats, childChats []tracetest.SpanStub
	for _, chat := range spansNamed(spans, "chat claude-sonnet-4-6") {
		switch chat.Parent.SpanID() {
		case root.SpanContext.SpanID():
			parentChats = append(parentChats, chat)
		case sub.SpanContext.SpanID():
			childChats = append(childChats, chat)
		}
	}
	if len(parentChats) != 2 || len(childChats) != 1 {
		t.Fatalf("found %d parent and %d child chat spans, want 2 and 1", len(parentChats), len(childChats))
	}
	sort.Slice(parentChats, func(i, j int) bool { return parentChats[i].StartTime.Before(parentChats[j].StartTime) })

	traceID := root.SpanContext.TraceID()
	spanOf := map[oteltrace.SpanID]string{
		root.SpanContext.SpanID():           "root",
		spawn.SpanContext.SpanID():          "spawn tool",
		sub.SpanContext.SpanID():            "sub-agent",
		parentChats[0].SpanContext.SpanID(): "parent chat 1",
		parentChats[1].SpanContext.SpanID(): "parent chat 2",
		childChats[0].SpanContext.SpanID():  "child chat",
	}
	want := map[string][]string{
		"run started":                      {"root", "sub-agent"},
		"turn started":                     {"root", "sub-agent", "root"},
		"turn completed":                   {"parent chat 1", "child chat", "parent chat 2"},
		"tool dispatched":                  {"spawn tool"},
		"component record without context": {"root"},
		"run finished":                     {"sub-agent", "root"},
	}

	exported := logExporter.all()
	if len(exported) == 0 {
		t.Fatal("no log records exported")
	}
	got := map[string][]string{}
	for _, r := range exported {
		if r.traceID != traceID {
			t.Errorf("OTLP record %q has trace ID %s, want the run's %s", r.message, r.traceID, traceID)
		}
		got[r.message] = append(got[r.message], spanOf[r.spanID])
	}
	for message, spans := range want {
		if !slices.Equal(got[message], spans) {
			t.Errorf("OTLP %q records correlate with %v, want %v", message, got[message], spans)
		}
	}

	gotStderr := map[string][]string{}
	lines := 0
	scanner := bufio.NewScanner(&stderr)
	for scanner.Scan() {
		lines++
		var line struct {
			Msg     string `json:"msg"`
			TraceID string `json:"trace_id"`
			SpanID  string `json:"span_id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("stderr line is not JSON: %v\n%s", err, scanner.Text())
		}
		if line.TraceID != traceID.String() {
			t.Errorf("stderr record %q has trace_id %q, want %s", line.Msg, line.TraceID, traceID)
		}
		spanID, err := oteltrace.SpanIDFromHex(line.SpanID)
		if err != nil {
			t.Errorf("stderr record %q has span_id %q: %v", line.Msg, line.SpanID, err)
			continue
		}
		gotStderr[line.Msg] = append(gotStderr[line.Msg], spanOf[spanID])
	}
	if lines != len(exported) {
		t.Errorf("stderr has %d records, OTLP has %d", lines, len(exported))
	}
	for message, spans := range want {
		if !slices.Equal(gotStderr[message], spans) {
			t.Errorf("stderr %q records correlate with %v, want %v", message, gotStderr[message], spans)
		}
	}
}

// TestLoop_LogRecordsCarryNoCorrelationWithoutARunSpan pins that a run
// whose emitter opens no span leaves records uncorrelated, rather than
// correlating them with an earlier run's span.
func TestLoop_LogRecordsCarryNoCorrelationWithoutARunSpan(t *testing.T) {
	var stderr bytes.Buffer
	scope := observability.NewRunScope("")
	scope.SetSpanContext(oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID: oteltrace.TraceID{1}, SpanID: oteltrace.SpanID{1}, TraceFlags: oteltrace.FlagsSampled,
	}))
	logger := observability.NewScopedLoggerWithExport(scope, slog.LevelInfo, &stderr, nil, nil)

	loop := buildTestLoop(&mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "hi"},
		{Type: "message_complete", StopReason: "end_turn"},
	}})
	loop.Logger = logger
	loop.LogScope = scope
	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	scanner := bufio.NewScanner(&stderr)
	lines := 0
	for scanner.Scan() {
		lines++
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("stderr line is not JSON: %v", err)
		}
		if _, ok := line["trace_id"]; ok {
			t.Errorf("record %q carries trace_id without a run span", line["msg"])
		}
	}
	if lines == 0 {
		t.Fatal("no records logged")
	}
}
