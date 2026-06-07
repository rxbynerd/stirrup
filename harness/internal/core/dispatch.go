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

// unknownToolMetricName is the sentinel substituted for tool.name on
// every metric emission when a model-supplied tool_use block references
// a name the registry cannot resolve. The raw name is unbounded
// (model-controlled, no schema) and MUST NOT be promoted into a TSDB
// label — see the substitution site in Phase 3 below for the full
// rationale (CWE-400, cardinality DoS).
const unknownToolMetricName = "__unknown__"

// pendingCall is the per-tool-call work item carried through the dispatch
// pipeline. Sync calls populate output/success inline; async calls are
// mutated by their goroutine and read back by the main routine after the
// WaitGroup join. Field writes from disjoint indices are race-free because
// the WaitGroup happens-before relation publishes them to the caller of
// planAndDispatch.
type pendingCall struct {
	call      types.ToolCall
	span      oteltrace.Span
	spanCtx   context.Context //nolint:containedctx // span parent ctx threaded into dispatch
	startedAt time.Time
	// internalName is the canonical internal tool ID call.Name resolved to
	// under the active toolset profile (issue #234). call.Name is the
	// model-facing alias; both are recorded in the trace. Equal to
	// call.Name under the default profile and for unknown tools (no
	// registry entry), so the trace's InternalName mirrors Name in those
	// cases and omitempty keeps the wire shape unchanged.
	internalName string
	output       string
	structured   structuredOutput // optional typed result payload + kind (issue #231); zero value for text-only tools and every failure path
	success      bool
	errorReason  string // guard-deny reason; written to trace (apply security.Scrub before setting)
	denied       bool   // PhasePreTool deny path; takes priority over (output, success)
	// failureCategory is the bounded ToolFailureCategory describing why
	// this call failed. Always empty when success is true; one of the
	// observability.ToolFailureCategory enum values otherwise. Read in
	// Phase 3 to emit the stirrup.harness.tool_failures counter and to
	// populate ToolCallTrace.ErrorCategory.
	failureCategory observability.ToolFailureCategory
}

