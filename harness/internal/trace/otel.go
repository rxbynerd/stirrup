package trace

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/types"
	"github.com/rxbynerd/stirrup/types/version"
)

// OTelTraceEmitter records harness run telemetry as OpenTelemetry spans,
// exported via OTLP/gRPC to a collector endpoint.
type OTelTraceEmitter struct {
	provider *sdktrace.TracerProvider
	tracer   oteltrace.Tracer

	mu        sync.Mutex
	runID     string
	config    *types.RunConfig
	startedAt time.Time
	rootSpan  oteltrace.Span
	rootCtx   context.Context
	turns     []types.TurnTrace
	toolCalls []types.ToolCallTrace
}

// NewOTelTraceEmitter creates an OTel trace emitter that exports spans to the
// given OTLP/gRPC endpoint (e.g. "localhost:4317"). The caller must eventually
// call Finish to flush and shut down the exporter.
func NewOTelTraceEmitter(ctx context.Context, endpoint string) (*OTelTraceEmitter, error) {
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
	)
	tracer := tp.Tracer("stirrup-harness")

	return &OTelTraceEmitter{
		provider: tp,
		tracer:   tracer,
	}, nil
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

	ctx := context.Background()
	ctx, span := e.tracer.Start(ctx, "run",
		oteltrace.WithAttributes(
			attribute.String("run.id", runID),
			attribute.String("harness.version", version.Version()),
		),
	)

	if config != nil {
		span.SetAttributes(
			attribute.String("run.mode", config.Mode),
			attribute.String("run.provider", config.Provider.Type),
		)
		if config.ModelRouter.Model != "" {
			span.SetAttributes(attribute.String("run.model", config.ModelRouter.Model))
		}
		if config.SessionName != "" {
			// Set on the root span so child spans inherit access via context.
			// Skipped when empty so we don't pollute traces with empty
			// attributes for runs that did not specify a label.
			span.SetAttributes(attribute.String("run.session_name", config.SessionName))
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

	_, span := e.tracer.Start(e.rootCtx, fmt.Sprintf("turn[%d]", turn.Turn),
		oteltrace.WithTimestamp(spanStart),
		oteltrace.WithAttributes(
			attribute.Int("turn.number", turn.Turn),
			attribute.Int("turn.tokens.input", turn.Tokens.Input),
			attribute.Int("turn.tokens.output", turn.Tokens.Output),
			attribute.Int("turn.tool_calls", turn.ToolCalls),
			attribute.String("turn.stop_reason", turn.StopReason),
			attribute.Int64("turn.duration_ms", turn.DurationMs),
		),
	)
	span.End(oteltrace.WithTimestamp(spanEnd))
}

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
			attribute.String("tool.name", call.Name),
			attribute.Bool("tool.success", call.Success),
			attribute.Int64("tool.duration_ms", call.DurationMs),
		),
	)
	span.End(oteltrace.WithTimestamp(spanEnd))
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
		)
		e.rootSpan.End()
	}

	// Flush and shut down the trace provider.
	if e.provider != nil {
		if err := e.provider.ForceFlush(ctx); err != nil {
			// Non-fatal: log but continue building the trace.
			_ = err
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
		ID:          e.runID,
		Config:      redactedConfig,
		StartedAt:   e.startedAt,
		CompletedAt: now,
		Turns:       len(e.turns),
		TokenUsage:  totalTokens,
		ToolCalls:   summaries,
		Outcome:     outcome,
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
