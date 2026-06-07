package trace

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

func newTestOTelEmitter() (*OTelTraceEmitter, *tracetest.InMemoryExporter) {
	return newTestOTelEmitterWithOpts(observability.ResourceOptions{})
}

// newTestOTelEmitterWithOpts mirrors NewOTelTraceEmitter's TracerProvider
// construction with a caller-supplied ResourceOptions so issue #95 resource
// attributes (deployment.environment, service.namespace, harness.run.mode)
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
		Tokens:     types.TokenUsage{Input: 100, Output: 50},
		ToolCalls:  2,
		StopReason: "tool_use",
		DurationMs: 1500,
	})
	emitter.RecordTurn(types.TurnTrace{
		Turn:       2,
		Tokens:     types.TokenUsage{Input: 200, Output: 75},
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

	trace, err := emitter.Finish(context.Background(), "success")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify trace fields.
	if trace.ID != "run-otel-1" {
		t.Errorf("ID: got %q, want %q", trace.ID, "run-otel-1")
	}
	if trace.Turns != 2 {
		t.Errorf("Turns: got %d, want 2", trace.Turns)
	}
	if trace.TokenUsage.Input != 300 || trace.TokenUsage.Output != 125 {
		t.Errorf("TokenUsage: got %+v, want {300, 125}", trace.TokenUsage)
	}
	if len(trace.ToolCalls) != 2 {
		t.Errorf("ToolCalls: got %d, want 2", len(trace.ToolCalls))
	}
	if trace.Outcome != "success" {
		t.Errorf("Outcome: got %q, want %q", trace.Outcome, "success")
	}
	// Verify config was redacted.
	if trace.Config.Provider.APIKeyRef != "secret://[REDACTED]" {
		t.Errorf("APIKeyRef should be redacted, got %q", trace.Config.Provider.APIKeyRef)
	}

	// Verify OTel spans were created.
	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected OTel spans to be exported, got none")
	}

	// We expect: 2 turn spans + 2 tool spans + 1 root span = 5.
	if len(spans) != 5 {
		t.Errorf("expected 5 spans, got %d", len(spans))
		for _, s := range spans {
			t.Logf("  span: %s", s.Name)
		}
	}

	// Find spans by name.
	spanNames := make(map[string]int)
	for _, s := range spans {
		spanNames[s.Name]++
	}
	if spanNames["run"] != 1 {
		t.Errorf("expected 1 'run' span, got %d", spanNames["run"])
	}
	if spanNames["turn[1]"] != 1 {
		t.Errorf("expected 1 'turn[1]' span, got %d", spanNames["turn[1]"])
	}
	if spanNames["turn[2]"] != 1 {
		t.Errorf("expected 1 'turn[2]' span, got %d", spanNames["turn[2]"])
	}
	if spanNames["execute_tool read_file"] != 1 {
		t.Errorf("expected 1 'execute_tool read_file' span, got %d", spanNames["execute_tool read_file"])
	}
	if spanNames["execute_tool write_file"] != 1 {
		t.Errorf("expected 1 'execute_tool write_file' span, got %d", spanNames["execute_tool write_file"])
	}

	// Verify root span has correct attributes.
	var rootSpan tracetest.SpanStub
	for _, s := range spans {
		if s.Name == "run" {
			rootSpan = s
			break
		}
	}

	assertAttribute(t, rootSpan, "run.id", "run-otel-1")
	assertAttribute(t, rootSpan, "run.mode", "execution")
	assertAttribute(t, rootSpan, "run.outcome", "success")
	assertAttribute(t, rootSpan, "harness.version", "dev")

	// GenAI semconv assertions (issue #108): provider and
	// model surface under the GenAI namespace so vendor-shipped APM
	// dashboards recognise the spans.
	assertAttribute(t, rootSpan, genAIProviderNameKey, "anthropic")
	assertAttribute(t, rootSpan, genAIRequestModelKey, "claude-sonnet-4-6")

	// Per-turn token usage and finish reason surface under the GenAI
	// namespace.
	var turn1 tracetest.SpanStub
	for _, s := range spans {
		if s.Name == "turn[1]" {
			turn1 = s
			break
		}
	}
	if turn1.Name == "" {
		t.Fatal("no turn[1] span found")
	}
	assertIntAttribute(t, turn1, genAIUsageInputTokens, 100)
	assertIntAttribute(t, turn1, genAIUsageOutputTokens, 50)
	assertStringSliceAttribute(t, turn1, genAIFinishReasonsKey, []string{"tool_use"})
	assertAttribute(t, turn1, genAIOperationNameKey, "chat")
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

	// Should have just the root span.
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Errorf("expected 1 span for empty run, got %d", len(spans))
	}
	if len(spans) > 0 && spans[0].Name != "run" {
		t.Errorf("expected root span name 'run', got %q", spans[0].Name)
	}
}

