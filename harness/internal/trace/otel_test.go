package trace

import (
	"context"
	"testing"

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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if trace.ID != "run-otel-1" {
		t.Errorf("ID: got %q, want %q", trace.ID, "run-otel-1")
	}
	if trace.Turns != 2 {
		t.Errorf("Turns: got %d, want 2", trace.Turns)
	}
	wantTokens := types.TokenUsage{Input: 300, Output: 125, CacheRead: 210, CacheWrite: 30, Reasoning: 15}
	if trace.TokenUsage != wantTokens {
		t.Errorf("TokenUsage: got %+v, want %+v", trace.TokenUsage, wantTokens)
	}
	if len(trace.ToolCalls) != 2 {
		t.Errorf("ToolCalls: got %d, want 2", len(trace.ToolCalls))
	}
	if trace.Outcome != "success" {
		t.Errorf("Outcome: got %q, want %q", trace.Outcome, "success")
	}

	if trace.Config.Provider.APIKeyRef != "secret://[REDACTED]" {
		t.Errorf("APIKeyRef should be redacted, got %q", trace.Config.Provider.APIKeyRef)
	}

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected OTel spans to be exported, got none")
	}

	// The Record* methods feed the aggregate only; requests and tool
	// calls become spans through the live handles, so only the root span
	// is exported.
	if len(spans) != 1 {
		t.Errorf("expected 1 span, got %d", len(spans))
		for _, s := range spans {
			t.Logf("  span: %s", s.Name)
		}
	}

	spanNames := make(map[string]int)
	for _, s := range spans {
		spanNames[s.Name]++
	}
	if spanNames["invoke_agent stirrup"] != 1 {
		t.Errorf("expected 1 root span, got %d", spanNames["invoke_agent stirrup"])
	}

	var rootSpan tracetest.SpanStub
	for _, s := range spans {
		if s.Name == "invoke_agent stirrup" {
			rootSpan = s
			break
		}
	}

	assertAttribute(t, rootSpan, "run.id", "run-otel-1")
	assertAttribute(t, rootSpan, "run.mode", "execution")
	assertAttribute(t, rootSpan, "run.outcome", "success")
	assertAttribute(t, rootSpan, "harness.version", "dev")

	// GenAI semconv assertions: provider and model surface under the
	// GenAI namespace so vendor-shipped APM dashboards recognise the
	// spans.
	assertAttribute(t, rootSpan, genAIProviderNameKey, "anthropic")
	assertAttribute(t, rootSpan, genAIRequestModelKey, "claude-sonnet-4-6")
}

func TestOTelTraceEmitter_EmptyRun(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()

	emitter.Start("run-empty", nil)

	trace, err := emitter.Finish(context.Background(), "error")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if trace.Turns != 0 {
		t.Errorf("Turns: got %d, want 0", trace.Turns)
	}
	if trace.Outcome != "error" {
		t.Errorf("Outcome: got %q, want %q", trace.Outcome, "error")
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Errorf("expected 1 span for empty run, got %d", len(spans))
	}
	if len(spans) > 0 && spans[0].Name != "invoke_agent stirrup" {
		t.Errorf("expected root span name invoke_agent stirrup, got %q", spans[0].Name)
	}
}

// TestOTelTraceEmitter_ResourceAttributes is the regression test for the
// "unknown_service:stirrup" bug: when no Resource is attached to the
// TracerProvider, OTel-aware backends (Zipkin, Jaeger, Tempo, ...) display
// the service name as "unknown_service:<binary>" because the SDK falls
// back to that placeholder. We assert here that every emitted span carries
// service.name=stirrup so this can never silently regress.
func TestOTelTraceEmitter_ResourceAttributes(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	emitter, exporter := newTestOTelEmitter()

	emitter.Start("run-resource", nil)
	emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{Name: "read_file", Known: true}).End(ToolSpanEnd{})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected at least one span")
	}

	for _, span := range spans {
		if span.Resource == nil {
			t.Errorf("span %q: Resource is nil — TracerProvider was constructed without WithResource", span.Name)
			continue
		}
		got := make(map[string]string)
		for _, kv := range span.Resource.Attributes() {
			got[string(kv.Key)] = kv.Value.AsString()
		}
		if got["service.name"] != observability.DefaultServiceName {
			t.Errorf("span %q: service.name=%q, want %q",
				span.Name, got["service.name"], observability.DefaultServiceName)
		}
		if got["service.version"] == "" {
			t.Errorf("span %q: service.version missing", span.Name)
		}
		if got["service.instance.id"] == "" {
			t.Errorf("span %q: service.instance.id missing", span.Name)
		}
	}
}

