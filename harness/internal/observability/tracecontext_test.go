package observability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func testSpanContext(t *testing.T, sampled bool) oteltrace.SpanContext {
	t.Helper()
	traceID, _ := oteltrace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := oteltrace.SpanIDFromHex("00f067aa0ba902b7")
	state, err := oteltrace.ParseTraceState("vendor=opaque")
	require.NoError(t, err, "parse tracestate")
	cfg := oteltrace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceState: state, Remote: true}
	if sampled {
		cfg.TraceFlags = oteltrace.FlagsSampled
	}
	return oteltrace.NewSpanContext(cfg)
}

func TestInjectTraceContext_CarriesTraceparentAndTracestate(t *testing.T) {
	ctx := oteltrace.ContextWithSpanContext(context.Background(), testSpanContext(t, true))
	got := InjectTraceContext(ctx)
	require.NotNil(t, got)
	assert.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", got.Traceparent)
	assert.Equal(t, "vendor=opaque", got.Tracestate)
}

func TestInjectTraceContext_ReturnsNilWithoutSpan(t *testing.T) {
	require.Nil(t, InjectTraceContext(context.Background()))
}

func TestExtractTraceContext_ParsesRemoteSampledParent(t *testing.T) {
	ctx := ExtractTraceContext(context.Background(),
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "vendor=opaque")
	sc := oteltrace.SpanContextFromContext(ctx)
	require.True(t, sc.IsValid() && sc.IsRemote() && sc.IsSampled(),
		"span context = %+v, want a valid, remote, sampled parent", sc)
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", sc.TraceID().String())
	assert.Equal(t, "00f067aa0ba902b7", sc.SpanID().String())
	assert.Equal(t, "opaque", sc.TraceState().Get("vendor"))
}

func TestExtractTraceContext_InvalidCarrierLeavesContextUnchanged(t *testing.T) {
	type key struct{}
	existing := testSpanContext(t, true)
	base := context.WithValue(oteltrace.ContextWithSpanContext(context.Background(), existing), key{}, "kept")

	for name, traceparent := range map[string]string{
		"empty":           "",
		"garbage":         "not-a-traceparent",
		"zero trace id":   "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"zero span id":    "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"invalid version": "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := ExtractTraceContext(base, traceparent, "vendor=other")
			require.Same(t, base, ctx, "ExtractTraceContext returned a new context for an invalid traceparent")
			got := oteltrace.SpanContextFromContext(ctx)
			require.True(t, got.Equal(existing), "span context = %+v, want the existing %+v", got, existing)
		})
	}
}

func TestExtractTraceContext_MalformedTracestateKeepsParent(t *testing.T) {
	ctx := ExtractTraceContext(context.Background(),
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "not a tracestate")
	sc := oteltrace.SpanContextFromContext(ctx)
	require.True(t, sc.IsValid(), "malformed tracestate dropped the parent")
	require.Zero(t, sc.TraceState().Len(), "tracestate = %q, want empty", sc.TraceState().String())
}
