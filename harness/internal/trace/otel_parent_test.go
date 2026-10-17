package trace

import (
	"context"
	"testing"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"
)

func remoteParent(t *testing.T, sampled bool) oteltrace.SpanContext {
	t.Helper()
	traceID, err := oteltrace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := oteltrace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatal(err)
	}
	cfg := oteltrace.SpanContextConfig{TraceID: traceID, SpanID: spanID, Remote: true}
	if sampled {
		cfg.TraceFlags = oteltrace.FlagsSampled
	}
	return oteltrace.NewSpanContext(cfg)
}

func TestOTelTraceEmitter_StartWithContextAdoptsParent(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()
	parent := remoteParent(t, true)

	emitter.StartWithContext(oteltrace.ContextWithRemoteSpanContext(context.Background(), parent), "run-parented", nil)
	ref := emitter.TraceReference()
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	root := findSpanByName(t, exporter.GetSpans(), "invoke_agent stirrup")
	if root.Parent.TraceID() != parent.TraceID() || root.Parent.SpanID() != parent.SpanID() {
		t.Errorf("root parent = %s/%s, want %s/%s", root.Parent.TraceID(), root.Parent.SpanID(), parent.TraceID(), parent.SpanID())
	}
	if !root.Parent.IsRemote() {
		t.Error("root parent is not marked remote")
	}
	if root.SpanContext.TraceID() != parent.TraceID() {
		t.Errorf("root trace ID = %s, want the parent's %s", root.SpanContext.TraceID(), parent.TraceID())
	}
	if ref == nil || ref.TraceID != parent.TraceID().String() || ref.RunSpanID != root.SpanContext.SpanID().String() {
		t.Errorf("TraceReference = %+v, want the parent's trace and the run span's ID", ref)
	}
}

func TestOTelTraceEmitter_StartWithContextWithoutValidParentStartsNewTrace(t *testing.T) {
	invalid := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{Remote: true})
	for name, ctx := range map[string]context.Context{
		"absent":  context.Background(),
		"invalid": oteltrace.ContextWithRemoteSpanContext(context.Background(), invalid),
	} {
		t.Run(name, func(t *testing.T) {
			emitter, exporter := newTestOTelEmitter()
			emitter.StartWithContext(ctx, "run-root", nil)
			if _, err := emitter.Finish(context.Background(), "success"); err != nil {
				t.Fatalf("Finish: %v", err)
			}
			root := findSpanByName(t, exporter.GetSpans(), "invoke_agent stirrup")
			if root.Parent.IsValid() {
				t.Errorf("root parent = %+v, want none", root.Parent)
			}
			if !root.SpanContext.IsValid() {
				t.Error("root span context is invalid")
			}
		})
	}
}

func TestOTelTraceEmitter_StartWithContextDropsCallerCancellation(t *testing.T) {
	emitter, _ := newTestOTelEmitter()
	parentCtx := oteltrace.ContextWithRemoteSpanContext(context.Background(), remoteParent(t, true))
	ctx, cancel := context.WithTimeout(parentCtx, time.Hour)

	emitter.StartWithContext(ctx, "run-cancel", nil)
	cancel()

	root := emitter.RootContext()
	if root.Err() != nil {
		t.Errorf("RootContext().Err() = %v after the caller's ctx was cancelled, want nil", root.Err())
	}
	if _, ok := root.Deadline(); ok {
		t.Error("RootContext carries the caller's deadline")
	}
	if root.Done() != nil {
		t.Error("RootContext has a Done channel inherited from the caller")
	}
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

func TestOnlyOTelEmitterStartsWithContext(t *testing.T) {
	for name, e := range map[string]TraceEmitter{
		"jsonl":  &JSONLTraceEmitter{},
		"gcs":    &GCSTraceEmitter{},
		"nested": &NestedJSONLEmitter{},
	} {
		if _, ok := e.(ContextStarter); ok {
			t.Errorf("%s emitter implements ContextStarter; its run would be parented on the caller's span", name)
		}
	}
}
