package platform

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/observability/otlp"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
	"github.com/rxbynerd/stirrup/types/version"
)

func TestLangfuse_Profile(t *testing.T) {
	p, err := Resolve(context.Background(), "langfuse", map[string]string{"host": "https://langfuse.example.com/"}, Deps{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.RequiredProtocol != otlp.ProtocolHTTPProtobuf {
		t.Errorf("RequiredProtocol = %q, want %q", p.RequiredProtocol, otlp.ProtocolHTTPProtobuf)
	}
	traces := p.Signals[otlp.SignalTraces]
	if traces.Endpoint != "https://langfuse.example.com/api/public/otel" {
		t.Errorf("traces endpoint = %q", traces.Endpoint)
	}
	if want := map[string]string{"x-langfuse-ingestion-version": "4"}; !maps.Equal(traces.Headers, want) {
		t.Errorf("traces headers = %v, want %v", traces.Headers, want)
	}
	if _, ok := p.Signals[otlp.SignalMetrics]; ok {
		t.Error("the profile must leave metric export alone")
	}
	if _, ok := p.Signals[otlp.SignalLogs]; ok {
		t.Error("the profile must leave log export alone")
	}
	if p.WrapTransport != nil || p.ResourceAttrs != nil {
		t.Errorf("langfuse neither signs requests nor adds resource attributes, got %+v", p)
	}
	if p.WrapSpanExporter == nil {
		t.Fatal("langfuse must decorate spans")
	}

	prefixed, err := Resolve(context.Background(), "langfuse", map[string]string{"host": "https://proxy.example.com/langfuse"}, Deps{})
	if err != nil {
		t.Fatalf("Resolve with a path prefix: %v", err)
	}
	if got := prefixed.Signals[otlp.SignalTraces].Endpoint; got != "https://proxy.example.com/langfuse/api/public/otel" {
		t.Errorf("a host path prefix must be kept, got %q", got)
	}

	noHost, err := Resolve(context.Background(), "langfuse", nil, Deps{})
	if err != nil {
		t.Fatalf("Resolve without options: %v", err)
	}
	if got := noHost.Signals[otlp.SignalTraces]; got.Endpoint != "" || got.Headers[langfuseIngestionVersionHeader] != "4" {
		t.Errorf("without host the profile must set the header but no endpoint, got %+v", got)
	}
}

func TestLangfuse_RejectsBadHost(t *testing.T) {
	cases := map[string]string{
		"no scheme":  "langfuse.example.com",
		"ftp scheme": "ftp://langfuse.example.com",
		"no host":    "https://",
		"userinfo":   "https://pk:sk-secret@langfuse.example.com",
		"query":      "https://langfuse.example.com?x=1",
		"fragment":   "https://langfuse.example.com#x",
	}
	for name, host := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Resolve(context.Background(), "langfuse", map[string]string{"host": host}, Deps{})
			if err == nil {
				t.Fatalf("host %q must be rejected", host)
			}
			if strings.Contains(err.Error(), "sk-secret") {
				t.Errorf("the error must not echo the host: %v", err)
			}
		})
	}
}

