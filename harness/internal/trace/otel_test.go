package trace

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

func newTestOTelEmitter() (*OTelTraceEmitter, *tracetest.InMemoryExporter) {
	return newTestOTelEmitterWithOpts(observability.ResourceOptions{})
}

// newTestOTelEmitterWithOpts mirrors NewOTelTraceEmitter's TracerProvider
// construction with a caller-supplied ResourceOptions so resource
// attributes (deployment.environment.name, service.namespace, harness.run.mode)
// can be asserted end-to-end on emitted spans.
func newTestOTelEmitterWithOpts(opts observability.ResourceOptions) (*OTelTraceEmitter, *tracetest.InMemoryExporter) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithResource(observability.BuildResource(opts)),
	)
	emitter := newOTelTraceEmitterForTest(tp, false)
	return emitter, exporter
}

func TestOTelTraceEmitter_FullLifecycle(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	emitter, exporter := newTestOTelEmitter()

	timeout := 300
	config := &types.RunConfig{
		RunID:    "run-otel-1",
		Mode:     "execution",
		MaxTurns: 20,
		Timeout:  &timeout,
		Provider: types.ProviderConfig{
			Type:      "anthropic",
			APIKeyRef: "secret://ANTHROPIC_KEY",
		},
		ModelRouter: types.ModelRouterConfig{
			Model: "claude-sonnet-4-6",
		},
	}

	emitter.Start("run-otel-1", config)

	emitter.RecordTurn(types.TurnTrace{
		Turn:       1,
		Tokens:     types.TokenUsage{Input: 100, Output: 50, CacheRead: 60, CacheWrite: 30, Reasoning: 10},
		ToolCalls:  2,
		StopReason: "tool_use",
		DurationMs: 1500,
	})
	emitter.RecordTurn(types.TurnTrace{
		Turn:       2,
		Tokens:     types.TokenUsage{Input: 200, Output: 75, CacheRead: 150, Reasoning: 5},
		ToolCalls:  0,
		StopReason: "end_turn",
		DurationMs: 800,
	})

	emitter.RecordToolCall(types.ToolCallTrace{
		Name:       "read_file",
		DurationMs: 10,
		Success:    true,
	})
	emitter.RecordToolCall(types.ToolCallTrace{
		Name:       "write_file",
		DurationMs: 25,
		Success:    false,
	})
	emitter.RecordTurnRecord(captureTurnRecord())

	trace, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	check.Equal("run-otel-1", trace.ID)
	check.Equal(2, trace.Turns)
	wantTokens := types.TokenUsage{Input: 300, Output: 125, CacheRead: 210, CacheWrite: 30, Reasoning: 15}
	check.Equal(wantTokens, trace.TokenUsage)
	check.Len(trace.ToolCalls, 2)
	check.Equal("success", trace.Outcome)
	check.Equal("secret://[REDACTED]", trace.Config.Provider.APIKeyRef, "API key reference should be redacted")

	spans := exporter.GetSpans()
	// The Record* methods feed the aggregate only; requests and tool
	// calls become spans through the live handles, so only the root span
	// is exported.
	must.Len(spans, 1, "expected only the root span to be exported")
	root := findSpanByName(t, spans, "invoke_agent stirrup")

	assertAttribute(t, root, "run.id", "run-otel-1")
	assertAttribute(t, root, "run.mode", "execution")
	assertAttribute(t, root, "run.outcome", "success")
	assertAttribute(t, root, "harness.version", "dev")

	// GenAI semconv assertions: provider and model surface under the
	// GenAI namespace so vendor-shipped APM dashboards recognise the
	// spans.
	assertAttribute(t, root, genAIProviderNameKey, "anthropic")
	assertAttribute(t, root, genAIRequestModelKey, "claude-sonnet-4-6")
}

func TestOTelTraceEmitter_EmptyRun(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	emitter, exporter := newTestOTelEmitter()

	emitter.Start("run-empty", nil)

	trace, err := emitter.Finish(context.Background(), "error")
	must.NoError(err)

	check.Zero(trace.Turns)
	check.Equal("error", trace.Outcome)

	spans := exporter.GetSpans()
	must.Len(spans, 1, "expected 1 span for empty run")
	check.Equal("invoke_agent stirrup", spans[0].Name)
}

