package trace

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/types"
)

func hookSpans(spans tracetest.SpanStubs) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for _, s := range spans {
		if s.Name == "hook" {
			out = append(out, s)
		}
	}
	return out
}

func spanAttrs(s tracetest.SpanStub) map[string]any {
	attrs := map[string]any{}
	for _, kv := range s.Attributes {
		attrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	return attrs
}

func TestOTelTraceEmitter_RecordHookExecution_EmitsChildSpanAndAggregates(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-otel-hooks", &types.RunConfig{RunID: "run-otel-hooks"})

	phaseCtx, phaseSpan := emitter.Tracer().Start(emitter.RootContext(), "hooks.preRun")
	emitter.RecordHookExecution(phaseCtx, types.HookExecution{
		Phase: "preRun", Index: 0, Name: "clone", Command: "git clone repo", DurationMs: 1200,
		OutputTail: "cloned", Truncated: true,
	})
	emitter.RecordHookExecution(phaseCtx, types.HookExecution{
		Phase: "preRun", Index: 1, Command: "echo sk-ant-api03-leak", ExitCode: 3,
		ContinuedOnError: true, Error: "exit status 3: sk-ant-api03-leak",
	})
	phaseSpan.End()

	tr, err := emitter.Finish(context.Background(), "success")
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.HookResults) != 2 || tr.HookResults[0].Name != "clone" {
		t.Fatalf("HookResults = %+v", tr.HookResults)
	}
	if strings.Contains(tr.HookResults[1].Command+tr.HookResults[1].Error, "sk-ant-api03-leak") {
		t.Errorf("HookResults carries an unscrubbed secret: %+v", tr.HookResults[1])
	}

	spans := hookSpans(exporter.GetSpans())
	if len(spans) != 2 {
		t.Fatalf("hook spans = %d, want 2", len(spans))
	}
	wantParent := phaseSpan.SpanContext().SpanID()
	for _, s := range spans {
		if s.Parent.SpanID() != wantParent {
			t.Errorf("hook span parent = %s, want the hooks.preRun span %s", s.Parent.SpanID(), wantParent)
		}
		attrs := spanAttrs(s)
		if _, ok := attrs[hookOutputTailKey]; ok {
			t.Errorf("output tail emitted with content capture off: %v", attrs)
		}
		if strings.Contains(attrs["hook.command"].(string), "sk-ant-api03-leak") {
			t.Errorf("hook.command carries an unscrubbed secret: %v", attrs["hook.command"])
		}
	}

	clone := spanAttrs(spans[0])
	if clone["hook.name"] != "clone" || clone["hook.phase"] != "preRun" || clone["hook.output_truncated"] != true {
		t.Errorf("clone span attributes = %v", clone)
	}
	if got := spans[0].EndTime.Sub(spans[0].StartTime); got != 1200*time.Millisecond {
		t.Errorf("clone span lasts %s, want the hook duration 1.2s", got)
	}
	if spans[0].Status.Code == codes.Error {
		t.Errorf("successful hook span has error status %q", spans[0].Status.Description)
	}

	failed := spanAttrs(spans[1])
	if _, ok := failed["hook.name"]; ok {
		t.Errorf("unnamed hook carries hook.name: %v", failed)
	}
	if failed["hook.exit_code"] != int64(3) || failed["hook.continued_on_error"] != true {
		t.Errorf("failed span attributes = %v", failed)
	}
	if spans[1].Status.Code != codes.Error || strings.Contains(spans[1].Status.Description, "sk-ant-api03-leak") {
		t.Errorf("failed span status = %+v, want a scrubbed error", spans[1].Status)
	}
}

