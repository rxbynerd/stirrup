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

func TestExtractTraceContext(t *testing.T) {
	ctx := ExtractTraceContext(context.Background(),
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "vendor=opaque")
	sc := oteltrace.SpanContextFromContext(ctx)
	if !sc.IsValid() || !sc.IsRemote() || !sc.IsSampled() {
		t.Fatalf("span context = %+v, want a valid, remote, sampled parent", sc)
	}
	if sc.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || sc.SpanID().String() != "00f067aa0ba902b7" {
		t.Errorf("IDs = %s/%s, want the traceparent's", sc.TraceID(), sc.SpanID())
	}
	if got := sc.TraceState().Get("vendor"); got != "opaque" {
		t.Errorf("tracestate vendor = %q, want opaque", got)
	}
}

func TestExtractTraceContext_InvalidCarrierLeavesContextUnchanged(t *testing.T) {
	type key struct{}
	existing := testSpanContext(t, true)
	base := context.WithValue(oteltrace.ContextWithSpanContext(context.Background(), existing), key{}, "kept")

	for name, traceparent := range map[string]string{
		"empty":           "",
		"garbage":         "not-a-traceparent",
		"zero trace ID":   "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"zero span ID":    "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"invalid version": "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := ExtractTraceContext(base, traceparent, "vendor=other")
			if ctx != base {
				t.Errorf("ExtractTraceContext returned a new context for an invalid traceparent")
			}
			if got := oteltrace.SpanContextFromContext(ctx); !got.Equal(existing) {
				t.Errorf("span context = %+v, want the existing %+v", got, existing)
			}
		})
	}
}

func TestExtractTraceContext_MalformedTracestateKeepsParent(t *testing.T) {
	ctx := ExtractTraceContext(context.Background(),
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "not a tracestate")
	sc := oteltrace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		t.Fatal("malformed tracestate dropped the parent")
	}
	if sc.TraceState().Len() != 0 {
		t.Errorf("tracestate = %q, want empty", sc.TraceState().String())
	}
}
