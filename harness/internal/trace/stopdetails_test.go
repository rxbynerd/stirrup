package trace

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/types"
)

// stopDetailsEmitter is a TraceEmitter that also records stop details;
// every production emitter satisfies it.
type stopDetailsEmitter interface {
	TraceEmitter
	StopDetailsRecorder
}

func TestEmitters_RecordStopDetailsReachesRunTrace(t *testing.T) {
	gcsSrv := httptest.NewServer(newGCSCaptureServer().handler())
	t.Cleanup(gcsSrv.Close)

	emitters := map[string]func(t *testing.T) stopDetailsEmitter{
		"jsonl": func(*testing.T) stopDetailsEmitter {
			return NewJSONLTraceEmitter(&bytes.Buffer{}, false)
		},
		"nested": func(*testing.T) stopDetailsEmitter {
			return NewNestedJSONLEmitter(NewJSONLTraceEmitter(&bytes.Buffer{}, false), "parent-run")
		},
		"otel": func(t *testing.T) stopDetailsEmitter {
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
			return NewOTelTraceEmitterForTest(tp)
		},
		"gcs": func(t *testing.T) stopDetailsEmitter {
			e, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
				Bucket:           "b",
				CredentialSource: &staticBearerSource{token: "t"},
				EndpointBaseURL:  gcsSrv.URL,
			})
			if err != nil {
				t.Fatalf("NewGCSTraceEmitter: %v", err)
			}
			return e
		},
	}

	details := &types.StopDetails{Type: "refusal", Category: "cyber", Explanation: "declined"}
	for name, build := range emitters {
		t.Run(name, func(t *testing.T) {
			e := build(t)

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			e.RecordStopDetails(details)
			got, err := e.Finish(context.Background(), "refusal")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if got.StopDetails == nil || *got.StopDetails != *details {
				t.Errorf("StopDetails = %+v, want %+v", got.StopDetails, details)
			}

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if got.StopDetails != nil {
				t.Errorf("StopDetails after restart = %+v, want nil", got.StopDetails)
			}
		})
	}
}

func TestNestedJSONLEmitter_StopDetailsNotForwardedToParent(t *testing.T) {
	parent := NewJSONLTraceEmitter(&bytes.Buffer{}, false)
	parent.Start("parent-run", &types.RunConfig{RunID: "parent-run"})

	child := NewNestedJSONLEmitter(parent, "parent-run")
	child.Start("child-run", &types.RunConfig{RunID: "child-run"})
	child.RecordStopDetails(&types.StopDetails{Type: "refusal"})
	if _, err := child.Finish(context.Background(), "refusal"); err != nil {
		t.Fatalf("child Finish: %v", err)
	}

	got, err := parent.Finish(context.Background(), "success")
	if err != nil {
		t.Fatalf("parent Finish: %v", err)
	}
	if got.StopDetails != nil {
		t.Errorf("parent StopDetails = %+v, want nil", got.StopDetails)
	}
}
