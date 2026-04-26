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
// the log scrubber actually fires. The implementation choice (a) per the
// brief: plumb a SecurityLogger into the log handler so log-side redactions
// produce the same audit trail as transport-side redactions.
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
	jsonHandler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
	})
	scrubHandler := NewScrubHandlerWithSecurity(jsonHandler, sec)
	return slog.New(scrubHandler).With("runId", runID)
}
