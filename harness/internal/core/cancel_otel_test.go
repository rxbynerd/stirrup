package core

import (
	"context"
	"errors"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
)

// errCustomCancelSentinel is a non-nil cancel cause that is neither
// ErrCancelledByControlPlane nor context.DeadlineExceeded nor
// context.Canceled. It exercises the "unrecognised cause" branch of
// classifyCtxOutcome (→ "error") and the default return branch of
// setRootCancelAttribute (no attribute set).
var errCustomCancelSentinel = errors.New("test custom cancel sentinel")

// fireAndCloseProvider fires a pre-configured action on the first Stream
// call and then returns a channel containing a single tool_use-stop-reason
// event followed by channel close. This lets tests deliver a cancel
// ControlEvent (or perform any other side effect) before the outer loop
// re-enters runInnerLoop for the next turn boundary — without relying on
// the provider's ctx being cancelled, which is needed for these OTel
// tests because the provider sees the trace span context (derived from
// context.Background) rather than the run context.
type fireAndCloseProvider struct {
	onStream func()
	fired    bool
}

func (p *fireAndCloseProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	if !p.fired && p.onStream != nil {
		p.fired = true
		p.onStream()
	}
	ch := make(chan types.StreamEvent, 2)
	// Emit a tool_call so the loop completes the turn normally and loops
	// back to the outer turn-boundary ctx check, where the cancel takes
	// effect. Using a tool_use stop_reason ensures the loop iterates.
	ch <- types.StreamEvent{
		Type:  "tool_call",
		ID:    "tc_otel_test",
		Name:  "test_tool",
		Input: map[string]any{},
	}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: "tool_use"}
	close(ch)
	return ch, nil
}

// newInMemoryOTelEmitter wires an OTelTraceEmitter to an in-memory span
// exporter so tests can assert on emitted span attributes without touching
// the network. The returned exporter captures spans as they are ended; the
// root "run" span is ended inside Finish, so GetSpans() after Run returns
// includes the root span and its attributes.
func newInMemoryOTelEmitter(t *testing.T) (*trace.OTelTraceEmitter, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	return trace.NewOTelTraceEmitterForTest(tp), exporter
}

// findRootRunSpanAttribute scans the recorded spans for the root "run"
// span and returns the value of the given attribute, or an empty string if
// either the span or the attribute is absent.
func findRootRunSpanAttribute(spans []tracetest.SpanStub, key string) (string, bool) {
	for _, s := range spans {
		if s.Name != "run" {
			continue
		}
		for _, attr := range s.Attributes {
			if string(attr.Key) == key {
				return attr.Value.AsString(), true
			}
		}
	}
	return "", false
}

// TestLoop_CancelAttribute_ControlPlane covers the OTel
// run.cancelled_by="control_plane" branch of setRootCancelAttribute. A
// "cancel" ControlEvent fires between turns so context.Cause(runCtx) is
// ErrCancelledByControlPlane; the attribute must therefore be
// "control_plane".
func TestLoop_CancelAttribute_ControlPlane(t *testing.T) {
	emitter, exporter := newInMemoryOTelEmitter(t)

	tr := &cancellableTransport{}
	loop := buildTestLoop(nil)
	loop.Transport = tr
	loop.Trace = emitter
	loop.Provider = &fireAndCloseProvider{
		onStream: func() {
			// Fire the cancel BEFORE the next turn boundary ctx check.
			tr.FireControl(types.ControlEvent{Type: "cancel"})
		},
	}

	config := buildTestConfig()
	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if runTrace.Outcome != "cancelled" {
		t.Fatalf("prerequisite: expected outcome 'cancelled', got %q", runTrace.Outcome)
	}

	got, ok := findRootRunSpanAttribute(exporter.GetSpans(), "run.cancelled_by")
	if !ok {
		t.Fatal("expected run.cancelled_by attribute on root span, none found")
	}
	if got != "control_plane" {
		t.Errorf("run.cancelled_by: got %q, want %q", got, "control_plane")
	}
}

