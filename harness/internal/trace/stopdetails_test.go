package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
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
			return NewNestedJSONLEmitter(NewJSONLTraceEmitter(&bytes.Buffer{}, false), "parent-run", context.Background())
		},
		"otel": func(t *testing.T) stopDetailsEmitter {
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
			return NewOTelTraceEmitterForTest(tp)
		},
		"gcs": func(t *testing.T) stopDetailsEmitter {
			must := require.New(t)
			e, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
				Bucket:           "b",
				CredentialSource: &staticBearerSource{token: "t"},
				EndpointBaseURL:  gcsSrv.URL,
			})
			must.NoError(err, "NewGCSTraceEmitter")
			return e
		},
	}
}

func TestEmitters_RecordStopDetailsReachesRunTrace(t *testing.T) {
	want := types.StopDetails{Type: "refusal", Category: "cyber", Explanation: "declined"}
	for name, build := range stopDetailsEmitterBuilders(t) {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			e := build(t)

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			details := want
			e.RecordStopDetails(&details)
			got, err := e.Finish(context.Background(), "refusal")
			must.NoError(err)
			must.NotNil(got.StopDetails)
			must.Equal(want, *got.StopDetails)

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			must.NoError(err)
			must.Nil(got.StopDetails, "after restart")
		})
	}
}

func TestEmitters_RecordStopDetailsNilClearsPriorValue(t *testing.T) {
	for name, build := range stopDetailsEmitterBuilders(t) {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			e := build(t)
			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			e.RecordStopDetails(&types.StopDetails{Type: "refusal"})
			e.RecordStopDetails(nil)
			got, err := e.Finish(context.Background(), "refusal")
			must.NoError(err)
			must.Nil(got.StopDetails, "after RecordStopDetails(nil)")
		})
	}
}

// TestEmitters_RecordStopDetailsConcurrentWithFinish is meaningful under
// -race: a record racing Finish must not be an unsynchronised access.
func TestEmitters_RecordStopDetailsConcurrentWithFinish(t *testing.T) {
	for name, build := range stopDetailsEmitterBuilders(t) {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
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
			_, err := e.Finish(context.Background(), "refusal")
			wg.Wait()
			must.NoError(err)
		})
	}
}

func TestGCSTraceEmitter_UploadedObjectCarriesStopDetails(t *testing.T) {
	must := require.New(t)
	srv := newGCSCaptureServer()
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	emitter, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "b",
		CredentialSource: &staticBearerSource{token: "t"},
		EndpointBaseURL:  httpSrv.URL,
	})
	must.NoError(err)
	want := types.StopDetails{Type: "refusal", Category: "bio", Explanation: "declined"}
	emitter.Start("run-gcs", &types.RunConfig{RunID: "run-gcs"})
	details := want
	emitter.RecordStopDetails(&details)
	_, err = emitter.Finish(context.Background(), "refusal")
	must.NoError(err)

	var uploaded types.RunTrace
	body := strings.TrimRight(string(srv.last().Body), "\n")
	must.NoError(json.Unmarshal([]byte(body), &uploaded), "body=%q", body)
	must.Equal("refusal", uploaded.Outcome)
	must.NotNil(uploaded.StopDetails)
	must.Equal(want, *uploaded.StopDetails)
}

func TestNestedJSONLEmitter_StopDetailsNotForwardedToParent(t *testing.T) {
	must := require.New(t)
	parent := NewJSONLTraceEmitter(&bytes.Buffer{}, false)
	parent.Start("parent-run", &types.RunConfig{RunID: "parent-run"})

	child := NewNestedJSONLEmitter(parent, "parent-run", context.Background())
	child.Start("child-run", &types.RunConfig{RunID: "child-run"})
	child.RecordStopDetails(&types.StopDetails{Type: "refusal"})
	_, err := child.Finish(context.Background(), "refusal")
	must.NoError(err, "child Finish")

	got, err := parent.Finish(context.Background(), "success")
	must.NoError(err, "parent Finish")
	must.Nil(got.StopDetails)
}

// degradedCapabilitiesEmitter is a TraceEmitter that also records degraded
// capabilities; the top-level production emitters satisfy it.
type degradedCapabilitiesEmitter interface {
	TraceEmitter
	DegradedCapabilitiesRecorder
}

func TestEmitters_RecordDegradedCapabilitiesReachesRunTrace(t *testing.T) {
	want := []types.DegradedCapability{{Kind: types.DegradedCapabilityMCP, Name: "docs", Error: "connection refused"}}
	for name, build := range stopDetailsEmitterBuilders(t) {
		e, ok := build(t).(degradedCapabilitiesEmitter)
		if !ok {
			continue // the nested child emitter carries no degraded capabilities
		}
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			e.RecordDegradedCapabilities(want)
			got, err := e.Finish(context.Background(), "success")
			must.NoError(err)
			must.Equal(want, got.DegradedCapabilities)

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			must.NoError(err)
			must.Empty(got.DegradedCapabilities, "after restart")
		})
	}
}
