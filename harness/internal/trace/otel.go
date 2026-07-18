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

// OpenTelemetry GenAI semantic-convention attribute keys. Stirrup-specific
// attributes with no GenAI counterpart keep stirrup-prefixed names.
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

	// Message-content attributes, emitted only when captureContent is on;
	// values are JSON-serialised per the GenAI semconv message schemas
	// (the Go attribute API has no structured "any" type).
	genAIInputMessagesKey      = "gen_ai.input.messages"
	genAIOutputMessagesKey     = "gen_ai.output.messages"
	genAISystemInstructionsKey = "gen_ai.system_instructions"

	// Tool-call content attributes, capture-gated like message content.
	// The call ID correlates the span with the turn's message attributes.
	genAIToolCallIDKey        = "gen_ai.tool.call.id"
	genAIToolCallArgumentsKey = "gen_ai.tool.call.arguments"
	genAIToolCallResultKey    = "gen_ai.tool.call.result"

	// Prompt-resolution attributes, stirrup-specific (no GenAI counterpart).
	promptModelKey = "prompt.model"
	promptTierKey  = "prompt.tier"
)

// genAIProviderName maps stirrup provider type strings to the OTel GenAI
// `gen_ai.provider.name` enum values. Unknown types (including
// `openai-compatible`, the generic adapter for vLLM/Ollama/Azure
// OpenAI/etc.) fall through to the raw value rather than mislabelling
// telemetry as a specific vendor.
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
	// content on turn spans via the GenAI semconv attributes. Immutable
	// after construction, so the off-path methods can read it without
	// the mutex.
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
	// pre-serialised once by RecordSystemInstructions rather than per
	// turn span. Empty when capture is off or no prompt was forwarded.
	systemInstructionsJSON string

	// rootInputMessagesJSON / rootOutputMessagesJSON are the parent run's
	// own turn content (forwarded sub-agent records never contribute),
	// stamped on the root span at Finish. Input is set-once (turn 0's
	// seed prompt); output is overwrite-always so the final assistant
	// message wins. Empty when capture is off.
	rootInputMessagesJSON  string
	rootOutputMessagesJSON string

	// pendingTurns buffers turn summaries between RecordTurn and the
	// matching RecordTurnRecord while capture is on, so one turn[N] span
	// carries counters and content together. Keyed by (RunID, Turn) so a
	// sub-agent's turn N never merges onto the parent's. Unmatched
	// entries are flushed as plain spans at Finish. Nil when capture is
	// off.
	pendingTurns map[pendingTurnKey]pendingTurn

	// pendingToolCalls is the tool-call analogue of pendingTurns, keyed
	// by (RunID, tool_use ID). Calls without an ID are un-keyable and
	// emit immediately instead of buffering.
	pendingToolCalls map[pendingToolKey]pendingToolCall
}

