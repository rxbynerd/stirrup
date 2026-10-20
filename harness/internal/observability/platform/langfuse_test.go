package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestLangfuse_Profile_SetsTraceDefaults(t *testing.T) {
	must := require.New(t)
	p, err := Resolve(context.Background(), "langfuse", map[string]string{"host": "https://langfuse.example.com/"}, Deps{})
	must.NoError(err, "Resolve")
	must.Equal(otlp.ProtocolHTTPProtobuf, p.RequiredProtocol)

	traces := p.Signals[otlp.SignalTraces]
	must.Equal("https://langfuse.example.com/api/public/otel", traces.Endpoint)
	must.Equal(map[string]string{"x-langfuse-ingestion-version": "4"}, traces.Headers)
	must.NotContains(p.Signals, otlp.SignalMetrics, "the profile must leave metric export alone")
	must.NotContains(p.Signals, otlp.SignalLogs, "the profile must leave log export alone")
	must.Nil(p.WrapTransport, "langfuse does not sign requests")
	must.Nil(p.ResourceAttrs, "langfuse adds no resource attributes")
	must.NotNil(p.WrapSpanExporter, "langfuse must decorate spans")

	t.Run("keeps a host path prefix", func(t *testing.T) {
		must := require.New(t)
		prefixed, err := Resolve(context.Background(), "langfuse", map[string]string{"host": "https://proxy.example.com/langfuse"}, Deps{})
		must.NoError(err, "Resolve with a path prefix")
		must.Equal("https://proxy.example.com/langfuse/api/public/otel", prefixed.Signals[otlp.SignalTraces].Endpoint,
			"a host path prefix must be kept")
	})

	t.Run("without a host sets the header but no endpoint", func(t *testing.T) {
		must := require.New(t)
		noHost, err := Resolve(context.Background(), "langfuse", nil, Deps{})
		must.NoError(err, "Resolve without options")
		got := noHost.Signals[otlp.SignalTraces]
		must.Empty(got.Endpoint, "without host the profile must set no endpoint")
		must.Equal("4", got.Headers[langfuseIngestionVersionHeader], "without host the profile must set the header")
	})
}

func TestLangfuse_RejectsBadHost(t *testing.T) {
	cases := map[string]string{
		"no scheme":  "langfuse.example.com",
		"ftp scheme": "ftp://langfuse.example.com",
		"no host":    "https://",
		"userinfo":   "https://pk:sk-secret@langfuse.example.com",
		"query":      "https://langfuse.example.com?x=1",
		"fragment":   "https://langfuse.example.com#x",
		"unparsable": "http://[::1",
		"otlp path":  "https://langfuse.example.com/api/public/otel",
		"trace path": "https://langfuse.example.com/api/public/otel/v1/traces",
	}
	for name, host := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			_, err := Resolve(context.Background(), "langfuse", map[string]string{"host": host}, Deps{})
			must.Error(err)
			must.NotContains(err.Error(), "sk-secret", "the error must not echo the host")
		})
	}
}

// TestLangfuse_ApplyRoutesTracesToTheHost pins that the trace exporter
// dials the profile's endpoint over OTLP/HTTP with the ingestion header
// beside the operator's Authorization, that metrics stay off, and that
// gRPC is rejected.
func TestLangfuse_ApplyRoutesTracesToTheHost(t *testing.T) {
	must := require.New(t)
	p, err := Resolve(context.Background(), "langfuse", map[string]string{"host": "https://langfuse.example.com"}, Deps{})
	must.NoError(err, "Resolve")

	got, err := Apply(Input{
		TraceEmitter: types.TraceEmitterConfig{Type: "otel"},
		Headers:      map[string]string{"Authorization": "Basic cGs6c2s="},
		Getenv:       noEnv,
	}, p)
	must.NoError(err, "Apply")
	must.Equal(otlp.ProtocolHTTPProtobuf, got.Traces.Protocol)
	must.Equal("https://langfuse.example.com/api/public/otel", got.Traces.Endpoint)
	must.Equal(map[string]string{
		"Authorization":                "Basic cGs6c2s=",
		"x-langfuse-ingestion-version": "4",
	}, got.Traces.Headers, "trace headers")
	must.Empty(got.Metrics.Endpoint, "metric export must stay disabled")

	_, err = Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel", Protocol: otlp.ProtocolGRPC}, Getenv: noEnv}, p)
	must.ErrorContains(err, `platform "langfuse" requires protocol http/protobuf`)
}

