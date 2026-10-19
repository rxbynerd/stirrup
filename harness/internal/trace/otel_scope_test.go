package trace

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/types"
)

func sessionConfig(session string) *types.RunConfig {
	return &types.RunConfig{
		RunID:       "run-scope",
		Mode:        "execution",
		SessionName: session,
		Provider:    types.ProviderConfig{Type: "anthropic"},
	}
}

func TestRunScope_SessionOnEverySpan(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-scope", sessionConfig("nightly-eval"))
	_, child := emitter.Tracer().Start(emitter.RootContext(), "context.prepare")
	child.End()
	_, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)

	spans := exporter.GetSpans()
	require.Len(t, spans, 2)
	for _, s := range spans {
		assertAttribute(t, s, genAIConversationIDKey, "nightly-eval")
		assertAttribute(t, s, sessionIDKey, "nightly-eval")
		assertAttribute(t, s, genAIAgentNameKey, agentNameRun)
	}
}

func TestRunScope_NoSessionAttributesWithoutSessionName(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-scope", sessionConfig(""))
	_, child := emitter.Tracer().Start(emitter.RootContext(), "context.prepare")
	child.End()
	_, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)

	for _, s := range exporter.GetSpans() {
		assertAttributesAbsent(t, s, genAIConversationIDKey, sessionIDKey)
		assertAttribute(t, s, genAIAgentNameKey, agentNameRun)
	}
}

func TestRunScope_FollowUpRunResetsSession(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-1", sessionConfig("first"))
	_, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err, "finish run-1")
	exporter.Reset()
	emitter.Start("run-2", sessionConfig(""))
	_, err = emitter.Finish(context.Background(), "success")
	require.NoError(t, err, "finish run-2")

	for _, s := range exporter.GetSpans() {
		assertAttributesAbsent(t, s, genAIConversationIDKey, sessionIDKey)
	}
}

func TestRunScope_AgentNameInheritedFromNearestAgentSpan(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-scope", sessionConfig("s"))
	tracer := emitter.Tracer()

	subCtx, sub := tracer.Start(emitter.RootContext(), "invoke_agent "+agentNameSubAgent,
		oteltrace.WithAttributes(attribute.String(genAIAgentNameKey, agentNameSubAgent)))
	grandCtx, mid := tracer.Start(subCtx, "execute_tool read_file")
	_, leaf := tracer.Start(grandCtx, "permission.check")
	leaf.End()
	mid.End()
	sub.End()
	_, after := tracer.Start(emitter.RootContext(), "git.checkpoint")
	after.End()
	_, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)

	spans := exporter.GetSpans()
	cases := []struct {
		name      string
		spanName  string
		wantAgent string
	}{
		{
			name:      "sub-agent invocation span",
			spanName:  "invoke_agent " + agentNameSubAgent,
			wantAgent: agentNameSubAgent,
		},
		{
			name:      "tool call under sub-agent",
			spanName:  "execute_tool read_file",
			wantAgent: agentNameSubAgent,
		},
		{
			name:      "permission check under sub-agent",
			spanName:  "permission.check",
			wantAgent: agentNameSubAgent,
		},
		{
			name:      "span started after sub-agent ends",
			spanName:  "git.checkpoint",
			wantAgent: agentNameRun,
		},
		{
			name:      "run agent invocation span",
			spanName:  "invoke_agent " + agentNameRun,
			wantAgent: agentNameRun,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := findSpanByName(t, spans, tc.spanName)
			assertAttribute(t, s, genAIAgentNameKey, tc.wantAgent)
			assertAttribute(t, s, sessionIDKey, "s")
		})
	}
	require.Empty(t, emitter.scope.agents, "span entries retained after every span ended")
}

// startAttrRecorder captures the attributes each span carries when its
// OnStart runs.
type startAttrRecorder struct {
	seen map[string][]attribute.KeyValue
}

