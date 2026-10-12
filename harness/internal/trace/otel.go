package trace

import (
	"context"
	"encoding/json"
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
	genAIResponseModelKey  = "gen_ai.response.model"
	genAIConversationIDKey = "gen_ai.conversation.id"
	genAIOperationNameKey  = "gen_ai.operation.name"
	genAIUsageInputTokens  = "gen_ai.usage.input_tokens"
	genAIUsageOutputTokens = "gen_ai.usage.output_tokens"
	genAIFinishReasonsKey  = "gen_ai.response.finish_reasons"
	genAIToolNameKey       = "gen_ai.tool.name"
	genAIAgentNameKey      = "gen_ai.agent.name"

	// Usage breakdown attributes, emitted on a turn span only when
	// non-zero so providers that report no breakdown keep the base set.
	genAIUsageCacheReadInputTokens     = "gen_ai.usage.cache_read.input_tokens"
	genAIUsageCacheCreationInputTokens = "gen_ai.usage.cache_creation.input_tokens"
	genAIUsageReasoningOutputTokens    = "gen_ai.usage.reasoning.output_tokens"
	tokensInputReportedKey             = "stirrup.tokens.input_reported"

	// errorTypeKey is the stable (non-Development) semconv error
	// attribute. Every value is from a bounded vocabulary: the run
	// outcome on agent spans, observability.ToolFailureCategory on tool
	// spans.
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

// OTelTraceEmitter records harness run telemetry as OpenTelemetry spans,
// exported via OTLP/gRPC to a collector endpoint.
type OTelTraceEmitter struct {
	provider *sdktrace.TracerProvider
	tracer   oteltrace.Tracer
	scope    *runScopeProcessor

	// captureContent opts the emitter into recording prompt/completion
	// content on turn spans via the GenAI semconv attributes. Immutable
	// after construction, so the off-path methods can read it without
	// the mutex.
	captureContent bool

	// redactionDisabled is the --debug bit, immutable after construction;
	// see JSONLTraceEmitter.redactionDisabled for the shared contract. Set
	// only by the factory, and only under a debug build.
	redactionDisabled bool

	mu                   sync.Mutex
	runID                string
	config               *types.RunConfig
	startedAt            time.Time
	rootSpan             oteltrace.Span
	rootCtx              context.Context
	turns                []types.TurnTrace
	toolCalls            []types.ToolCallTrace
	permissionDenials    int
	finalAssistantText   string
	stopDetails          *types.StopDetails
	commandOutputArchive string
	gitHandoff           *types.GitHandoff
	workspaceExport      *types.ExportReceipt
	runError             string
	structuredOutput     json.RawMessage

	// systemInstructionsJSON is the run's system prompt, scrubbed and
	// pre-serialised once by RecordSystemInstructions rather than per
	// turn span. Empty when capture is off or no prompt was forwarded.
	systemInstructionsJSON string

	// runContent is each run's own I/O, keyed by run ID ("" for the
	// top-level run): stamped on the root span at Finish and on a
	// sub-agent span at its End. Empty when capture is off.
	runContent map[string]*turnContent

	// open holds live span handles not yet ended, so Finish can end any
	// the loop left open.
	open map[liveHandle]struct{}

	// pendingToolCalls buffers tool call summaries between RecordToolCall
	// and the enclosing turn's RecordTurnRecord while capture is on, keyed
	// by (RunID, tool_use ID). Calls without an ID are un-keyable and emit
	// immediately instead of buffering.
	pendingToolCalls map[pendingToolKey]pendingToolCall
}

// Compile-time interface satisfaction guards: the loop discovers the
// content-capture system-prompt hook through the optional
// SystemInstructionsRecorder assertion, so losing the method would be
// a silent regression rather than a build break without this.
var (
	_ TraceEmitter                 = (*OTelTraceEmitter)(nil)
	_ SystemInstructionsRecorder   = (*OTelTraceEmitter)(nil)
	_ PromptResolutionRecorder     = (*OTelTraceEmitter)(nil)
	_ FinalAssistantTextRecorder   = (*OTelTraceEmitter)(nil)
	_ StopDetailsRecorder          = (*OTelTraceEmitter)(nil)
	_ CommandOutputRecorder        = (*OTelTraceEmitter)(nil)
	_ CommandOutputArchiveRecorder = (*OTelTraceEmitter)(nil)
	_ GitHandoffRecorder           = (*OTelTraceEmitter)(nil)
	_ WorkspaceExportRecorder      = (*OTelTraceEmitter)(nil)
	_ RunErrorRecorder             = (*OTelTraceEmitter)(nil)
	_ StructuredOutputRecorder     = (*OTelTraceEmitter)(nil)
	_ RootContextProvider          = (*OTelTraceEmitter)(nil)
)

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
//
// redactionDisabled is the --debug bypass: production callers pass the
// factory's already debugbuild-gated value, every other caller passes false.
func NewOTelTraceEmitter(ctx context.Context, endpoint, protocol string, headers map[string]string, resourceOpts observability.ResourceOptions, captureContent, redactionDisabled bool, opts ...OTelOption) (*OTelTraceEmitter, error) {
	exporter, err := buildOTLPTraceExporter(ctx, endpoint, protocol, headers)
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(observability.BuildResource(resourceOpts)),
	)
	return newOTelTraceEmitter(tp, captureContent, redactionDisabled, opts...), nil
}

