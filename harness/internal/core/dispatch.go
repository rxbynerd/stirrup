package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// unknownToolMetricName is the sentinel substituted for tool.name when a
// tool_use block does not resolve to a registered tool.
const unknownToolMetricName = "__unknown__"

// pendingCall is the per-tool-call work item carried through the dispatch
// pipeline. Async calls are mutated by their goroutine and read back after
// the WaitGroup join, which happens-before publishes those writes to the
// caller of planAndDispatch.
type pendingCall struct {
	call      types.ToolCall
	span      oteltrace.Span
	spanCtx   context.Context //nolint:containedctx // span parent ctx threaded into dispatch
	startedAt time.Time
	// internalName is the canonical internal tool ID call.Name resolves to
	// under the active toolset profile. Equal to call.Name under the
	// default profile and for unknown tools.
	internalName string
	output       string
	structured   structuredOutput // optional typed result payload + kind; zero value for text-only tools and every failure path
	success      bool
	errorReason  string // guard-deny reason; written to trace (apply security.Scrub before setting)
	denied       bool   // PhasePreTool deny path; takes priority over (output, success)
	// failureCategory is the bounded ToolFailureCategory describing why
	// this call failed; empty when success is true.
	failureCategory observability.ToolFailureCategory
}

// planAndDispatch executes the tool calls produced by one assistant turn,
// preserving the side-effect ordering of the sequential implementation: sync
// calls run inline in order, async calls fan out under a bounded semaphore
// sized to cfg.EffectiveToolDispatchMaxParallel(). See
// docs/architecture.md for the phase breakdown and the invariants
// preserved relative to the sequential path.
//
// providerType and providerModel attribute per-call failure metrics back to
// the model that emitted the offending tool_use block; empty strings are
// tolerated for callers with no resolved selection.
//
// Returns toolResults and toolRecords indexed by original call order
// (truncated together when the stall detector trips), and a non-empty
// stallOutcome when it did — the caller must append toolResults to the
// message history and return immediately.
func (l *AgenticLoop) planAndDispatch(
	ctx context.Context,
	config *types.RunConfig,
	toolCalls []types.ToolCall,
	stall *stallDetector,
	providerType string,
	providerModel string,
) ([]types.ToolResult, []types.ToolCallRecord, string) {
	plan := make([]pendingCall, len(toolCalls))

	// Phase 1: open the tool span, run PhasePreTool guard, and resolve the
	// tool. Sync calls (and Unknown tools) execute inline; async survivors
	// queue for the fan-out in Phase 2.
	asyncIndices := make([]int, 0, len(toolCalls))
	for i, call := range toolCalls {
		l.Logger.Info("tool dispatched", "tool", call.Name)
		callStart := time.Now()
		// Resolve up front so every gating surface below keys on the
		// internal tool ID rather than the model-facing alias. An
		// unresolved tool falls through to dispatchToolCall, which fails
		// fast.
		t := l.Tools.Resolve(call.Name)

		// Bound the span name's cardinality the same way as the metric
		// label below: an unknown tool's name is model-controlled, so the
		// span name uses the __unknown__ sentinel while the raw name is
		// still preserved in the tool.name attribute.
		spanName := "tool." + call.Name
		if t == nil {
			spanName = "tool." + unknownToolMetricName
		}
		// Span parented under l.traceCtx(ctx) (nests under the trace
		// emitter's root) but the propagated span ctx is rooted in the
		// cancellable ctx — otherwise the dispatch goroutines below would
		// not observe run cancellation until the async tool timeout expires.
		_, toolSpan := l.Tracer.Start(l.traceCtx(ctx), spanName,
			oteltrace.WithAttributes(
				attribute.String("tool.name", call.Name),
				attribute.Int("tool.input_size", len(call.Input)),
			),
		)
		toolSpanCtx := oteltrace.ContextWithSpan(ctx, toolSpan)
		plan[i] = pendingCall{
			call:      call,
			span:      toolSpan,
			spanCtx:   toolSpanCtx,
			startedAt: callStart,
			// internalName defaults to the model-facing name; refined below
			// once resolution succeeds. Denied and unknown-tool calls keep
			// this default (alias == internal).
			internalName: call.Name,
		}

		// guardToolName is what the guardrail classifier sees: the internal
		// ID when resolved, else the model-supplied name. A rule written
		// against an internal name must fire under any toolset profile.
		guardToolName := call.Name
		if t != nil {

			plan[i].internalName = t.Name
			guardToolName = t.Name
		}

		// PhasePreTool guard. Passes the tool-span ctx so guard.pre_tool
		// nests under tool.<name>. A deny short-circuits dispatch as a
		// tool failure.
		preToolIn := guard.Input{
			Phase:     guard.PhasePreTool,
			Content:   string(call.Input),
			Source:    "tool_call:" + call.Name,
			ToolName:  guardToolName,
			ToolInput: call.Input,
			Mode:      config.Mode,
			RunID:     config.RunID,
		}
		preToolAllow, preToolDecision, _ := l.guardCheck(toolSpanCtx, preToolIn, guardFailOpen(config))
		if !preToolAllow {
			// The user-visible tool error MUST be a fixed string:
			// preToolDecision.Reason is adversary-influenceable
			// (classifier-model output). The structured reason is scrubbed
			// separately for trace/log fields.
			const blockedToolMessage = "guardrail blocked tool call"
			reason := blockedToolMessage
			if preToolDecision != nil && preToolDecision.Reason != "" {
				reason = blockedToolMessage + ": " + security.Scrub(preToolDecision.Reason)
			}
			plan[i].denied = true
			plan[i].output = blockedToolMessage
			plan[i].success = false
			plan[i].errorReason = reason
			plan[i].failureCategory = observability.ToolFailureGuardrailDenied
			continue
		}

		if t != nil && t.AsyncHandler != nil {
			asyncIndices = append(asyncIndices, i)
			continue
		}

		// Sync path: dispatch inline (including Unknown tools).
		output, success, category, structured := l.dispatchToolCallCategorized(toolSpanCtx, call)
		plan[i].output = output
		plan[i].structured = structured
		plan[i].success = success
		plan[i].failureCategory = category
	}

	// Phase 2: fan out async calls under a bounded semaphore. Defend
	// against a misconstructed RunConfig (maxParallel <= 0) deadlocking.
	if len(asyncIndices) > 0 {
		maxParallel := config.EffectiveToolDispatchMaxParallel()
		if maxParallel < 1 {
			maxParallel = 1
		}
		sem := make(chan struct{}, maxParallel)
		var wg sync.WaitGroup
		for _, idx := range asyncIndices {
			idx := idx
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				defer func() {
					if r := recover(); r != nil {
						// Convert panic into a structured tool failure so
						// other in-flight goroutines are unaffected. The
						// recovered value is scrubbed before it reaches the
						// tool result (model context + transport emit) so a
						// panic capturing secret-shaped fragments can't leak.
						msg := fmt.Sprintf("async tool %s panic: %v", plan[idx].call.Name,
							security.Scrub(fmt.Sprintf("%v", r)))
						plan[idx].output = msg
						plan[idx].success = false
						plan[idx].failureCategory = observability.ToolFailureAsyncPanic
						l.Logger.Error(
							"async tool panic recovered",
							"tool", plan[idx].call.Name,
							"recovered", fmt.Sprintf("%v", r),
							"stack", string(debug.Stack()),
						)
					}
				}()
				output, success, category, structured := l.dispatchToolCallCategorized(plan[idx].spanCtx, plan[idx].call)
				plan[idx].output = output
				plan[idx].structured = structured
				plan[idx].success = success
				plan[idx].failureCategory = category
			}()
		}
		// Wait under cancellation: ctx cancellation propagates through
		// dispatchAsyncToolCall -> correlator.Await, so Wait returns once
		// every goroutine has observed it.
		wg.Wait()
	}

	// Phase 3: walk the plan in original call order, after the WaitGroup
	// join, to close out per-call observability deterministically — the
	// stall detector's identical-call heuristic and the transport's
	// tool_result sequence both depend on this ordering.
	toolResults := make([]types.ToolResult, len(toolCalls))
	toolRecords := make([]types.ToolCallRecord, len(toolCalls))
	for i := range plan {
		p := &plan[i]
		callDuration := time.Since(p.startedAt)

		// Compute the bounded failure category up front: it feeds both the
		// span attribute set and the tool_failures metric emission below.
		errorCategory := ""
		if !p.success && p.failureCategory.IsValid() {
			errorCategory = p.failureCategory.String()
		}

		// Attributes MUST be set before End() — OTel SDKs typically drop
		// SetAttributes on an already-ended span.
		spanAttrs := []attribute.KeyValue{
			attribute.Bool("tool.success", p.success),
			attribute.Int("tool.output_size", len(p.output)),
			attribute.Int64("tool.duration_ms", callDuration.Milliseconds()),
		}
		if errorCategory != "" {
			spanAttrs = append(spanAttrs, attribute.String("tool.failure_category", errorCategory))
		}
		p.span.SetAttributes(spanAttrs...)
		switch {
		case p.denied:
			p.span.SetStatus(codes.Error, "guardrail_denied")
		case !p.success:
			p.span.SetStatus(codes.Error, "tool call failed")
		}
		p.span.End()

		// Guard-denial carries the structured reason; success/failure
		// carries the scrubbed dispatch output (raw error text can echo
		// IAM paths, schema fields, secret-shaped inputs).
		errorReason := ""
		switch {
		case p.denied:
			errorReason = p.errorReason
		case !p.success:
			errorReason = security.Scrub(p.output)
		}
		// Emit InternalName only when an alias was actually resolved, so
		// the trace wire shape is unchanged under the default profile.
		internalForTrace := ""
		if p.internalName != p.call.Name {
			internalForTrace = p.internalName
		}
		l.Trace.RecordToolCall(types.ToolCallTrace{
			ID:            p.call.ID,
			Name:          p.call.Name,
			InternalName:  internalForTrace,
			DurationMs:    callDuration.Milliseconds(),
			Success:       p.success,
			ErrorReason:   errorReason,
			InputSize:     len(p.call.Input),
			OutputSize:    len(p.output),
			ErrorCategory: errorCategory,
		})

		metricToolName := l.emitToolCallMetrics(ctx, p, callDuration, providerType, providerModel, config.Mode)

		// Independent copies so the two trace structs never alias the same
		// backing slice: a downstream in-place edit to one must not
		// silently mutate the other.
		structuredForResult := cloneRawMessage(p.structured.payload)
		structuredForRecord := cloneRawMessage(p.structured.payload)
		toolResults[i] = types.ToolResult{
			ToolUseID:  p.call.ID,
			Content:    p.output,
			IsError:    !p.success,
			Structured: structuredForResult,
			Kind:       p.structured.kind,
		}
		// Full record carries raw input/output for the turn transcript.
		toolRecords[i] = types.ToolCallRecord{
			ID:           p.call.ID,
			Name:         p.call.Name,
			InternalName: internalForTrace,
			Input:        p.call.Input,
			Output:       p.output,
			DurationMs:   callDuration.Milliseconds(),
			Success:      p.success,
			Structured:   structuredForRecord,
			Kind:         p.structured.kind,
		}

		if err := l.Transport.Emit(types.HarnessEvent{
			Type:      "tool_result",
			ToolUseID: p.call.ID,
			Content:   p.output,
		}); err != nil {
			l.Logger.Warn("transport emit failed", "event", "tool_result", "error", err)
		}

		if outcome := stall.recordToolCall(p.call.Name, p.call.Input, p.success); outcome != "" {
			l.Metrics.Stalls.Add(ctx, 1,
				l.metricAttrs(attribute.String("run.mode", config.Mode)),
			)
			// Co-emit stall terminations into the tool-failure series so
			// dashboards can attribute "run ended via stall" back to
			// (provider, model, tool, stall-flavour).
			var stallCategory observability.ToolFailureCategory
			switch outcome {
			case "stalled":
				stallCategory = observability.ToolFailureStallRepeated
			case "tool_failures":
				stallCategory = observability.ToolFailureStallConsecutiveFailures
			}
			if stallCategory.IsValid() {
				// Reuse metricToolName (already sentinel-substituted) so a
				// stall on repeated unknown-tool calls doesn't re-widen
				// the label.
				l.Metrics.ToolFailures.Add(ctx, 1, l.metricAttrs(
					attribute.String("tool.name", metricToolName),
					attribute.String("category", stallCategory.String()),
					attribute.String("provider.type", providerType),
					attribute.String("provider.model", providerModel),
					attribute.String("run.mode", config.Mode),
				))
			}
			// Close spans for calls past the stall point (Phase 2 already
			// dispatched them). Their trace/metric/transport side effects
			// are intentionally dropped to match the sequential code.
			for j := i + 1; j < len(plan); j++ {
				plan[j].span.End()
			}
			// Truncate to the calls actually observed by the stall
			// detector, matching the sequential code's early return.
			return toolResults[:i+1], toolRecords[:i+1], outcome
		}
	}

	return toolResults, toolRecords, ""
}

