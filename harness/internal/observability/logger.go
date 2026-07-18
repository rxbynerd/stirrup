// Package observability provides structured logging with automatic secret
// scrubbing for the stirrup harness. All string attribute values are passed
// through security.Scrub() before reaching the underlying handler, ensuring
// that API keys, tokens, and other secrets cannot leak into log output.
package observability

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/rxbynerd/stirrup/harness/internal/security"
)

// SecurityNotifier is the minimal interface ScrubHandler uses to emit a
// SecretRedactedInOutput event when an attribute scrub redacts a secret.
// security.SecurityLogger satisfies this interface directly. Defining the
// interface here (rather than importing security.SecurityLogger as a
// concrete type) avoids tightly coupling the log handler to a specific
// implementation and keeps the dependency optional.
type SecurityNotifier interface {
	SecretRedactedInOutput(pattern, location string)
}

// ScrubHandler wraps any slog.Handler and redacts known secret patterns from
// all string attribute values before delegating to the inner handler.
//
// Optionally accepts a SecurityNotifier (typically *security.SecurityLogger)
// so the harness emits a structured SecretRedactedInOutput event each time
// the log scrubber fires, giving log-side redactions the same audit trail
// as transport-side redactions.
type ScrubHandler struct {
	inner    slog.Handler
	security SecurityNotifier // optional; nil means no event emission
}

// NewScrubHandler creates a ScrubHandler wrapping the given inner handler.
func NewScrubHandler(inner slog.Handler) *ScrubHandler {
	return &ScrubHandler{inner: inner}
}

// NewScrubHandlerWithSecurity is like NewScrubHandler but also wires a
// SecurityNotifier. Pass nil to disable emission (equivalent to
// NewScrubHandler).
func NewScrubHandlerWithSecurity(inner slog.Handler, sec SecurityNotifier) *ScrubHandler {
	return &ScrubHandler{inner: inner, security: sec}
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
		scrubbed.AddAttrs(h.scrubAttrAndReport(a))
		return true
	})
	return h.inner.Handle(ctx, scrubbed)
}

// WithAttrs scrubs string attribute values, then delegates to the inner handler.
func (h *ScrubHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		scrubbed[i] = h.scrubAttrAndReport(a)
	}
	return &ScrubHandler{inner: h.inner.WithAttrs(scrubbed), security: h.security}
}

// WithGroup delegates to the inner handler.
func (h *ScrubHandler) WithGroup(name string) slog.Handler {
	return &ScrubHandler{inner: h.inner.WithGroup(name), security: h.security}
}

// scrubAttrAndReport recursively scrubs string values within an slog.Attr.
// Group attributes have their sub-attributes scrubbed individually. When the
// handler has a SecurityNotifier wired, every redaction produces a
// SecretRedactedInOutput event.
func (h *ScrubHandler) scrubAttrAndReport(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		scrubbed, stats := security.ScrubWithStats(a.Value.String())
		if stats.Count > 0 && h.security != nil {
			for _, p := range stats.Patterns {
				h.security.SecretRedactedInOutput(p, "logger.attr."+a.Key)
			}
		}
		return slog.Attr{Key: a.Key, Value: slog.StringValue(scrubbed)}
	case slog.KindGroup:
		attrs := a.Value.Group()
		scrubbed := make([]slog.Attr, len(attrs))
		for i, ga := range attrs {
			scrubbed[i] = h.scrubAttrAndReport(ga)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(scrubbed...)}
	default:
		return a
	}
}

// NewLogger creates a structured JSON logger that scrubs secrets from all
// output. The logger writes to w and includes the runId as a default attribute.
func NewLogger(runID string, level slog.Level, w io.Writer) *slog.Logger {
	return NewLoggerWithSecurity(runID, level, w, nil)
}

// NewLoggerWithSecurity is like NewLogger but additionally wires a
// SecurityNotifier so the structured log scrubber emits a
// SecretRedactedInOutput event each time it redacts a value. Pass nil to
// disable (equivalent to NewLogger).
func NewLoggerWithSecurity(runID string, level slog.Level, w io.Writer, sec SecurityNotifier) *slog.Logger {
	return NewLoggerWithExport(runID, level, w, sec, nil)
}

// NewLoggerWithExport is the full logger constructor. It always writes JSON
// to w; when exportHandler is non-nil (an OTLP-bridge leaf handler from
// NewLogExporter) each record is additionally fanned out to that handler so
// the same line ships to the configured OTel collector. Pass nil to keep
// stderr-only behaviour.
//
// The scrubbing and span-correlation layers wrap the fan-out point, so the
// secret scrubber runs once and covers both sinks — no log value leaves the
// process unscrubbed regardless of sink.
//
//	SpanContextHandler ← ScrubHandler ← fanout{JSONHandler, exportHandler}
func NewLoggerWithExport(runID string, level slog.Level, w io.Writer, sec SecurityNotifier, exportHandler slog.Handler) *slog.Logger {
	jsonHandler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
	})

	var leaf slog.Handler = jsonHandler
	if exportHandler != nil {
		leaf = newFanoutHandler(jsonHandler, exportHandler)
	}

	scrubHandler := NewScrubHandlerWithSecurity(leaf, sec)
	// SpanContextHandler is outermost so the trace_id / span_id it injects
	// still flow through ScrubHandler before reaching the leaf sink(s).
	spanHandler := NewSpanContextHandler(scrubHandler)
	return slog.New(spanHandler).With("runId", runID)
}

// fanoutHandler dispatches every record to two inner handlers. It exists so
// the stderr JSON sink and the OTLP-bridge sink sit below the shared
// Scrub / SpanContext layers, letting those decorators run exactly once
// while both sinks still receive the enriched record.
type fanoutHandler struct {
	handlers []slog.Handler
}

func newFanoutHandler(handlers ...slog.Handler) *fanoutHandler {
	return &fanoutHandler{handlers: handlers}
}

// Enabled reports true when any inner handler is enabled for the level, so
// a record is processed if either sink wants it.
func (h *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, inner := range h.handlers {
		if inner.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle delivers the record to every inner handler. Errors are joined so a
// failure on one sink (e.g. the OTLP bridge) does not suppress delivery to
// the others; the stderr write still happens.
func (h *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, inner := range h.handlers {
		if !inner.Enabled(ctx, r.Level) {
			continue
		}
		if err := inner.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (h *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, inner := range h.handlers {
		next[i] = inner.WithAttrs(attrs)
	}
	return &fanoutHandler{handlers: next}
}

func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, inner := range h.handlers {
		next[i] = inner.WithGroup(name)
	}
	return &fanoutHandler{handlers: next}
}