func TestLangfuse_ApplyWithoutHost_RequiresTraceEndpoint(t *testing.T) {
	must := require.New(t)
	p, err := Resolve(context.Background(), "langfuse", nil, Deps{})
	must.NoError(err, "Resolve")

	_, err = Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel"}, Getenv: noEnv}, p)
	must.ErrorContains(err, "has no trace endpoint",
		"no host and no endpoint must fail instead of exporting to the default gRPC address")

	got, err := Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel", Endpoint: "https://gateway.example.com/api/public/otel"}, Getenv: noEnv}, p)
	must.NoError(err, "Apply with an explicit endpoint")
	must.Equal("https://gateway.example.com/api/public/otel", got.Traces.Endpoint)
	must.Equal("4", got.Traces.Headers[langfuseIngestionVersionHeader])
}

// exportThroughLangfuse records the spans built by fn through the langfuse
// span decorator, under a resource carrying res, and returns what reached
// the downstream exporter.
func exportThroughLangfuse(t *testing.T, res []attribute.KeyValue, fn func(tracer oteltrace.Tracer)) map[string]map[attribute.Key]attribute.Value {
	t.Helper()
	check := assert.New(t)
	must := require.New(t)
	p, err := Resolve(context.Background(), "langfuse", nil, Deps{})
	must.NoError(err, "Resolve")
	sink := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(p.WrapSpanExporter(sink)),
		sdktrace.WithResource(sdkresource.NewSchemaless(res...)),
	)
	fn(tp.Tracer("test"))
	exported := sink.GetSpans()
	must.NoError(tp.Shutdown(context.Background()), "shut down the tracer provider")

	out := map[string]map[attribute.Key]attribute.Value{}
	for _, s := range exported {
		attrs := map[attribute.Key]attribute.Value{}
		for _, kv := range s.Attributes {
			check.NotContains(attrs, kv.Key, "span %q carries %s twice", s.Name, kv.Key)
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
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			attrs := spans[name]
			for key, value := range want {
				must.Equal(value, attrs[key].AsString(), "%s", key)
			}
			must.Equal([]string{"stirrup", "execution"}, attrs["langfuse.trace.tags"].AsStringSlice(), "langfuse.trace.tags")
			must.Equal("task-7", attrs["stirrup.task.id"].AsString(), "the original attributes must reach the exporter unchanged")
			must.NotContains(attrs, attribute.Key("langfuse.observation.type"), "GenAI spans keep Langfuse's own typing")
		})
	}
}

func TestLangfuse_TraceNameOnTheRunSpanOnly(t *testing.T) {
	must := require.New(t)
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
	must.Equal("stirrup execution", spans["invoke_agent stirrup"]["langfuse.trace.name"].AsString(), "run span trace name")
	for _, name := range []string{"upstream task", "invoke_agent stirrup.subagent", "chat model"} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			must.NotContains(spans[name], attribute.Key("langfuse.trace.name"), "must not name the trace")
		})
	}
}