// Every span carries the default service identity, so OTel-aware backends
// (Zipkin, Jaeger, Tempo, ...) do not fall back to showing
// "unknown_service:<binary>" for spans from a TracerProvider without a Resource.
func TestOTelTraceEmitter_ResourceAttributes(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	t.Setenv("OTEL_SERVICE_NAME", "")
	emitter, exporter := newTestOTelEmitter()

	emitter.Start("run-resource", nil)
	emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{Name: "read_file", Known: true}).End(ToolSpanEnd{})
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	spans := exporter.GetSpans()
	must.NotEmpty(spans, "expected at least one span")

	for _, span := range spans {
		if !check.NotNil(span.Resource, "span %q: TracerProvider was built without WithResource", span.Name) {
			continue
		}
		got := make(map[string]string)
		for _, kv := range span.Resource.Attributes() {
			got[string(kv.Key)] = kv.Value.AsString()
		}
		check.Equal(observability.DefaultServiceName, got["service.name"], "span %q", span.Name)
		check.NotEmpty(got["service.version"], "span %q: service.version", span.Name)
		check.NotEmpty(got["service.instance.id"], "span %q: service.instance.id", span.Name)
	}
}

// Operator-supplied ResourceOptions reach every emitted span through the
// TracerProvider's Resource, so backend queries grouping by
// deployment.environment.name see the values on every span.
func TestOTelTraceEmitter_ResourceAttributesOnSpan(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	emitter, exporter := newTestOTelEmitterWithOpts(observability.ResourceOptions{
		Environment:      "test-env",
		ServiceNamespace: "test-ns",
		RunMode:          "execution",
	})

	emitter.Start("run-resource-explicit", nil)
	emitter.RecordTurn(types.TurnTrace{
		Turn:       1,
		Tokens:     types.TokenUsage{Input: 10, Output: 5},
		StopReason: "end_turn",
		DurationMs: 10,
	})
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	spans := exporter.GetSpans()
	must.NotEmpty(spans, "expected at least one span")
	for _, span := range spans {
		if !check.NotNil(span.Resource, "span %q has no resource", span.Name) {
			continue
		}
		got := make(map[string]string)
		for _, kv := range span.Resource.Attributes() {
			got[string(kv.Key)] = kv.Value.AsString()
		}
		check.Equal("test-env", got["deployment.environment.name"], "span %q", span.Name)
		check.Equal("test-ns", got["service.namespace"], "span %q", span.Name)
		check.Equal("execution", got["harness.run.mode"], "span %q", span.Name)
	}
}

// assertAttribute checks that a span has the expected string attribute value.
func assertAttribute(t *testing.T, span tracetest.SpanStub, key, want string) {
	t.Helper()
	check := assert.New(t)
	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			check.Equal(want, attr.Value.AsString(), "attribute %q", key)
			return
		}
	}
	check.Fail("attribute not found", "attribute %q not found on span %q", key, span.Name)
}

// assertIntAttribute checks that a span has the expected int64 attribute
// value. The OTel SDK promotes attribute.Int(...) to attribute.INT64
// internally, so this helper covers both Int and Int64 attributes.
func assertIntAttribute(t *testing.T, span tracetest.SpanStub, key string, want int64) {
	t.Helper()
	check := assert.New(t)
	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			check.Equal(want, attr.Value.AsInt64(), "attribute %q", key)
			return
		}
	}
	check.Fail("attribute not found", "attribute %q not found on span %q", key, span.Name)
}

// assertStringSliceAttribute checks that a span has the expected string
// slice attribute. Used for the GenAI gen_ai.response.finish_reasons
// attribute, which the semconv defines as an array.
func assertStringSliceAttribute(t *testing.T, span tracetest.SpanStub, key string, want []string) {
	t.Helper()
	check := assert.New(t)
	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			check.Equal(want, attr.Value.AsStringSlice(), "attribute %q", key)
			return
		}
	}
	check.Fail("attribute not found", "attribute %q not found on span %q", key, span.Name)
}