// TestLangfuse_ApplyRoutesTracesToTheHost pins that the trace exporter
// dials the profile's endpoint over OTLP/HTTP with the ingestion header
// beside the operator's Authorization, that metrics stay off, and that
// gRPC is rejected.
func TestLangfuse_ApplyRoutesTracesToTheHost(t *testing.T) {
	p, err := Resolve(context.Background(), "langfuse", map[string]string{"host": "https://langfuse.example.com"}, Deps{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, err := Apply(Input{
		TraceEmitter: types.TraceEmitterConfig{Type: "otel"},
		Headers:      map[string]string{"Authorization": "Basic cGs6c2s="},
		Getenv:       noEnv,
	}, p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.Traces.Protocol != otlp.ProtocolHTTPProtobuf || got.Traces.Endpoint != "https://langfuse.example.com/api/public/otel" {
		t.Errorf("traces = %+v", got.Traces)
	}
	wantHeaders := map[string]string{"Authorization": "Basic cGs6c2s=", "x-langfuse-ingestion-version": "4"}
	if !maps.Equal(got.Traces.Headers, wantHeaders) {
		t.Errorf("trace headers = %v, want %v", got.Traces.Headers, wantHeaders)
	}
	if got.Metrics.Endpoint != "" {
		t.Errorf("metrics endpoint = %q, want metric export disabled", got.Metrics.Endpoint)
	}

	_, err = Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel", Protocol: otlp.ProtocolGRPC}, Getenv: noEnv}, p)
	if err == nil || !strings.Contains(err.Error(), `platform "langfuse" requires protocol http/protobuf`) {
		t.Errorf("protocol grpc with platform langfuse: got %v", err)
	}
}

// exportThroughLangfuse records the spans built by fn through the langfuse
// span decorator, under a resource carrying res, and returns what reached
// the downstream exporter.
func exportThroughLangfuse(t *testing.T, res []attribute.KeyValue, fn func(tracer oteltrace.Tracer)) map[string]map[attribute.Key]attribute.Value {
	t.Helper()
	p, err := Resolve(context.Background(), "langfuse", nil, Deps{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	sink := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(p.WrapSpanExporter(sink)),
		sdktrace.WithResource(sdkresource.NewSchemaless(res...)),
	)
	fn(tp.Tracer("test"))
	exported := sink.GetSpans()
	if err := tp.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	out := map[string]map[attribute.Key]attribute.Value{}
	for _, s := range exported {
		attrs := map[attribute.Key]attribute.Value{}
		for _, kv := range s.Attributes {
			if _, dup := attrs[kv.Key]; dup {
				t.Errorf("span %q carries %s twice", s.Name, kv.Key)
			}
			attrs[kv.Key] = kv.Value
		}
		out[s.Name] = attrs
	}
	return out
}

var langfuseTestResource = []attribute.KeyValue{
	attribute.String("service.version", "v1.2.3"),
	attribute.String("harness.run.mode", "execution"),
}

func runSpanAttributes() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "invoke_agent"),
		attribute.String("gen_ai.agent.name", "stirrup"),
		attribute.String("run.id", "run-1"),
		attribute.String("run.mode", "execution"),
	}
}

func correlationAttributes() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("stirrup.control_plane.url", "https://cp.example.com/runs/1"),
		attribute.String("stirrup.task.id", "task-7"),
		attribute.String("stirrup.attempt.id", "attempt-2"),
		attribute.String("stirrup.label.agent_profile", "triage-v3"),
		attribute.String("stirrup.label.execution_mode", "autonomous"),
	}
}