func TestOTelTraceEmitter_RecordHookExecution_OutputTailRequiresCaptureContent(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()
	emitter.Start("run-otel-hook-tail", &types.RunConfig{RunID: "run-otel-hook-tail"})

	emitter.RecordHookExecution(emitter.RootContext(), types.HookExecution{
		Phase: "postRun", Command: "make test", OutputTail: "FAIL token=sk-ant-api03-leak", ExitCode: 1, Error: "exit status 1",
	})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatal(err)
	}

	spans := hookSpans(exporter.GetSpans())
	if len(spans) != 1 {
		t.Fatalf("hook spans = %d, want 1", len(spans))
	}
	tail, ok := spanAttrs(spans[0])[hookOutputTailKey].(string)
	if !ok || !strings.HasPrefix(tail, "FAIL token=") {
		t.Fatalf("%s = %q, want the captured tail", hookOutputTailKey, tail)
	}
	if strings.Contains(tail, "sk-ant-api03-leak") {
		t.Errorf("captured tail carries an unscrubbed secret: %q", tail)
	}
}

func TestOTelTraceEmitter_RecordHookExecution_ParentsUnderRootWithoutPhaseSpan(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-otel-hook-root", &types.RunConfig{RunID: "run-otel-hook-root"})

	emitter.RecordHookExecution(context.Background(), types.HookExecution{Phase: "postRun", Command: "true"})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatal(err)
	}

	spans := exporter.GetSpans()
	hooks := hookSpans(spans)
	if len(hooks) != 1 {
		t.Fatalf("hook spans = %d, want 1", len(hooks))
	}
	root := findSpanByName(t, spans, "invoke_agent stirrup")
	if hooks[0].Parent.SpanID() != root.SpanContext.SpanID() {
		t.Errorf("hook span parent = %s, want the root span %s", hooks[0].Parent.SpanID(), root.SpanContext.SpanID())
	}
}

func TestGCSTraceEmitter_RecordHookExecution_AccumulatesAndScrubs(t *testing.T) {
	srv := newGCSCaptureServer()
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	emitter, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "my-bucket",
		CredentialSource: &staticBearerSource{token: "test-token"},
		EndpointBaseURL:  httpSrv.URL,
	})
	if err != nil {
		t.Fatalf("NewGCSTraceEmitter: %v", err)
	}
	emitter.Start("run-gcs-hooks", nil)
	emitter.RecordHookExecution(context.Background(), types.HookExecution{Phase: "preRun", Name: "clone", Command: "git clone repo"})
	emitter.RecordHookExecution(context.Background(), types.HookExecution{
		Phase: "postRun", Index: 1, Command: "notify", OutputTail: "sk-ant-api03-leak", Error: "exit status 1",
	})

	tr, err := emitter.Finish(context.Background(), "success")
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if len(tr.HookResults) != 2 || tr.HookResults[0].Name != "clone" {
		t.Fatalf("returned HookResults = %+v", tr.HookResults)
	}

	body := string(srv.last().Body)
	if strings.Contains(body, "sk-ant-api03-leak") {
		t.Errorf("secret reached the uploaded trace:\n%s", body)
	}
	var uploaded types.RunTrace
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &uploaded); err != nil {
		t.Fatalf("unmarshal uploaded body: %v", err)
	}
	if len(uploaded.HookResults) != 2 || uploaded.HookResults[1].Phase != "postRun" {
		t.Errorf("uploaded HookResults = %+v", uploaded.HookResults)
	}
}

func TestOTelTraceEmitter_RecordHookExecution_CapKeepsOutputTailEnd(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	emitter := newOTelTraceEmitter(tp, true, 256, false)
	emitter.Start("run-otel-hook-cap", &types.RunConfig{RunID: "run-otel-hook-cap"})

	tail := strings.Repeat("é", 200) + "FAILED: last line"
	emitter.RecordHookExecution(emitter.RootContext(), types.HookExecution{Phase: "postRun", Command: "make", OutputTail: tail})
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatal(err)
	}

	attrs := spanAttrs(hookSpans(exporter.GetSpans())[0])
	got := attrs[hookOutputTailKey].(string)
	if len(got) > 256 || !strings.HasSuffix(got, "FAILED: last line") || !utf8.ValidString(got) {
		t.Errorf("capped tail = %q (%d bytes), want valid UTF-8 ending in the last line within 256 bytes", got, len(got))
	}
	if attrs[contentTruncatedKey(hookOutputTailKey)] != true {
		t.Errorf("missing %s marker: %v", contentTruncatedKey(hookOutputTailKey), attrs)
	}
}