// TestOTelTraceEmitter_GenAIAttributes pins the GenAI semantic-convention
// attributes on the root, chat and tool spans, so a regression in any
// single attribute fails under this test name.
func TestOTelTraceEmitter_GenAIAttributes(t *testing.T) {
	must := require.New(t)
	emitter, exporter := newTestOTelEmitter()

	timeout := 60
	config := &types.RunConfig{
		RunID:       "run-genai-1",
		Mode:        "execution",
		SessionName: "alignment-test",
		MaxTurns:    5,
		Timeout:     &timeout,
		Provider:    types.ProviderConfig{Type: "anthropic"},
		ModelRouter: types.ModelRouterConfig{Model: "claude-sonnet-4-6"},
	}
	emitter.Start("run-genai-1", config)
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Turn: 1, Model: "claude-opus-4-8"}).End(ChatSpanEnd{
		Summary: &types.TurnTrace{
			Turn:       1,
			Tokens:     types.TokenUsage{Input: 42, Output: 17},
			ToolCalls:  1,
			StopReason: "end_turn",
			DurationMs: 5,
			Model:      "claude-opus-4-8",
		},
	})
	emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{Name: "read_file", Known: true}).End(ToolSpanEnd{
		Summary: &types.ToolCallTrace{Name: "read_file", DurationMs: 1, Success: true},
	})
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	spans := exporter.GetSpans()
	root := findSpanByName(t, spans, "invoke_agent stirrup")
	turn := findSpanByName(t, spans, "chat claude-opus-4-8")
	tool := findSpanByName(t, spans, "execute_tool read_file")

	// Root span: operation name, model, provider, conversation.
	// gen_ai.agent.id is intentionally NOT emitted; the spec defines it
	// as a persistent agent identity (e.g. an OpenAI Assistant ID), not
	// a per-execution run ID.
	assertAttribute(t, root, genAIOperationNameKey, "invoke_agent")
	assertAttribute(t, root, genAIProviderNameKey, "anthropic")
	assertAttribute(t, root, genAIRequestModelKey, "claude-sonnet-4-6")
	assertAttribute(t, root, genAIConversationIDKey, "alignment-test")
	assertAttributesAbsent(t, root, "gen_ai.agent.id")

	// Chat span: usage tokens, finish reasons, operation name, and the
	// per-request model/provider that backends type and price generations
	// from. The request's model (the router's selection) wins over the
	// run-level configured model.
	assertIntAttribute(t, turn, genAIUsageInputTokens, 42)
	assertIntAttribute(t, turn, genAIUsageOutputTokens, 17)
	assertStringSliceAttribute(t, turn, genAIFinishReasonsKey, []string{"end_turn"})
	assertAttribute(t, turn, genAIOperationNameKey, "chat")
	assertAttribute(t, turn, genAIRequestModelKey, "claude-opus-4-8")
	assertAttribute(t, turn, genAIProviderNameKey, "anthropic")

	// Tool span: tool name and operation name.
	assertAttribute(t, tool, genAIToolNameKey, "read_file")
	assertAttribute(t, tool, genAIOperationNameKey, "execute_tool")
}

// assertAttributesAbsent fails for each of keys that the span carries.
func assertAttributesAbsent(t *testing.T, span tracetest.SpanStub, keys ...string) {
	t.Helper()
	check := assert.New(t)
	for _, attr := range span.Attributes {
		for _, key := range keys {
			if string(attr.Key) == key {
				check.Fail("unexpected attribute", "%s: unexpected attribute %s=%s", span.Name, key, attr.Value.String())
			}
		}
	}
}

// The GenAI usage keys are string constants; this pins them to the
// semconv release that defines all five.
func TestGenAIUsageKeys_MatchSemconv(t *testing.T) {
	check := assert.New(t)
	for got, want := range map[string]attribute.Key{
		genAIUsageInputTokens:              semconv.GenAIUsageInputTokensKey,
		genAIUsageOutputTokens:             semconv.GenAIUsageOutputTokensKey,
		genAIUsageCacheReadInputTokens:     semconv.GenAIUsageCacheReadInputTokensKey,
		genAIUsageCacheCreationInputTokens: semconv.GenAIUsageCacheCreationInputTokensKey,
		genAIUsageReasoningOutputTokens:    semconv.GenAIUsageReasoningOutputTokensKey,
	} {
		check.Equal(string(want), got)
	}
}

// findSpanByName fails the test when no span with the given name was
// exported.
func findSpanByName(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	t.Helper()
	must := require.New(t)
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	must.FailNow("span not exported", "no span named %q exported", name)
	return tracetest.SpanStub{}
}

