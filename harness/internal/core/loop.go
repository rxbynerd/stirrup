package core

import (
	"context"
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
	// Start tracing.
	l.Trace.Start(config.RunID, config)

	// Extract the root trace context for child span parenting.
	if otelEmitter, ok := l.Trace.(*trace.OTelTraceEmitter); ok {
		l.TraceContext = otelEmitter.RootContext()
	} else {
		l.TraceContext = ctx
	}

	// Start heartbeat emission so the control plane knows we are alive.
	stopHeartbeat := l.startHeartbeat(ctx, 30*time.Second)

	// Build the system prompt.
	systemPrompt, err := l.Prompt.Build(ctx, prompt.PromptContext{
		Mode:           config.Mode,
		Workspace:      config.Executor.Workspace,
		MaxTurns:       config.MaxTurns,
		DynamicContext: config.DynamicContext,
	})
	if err != nil {
		return l.finishWithError(ctx, fmt.Errorf("build system prompt: %w", err))
	}

	// Set up git workspace.
	_, gitSetupSpan := l.Tracer.Start(l.traceCtx(ctx), "git.setup")
	if err := l.Git.Setup(ctx, config.Executor.Workspace, config.RunID); err != nil {
		gitSetupSpan.RecordError(err)
		gitSetupSpan.SetStatus(codes.Error, err.Error())
		gitSetupSpan.End()
		return l.finishWithError(ctx, fmt.Errorf("git setup: %w", err))
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
	l.Metrics.Runs.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("run.mode", config.Mode),
		),
	)

	// Reset the per-run absolute token estimate before registering the
	// gauge callback so the first observation (before any Context.Prepare)
	// is 0 rather than the value from a previous run.
	l.lastContextTokens.Store(0)

	// Register the ContextTokens observable gauge callback. The callback
	// returns the current absolute token estimate tagged with run.id and
	// run.mode. Unregister at run end so the OTel SDK does not continue
	// observing this run after it has finished.
	unregisterCtxTokens, err := l.Metrics.RegisterContextTokensCallback(func() (int64, []attribute.KeyValue) {
		return l.lastContextTokens.Load(), []attribute.KeyValue{
			attribute.String("run.mode", config.Mode),
			attribute.String("run.id", config.RunID),
		}
	})
	if err != nil {
		l.Logger.Warn("register context_tokens callback failed", "error", err)
	}
	defer unregisterCtxTokens()

	// Outer verification loop.
	outcome := "success"
	verificationAttempts := 0

	for verificationAttempts <= maxVerificationRetries {
		// Run the inner agentic loop.
		var innerOutcome string
		messages, innerOutcome = l.runInnerLoop(ctx, config, systemPrompt, messages, tokenTracker)

		if innerOutcome != "success" {
			outcome = innerOutcome
			break
		}

		// Run verifier.
		l.Metrics.VerificationAttempts.Add(ctx, 1)
		_, verifySpan := l.Tracer.Start(l.traceCtx(ctx), "verifier.verify",
			oteltrace.WithAttributes(
				attribute.Int("verifier.attempt", verificationAttempts),
			),
		)
		vResult, verifyErr := l.Verifier.Verify(ctx, verifier.VerifyContext{
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

	// Finalise git.
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

	// Finish trace.
	runTrace, traceErr := l.Trace.Finish(ctx, outcome)
	if traceErr != nil {
		return nil, fmt.Errorf("finish trace: %w", traceErr)
	}

	return runTrace, nil
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

		// Check context cancellation.
		select {
		case <-ctx.Done():
			return messages, "error"
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
			return messages, "error"
		}
		// Publish the post-Prepare absolute token estimate so the
		// ContextTokens observable gauge callback (registered in Run)
		// observes the live context window utilisation. A successful
		// compaction shrinks the value; new messages grow it.
		tokensAfterPrepare := estimateCurrentTokens(preparedMessages) +
			estimateSystemPromptTokens(systemPrompt) +
			estimateToolDefinitionTokens(toolDefs)
		l.lastContextTokens.Store(int64(tokensAfterPrepare))
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

	loop.Transport.OnControl(func(event types.ControlEvent) {
		switch event.Type {
		case "user_response":
			select {
			case followUpCh <- event.UserResponse:
			default:
				// A follow-up is already queued. Drop this one; the control
				// plane should wait for "done" before sending another request.
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
