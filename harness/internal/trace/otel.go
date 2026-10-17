package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/observability/otlp"
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

	// Usage breakdown attributes, emitted on a chat span only when
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

	// Repository-guidance attributes, stirrup-specific. Parallel slices,
	// one element per configured file in render order.
	repositoryGuidancePathsKey         = "repository_guidance.paths"
	repositoryGuidanceTiersKey         = "repository_guidance.tiers"
	repositoryGuidanceMissingKey       = "repository_guidance.missing"
	repositoryGuidanceBytesKey         = "repository_guidance.bytes"
	repositoryGuidanceRenderedBytesKey = "repository_guidance.rendered_bytes"
	repositoryGuidanceTruncatedKey     = "repository_guidance.truncated"
	repositoryGuidanceSHA256Key        = "repository_guidance.sha256"
)

// OTelTraceEmitter records harness run telemetry as OpenTelemetry spans,
// exported via OTLP to a collector endpoint.
type OTelTraceEmitter struct {
	provider *sdktrace.TracerProvider
	tracer   oteltrace.Tracer
	scope    *runScopeProcessor

	// captureContent opts the emitter into recording prompt/completion
	// content on chat spans via the GenAI semconv attributes. Immutable
	// after construction, so the off-path methods can read it without
	// the mutex.
	captureContent bool

	// captureContentMaxBytes caps each content attribute value; 0 means
	// no cap. Immutable after construction, like captureContent.
	captureContentMaxBytes int

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
	verificationResults  []types.VerificationResult
	finalAssistantText   string
	stopDetails          *types.StopDetails
	commandOutputArchive string
	gitHandoff           *types.GitHandoff
	workspaceExport      *types.ExportReceipt
	runError             string
	// lastChatErrorType is the error.type of the run's most recent
	// top-level chat span; empty when that request succeeded.
	lastChatErrorType    string
	structuredOutput     json.RawMessage
	degradedCapabilities []types.DegradedCapability
	repositoryGuidance   []types.RepositoryGuidanceRecord
	auxiliaryTokens      types.TokenUsage
	externalGuardTokens  types.TokenUsage
	postEditChecks       []types.PostEditCheckRecord

	// systemInstructionsJSON is the run's system prompt, scrubbed and
	// pre-serialised once by RecordSystemInstructions rather than per
	// chat span. Empty when capture is off or no prompt was forwarded.
	systemInstructionsJSON string

	// runContent is each run's own I/O, keyed by run ID ("" for the
	// top-level run): stamped on the root span at Finish and on a
	// sub-agent span at its End. Empty when capture is off.
	runContent map[string]*turnContent

	// open holds live span handles not yet ended, so Finish can end any
	// the loop left open.
	open map[liveHandle]struct{}
}

// Compile-time interface satisfaction guards: the loop discovers the
// content-capture system-prompt hook through the optional
// SystemInstructionsRecorder assertion, so losing the method would be
// a silent regression rather than a build break without this.
var (
	_ TraceEmitter                 = (*OTelTraceEmitter)(nil)
	_ SystemInstructionsRecorder   = (*OTelTraceEmitter)(nil)
	_ PromptResolutionRecorder     = (*OTelTraceEmitter)(nil)
	_ RepositoryGuidanceRecorder   = (*OTelTraceEmitter)(nil)
	_ FinalAssistantTextRecorder   = (*OTelTraceEmitter)(nil)
	_ StopDetailsRecorder          = (*OTelTraceEmitter)(nil)
	_ DegradedCapabilitiesRecorder = (*OTelTraceEmitter)(nil)
	_ CommandOutputRecorder        = (*OTelTraceEmitter)(nil)
	_ CommandOutputArchiveRecorder = (*OTelTraceEmitter)(nil)
	_ GitHandoffRecorder           = (*OTelTraceEmitter)(nil)
	_ WorkspaceExportRecorder      = (*OTelTraceEmitter)(nil)
	_ RunErrorRecorder             = (*OTelTraceEmitter)(nil)
	_ StructuredOutputRecorder     = (*OTelTraceEmitter)(nil)
	_ RootContextProvider          = (*OTelTraceEmitter)(nil)
	_ TraceReferenceProvider       = (*OTelTraceEmitter)(nil)
	_ VerificationRecorder         = (*OTelTraceEmitter)(nil)
	_ LedgerUsageRecorder          = (*OTelTraceEmitter)(nil)
	_ PostEditCheckRecorder        = (*OTelTraceEmitter)(nil)
)

