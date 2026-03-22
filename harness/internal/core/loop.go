package core

import (
	"context"
	"fmt"
	"log"
	"time"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/router"
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
	if err := l.Git.Setup(ctx, config.Executor.Workspace, config.RunID); err != nil {
		return l.finishWithError(ctx, fmt.Errorf("git setup: %w", err))
	}

	// Initialize message history.
	messages := buildMessages(config.Prompt)

	// Cost tracking.
	costTracker := &CostTracker{}

	// Emit ready event.
	if l.emitReady {
		if err := l.Transport.Emit(types.HarnessEvent{
			Type: "ready",
		}); err != nil {
			log.Printf("warning: transport emit ready: %v", err)
		}
	}

	// Outer verification loop.
	outcome := "success"
	verificationAttempts := 0

	for verificationAttempts <= maxVerificationRetries {
		// Run the inner agentic loop.
		var innerOutcome string
		messages, innerOutcome = l.runInnerLoop(ctx, config, systemPrompt, messages, costTracker)

		if innerOutcome != "success" {
			outcome = innerOutcome
			break
		}

		// Run verifier.
		vResult, verifyErr := l.Verifier.Verify(ctx, verifier.VerifyContext{
			Mode:     config.Mode,
			Executor: l.Executor,
			Messages: messages,
		})
		if verifyErr != nil {
			outcome = "verification_error"
			break
		}
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
	if _, err := l.Git.Finalise(ctx); err != nil {
		log.Printf("warning: git finalise: %v", err)
		_ = l.Transport.Emit(types.HarnessEvent{
			Type:    "warning",
			Message: fmt.Sprintf("git finalise: %v", err),
		})
	}

	// Record final cost in trace.
	l.Trace.RecordCost(costTracker.CurrentCost())

	// Emit done event.
	if err := l.Transport.Emit(types.HarnessEvent{
		Type:       "done",
		StopReason: outcome,
	}); err != nil {
		log.Printf("warning: transport emit done: %v", err)
	}

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
	costTracker *CostTracker,
) ([]types.Message, string) {
	var lastStopReason string

	for turn := 0; turn < config.MaxTurns; turn++ {
		// Check budget before each turn.
		budgetCheck := costTracker.CheckBudget(config.MaxCostBudget, config.MaxTokenBudget)
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
				Input:  costTracker.totalInputTokens,
				Output: costTracker.totalOutputTokens,
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
		preparedMessages, err := l.Context.Prepare(ctx, messages, contextpkg.TokenBudget{
			MaxTokens:          maxTokens,
			CurrentTokens:      currentTokens,
			ReserveForResponse: defaultReserveForResponse,
		})
		if err != nil {
			return messages, "error"
		}

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
		ch, err := selectedProvider.Stream(ctx, types.StreamParams{
			Model:       selection.Model,
			System:      systemPrompt,
			Messages:    preparedMessages,
			Tools:       l.Tools.List(),
			MaxTokens:   defaultReserveForResponse,
			Temperature: 0.1,
		})
		if err != nil {
			// Rollback: don't append anything on error.
			l.Trace.RecordTurn(types.TurnTrace{
				Turn:       turn,
				StopReason: "error",
				DurationMs: time.Since(turnStart).Milliseconds(),
			})
			return messages, "error"
		}

		// Consume stream events.
		sr, streamErr := streamEventsToResult(ctx, ch, l.Transport)
		turnDuration := time.Since(turnStart)

		if streamErr != nil {
			// Rollback on stream error — don't append partial content.
			l.Trace.RecordTurn(types.TurnTrace{
				Turn:       turn,
				StopReason: "error",
				DurationMs: turnDuration.Milliseconds(),
			})
			return messages, "error"
		}

		lastStopReason = sr.StopReason

		// Track token usage. Output tokens come from the stream; input tokens
		// are estimated from the messages sent plus system prompt and tools.
		inputTokenEstimate := estimateCurrentTokens(preparedMessages) +
			estimateSystemPromptTokens(systemPrompt) +
			estimateToolDefinitionTokens(toolDefs)
		pricing := defaultModelPricing(selection.Model)
		costTracker.RecordTurn(inputTokenEstimate, sr.OutputTokens, pricing)

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

		// Append assistant message.
		messages = appendAssistantContent(messages, sr.Blocks)

		// Extract tool calls.
		toolCalls := collectToolCalls(sr.Blocks)

		if sr.StopReason == "end_turn" {
			return messages, "success"
		}
		if sr.StopReason != "tool_use" {
			if sr.StopReason == "" {
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
			callStart := time.Now()

			output, success := l.dispatchToolCall(ctx, call)
			callDuration := time.Since(callStart)

			l.Trace.RecordToolCall(types.ToolCallTrace{
				Name:       call.Name,
				DurationMs: callDuration.Milliseconds(),
				Success:    success,
			})

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
				log.Printf("warning: transport emit tool_result: %v", err)
			}
		}

		// Append tool results.
		messages = appendToolResults(messages, toolResults)

		// Re-check budget after tool results are appended. This prevents the
		// next turn from sending an over-budget context to the provider.
		budgetCheck = costTracker.CheckBudget(config.MaxCostBudget, config.MaxTokenBudget)
		if !budgetCheck.WithinBudget {
			return messages, "budget_exceeded"
		}

		// Git checkpoint after tool use.
		if err := l.Git.Checkpoint(ctx, fmt.Sprintf("Turn %d: %d tool calls", turn, len(toolCalls))); err != nil {
			log.Printf("warning: git checkpoint: %v", err)
			_ = l.Transport.Emit(types.HarnessEvent{
				Type:    "warning",
				Message: fmt.Sprintf("git checkpoint: %v", err),
			})
		}
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

// finishWithError records an error outcome and finishes the trace.
func (l *AgenticLoop) finishWithError(ctx context.Context, err error) (*types.RunTrace, error) {
	if emitErr := l.Transport.Emit(types.HarnessEvent{
		Type:    "error",
		Message: err.Error(),
	}); emitErr != nil {
		log.Printf("warning: transport emit error event: %v", emitErr)
	}
	runTrace, traceErr := l.Trace.Finish(ctx, "error")
	if traceErr != nil {
		log.Printf("warning: trace finish: %v", traceErr)
	}
	return runTrace, err
}