// Compile-time interface satisfaction guards: the loop discovers the
// content-capture system-prompt hook through the optional
// SystemInstructionsRecorder assertion, so losing the method would be
// a silent regression rather than a build break without this.
var (
	_ TraceEmitter               = (*OTelTraceEmitter)(nil)
	_ SystemInstructionsRecorder = (*OTelTraceEmitter)(nil)
	_ PromptResolutionRecorder   = (*OTelTraceEmitter)(nil)
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
// the given OTLP endpoint over the chosen wire protocol ("" or "grpc" for
// OTLP/gRPC, "http/protobuf" for OTLP/HTTP; see docs/observability-cloud.md
// for endpoint/TLS/header resolution). The caller must eventually call
// Finish to flush and shut down the exporter. headers must already have
// "secret://" references resolved to plaintext.
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
// returns the matching OTel SDK trace exporter.
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
		// WithEndpointURL would parse the full URL in one call but isn't
		// available in v1.43.0; emulate by stripping the scheme for the
		// host component and re-applying the path below.
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

// isInsecureEndpoint returns true when the endpoint should use plain
// HTTP: an "http://" scheme or no scheme at all (the local-collector
// case). "https://" always means TLS.
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
// captureContent must not be mutated post-construction, as in production.
func newOTelTraceEmitterForTest(tp *sdktrace.TracerProvider, captureContent bool) *OTelTraceEmitter {
	return &OTelTraceEmitter{
		provider:       tp,
		tracer:         tp.Tracer("stirrup-harness"),
		captureContent: captureContent,
	}
}

// NewOTelTraceEmitterForTest lets tests in other packages build an
// OTelTraceEmitter around an in-memory TracerProvider without an OTLP
// collector. Content capture stays off. Not intended for production use.
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
	// gen_ai.agent.id and the semconv "invoke_agent {gen_ai.agent.name}"
	// span name are not emitted: stirrup has no first-class named-agent
	// identity, and backends type observations from the operation-name
	// attribute rather than the span name.
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
			// gen_ai.provider.name is the semconv enum value.
			attribute.String(genAIProviderNameKey, genAIProviderName(config.Provider.Type)),
		)
		if config.ModelRouter.Model != "" {
			span.SetAttributes(
				attribute.String(genAIRequestModelKey, config.ModelRouter.Model),
			)
		}
		if config.SessionName != "" {
			// Skipped when empty so we don't stamp an empty attribute.
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
// TODO(#89): turn[N] is parented off e.rootCtx (context.Background()),
// so child sub-agent turn[N] spans don't nest under the parent's
// tool.spawn_agent span. Fix: inject a parentCtx for child emitters.
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
	// gen_ai.request.model is Required per semconv; prefer the per-turn
	// selection, falling back to the run-level model for legacy callers.
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
// content capture is opted into; a no-op otherwise, since the GenAI
// semconv marks message content Opt-In.
//
// The record passes through the same scrubTurnRecord pass the JSONL
// emitter applies before any span attribute is built.
//
// The record pairs with the summary buffered by RecordTurn under
// (RunID, Turn); an unpaired record (a forwarded sub-agent record
// arriving out of order) emits a content-only turn[N] span instead of
// being dropped.
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

	key := pendingTurnKey{runID: scrubbed.RunID, turn: scrubbed.Turn}
	if pending, ok := e.pendingTurns[key]; ok {
		delete(e.pendingTurns, key)
		// finish_reason comes from the paired summary so it matches the
		// gen_ai.response.finish_reasons attribute on the same span.
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
		// No summary means no duration to derive timing from: pin the
		// span to the wall clock at delivery time (start == end).
		now := time.Now()
		_, span := e.tracer.Start(e.rootCtx, fmt.Sprintf("turn[%d]", scrubbed.Turn),
			oteltrace.WithTimestamp(now),
			oteltrace.WithAttributes(attrs...),
		)
		span.End(oteltrace.WithTimestamp(now))
	}

	// The record also carries the turn's tool transcript: pair each
	// entry with its buffered summary and emit the execute_tool spans.
	e.emitCapturedToolSpansLocked(scrubbed)
}

// emitCapturedToolSpansLocked emits execute_tool spans for a captured
// turn record's tool calls, merging each with the summary buffered by
// RecordToolCall under (RunID, tool_use ID). Must be called with e.mu
// held.
//
// Entries without an ID are skipped: their plain span was already
// emitted on RecordToolCall's immediate path, and a content span here
// would double-count the call. Entries with no buffered summary
// synthesise counters from the record itself, which carries a real
// duration unlike the unpaired-turn fallback.
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
// run's own records contribute (empty runID); forwarded sub-agent
// records are the spawn_agent tool call's business, not the run's I/O.
// Input is set-once (turn 0's seed prompt); output overwrites, and an
// empty serialisation never clobbers earlier content. Must be called
// with e.mu held.
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

