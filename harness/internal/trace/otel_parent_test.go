package trace

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func remoteParent(t *testing.T, sampled bool) oteltrace.SpanContext {
	t.Helper()
	must := require.New(t)
	traceID, err := oteltrace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	must.NoError(err)
	spanID, err := oteltrace.SpanIDFromHex("00f067aa0ba902b7")
	must.NoError(err)
	cfg := oteltrace.SpanContextConfig{TraceID: traceID, SpanID: spanID, Remote: true}
	if sampled {
		cfg.TraceFlags = oteltrace.FlagsSampled
	}
	return oteltrace.NewSpanContext(cfg)
}

func TestOTelTraceEmitter_StartWithContextAdoptsParent(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	emitter, exporter := newTestOTelEmitter()
	parent := remoteParent(t, true)

	emitter.StartWithContext(oteltrace.ContextWithRemoteSpanContext(context.Background(), parent), "run-parented", nil)
	ref := emitter.TraceReference()
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)

	root := findSpanByName(t, exporter.GetSpans(), "invoke_agent stirrup")
	check.Equal(parent.TraceID().String(), root.Parent.TraceID().String(), "root parent trace ID")
	check.Equal(parent.SpanID().String(), root.Parent.SpanID().String(), "root parent span ID")
	check.True(root.Parent.IsRemote(), "root parent should be marked remote")
	check.Equal(parent.TraceID().String(), root.SpanContext.TraceID().String(), "root trace ID")
	must.NotNil(ref, "TraceReference")
	check.Equal(parent.TraceID().String(), ref.TraceID, "TraceReference trace ID")
	check.Equal(root.SpanContext.SpanID().String(), ref.RunSpanID, "TraceReference run span ID")
}

func TestOTelTraceEmitter_StartWithContextWithoutValidParentStartsNewTrace(t *testing.T) {
	invalid := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{Remote: true})
	for name, ctx := range map[string]context.Context{
		"absent":  context.Background(),
		"invalid": oteltrace.ContextWithRemoteSpanContext(context.Background(), invalid),
	} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			emitter, exporter := newTestOTelEmitter()
			emitter.StartWithContext(ctx, "run-root", nil)
			_, err := emitter.Finish(context.Background(), "success")
			must.NoError(err)

			root := findSpanByName(t, exporter.GetSpans(), "invoke_agent stirrup")
			must.False(root.Parent.IsValid(), "root parent = %+v, want none", root.Parent)
			must.True(root.SpanContext.IsValid())
		})
	}
}

func TestOTelTraceEmitter_StartWithContextDropsCallerCancellation(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	emitter, _ := newTestOTelEmitter()
	parentCtx := oteltrace.ContextWithRemoteSpanContext(context.Background(), remoteParent(t, true))
	ctx, cancel := context.WithTimeout(parentCtx, time.Hour)

	emitter.StartWithContext(ctx, "run-cancel", nil)
	cancel()

	root := emitter.RootContext()
	must.NoError(root.Err(), "RootContext should not inherit the caller's cancellation")
	_, hasDeadline := root.Deadline()
	check.False(hasDeadline, "RootContext should not carry the caller's deadline")
	check.Nil(root.Done(), "RootContext should not have a Done channel inherited from the caller")
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)
}

func TestTraceEmitter_OnlyOTelEmitterImplementsContextStarter(t *testing.T) {
	for name, e := range map[string]TraceEmitter{
		"jsonl":  &JSONLTraceEmitter{},
		"gcs":    &GCSTraceEmitter{},
		"nested": &NestedJSONLEmitter{},
	} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			_, ok := e.(ContextStarter)
			must.False(ok, "implements ContextStarter; its run would be parented on the caller's span")
		})
	}
}