// planAndDispatch executes the tool calls produced by one assistant turn,
// preserving the side-effect ordering of the sequential implementation.
// Sync calls run inline in assistant-message order; async calls fan out
// under a bounded semaphore sized to cfg.EffectiveToolDispatchMaxParallel().
//
// providerType and providerModel are the router's selection for this turn,
// threaded in so per-call failure metrics can be attributed back to the
// model that emitted the offending tool_use block. Empty strings are
// tolerated for callers without a resolved selection (e.g. unit tests of
// the dispatch path); the metric attributes still emit with empty
// provider.type / provider.model.
//
// Returned values:
//   - toolResults: results indexed by original call order (always len(toolCalls))
//   - toolRecords: full per-call records (raw Input + Output) for the turn
//     transcript. Indexed by original call order. Truncated to the same
//     length as toolResults when the stall detector trips.
//   - stallOutcome: non-empty when the stall detector tripped; the caller
//     must append toolResults to the message history and return immediately
//
// Invariants preserved relative to the sequential path:
//   - PhasePreTool guard runs sequentially before any async handler executes
//   - Per-call OTel spans are siblings of one another, children of the turn span
//   - Per-call metrics observations match the sequential code 1:1
//   - Trace records, tool_result transport emits, and stall.recordToolCall
//     are invoked in original call order
//   - Per-call timeout (DefaultAsyncToolTimeout or AsyncDispatch override)
//     applies inside dispatchAsyncToolCall — no parent timeout is introduced
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
	// tool. Sync calls (and Unknown tools) execute inline. Async survivors
	// are queued for the concurrent fan-out below.
	asyncIndices := make([]int, 0, len(toolCalls))
	for i, call := range toolCalls {
		l.Logger.Info("tool dispatched", "tool", call.Name)
		callStart := time.Now()
		// Resolve up front so every gating surface below keys on the
		// internal tool ID rather than the model-facing alias (issue #234).
		// Resolve is a pure lookup; an Unknown tool resolves to nil and
		// falls through to dispatchToolCall, which fails fast as today.
		// Resolution must precede span creation so the span name can be
		// bounded against model-controlled tool names (issue #309).
		t := l.Tools.Resolve(call.Name)

		// Bound the span name's cardinality the same way the metric label
		// below is bounded: an unknown tool resolves to nil and carries a
		// model-controlled name, so the span name is substituted with the
		// __unknown__ sentinel. Trace backends index on span name, so an
		// unbounded name is the same CWE-400 vector as an unbounded TSDB
		// label (issue #309). The model-supplied name is still preserved
		// verbatim in the tool.name attribute for debuggability.
		spanName := "tool." + call.Name
		if t == nil {
			spanName = "tool." + unknownToolMetricName
		}
		// Span is parented under l.traceCtx(ctx) (the trace-emitter's root
		// when OTel is wired) so it nests correctly in the trace backend,
		// but the propagated span ctx is rooted in the cancellable `ctx`.
		// Without this split, l.TraceContext = otelEmitter.RootContext()
		// would derive from context.Background() and the dispatch
		// goroutines below would not observe a run-level cancellation
		// until the per-call DefaultAsyncToolTimeout (60s) expired.
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
			// Default the internal name to the model-facing name; it is
			// refined to the resolved tool's internal ID below once the
			// registry lookup succeeds. Denied and unknown-tool calls keep
			// this default (alias == internal), which is the correct trace
			// shape for both.
			internalName: call.Name,
		}

		// guardToolName is the name the guardrail classifier sees: the
		// internal ID when resolved, falling back to the model-supplied
		// name for an unknown tool. A guardrail rule written against an
		// internal name must fire under any toolset profile.
		guardToolName := call.Name
		if t != nil {
			// Record the canonical internal tool ID resolved from the
			// model-facing name. Under a toolset profile call.Name is an
			// alias; t.Name is always the internal identity (Resolve
			// returns the underlying tool unchanged), so this is the
			// alias→internal binding the trace captures.
			plan[i].internalName = t.Name
			guardToolName = t.Name
		}

		// PhasePreTool guard: same semantics as the sequential code. A
		// deny short-circuits dispatch as a tool failure. Pass the
		// tool-span ctx so guard.pre_tool nests under tool.<name>.
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
			// (classifier-model output). The structured reason is
			// captured separately for trace/log fields and scrubbed
			// before it reaches them, matching the upstream_error path
			// in types.go and the non-denial trace branch below.
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

		// Sync path: dispatch inline, preserving the sequential code's
		// behaviour for sync tools (including Unknown).
		output, success, category, structured := l.dispatchToolCallCategorized(toolSpanCtx, call)
		plan[i].output = output
		plan[i].structured = structured
		plan[i].success = success
		plan[i].failureCategory = category
	}

	// Phase 2: fan out async calls under a bounded semaphore. The cap
	// comes from RunConfig validation (defaulted, clamped, validated to
	// be >0 and <=16), but defend against zero/negative here as well —
	// a misconstructed RunConfig in tests would otherwise deadlock.
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
						// other in-flight goroutines are unaffected.
						// Stable error string lets callers (and any
						// future regression test) match on the prefix.
						// The recovered value is scrubbed before it
						// flows into the tool result (which reaches the
						// model context and the transport tool_result
						// emit) so a panic whose %v rendering captures
						// secret-shaped fragments cannot leak.
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
		// dispatchAsyncToolCall -> correlator.Await(ctx, ...) which
		// honours it; Wait blocks until every goroutine has observed
		// that cancellation and returned an error result.
		wg.Wait()
	}

	// Phase 3: walk the plan in original call order to close out per-call
	// observability — span end, trace record, metrics, transport emit,
	// stall detector. Iterating sequentially after the WaitGroup join
	// guarantees the stall detector sees calls in deterministic order
	// (its identical-call heuristic depends on this) and that the
	// transport observes a deterministic tool_result sequence.
	toolResults := make([]types.ToolResult, len(toolCalls))
	toolRecords := make([]types.ToolCallRecord, len(toolCalls))
	for i := range plan {
		p := &plan[i]
		callDuration := time.Since(p.startedAt)

		// Compute the bounded failure category up front: it conditions
		// both the per-call OTel span attribute set (so trace backends
		// without JSONL access still see the category) and the
		// tool_failures metric emission below.
		errorCategory := ""
		if !p.success && p.failureCategory.IsValid() {
			errorCategory = p.failureCategory.String()
		}

		// Span finalisation. Attributes MUST be set before End() —
		// OTel SDKs typically drop SetAttributes calls on an already-
		// ended span — so the failure_category attribute is included
		// in the same batch as tool.success/duration/output_size.
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

		// Trace record: guard-denial carries the structured reason so
		// operators can see why; success/failure carries the scrubbed
		// dispatchToolCall output (raw error text can echo IAM paths,
		// schema field names, secret-shaped inputs).
		errorReason := ""
		switch {
		case p.denied:
			errorReason = p.errorReason
		case !p.success:
			errorReason = security.Scrub(p.output)
		}
		// Emit InternalName only when an alias was actually resolved (it
		// differs from the model-facing name). Under the default profile
		// the two are equal, so the field stays empty and omitempty keeps
		// the trace wire shape byte-identical to the pre-profile behaviour.
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

		// p.structured.payload is a json.RawMessage ([]byte) consumed by both
		// the ToolResult below and the ToolCallRecord further down. Hand each
		// struct its own copy so the two trace surfaces never alias the same
		// backing slice: a downstream in-place edit (scrub, redaction) to one
		// must not silently mutate the other.
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
		// The dispatch site is the only place with both fields in scope:
		// p.call.Input is the model-supplied raw JSON; p.output is the
		// post-dispatch result string (either tool output or scrubbed
		// error reason). Recording is done here so the loop's
		// RecordTurnRecord call after planAndDispatch returns is a pure
		// data hand-off.
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
			// Co-emit stall terminations into the tool-failure series
			// so dashboards can attribute "run ended via stall" back
			// to (provider, model, tool, stall-flavour). The stall
			// detector reports two outcomes: "stalled" for repeated
			// identical calls and "tool_failures" for consecutive
			// failures — map each onto its own bounded category.
			var stallCategory observability.ToolFailureCategory
			switch outcome {
			case "stalled":
				stallCategory = observability.ToolFailureStallRepeated
			case "tool_failures":
				stallCategory = observability.ToolFailureStallConsecutiveFailures
			}
			if stallCategory.IsValid() {
				// metricToolName carries the unknown_tool sentinel
				// substitution applied above; reuse it so a stall
				// triggered by repeated unknown-tool calls does not
				// re-introduce the unbounded label.
				l.Metrics.ToolFailures.Add(ctx, 1, l.metricAttrs(
					attribute.String("tool.name", metricToolName),
					attribute.String("category", stallCategory.String()),
					attribute.String("provider.type", providerType),
					attribute.String("provider.model", providerModel),
					attribute.String("run.mode", config.Mode),
				))
			}
			// Close any spans for calls past the stall point. Phase 2
			// has already invoked async handlers for these indices, so
			// the spans are open and would leak if we returned without
			// calling End(). Their observability side effects (trace
			// record, metric, transport emit, stall record) are
			// intentionally dropped to match the sequential code, which
			// never executed them at all once stall tripped.
			for j := i + 1; j < len(plan); j++ {
				plan[j].span.End()
			}
			// Truncate the result slice to the calls actually observed
			// by the stall detector. The sequential code did not append
			// the un-observed tail because it `continue`d/`return`ed
			// from the loop; preserving that ensures appendToolResults
			// produces a turn message identical to the sequential
			// version when a stall trips on the first call.
			return toolResults[:i+1], toolRecords[:i+1], outcome
		}
	}

	return toolResults, toolRecords, ""
}

