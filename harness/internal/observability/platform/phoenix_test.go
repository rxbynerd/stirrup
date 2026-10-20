package platform

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/types"
)

func TestRegistry_MatchesTypesClosedSet(t *testing.T) {
	must := require.New(t)
	got := sortedKeys(registry)
	want := types.ValidTraceEmitterPlatformValues()
	must.Equal(want, got, "registry and types closed set must name the same platforms")
}

func TestPhoenix_Profile_ResolvesProjectOption(t *testing.T) {
	must := require.New(t)
	p, err := Resolve(context.Background(), "phoenix", map[string]string{"project": "agents"}, Deps{})
	must.NoError(err, "Resolve")
	must.Empty(p.RequiredProtocol, "phoenix needs no protocol default")
	must.Nil(p.WrapTransport, "phoenix needs no transport wrapper")
	must.Empty(p.Signals, "phoenix needs no endpoint defaults")
	want := []attribute.KeyValue{attribute.String("openinference.project.name", "agents")}
	must.Equal(want, p.ResourceAttrs)
	must.NotNil(p.WrapSpanExporter, "phoenix must decorate spans")

	noProject, err := Resolve(context.Background(), "phoenix", nil, Deps{})
	must.NoError(err, "Resolve without options")
	must.Nil(noProject.ResourceAttrs, "no project must add no resource attribute")
}

// exportThroughPhoenix records the spans built by fn through the phoenix
// span decorator and returns what reached the downstream exporter.
func exportThroughPhoenix(t *testing.T, fn func(tracer oteltrace.Tracer)) map[string]map[attribute.Key]attribute.Value {
	t.Helper()
	must := require.New(t)
	check := assert.New(t)
	p, err := Resolve(context.Background(), "phoenix", nil, Deps{})
	must.NoError(err, "Resolve")
	sink := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(p.WrapSpanExporter(sink)))
	fn(tp.Tracer("test"))
	// The in-memory exporter discards its spans on shutdown, so read first.
	exported := sink.GetSpans()
	must.NoError(tp.Shutdown(context.Background()), "shutdown")

	out := map[string]map[attribute.Key]attribute.Value{}
	for _, s := range exported {
		attrs := map[attribute.Key]attribute.Value{}
		for _, kv := range s.Attributes {
			_, dup := attrs[kv.Key]
			check.False(dup, "span %q carries %s twice", s.Name, kv.Key)
			attrs[kv.Key] = kv.Value
		}
		out[s.Name] = attrs
	}
	return out
}

func TestPhoenix_SpanKinds(t *testing.T) {
	spans := exportThroughPhoenix(t, func(tr oteltrace.Tracer) {
		ctx, root := tr.Start(context.Background(), "invoke_agent stirrup",
			oteltrace.WithAttributes(attribute.String("gen_ai.operation.name", "invoke_agent")))
		for name, op := range map[string]string{
			"chat model":                    "chat",
			"execute_tool list_directory":   "execute_tool",
			"invoke_agent stirrup.subagent": "invoke_agent",
		} {
			_, s := tr.Start(ctx, name, oteltrace.WithAttributes(attribute.String("gen_ai.operation.name", op)))
			s.End()
		}
		for _, name := range []string{"guard.pre_tool", "verifier.verify", "git.setup"} {
			_, s := tr.Start(ctx, name)
			s.End()
		}
		_, preset := tr.Start(ctx, "preset", oteltrace.WithAttributes(attribute.String("openinference.span.kind", "RETRIEVER")))
		preset.End()
		root.End()
	})
	want := map[string]string{
		"invoke_agent stirrup":          "AGENT",
		"chat model":                    "LLM",
		"execute_tool list_directory":   "TOOL",
		"invoke_agent stirrup.subagent": "AGENT",
		"guard.pre_tool":                "GUARDRAIL",
		"verifier.verify":               "EVALUATOR",
		"git.setup":                     "CHAIN",
		"preset":                        "RETRIEVER",
	}
	for name, kind := range want {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			got := spans[name]["openinference.span.kind"].AsString()
			must.Equal(kind, got)
		})
	}
}

func TestPhoenix_RootInputOutput(t *testing.T) {
	must := require.New(t)
	input := `[{"role":"user","parts":[{"type":"text","content":"List the files"}]},{"role":"assistant","parts":[{"type":"text","content":"ignored"}]}]`
	output := `[{"role":"assistant","parts":[{"type":"tool_call","id":"1","name":"x"}]},{"role":"assistant","parts":[{"type":"text","content":"Two files."},{"type":"text","content":"Done."}]}]`
	spans := exportThroughPhoenix(t, func(tr oteltrace.Tracer) {
		ctx, root := tr.Start(context.Background(), "invoke_agent stirrup", oteltrace.WithAttributes(
			attribute.String("gen_ai.operation.name", "invoke_agent"),
			attribute.String("run.id", "run-1"),
			attribute.String("run.mode", "planning"),
		))
		_, sub := tr.Start(ctx, "invoke_agent stirrup.subagent", oteltrace.WithAttributes(
			attribute.String("gen_ai.operation.name", "invoke_agent"),
			attribute.String("gen_ai.input.messages", input),
		))
		sub.End()
		// Content arrives after Start, as the emitter sets it at Finish.
		root.SetAttributes(
			attribute.String("gen_ai.input.messages", input),
			attribute.String("gen_ai.output.messages", output),
		)
		root.End()
	})
	root := spans["invoke_agent stirrup"]
	must.Equal("List the files", root["input.value"].AsString(), "root input.value")
	must.Equal("Two files.\nDone.", root["output.value"].AsString(), "root output.value")
	must.JSONEq(`{"run_id":"run-1","run_mode":"planning"}`, root["metadata"].AsString(), "root metadata")
	must.Equal(input, root["gen_ai.input.messages"].AsString(), "the original attributes must reach the exporter unchanged")

	sub := spans["invoke_agent stirrup.subagent"]
	must.NotContains(sub, attribute.Key("input.value"), "only the root span gets input.value")
}

func TestPhoenix_RootWithoutCapturedContent(t *testing.T) {
	spans := exportThroughPhoenix(t, func(tr oteltrace.Tracer) {
		_, root := tr.Start(context.Background(), "invoke_agent stirrup", oteltrace.WithAttributes(
			attribute.String("gen_ai.operation.name", "invoke_agent"),
			attribute.String("gen_ai.output.messages", "not json"),
		))
		root.End()
	})
	root := spans["invoke_agent stirrup"]
	for _, key := range []attribute.Key{"input.value", "output.value", "metadata"} {
		t.Run(string(key), func(t *testing.T) {
			must := require.New(t)
			must.NotContains(root, key, "must be absent without content or run identity")
		})
	}
}

// TestPhoenix_DecorationCopiesTheAttributeSlice pins that decoration never
// writes into spare capacity of a span's attribute slice, which the SDK
// shares with every other span processor and exporter.
func TestPhoenix_DecorationCopiesTheAttributeSlice(t *testing.T) {
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

	decorated := withOpenInference(span).Attributes()

	must.Len(decorated, 2, "decorated attributes must be the original plus the span kind")
	must.Equal(attribute.Key(openInferenceSpanKindKey), decorated[1].Key)
	must.NotSame(&backing[0], &decorated[0], "the decorated slice must not share the span's backing array")
	for i, kv := range spare[1:] {
		check.Equal(sentinel, kv, "spare slot %d of the span's attributes must stay untouched", i+1)
	}

	got := span.Attributes()
	must.Len(got, 1, "the span's own attributes must keep their length")
	must.Equal(backing[0], got[0], "the span's own attributes must stay unchanged")
}
