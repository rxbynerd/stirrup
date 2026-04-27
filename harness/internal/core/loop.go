package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// outcomeCtxDone is a sentinel outcome returned by runInnerLoop when the
// loop observes ctx.Done(). The outer Run loop inspects context.Cause to
// translate this into a user-visible outcome: "cancelled" (control plane
// or plain/signal cancel), "timeout" (deadline), or "error" (non-nil but
// unrecognised cause).
const outcomeCtxDone = "_ctx_done"

const (
	// maxVerificationRetries is the maximum number of times the verifier can
	// request a retry before the run is terminated with verification_failed.
	maxVerificationRetries = 3

	// defaultMaxContextTokens is the assumed context window size when the
	// RunConfig does not specify one explicitly.
	defaultMaxContextTokens = 200_000

	// defaultReserveForResponse is the number of tokens reserved for the
	// model's response within the context window.
	defaultReserveForResponse = 64_000

	// tokenEstimationDivisor is the approximate character-to-token ratio
	// used by token estimation functions (≈4 characters per token).
	tokenEstimationDivisor = 4

	// messageOverheadTokens accounts for the JSON structure around each
	// message (role field, content array wrapper, separators).
	messageOverheadTokens = 4

	// blockOverheadTokens accounts for the JSON structure around each
	// content block (type field, object braces, separators).
	blockOverheadTokens = 3

	// toolDefinitionOverheadTokens accounts for the structural JSON
	// wrapping each tool definition (type, function wrapper, field keys).
	toolDefinitionOverheadTokens = 10
)