// OTelTraceEmitterOptions configures NewOTelTraceEmitter.
type OTelTraceEmitterOptions struct {
	// Exporter selects the OTLP endpoint, protocol, headers, and HTTP
	// client for the span exporter.
	Exporter otlp.Settings

	// Resource carries the run-scoped resource attributes shared with the
	// metric and log signals.
	Resource observability.ResourceOptions

	// CaptureContent opts into recording prompt and completion content on
	// spans; see types.TraceEmitterConfig.CaptureContent.
	CaptureContent bool

	// CaptureContentMaxBytes caps each captured content attribute value;
	// 0 means no cap. See types.TraceEmitterConfig.CaptureContentMaxBytes.
	CaptureContentMaxBytes int

	// RedactionDisabled is the --debug bypass: production callers pass the
	// factory's already debugbuild-gated value, every other caller false.
	RedactionDisabled bool

	// WrapSpanExporter, when set, decorates the OTLP exporter before it is
	// registered with the batcher, so the decorator sees every span's
	// final attributes at export time.
	WrapSpanExporter func(sdktrace.SpanExporter) sdktrace.SpanExporter
}

// NewOTelTraceEmitter creates an OTel trace emitter that exports spans over
// OTLP as described by opts.Exporter (see docs/observability-cloud.md for
// endpoint/TLS/header resolution). The caller must eventually call Finish
// to flush and shut down the exporter.
func NewOTelTraceEmitter(ctx context.Context, opts OTelTraceEmitterOptions, extra ...OTelOption) (*OTelTraceEmitter, error) {
	exporter, err := otlp.NewTraceExporter(ctx, opts.Exporter)
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}
	if opts.WrapSpanExporter != nil {
		exporter = opts.WrapSpanExporter(exporter)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(observability.BuildResource(opts.Resource)),
	)
	return newOTelTraceEmitter(tp, opts.CaptureContent, opts.CaptureContentMaxBytes, opts.RedactionDisabled, extra...), nil
}

// newOTelTraceEmitter registers the run-scope processor, then any extra
// processors, on tp and builds the emitter around it.
func newOTelTraceEmitter(tp *sdktrace.TracerProvider, captureContent bool, captureContentMaxBytes int, redactionDisabled bool, opts ...OTelOption) *OTelTraceEmitter {
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
		provider:               tp,
		tracer:                 tp.Tracer("stirrup-harness"),
		scope:                  scope,
		captureContent:         captureContent,
		captureContentMaxBytes: captureContentMaxBytes,
		redactionDisabled:      redactionDisabled,
	}
}

// newOTelTraceEmitterForTest creates an OTel trace emitter backed by the
// given TracerProvider, used in tests to capture spans in-memory.
// captureContent must not be mutated post-construction, as in production.
func newOTelTraceEmitterForTest(tp *sdktrace.TracerProvider, captureContent bool, opts ...OTelOption) *OTelTraceEmitter {
	return newOTelTraceEmitter(tp, captureContent, 0, false, opts...)
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
	e.start(context.Background(), runID, config)
}

var _ ContextStarter = (*OTelTraceEmitter)(nil)

// StartWithContext is Start with the run span parented on the span
// context ctx carries, or a new trace when that is invalid. Only the span
// context is copied: RootContext carries no cancellation or deadline from
// ctx, so post-run spans still record after the caller's ctx ends.
func (e *OTelTraceEmitter) StartWithContext(ctx context.Context, runID string, config *types.RunConfig) {
	e.start(oteltrace.ContextWithSpanContext(context.Background(), oteltrace.SpanContextFromContext(ctx)), runID, config)
}

