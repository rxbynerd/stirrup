package core

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// pendingCall is the per-tool-call work item carried through the dispatch
// pipeline. Sync calls populate output/success inline; async calls are
// mutated by their goroutine and read back by the main routine after the
// WaitGroup join. Field writes from disjoint indices are race-free because
// the WaitGroup happens-before relation publishes them to the caller of
// planAndDispatch.
type pendingCall struct {
	call        types.ToolCall
	span        oteltrace.Span
	spanCtx     context.Context //nolint:containedctx // span parent ctx threaded into dispatch
	startedAt   time.Time
	output      string
	success     bool
	errorReason string // guard-deny reason; written to trace (apply security.Scrub before setting)
	denied      bool   // PhasePreTool deny path; takes priority over (output, success)
}

// planAndDispatch executes the tool calls produced by one assistant turn,
// preserving the side-effect ordering of the sequential implementation.
// Sync calls run inline in assistant-message order; async calls fan out
// under a bounded semaphore sized to cfg.EffectiveToolDispatchMaxParallel().
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
) ([]types.ToolResult, []types.ToolCallRecord, string) {
	plan := make([]pendingCall, len(toolCalls))

	// Phase 1: open the tool span, run PhasePreTool guard, and resolve the
	// tool. Sync calls (and Unknown tools) execute inline. Async survivors
	// are queued for the concurrent fan-out below.
	asyncIndices := make([]int, 0, len(toolCalls))
	for i, call := range toolCalls {
		l.Logger.Info("tool dispatched", "tool", call.Name)
		callStart := time.Now()
		// Span is parented under l.traceCtx(ctx) (the trace-emitter's root
		// when OTel is wired) so it nests correctly in the trace backend,
		// but the propagated span ctx is rooted in the cancellable `ctx`.
		// Without this split, l.TraceContext = otelEmitter.RootContext()
		// would derive from context.Background() and the dispatch
		// goroutines below would not observe a run-level cancellation
		// until the per-call DefaultAsyncToolTimeout (60s) expired.
		_, toolSpan := l.Tracer.Start(l.traceCtx(ctx), "tool."+call.Name,
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
		}

		// PhasePreTool guard: same semantics as the sequential code. A
		// deny short-circuits dispatch as a tool failure. Pass the
		// tool-span ctx so guard.pre_tool nests under tool.<name>.
		preToolIn := guard.Input{
			Phase:     guard.PhasePreTool,
			Content:   string(call.Input),
			Source:    "tool_call:" + call.Name,
			ToolName:  call.Name,
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
			continue
		}

		// Resolve to decide sync vs async. An Unknown tool falls through
		// to dispatchToolCall which fails fast with the same error path
		// as today — treat it as sync.
		t := l.Tools.Resolve(call.Name)
		if t != nil && t.AsyncHandler != nil {
			asyncIndices = append(asyncIndices, i)
			continue
		}

		// Sync path: dispatch inline, preserving the sequential code's
		// behaviour for sync tools (including Unknown).
		output, success := l.dispatchToolCall(toolSpanCtx, call)
		plan[i].output = output
		plan[i].success = success
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
						l.Logger.Error(
							"async tool panic recovered",
							"tool", plan[idx].call.Name,
							"recovered", fmt.Sprintf("%v", r),
							"stack", string(debug.Stack()),
						)
					}
				}()
				output, success := l.dispatchToolCall(plan[idx].spanCtx, plan[idx].call)
				plan[idx].output = output
				plan[idx].success = success
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

		// Span finalisation.
		p.span.SetAttributes(
			attribute.Bool("tool.success", p.success),
			attribute.Int("tool.output_size", len(p.output)),
			attribute.Int64("tool.duration_ms", callDuration.Milliseconds()),
		)
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
		l.Trace.RecordToolCall(types.ToolCallTrace{
			Name:        p.call.Name,
			DurationMs:  callDuration.Milliseconds(),
			Success:     p.success,
			ErrorReason: errorReason,
			InputSize:   len(p.call.Input),
			OutputSize:  len(p.output),
		})

		toolNameAttr := l.metricAttrs(attribute.String("tool.name", p.call.Name))
		l.Metrics.ToolCalls.Add(ctx, 1, toolNameAttr)
		l.Metrics.ToolCallDuration.Record(ctx, float64(callDuration.Milliseconds()), toolNameAttr)
		if !p.success {
			l.Metrics.ToolErrors.Add(ctx, 1, toolNameAttr)
		}

		toolResults[i] = types.ToolResult{
			ToolUseID: p.call.ID,
			Content:   p.output,
			IsError:   !p.success,
		}
		// Full record carries raw input/output for the turn transcript.
		// The dispatch site is the only place with both fields in scope:
		// p.call.Input is the model-supplied raw JSON; p.output is the
		// post-dispatch result string (either tool output or scrubbed
		// error reason). Recording is done here so the loop's
		// RecordTurnRecord call after planAndDispatch returns is a pure
		// data hand-off.
		toolRecords[i] = types.ToolCallRecord{
			ID:         p.call.ID,
			Name:       p.call.Name,
			Input:      p.call.Input,
			Output:     p.output,
			DurationMs: callDuration.Milliseconds(),
			Success:    p.success,
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
