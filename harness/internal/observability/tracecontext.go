package observability

import (
	"context"

	"go.opentelemetry.io/otel/propagation"

	"github.com/rxbynerd/stirrup/types"
)

// InjectTraceContext returns the W3C trace context of the span on ctx, or
// nil when ctx carries no valid span context. It uses the W3C propagator
// directly rather than the global one, so the wire format does not depend
// on process-wide OTel configuration.
func InjectTraceContext(ctx context.Context) *types.TraceContext {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	traceparent := carrier.Get("traceparent")
	if traceparent == "" {
		return nil
	}
	return &types.TraceContext{
		Traceparent: traceparent,
		Tracestate:  carrier.Get("tracestate"),
	}
}

// ExtractTraceContext returns ctx carrying the remote span context that
// traceparent and tracestate describe. An invalid traceparent returns ctx
// unchanged; a malformed tracestate is dropped and the parent kept.
func ExtractTraceContext(ctx context.Context, traceparent, tracestate string) context.Context {
	carrier := propagation.MapCarrier{"traceparent": traceparent}
	if tracestate != "" {
		carrier["tracestate"] = tracestate
	}
	return propagation.TraceContext{}.Extract(ctx, carrier)
}