// Run executes the agentic loop as described in VERSION1.md:
//
//	repeat {
//	  run agentic loop until model says "done"
//	  run verifier
//	  if verifier passes → done
//	  if retries exhausted → done (with failure)
//	  else → feed verifier feedback back into the loop as a user message
//	}
func (l *AgenticLoop) Run(ctx context.Context, config *types.RunConfig) (*types.RunTrace, error) {
	// Derive a cancellable context so a "cancel" ControlEvent can abort the
	// run within one turn boundary. WithCancelCause lets us disambiguate
	// control-plane cancellation from deadline expiry and caller cancellation
	// later via context.Cause().
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)

	// Register a cancel handler on the transport. Fan-out OnControl is
	// supported by all production transports (stdio, gRPC); sub-agents use
	// NullTransport whose OnControl is a no-op, so this is a harmless no-op
	// in the sub-agent case.
	l.Transport.OnControl(func(event types.ControlEvent) {
		if event.Type == "cancel" {
			cancelRun(ErrCancelledByControlPlane)
		}
	})

	// Start tracing.
	l.Trace.Start(config.RunID, config)

	// Extract the root trace context for child span parenting.
	if otelEmitter, ok := l.Trace.(*trace.OTelTraceEmitter); ok {
		l.TraceContext = otelEmitter.RootContext()
	} else {
		l.TraceContext = runCtx
	}

	// Start heartbeat emission so the control plane knows we are alive.
	stopHeartbeat := l.startHeartbeat(runCtx, 30*time.Second)

	// Build the system prompt.
	systemPrompt, err := l.Prompt.Build(runCtx, prompt.PromptContext{
		Mode:           config.Mode,
		Workspace:      config.Executor.Workspace,
		MaxTurns:       config.MaxTurns,
		DynamicContext: config.DynamicContext,
	})
	if err != nil {
		return l.finishWithError(runCtx, fmt.Errorf("build system prompt: %w", err))
	}

	// Set up git workspace.
	_, gitSetupSpan := l.Tracer.Start(l.traceCtx(runCtx), "git.setup")
	if err := l.Git.Setup(runCtx, config.Executor.Workspace, config.RunID); err != nil {
		gitSetupSpan.RecordError(err)
		gitSetupSpan.SetStatus(codes.Error, err.Error())
		gitSetupSpan.End()
		return l.finishWithError(runCtx, fmt.Errorf("git setup: %w", err))
	}
	gitSetupSpan.End()

	// Initialize message history.
	messages := buildMessages(config.Prompt)

	// Token tracking (cost estimation is a control plane concern).
	tokenTracker := &TokenTracker{}

	// Emit ready event.
	if l.emitReady {
		if err := l.Transport.Emit(types.HarnessEvent{
			Type: "ready",
		}); err != nil {
			l.Logger.Warn("transport emit failed", "event", "ready", "error", err)
		}
	}

	l.Logger.Info("run started", "mode", config.Mode, "maxTurns", config.MaxTurns)

	runStart := time.Now()
	l.Metrics.Runs.Add(runCtx, 1,
		metric.WithAttributes(
			attribute.String("run.mode", config.Mode),
		),
	)

	// Outer verification loop.
	outcome := "success"
	verificationAttempts := 0

	for verificationAttempts <= maxVerificationRetries {
		// Run the inner agentic loop.
		var innerOutcome string
		messages, innerOutcome = l.runInnerLoop(runCtx, config, systemPrompt, messages, tokenTracker)

		if innerOutcome != "success" {
			outcome = innerOutcome
			break
		}

		// Run verifier.
		l.Metrics.VerificationAttempts.Add(runCtx, 1)
		_, verifySpan := l.Tracer.Start(l.traceCtx(runCtx), "verifier.verify",
			oteltrace.WithAttributes(
				attribute.Int("verifier.attempt", verificationAttempts),
			),
		)
		vResult, verifyErr := l.Verifier.Verify(runCtx, verifier.VerifyContext{
			Mode:     config.Mode,
			Executor: l.Executor,
			Messages: messages,
		})
		if verifyErr != nil {
			verifySpan.RecordError(verifyErr)
			verifySpan.SetStatus(codes.Error, verifyErr.Error())
			verifySpan.End()
			outcome = "verification_error"
			break
		}
		verifySpan.SetAttributes(attribute.Bool("verifier.passed", vResult.Passed))
		verifySpan.End()
		if vResult.Passed {
			outcome = "success"
			break
		}

		// Verification failed.
		verificationAttempts++
		if verificationAttempts > maxVerificationRetries {
			outcome = "verification_failed"
			break
		}

		// Feed verifier feedback back into the loop as a user message.
		feedback := vResult.Feedback
		if feedback == "" {
			feedback = "Verification failed. Please review and fix the issues."
		}
		messages = append(messages, types.Message{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "text", Text: feedback},
			},
		})
	}

	// Cancellation wins over verification-path outcomes. A cancel arriving
	// between the inner loop returning and Verify completing can otherwise
	// cause Verify to return a ctx-cancelled error and set
	// outcome="verification_error", masking the true termination reason on
	// the wire. If the run context is done, reclassify so the cancel/timeout
	// path below runs.
	if runCtx.Err() != nil && outcome != outcomeCtxDone {
		outcome = outcomeCtxDone
	}

	// If the inner loop exited because the context was cancelled, inspect
	// the cause to distinguish control-plane cancellation ("cancelled"),
	// deadline expiry ("timeout"), plain/signal cancel ("cancelled"), and
	// anything else ("error").
	if outcome == outcomeCtxDone {
		cause := context.Cause(runCtx)
		outcome = classifyCtxOutcome(cause)
		l.setRootCancelAttribute(cause)
	}

	// Finalise git. Use the parent ctx here: if the run was cancelled, we
	// still want git.Finalise to be able to persist whatever state exists.
	_, finaliseSpan := l.Tracer.Start(l.traceCtx(ctx), "git.finalise")
	if _, err := l.Git.Finalise(ctx); err != nil {
		finaliseSpan.RecordError(err)
		l.Logger.Warn("git finalise failed", "error", err)
		_ = l.Transport.Emit(types.HarnessEvent{
			Type:    "warning",
			Message: fmt.Sprintf("git finalise: %v", err),
		})
	}
	finaliseSpan.End()

	l.Logger.Info("run finished", "outcome", outcome)

	l.Metrics.RunDuration.Record(ctx, float64(time.Since(runStart).Milliseconds()),
		metric.WithAttributes(
			attribute.String("run.mode", config.Mode),
			attribute.String("run.outcome", outcome),
		),
	)

	// Emit done event.
	if err := l.Transport.Emit(types.HarnessEvent{
		Type:       "done",
		StopReason: outcome,
	}); err != nil {
		l.Logger.Warn("transport emit failed", "event", "done", "error", err)
	}

	// Stop heartbeat before finishing the trace.
	stopHeartbeat()

	// Finish trace using the parent ctx — the trace exporter's ForceFlush
	// should still have a usable deadline even if the run-scoped ctx is
	// already cancelled.
	runTrace, traceErr := l.Trace.Finish(ctx, outcome)
	if traceErr != nil {
		return nil, fmt.Errorf("finish trace: %w", traceErr)
	}

	return runTrace, nil
}

