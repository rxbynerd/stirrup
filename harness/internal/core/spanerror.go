package core

import (
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/security"
)

// recordSpanException adds err to span as a semconv exception event with
// the error text scrubbed, and returns the scrubbed text. The log
// scrubber does not cover spans, so every error recorded on a span goes
// through here. exception.type names the most specific error type in the
// chain; type names are code identifiers and need no scrubbing.
func recordSpanException(span oteltrace.Span, err error) string {
	scrubbed := security.Scrub(err.Error())
	span.AddEvent("exception", oteltrace.WithAttributes(
		attribute.String("exception.type", errorTypeName(err)),
		attribute.String("exception.message", scrubbed),
	))
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

// errorTypeName returns the Go type of the first error in err's tree,
// searched depth-first through single and multi-error Unwrap, that is
// not a plain errors.New, fmt.Errorf or errors.Join wrapper, falling
// back to the outermost error's type.
func errorTypeName(err error) string {
	if name, ok := specificErrorType(err); ok {
		return name
	}
	return fmt.Sprintf("%T", err)
}

func specificErrorType(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	switch name := fmt.Sprintf("%T", err); name {
	case "*errors.errorString", "*fmt.wrapError", "*fmt.wrapErrors", "*errors.joinError":
	default:
		return name, true
	}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return specificErrorType(u.Unwrap())
	case interface{ Unwrap() []error }:
		for _, child := range u.Unwrap() {
			if name, ok := specificErrorType(child); ok {
				return name, true
			}
		}
	}
	return "", false
}