func TestLangfuse_ModeFallsBackToTheRunSpan(t *testing.T) {
	must := require.New(t)
	spans := exportThroughLangfuse(t, nil, func(tr oteltrace.Tracer) {
		ctx, root := tr.Start(context.Background(), "invoke_agent stirrup", oteltrace.WithAttributes(runSpanAttributes()...))
		_, guard := tr.Start(ctx, "guard.pre_tool")
		guard.End()
		root.End()
	})
	root := spans["invoke_agent stirrup"]
	must.Equal("stirrup execution", root["langfuse.trace.name"].AsString(), "trace name")
	must.Equal("execution", root["langfuse.trace.metadata.run_mode"].AsString(), "run_mode")
	must.NotContains(root, attribute.Key("langfuse.release"), "no service.version resource attribute must set no release")

	guard := spans["guard.pre_tool"]
	must.Equal([]string{"stirrup"}, guard["langfuse.trace.tags"].AsStringSlice(),
		"a span without a run mode is tagged [stirrup]")
	for _, key := range []attribute.Key{"langfuse.trace.metadata.run_mode", "langfuse.trace.metadata.task_id", "langfuse.trace.name"} {
		must.NotContains(guard, key, "guard span must not carry %s without a source value", key)
	}
}

// TestLangfuse_ObservationTypes_MapsHarnessSpans uses the span names the harness emits.
func TestLangfuse_ObservationTypes_MapsHarnessSpans(t *testing.T) {
	names := []string{
		"guard.pre_turn", "guard.pre_tool", "guard.post_turn", "verifier.verify", "context.prepare",
		"hooks.pre_run", "hooks.post_run", "git.setup", "git.checkpoint", "git.finalise", "workspace.handoff",
		"permission.check", "provider.stream", "tool_choice.escalation", "invoke_agent stirrup",
	}
	spans := exportThroughLangfuse(t, langfuseTestResource, func(tr oteltrace.Tracer) {
		for _, name := range names {
			_, s := tr.Start(context.Background(), name)
			s.End()
		}
	})
	want := map[string]string{
		"guard.pre_turn":    "guardrail",
		"guard.pre_tool":    "guardrail",
		"guard.post_turn":   "guardrail",
		"verifier.verify":   "evaluator",
		"context.prepare":   "chain",
		"hooks.pre_run":     "chain",
		"hooks.post_run":    "chain",
		"git.setup":         "chain",
		"git.checkpoint":    "chain",
		"git.finalise":      "chain",
		"workspace.handoff": "chain",
	}
	observationType := attribute.Key("langfuse.observation.type")
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			wantType, mapped := want[name]
			if !mapped {
				must.NotContains(spans[name], observationType, "must keep Langfuse's own typing")
				return
			}
			must.Equal(wantType, spans[name][observationType].AsString(), "observation type")
		})
	}
}

func TestLangfuse_PresetAttributes_TakePrecedence(t *testing.T) {
	must := require.New(t)
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
		must.Equal(want, root[key].AsString(), "%s", key)
	}
	must.Equal([]string{"operator"}, root["langfuse.trace.tags"].AsStringSlice(), "tags must be the operator's")
	must.Equal("span", spans["guard.pre_tool"]["langfuse.observation.type"].AsString(), "observation type must be the operator's")
}

func TestLangfuse_SpanWithNothingToAdd_IsReturnedAsIs(t *testing.T) {
	must := require.New(t)
	span := tracetest.SpanStub{Name: "provider.stream", Attributes: []attribute.KeyValue{
		attribute.StringSlice("langfuse.trace.tags", []string{"operator"}),
	}}.Snapshot()

	decorated := withLangfuse(span)
	_, wrapped := decorated.(langfuseSpan)
	must.False(wrapped, "a span with nothing to add must be returned as is, got attributes %v", decorated.Attributes())
}