// classifyCtxOutcome maps a context cancellation cause onto the outcome
// string reported on the "done" event and recorded in RunTrace.Outcome.
//
// A nil cause or a bare context.Canceled indicates the run was cancelled
// via a plain cancel() without a cause attached — e.g. SIGINT/SIGTERM via
// the root cobra signal handler, or a caller invoking context.WithCancel
// on a parent and then cancel() (which propagates context.Canceled as the
// cause of our WithCancelCause child). The spec treats this as a
// user-initiated cancellation, distinct from a deadline-driven timeout or
// an internal error. A non-nil cause that is neither a recognised cancel
// sentinel nor a deadline is surfaced as "error" since we cannot attribute
// it to a known cancel or timeout path.
func classifyCtxOutcome(cause error) string {
	switch {
	case errors.Is(cause, ErrCancelledByControlPlane):
		return "cancelled"
	case errors.Is(cause, context.DeadlineExceeded):
		return "timeout"
	case cause == nil, errors.Is(cause, context.Canceled):
		return "cancelled"
	default:
		return "error"
	}
}

// setRootCancelAttribute tags the root "run" OTel span with the reason for
// context cancellation so operators can filter cancelled runs from timed-out
// or errored runs in tracing backends. Only applied when the run actually
// ended via ctx cancellation.
//
// The attribute is derived from the context cause so that a plain/signal
// cancel and a control-plane cancel are distinguished on the span even
// though both map to outcome="cancelled".
//
//	run.cancelled_by="control_plane" — ErrCancelledByControlPlane cause
//	run.cancelled_by="deadline"      — context.DeadlineExceeded cause
//	run.cancelled_by="signal"        — nil cause or bare context.Canceled
//	                                   (plain cancel(), SIGINT, etc.)
//	(no attribute)                   — non-nil unrecognised cause ("error")
func (l *AgenticLoop) setRootCancelAttribute(cause error) {
	otelEmitter, ok := l.Trace.(*trace.OTelTraceEmitter)
	if !ok {
		return
	}
	span := oteltrace.SpanFromContext(otelEmitter.RootContext())
	if !span.SpanContext().IsValid() {
		return
	}
	var reason string
	switch {
	case errors.Is(cause, ErrCancelledByControlPlane):
		reason = "control_plane"
	case errors.Is(cause, context.DeadlineExceeded):
		reason = "deadline"
	case cause == nil, errors.Is(cause, context.Canceled):
		reason = "signal"
	default:
		// Non-nil unrecognised cause → outcome=="error"; no attribute.
		return
	}
	span.SetAttributes(attribute.String("run.cancelled_by", reason))
}