func TestOTelTraceEmitter_ToolCallAttributes(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()

	emitter.Start("run-tools", nil)

	emitter.RecordToolCall(types.ToolCallTrace{
		Name:       "shell",
		DurationMs: 500,
		Success:    true,
	})

	_, err := emitter.Finish(context.Background(), "success")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exporter.GetSpans()
	var toolSpan tracetest.SpanStub
	for _, s := range spans {
		if s.Name == "execute_tool shell" {
			toolSpan = s
			break
		}
	}

	if toolSpan.Name == "" {
		t.Fatal("no 'execute_tool shell' span found")
	}

	assertAttribute(t, toolSpan, genAIToolNameKey, "shell")
}

// TestOTelTraceEmitter_SessionNameAttribute verifies that SessionName, when
// set on the RunConfig, appears as gen_ai.conversation.id on the root span.
// Child spans inherit access to it via context, so setting it on the root
// is sufficient.
func TestOTelTraceEmitter_SessionNameAttribute(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()

	timeout := 60
	config := &types.RunConfig{
		RunID:       "run-session-1",
		Mode:        "execution",
		SessionName: "nightly-eval",
		MaxTurns:    5,
		Timeout:     &timeout,
		Provider:    types.ProviderConfig{Type: "anthropic"},
	}
	emitter.Start("run-session-1", config)
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	assertAttribute(t, spans[0], genAIConversationIDKey, "nightly-eval")
}

// TestOTelTraceEmitter_SessionNameAbsentWhenEmpty pins the inverse: when
// no session name is set, the gen_ai.conversation.id attribute must not
// appear on the root span. Empty attributes pollute downstream filtering
// and would cost real money on usage-billed backends.
func TestOTelTraceEmitter_SessionNameAbsentWhenEmpty(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()

	timeout := 60
	config := &types.RunConfig{
		RunID:    "run-no-session",
		Mode:     "execution",
		MaxTurns: 5,
		Timeout:  &timeout,
		Provider: types.ProviderConfig{Type: "anthropic"},
	}
	emitter.Start("run-no-session", config)
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	for _, attr := range spans[0].Attributes {
		if string(attr.Key) == genAIConversationIDKey {
			t.Errorf("%s should be absent when SessionName is empty, found value %q", genAIConversationIDKey, attr.Value.AsString())
		}
	}
}

// TestOTelTraceEmitter_ResourceAttributes is the regression test for the
// "unknown_service:stirrup" bug: when no Resource is attached to the
// TracerProvider, OTel-aware backends (Zipkin, Jaeger, Tempo, ...) display
// the service name as "unknown_service:<binary>" because the SDK falls
// back to that placeholder. We assert here that every emitted span carries
// service.name=stirrup so this can never silently regress.
func TestOTelTraceEmitter_ResourceAttributes(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()

	emitter.Start("run-resource", nil)
	emitter.RecordToolCall(types.ToolCallTrace{Name: "read_file", DurationMs: 1, Success: true})
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
		if got["service.name"] != observability.ServiceName {
			t.Errorf("span %q: service.name=%q, want %q",
				span.Name, got["service.name"], observability.ServiceName)
		}
		if got["service.version"] == "" {
			t.Errorf("span %q: service.version missing", span.Name)
		}
		if got["service.instance.id"] == "" {
			t.Errorf("span %q: service.instance.id missing", span.Name)
		}
	}
}

