package trace

import (
	"context"
	"fmt"
	"strings"
	"testing"

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
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(spans))
	}
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
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	for _, s := range exporter.GetSpans() {
		assertAttributesAbsent(t, s, genAIConversationIDKey, sessionIDKey)
		assertAttribute(t, s, genAIAgentNameKey, agentNameRun)
	}
}

func TestRunScope_FollowUpRunResetsSession(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-1", sessionConfig("first"))
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	exporter.Reset()
	emitter.Start("run-2", sessionConfig(""))
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
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
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	spans := exporter.GetSpans()
	for name, want := range map[string]string{
		"invoke_agent " + agentNameSubAgent: agentNameSubAgent,
		"execute_tool read_file":            agentNameSubAgent,
		"permission.check":                  agentNameSubAgent,
		"git.checkpoint":                    agentNameRun,
		"invoke_agent " + agentNameRun:      agentNameRun,
	} {
		s := findSpanByName(t, spans, name)
		assertAttribute(t, s, genAIAgentNameKey, want)
		assertAttribute(t, s, sessionIDKey, "s")
	}
	if n := len(emitter.scope.agents); n != 0 {
		t.Errorf("processor retains %d span entries after every span ended", n)
	}
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
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	attrs := rec.seen["context.prepare"]
	want := map[attribute.Key]string{
		genAIAgentNameKey:      agentNameRun,
		genAIConversationIDKey: "s",
		sessionIDKey:           "s",
	}
	for _, kv := range attrs {
		if v, ok := want[kv.Key]; ok && kv.Value.AsString() == v {
			delete(want, kv.Key)
		}
	}
	if len(want) > 0 {
		t.Errorf("extra processor missing start attributes %v; saw %v", want, attrs)
	}
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
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("expected 3 spans, got %d", len(spans))
	}
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
		if labels != 2 {
			t.Errorf("%s: %d label attributes, want one per label", s.Name, labels)
		}
	}
}

func TestRunScope_SessionNameIsSessionFallback(t *testing.T) {
	cfg := sessionConfig("nightly-eval")
	cfg.Correlation = &types.CorrelationConfig{TaskID: "task-7"}
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-scope", cfg)
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
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
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	for _, s := range exporter.GetSpans() {
		assertAttributesAbsent(t, s, sessionIDKey, genAIConversationIDKey)
		assertAttribute(t, s, userIDKey, "user-9f2c")
	}
}

func TestRunScope_FollowUpRunResetsCorrelation(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-1", correlatedConfig(""))
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	exporter.Reset()
	emitter.Start("run-2", sessionConfig(""))
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
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
		if _, err := emitter.Finish(context.Background(), "success"); err != nil {
			t.Fatalf("Finish run %d: %v", i+1, err)
		}
	}

	spans := exporter.GetSpans()
	if len(spans) != 4 {
		t.Fatalf("expected 4 spans, got %d", len(spans))
	}
	byTrace := map[oteltrace.TraceID]*types.CorrelationConfig{
		spans[1].SpanContext.TraceID(): first.Correlation,
		spans[3].SpanContext.TraceID(): second.Correlation,
	}
	if len(byTrace) != 2 {
		t.Fatal("the two runs should be separate traces")
	}
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
	trace, err := emitter.Finish(context.Background(), "success")
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	for _, s := range exporter.GetSpans() {
		for _, kv := range s.Attributes {
			if strings.Contains(kv.Value.String(), key) {
				t.Errorf("%s: attribute %s carries the unscrubbed key", s.Name, kv.Key)
			}
		}
		assertAttribute(t, s, controlPlaneURLKey, "https://cp.example.com/runs/42?token=[REDACTED]")
	}
	if got := trace.Config.Correlation; got == nil || strings.Contains(got.URL+got.UserID+got.Labels["agent_profile"], key) {
		t.Errorf("RunTrace config correlation not scrubbed: %+v", got)
	}
	if !strings.Contains(cfg.Correlation.URL, key) {
		t.Error("scrubbing must not reach the live config")
	}
}
