package trace

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
	"github.com/rxbynerd/stirrup/types/version"
)

// OpenTelemetry GenAI semantic-convention attribute keys.
//
// These are the sole names emitted by the OTel trace emitter for any
// concept that has a GenAI semconv counterpart.
// Stirrup-specific attributes with no GenAI counterpart
// (run.id, run.mode, run.outcome, run.turns, harness.version,
// turn.number, turn.tool_calls, turn.duration_ms, tool.success,
// tool.duration_ms) keep their stirrup-prefixed names alongside.
//
// Spec: https://opentelemetry.io/docs/specs/semconv/gen-ai/
const (
	genAIProviderNameKey   = "gen_ai.provider.name"
	genAIRequestModelKey   = "gen_ai.request.model"
	genAIConversationIDKey = "gen_ai.conversation.id"
	genAIOperationNameKey  = "gen_ai.operation.name"
	genAIUsageInputTokens  = "gen_ai.usage.input_tokens"
	genAIUsageOutputTokens = "gen_ai.usage.output_tokens"
	genAIFinishReasonsKey  = "gen_ai.response.finish_reasons"
	genAIToolNameKey       = "gen_ai.tool.name"
)

// genAIProviderName maps stirrup provider type strings to the OTel GenAI
// `gen_ai.provider.name` enum values defined at
// https://opentelemetry.io/docs/specs/semconv/attributes-registry/gen-ai/.
// Unknown stirrup types fall through to the raw value so future provider
// types are still observable, even if dashboards don't recognise them.
//
// `openai-compatible` is intentionally NOT mapped to `openai`: it is the
// generic Chat Completions adapter used by vLLM, Granite Guardian, Ollama,
// Azure OpenAI, LiteLLM, and other vendors. Tagging those runs as
// `gen_ai.provider.name = "openai"` would mislabel telemetry. It falls
// through to the default branch and surfaces as the raw string until/unless
// a more specific provider type is configured.
func genAIProviderName(stirrupType string) string {
	switch stirrupType {
	case "anthropic":
		return "anthropic"
	case "bedrock":
		return "aws.bedrock"
	case "openai-responses":
		return "openai"
	case "gemini":
		return "gcp.vertex_ai"
	default:
		return stirrupType
	}
}

// OTelTraceEmitter records harness run telemetry as OpenTelemetry spans,
// exported via OTLP/gRPC to a collector endpoint.
type OTelTraceEmitter struct {
	provider *sdktrace.TracerProvider
	tracer   oteltrace.Tracer

	mu                sync.Mutex
	runID             string
	config            *types.RunConfig
	startedAt         time.Time
	rootSpan          oteltrace.Span
	rootCtx           context.Context
	turns             []types.TurnTrace
	toolCalls         []types.ToolCallTrace
	permissionDenials int
}

// NewOTelTraceEmitter creates an OTel trace emitter that exports spans to
// the given OTLP endpoint over the chosen wire protocol. The caller must
// eventually call Finish to flush and shut down the exporter.
//
// protocol selects the OTLP wire protocol:
//   - "" or "grpc": OTLP/gRPC. endpoint is host:port (default
//     "localhost:4317" upstream of this constructor); on-the-wire frames
//     are protobuf with gRPC framing.
//   - "http/protobuf": OTLP/HTTP with binary protobuf bodies. endpoint
//     is a full URL ending in the gateway base path
//     (e.g. "https://otlp-gateway-prod-us-east-0.grafana.net/otlp"); the
//     SDK appends "/v1/traces". TLS is on by default; the constructor
//     opts into WithInsecure() only when the endpoint scheme is plain
//     "http://" or omitted entirely so a Grafana Cloud URL never falls
//     back to an unencrypted POST.
//
// headers is forwarded to both transports unchanged. Resolve "secret://"
// references upstream via observability.ResolveHeaders so the OTel SDK
// only ever sees plaintext bearer tokens.
//
// resourceOpts threads the run-scoped resource attributes
// (deployment.environment, service.namespace, harness.run.mode) so traces
// emitted here share a consistent resource identity with metrics emitted
// from the same run. Callers that don't have a config in hand can pass a
// zero ResourceOptions and the resource builder will fall through to
// env-var fallbacks and the documented defaults.
func NewOTelTraceEmitter(ctx context.Context, endpoint, protocol string, headers map[string]string, resourceOpts observability.ResourceOptions) (*OTelTraceEmitter, error) {
	exporter, err := buildOTLPTraceExporter(ctx, endpoint, protocol, headers)
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(observability.BuildResource(resourceOpts)),
	)
	tracer := tp.Tracer("stirrup-harness")

	return &OTelTraceEmitter{
		provider: tp,
		tracer:   tracer,
	}, nil
}

