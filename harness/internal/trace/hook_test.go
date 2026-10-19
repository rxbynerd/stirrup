package trace

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err)
	require.Len(t, tr.HookResults, 2)
	require.Equal(t, "clone", tr.HookResults[0].Name)
	assert.NotContains(t, tr.HookResults[1].Command+tr.HookResults[1].Error, "sk-ant-api03-leak",
		"HookResults carries an unscrubbed secret: %+v", tr.HookResults[1])

	spans := hookSpans(exporter.GetSpans())
	require.Len(t, spans, 2)
	wantParent := phaseSpan.SpanContext().SpanID()
	for _, s := range spans {
		assert.Equal(t, wantParent, s.Parent.SpanID(),
			"hook span should parent under the hooks.preRun span")
		attrs := spanAttrs(s)
		assert.NotContains(t, attrs, hookOutputTailKey, "output tail emitted with content capture off")
		assert.NotContains(t, attrs["hook.command"].(string), "sk-ant-api03-leak",
			"hook.command carries an unscrubbed secret")
	}

	clone := spanAttrs(spans[0])
	assert.Equal(t, "clone", clone["hook.name"])
	assert.Equal(t, "preRun", clone["hook.phase"])
	assert.Equal(t, true, clone["hook.output_truncated"])
	assert.Equal(t, 1200*time.Millisecond, spans[0].EndTime.Sub(spans[0].StartTime),
		"clone span should last the hook duration")
	assert.NotEqual(t, codes.Error, spans[0].Status.Code,
		"successful hook span has error status %q", spans[0].Status.Description)

	failed := spanAttrs(spans[1])
	assert.NotContains(t, failed, "hook.name", "unnamed hook should not carry hook.name")
	assert.Equal(t, int64(3), failed["hook.exit_code"])
	assert.Equal(t, true, failed["hook.continued_on_error"])
	assert.Equal(t, codes.Error, spans[1].Status.Code)
	assert.NotContains(t, spans[1].Status.Description, "sk-ant-api03-leak",
		"failed span status should be scrubbed")
}

func TestOTelTraceEmitter_RecordHookExecution_OutputTailRequiresCaptureContent(t *testing.T) {
	emitter, exporter := newTestOTelEmitterWithCapture()
	emitter.Start("run-otel-hook-tail", &types.RunConfig{RunID: "run-otel-hook-tail"})

	emitter.RecordHookExecution(emitter.RootContext(), types.HookExecution{
		Phase: "postRun", Command: "make test", OutputTail: "FAIL token=sk-ant-api03-leak", ExitCode: 1, Error: "exit status 1",
	})
	_, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)

	spans := hookSpans(exporter.GetSpans())
	require.Len(t, spans, 1)
	tail, ok := spanAttrs(spans[0])[hookOutputTailKey].(string)
	require.True(t, ok, "%s should be a string", hookOutputTailKey)
	require.True(t, strings.HasPrefix(tail, "FAIL token="),
		"%s = %q, want the captured tail", hookOutputTailKey, tail)
	require.NotContains(t, tail, "sk-ant-api03-leak", "captured tail carries an unscrubbed secret")
}

func TestOTelTraceEmitter_RecordHookExecution_ParentsUnderRootWithoutPhaseSpan(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-otel-hook-root", &types.RunConfig{RunID: "run-otel-hook-root"})

	emitter.RecordHookExecution(context.Background(), types.HookExecution{Phase: "postRun", Command: "true"})
	_, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)

	spans := exporter.GetSpans()
	hooks := hookSpans(spans)
	require.Len(t, hooks, 1)
	root := findSpanByName(t, spans, "invoke_agent stirrup")
	require.Equal(t, root.SpanContext.SpanID(), hooks[0].Parent.SpanID(),
		"hook span should parent under the root span")
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
	require.NoError(t, err)
	emitter.Start("run-gcs-hooks", nil)
	emitter.RecordHookExecution(context.Background(), types.HookExecution{Phase: "preRun", Name: "clone", Command: "git clone repo"})
	emitter.RecordHookExecution(context.Background(), types.HookExecution{
		Phase: "postRun", Index: 1, Command: "notify", OutputTail: "sk-ant-api03-leak", Error: "exit status 1",
	})

	tr, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)
	require.Len(t, tr.HookResults, 2)
	require.Equal(t, "clone", tr.HookResults[0].Name)

	body := string(srv.last().Body)
	require.NotContains(t, body, "sk-ant-api03-leak", "secret reached the uploaded trace:\n%s", body)
	var uploaded types.RunTrace
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(body)), &uploaded), "unmarshal uploaded body")
	require.Len(t, uploaded.HookResults, 2)
	require.Equal(t, "postRun", uploaded.HookResults[1].Phase)
}

func TestOTelTraceEmitter_RecordHookExecution_CapKeepsOutputTailEnd(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	emitter := newOTelTraceEmitter(tp, true, 256, false)
	emitter.Start("run-otel-hook-cap", &types.RunConfig{RunID: "run-otel-hook-cap"})

	tail := strings.Repeat("é", 200) + "FAILED: last line"
	emitter.RecordHookExecution(emitter.RootContext(), types.HookExecution{Phase: "postRun", Command: "make", OutputTail: tail})
	_, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)

	attrs := spanAttrs(hookSpans(exporter.GetSpans())[0])
	got := attrs[hookOutputTailKey].(string)
	assert.LessOrEqual(t, len(got), 256, "capped tail %q should fit within 256 bytes", got)
	assert.True(t, strings.HasSuffix(got, "FAILED: last line"), "capped tail %q should end with the last line", got)
	assert.True(t, utf8.ValidString(got), "capped tail %q should be valid UTF-8", got)
	assert.Equal(t, true, attrs[contentTruncatedKey(hookOutputTailKey)],
		"missing %s marker", contentTruncatedKey(hookOutputTailKey))
}