// newOTelTraceEmitter registers the run-scope processor, then any extra
// processors, on tp and builds the emitter around it.
func newOTelTraceEmitter(tp *sdktrace.TracerProvider, captureContent, redactionDisabled bool, opts ...OTelOption) *OTelTraceEmitter {
	var o otelOptions
	for _, opt := range opts {
		opt(&o)
	}
	scope := newRunScopeProcessor()
	tp.RegisterSpanProcessor(scope)
	for _, p := range o.spanProcessors {
		tp.RegisterSpanProcessor(p)
	}
	return &OTelTraceEmitter{
		provider:          tp,
		tracer:            tp.Tracer("stirrup-harness"),
		scope:             scope,
		captureContent:    captureContent,
		redactionDisabled: redactionDisabled,
	}
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
func newOTelTraceEmitterForTest(tp *sdktrace.TracerProvider, captureContent bool, opts ...OTelOption) *OTelTraceEmitter {
	return newOTelTraceEmitter(tp, captureContent, false, opts...)
}

// NewOTelTraceEmitterForTest lets tests in other packages build an
// OTelTraceEmitter around an in-memory TracerProvider without an OTLP
// collector. Content capture stays off. Not intended for production use.
func NewOTelTraceEmitterForTest(tp *sdktrace.TracerProvider) *OTelTraceEmitter {
	return newOTelTraceEmitterForTest(tp, false)
}

// Start initialises a new trace with the given run ID and configuration.
// It creates the root "invoke_agent stirrup" span.
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
	e.stopDetails = nil
	e.gitHandoff = nil
	e.workspaceExport = nil
	e.runError = ""
	e.structuredOutput = nil
	e.systemInstructionsJSON = ""
	e.runContent = nil
	e.open = nil
	e.pendingToolCalls = nil

	conversationID := ""
	if config != nil {
		conversationID = config.SessionName
	}
	e.scope.setRun(conversationID)

	attrs := []attribute.KeyValue{
		attribute.String("run.id", runID),
		attribute.String("harness.version", version.Version()),
	}
	attrs = append(attrs, agentSpanAttributes(agentNameRun, config)...)
	ctx, span := e.tracer.Start(context.Background(), "invoke_agent "+agentNameRun,
		oteltrace.WithAttributes(attrs...),
	)

	e.rootSpan = span
	e.rootCtx = ctx
}

// RecordTurn appends a turn summary to the run aggregate. Model requests
// are recorded as spans through StartChatSpan.
func (e *OTelTraceEmitter) RecordTurn(turn types.TurnTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.turns = append(e.turns, turn)
}

