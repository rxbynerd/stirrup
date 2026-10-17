package observability

import (
	"context"
	"testing"

	oteltrace "go.opentelemetry.io/otel/trace"
)

func testSpanContext(t *testing.T, sampled bool) oteltrace.SpanContext {
	t.Helper()
	traceID, _ := oteltrace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := oteltrace.SpanIDFromHex("00f067aa0ba902b7")
	state, err := oteltrace.ParseTraceState("vendor=opaque")
	if err != nil {
		t.Fatalf("parse tracestate: %v", err)
	}
	cfg := oteltrace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceState: state, Remote: true}
	if sampled {
		cfg.TraceFlags = oteltrace.FlagsSampled
	}
	return oteltrace.NewSpanContext(cfg)
}

func TestInjectTraceContext(t *testing.T) {
	ctx := oteltrace.ContextWithSpanContext(context.Background(), testSpanContext(t, true))
	got := InjectTraceContext(ctx)
	if got == nil {
		t.Fatal("InjectTraceContext = nil, want a carrier")
	}
	if want := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"; got.Traceparent != want {
		t.Errorf("traceparent = %q, want %q", got.Traceparent, want)
	}
	if got.Tracestate != "vendor=opaque" {
		t.Errorf("tracestate = %q, want vendor=opaque", got.Tracestate)
	}
}

func TestInjectTraceContext_NilWithoutSpan(t *testing.T) {
	if got := InjectTraceContext(context.Background()); got != nil {
		t.Errorf("InjectTraceContext = %+v, want nil for a context without a span", got)
	}
}
