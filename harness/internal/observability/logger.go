// Package observability provides structured logging with automatic secret
// scrubbing for the stirrup harness. Log messages and every attribute value
// are passed through security.Scrub() before reaching the underlying
// handler, which redacts credentials in the formats the scrubber recognises.
// A secret with no recognisable format is not redacted.
package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sync/atomic"

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
// the message and every attribute value before delegating to the inner
// handler. LogValuer values are resolved first. Errors, fmt.Stringers and
// byte slices become scrubbed strings, groups are scrubbed recursively, and
// any other composite value (map, slice, struct) is rendered as one scrubbed
// JSON string. Numeric, boolean, time and duration values are unchanged.
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

// Handle scrubs the message and every attribute value, then delegates to the
// inner handler. The original record is not modified.
func (h *ScrubHandler) Handle(ctx context.Context, r slog.Record) error {
	scrubbed := slog.NewRecord(r.Time, r.Level, h.scrubString(r.Message, "logger.message"), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		scrubbed.AddAttrs(h.scrubAttrAndReport(a))
		return true
	})
	return h.inner.Handle(ctx, scrubbed)
}

// WithAttrs scrubs the attribute values, then delegates to the inner handler.
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

// scrubString redacts known secret patterns from s. When the handler has a
// SecurityNotifier wired, every redaction produces a SecretRedactedInOutput
// event tagged with location.
func (h *ScrubHandler) scrubString(s, location string) string {
	scrubbed, stats := security.ScrubWithStats(s)
	if stats.Count > 0 && h.security != nil {
		for _, p := range stats.Patterns {
			h.security.SecretRedactedInOutput(p, location)
		}
	}
	return scrubbed
}

// scrubAttrAndReport returns a with its value scrubbed. The value is
// resolved first so a LogValuer cannot defer its content past the handler.
// Group attributes have their sub-attributes scrubbed individually.
func (h *ScrubHandler) scrubAttrAndReport(a slog.Attr) slog.Attr {
	if a.Equal(slog.Attr{}) {
		return a
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.scrubString(v.String(), "logger.attr."+a.Key))
	case slog.KindGroup:
		attrs := v.Group()
		scrubbed := make([]slog.Attr, len(attrs))
		for i, ga := range attrs {
			scrubbed[i] = h.scrubAttrAndReport(ga)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(scrubbed...)}
	case slog.KindAny:
		if v.Any() == nil {
			return slog.Attr{Key: a.Key, Value: v}
		}
		if scalar, ok := namedScalar(v.Any()); ok {
			return h.scrubAttrAndReport(slog.Attr{Key: a.Key, Value: scalar})
		}
		return slog.String(a.Key, h.scrubString(renderAny(v.Any()), "logger.attr."+a.Key))
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}

// namedScalar converts a value whose underlying type is a basic kind (a
// named string, bool or number that slog stores as KindAny) to the matching
// slog value, so it keeps its scalar shape instead of being rendered as
// JSON. Errors and fmt.Stringers are excluded because their own rendering
// takes precedence.
func namedScalar(v any) (slog.Value, bool) {
	switch v.(type) {
	case nil, error, fmt.Stringer:
		return slog.Value{}, false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String:
		return slog.StringValue(rv.String()), true
	case reflect.Bool:
		return slog.BoolValue(rv.Bool()), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return slog.Int64Value(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return slog.Uint64Value(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return slog.Float64Value(rv.Float()), true
	}
	return slog.Value{}, false
}

// renderAny renders an opaque log value as text: Error() for errors, String()
// for fmt.Stringers, the content of a byte slice, and a single JSON string
// for anything else (falling back to %+v when the value cannot be marshalled).
// A value whose rendering panics yields a placeholder rather than crashing
// the logging call.
func renderAny(v any) (out string) {
	defer func() {
		if recover() != nil {
			out = "[unrenderable log value]"
		}
	}()
	switch t := v.(type) {
	case error:
		return t.Error()
	case fmt.Stringer:
		return t.String()
	case []byte:
		return string(t)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf("%+v", v)
	}
	return string(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
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
	return NewScopedLoggerWithExport(NewRunScope(runID), level, w, sec, exportHandler)
}

// NewScopedLoggerWithExport is NewLoggerWithExport with the runId
// attribute read from scope at log time rather than fixed at
// construction. Every logger derived from the result — With, WithGroup,
// the copies handed to components at build — shares the scope, so a
// process that runs several runs in sequence (follow-ups) re-attributes
// all of them with one scope.Set.
func NewScopedLoggerWithExport(scope *RunScope, level slog.Level, w io.Writer, sec SecurityNotifier, exportHandler slog.Handler) *slog.Logger {
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
	return slog.New(&runScopeHandler{inner: spanHandler, scope: scope})
}

// RunScope is the mutable run identity a scoped logger stamps on every
// record. Safe for concurrent use.
type RunScope struct {
	runID atomic.Pointer[string]
}

// NewRunScope returns a scope initially naming runID.
func NewRunScope(runID string) *RunScope {
	s := &RunScope{}
	s.Set(runID)
	return s
}

// Set changes the run every subsequent record is attributed to.
func (s *RunScope) Set(runID string) {
	s.runID.Store(&runID)
}

// RunID returns the run currently named by the scope.
func (s *RunScope) RunID() string {
	if p := s.runID.Load(); p != nil {
		return *p
	}
	return ""
}

// runScopeHandler adds the scope's current runId to each record as it
// is handled. It is a record attribute, so it follows any With-bound
// attributes and would nest inside an opened group; no logger in the
// harness opens one.
type runScopeHandler struct {
	inner slog.Handler
	scope *RunScope
}

func (h *runScopeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *runScopeHandler) Handle(ctx context.Context, r slog.Record) error {
	scoped := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	scoped.AddAttrs(slog.String("runId", h.scope.RunID()))
	r.Attrs(func(a slog.Attr) bool {
		scoped.AddAttrs(a)
		return true
	})
	return h.inner.Handle(ctx, scoped)
}

func (h *runScopeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &runScopeHandler{inner: h.inner.WithAttrs(attrs), scope: h.scope}
}

func (h *runScopeHandler) WithGroup(name string) slog.Handler {
	return &runScopeHandler{inner: h.inner.WithGroup(name), scope: h.scope}
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
