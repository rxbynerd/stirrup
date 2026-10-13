package platform

import (
	"context"
	"slices"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/types"
)

func TestRegistryMatchesTypesClosedSet(t *testing.T) {
	got := sortedKeys(registry)
	want := types.ValidTraceEmitterPlatformValues()
	if !slices.Equal(got, want) {
		t.Errorf("registry platforms = %v, types closed set = %v; they must name the same platforms", got, want)
	}
}

func TestPhoenix_Profile(t *testing.T) {
	p, err := Resolve(context.Background(), "phoenix", map[string]string{"project": "agents"}, Deps{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.RequiredProtocol != "" || p.WrapTransport != nil || len(p.Signals) != 0 {
		t.Errorf("phoenix needs no protocol, transport or endpoint defaults, got %+v", p)
	}
	want := []attribute.KeyValue{attribute.String("openinference.project.name", "agents")}
	if !slices.Equal(p.ResourceAttrs, want) {
		t.Errorf("resource attributes = %v, want %v", p.ResourceAttrs, want)
	}
	if p.WrapSpanExporter == nil {
		t.Fatal("phoenix must decorate spans")
	}

	noProject, err := Resolve(context.Background(), "phoenix", nil, Deps{})
	if err != nil {
		t.Fatalf("Resolve without options: %v", err)
	}
	if noProject.ResourceAttrs != nil {
		t.Errorf("no project must add no resource attribute, got %v", noProject.ResourceAttrs)
	}
}

// exportThroughPhoenix records the spans built by fn through the phoenix
// span decorator and returns what reached the downstream exporter.
func exportThroughPhoenix(t *testing.T, fn func(tracer oteltrace.Tracer)) map[string]map[attribute.Key]attribute.Value {
	t.Helper()
	p, err := Resolve(context.Background(), "phoenix", nil, Deps{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	sink := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(p.WrapSpanExporter(sink)))
	fn(tp.Tracer("test"))
	// The in-memory exporter discards its spans on shutdown, so read first.
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
		if got := spans[name]["openinference.span.kind"].AsString(); got != kind {
			t.Errorf("%s kind = %q, want %q", name, got, kind)
		}
	}
}

func TestPhoenix_RootInputOutput(t *testing.T) {
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
	if got := root["input.value"].AsString(); got != "List the files" {
		t.Errorf("input.value = %q", got)
	}
	if got := root["output.value"].AsString(); got != "Two files.\nDone." {
		t.Errorf("output.value = %q", got)
	}
	if got := root["metadata"].AsString(); got != `{"run_id":"run-1","run_mode":"planning"}` {
		t.Errorf("metadata = %q", got)
	}
	if got := root["gen_ai.input.messages"].AsString(); got != input {
		t.Error("the original attributes must reach the exporter unchanged")
	}
	sub := spans["invoke_agent stirrup.subagent"]
	if _, ok := sub["input.value"]; ok {
		t.Error("only the root span gets input.value")
	}
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
		if _, ok := root[key]; ok {
			t.Errorf("%s must be absent without content or run identity", key)
		}
	}
}

// TestPhoenix_DecorationCopiesTheAttributeSlice pins that decoration never
// writes into spare capacity of a span's attribute slice, which the SDK
// shares with every other span processor and exporter.
func TestPhoenix_DecorationCopiesTheAttributeSlice(t *testing.T) {
	sentinel := attribute.String("sentinel", "untouched")
	backing := make([]attribute.KeyValue, 1, 4)
	backing[0] = attribute.String("a", "b")
	spare := backing[:cap(backing)]
	for i := 1; i < len(spare); i++ {
		spare[i] = sentinel
	}
	span := tracetest.SpanStub{Name: "git.setup", Attributes: backing}.Snapshot()

	decorated := withOpenInference(span).Attributes()

	if len(decorated) != 2 || decorated[1].Key != openInferenceSpanKindKey {
		t.Fatalf("decorated attributes = %v, want the original plus the span kind", decorated)
	}
	if &decorated[0] == &backing[0] {
		t.Error("the decorated slice must not share the span's backing array")
	}
	for i, kv := range spare[1:] {
		if kv != sentinel {
			t.Errorf("spare slot %d of the span's attributes = %v, want it untouched", i+1, kv)
		}
	}
	if got := span.Attributes(); len(got) != 1 || got[0] != backing[0] {
		t.Errorf("the span's own attributes = %v, want them unchanged", got)
	}
}