// emitToolCallMetrics records the per-call observability counters for a
// completed pendingCall (tool_calls, tool_call_duration, and on failure
// tool_errors / tool_failures) and returns the bounded metric tool name
// (unknown_tool sentinel applied) for the caller to reuse in stall
// co-emission.
func (l *AgenticLoop) emitToolCallMetrics(
	ctx context.Context,
	p *pendingCall,
	callDuration time.Duration,
	providerType string,
	providerModel string,
	mode string,
) string {
	// p.call.Name is model-supplied and unbounded when unresolved, so it
	// must not flow into a TSDB label; trace records retain the raw name.
	metricToolName := p.call.Name
	if p.failureCategory == observability.ToolFailureUnknownTool {
		metricToolName = unknownToolMetricName
	}
	toolNameAttr := l.metricAttrs(attribute.String("tool.name", metricToolName))
	l.Metrics.ToolCalls.Add(ctx, 1, toolNameAttr)
	l.Metrics.ToolCallDuration.Record(ctx, float64(callDuration.Milliseconds()), toolNameAttr)
	if !p.success {
		l.Metrics.ToolErrors.Add(ctx, 1, toolNameAttr)
		// Re-check validity here so a producer that leaves category empty
		// or invents a free-form one still bumps ToolErrors without
		// widening the bounded ToolFailures label.
		if p.failureCategory.IsValid() {
			l.Metrics.ToolFailures.Add(ctx, 1, l.metricAttrs(
				attribute.String("tool.name", metricToolName),
				attribute.String("category", p.failureCategory.String()),
				attribute.String("provider.type", providerType),
				attribute.String("provider.model", providerModel),
				attribute.String("run.mode", mode),
			))
		}
	}
	return metricToolName
}

// cloneRawMessage returns an independent copy of raw so two trace structs
// don't alias the same backing slice. bytes.Clone preserves nil vs empty.
func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	return bytes.Clone(raw)
}
