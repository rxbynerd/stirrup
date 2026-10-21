package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
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
	span      trace.ToolSpan
	spanCtx   context.Context //nolint:containedctx // span parent ctx threaded into dispatch
	startedAt time.Time
	// finishedAt is when the call's outcome was settled, so a call's span
	// and recorded duration exclude the calls dispatched after it.
	finishedAt time.Time
	// queuedAt is when an async call left Phase 1; queued is how long it
	// then waited for Phase 2 to start it, behind later sync calls and the
	// dispatch semaphore.
	queuedAt time.Time
	queued   time.Duration
	// internalName is the canonical internal tool ID call.Name resolves to
	// under the active toolset profile. Equal to call.Name under the
	// default profile and for unknown tools.
	internalName string
	// tool and input are set once preflightToolCall accepts the call;
	// input is the cleaned form the guard classified and the handler
	// receives.
	tool        *tool.Tool
	input       json.RawMessage
	output      string
	structured  structuredOutput // optional typed result payload + kind; zero value for text-only tools and every failure path
	success     bool
	errorReason string // trace reason overriding the scrubbed output (guard deny, invalid arguments); apply security.Scrub before setting
	// failureCategory is the bounded ToolFailureCategory describing why
	// this call failed; empty when success is true.
	failureCategory observability.ToolFailureCategory
	// toolsLoaded lists the deferred tools the call loaded (a harness
	// tool_search), by internal ID.
	toolsLoaded []string
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
// (truncated together when the stall detector ends the run), and a
// non-empty stallOutcome when it did — the caller must append
// toolResults to the message history and return immediately. A stall
// nudge does not truncate: every call's result is returned, and the
// caller collects the nudge with stall.takeNudge.
func (l *AgenticLoop) planAndDispatch(
	ctx context.Context,
	config *types.RunConfig,
	toolCalls []types.ToolCall,
	stall *stallDetector,
	providerType string,
	providerModel string,
) ([]types.ToolResult, []types.ToolCallRecord, string) {
	plan := make([]pendingCall, len(toolCalls))

	// Phase 1: resolve the tool, open the tool span, run the deterministic
	// preflight, then the PhasePreTool guard. Sync calls execute inline;
	// async survivors queue for the fan-out in Phase 2.
	asyncIndices := make([]int, 0, len(toolCalls))
	for i, call := range toolCalls {
		callStart := time.Now()
		// Resolve up front so every gating surface below keys on the
		// internal tool ID rather than the model-facing alias.
		t := l.Tools.Resolve(call.Name)

		// The span nests under the run span, but toolSpanCtx descends from
		// the cancellable ctx so the dispatch goroutines below observe run
		// cancellation.
		toolSpanCtx, toolSpan := l.startToolSpan(ctx, call, t != nil)
		l.Logger.InfoContext(toolSpanCtx, "tool dispatched", "tool", call.Name)
		callMeta := tool.CallContextFrom(ctx)
		callMeta.ToolUseID = call.ID
		callMeta.Lazy = l.LazyTools
		toolSpanCtx = tool.WithCallContext(toolSpanCtx, callMeta)
		plan[i] = pendingCall{
			call:      call,
			span:      toolSpan,
			spanCtx:   toolSpanCtx,
			startedAt: callStart,
			// internalName defaults to the model-facing name; refined below
			// once resolution succeeds. Unknown-tool calls keep this
			// default (alias == internal).
			internalName: call.Name,
		}
		if t != nil {
			plan[i].internalName = t.Name
		}

		// Deterministic checks run before the model guard: a call they
		// reject never costs a classifier round-trip, and the guard
		// classifies the cleaned input the handler would receive.
		input, rejection, rejectCategory := l.preflightToolCall(call, t)
		if rejectCategory != "" {
			plan[i].output = rejection
			plan[i].success = false
			plan[i].failureCategory = rejectCategory
			if rejectCategory == observability.ToolFailureInvalidArguments {
				plan[i].errorReason = invalidArgsTraceReason(call.Name, call.InvalidInput)
			}
			plan[i].finishedAt = time.Now()
			continue
		}
		plan[i].tool = t
		plan[i].input = input

		// PhasePreTool guard. Passes the tool-span ctx so guard.pre_tool
		// nests under execute_tool. ToolName is the internal ID so a rule
		// written against an internal name fires under any toolset
		// profile. A deny short-circuits dispatch as a tool failure.
		preToolIn := guard.Input{
			Phase:     guard.PhasePreTool,
			Content:   string(input),
			Source:    "tool_call:" + call.Name,
			ToolName:  t.Name,
			ToolInput: input,
			Mode:      config.Mode,
			RunID:     config.RunID,
		}
		if guard.ChecksPhase(l.GuardRail, guard.PhasePreTool) {
			scrubToolInputForGuard(&preToolIn, input)
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
			plan[i].output = blockedToolMessage
			plan[i].success = false
			plan[i].errorReason = reason
			plan[i].failureCategory = observability.ToolFailureGuardrailDenied
			plan[i].finishedAt = time.Now()
			continue
		}

		if t.AsyncHandler != nil {
			plan[i].queuedAt = time.Now()
			asyncIndices = append(asyncIndices, i)
			continue
		}

		// Sync calls run one at a time, so the loaded set grows only by
		// what this call loaded.
		loadedBefore := l.lazyLoadedCount()
		output, success, category, structured := l.executeToolCall(toolSpanCtx, t, call, input)
		plan[i].output = output
		plan[i].structured = structured
		plan[i].success = success
		plan[i].failureCategory = category
		plan[i].finishedAt = time.Now()
		plan[i].toolsLoaded = l.lazyLoadedSince(loadedBefore)
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
				plan[idx].queued = time.Since(plan[idx].queuedAt)
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
						plan[idx].finishedAt = time.Now()
						l.Logger.ErrorContext(plan[idx].spanCtx,
							"async tool panic recovered",
							"tool", plan[idx].call.Name,
							"recovered", fmt.Sprintf("%v", r),
							"stack", string(debug.Stack()),
						)
					}
				}()
				output, success, category, structured := l.executeToolCall(plan[idx].spanCtx, plan[idx].tool, plan[idx].call, plan[idx].input)
				plan[idx].output = output
				plan[idx].structured = structured
				plan[idx].success = success
				plan[idx].failureCategory = category
				plan[idx].finishedAt = time.Now()
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
	resultBudget := config.Tools.EffectiveMaxResultBytes()
	for i := range plan {
		p := &plan[i]
		// Sync and async results converge here, so this is the single
		// point that bounds what reaches history, trace and transport. A
		// cut result drops its structured envelope, which would otherwise
		// repeat the omitted data.
		if p.tool == nil || !p.tool.SelfBoundedResult {
			if bounded, cut := boundToolResult(p.output, resultBudget, p.internalName); cut {
				p.span.Span().SetAttributes(attribute.Int("tool.output_truncated_from", len(p.output)))
				p.output = bounded
				p.structured = structuredOutput{}
			}
		}

		callDuration := p.duration()
		callTrace := p.toolCallTrace(callDuration)
		internalForTrace := callTrace.InternalName

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
			IsError:      !p.success,
		}
		p.span.End(trace.ToolSpanEnd{Summary: &callTrace, Record: &toolRecords[i], EndTime: p.finishedAt, Queued: p.queued})
		l.Trace.RecordToolCall(callTrace)

		if err := l.Transport.Emit(types.HarnessEvent{
			Type:       "tool_result",
			ToolUseID:  p.call.ID,
			Name:       p.call.Name,
			Content:    p.output,
			IsError:    !p.success,
			DurationMs: callDuration.Milliseconds(),
		}); err != nil {
			l.logEmitFailure("tool_result", err)
		}
		if len(p.toolsLoaded) > 0 {
			l.emitToolsLoaded(types.ToolsLoadedSourceHarness, p.internalName, p.input, p.toolsLoaded)
		}

		if outcome := stall.recordToolCall(p.call.Name, stallInput(p.call), p.output, p.success); outcome != "" {
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
			// Close spans for calls past the stall point, which already ran
			// in Phase 1 or 2. Their spans keep the outcome, but their
			// trace/metric/transport side effects are intentionally dropped
			// to match the sequential code.
			for j := i + 1; j < len(plan); j++ {
				dropped := &plan[j]
				droppedTrace := dropped.toolCallTrace(dropped.duration())
				dropped.span.End(trace.ToolSpanEnd{Summary: &droppedTrace, EndTime: dropped.finishedAt, Queued: dropped.queued})
			}
			// Truncate to the calls actually observed by the stall
			// detector, matching the sequential code's early return.
			return toolResults[:i+1], toolRecords[:i+1], outcome
		}
	}

	return toolResults, toolRecords, ""
}