// emitToolCallMetrics records the per-call observability counters for a
// completed pendingCall: tool_calls, tool_call_duration, and (on failure)
// tool_errors plus the failure-category breakdown tool_failures. It
// returns the bounded metric tool name — the unknown_tool sentinel
// substitution applied here — so the caller can reuse it for the stall
// co-emission without re-deriving the substitution.
//
// Two cardinality bounds live here and are the reason this is a single
// seam rather than inline code: the unknown_tool name substitution
// (model-supplied names must not flow into a TSDB label, CWE-400) and the
// failureCategory.IsValid() guard on tool_failures (a producer that sets
// Success=false with an unrecognised category still bumps tool_errors but
// MUST NOT widen the bounded tool_failures category label). The IsValid()
// guard in particular has no model-facing trigger, so it is exercised via
// a synthetic pendingCall in tool_failure_metrics_test.go.
func (l *AgenticLoop) emitToolCallMetrics(
	ctx context.Context,
	p *pendingCall,
	callDuration time.Duration,
	providerType string,
	providerModel string,
	mode string,
) string {
	// Sentinel substitution for the unknown_tool path: p.call.Name is
	// model-supplied and unbounded when no registry entry resolves, so it
	// MUST NOT flow into a TSDB label. An attacker- or malfunctioning-
	// model loop emitting tool_use blocks with high-entropy unique names
	// would create O(n) unique timeseries on
	// stirrup.harness.tool_{calls,errors,failures,call_duration}
	// (CWE-400). Trace records (ErrorReason, ToolCallTrace.Name) retain
	// the raw name for debugging.
	metricToolName := p.call.Name
	if p.failureCategory == observability.ToolFailureUnknownTool {
		metricToolName = unknownToolMetricName
	}
	toolNameAttr := l.metricAttrs(attribute.String("tool.name", metricToolName))
	l.Metrics.ToolCalls.Add(ctx, 1, toolNameAttr)
	l.Metrics.ToolCallDuration.Record(ctx, float64(callDuration.Milliseconds()), toolNameAttr)
	if !p.success {
		l.Metrics.ToolErrors.Add(ctx, 1, toolNameAttr)
		// Failure-category breakdown for dashboards. Validity is re-checked
		// here so a producer that forgets to set a category (Success=false
		// with empty category) — or sets a free-form one — still bumps
		// ToolErrors but does not silently widen ToolFailures label
		// cardinality past the bounded enum.
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

// cloneRawMessage returns an independent copy of a json.RawMessage so two
// trace structs derived from the same dispatch result do not alias one
// backing slice. bytes.Clone preserves the nil/empty distinction: a nil
// payload (the "no structured data" case) stays nil rather than becoming an
// empty non-nil slice, keeping the trace wire shape unchanged.
func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	return bytes.Clone(raw)
}