func TestLangfuse_TraceAttributesOnEverySpan(t *testing.T) {
	spans := exportThroughLangfuse(t, langfuseTestResource, func(tr oteltrace.Tracer) {
		ctx, root := tr.Start(context.Background(), "invoke_agent stirrup",
			oteltrace.WithAttributes(append(runSpanAttributes(), correlationAttributes()...)...))
		_, chat := tr.Start(ctx, "chat model", oteltrace.WithAttributes(append(
			[]attribute.KeyValue{attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.agent.name", "stirrup")},
			correlationAttributes()...)...))
		chat.End()
		root.End()
	})
	want := map[attribute.Key]string{
		"langfuse.trace.metadata.control_plane_url":    "https://cp.example.com/runs/1",
		"langfuse.trace.metadata.task_id":              "task-7",
		"langfuse.trace.metadata.attempt_id":           "attempt-2",
		"langfuse.trace.metadata.run_mode":             "execution",
		"langfuse.trace.metadata.agent_profile":        "triage-v3",
		"langfuse.trace.metadata.execution_mode":       "autonomous",
		"langfuse.observation.metadata.agent_profile":  "triage-v3",
		"langfuse.observation.metadata.execution_mode": "autonomous",
		"langfuse.release":                             "v1.2.3",
	}
	for _, name := range []string{"invoke_agent stirrup", "chat model"} {
		attrs := spans[name]
		for key, value := range want {
			if got := attrs[key].AsString(); got != value {
				t.Errorf("%s: %s = %q, want %q", name, key, got, value)
			}
		}
		if got := attrs["langfuse.trace.tags"].AsStringSlice(); !slices.Equal(got, []string{"stirrup", "execution"}) {
			t.Errorf("%s: langfuse.trace.tags = %v", name, got)
		}
		if got := attrs["stirrup.task.id"].AsString(); got != "task-7" {
			t.Errorf("%s: the original attributes must reach the exporter unchanged, stirrup.task.id = %q", name, got)
		}
		if _, ok := attrs["langfuse.observation.type"]; ok {
			t.Errorf("%s: GenAI spans keep Langfuse's own typing", name)
		}
	}
}

func TestLangfuse_TraceNameOnTheRunSpanOnly(t *testing.T) {
	spans := exportThroughLangfuse(t, langfuseTestResource, func(tr oteltrace.Tracer) {
		// An upstream parent stands in for a control plane's span.
		upstream, parent := tr.Start(context.Background(), "upstream task")
		ctx, root := tr.Start(upstream, "invoke_agent stirrup", oteltrace.WithAttributes(runSpanAttributes()...))
		_, sub := tr.Start(ctx, "invoke_agent stirrup.subagent", oteltrace.WithAttributes(
			attribute.String("gen_ai.operation.name", "invoke_agent"),
			attribute.String("gen_ai.agent.name", "stirrup.subagent"),
		))
		sub.End()
		_, chat := tr.Start(ctx, "chat model", oteltrace.WithAttributes(
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.agent.name", "stirrup"),
		))
		chat.End()
		root.End()
		parent.End()
	})
	if got := spans["invoke_agent stirrup"]["langfuse.trace.name"].AsString(); got != "stirrup execution" {
		t.Errorf("run span trace name = %q, want %q", got, "stirrup execution")
	}
	for _, name := range []string{"upstream task", "invoke_agent stirrup.subagent", "chat model"} {
		if v, ok := spans[name]["langfuse.trace.name"]; ok {
			t.Errorf("%s must not name the trace, got %q", name, v.AsString())
		}
	}
}

func TestLangfuse_ModeFallsBackToTheRunSpan(t *testing.T) {
	spans := exportThroughLangfuse(t, nil, func(tr oteltrace.Tracer) {
		ctx, root := tr.Start(context.Background(), "invoke_agent stirrup", oteltrace.WithAttributes(runSpanAttributes()...))
		_, guard := tr.Start(ctx, "guard.pre_tool")
		guard.End()
		root.End()
	})
	root := spans["invoke_agent stirrup"]
	if got := root["langfuse.trace.name"].AsString(); got != "stirrup execution" {
		t.Errorf("trace name = %q", got)
	}
	if got := root["langfuse.trace.metadata.run_mode"].AsString(); got != "execution" {
		t.Errorf("run_mode = %q", got)
	}
	if _, ok := root["langfuse.release"]; ok {
		t.Error("no service.version resource attribute must set no release")
	}
	guard := spans["guard.pre_tool"]
	if got := guard["langfuse.trace.tags"].AsStringSlice(); !slices.Equal(got, []string{"stirrup"}) {
		t.Errorf("a span without a run mode is tagged %v, want [stirrup]", got)
	}
	for _, key := range []attribute.Key{"langfuse.trace.metadata.run_mode", "langfuse.trace.metadata.task_id", "langfuse.trace.name"} {
		if _, ok := guard[key]; ok {
			t.Errorf("guard span must not carry %s without a source value", key)
		}
	}
}

func TestLangfuse_ObservationTypes(t *testing.T) {
	names := []string{
		"guard.pre_tool", "verifier.verify", "context.prepare", "hooks.pre_run",
		"git.setup", "workspace.export", "verifier.other", "contextual", "invoke_agent stirrup",
	}
	spans := exportThroughLangfuse(t, langfuseTestResource, func(tr oteltrace.Tracer) {
		for _, name := range names {
			_, s := tr.Start(context.Background(), name)
			s.End()
		}
	})
	want := map[string]string{
		"guard.pre_tool":   "guardrail",
		"verifier.verify":  "evaluator",
		"context.prepare":  "chain",
		"hooks.pre_run":    "chain",
		"git.setup":        "chain",
		"workspace.export": "chain",
	}
	for _, name := range names {
		got, ok := spans[name]["langfuse.observation.type"]
		if want[name] == "" {
			if ok {
				t.Errorf("%s must keep Langfuse's own typing, got %q", name, got.AsString())
			}
			continue
		}
		if got.AsString() != want[name] {
			t.Errorf("%s observation type = %q, want %q", name, got.AsString(), want[name])
		}
	}
}

func TestLangfuse_PresetAttributesWin(t *testing.T) {
	spans := exportThroughLangfuse(t, langfuseTestResource, func(tr oteltrace.Tracer) {
		attrs := append(runSpanAttributes(),
			attribute.String("stirrup.task.id", "task-7"),
			attribute.String("stirrup.label.task_id", "from-label"),
			attribute.String("stirrup.label.team", "infra"),
			attribute.String("langfuse.trace.name", "operator name"),
			attribute.StringSlice("langfuse.trace.tags", []string{"operator"}),
			attribute.String("langfuse.trace.metadata.team", "operator team"),
		)
		_, root := tr.Start(context.Background(), "invoke_agent stirrup", oteltrace.WithAttributes(attrs...))
		_, guard := tr.Start(context.Background(), "guard.pre_tool", oteltrace.WithAttributes(
			attribute.String("langfuse.observation.type", "span")))
		guard.End()
		root.End()
	})
	root := spans["invoke_agent stirrup"]
	for key, want := range map[attribute.Key]string{
		"langfuse.trace.name":                   "operator name",
		"langfuse.trace.metadata.team":          "operator team",
		"langfuse.observation.metadata.team":    "infra",
		"langfuse.trace.metadata.task_id":       "task-7",
		"langfuse.observation.metadata.task_id": "from-label",
	} {
		if got := root[key].AsString(); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if got := root["langfuse.trace.tags"].AsStringSlice(); !slices.Equal(got, []string{"operator"}) {
		t.Errorf("tags = %v, want the operator's", got)
	}
	if got := spans["guard.pre_tool"]["langfuse.observation.type"].AsString(); got != "span" {
		t.Errorf("observation type = %q, want the operator's", got)
	}
}

// TestLangfuse_DecorationCopiesTheAttributeSlice pins that decoration never
// writes into spare capacity of a span's attribute slice, which the SDK
// shares with every other span processor and exporter.
func TestLangfuse_DecorationCopiesTheAttributeSlice(t *testing.T) {
	sentinel := attribute.String("sentinel", "untouched")
	backing := make([]attribute.KeyValue, 1, 4)
	backing[0] = attribute.String("a", "b")
	spare := backing[:cap(backing)]
	for i := 1; i < len(spare); i++ {
		spare[i] = sentinel
	}
	span := tracetest.SpanStub{Name: "git.setup", Attributes: backing}.Snapshot()

	decorated := withLangfuse(span).Attributes()

	if len(decorated) < 2 || decorated[0] != backing[0] {
		t.Fatalf("decorated attributes = %v, want the original followed by the added ones", decorated)
	}
	if &decorated[0] == &backing[0] {
		t.Error("the decorated slice must not share the span's backing array")
	}
	for i, kv := range spare[1:] {
		if kv != sentinel {
			t.Errorf("spare slot %d of the span's attributes = %v, want it untouched", i+1, kv)
		}
	}
}

// spanRecorder keeps each exported span's attributes by span name before
// passing the batch on.
type spanRecorder struct {
	sdktrace.SpanExporter
	mu    sync.Mutex
	spans map[string]map[attribute.Key]attribute.Value
}

func (r *spanRecorder) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	r.mu.Lock()
	for _, s := range spans {
		attrs := map[attribute.Key]attribute.Value{}
		for _, kv := range s.Attributes() {
			attrs[kv.Key] = kv.Value
		}
		r.spans[s.Name()] = attrs
	}
	r.mu.Unlock()
	return r.SpanExporter.ExportSpans(ctx, spans)
}

// TestLangfuse_MapsTheTraceEmittersAttributes pins the profile against the
// attribute names the trace emitter, its run-scope processor and the
// resource builder actually stamp.
func TestLangfuse_MapsTheTraceEmittersAttributes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p, err := Resolve(context.Background(), "langfuse", nil, Deps{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	rec := &spanRecorder{spans: map[string]map[attribute.Key]attribute.Value{}}
	emitter, err := trace.NewOTelTraceEmitter(context.Background(), trace.OTelTraceEmitterOptions{
		Exporter: otlp.Settings{Endpoint: srv.URL, Protocol: otlp.ProtocolHTTPProtobuf},
		Resource: observability.ResourceOptions{RunMode: "execution"},
		WrapSpanExporter: func(inner sdktrace.SpanExporter) sdktrace.SpanExporter {
			rec.SpanExporter = inner
			return p.WrapSpanExporter(rec)
		},
	})
	if err != nil {
		t.Fatalf("NewOTelTraceEmitter: %v", err)
	}
	emitter.Start("run-1", &types.RunConfig{
		RunID: "run-1",
		Mode:  "execution",
		Correlation: &types.CorrelationConfig{
			TaskID:    "task-7",
			AttemptID: "attempt-2",
			URL:       "https://cp.example.com/runs/1",
			Labels:    map[string]string{"agent_profile": "triage-v3"},
		},
	})
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := emitter.Finish(flushCtx, "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := emitter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	root, ok := rec.spans["invoke_agent stirrup"]
	if !ok {
		t.Fatalf("no run span exported; got %v", slices.Collect(maps.Keys(rec.spans)))
	}
	for key, want := range map[attribute.Key]string{
		"langfuse.trace.name":                         "stirrup execution",
		"langfuse.trace.metadata.control_plane_url":   "https://cp.example.com/runs/1",
		"langfuse.trace.metadata.task_id":             "task-7",
		"langfuse.trace.metadata.attempt_id":          "attempt-2",
		"langfuse.trace.metadata.run_mode":            "execution",
		"langfuse.trace.metadata.agent_profile":       "triage-v3",
		"langfuse.observation.metadata.agent_profile": "triage-v3",
		"langfuse.release":                            version.Version(),
	} {
		if got := root[key].AsString(); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if got := root["langfuse.trace.tags"].AsStringSlice(); !slices.Equal(got, []string{"stirrup", "execution"}) {
		t.Errorf("langfuse.trace.tags = %v", got)
	}
}