// TestOTelTraceEmitter_ErrorStatus pins the OTel status-code surface:
// backends derive their error level and message from span status, so a
// failed run, turn, or tool call must carry codes.Error while the
// success paths leave status Unset (the spec default; Ok is reserved
// for explicit operator assertion).
func TestOTelTraceEmitter_ErrorStatus(t *testing.T) {
	t.Run("root error for non-success outcomes including cancelled", func(t *testing.T) {
		for _, outcome := range []string{"max_turns", "cancelled"} {
			t.Run(outcome, func(t *testing.T) {
				must := require.New(t)
				emitter, exporter := newTestOTelEmitter()
				emitter.Start("run-status", nil)
				_, err := emitter.Finish(context.Background(), outcome)
				must.NoError(err)

				span := findSpanByName(t, exporter.GetSpans(), "invoke_agent stirrup")
				must.Equal(codes.Error, span.Status.Code)
				must.Equal(outcome, span.Status.Description, "root status description should be the outcome")
				assertAttribute(t, span, errorTypeKey, outcome)
			})
		}
	})

	t.Run("root unset on success", func(t *testing.T) {
		must := require.New(t)
		emitter, exporter := newTestOTelEmitter()
		emitter.Start("run-status-ok", nil)
		_, err := emitter.Finish(context.Background(), "success")
		must.NoError(err)

		span := findSpanByName(t, exporter.GetSpans(), "invoke_agent stirrup")
		must.Equal(codes.Unset, span.Status.Code)
		assertAttributesAbsent(t, span, errorTypeKey)
	})
}

// RecordPromptResolution is config metadata, not content: the attributes
// must land on the root span even with content capture off (the test
// emitter's default), so a prompt/model comparison run is attributable
// from any trace.
func TestOTelTraceEmitter_RecordPromptResolution(t *testing.T) {
	must := require.New(t)
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-prompt-res", nil)
	emitter.RecordPromptResolution("claude-fable-5", "frontier")
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	root := findSpanByName(t, exporter.GetSpans(), "invoke_agent stirrup")
	assertAttribute(t, root, promptModelKey, "claude-fable-5")
	assertAttribute(t, root, promptTierKey, "frontier")
}

// Calling the recorder before Start (no root span yet) must be a no-op,
// not a panic.
func TestOTelTraceEmitter_RecordPromptResolutionBeforeStart(t *testing.T) {
	emitter, _ := newTestOTelEmitter()
	emitter.RecordPromptResolution("claude-fable-5", "frontier")
}

func TestOTelTraceEmitter_TraceReferenceIdentifiesRunSpan(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	emitter, exporter := newTestOTelEmitter()

	must.Nil(emitter.TraceReference(), "TraceReference before Start")

	emitter.Start("run-ref-1", nil)
	ref := emitter.TraceReference()
	must.NotNil(ref, "TraceReference after Start")
	rt, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	root := findSpanByName(t, exporter.GetSpans(), "invoke_agent stirrup")
	want := types.TraceReference{
		TraceID:   root.SpanContext.TraceID().String(),
		RunSpanID: root.SpanContext.SpanID().String(),
		Sampled:   true,
	}
	check.Equal(want, *ref, "TraceReference should identify the exported root span")
	check.Len(ref.TraceID, 32)
	check.Len(ref.RunSpanID, 16)
	must.NotNil(rt.TraceReference, "RunTrace.TraceReference")
	check.Equal(want, *rt.TraceReference, "RunTrace.TraceReference")

	emitter.Start("run-ref-2", nil)
	next := emitter.TraceReference()
	must.NotNil(next, "TraceReference after second Start")
	must.NotEqual(ref.TraceID, next.TraceID, "second run should get a new trace ID")
	_, err = emitter.Finish(context.Background(), "success")
	must.NoError(err)
}

func TestOTelTraceEmitter_TraceReferenceReportsUnsampledSpan(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
	emitter := newOTelTraceEmitterForTest(tp, false)

	emitter.Start("run-unsampled", nil)
	ref := emitter.TraceReference()
	must.NotNil(ref, "TraceReference for an unsampled run span")
	check.False(ref.Sampled, "NeverSample run span should not be sampled")

	rt, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)
	must.NotNil(rt.TraceReference, "RunTrace.TraceReference")
	check.False(rt.TraceReference.Sampled, "RunTrace.TraceReference should be unsampled")
}

// TestNonOTelEmitters_DoNotProvideTraceReference keeps a run under a
// non-OTel emitter from reporting a trace reference with no span behind it.
func TestNonOTelEmitters_DoNotProvideTraceReference(t *testing.T) {
	for name, e := range map[string]TraceEmitter{
		"jsonl":  &JSONLTraceEmitter{},
		"gcs":    &GCSTraceEmitter{},
		"nested": &NestedJSONLEmitter{},
	} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			_, ok := e.(TraceReferenceProvider)
			must.False(ok, "emitter implements TraceReferenceProvider; a run under it would report a reference with no OTel span behind it")
		})
	}
}
