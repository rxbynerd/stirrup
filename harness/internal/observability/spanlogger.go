package observability

import (
	"context"
	"log/slog"

	oteltrace "go.opentelemetry.io/otel/trace"
)

// SpanContextHandler wraps any slog.Handler and, when a record is emitted
// inside an active span, appends lowercase trace_id and span_id attributes
// so the record can be correlated with the trace in Grafana's "Logs for
// trace" panel (and any other OTel/Loki-aware backend). The lowercase
// snake_case naming (rather than the camelCase the rest of the stirrup log
// schema uses) is the OTel/Loki convention the Tempo↔Loki correlation
// derived-field is keyed on; see docs/observability-cloud.md.
//
// This handler is the outermost layer in the stack
// (JSONHandler ← ScrubHandler ← SpanContextHandler) so the trace_id and
// span_id it adds still pass through ScrubHandler on their way to the
// JSON encoder.
type SpanContextHandler struct {
	inner slog.Handler
}

// NewSpanContextHandler creates a SpanContextHandler wrapping the given
// inner handler.
func NewSpanContextHandler(inner slog.Handler) *SpanContextHandler {
	return &SpanContextHandler{inner: inner}
}

// Enabled delegates to the inner handler.
func (h *SpanContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle appends trace_id and span_id when ctx carries a valid span
// context, then delegates to the inner handler. When no span is active
// (the common case for boot-time and context-less logging) the record is
// passed through unchanged so non-traced lines stay free of empty
// correlation fields.
func (h *SpanContextHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := oteltrace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return h.inner.Handle(ctx, r)
	}
	// Clone so the caller's record is not mutated (matches ScrubHandler's
	// copy-then-delegate discipline).
	enriched := r.Clone()
	enriched.AddAttrs(
		slog.String("trace_id", sc.TraceID().String()),
		slog.String("span_id", sc.SpanID().String()),
	)
	return h.inner.Handle(ctx, enriched)
}

// WithAttrs delegates to the inner handler, preserving the span-context
// behaviour on the returned handler.
func (h *SpanContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &SpanContextHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup delegates to the inner handler, preserving the span-context
// behaviour on the returned handler.
func (h *SpanContextHandler) WithGroup(name string) slog.Handler {
	return &SpanContextHandler{inner: h.inner.WithGroup(name)}
}