// TestOTelTraceEmitter_ResourceAttributesOnSpan locks down that
// operator-supplied ResourceOptions reach every emitted span via the
// TracerProvider's Resource. The pre-existing
// TestOTelTraceEmitter_ResourceAttributes covers only the default-value
// path (service.name etc.); without this test, a regression that stopped
// threading explicit ResourceOptions through to the TracerProvider would
// not surface — Grafana queries grouping by deployment.environment.name would
// quietly return empty rows.
func TestOTelTraceEmitter_ResourceAttributesOnSpan(t *testing.T) {
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
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected at least one span")
	}
	for _, span := range spans {
		if span.Resource == nil {
			t.Errorf("span %q: Resource is nil", span.Name)
			continue
		}
		got := make(map[string]string)
		for _, kv := range span.Resource.Attributes() {
			got[string(kv.Key)] = kv.Value.AsString()
		}
		if got["deployment.environment.name"] != "test-env" {
			t.Errorf("span %q: deployment.environment.name=%q, want test-env", span.Name, got["deployment.environment.name"])
		}
		if got["service.namespace"] != "test-ns" {
			t.Errorf("span %q: service.namespace=%q, want test-ns", span.Name, got["service.namespace"])
		}
		if got["harness.run.mode"] != "execution" {
			t.Errorf("span %q: harness.run.mode=%q, want execution", span.Name, got["harness.run.mode"])
		}
	}
}

// assertAttribute checks that a span has the expected string attribute value.
func assertAttribute(t *testing.T, span tracetest.SpanStub, key, want string) {
	t.Helper()
	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			got := attr.Value.AsString()
			if got != want {
				t.Errorf("attribute %q: got %q, want %q", key, got, want)
			}
			return
		}
	}
	t.Errorf("attribute %q not found on span %q", key, span.Name)
}

// assertIntAttribute checks that a span has the expected int64 attribute
// value. The OTel SDK promotes attribute.Int(...) to attribute.INT64
// internally, so this helper covers both Int and Int64 attributes.
func assertIntAttribute(t *testing.T, span tracetest.SpanStub, key string, want int64) {
	t.Helper()
	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			got := attr.Value.AsInt64()
			if got != want {
				t.Errorf("attribute %q: got %d, want %d", key, got, want)
			}
			return
		}
	}
	t.Errorf("attribute %q not found on span %q", key, span.Name)
}

// assertStringSliceAttribute checks that a span has the expected string
// slice attribute. Used for the GenAI gen_ai.response.finish_reasons
// attribute, which the semconv defines as an array.
func assertStringSliceAttribute(t *testing.T, span tracetest.SpanStub, key string, want []string) {
	t.Helper()
	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			got := attr.Value.AsStringSlice()
			if len(got) != len(want) {
				t.Errorf("attribute %q: got %v, want %v", key, got, want)
				return
			}
			for i := range got {
				if got[i] != want[i] {
					t.Errorf("attribute %q: got %v, want %v", key, got, want)
					return
				}
			}
			return
		}
	}
	t.Errorf("attribute %q not found on span %q", key, span.Name)
}

