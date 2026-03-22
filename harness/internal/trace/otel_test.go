package trace

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/types"
)

func newTestOTelEmitter() (*OTelTraceEmitter, *tracetest.InMemoryExporter) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
	)
	emitter := newOTelTraceEmitterForTest(tp)
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

	// We expect: 2 turn spans + 2 tool_call spans + 1 root span = 5.
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
	if spanNames["tool_call"] != 2 {
		t.Errorf("expected 2 'tool_call' spans, got %d", spanNames["tool_call"])
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
	assertAttribute(t, rootSpan, "run.model", "claude-sonnet-4-6")
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
		if s.Name == "tool_call" {
			toolSpan = s
			break
		}
	}

	if toolSpan.Name == "" {
		t.Fatal("no tool_call span found")
	}

	assertAttribute(t, toolSpan, "tool.name", "shell")
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
