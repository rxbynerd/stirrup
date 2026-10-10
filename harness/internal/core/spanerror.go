package core

import (
	"errors"

	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/security"
)

// recordSpanException adds err to span as an exception event with the
// error text scrubbed, and returns the scrubbed text. The log scrubber
// does not cover spans, so every error recorded on a span goes through
// here. The original error type is not preserved.
func recordSpanException(span oteltrace.Span, err error) string {
	scrubbed := security.Scrub(err.Error())
	span.RecordError(errors.New(scrubbed))
	return scrubbed
}

// recordSpanError records err as an exception event and marks span as
// failed, both with scrubbed text. It returns the scrubbed text for
// reuse in logs and events.
func recordSpanError(span oteltrace.Span, err error) string {
	scrubbed := recordSpanException(span, err)
	span.SetStatus(codes.Error, scrubbed)
	return scrubbed
}