// agentSpanAttributes returns the creation-time attributes shared by the
// run span and sub-agent spans. gen_ai.agent.name is set at creation so
// the run-scope processor's OnStart sees it and descendants inherit it.
func agentSpanAttributes(agentName string, config *types.RunConfig) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(genAIOperationNameKey, "invoke_agent"),
		attribute.String(genAIAgentNameKey, agentName),
	}
	if config == nil {
		return attrs
	}
	attrs = append(attrs,
		attribute.String("run.mode", config.Mode),
		attribute.String(genAIProviderNameKey, observability.GenAIProviderName(config.Provider.Type)),
	)
	if config.ModelRouter.Model != "" {
		attrs = append(attrs, attribute.String(genAIRequestModelKey, config.ModelRouter.Model))
	}
	return attrs
}

// turnUsageBreakdownAttributes returns the optional per-turn usage
// attributes, omitting each one that is zero or false.
func turnUsageBreakdownAttributes(turn types.TurnTrace) []attribute.KeyValue {
	var attrs []attribute.KeyValue
	if turn.InputReported {
		attrs = append(attrs, attribute.Bool(tokensInputReportedKey, true))
	}
	if turn.Tokens.CacheRead > 0 {
		attrs = append(attrs, attribute.Int(genAIUsageCacheReadInputTokens, turn.Tokens.CacheRead))
	}
	if turn.Tokens.CacheWrite > 0 {
		attrs = append(attrs, attribute.Int(genAIUsageCacheCreationInputTokens, turn.Tokens.CacheWrite))
	}
	if turn.Tokens.Reasoning > 0 {
		attrs = append(attrs, attribute.Int(genAIUsageReasoningOutputTokens, turn.Tokens.Reasoning))
	}
	return attrs
}

// RecordTurnRecord attaches each captured tool call's arguments and
// result to its execute_tool span; a no-op without content capture. The
// request's own content reaches its chat span through ChatSpan.End.
func (e *OTelTraceEmitter) RecordTurnRecord(turn types.TurnRecord) {
	if !e.captureContent {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.rootCtx == nil {
		return
	}
	e.emitCapturedToolSpansLocked(scrubTurnRecord(turn, e.redactionDisabled))
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

// retainRunContentLocked feeds a captured request's content into its
// run's I/O slot. Input is set-once (the first request's seed prompt);
// output overwrites so the final assistant message wins, and an empty
// serialisation never clobbers earlier content. Must be called with e.mu
// held.
func (e *OTelTraceEmitter) retainRunContentLocked(runID string, content *turnContent) {
	if e.runContent == nil {
		e.runContent = map[string]*turnContent{}
	}
	run, ok := e.runContent[runID]
	if !ok {
		run = &turnContent{}
		e.runContent[runID] = run
	}
	if run.inputMessages == "" {
		run.inputMessages = content.inputMessages
	}
	if content.outputMessages != "" {
		run.outputMessages = content.outputMessages
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
	e.systemInstructionsJSON = genAISystemInstructionsJSON(scrubOrPass(system, e.redactionDisabled))
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

// RecordStopDetails stores the run's final stop details so the RunTrace
// aggregate returned by Finish carries them.
func (e *OTelTraceEmitter) RecordStopDetails(details *types.StopDetails) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopDetails = details
}

func (e *OTelTraceEmitter) RecordCommandOutput(record types.CommandOutputRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rootCtx == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("command_output.archive_id", record.ArchiveID),
		attribute.String("command_output.tool_use_id", record.ToolUseID),
		attribute.Bool("command_output.capture_complete", record.CaptureComplete),
		attribute.Int64("command_output.stdout.raw_bytes", record.Stdout.RawBytes),
		attribute.Int64("command_output.stdout.scrubbed_bytes", record.Stdout.ScrubbedBytes),
		attribute.String("command_output.stdout.raw_sha256", record.Stdout.RawSHA256),
		attribute.String("command_output.stdout.scrubbed_sha256", record.Stdout.ScrubbedSHA256),
		attribute.Int64("command_output.stderr.raw_bytes", record.Stderr.RawBytes),
		attribute.Int64("command_output.stderr.scrubbed_bytes", record.Stderr.ScrubbedBytes),
		attribute.String("command_output.stderr.raw_sha256", record.Stderr.RawSHA256),
		attribute.String("command_output.stderr.scrubbed_sha256", record.Stderr.ScrubbedSHA256),
	}
	_, span := e.tracer.Start(e.rootCtx, "command_output", oteltrace.WithAttributes(attrs...))
	if !record.CaptureComplete {
		span.SetStatus(codes.Error, "command output capture incomplete")
	}
	span.End()
}

// RecordGitHandoff stores the run's git handoff for the RunTrace Finish
// builds.
func (e *OTelTraceEmitter) RecordGitHandoff(handoff types.GitHandoff) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gitHandoff = &handoff
}

// RecordWorkspaceExport stores the run's workspace export receipt for the
// RunTrace Finish builds.
func (e *OTelTraceEmitter) RecordWorkspaceExport(receipt types.ExportReceipt) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.workspaceExport = &receipt
}