func (r *startAttrRecorder) OnStart(_ context.Context, s sdktrace.ReadWriteSpan) {
	r.seen[s.Name()] = s.Attributes()
}
func (r *startAttrRecorder) OnEnd(sdktrace.ReadOnlySpan)      {}
func (r *startAttrRecorder) Shutdown(context.Context) error   { return nil }
func (r *startAttrRecorder) ForceFlush(context.Context) error { return nil }

func TestWithSpanProcessor_SeesRunScopeAttributesAtStart(t *testing.T) {
	rec := &startAttrRecorder{seen: map[string][]attribute.KeyValue{}}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))
	emitter := newOTelTraceEmitterForTest(tp, false, WithSpanProcessor(rec))
	emitter.Start("run-scope", sessionConfig("s"))
	_, child := emitter.Tracer().Start(emitter.RootContext(), "context.prepare")
	child.End()
	_, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)

	attrs := rec.seen["context.prepare"]
	missing := map[attribute.Key]string{
		genAIAgentNameKey:      agentNameRun,
		genAIConversationIDKey: "s",
		sessionIDKey:           "s",
	}
	for _, kv := range attrs {
		if v, ok := missing[kv.Key]; ok && kv.Value.AsString() == v {
			delete(missing, kv.Key)
		}
	}
	require.Empty(t, missing, "start attributes missing from span; saw %v", attrs)
}

func correlatedConfig(session string) *types.RunConfig {
	cfg := sessionConfig(session)
	cfg.Correlation = &types.CorrelationConfig{
		SessionID: "cp-session-42",
		TaskID:    "task-7",
		AttemptID: "attempt-3",
		UserID:    "user-9f2c",
		URL:       "https://cp.example.com/runs/42",
		Labels:    map[string]string{"agent_profile": "reviewer", "execution_mode": "batch"},
	}
	return cfg
}

func TestRunScope_CorrelationOnEverySpan(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-scope", correlatedConfig("nightly-eval"))
	tracer := emitter.Tracer()
	subCtx, sub := tracer.Start(emitter.RootContext(), "invoke_agent "+agentNameSubAgent,
		oteltrace.WithAttributes(attribute.String(genAIAgentNameKey, agentNameSubAgent)))
	_, leaf := tracer.Start(subCtx, "execute_tool read_file")
	leaf.End()
	sub.End()
	_, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)

	spans := exporter.GetSpans()
	require.Len(t, spans, 3)
	for _, s := range spans {
		assertAttribute(t, s, sessionIDKey, "cp-session-42")
		assertAttribute(t, s, genAIConversationIDKey, "cp-session-42")
		assertAttribute(t, s, userIDKey, "user-9f2c")
		assertAttribute(t, s, taskIDKey, "task-7")
		assertAttribute(t, s, attemptIDKey, "attempt-3")
		assertAttribute(t, s, controlPlaneURLKey, "https://cp.example.com/runs/42")
		assertAttribute(t, s, "stirrup.label.agent_profile", "reviewer")
		assertAttribute(t, s, "stirrup.label.execution_mode", "batch")
		labels := 0
		for _, kv := range s.Attributes {
			if strings.HasPrefix(string(kv.Key), labelKeyPrefix) {
				labels++
			}
		}
		assert.Equal(t, 2, labels, "%s: label attributes, want one per label", s.Name)
	}
}

func TestRunScope_SessionNameIsSessionFallback(t *testing.T) {
	cfg := sessionConfig("nightly-eval")
	cfg.Correlation = &types.CorrelationConfig{TaskID: "task-7"}
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-scope", cfg)
	_, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)

	for _, s := range exporter.GetSpans() {
		assertAttribute(t, s, sessionIDKey, "nightly-eval")
		assertAttribute(t, s, genAIConversationIDKey, "nightly-eval")
		assertAttribute(t, s, taskIDKey, "task-7")
		assertAttributesAbsent(t, s, userIDKey, attemptIDKey, controlPlaneURLKey)
	}
}