// TestOTelTraceEmitter_ResourceAttributesOnSpan locks down the issue #95
// acceptance criterion that operator-supplied ResourceOptions reach every
// emitted span via the TracerProvider's Resource. The pre-existing
// TestOTelTraceEmitter_ResourceAttributes covers only the default-value
// path (service.name etc.); without this test, a regression that stopped
// threading explicit ResourceOptions through to the TracerProvider would
// not surface — Grafana queries grouping by deployment.environment would
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
		if got["deployment.environment"] != "test-env" {
			t.Errorf("span %q: deployment.environment=%q, want test-env", span.Name, got["deployment.environment"])
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

// TestGenAIProviderName pins the stirrup→OTel GenAI provider enum
// translation table. Vendor APM dashboards filter on the spec enum
// values; if any of these mappings drift, dashboards stop matching
// stirrup spans for that provider with no other surface signal.
func TestGenAIProviderName(t *testing.T) {
	cases := []struct {
		stirrupType string
		want        string
	}{
		{"anthropic", "anthropic"},
		{"bedrock", "aws.bedrock"},
		// openai-compatible falls through unchanged: it is generic Chat Completions, not OpenAI specifically.
		{"openai-compatible", "openai-compatible"},
		{"openai-responses", "openai"},
		{"gemini", "gcp.vertex_ai"},
	}
	for _, tc := range cases {
		t.Run(tc.stirrupType, func(t *testing.T) {
			if got := genAIProviderName(tc.stirrupType); got != tc.want {
				t.Errorf("genAIProviderName(%q) = %q, want %q", tc.stirrupType, got, tc.want)
			}
		})
	}
}

// TestOTelTraceEmitter_GenAIAttributes exhaustively pins the OTel
// GenAI semantic-convention attribute set adopted by issue #108. The
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
	emitter.RecordTurn(types.TurnTrace{
		Turn:       1,
		Tokens:     types.TokenUsage{Input: 42, Output: 17},
		ToolCalls:  1,
		StopReason: "end_turn",
		DurationMs: 5,
		Model:      "claude-opus-4-8",
	})
	emitter.RecordToolCall(types.ToolCallTrace{
		Name:       "read_file",
		DurationMs: 1,
		Success:    true,
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
		case "run":
			root = s
		case "turn[1]":
			turn = s
		case "execute_tool read_file":
			tool = s
		}
	}
	if root.Name == "" || turn.Name == "" || tool.Name == "" {
		t.Fatalf("expected run, turn[1], execute_tool read_file spans; got %v", spans)
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

	// Turn span: usage tokens, finish reasons, operation name, and the
	// per-turn model/provider that backends type and price generations
	// from. The turn-level model (the router's selection) wins over the
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

// TestOTelTraceEmitter_TurnModelFallback pins the gen_ai.request.model
// resolution order on turn spans: the router's per-turn selection
// (TurnTrace.Model) wins, the run-level configured model fills in for
// legacy summaries without one, and the attribute is absent when
// neither is known — empty attributes pollute downstream filtering.
func TestOTelTraceEmitter_TurnModelFallback(t *testing.T) {
	timeout := 60
	config := &types.RunConfig{
		RunID:       "run-model-fallback",
		Mode:        "execution",
		MaxTurns:    5,
		Timeout:     &timeout,
		Provider:    types.ProviderConfig{Type: "anthropic"},
		ModelRouter: types.ModelRouterConfig{Model: "config-model"},
	}

	t.Run("turn model wins over config", func(t *testing.T) {
		emitter, exporter := newTestOTelEmitter()
		emitter.Start("run-model-fallback", config)
		emitter.RecordTurn(types.TurnTrace{Turn: 1, StopReason: "end_turn", DurationMs: 1, Model: "turn-model"})
		if _, err := emitter.Finish(context.Background(), "success"); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		assertAttribute(t, findSpanByName(t, exporter.GetSpans(), "turn[1]"), genAIRequestModelKey, "turn-model")
	})

	t.Run("config model fills in", func(t *testing.T) {
		emitter, exporter := newTestOTelEmitter()
		emitter.Start("run-model-fallback", config)
		emitter.RecordTurn(types.TurnTrace{Turn: 1, StopReason: "end_turn", DurationMs: 1})
		if _, err := emitter.Finish(context.Background(), "success"); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		assertAttribute(t, findSpanByName(t, exporter.GetSpans(), "turn[1]"), genAIRequestModelKey, "config-model")
	})

	t.Run("absent when neither known", func(t *testing.T) {
		emitter, exporter := newTestOTelEmitter()
		emitter.Start("run-no-model", nil)
		emitter.RecordTurn(types.TurnTrace{Turn: 1, StopReason: "end_turn", DurationMs: 1})
		if _, err := emitter.Finish(context.Background(), "success"); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		span := findSpanByName(t, exporter.GetSpans(), "turn[1]")
		for _, attr := range span.Attributes {
			if string(attr.Key) == genAIRequestModelKey {
				t.Errorf("%s should be absent when no model is known, found %q", genAIRequestModelKey, attr.Value.AsString())
			}
		}
	})
}

// TestOTelTraceEmitter_UnknownToolSpanNameBounded pins the cardinality
// bound on tool span names: an unknown-tool failure carries a
// model-controlled tool name, which must not become an unbounded span
// name (the vector issue #309 bounded on the loop's tool.<name> spans).
// The raw requested name still rides the gen_ai.tool.name attribute.
func TestOTelTraceEmitter_UnknownToolSpanNameBounded(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()

	emitter.Start("run-unknown-tool", nil)
	emitter.RecordToolCall(types.ToolCallTrace{
		Name:          "totally_made_up_tool_9000",
		DurationMs:    1,
		Success:       false,
		ErrorReason:   "unknown tool",
		ErrorCategory: string(observability.ToolFailureUnknownTool),
	})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	span := findSpanByName(t, exporter.GetSpans(), "execute_tool")
	assertAttribute(t, span, genAIToolNameKey, "totally_made_up_tool_9000")
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
			span := findSpanByName(t, exporter.GetSpans(), "run")
			if span.Status.Code != codes.Error {
				t.Errorf("outcome %q: root status = %v, want Error", outcome, span.Status.Code)
			}
			if span.Status.Description != outcome {
				t.Errorf("outcome %q: root status description = %q, want the outcome", outcome, span.Status.Description)
			}
		}
	})

	t.Run("root unset on success", func(t *testing.T) {
		emitter, exporter := newTestOTelEmitter()
		emitter.Start("run-status-ok", nil)
		if _, err := emitter.Finish(context.Background(), "success"); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		span := findSpanByName(t, exporter.GetSpans(), "run")
		if span.Status.Code != codes.Unset {
			t.Errorf("root status = %v, want Unset", span.Status.Code)
		}
	})

	t.Run("turn error on error stop reason", func(t *testing.T) {
		emitter, exporter := newTestOTelEmitter()
		emitter.Start("run-status-turn", nil)
		emitter.RecordTurn(types.TurnTrace{Turn: 1, StopReason: "error", DurationMs: 1})
		emitter.RecordTurn(types.TurnTrace{Turn: 2, StopReason: "end_turn", DurationMs: 1})
		if _, err := emitter.Finish(context.Background(), "error"); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		spans := exporter.GetSpans()
		if got := findSpanByName(t, spans, "turn[1]").Status.Code; got != codes.Error {
			t.Errorf("error turn status = %v, want Error", got)
		}
		if got := findSpanByName(t, spans, "turn[2]").Status.Code; got != codes.Unset {
			t.Errorf("clean turn status = %v, want Unset", got)
		}
	})

	t.Run("tool error with reason and error.type", func(t *testing.T) {
		emitter, exporter := newTestOTelEmitter()
		emitter.Start("run-status-tool", nil)
		emitter.RecordToolCall(types.ToolCallTrace{
			Name:          "run_command",
			DurationMs:    3,
			Success:       false,
			ErrorReason:   "exit status 1",
			ErrorCategory: "execution_failed",
		})
		emitter.RecordToolCall(types.ToolCallTrace{
			Name:       "read_file",
			DurationMs: 1,
			Success:    true,
		})
		if _, err := emitter.Finish(context.Background(), "success"); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		spans := exporter.GetSpans()
		failed := findSpanByName(t, spans, "execute_tool run_command")
		if failed.Status.Code != codes.Error {
			t.Errorf("failed tool status = %v, want Error", failed.Status.Code)
		}
		if failed.Status.Description != "exit status 1" {
			t.Errorf("failed tool status description = %q, want the error reason", failed.Status.Description)
		}
		assertAttribute(t, failed, errorTypeKey, "execution_failed")

		ok := findSpanByName(t, spans, "execute_tool read_file")
		if ok.Status.Code != codes.Unset {
			t.Errorf("successful tool status = %v, want Unset", ok.Status.Code)
		}
		for _, attr := range ok.Attributes {
			if string(attr.Key) == errorTypeKey {
				t.Errorf("%s should be absent on successful calls, found %q", errorTypeKey, attr.Value.AsString())
			}
		}
	})

	t.Run("failed tool with empty reason gets fallback description", func(t *testing.T) {
		emitter, exporter := newTestOTelEmitter()
		emitter.Start("run-status-tool-bare", nil)
		emitter.RecordToolCall(types.ToolCallTrace{Name: "edit_file", DurationMs: 1, Success: false})
		if _, err := emitter.Finish(context.Background(), "success"); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		span := findSpanByName(t, exporter.GetSpans(), "execute_tool edit_file")
		if span.Status.Code != codes.Error || span.Status.Description != "tool call failed" {
			t.Errorf("status = %v %q, want Error with fallback description", span.Status.Code, span.Status.Description)
		}
	})
}