// runInnerLoop runs the agentic loop turns until the model says "done",
// max turns is reached, budget is exceeded, or an error occurs.
// Returns the updated messages and the outcome.
func (l *AgenticLoop) runInnerLoop(
	ctx context.Context,
	config *types.RunConfig,
	systemPrompt string,
	messages []types.Message,
	tokenTracker *TokenTracker,
) ([]types.Message, string) {
	var lastStopReason string
	stall := &stallDetector{}

	for turn := 0; turn < config.MaxTurns; turn++ {
		l.Logger.Info("turn started", "turn", turn)

		// Check budget before each turn.
		budgetCheck := tokenTracker.CheckBudget(config.MaxTokenBudget)
		if !budgetCheck.WithinBudget {
			return messages, "budget_exceeded"
		}

		// Check context cancellation. Return a sentinel outcome so the
		// outer Run loop can distinguish control-plane cancellation,
		// deadline expiry, and caller-initiated cancellation via
		// context.Cause().
		select {
		case <-ctx.Done():
			return messages, outcomeCtxDone
		default:
		}

		// Select model for this turn.
		selection := l.Router.Select(ctx, router.RouterContext{
			Mode:           config.Mode,
			Turn:           turn,
			LastStopReason: lastStopReason,
			TokenUsage: router.TokenUsage{
				Input:  tokenTracker.Tokens().Input,
				Output: tokenTracker.Tokens().Output,
			},
		})

		// Prepare context (compact if needed). Token estimate includes
		// system prompt and tool definitions — these consume context but
		// aren't in the message history.
		toolDefs := l.Tools.List()
		currentTokens := estimateCurrentTokens(messages) +
			estimateSystemPromptTokens(systemPrompt) +
			estimateToolDefinitionTokens(toolDefs)
		maxTokens := defaultMaxContextTokens
		if config.ContextStrategy.MaxTokens > 0 {
			maxTokens = config.ContextStrategy.MaxTokens
		}
		_, contextSpan := l.Tracer.Start(l.traceCtx(ctx), "context.prepare",
			oteltrace.WithAttributes(
				attribute.Int("messages.before", len(messages)),
				attribute.Int("tokens.before", currentTokens),
			),
		)
		preparedMessages, err := l.Context.Prepare(ctx, messages, contextpkg.TokenBudget{
			MaxTokens:          maxTokens,
			CurrentTokens:      currentTokens,
			ReserveForResponse: defaultReserveForResponse,
		})
		if err != nil {
			contextSpan.RecordError(err)
			contextSpan.SetStatus(codes.Error, err.Error())
			contextSpan.End()
			if ctx.Err() != nil {
				return messages, outcomeCtxDone
			}
			return messages, "error"
		}
		contextSpan.SetAttributes(attribute.Int("messages.after", len(preparedMessages)))
		if compaction := l.Context.LastCompaction(); compaction != nil {
			contextSpan.SetAttributes(
				attribute.String("context.strategy", compaction.Strategy),
				attribute.Int("context.tokens.after", compaction.TokensAfter),
			)
			l.Metrics.ContextCompactions.Add(ctx, 1,
				metric.WithAttributes(attribute.String("context.strategy", compaction.Strategy)),
			)
			l.Logger.Info("context compacted",
				"strategy", compaction.Strategy,
				"messages.before", compaction.MessagesBefore,
				"messages.after", compaction.MessagesAfter,
				"tokens.before", compaction.TokensBefore,
				"tokens.after", compaction.TokensAfter,
			)
		}
		contextSpan.End()

		// Stream model response.
		turnStart := time.Now()
		selectedProvider := l.Provider
		if selection.Provider != "" && len(l.Providers) > 0 {
			prov, ok := l.Providers[selection.Provider]
			if !ok {
				l.Trace.RecordTurn(types.TurnTrace{
					Turn:       turn,
					StopReason: "error",
					DurationMs: time.Since(turnStart).Milliseconds(),
				})
				return messages, "error"
			}
			selectedProvider = prov
		}
		if selectedProvider == nil {
			l.Trace.RecordTurn(types.TurnTrace{
				Turn:       turn,
				StopReason: "error",
				DurationMs: time.Since(turnStart).Milliseconds(),
			})
			return messages, "error"
		}
		providerAttrs := metric.WithAttributes(
			attribute.String("provider.type", selection.Provider),
			attribute.String("provider.model", selection.Model),
		)
		l.Metrics.ProviderRequests.Add(ctx, 1, providerAttrs)

		spanCtx, providerSpan := l.Tracer.Start(l.traceCtx(ctx), "provider.stream",
			oteltrace.WithAttributes(
				attribute.String("provider.type", selection.Provider),
				attribute.String("provider.model", selection.Model),
				attribute.Int("turn.number", turn),
			),
		)

		ch, err := selectedProvider.Stream(spanCtx, types.StreamParams{
			Model:       selection.Model,
			System:      systemPrompt,
			Messages:    preparedMessages,
			Tools:       l.Tools.List(),
			MaxTokens:   defaultReserveForResponse,
			Temperature: 0.1,
		})
		if err != nil {
			providerSpan.RecordError(err)
			providerSpan.SetStatus(codes.Error, err.Error())
			providerSpan.End()
			// Rollback: don't append anything on error.
			l.Metrics.ProviderErrors.Add(ctx, 1, providerAttrs)
			l.Trace.RecordTurn(types.TurnTrace{
				Turn:       turn,
				StopReason: "error",
				DurationMs: time.Since(turnStart).Milliseconds(),
			})
			// If the provider call failed because the run context was
			// cancelled, surface that so the outer loop can classify the
			// outcome as cancelled/timeout rather than a generic error.
			if ctx.Err() != nil {
				return messages, outcomeCtxDone
			}
			return messages, "error"
		}

		// Consume stream events.
		sr, streamErr := streamEventsToResult(ctx, ch, l.Transport, l.Logger)
		turnDuration := time.Since(turnStart)

		if streamErr != nil {
			providerSpan.RecordError(streamErr)
			providerSpan.SetStatus(codes.Error, streamErr.Error())
			providerSpan.End()
			// Rollback on stream error — don't append partial content.
			l.Metrics.ProviderErrors.Add(ctx, 1, providerAttrs)
			l.Trace.RecordTurn(types.TurnTrace{
				Turn:       turn,
				StopReason: "error",
				DurationMs: turnDuration.Milliseconds(),
			})
			// Distinguish stream-abort-due-to-ctx from other stream errors
			// so the outer loop can classify the outcome correctly.
			if ctx.Err() != nil {
				return messages, outcomeCtxDone
			}
			return messages, "error"
		}
		providerSpan.SetAttributes(
			attribute.Int("tokens.output", sr.OutputTokens),
			attribute.String("stop_reason", sr.StopReason),
		)
		providerSpan.End()

		lastStopReason = sr.StopReason

		// Track token usage. Output tokens come from the stream; input tokens
		// are estimated from the messages sent plus system prompt and tools.
		inputTokenEstimate := estimateCurrentTokens(preparedMessages) +
			estimateSystemPromptTokens(systemPrompt) +
			estimateToolDefinitionTokens(toolDefs)
		tokenTracker.RecordTurn(inputTokenEstimate, sr.OutputTokens)

		// Record turn in trace.
		l.Trace.RecordTurn(types.TurnTrace{
			Turn: turn,
			Tokens: types.TokenUsage{
				Input:  inputTokenEstimate,
				Output: sr.OutputTokens,
			},
			StopReason: sr.StopReason,
			DurationMs: turnDuration.Milliseconds(),
		})

		modeAttr := metric.WithAttributes(attribute.String("run.mode", config.Mode))
		l.Metrics.Turns.Add(ctx, 1, modeAttr)
		l.Metrics.TokensInput.Add(ctx, int64(inputTokenEstimate))
		l.Metrics.TokensOutput.Add(ctx, int64(sr.OutputTokens))
		l.Metrics.TurnDuration.Record(ctx, float64(turnDuration.Milliseconds()), modeAttr)

		l.Logger.Info("turn completed", "turn", turn,
			"tokens.input", inputTokenEstimate,
			"tokens.output", sr.OutputTokens,
			"stopReason", sr.StopReason)

		// Append assistant message.
		messages = appendAssistantContent(messages, sr.Blocks)

		// Extract tool calls.
		toolCalls := collectToolCalls(sr.Blocks)

		if sr.StopReason == "end_turn" {
			return messages, "success"
		}
		if sr.StopReason != "tool_use" {
			if sr.StopReason == "" {
				l.Logger.Warn("provider returned empty stop reason", "turn", turn)
				return messages, "error"
			}
			return messages, sr.StopReason
		}
		if len(toolCalls) == 0 {
			return messages, "error"
		}

		// Dispatch tool calls.
		var toolResults []types.ToolResult
		for _, call := range toolCalls {
			l.Logger.Info("tool dispatched", "tool", call.Name)
			callStart := time.Now()

			_, toolSpan := l.Tracer.Start(l.traceCtx(ctx), "tool."+call.Name,
				oteltrace.WithAttributes(
					attribute.String("tool.name", call.Name),
					attribute.Int("tool.input_size", len(call.Input)),
				),
			)

			output, success := l.dispatchToolCall(ctx, call)
			callDuration := time.Since(callStart)

			toolSpan.SetAttributes(
				attribute.Bool("tool.success", success),
				attribute.Int("tool.output_size", len(output)),
				attribute.Int64("tool.duration_ms", callDuration.Milliseconds()),
			)
			if !success {
				toolSpan.SetStatus(codes.Error, "tool call failed")
			}
			toolSpan.End()

			errorReason := ""
			if !success {
				errorReason = output
			}
			l.Trace.RecordToolCall(types.ToolCallTrace{
				Name:        call.Name,
				DurationMs:  callDuration.Milliseconds(),
				Success:     success,
				ErrorReason: errorReason,
				InputSize:   len(call.Input),
				OutputSize:  len(output),
			})

			toolNameAttr := metric.WithAttributes(attribute.String("tool.name", call.Name))
			l.Metrics.ToolCalls.Add(ctx, 1, toolNameAttr)
			l.Metrics.ToolCallDuration.Record(ctx, float64(callDuration.Milliseconds()), toolNameAttr)
			if !success {
				l.Metrics.ToolErrors.Add(ctx, 1, toolNameAttr)
			}

			toolResults = append(toolResults, types.ToolResult{
				ToolUseID: call.ID,
				Content:   output,
				IsError:   !success,
			})

			if err := l.Transport.Emit(types.HarnessEvent{
				Type:      "tool_result",
				ToolUseID: call.ID,
				Content:   output,
			}); err != nil {
				l.Logger.Warn("transport emit failed", "event", "tool_result", "error", err)
			}

			// Check for stall conditions after each tool call.
			if outcome := stall.recordToolCall(call.Name, call.Input, success); outcome != "" {
				l.Metrics.Stalls.Add(ctx, 1,
					metric.WithAttributes(attribute.String("run.mode", config.Mode)),
				)
				messages = appendToolResults(messages, toolResults)
				return messages, outcome
			}
		}

		// Append tool results.
		messages = appendToolResults(messages, toolResults)

		// Re-check budget after tool results are appended. This prevents the
		// next turn from sending an over-budget context to the provider.
		budgetCheck = tokenTracker.CheckBudget(config.MaxTokenBudget)
		if !budgetCheck.WithinBudget {
			return messages, "budget_exceeded"
		}

		// Git checkpoint after tool use.
		_, checkpointSpan := l.Tracer.Start(l.traceCtx(ctx), "git.checkpoint")
		if err := l.Git.Checkpoint(ctx, fmt.Sprintf("Turn %d: %d tool calls", turn, len(toolCalls))); err != nil {
			checkpointSpan.RecordError(err)
			l.Logger.Warn("git checkpoint failed", "error", err)
			_ = l.Transport.Emit(types.HarnessEvent{
				Type:    "warning",
				Message: fmt.Sprintf("git checkpoint: %v", err),
			})
		}
		checkpointSpan.End()
	}

	// Reached max turns.
	return messages, "max_turns"
}