// TestOTelTraceEmitter_GenAIAttributes exhaustively pins the OTel
// GenAI semantic-convention attribute set. The
// individual GenAI attributes are also asserted opportunistically by
// FullLifecycle, ToolCallAttributes, and SessionName, but this test
// exists so a regression in any single GenAI attribute fails loudly
// at the dedicated test name rather than as a side-effect of an
// unrelated assertion.
func TestOTelTraceEmitter_GenAIAttributes(t *testing.T) {
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
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	spans := exporter.GetSpans()
	var (
		root tracetest.SpanStub
		turn tracetest.SpanStub
		tool tracetest.SpanStub
	)
	for _, s := range spans {
		switch s.Name {
		case "invoke_agent stirrup":
			root = s
		case "chat claude-opus-4-8":
			turn = s
		case "execute_tool read_file":
			tool = s
		}
	}
	if root.Name == "" || turn.Name == "" || tool.Name == "" {
		t.Fatalf("expected invoke_agent, chat and execute_tool read_file spans; got %v", spans)
	}

	// Root span: operation name, model, provider, conversation.
	// gen_ai.agent.id is intentionally NOT emitted; the spec defines it
	// as a persistent agent identity (e.g. an OpenAI Assistant ID), not
	// a per-execution run ID.
	assertAttribute(t, root, genAIOperationNameKey, "invoke_agent")
	assertAttribute(t, root, genAIProviderNameKey, "anthropic")
	assertAttribute(t, root, genAIRequestModelKey, "claude-sonnet-4-6")
	assertAttribute(t, root, genAIConversationIDKey, "alignment-test")
	for _, attr := range root.Attributes {
		if string(attr.Key) == "gen_ai.agent.id" {
			t.Errorf("gen_ai.agent.id should not be emitted, found value %q", attr.Value.AsString())
		}
	}

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

func assertAttributesAbsent(t *testing.T, span tracetest.SpanStub, keys ...string) {
	t.Helper()
	for _, attr := range span.Attributes {
		for _, key := range keys {
			if string(attr.Key) == key {
				t.Errorf("%s: unexpected attribute %s=%s", span.Name, key, attr.Value.String())
			}
		}
	}
}

// The GenAI usage keys are string constants; this pins them to the
// semconv release that defines all five.
func TestGenAIUsageKeysMatchSemconv(t *testing.T) {
	for got, want := range map[string]attribute.Key{
		genAIUsageInputTokens:              semconv.GenAIUsageInputTokensKey,
		genAIUsageOutputTokens:             semconv.GenAIUsageOutputTokensKey,
		genAIUsageCacheReadInputTokens:     semconv.GenAIUsageCacheReadInputTokensKey,
		genAIUsageCacheCreationInputTokens: semconv.GenAIUsageCacheCreationInputTokensKey,
		genAIUsageReasoningOutputTokens:    semconv.GenAIUsageReasoningOutputTokensKey,
	} {
		if got != string(want) {
			t.Errorf("attribute key %q, want semconv %q", got, want)
		}
	}
}

// findSpanByName fails the test when no span with the given name was
// exported.
func findSpanByName(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	t.Helper()
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no span named %q exported", name)
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
			emitter, exporter := newTestOTelEmitter()
			emitter.Start("run-status", nil)
			if _, err := emitter.Finish(context.Background(), outcome); err != nil {
				t.Fatalf("Finish: %v", err)
			}
			span := findSpanByName(t, exporter.GetSpans(), "invoke_agent stirrup")
			if span.Status.Code != codes.Error {
				t.Errorf("outcome %q: root status = %v, want Error", outcome, span.Status.Code)
			}
			if span.Status.Description != outcome {
				t.Errorf("outcome %q: root status description = %q, want the outcome", outcome, span.Status.Description)
			}
			assertAttribute(t, span, errorTypeKey, outcome)
		}
	})

	t.Run("root unset on success", func(t *testing.T) {
		emitter, exporter := newTestOTelEmitter()
		emitter.Start("run-status-ok", nil)
		if _, err := emitter.Finish(context.Background(), "success"); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		span := findSpanByName(t, exporter.GetSpans(), "invoke_agent stirrup")
		if span.Status.Code != codes.Unset {
			t.Errorf("root status = %v, want Unset", span.Status.Code)
		}
		assertAttributesAbsent(t, span, errorTypeKey)
	})

}

// RecordPromptResolution is config metadata, not content: the attributes
// must land on the root span even with content capture off (the test
// emitter's default), so a prompt/model comparison run is attributable
// from any trace.
func TestOTelTraceEmitter_RecordPromptResolution(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-prompt-res", nil)
	emitter.RecordPromptResolution("claude-fable-5", "frontier")
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
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