// RecordRunError stores the run's error text for the RunTrace Finish
// builds.
func (e *OTelTraceEmitter) RecordRunError(text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runError = text
}

// RecordStructuredOutput stores the run's validated structured answer for
// the RunTrace Finish builds.
func (e *OTelTraceEmitter) RecordStructuredOutput(doc json.RawMessage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.structuredOutput = append(json.RawMessage(nil), doc...)
}

func (e *OTelTraceEmitter) RecordCommandOutputArchive(location string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.commandOutputArchive = location
	if e.rootSpan != nil {
		e.rootSpan.SetAttributes(attribute.String("command_output.archive_uri", location))
	}
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
	if call.ID != "" {
		attrs = append(attrs, attribute.String(genAIToolCallIDKey, scrubOrPass(call.ID, e.redactionDisabled)))
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
		desc := scrubOrPass(call.ErrorReason, e.redactionDisabled)
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
	// An SDK span that never ends is never exported, so a handle the loop
	// left open (after a panic, say) is ended here and marked.
	for _, h := range e.takeOpenHandles() {
		h.abandon()
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()

	// Flush tool call summaries whose transcript entry never arrived.
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
			rootContent := e.runContent[""]
			if rootContent == nil {
				rootContent = &turnContent{}
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
			e.rootSpan.SetAttributes(attribute.String(errorTypeKey, outcome))
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
		totalTokens.Add(turn.Tokens)
	}

	summaries := make([]types.ToolCallSummary, len(e.toolCalls))
	for i, tc := range e.toolCalls {
		summaries[i] = types.ToolCallSummary(tc)
	}

	var redactedConfig types.RunConfig
	if e.config != nil {
		if e.redactionDisabled {
			redactedConfig = *e.config
		} else {
			redactedConfig = e.config.Redact()
		}
	}

	trace := &types.RunTrace{
		ID:                   e.runID,
		Config:               redactedConfig,
		StartedAt:            e.startedAt,
		CompletedAt:          now,
		Turns:                len(e.turns),
		TokenUsage:           totalTokens,
		ToolCalls:            summaries,
		PermissionDenials:    e.permissionDenials,
		Outcome:              outcome,
		StopDetails:          e.stopDetails,
		FinalAssistantText:   e.finalAssistantText,
		CommandOutputArchive: e.commandOutputArchive,
		Git:                  e.gitHandoff,
		WorkspaceExport:      e.workspaceExport,
		Error:                e.runError,
		StructuredOutput:     e.structuredOutput,
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

// RootContext returns the context containing the root run span, for use
// as the parent of loop-level spans, or nil before Start.
func (e *OTelTraceEmitter) RootContext() context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rootCtx
}

// Close shuts down the tracer provider and exporter.
func (e *OTelTraceEmitter) Close() error {
	if e.provider == nil {
		return nil
	}
	return e.provider.Shutdown(context.Background())
}