// buildOTLPTraceExporter dispatches on the configured wire protocol and
// returns the matching OTel SDK trace exporter. Kept private so the
// public NewOTelTraceEmitter signature stays stable while the dispatch
// logic evolves (e.g. when otlploghttp lands per #96).
func buildOTLPTraceExporter(ctx context.Context, endpoint, protocol string, headers map[string]string) (*otlptrace.Exporter, error) {
	switch protocol {
	case "", "grpc":
		opts := []otlptracegrpc.Option{
			otlptracegrpc.WithEndpoint(endpoint),
			otlptracegrpc.WithInsecure(),
		}
		if len(headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(headers))
		}
		return otlptracegrpc.New(ctx, opts...)
	case "http/protobuf":
		opts := []otlptracehttp.Option{
			otlptracehttp.WithEndpoint(stripURLScheme(endpoint)),
		}
		// The SDK's WithEndpointURL would parse the full URL and pick
		// up scheme+path in one call, but it is not available in
		// v1.43.0; emulate by stripping the scheme for the host
		// component and toggling WithInsecure based on the original
		// scheme. Path is propagated below via WithURLPath when the
		// endpoint carries one.
		if path := urlPath(endpoint); path != "" {
			opts = append(opts, otlptracehttp.WithURLPath(joinTracesPath(path)))
		}
		if isInsecureEndpoint(endpoint) {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		if len(headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(headers))
		}
		return otlptracehttp.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unsupported OTLP protocol %q (allowed: grpc, http/protobuf)", protocol)
	}
}

// stripURLScheme returns the host:port portion of an OTLP endpoint URL
// for use with otlptracehttp.WithEndpoint, which expects a bare host
// and toggles TLS via WithInsecure(). When the endpoint has no scheme
// (e.g. "localhost:4318"), the value is returned unchanged. Path
// components are dropped here and re-applied separately via
// WithURLPath; the caller is responsible for that step.
func stripURLScheme(endpoint string) string {
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(endpoint, scheme) {
			rest := strings.TrimPrefix(endpoint, scheme)
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				return rest[:i]
			}
			return rest
		}
	}
	if i := strings.IndexByte(endpoint, '/'); i >= 0 {
		return endpoint[:i]
	}
	return endpoint
}

// urlPath returns the path component of an OTLP endpoint URL, or the
// empty string when the endpoint has no path beyond the host.
func urlPath(endpoint string) string {
	rest := endpoint
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(endpoint, scheme) {
			rest = strings.TrimPrefix(endpoint, scheme)
			break
		}
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[i:]
	}
	return ""
}

// joinTracesPath appends the per-signal "/v1/traces" suffix to a base
// gateway path, mirroring what the OTel SDK does when given an endpoint
// without an explicit URL path. Grafana Cloud's gateway expects the
// configured URL to end in "/otlp" and resolves the per-signal segment
// itself, so we preserve the operator-supplied prefix and tack on the
// suffix the SDK would otherwise apply silently.
func joinTracesPath(basePath string) string {
	return strings.TrimRight(basePath, "/") + "/v1/traces"
}

// isInsecureEndpoint returns true when the endpoint URL uses plain
// HTTP — the only case in which the constructor opts into
// WithInsecure(). A bare "localhost:4318" or any other scheme-less
// endpoint also gets WithInsecure() (the typical local-collector
// case); a "https://" scheme always means TLS.
func isInsecureEndpoint(endpoint string) bool {
	if strings.HasPrefix(endpoint, "https://") {
		return false
	}
	if strings.HasPrefix(endpoint, "http://") {
		return true
	}
	// No scheme: assume plaintext (developer / local collector flow).
	return true
}

// newOTelTraceEmitterForTest creates an OTel trace emitter backed by the given
// TracerProvider, used in tests to capture spans in-memory.
func newOTelTraceEmitterForTest(tp *sdktrace.TracerProvider) *OTelTraceEmitter {
	return &OTelTraceEmitter{
		provider: tp,
		tracer:   tp.Tracer("stirrup-harness"),
	}
}