// duration is the call's own run time: dispatch to settlement, less the
// time an async call spent queued.
func (p *pendingCall) duration() time.Duration {
	return p.finishedAt.Sub(p.startedAt) - p.queued
}

// toolCallTrace builds the call's trace summary from its settled outcome.
func (p *pendingCall) toolCallTrace(duration time.Duration) types.ToolCallTrace {
	// The bounded failure category feeds the span's error.type and the
	// tool_failures metric.
	errorCategory := ""
	if !p.success && p.failureCategory.IsValid() {
		errorCategory = p.failureCategory.String()
	}

	// Guard-denial and invalid arguments carry their own reason; other
	// failures carry the scrubbed dispatch output (raw error text can echo
	// IAM paths, schema fields, secret-shaped inputs).
	errorReason := ""
	switch {
	case p.errorReason != "":
		errorReason = p.errorReason
	case !p.success:
		errorReason = security.Scrub(p.output)
	}
	// Emit InternalName only when an alias was actually resolved, so the
	// trace wire shape is unchanged under the default profile.
	internalForTrace := ""
	if p.internalName != p.call.Name {
		internalForTrace = p.internalName
	}
	return types.ToolCallTrace{
		ID:            p.call.ID,
		Name:          p.call.Name,
		InternalName:  internalForTrace,
		DurationMs:    duration.Milliseconds(),
		Success:       p.success,
		ErrorReason:   errorReason,
		InputSize:     toolInputSize(p.call),
		OutputSize:    len(p.output),
		ErrorCategory: errorCategory,
	}
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

// scrubToolInputForGuard gives the pre_tool guard the scrubbed copy of a
// call's arguments, as Content and as ToolInput, which stays valid JSON:
// a scrub that breaks the document leaves it a JSON string of the
// scrubbed text. The handler and the permission policy keep input as is.
func scrubToolInputForGuard(in *guard.Input, input json.RawMessage) {
	scrubbed, stats := security.ScrubForGuard(string(input))
	if stats.Count == 0 {
		return
	}
	in.Content = scrubbed
	in.ToolInput = json.RawMessage(scrubbed)
	if !json.Valid(in.ToolInput) {
		quoted, _ := json.Marshal(scrubbed)
		in.ToolInput = quoted
	}
	in.Metadata = map[string]string{guard.MetadataRedactedSpans: strconv.Itoa(stats.Count)}
}
