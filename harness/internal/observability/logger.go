// Package observability provides structured logging with automatic secret
// scrubbing for the stirrup harness. All string attribute values are passed
// through security.Scrub() before reaching the underlying handler, ensuring
// that API keys, tokens, and other secrets cannot leak into log output.
package observability

import (
	"context"
	"io"
	"log/slog"

	"github.com/rxbynerd/stirrup/harness/internal/security"
)

// ScrubHandler wraps any slog.Handler and redacts known secret patterns from
// all string attribute values before delegating to the inner handler.
type ScrubHandler struct {
	inner slog.Handler
}

// NewScrubHandler creates a ScrubHandler wrapping the given inner handler.
func NewScrubHandler(inner slog.Handler) *ScrubHandler {
	return &ScrubHandler{inner: inner}
}

// Enabled delegates to the inner handler.
func (h *ScrubHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle walks all attributes in the record, scrubs string values, then
// delegates to the inner handler. The original record is not modified.
func (h *ScrubHandler) Handle(ctx context.Context, r slog.Record) error {
	scrubbed := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		scrubbed.AddAttrs(scrubAttr(a))
		return true
	})
	return h.inner.Handle(ctx, scrubbed)
}

// WithAttrs scrubs string attribute values, then delegates to the inner handler.
func (h *ScrubHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		scrubbed[i] = scrubAttr(a)
	}
	return &ScrubHandler{inner: h.inner.WithAttrs(scrubbed)}
}

// WithGroup delegates to the inner handler.
func (h *ScrubHandler) WithGroup(name string) slog.Handler {
	return &ScrubHandler{inner: h.inner.WithGroup(name)}
}

// scrubAttr recursively scrubs string values within an slog.Attr. Group
// attributes have their sub-attributes scrubbed individually.
func scrubAttr(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.Attr{Key: a.Key, Value: slog.StringValue(security.Scrub(a.Value.String()))}
	case slog.KindGroup:
		attrs := a.Value.Group()
		scrubbed := make([]slog.Attr, len(attrs))
		for i, ga := range attrs {
			scrubbed[i] = scrubAttr(ga)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(scrubbed...)}
	default:
		return a
	}
}

// NewLogger creates a structured JSON logger that scrubs secrets from all
// output. The logger writes to w and includes the runId as a default attribute.
func NewLogger(runID string, level slog.Level, w io.Writer) *slog.Logger {
	jsonHandler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
	})
	scrubHandler := NewScrubHandler(jsonHandler)
	return slog.New(scrubHandler).With("runId", runID)
}