// NewOTelTraceEmitterForTest is the exported wrapper of the test-only
// constructor. It lets tests in other packages (notably core) build an
// OTelTraceEmitter around an in-memory TracerProvider so they can assert
// on emitted spans without spinning up an OTLP collector. Not intended for
// production use.
func NewOTelTraceEmitterForTest(tp *sdktrace.TracerProvider) *OTelTraceEmitter {
	return newOTelTraceEmitterForTest(tp)
}

// Start initialises a new trace with the given run ID and configuration.
// It creates the root "run" span.
func (e *OTelTraceEmitter) Start(runID string, config *types.RunConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.runID = runID
	e.config = config
	e.startedAt = time.Now()
	e.turns = nil
	e.toolCalls = nil
	e.permissionDenials = 0

	ctx := context.Background()
	// NOTE: gen_ai.agent.id is intentionally NOT emitted. The OTel GenAI spec
	// defines this as a persistent agent identity (e.g. an OpenAI Assistant ID),
	// not a per-execution run ID. Stirrup has no first-class named-agent concept;
	// emit when one exists. See follow-up issue (#127).
	ctx, span := e.tracer.Start(ctx, "run",
		oteltrace.WithAttributes(
			attribute.String("run.id", runID),
			attribute.String("harness.version", version.Version()),
		),
	)

	if config != nil {
		span.SetAttributes(
			attribute.String("run.mode", config.Mode),
			// gen_ai.provider.name surfaces the spec enum value
			// ("openai", "aws.bedrock", ...) translated from stirrup's
			// internal Provider.Type vocabulary so vendor APM
			// dashboards filter correctly.
			attribute.String(genAIProviderNameKey, genAIProviderName(config.Provider.Type)),
		)
		if config.ModelRouter.Model != "" {
			span.SetAttributes(
				attribute.String(genAIRequestModelKey, config.ModelRouter.Model),
			)
		}
		if config.SessionName != "" {
			// Set on the root span so child spans inherit access via context.
			// Skipped when empty so we don't pollute traces with empty
			// attributes for runs that did not specify a label.
			span.SetAttributes(
				attribute.String(genAIConversationIDKey, config.SessionName),
			)
		}
	}

	e.rootSpan = span
	e.rootCtx = ctx
}

// RecordTurn creates a child span under the root representing a single
// agentic loop turn. The span duration is derived from DurationMs.
func (e *OTelTraceEmitter) RecordTurn(turn types.TurnTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.turns = append(e.turns, turn)

	if e.rootCtx == nil {
		return
	}

	// Create a child span with explicit timing derived from turn duration.
	spanEnd := time.Now()
	spanStart := spanEnd.Add(-time.Duration(turn.DurationMs) * time.Millisecond)

	// TODO(#89): turn[N] is parented off e.rootCtx, which Start()
	// derives from context.Background(). For child sub-agent loops
	// (#55) this prevents turn[N] spans from nesting under the
	// parent's tool.spawn_agent span — the loop's TraceContext is not
	// visible to the emitter. The preferred fix is to add an
	// injected parentCtx for child emitter variants; tracked
	// separately in #89.
	_, span := e.tracer.Start(e.rootCtx, fmt.Sprintf("turn[%d]", turn.Turn),
		oteltrace.WithTimestamp(spanStart),
		oteltrace.WithAttributes(
			attribute.Int("turn.number", turn.Turn),
			attribute.Int(genAIUsageInputTokens, turn.Tokens.Input),
			attribute.Int(genAIUsageOutputTokens, turn.Tokens.Output),
			attribute.Int("turn.tool_calls", turn.ToolCalls),
			// gen_ai.response.finish_reasons is defined as a string
			// array in the GenAI semconv; we wrap our single scalar
			// StopReason in a one-element slice rather than emitting
			// a scalar that downstream consumers would have to
			// special-case.
			attribute.StringSlice(genAIFinishReasonsKey, []string{turn.StopReason}),
			attribute.Int64("turn.duration_ms", turn.DurationMs),
			// Per GenAI semconv, a turn is a chat completion.
			attribute.String(genAIOperationNameKey, "chat"),
		),
	)
	span.End(oteltrace.WithTimestamp(spanEnd))
}