func TestRunScope_NoSessionWithoutSessionIDOrName(t *testing.T) {
	cfg := sessionConfig("")
	cfg.Correlation = &types.CorrelationConfig{UserID: "user-9f2c"}
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-scope", cfg)
	_, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)

	for _, s := range exporter.GetSpans() {
		assertAttributesAbsent(t, s, sessionIDKey, genAIConversationIDKey)
		assertAttribute(t, s, userIDKey, "user-9f2c")
	}
}

func TestRunScope_FollowUpRunResetsCorrelation(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-1", correlatedConfig(""))
	_, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err, "finish run-1")
	exporter.Reset()
	emitter.Start("run-2", sessionConfig(""))
	_, err = emitter.Finish(context.Background(), "success")
	require.NoError(t, err, "finish run-2")

	for _, s := range exporter.GetSpans() {
		assertAttributesAbsent(t, s, sessionIDKey, userIDKey, taskIDKey, attemptIDKey,
			controlPlaneURLKey, "stirrup.label.agent_profile", "stirrup.label.execution_mode")
	}
}

func TestRunScope_BackToBackRunsCarryTheirOwnCorrelation(t *testing.T) {
	first := correlatedConfig("")
	second := correlatedConfig("")
	second.Correlation = &types.CorrelationConfig{
		SessionID: "cp-session-43",
		TaskID:    "task-8",
		AttemptID: "attempt-1",
		UserID:    "user-1a2b",
		URL:       "https://cp.example.com/runs/43",
		Labels:    map[string]string{"agent_profile": "triager"},
	}

	emitter, exporter := newTestOTelEmitter()
	for i, cfg := range []*types.RunConfig{first, second} {
		emitter.Start(fmt.Sprintf("run-%d", i+1), cfg)
		_, child := emitter.Tracer().Start(emitter.RootContext(), "context.prepare")
		child.End()
		_, err := emitter.Finish(context.Background(), "success")
		require.NoError(t, err, "finish run %d", i+1)
	}

	spans := exporter.GetSpans()
	require.Len(t, spans, 4)
	byTrace := map[oteltrace.TraceID]*types.CorrelationConfig{
		spans[1].SpanContext.TraceID(): first.Correlation,
		spans[3].SpanContext.TraceID(): second.Correlation,
	}
	require.Len(t, byTrace, 2, "the two runs should be separate traces")
	for _, s := range spans {
		want := byTrace[s.SpanContext.TraceID()]
		assertAttribute(t, s, sessionIDKey, want.SessionID)
		assertAttribute(t, s, userIDKey, want.UserID)
		assertAttribute(t, s, taskIDKey, want.TaskID)
		assertAttribute(t, s, attemptIDKey, want.AttemptID)
		assertAttribute(t, s, controlPlaneURLKey, want.URL)
		assertAttribute(t, s, "stirrup.label.agent_profile", want.Labels["agent_profile"])
		if want == second.Correlation {
			assertAttributesAbsent(t, s, "stirrup.label.execution_mode")
		}
	}
}

func TestRunScope_CorrelationIsScrubbed(t *testing.T) {
	key := "sk-ant-api03-" + strings.Repeat("A", 40)
	cfg := sessionConfig("")
	cfg.Correlation = &types.CorrelationConfig{
		UserID: "user-" + key,
		URL:    "https://cp.example.com/runs/42?token=" + key,
		Labels: map[string]string{"agent_profile": key},
	}
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-scope", cfg)
	runTrace, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)

	for _, s := range exporter.GetSpans() {
		for _, kv := range s.Attributes {
			assert.NotContains(t, kv.Value.String(), key,
				"%s: attribute %s carries the unscrubbed key", s.Name, kv.Key)
		}
		assertAttribute(t, s, controlPlaneURLKey, "https://cp.example.com/runs/42?token=[REDACTED]")
	}

	got := runTrace.Config.Correlation
	require.NotNil(t, got, "RunTrace config has no correlation")
	require.NotContains(t, got.URL+got.UserID+got.Labels["agent_profile"], key,
		"RunTrace config correlation not scrubbed: %+v", got)
	require.Contains(t, cfg.Correlation.URL, key, "scrubbing must not reach the live config")
}