// RunFollowUpLoop waits for follow-up user_response control events on the
// transport after the primary run has completed. For each follow-up it
// re-runs the agentic loop with the new prompt. The loop exits when the
// grace period timer fires with no new request, the context is cancelled,
// or a "cancel" control event arrives.
//
// graceSecs must be > 0. The transport must support fan-out OnControl
// registration (both GRPCTransport and StdioTransport do).
func RunFollowUpLoop(ctx context.Context, loop *AgenticLoop, config *types.RunConfig, graceSecs int) {
	followUpCh := make(chan string, 1)
	cancelCh := make(chan struct{}, 1)

	loop.Transport.OnControl(func(event types.ControlEvent) {
		switch event.Type {
		case "user_response":
			select {
			case followUpCh <- event.UserResponse:
			default:
				// A follow-up is already queued. Drop this one; the control
				// plane should wait for "done" before sending another request.
			}
		case "cancel":
			// Exit the grace window immediately on cancel. Any in-flight
			// Run invocation has its own cancel handler and will terminate
			// on the next turn boundary.
			select {
			case cancelCh <- struct{}{}:
			default:
			}
		}
	})

	grace := time.Duration(graceSecs) * time.Second
	timer := time.NewTimer(grace)
	defer timer.Stop()

	for {
		select {
		case newPrompt := <-followUpCh:
			// Reset the grace period for the next idle window.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(grace)

			// Issue a fresh run ID so traces don't collide.
			config.RunID = fmt.Sprintf("run-%d", time.Now().UnixNano())
			config.Prompt = newPrompt

			if _, err := loop.Run(ctx, config); err != nil {
				// Transport already carries the error event from finishWithError.
				return
			}

		case <-cancelCh:
			return

		case <-timer.C:
			return

		case <-ctx.Done():
			return
		}
	}
}

// startHeartbeat launches a background goroutine that emits heartbeat events
// at the given interval. Returns a cancel function that stops emission.
func (l *AgenticLoop) startHeartbeat(ctx context.Context, interval time.Duration) context.CancelFunc {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = l.Transport.Emit(types.HarnessEvent{Type: "heartbeat"})
			}
		}
	}()
	return cancel
}

// finishWithError records an error outcome and finishes the trace.
func (l *AgenticLoop) finishWithError(ctx context.Context, err error) (*types.RunTrace, error) {
	if emitErr := l.Transport.Emit(types.HarnessEvent{
		Type:    "error",
		Message: err.Error(),
	}); emitErr != nil {
		l.Logger.Warn("transport emit failed", "event", "error", "error", emitErr)
	}
	runTrace, traceErr := l.Trace.Finish(ctx, "error")
	if traceErr != nil {
		l.Logger.Warn("trace finish failed", "error", traceErr)
	}
	return runTrace, err
}