// RecordSystemInstructions stores the run's built system prompt,
// scrubbed and serialised once, for emission as gen_ai.system_instructions
// on captured turn spans. With capture off nothing is stored.
func (e *OTelTraceEmitter) RecordSystemInstructions(system string) {
	if !e.captureContent {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.systemInstructionsJSON = genAISystemInstructionsJSON(security.Scrub(system))
}

// RecordPromptResolution sets the resolved prompt model and tier on the
// root span. Always-on, unlike RecordSystemInstructions: the values are
// config metadata, not message content.
func (e *OTelTraceEmitter) RecordPromptResolution(model, tier string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rootSpan == nil || !e.rootSpan.SpanContext().IsValid() {
		return
	}
	e.rootSpan.SetAttributes(
		attribute.String(promptModelKey, model),
		attribute.String(promptTierKey, tier),
	)
}

// RecordFinalAssistantText stores the run's final assistant text so the
// RunTrace aggregate returned by Finish carries it. Unlike
// RecordSystemInstructions, this is not gated on captureContent: the
// value feeds the RunResult, not a span attribute.
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
			// record; defensive (mirroring RecordTurn). Flush the stale
			// entry as a plain span rather than dropping it.
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

// emitToolSpanLocked creates and ends the execute_tool child span,
// mirroring emitTurnSpanLocked's nil-content contract. Must be called
// with e.mu held.
//
// The span name follows the semconv "execute_tool {gen_ai.tool.name}"
// form, except for unknown-tool failures: the model-requested name is
// unbounded, so those spans use the bare operation name and carry the
// raw name only in the bounded-cardinality-safe gen_ai.tool.name
// attribute.
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
		// is defence-in-depth since span status strings bypass ScrubHandler.
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

	// Flush turn summaries still waiting for a transcript record (e.g.
	// the loop's empty-stop-reason error return). Emitted as plain
	// counter spans with timestamps frozen at RecordTurn time; a missing
	// record costs the content attributes, never the span itself. Map
	// emission order is fine because each span carries its own explicit
	// timestamps.
	for _, pending := range e.pendingTurns {
		e.emitTurnSpanLocked(pending.trace, pending.spanStart, pending.spanEnd, nil)
	}
	e.pendingTurns = nil

	// Same flush for tool call summaries whose transcript entry never
	// arrived.
	for _, pending := range e.pendingToolCalls {
		e.emitToolSpanLocked(pending.call, pending.spanStart, pending.spanEnd, nil)
	}
	e.pendingToolCalls = nil

	if e.rootSpan != nil && e.rootSpan.SpanContext().IsValid() {
		e.rootSpan.SetAttributes(
			attribute.String("run.outcome", outcome),
			attribute.Int("run.turns", len(e.turns)),
			attribute.Int("run.permission_denials", e.permissionDenials),
		)
		// Must happen before End below — the SDK silently drops
		// attributes set afterwards.
		if e.captureContent {
			rootContent := turnContent{
				inputMessages:  e.rootInputMessagesJSON,
				outputMessages: e.rootOutputMessagesJSON,
			}
			if attrs := rootContent.attributes(e.systemInstructionsJSON); len(attrs) > 0 {
				e.rootSpan.SetAttributes(attrs...)
			}
		}
		// Every non-success outcome marks the root Error so backends
		// expose one "didn't finish" predicate; run.outcome disambiguates
		// cancellation from failure. Outcome is a bounded enum, so the
		// status description needs no scrubbing.
		if outcome != "success" {
			e.rootSpan.SetStatus(codes.Error, outcome)
		}
		e.rootSpan.End()
	}

	if e.provider != nil {
		if err := e.provider.ForceFlush(ctx); err != nil {
			// Non-fatal: the RunTrace aggregate below is still valid for
			// the caller even though the OTel backend will be missing
			// the tail of this run.
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

// Probe checks OTLP exporter reachability for a dry-run preflight (see
// docs/configuration.md#dry-run-preflight) by starting and ForceFlushing
// a throwaway span tagged stirrup.preflight=true, so a flush error
// surfaces the exporter's own diagnostic rather than only at run-end.
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