// RecordTurnRecord is a no-op for OTel. OTel spans capture turn-level
// counters (tokens, duration, stop reason) and tool spans capture per-
// call metadata, but full transcript content is not modelled as span
// attributes — span exporters typically truncate large strings and the
// GenAI semantic conventions intentionally do not push raw prompt /
// completion text through trace exporters. Transcript recording lives
// on the streamed JSONLTraceEmitter.
func (e *OTelTraceEmitter) RecordTurnRecord(_ types.TurnRecord) {}

// RecordToolCall creates a child span for a tool invocation.
func (e *OTelTraceEmitter) RecordToolCall(call types.ToolCallTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.toolCalls = append(e.toolCalls, call)

	if e.rootCtx == nil {
		return
	}

	spanEnd := time.Now()
	spanStart := spanEnd.Add(-time.Duration(call.DurationMs) * time.Millisecond)

	_, span := e.tracer.Start(e.rootCtx, "tool_call",
		oteltrace.WithTimestamp(spanStart),
		oteltrace.WithAttributes(
			attribute.String(genAIToolNameKey, call.Name),
			attribute.Bool("tool.success", call.Success),
			attribute.Int64("tool.duration_ms", call.DurationMs),
		),
	)
	span.End(oteltrace.WithTimestamp(spanEnd))
}

// RecordPermissionDenial increments the run-level permission denial count.
func (e *OTelTraceEmitter) RecordPermissionDenial() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.permissionDenials++
}

// Finish sets the outcome on the root span, ends it, flushes the exporter,
// and returns the aggregated RunTrace.
func (e *OTelTraceEmitter) Finish(ctx context.Context, outcome string) (*types.RunTrace, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()

	// Set outcome on root span and end it.
	if e.rootSpan != nil && e.rootSpan.SpanContext().IsValid() {
		e.rootSpan.SetAttributes(
			attribute.String("run.outcome", outcome),
			attribute.Int("run.turns", len(e.turns)),
			attribute.Int("run.permission_denials", e.permissionDenials),
		)
		e.rootSpan.End()
	}

	// Flush and shut down the trace provider.
	if e.provider != nil {
		if err := e.provider.ForceFlush(ctx); err != nil {
			// Non-fatal: log but continue building the trace.
			// A ForceFlush failure means the in-memory span batch
			// could not be exported before the run ended; the
			// RunTrace aggregate below is still valid for the
			// caller, but downstream observers querying the OTel
			// backend will be missing the tail of this run.
			// ScrubHandler still runs over the message, so any
			// secret-shaped substring in the wrapped error is
			// redacted before it lands in stderr JSONL.
			slog.Default().Warn("OTel ForceFlush failed, spans may be lost", "error", err)
		}
	}

	// Build the RunTrace aggregate (same logic as JSONLTraceEmitter).
	var totalTokens types.TokenUsage
	for _, turn := range e.turns {
		totalTokens.Input += turn.Tokens.Input
		totalTokens.Output += turn.Tokens.Output
	}

	summaries := make([]types.ToolCallSummary, len(e.toolCalls))
	for i, tc := range e.toolCalls {
		summaries[i] = types.ToolCallSummary(tc)
	}

	var redactedConfig types.RunConfig
	if e.config != nil {
		redactedConfig = e.config.Redact()
	}

	trace := &types.RunTrace{
		ID:                e.runID,
		Config:            redactedConfig,
		StartedAt:         e.startedAt,
		CompletedAt:       now,
		Turns:             len(e.turns),
		TokenUsage:        totalTokens,
		ToolCalls:         summaries,
		PermissionDenials: e.permissionDenials,
		Outcome:           outcome,
	}

	return trace, nil
}

// Tracer returns the OTel tracer used by this emitter, allowing the loop
// to create child spans for component calls.
func (e *OTelTraceEmitter) Tracer() oteltrace.Tracer {
	return e.tracer
}

// RootContext returns the context containing the root run span,
// for use as the parent of loop-level spans.
func (e *OTelTraceEmitter) RootContext() context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rootCtx != nil {
		return e.rootCtx
	}
	return context.Background()
}

// Close shuts down the tracer provider and exporter.
func (e *OTelTraceEmitter) Close() error {
	if e.provider == nil {
		return nil
	}
	return e.provider.Shutdown(context.Background())
}
