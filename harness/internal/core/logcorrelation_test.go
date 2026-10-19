package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	_, err := parentLoop.Run(context.Background(), parentConfig)
	require.NoError(t, err)

	spans := spanExporter.GetSpans()
	one := func(name string) tracetest.SpanStub {
		t.Helper()
		found := spansNamed(spans, name)
		require.Len(t, found, 1, "spans named %q", name)
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
	require.Len(t, parentChats, 2, "chat spans under the root span")
	require.Len(t, childChats, 1, "chat spans under the sub-agent span")
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
	require.NotEmpty(t, exported, "no log records exported")
	got := map[string][]string{}
	for _, r := range exported {
		assert.Equal(t, traceID, r.traceID, "OTLP record %q trace ID", r.message)
		got[r.message] = append(got[r.message], spanOf[r.spanID])
	}
	for message, wantSpans := range want {
		t.Run("otlp "+message, func(t *testing.T) {
			require.Equal(t, wantSpans, got[message])
		})
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
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &line), "stderr line is not JSON: %s", scanner.Text())
		assert.Equal(t, traceID.String(), line.TraceID, "stderr record %q trace_id", line.Msg)
		spanID, err := oteltrace.SpanIDFromHex(line.SpanID)
		require.NoError(t, err, "stderr record %q span_id %q", line.Msg, line.SpanID)
		gotStderr[line.Msg] = append(gotStderr[line.Msg], spanOf[spanID])
	}
	assert.Equal(t, len(exported), lines, "stderr record count, compared with OTLP record count")
	for message, wantSpans := range want {
		t.Run("stderr "+message, func(t *testing.T) {
			require.Equal(t, wantSpans, gotStderr[message])
		})
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
	_, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)

	scanner := bufio.NewScanner(&stderr)
	lines := 0
	for scanner.Scan() {
		lines++
		var line map[string]any
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &line), "stderr line is not JSON: %s", scanner.Text())
		assert.NotContains(t, line, "trace_id", "record %q carries trace_id without a run span", line["msg"])
	}
	require.NotZero(t, lines, "no records logged")
}
