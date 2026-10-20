package cmd

import (
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
)

// otlpSpanReceiver decodes every OTLP/HTTP trace export it receives and
// accepts other signals without decoding them.
type otlpSpanReceiver struct {
	mu    sync.Mutex
	spans []*otlptrace.Span
	errs  []string
}

func (r *otlpSpanReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if !strings.HasSuffix(req.URL.Path, "/v1/traces") {
		w.WriteHeader(http.StatusOK)
		return
	}
	var export collectortrace.ExportTraceServiceRequest
	if err == nil {
		err = proto.Unmarshal(body, &export)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.errs = append(r.errs, err.Error())
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for _, rs := range export.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			r.spans = append(r.spans, ss.GetSpans()...)
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (r *otlpSpanReceiver) span(name string) (*otlptrace.Span, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for _, s := range r.spans {
		if s.GetName() == name {
			return s, nil
		}
		names = append(names, s.GetName())
	}
	return nil, append(names, r.errs...)
}

func (r *otlpSpanReceiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.spans)
}

const (
	upstreamTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	upstreamSpanID  = "00f067aa0ba902b7"
)

// runParentedJob runs task through stirrup job with the given traceparent
// on its task_assignment, exporting spans to a fake OTLP/HTTP receiver.
// The sampler env var is cleared so the SDK's parent-based default applies.
func runParentedJob(t *testing.T, task *pb.RunConfig, traceparent string) (*recordingControlPlane, *otlpSpanReceiver, error) {
	t.Helper()
	must := require.New(t)
	useTempMarkerPaths(t)
	t.Setenv("OTEL_TRACES_SAMPLER", "")
	receiver := &otlpSpanReceiver{}
	otlp := httptest.NewServer(receiver)
	t.Cleanup(otlp.Close)

	task.TraceEmitter = &pb.TraceEmitterConfig{Type: "otel", Endpoint: otlp.URL, Protocol: "http/protobuf"}
	srv := newRecordingControlPlane(task)
	srv.traceContext = &pb.TraceContext{Traceparent: traceparent}
	startControlPlane(t, srv)
	t.Setenv("CONTROL_PLANE_SESSION_ID", "")

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()
	select {
	case runErr := <-errCh:
		return srv, receiver, runErr
	case <-time.After(terminalWaitBudget):
		must.Failf("runJob did not return", "control plane saw %v", srv.events())
		return nil, nil, nil
	}
}

func TestRunJob_TaskAssignmentTraceContextParentsRunSpan(t *testing.T) {
	must := require.New(t)
	srv, receiver, runErr := runParentedJob(t, fatalPreRunHookJobConfig(t),
		"00-"+upstreamTraceID+"-"+upstreamSpanID+"-01")

	t.Run("run fails on the fatal preRun hook", func(t *testing.T) {
		must := require.New(t)
		must.Error(runErr, "the fatal preRun hook should fail the process")
	})

	t.Run("done event references the assignment trace", func(t *testing.T) {
		must := require.New(t)
		done := srv.findEvent("done")
		must.NotNil(done.GetTrace().GetTraceReference(),
			"done event %v should carry a trace reference", done)
		must.Equal(upstreamTraceID, done.GetTrace().GetTraceReference().GetTraceId(),
			"done trace_reference.trace_id should be the assignment's trace")
	})

	root, seen := receiver.span("invoke_agent stirrup")
	must.NotNil(root, "no exported invoke_agent stirrup span; received %s", strings.Join(seen, ", "))

	t.Run("run span joins the assignment trace", func(t *testing.T) {
		must := require.New(t)
		must.Equal(upstreamTraceID, hex.EncodeToString(root.GetTraceId()),
			"exported run span trace ID should be the assignment's trace")
	})

	t.Run("run span is a child of the assignment span", func(t *testing.T) {
		must := require.New(t)
		must.Equal(upstreamSpanID, hex.EncodeToString(root.GetParentSpanId()),
			"exported run span parent should be the assignment's span")
	})
}

func TestRunJob_UnsampledTaskAssignmentExportsNothing(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	provider, requests := startOpenAIStub(t)
	task := followUpJobConfig(t, provider.URL, 30, 0)
	task.FollowUpGrace = nil

	srv, receiver, runErr := runParentedJob(t, task, "00-"+upstreamTraceID+"-"+upstreamSpanID+"-00")
	must.NoError(runErr, "runJob")
	must.NotZero(requests.Load(), "the run never reached the provider")

	started := srv.findEvent("run_started")
	must.NotNil(started.GetTraceReference(),
		"no run_started with a trace reference; got %v", srv.events())
	ref := started.GetTraceReference()

	t.Run("run_started is not sampled", func(t *testing.T) {
		must := require.New(t)
		must.False(ref.GetSampled(),
			"run_started should not report sampled under an unsampled parent")
	})

	t.Run("run_started keeps the assignment trace ID", func(t *testing.T) {
		must := require.New(t)
		must.Equal(upstreamTraceID, ref.GetTraceId(), "run_started trace_id")
	})

	t.Run("done event reports success", func(t *testing.T) {
		must := require.New(t)
		done := srv.findEvent("done")
		must.Equal("success", done.GetStopReason(), "done event %v", done)
	})

	t.Run("no spans are exported", func(t *testing.T) {
		must := require.New(t)
		_, names := receiver.span("")
		must.Zero(receiver.count(),
			"OTLP receiver got spans under an unsampled parent: %v", names)
	})
}
