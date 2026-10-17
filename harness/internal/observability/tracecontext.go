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