func (e *OTelTraceEmitter) start(parent context.Context, runID string, config *types.RunConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.runID = runID
	e.config = config
	e.startedAt = time.Now()
	e.turns = nil
	e.toolCalls = nil
	e.permissionDenials = 0
	e.verificationResults = nil
	e.finalAssistantText = ""
	e.stopDetails = nil
	e.gitHandoff = nil
	e.workspaceExport = nil
	e.runError = ""
	e.lastChatErrorType = ""
	e.structuredOutput = nil
	e.degradedCapabilities = nil
	e.repositoryGuidance = nil
	e.auxiliaryTokens = types.TokenUsage{}
	e.externalGuardTokens = types.TokenUsage{}
	e.systemInstructionsJSON = ""
	e.runContent = nil
	e.open = nil
	e.postEditChecks = nil

	e.scope.setRun(runIdentityFor(config, e.redactionDisabled))

	attrs := []attribute.KeyValue{
		attribute.String("run.id", runID),
		attribute.String("harness.version", version.Version()),
	}
	attrs = append(attrs, agentSpanAttributes(agentNameRun, config)...)
	ctx, span := e.tracer.Start(parent, "invoke_agent "+agentNameRun,
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

// RecordTurnRecord is a no-op: request and tool content reach their
// spans through ChatSpan.End and ToolSpan.End.
func (e *OTelTraceEmitter) RecordTurnRecord(types.TurnRecord) {}

// RecordSystemInstructions stores the run's built system prompt,
// scrubbed and serialised once, for emission as gen_ai.system_instructions
// on captured chat spans. With capture off nothing is stored.
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

// RecordRepositoryGuidance sets the loaded guidance metadata on the root
// span and keeps it for the RunTrace aggregate returned by Finish.
func (e *OTelTraceEmitter) RecordRepositoryGuidance(records []types.RepositoryGuidanceRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.repositoryGuidance = append([]types.RepositoryGuidanceRecord(nil), records...)
	if e.rootSpan == nil || !e.rootSpan.SpanContext().IsValid() || len(records) == 0 {
		return
	}
	n := len(records)
	paths, tiers, digests := make([]string, n), make([]string, n), make([]string, n)
	bytes, rendered := make([]int, n), make([]int, n)
	missing, truncated := make([]bool, n), make([]bool, n)
	for i, r := range records {
		paths[i], tiers[i], digests[i] = r.Path, r.Tier, r.SHA256
		bytes[i], rendered[i] = r.Bytes, r.RenderedBytes
		missing[i], truncated[i] = r.Missing, r.Truncated
	}
	e.rootSpan.SetAttributes(
		attribute.StringSlice(repositoryGuidancePathsKey, paths),
		attribute.StringSlice(repositoryGuidanceTiersKey, tiers),
		attribute.BoolSlice(repositoryGuidanceMissingKey, missing),
		attribute.IntSlice(repositoryGuidanceBytesKey, bytes),
		attribute.IntSlice(repositoryGuidanceRenderedBytesKey, rendered),
		attribute.BoolSlice(repositoryGuidanceTruncatedKey, truncated),
		attribute.StringSlice(repositoryGuidanceSHA256Key, digests),
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

// RecordVerification appends a verifier attempt for the RunTrace
// aggregate returned by Finish. The loop's verifier.verify spans carry
// the per-attempt telemetry.
func (e *OTelTraceEmitter) RecordVerification(result types.VerificationResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.verificationResults = append(e.verificationResults, result)
}

// RecordStopDetails stores the run's final stop details so the RunTrace
// aggregate returned by Finish carries them.
func (e *OTelTraceEmitter) RecordStopDetails(details *types.StopDetails) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopDetails = details
}

// RecordDegradedCapabilities stores the capabilities the run proceeded
// without so the RunTrace aggregate returned by Finish carries them.
func (e *OTelTraceEmitter) RecordDegradedCapabilities(degraded []types.DegradedCapability) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.degradedCapabilities = append([]types.DegradedCapability(nil), degraded...)
}

// RecordLedgerUsage stores the run's auxiliary and external guard usage
// for the RunTrace aggregate returned by Finish.
func (e *OTelTraceEmitter) RecordLedgerUsage(auxiliary, externalGuard types.TokenUsage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.auxiliaryTokens = auxiliary
	e.externalGuardTokens = externalGuard
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

// RecordPostEditCheck emits a post_edit_check span under the root span
// and accumulates the record for the RunTrace aggregate.
func (e *OTelTraceEmitter) RecordPostEditCheck(record types.PostEditCheckRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	scrubbed := scrubPostEditCheck(record, e.redactionDisabled)
	e.postEditChecks = append(e.postEditChecks, scrubbed)
	if e.rootCtx == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("post_edit_check.tool", scrubbed.Tool),
		attribute.String("post_edit_check.tool_use_id", scrubbed.ToolUseID),
		attribute.Int("post_edit_check.index", scrubbed.Index),
		attribute.String("post_edit_check.command", scrubbed.Command),
		attribute.Int("post_edit_check.exit_code", scrubbed.ExitCode),
		attribute.Int64("post_edit_check.duration_ms", scrubbed.DurationMs),
		attribute.Bool("post_edit_check.timed_out", scrubbed.TimedOut),
	}
	end := time.Now()
	_, span := e.tracer.Start(e.rootCtx, "post_edit_check",
		oteltrace.WithAttributes(attrs...),
		oteltrace.WithTimestamp(end.Add(-time.Duration(scrubbed.DurationMs)*time.Millisecond)),
	)
	if scrubbed.Failed() {
		status := fmt.Sprintf("exit code %d", scrubbed.ExitCode)
		if scrubbed.Error != "" {
			status = scrubbed.Error
		}
		span.SetStatus(codes.Error, status)
	}
	span.End(oteltrace.WithTimestamp(end))
}

func (e *OTelTraceEmitter) RecordCommandOutputArchive(location string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.commandOutputArchive = location
	if e.rootSpan != nil {
		e.rootSpan.SetAttributes(attribute.String("command_output.archive_uri", location))
	}
}

// RecordToolCall appends a tool call summary to the run aggregate. Tool
// calls are recorded as spans through StartToolSpan.
func (e *OTelTraceEmitter) RecordToolCall(call types.ToolCallTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.toolCalls = append(e.toolCalls, call)
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
			if attrs := rootContent.attributes(e.systemInstructionsJSON, e.captureContentMaxBytes); len(attrs) > 0 {
				e.rootSpan.SetAttributes(attrs...)
			}
		}
		// Every non-success outcome marks the root Error so backends
		// expose one "didn't finish" predicate; run.outcome disambiguates
		// cancellation from failure. error.type is the bounded outcome,
		// refined for a generic "error" to the type of the model request
		// that failed last. The description is the run error, scrubbed,
		// so trace list views show the reason.
		if outcome != "success" {
			errorType := outcome
			if outcome == "error" && e.lastChatErrorType != "" {
				errorType = e.lastChatErrorType
			}
			desc := outcome
			if e.runError != "" {
				desc = scrubOrPass(e.runError, e.redactionDisabled)
			}
			e.rootSpan.SetAttributes(attribute.String(errorTypeKey, errorType))
			e.rootSpan.SetStatus(codes.Error, desc)
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
	totalTokens.Add(e.auxiliaryTokens)

	summaries := make([]types.ToolCallSummary, len(e.toolCalls))
	for i, tc := range e.toolCalls {
		summaries[i] = types.ToolCallSummary(tc)
	}

	var redactedConfig types.RunConfig
	if e.config != nil {
		if e.redactionDisabled {
			redactedConfig = *e.config
		} else {
			redactedConfig = redactRunConfig(e.config)
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
		VerificationResults:  e.verificationResults,
		Outcome:              outcome,
		StopDetails:          e.stopDetails,
		FinalAssistantText:   e.finalAssistantText,
		CommandOutputArchive: e.commandOutputArchive,
		Git:                  e.gitHandoff,
		WorkspaceExport:      e.workspaceExport,
		Error:                e.runError,
		StructuredOutput:     e.structuredOutput,
		DegradedCapabilities: e.degradedCapabilities,
		RepositoryGuidance:   e.repositoryGuidance,
		AuxiliaryTokens:      e.auxiliaryTokens,
		ExternalGuardTokens:  e.externalGuardTokens,
		PostEditChecks:       e.postEditChecks,
		TraceReference:       e.traceReferenceLocked(),
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

// TraceReference returns the identity of the current run's invoke_agent
// span, or nil before Start.
func (e *OTelTraceEmitter) TraceReference() *types.TraceReference {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.traceReferenceLocked()
}

func (e *OTelTraceEmitter) traceReferenceLocked() *types.TraceReference {
	if e.rootSpan == nil {
		return nil
	}
	sc := e.rootSpan.SpanContext()
	if !sc.IsValid() {
		return nil
	}
	return &types.TraceReference{
		TraceID:   sc.TraceID().String(),
		RunSpanID: sc.SpanID().String(),
		Sampled:   sc.IsSampled(),
	}
}

// RootContext returns the context containing the invoke_agent span, for use
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
