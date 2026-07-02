package trace

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/security"
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

	// errorTypeKey is the stable (non-Development) semconv error
	// attribute, emitted on failed tool spans with the bounded
	// observability.ToolFailureCategory vocabulary as its value.
	errorTypeKey = "error.type"

	// Message-content attributes, emitted only when CaptureContent is
	// opted into. As of the pinned semconv (v1.40.0, matching
	// observability/resource.go) these span attributes are the current
	// form for recording content — the older gen_ai.{role}.message span
	// events and gen_ai.prompt/gen_ai.completion attributes are
	// deprecated in their favour. The Go attribute API has no structured
	// "any" value, so the spec's "MAY be recorded as a JSON string if
	// structured format is not supported" branch applies: each value is
	// the JSON-serialised form of the schema in
	// gen-ai-{input,output}-messages.json / gen-ai-system-instructions.json.
	genAIInputMessagesKey      = "gen_ai.input.messages"
	genAIOutputMessagesKey     = "gen_ai.output.messages"
	genAISystemInstructionsKey = "gen_ai.system_instructions"

	// Tool-call content attributes, likewise capture-gated: the spec
	// marks gen_ai.tool.call.arguments / .result Opt-In for the same
	// PII reasons as message content. The call ID rides along so a tool
	// span correlates with the tool_call / tool_call_response parts
	// inside the surrounding turn's message attributes.
	genAIToolCallIDKey        = "gen_ai.tool.call.id"
	genAIToolCallArgumentsKey = "gen_ai.tool.call.arguments"
	genAIToolCallResultKey    = "gen_ai.tool.call.result"
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

	// captureContent opts the emitter into recording prompt/completion
	// content on turn spans via the GenAI semconv attributes
	// (traceEmitter.captureContent, issue #413). Immutable after
	// construction, so the off-path methods can read it without the
	// mutex; when false every content path below is unreachable and the
	// emitter's span output is byte-identical to the pre-capture
	// behaviour.
	captureContent bool

	mu                 sync.Mutex
	runID              string
	config             *types.RunConfig
	startedAt          time.Time
	rootSpan           oteltrace.Span
	rootCtx            context.Context
	turns              []types.TurnTrace
	toolCalls          []types.ToolCallTrace
	permissionDenials  int
	finalAssistantText string

	// systemInstructionsJSON is the run's system prompt, scrubbed and
	// pre-serialised to the gen_ai.system_instructions attribute encoding
	// by RecordSystemInstructions. Serialising once at record time
	// (rather than per turn span) keeps the per-turn cost to a string
	// copy. Empty when capture is off or the loop has not (yet)
	// forwarded a prompt.
	systemInstructionsJSON string

	// rootInputMessagesJSON / rootOutputMessagesJSON are the run-level
	// content stamped on the root span at Finish, making the root span a
	// complete invoke_agent observation (the agent's input and final
	// output) for backends that derive trace-level views from it. Both
	// are retained from the parent run's own turn records (RunID == "");
	// forwarded sub-agent records never contribute. Input is set-once —
	// turn 0's input messages are exactly the seed prompt — and output
	// is overwrite-always so the final assistant message wins. Empty
	// when capture is off.
	rootInputMessagesJSON  string
	rootOutputMessagesJSON string

	// pendingTurns buffers turn summaries (and the span timestamps
	// derived at RecordTurn time) between RecordTurn and the matching
	// RecordTurnRecord while capture is on, so a single turn[N] span
	// carries both the counter attributes and the content attributes.
	// Keyed by (RunID, Turn): RunID is empty for the parent run's own
	// turns and set on events forwarded from sub-agent loops, so a
	// child's turn N never merges onto the parent's turn N. Entries
	// whose record never arrives (e.g. the empty-stop-reason error
	// return) are flushed as plain counter spans at Finish. Nil when
	// capture is off.
	pendingTurns map[pendingTurnKey]pendingTurn

	// pendingToolCalls is the tool-call analogue of pendingTurns: while
	// capture is on, RecordToolCall buffers the summary (timestamps
	// frozen) until the enclosing turn's RecordTurnRecord delivers the
	// call's arguments and result, so one execute_tool span carries the
	// counters and the content together. Keyed by (RunID, tool_use ID);
	// calls without an ID are un-keyable and emit immediately as plain
	// spans instead of buffering. Entries whose record never arrives are
	// flushed as plain spans at Finish. Nil when capture is off.
	pendingToolCalls map[pendingToolKey]pendingToolCall
}

