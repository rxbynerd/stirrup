package cmd

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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
		t.Fatalf("runJob did not return; control plane saw %v", srv.events())
		return nil, nil, nil
	}
}

func TestRunJob_TaskAssignmentTraceContextParentsRunSpan(t *testing.T) {
	srv, receiver, runErr := runParentedJob(t, fatalPreRunHookJobConfig(t),
		"00-"+upstreamTraceID+"-"+upstreamSpanID+"-01")
	if runErr == nil {
		t.Error("runJob returned nil, want the fatal preRun hook to fail the process")
	}

	done := srv.findEvent("done")
	if done == nil || done.GetTrace().GetTraceReference() == nil {
		t.Fatalf("done event = %v, want a summary with a trace reference", done)
	}
	if got := done.GetTrace().GetTraceReference().GetTraceId(); got != upstreamTraceID {
		t.Errorf("done trace_reference.trace_id = %s, want the assignment's %s", got, upstreamTraceID)
	}

	root, seen := receiver.span("invoke_agent stirrup")
	if root == nil {
		t.Fatalf("no exported invoke_agent stirrup span; received %s", strings.Join(seen, ", "))
	}
	wantTrace, _ := hex.DecodeString(upstreamTraceID)
	wantParent, _ := hex.DecodeString(upstreamSpanID)
	if !bytes.Equal(root.GetTraceId(), wantTrace) {
		t.Errorf("exported run span trace ID = %x, want %s", root.GetTraceId(), upstreamTraceID)
	}
	if !bytes.Equal(root.GetParentSpanId(), wantParent) {
		t.Errorf("exported run span parent = %x, want the assignment's span %s", root.GetParentSpanId(), upstreamSpanID)
	}
}

func TestRunJob_UnsampledTaskAssignmentExportsNothing(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	provider, requests := startOpenAIStub(t)
	task := followUpJobConfig(t, provider.URL, 30, 0)
	task.FollowUpGrace = nil

	srv, receiver, runErr := runParentedJob(t, task, "00-"+upstreamTraceID+"-"+upstreamSpanID+"-00")
	if runErr != nil {
		t.Fatalf("runJob: %v", runErr)
	}
	if requests.Load() == 0 {
		t.Fatal("the run never reached the provider")
	}

	started := srv.findEvent("run_started")
	if started == nil || started.GetTraceReference() == nil {
		t.Fatalf("no run_started with a trace reference; got %v", srv.events())
	}
	ref := started.GetTraceReference()
	if ref.GetSampled() {
		t.Error("run_started reports sampled: true under an unsampled parent")
	}
	if ref.GetTraceId() != upstreamTraceID {
		t.Errorf("run_started trace_id = %s, want the assignment's %s", ref.GetTraceId(), upstreamTraceID)
	}
	if done := srv.findEvent("done"); done == nil || done.GetStopReason() != "success" {
		t.Errorf("done = %v, want stop_reason success", done)
	}
	if n := receiver.count(); n != 0 {
		_, names := receiver.span("")
		t.Errorf("OTLP receiver got %d spans under an unsampled parent, want none: %v", n, names)
	}
}