// TestLangfuse_Decoration_CopiesAttributeSlice pins that decoration never
// writes into spare capacity of a span's attribute slice, which the SDK
// shares with every other span processor and exporter.
func TestLangfuse_Decoration_CopiesAttributeSlice(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	sentinel := attribute.String("sentinel", "untouched")
	backing := make([]attribute.KeyValue, 1, 4)
	backing[0] = attribute.String("a", "b")
	spare := backing[:cap(backing)]
	for i := 1; i < len(spare); i++ {
		spare[i] = sentinel
	}
	span := tracetest.SpanStub{Name: "git.setup", Attributes: backing}.Snapshot()

	decorated := withLangfuse(span).Attributes()

	must.GreaterOrEqual(len(decorated), 2, "decorated attributes = %v, want the original followed by the added ones", decorated)
	must.Equal(backing[0], decorated[0], "decorated attributes must start with the original")
	must.NotSame(&backing[0], &decorated[0], "the decorated slice must not share the span's backing array")
	for i, kv := range spare[1:] {
		check.Equal(sentinel, kv, "spare slot %d of the span's attributes must stay untouched", i+1)
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

// TestLangfuse_MapsTraceEmitterAttributes pins the profile against the
// attribute names the trace emitter, its run-scope processor and the
// resource builder actually stamp.
func TestLangfuse_MapsTraceEmitterAttributes(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p, err := Resolve(context.Background(), "langfuse", nil, Deps{})
	must.NoError(err, "Resolve")

	rec := &spanRecorder{spans: map[string]map[attribute.Key]attribute.Value{}}
	emitter, err := trace.NewOTelTraceEmitter(context.Background(), trace.OTelTraceEmitterOptions{
		Exporter: otlp.Settings{Endpoint: srv.URL, Protocol: otlp.ProtocolHTTPProtobuf},
		Resource: observability.ResourceOptions{RunMode: "execution"},
		WrapSpanExporter: func(inner sdktrace.SpanExporter) sdktrace.SpanExporter {
			rec.SpanExporter = inner
			return p.WrapSpanExporter(rec)
		},
	})
	must.NoError(err, "NewOTelTraceEmitter")

	config := &types.RunConfig{
		RunID: "run-1",
		Mode:  "execution",
		Correlation: &types.CorrelationConfig{
			TaskID:    "task-7",
			AttemptID: "attempt-2",
			URL:       "https://cp.example.com/runs/1",
			Labels:    map[string]string{"agent_profile": "triage-v3"},
		},
	}
	emitter.Start("run-1", config)
	rootCtx := emitter.RootContext()
	chat := emitter.StartChatSpan(rootCtx, trace.ChatSpanStart{Turn: 1, ProviderType: "anthropic", Model: "model-a"})
	must.NotNil(chat, "StartChatSpan")
	chat.End(trace.ChatSpanEnd{})
	sub := emitter.StartSubAgentSpan(rootCtx, trace.SubAgentSpanStart{RunID: "run-1-sub", ParentRunID: "run-1", Config: config})
	must.NotNil(sub, "StartSubAgentSpan")
	sub.End(trace.AgentSpanEnd{Outcome: "success"})
	_, handoff := emitter.Tracer().Start(rootCtx, "workspace.handoff")
	handoff.End()
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = emitter.Finish(flushCtx, "success")
	must.NoError(err, "Finish")
	must.NoError(emitter.Close(), "Close")

	rec.mu.Lock()
	defer rec.mu.Unlock()
	must.Contains(rec.spans, "invoke_agent stirrup", "no run span exported")
	root := rec.spans["invoke_agent stirrup"]
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
		must.Equal(want, root[key].AsString(), "%s", key)
	}
	must.Equal([]string{"stirrup", "execution"}, root["langfuse.trace.tags"].AsStringSlice(), "langfuse.trace.tags")

	for _, name := range []string{"chat model-a", "invoke_agent stirrup.subagent", "workspace.handoff"} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			must.Contains(rec.spans, name, "no span exported under this name")
			attrs := rec.spans[name]
			must.NotContains(attrs, attribute.Key("langfuse.trace.name"), "must not name the trace")
			must.Equal("task-7", attrs["langfuse.trace.metadata.task_id"].AsString(), "langfuse.trace.metadata.task_id")
		})
	}
	must.Equal("chain", rec.spans["workspace.handoff"]["langfuse.observation.type"].AsString(), "workspace.handoff observation type")
	must.NotContains(rec.spans["chat model-a"], attribute.Key("langfuse.observation.type"), "chat spans keep Langfuse's own typing")
}