// Compile-time interface satisfaction guards: the loop discovers the
// content-capture system-prompt hook through the optional
// SystemInstructionsRecorder assertion, so losing the method would be
// a silent regression rather than a build break without this.
var (
	_ TraceEmitter               = (*OTelTraceEmitter)(nil)
	_ SystemInstructionsRecorder = (*OTelTraceEmitter)(nil)
	_ FinalAssistantTextRecorder = (*OTelTraceEmitter)(nil)
)

// pendingTurnKey pairs a buffered turn summary with its later
// transcript record. See OTelTraceEmitter.pendingTurns.
type pendingTurnKey struct {
	runID string
	turn  int
}

// pendingTurn is a turn summary awaiting its transcript record, with
// the span timing frozen at RecordTurn time so the merged span carries
// the same timestamps an unmerged span would have.
type pendingTurn struct {
	trace     types.TurnTrace
	spanStart time.Time
	spanEnd   time.Time
}

// pendingToolKey pairs a buffered tool call summary with its later
// transcript entry. See OTelTraceEmitter.pendingToolCalls.
type pendingToolKey struct {
	runID string
	id    string
}

// pendingToolCall is a tool call summary awaiting its transcript entry,
// with the span timing frozen at RecordToolCall time.
type pendingToolCall struct {
	call      types.ToolCallTrace
	spanStart time.Time
	spanEnd   time.Time
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
//
// captureContent opts the emitter into recording prompt/completion
// content on turn spans (traceEmitter.captureContent). Default-off
// upstream; see RecordTurnRecord for the scrubbing contract.
func NewOTelTraceEmitter(ctx context.Context, endpoint, protocol string, headers map[string]string, resourceOpts observability.ResourceOptions, captureContent bool) (*OTelTraceEmitter, error) {
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
		provider:       tp,
		tracer:         tracer,
		captureContent: captureContent,
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

// newOTelTraceEmitterForTest creates an OTel trace emitter backed by the
// given TracerProvider, used in tests to capture spans in-memory.
// captureContent is taken at construction time, like the production
// constructor — it is documented as immutable afterwards (the off-path
// methods read it without the mutex), so tests must not set the field
// post-construction either.
func newOTelTraceEmitterForTest(tp *sdktrace.TracerProvider, captureContent bool) *OTelTraceEmitter {
	return &OTelTraceEmitter{
		provider:       tp,
		tracer:         tp.Tracer("stirrup-harness"),
		captureContent: captureContent,
	}
}

// NewOTelTraceEmitterForTest is the exported wrapper of the test-only
// constructor. It lets tests in other packages (notably core) build an
// OTelTraceEmitter around an in-memory TracerProvider so they can assert
// on emitted spans without spinning up an OTLP collector. Content
// capture stays off, matching the default emitter shape those tests
// exercise. Not intended for production use.
func NewOTelTraceEmitterForTest(tp *sdktrace.TracerProvider) *OTelTraceEmitter {
	return newOTelTraceEmitterForTest(tp, false)
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
	e.finalAssistantText = ""
	e.systemInstructionsJSON = ""
	e.rootInputMessagesJSON = ""
	e.rootOutputMessagesJSON = ""
	e.pendingTurns = nil
	e.pendingToolCalls = nil

	ctx := context.Background()
	// NOTE: gen_ai.agent.id is intentionally NOT emitted. The OTel GenAI spec
	// defines this as a persistent agent identity (e.g. an OpenAI Assistant ID),
	// not a per-execution run ID. Stirrup has no first-class named-agent concept;
	// emit when one exists. See follow-up issue (#127).
	// The root span is the run-level agent invocation; the semconv
	// "invoke_agent {gen_ai.agent.name}" span name is not adopted because
	// stirrup has no named-agent concept (#127) and backends type
	// observations from the operation-name attribute, not the span name.
	ctx, span := e.tracer.Start(ctx, "run",
		oteltrace.WithAttributes(
			attribute.String("run.id", runID),
			attribute.String("harness.version", version.Version()),
			attribute.String(genAIOperationNameKey, "invoke_agent"),
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
//
// When content capture is on, the span is not emitted here: the summary
// is buffered (with its timestamps frozen) until the matching
// RecordTurnRecord arrives, so one turn[N] span carries the counters
// and the GenAI content attributes together. Unmatched entries are
// flushed as plain counter spans at Finish.
func (e *OTelTraceEmitter) RecordTurn(turn types.TurnTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.turns = append(e.turns, turn)

	if e.rootCtx == nil {
		return
	}

	// Derive explicit span timing from the turn duration.
	spanEnd := time.Now()
	spanStart := spanEnd.Add(-time.Duration(turn.DurationMs) * time.Millisecond)

	if e.captureContent {
		key := pendingTurnKey{runID: turn.RunID, turn: turn.Turn}
		if prev, ok := e.pendingTurns[key]; ok {
			// A second summary for the same key without an intervening
			// record (the loop never does this; defensive against a
			// future re-entry path). Flush the stale entry as a plain
			// span so it is not silently dropped.
			e.emitTurnSpanLocked(prev.trace, prev.spanStart, prev.spanEnd, nil)
		}
		if e.pendingTurns == nil {
			e.pendingTurns = map[pendingTurnKey]pendingTurn{}
		}
		e.pendingTurns[key] = pendingTurn{trace: turn, spanStart: spanStart, spanEnd: spanEnd}
		return
	}

	e.emitTurnSpanLocked(turn, spanStart, spanEnd, nil)
}

// emitTurnSpanLocked creates and ends the turn[N] child span. content
// is nil on the no-capture path (and for flushed unmatched summaries),
// keeping the attribute set byte-identical to the pre-capture emitter;
// non-nil content appends the GenAI message attributes after the
// counter attributes. Must be called with e.mu held.
//
// TODO(#89): turn[N] is parented off e.rootCtx, which Start()
// derives from context.Background(). For child sub-agent loops
// (#55) this prevents turn[N] spans from nesting under the
// parent's tool.spawn_agent span — the loop's TraceContext is not
// visible to the emitter. The preferred fix is to add an
// injected parentCtx for child emitter variants; tracked
// separately in #89.
func (e *OTelTraceEmitter) emitTurnSpanLocked(turn types.TurnTrace, spanStart, spanEnd time.Time, content *turnContent) {
	attrs := []attribute.KeyValue{
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
	}
	// gen_ai.request.model is Required on chat spans per semconv, and
	// backends price generations from the span-level model — the
	// root-span copy is not enough. Prefer the router's per-turn
	// selection; fall back to the run-level configured model for legacy
	// callers that predate TurnTrace.Model. Skipped when both are empty
	// (matches the empty-attribute convention in Start).
	model := turn.Model
	if model == "" && e.config != nil {
		model = e.config.ModelRouter.Model
	}
	if model != "" {
		attrs = append(attrs, attribute.String(genAIRequestModelKey, model))
	}
	if e.config != nil {
		attrs = append(attrs, attribute.String(genAIProviderNameKey, genAIProviderName(e.config.Provider.Type)))
	}
	if content != nil {
		attrs = append(attrs, content.attributes(e.systemInstructionsJSON)...)
	}

	_, span := e.tracer.Start(e.rootCtx, fmt.Sprintf("turn[%d]", turn.Turn),
		oteltrace.WithTimestamp(spanStart),
		oteltrace.WithAttributes(attrs...),
	)
	// The loop's error paths (provider resolution, stream open,
	// mid-stream failure) all record the turn with this sentinel stop
	// reason and never produce a TurnRecord, so this is the single
	// choke point that marks failed turns for backend level filtering.
	if turn.StopReason == "error" {
		span.SetStatus(codes.Error, "error")
	}
	span.End(oteltrace.WithTimestamp(spanEnd))
}

// RecordTurnRecord attaches the turn's transcript to its span when
// content capture is opted into, and is a no-op otherwise (the
// pre-capture behaviour: the GenAI semconv marks message content
// Opt-In, and transcript recording otherwise lives on the streamed
// JSONLTraceEmitter).
//
// Scrubbing contract: the record passes through the same
// scrubTurnRecord pass the JSONL emitter applies before its
// turn_record lines, so a secret-shaped substring in message content,
// model output, or tool I/O is replaced with [REDACTED] before any
// span attribute is built. The serialised attributes follow the GenAI
// semconv message schemas; see the genAI*MessagesJSON helpers.
//
// The record is paired with the summary buffered by RecordTurn under
// (RunID, Turn). A record with no pending summary (the loop always
// summarises first; a forwarded sub-agent record could in principle
// arrive unpaired) is emitted as a content-only turn[N] span rather
// than silently dropped.
func (e *OTelTraceEmitter) RecordTurnRecord(turn types.TurnRecord) {
	// captureContent is immutable after construction: the off-path
	// reads it without the mutex and preserves the historical no-op.
	if !e.captureContent {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.rootCtx == nil {
		return
	}

	scrubbed := scrubTurnRecord(turn)
	content := &turnContent{
		inputMessages: genAIInputMessagesJSON(scrubbed.ModelInput.Messages),
	}

	// Key fields come from the scrubbed copy alongside the content so
	// the pairing and the payload share one source — pairing must not
	// silently depend on the scrubber never touching RunID/Turn.
	key := pendingTurnKey{runID: scrubbed.RunID, turn: scrubbed.Turn}
	if pending, ok := e.pendingTurns[key]; ok {
		delete(e.pendingTurns, key)
		// finish_reason comes from the paired summary so the in-message
		// value matches the gen_ai.response.finish_reasons attribute on
		// the same span (raw provider vocabulary, not the semconv enum,
		// for consistency between the two surfaces).
		content.outputMessages = genAIOutputMessagesJSON(scrubbed.ModelOutput, pending.trace.StopReason)
		e.retainRootContentLocked(scrubbed.RunID, content)
		e.emitTurnSpanLocked(pending.trace, pending.spanStart, pending.spanEnd, content)
	} else {
		content.outputMessages = genAIOutputMessagesJSON(scrubbed.ModelOutput, "")
		e.retainRootContentLocked(scrubbed.RunID, content)
		attrs := append([]attribute.KeyValue{
			attribute.Int("turn.number", scrubbed.Turn),
			attribute.String(genAIOperationNameKey, "chat"),
		}, content.attributes(e.systemInstructionsJSON)...)
		// No summary means no duration to derive timing from: the span is
		// pinned to the wall clock at delivery time (start == end). That is
		// a deliberate degraded shape for a path the loop never takes — the
		// content is preserved, the timing is honest about knowing only
		// when the record arrived.
		now := time.Now()
		_, span := e.tracer.Start(e.rootCtx, fmt.Sprintf("turn[%d]", scrubbed.Turn),
			oteltrace.WithTimestamp(now),
			oteltrace.WithAttributes(attrs...),
		)
		span.End(oteltrace.WithTimestamp(now))
	}

	// The record also carries the turn's tool transcript: pair each
	// entry with its buffered summary and emit the execute_tool spans.
	// Runs after the turn-span branch on both arms — a record that
	// missed its turn summary can still complete its tool spans.
	e.emitCapturedToolSpansLocked(scrubbed)
}

// emitCapturedToolSpansLocked emits execute_tool spans for a captured
// turn record's tool calls, merging each with the summary RecordToolCall
// buffered under (RunID, tool_use ID). The inputs are already scrubbed —
// the caller passes the scrubTurnRecord output, which covers tool Input
// and Output. Must be called with e.mu held.
//
// Entries without an ID are skipped: their plain span was already
// emitted on RecordToolCall's immediate path, and a content span here
// would double-count the call. Entries with no buffered summary (a
// forwarded sub-agent record arriving unpaired) synthesise counters from
// the record itself — unlike the unpaired-turn fallback, the record
// carries a real duration, so span timing derives from it.
func (e *OTelTraceEmitter) emitCapturedToolSpansLocked(scrubbed types.TurnRecord) {
	for _, tc := range scrubbed.ToolCalls {
		if tc.ID == "" {
			continue
		}
		content := &toolContent{
			id:        tc.ID,
			arguments: validRawJSON(tc.Input),
			result:    tc.Output,
		}
		key := pendingToolKey{runID: scrubbed.RunID, id: tc.ID}
		if pending, ok := e.pendingToolCalls[key]; ok {
			delete(e.pendingToolCalls, key)
			e.emitToolSpanLocked(pending.call, pending.spanStart, pending.spanEnd, content)
			continue
		}
		spanEnd := time.Now()
		spanStart := spanEnd.Add(-time.Duration(tc.DurationMs) * time.Millisecond)
		e.emitToolSpanLocked(types.ToolCallTrace{
			ID:           tc.ID,
			Name:         tc.Name,
			InternalName: tc.InternalName,
			DurationMs:   tc.DurationMs,
			Success:      tc.Success,
			RunID:        scrubbed.RunID,
			ParentRunID:  scrubbed.ParentRunID,
		}, spanStart, spanEnd, content)
	}
}

// retainRootContentLocked feeds a captured turn's content into the
// run-level slots stamped on the root span at Finish. Only the parent
// run's own records contribute (runID is empty exactly there; forwarded
// sub-agent records carry the child's run ID) — a sub-agent's transcript
// is its spawn_agent tool call's business, not the run's I/O. Input is
// set-once: turn 0's input messages are the seed prompt verbatim, and if
// that record never arrives, a later turn's history still embeds the
// seed. Output overwrites so the final assistant message wins; an empty
// serialisation (e.g. an aborted turn with no output blocks) never
// clobbers earlier content. Must be called with e.mu held.
func (e *OTelTraceEmitter) retainRootContentLocked(runID string, content *turnContent) {
	if runID != "" {
		return
	}
	if e.rootInputMessagesJSON == "" {
		e.rootInputMessagesJSON = content.inputMessages
	}
	if content.outputMessages != "" {
		e.rootOutputMessagesJSON = content.outputMessages
	}
}

// RecordSystemInstructions stores the run's built system prompt for
// emission as gen_ai.system_instructions on captured turn spans. The
// loop forwards it via the SystemInstructionsRecorder assertion after
// PromptBuilder.Build; with capture off nothing is stored, so the
// emitter holds no prompt content it will never emit.
//
// The prompt is scrubbed at record time — the system prompt can carry
// operator-supplied dynamic context, which is exactly the kind of
// surface a secret-shaped substring leaks through — and serialised to
// the attribute encoding once here rather than on every turn span.
func (e *OTelTraceEmitter) RecordSystemInstructions(system string) {
	if !e.captureContent {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.systemInstructionsJSON = genAISystemInstructionsJSON(security.Scrub(system))
}

// RecordFinalAssistantText stores the run's final assistant text so the
// RunTrace aggregate returned by Finish carries it for the in-process
// caller (harness factory → RunResult). Unlike RecordSystemInstructions,
// this is not gated on captureContent: the value feeds the RunResult, not
// a span attribute, so it is retained regardless of content capture. The
// loop forwards a value already scrubbed and gated by the PhasePostTurn
// guard.
func (e *OTelTraceEmitter) RecordFinalAssistantText(text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.finalAssistantText = text
}

// RecordToolCall creates a child span for a tool invocation.
//
// When content capture is on and the call carries a tool_use ID, the
// span is not emitted here: the summary is buffered (timestamps frozen)
// until the enclosing turn's RecordTurnRecord delivers the call's
// arguments and result, so one execute_tool span carries counters and
// content together. Calls without an ID are un-keyable for pairing and
// emit immediately; unmatched entries are flushed as plain spans at
// Finish.
func (e *OTelTraceEmitter) RecordToolCall(call types.ToolCallTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.toolCalls = append(e.toolCalls, call)

	if e.rootCtx == nil {
		return
	}

	spanEnd := time.Now()
	spanStart := spanEnd.Add(-time.Duration(call.DurationMs) * time.Millisecond)

	if e.captureContent && call.ID != "" {
		key := pendingToolKey{runID: call.RunID, id: call.ID}
		if prev, ok := e.pendingToolCalls[key]; ok {
			// A second summary under the same key without an intervening
			// record. Provider IDs are unique within a stream, so this is
			// defensive (mirroring the duplicate-summary handling in
			// RecordTurn): flush the stale entry as a plain span rather
			// than silently dropping it.
			e.emitToolSpanLocked(prev.call, prev.spanStart, prev.spanEnd, nil)
		}
		if e.pendingToolCalls == nil {
			e.pendingToolCalls = map[pendingToolKey]pendingToolCall{}
		}
		e.pendingToolCalls[key] = pendingToolCall{call: call, spanStart: spanStart, spanEnd: spanEnd}
		return
	}

	e.emitToolSpanLocked(call, spanStart, spanEnd, nil)
}

// emitToolSpanLocked creates and ends the execute_tool child span.
// content is nil on the no-capture path (and for flushed unmatched
// summaries), keeping the attribute set identical to the
// capture-off emitter; non-nil content appends the gen_ai.tool.call.*
// attributes after the counter attributes, mirroring
// emitTurnSpanLocked's contract. Must be called with e.mu held.
//
// The span name follows the semconv "execute_tool {gen_ai.tool.name}"
// form so each tool reads distinctly in backend trace trees — except
// for unknown-tool failures, where the name the model asked for is
// model-controlled and unbounded; emitting it as a span name would be
// the cardinality vector #309 bounded on the loop's tool.<name> spans.
// Those calls use the bare operation name, and the raw requested name
// still rides the bounded-cardinality-safe gen_ai.tool.name attribute.
func (e *OTelTraceEmitter) emitToolSpanLocked(call types.ToolCallTrace, spanStart, spanEnd time.Time, content *toolContent) {
	name := "execute_tool " + call.Name
	if call.ErrorCategory == string(observability.ToolFailureUnknownTool) {
		name = "execute_tool"
	}

	attrs := []attribute.KeyValue{
		attribute.String(genAIToolNameKey, call.Name),
		attribute.String(genAIOperationNameKey, "execute_tool"),
		attribute.Bool("tool.success", call.Success),
		attribute.Int64("tool.duration_ms", call.DurationMs),
	}
	if !call.Success && call.ErrorCategory != "" {
		attrs = append(attrs, attribute.String(errorTypeKey, call.ErrorCategory))
	}
	if content != nil {
		attrs = append(attrs, content.attributes()...)
	}

	_, span := e.tracer.Start(e.rootCtx, name,
		oteltrace.WithTimestamp(spanStart),
		oteltrace.WithAttributes(attrs...),
	)
	if !call.Success {
		// ErrorReason is scrubbed at dispatch time; the second pass here
		// is the same defence-in-depth posture the JSONL emitter applies
		// — span status strings bypass ScrubHandler.
		desc := security.Scrub(call.ErrorReason)
		if desc == "" {
			desc = "tool call failed"
		}
		span.SetStatus(codes.Error, desc)
	}
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

	// Flush turn summaries still waiting for a transcript record
	// (capture mode only; e.g. the loop's empty-stop-reason error
	// return records a summary but no record). They are emitted as
	// plain counter spans with the timestamps frozen at RecordTurn
	// time, so a missing record costs the content attributes, never
	// the turn span itself. Emission order over the map is undefined;
	// that is fine because each span carries explicit timestamps and
	// consumers order by those, not by export sequence.
	for _, pending := range e.pendingTurns {
		e.emitTurnSpanLocked(pending.trace, pending.spanStart, pending.spanEnd, nil)
	}
	e.pendingTurns = nil

	// Same flush for tool call summaries whose transcript entry never
	// arrived: a missing record costs the content attributes, never the
	// tool span itself.
	for _, pending := range e.pendingToolCalls {
		e.emitToolSpanLocked(pending.call, pending.spanStart, pending.spanEnd, nil)
	}
	e.pendingToolCalls = nil

	// Set outcome on root span and end it.
	if e.rootSpan != nil && e.rootSpan.SpanContext().IsValid() {
		e.rootSpan.SetAttributes(
			attribute.String("run.outcome", outcome),
			attribute.Int("run.turns", len(e.turns)),
			attribute.Int("run.permission_denials", e.permissionDenials),
		)
		// Stamp the run-level content retained from the parent run's
		// turn records (see retainRootContentLocked), completing the
		// root span as an invoke_agent observation. Must happen before
		// End below — the SDK silently drops attributes set afterwards.
		if e.captureContent {
			rootContent := turnContent{
				inputMessages:  e.rootInputMessagesJSON,
				outputMessages: e.rootOutputMessagesJSON,
			}
			if attrs := rootContent.attributes(e.systemInstructionsJSON); len(attrs) > 0 {
				e.rootSpan.SetAttributes(attrs...)
			}
		}
		// Every non-success outcome — including cancelled — marks the
		// root Error so backends expose one "didn't finish" predicate;
		// the run.outcome attribute (and run.cancelled_by, when set by
		// the loop) disambiguates operator-initiated cancellation from
		// failure. The outcome vocabulary is a bounded enum, so the
		// status description needs no scrubbing.
		if outcome != "success" {
			e.rootSpan.SetStatus(codes.Error, outcome)
		}
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
		ID:                 e.runID,
		Config:             redactedConfig,
		StartedAt:          e.startedAt,
		CompletedAt:        now,
		Turns:              len(e.turns),
		TokenUsage:         totalTokens,
		ToolCalls:          summaries,
		PermissionDenials:  e.permissionDenials,
		Outcome:            outcome,
		FinalAssistantText: e.finalAssistantText,
	}

	return trace, nil
}

// Probe checks OTLP exporter reachability for a dry-run preflight. It
// starts and immediately ends a throwaway span, then ForceFlushes the
// provider so the configured exporter actually attempts a connection to
// the collector. A flush error (unreachable endpoint, TLS failure, auth
// rejection) is surfaced so the preflight step fails with the exporter's
// diagnostic rather than discovering the misconfiguration only at
// run-end when the first batch is dropped.
//
// The probe span carries no run data and is not the root span, so it does
// not perturb the run trace; Start has not been called at preflight time.
// It does, however, reach the operator's live OTLP collector — this is the
// only way to confirm reachability — so it is tagged stirrup.preflight=true
// so dashboards and alert rules can filter out the synthetic span.
// Operators who want zero collector contact pass --no-probe-trace.
func (e *OTelTraceEmitter) Probe(ctx context.Context) error {
	if e.provider == nil {
		return nil
	}
	_, span := e.tracer.Start(ctx, "stirrup.preflight.probe",
		oteltrace.WithAttributes(attribute.Bool("stirrup.preflight", true)))
	span.End()
	if err := e.provider.ForceFlush(ctx); err != nil {
		return fmt.Errorf("OTLP exporter flush failed: %w", err)
	}
	return nil
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
