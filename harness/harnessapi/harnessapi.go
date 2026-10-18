// Package harnessapi provides the public API for embedding the stirrup harness
// in-process. It re-exports the minimal surface needed by external consumers
// (e.g. the stable control plane's local provisioner) without exposing the
// full internal implementation.
package harnessapi

import (
	"context"
	"io"

	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// Transport is the bidirectional channel between the harness and the
// control plane. Implementations must be safe for concurrent use.
//
// text_delta and thinking_delta events are re-chunked: concatenated in
// order they give the model's text with recognised secrets redacted, and
// held text is emitted no later than the turn's next non-delta event.
// The harness does not scrub every other event's string fields; the shipped
// transports scrub all of them, and an embedder's Transport should too.
type Transport = transport.Transport

// Loop wraps an AgenticLoop, exposing only the methods needed by external
// callers: Run and Close.
type Loop struct {
	inner *core.AgenticLoop
}

// BuildLoopWithTransport constructs an agentic loop from the given RunConfig,
// injecting the provided Transport for event I/O. When tp is non-nil it is
// used directly; when nil the transport is built from config.Transport.
func BuildLoopWithTransport(ctx context.Context, config *types.RunConfig, tp Transport) (*Loop, error) {
	al, err := core.BuildLoopWithTransport(ctx, config, tp)
	if err != nil {
		return nil, err
	}
	return &Loop{inner: al}, nil
}

// Run executes the agentic loop to completion and returns the run trace.
// A non-nil trace returned with a non-nil error means the run completed
// but its trace could not be persisted.
//
// With the otel trace emitter, a valid OpenTelemetry span context on ctx
// becomes the parent of the run span, so the run joins the caller's trace
// and follows its sampling decision. Only the span context is used, never
// ctx's cancellation or deadline. To start a new trace, pass a ctx without
// a span, such as one derived from context.Background.
func (l *Loop) Run(ctx context.Context, config *types.RunConfig) (*types.RunTrace, error) {
	return l.inner.Run(ctx, config)
}

// Close releases all resources owned by the loop.
func (l *Loop) Close() error {
	return l.inner.Close()
}

var _ io.Closer = (*Loop)(nil)
