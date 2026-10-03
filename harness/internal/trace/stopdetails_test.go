package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
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

// stopDetailsEmitterBuilders returns a constructor for each production
// emitter, with GCS uploads going to a local capture server.
func stopDetailsEmitterBuilders(t *testing.T) map[string]func(t *testing.T) stopDetailsEmitter {
	gcsSrv := httptest.NewServer(newGCSCaptureServer().handler())
	t.Cleanup(gcsSrv.Close)

	return map[string]func(t *testing.T) stopDetailsEmitter{
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
}

func TestEmitters_RecordStopDetailsReachesRunTrace(t *testing.T) {
	want := types.StopDetails{Type: "refusal", Category: "cyber", Explanation: "declined"}
	for name, build := range stopDetailsEmitterBuilders(t) {
		t.Run(name, func(t *testing.T) {
			e := build(t)

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			details := want
			e.RecordStopDetails(&details)
			got, err := e.Finish(context.Background(), "refusal")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if got.StopDetails == nil || *got.StopDetails != want {
				t.Errorf("StopDetails = %+v, want %+v", got.StopDetails, want)
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

func TestEmitters_RecordStopDetailsNilClearsPriorValue(t *testing.T) {
	for name, build := range stopDetailsEmitterBuilders(t) {
		t.Run(name, func(t *testing.T) {
			e := build(t)
			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			e.RecordStopDetails(&types.StopDetails{Type: "refusal"})
			e.RecordStopDetails(nil)
			got, err := e.Finish(context.Background(), "refusal")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if got.StopDetails != nil {
				t.Errorf("StopDetails = %+v, want nil after RecordStopDetails(nil)", got.StopDetails)
			}
		})
	}
}

// TestEmitters_RecordStopDetailsConcurrentWithFinish is meaningful under
// -race: a record racing Finish must not be an unsynchronised access.
func TestEmitters_RecordStopDetailsConcurrentWithFinish(t *testing.T) {
	for name, build := range stopDetailsEmitterBuilders(t) {
		t.Run(name, func(t *testing.T) {
			e := build(t)
			e.Start("run-1", &types.RunConfig{RunID: "run-1"})

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 50; i++ {
					e.RecordStopDetails(&types.StopDetails{Type: "refusal"})
				}
			}()
			if _, err := e.Finish(context.Background(), "refusal"); err != nil {
				t.Errorf("Finish: %v", err)
			}
			wg.Wait()
		})
	}
}

func TestGCSTraceEmitter_UploadedObjectCarriesStopDetails(t *testing.T) {
	srv := newGCSCaptureServer()
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	emitter, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "b",
		CredentialSource: &staticBearerSource{token: "t"},
		EndpointBaseURL:  httpSrv.URL,
	})
	if err != nil {
		t.Fatalf("NewGCSTraceEmitter: %v", err)
	}
	want := types.StopDetails{Type: "refusal", Category: "bio", Explanation: "declined"}
	emitter.Start("run-gcs", &types.RunConfig{RunID: "run-gcs"})
	details := want
	emitter.RecordStopDetails(&details)
	if _, err := emitter.Finish(context.Background(), "refusal"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	var uploaded types.RunTrace
	body := strings.TrimRight(string(srv.last().Body), "\n")
	if err := json.Unmarshal([]byte(body), &uploaded); err != nil {
		t.Fatalf("unmarshal uploaded body: %v\nbody=%q", err, body)
	}
	if uploaded.Outcome != "refusal" || uploaded.StopDetails == nil || *uploaded.StopDetails != want {
		t.Errorf("uploaded trace outcome=%q StopDetails=%+v, want refusal with %+v", uploaded.Outcome, uploaded.StopDetails, want)
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