// TestLoop_CancelAttribute_Deadline covers the OTel
// run.cancelled_by="deadline" branch. A ctx with a short deadline is used
// so that classifyCtxOutcome sees context.DeadlineExceeded as the cause.
// The provider emits a normal turn; deadline expiry is detected at the
// next turn boundary ctx check.
func TestLoop_CancelAttribute_Deadline(t *testing.T) {
	emitter, exporter := newInMemoryOTelEmitter(t)

	tr := &cancellableTransport{}
	loop := buildTestLoop(nil)
	loop.Transport = tr
	loop.Trace = emitter
	loop.Provider = &fireAndCloseProvider{
		onStream: func() {
			// Block just long enough for the deadline to fire before the
			// next turn's ctx check. 150ms against a 50ms deadline is
			// comfortably deterministic.
			time.Sleep(150 * time.Millisecond)
		},
	}

	config := buildTestConfig()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	runTrace, err := loop.Run(ctx, config)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if runTrace.Outcome != "timeout" {
		t.Fatalf("prerequisite: expected outcome 'timeout', got %q", runTrace.Outcome)
	}

	got, ok := findRootRunSpanAttribute(exporter.GetSpans(), "run.cancelled_by")
	if !ok {
		t.Fatal("expected run.cancelled_by attribute on root span, none found")
	}
	if got != "deadline" {
		t.Errorf("run.cancelled_by: got %q, want %q", got, "deadline")
	}
}

// TestSetRootCancelAttribute_NoStartedRunSpan covers the defensive
// !span.SpanContext().IsValid() early-return branch: if the OTel emitter
// has not yet been Started (no root span), calling setRootCancelAttribute
// must be a safe no-op. This cannot happen on the Run() path (Start is
// always called before any cancel classification), but the guard remains
// for defence in depth.
func TestSetRootCancelAttribute_NoStartedRunSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	emitter := trace.NewOTelTraceEmitterForTest(tp)

	// Build a loop with the uninitialised OTel emitter — Start is never
	// called, so RootContext() returns context.Background() and the span
	// extracted from it has an invalid SpanContext.
	loop := buildTestLoop(nil)
	loop.Trace = emitter

	// Should return quietly without panicking and without setting an
	// attribute (there is no root span to set it on).
	loop.setRootCancelAttribute(ErrCancelledByControlPlane)

	for _, s := range exporter.GetSpans() {
		for _, attr := range s.Attributes {
			if string(attr.Key) == "run.cancelled_by" {
				t.Errorf("did not expect run.cancelled_by attribute on span %q", s.Name)
			}
		}
	}
}

// TestLoop_CancelAttribute_UnrecognisedCause covers the default return
// branch of setRootCancelAttribute: when the cause is non-nil and neither
// a recognised cancel sentinel nor a deadline, outcome classifies as
// "error" and NO run.cancelled_by attribute is set on the root span.
func TestLoop_CancelAttribute_UnrecognisedCause(t *testing.T) {
	emitter, exporter := newInMemoryOTelEmitter(t)

	tr := &cancellableTransport{}
	loop := buildTestLoop(nil)
	loop.Transport = tr
	loop.Trace = emitter

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	customCause := errCustomCancelSentinel
	loop.Provider = &fireAndCloseProvider{
		onStream: func() {
			cancel(customCause)
		},
	}

	config := buildTestConfig()
	runTrace, err := loop.Run(ctx, config)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if runTrace.Outcome != "error" {
		t.Fatalf("prerequisite: expected outcome 'error' for unrecognised cause, got %q", runTrace.Outcome)
	}

	// The default branch of setRootCancelAttribute returns without
	// setting the attribute, so it must be absent from the root span.
	if _, ok := findRootRunSpanAttribute(exporter.GetSpans(), "run.cancelled_by"); ok {
		t.Error("expected no run.cancelled_by attribute for unrecognised cause")
	}
}

// TestLoop_CancelAttribute_Signal covers the OTel
// run.cancelled_by="signal" branch. A plain cancel() on the parent ctx
// propagates context.Canceled as the cause of our WithCancelCause child —
// the same path SIGINT/SIGTERM takes via the root cobra signal handler.
func TestLoop_CancelAttribute_Signal(t *testing.T) {
	emitter, exporter := newInMemoryOTelEmitter(t)

	tr := &cancellableTransport{}
	loop := buildTestLoop(nil)
	loop.Transport = tr
	loop.Trace = emitter

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loop.Provider = &fireAndCloseProvider{
		onStream: func() {
			// Cancel the parent ctx (what a signal handler does); the
			// outer loop detects it at the next turn-boundary check with
			// context.Canceled as the cause.
			cancel()
		},
	}

	config := buildTestConfig()
	runTrace, err := loop.Run(ctx, config)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if runTrace.Outcome != "cancelled" {
		t.Fatalf("prerequisite: expected outcome 'cancelled', got %q", runTrace.Outcome)
	}

	got, ok := findRootRunSpanAttribute(exporter.GetSpans(), "run.cancelled_by")
	if !ok {
		t.Fatal("expected run.cancelled_by attribute on root span, none found")
	}
	if got != "signal" {
		t.Errorf("run.cancelled_by: got %q, want %q", got, "signal")
	}
}
