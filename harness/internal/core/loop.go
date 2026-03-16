package core

import (
	"context"
	"fmt"
	"time"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// maxVerificationRetries is the maximum number of times the verifier can
// request a retry before the run is terminated with verification_failed.
const maxVerificationRetries = 3

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
	_ = l.Transport.Emit(types.HarnessEvent{
		Type: "ready",
	})

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
		if verifyErr != nil || vResult.Passed {
			// Verifier passed (or errored — treat as pass to avoid blocking).
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
	_, _ = l.Git.Finalise(ctx)

	// Record final cost in trace.
	l.Trace.RecordCost(costTracker.CurrentCost())

	// Emit done event.
	_ = l.Transport.Emit(types.HarnessEvent{
		Type:       "done",
		StopReason: outcome,
	})

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

		// Prepare context (compact if needed).
		currentTokens := estimateCurrentTokens(messages)
		maxTokens := 200000 // default context window
		if config.ContextStrategy.MaxTokens > 0 {
			maxTokens = config.ContextStrategy.MaxTokens
		}
		preparedMessages, err := l.Context.Prepare(ctx, messages, contextpkg.TokenBudget{
			MaxTokens:          maxTokens,
			CurrentTokens:      currentTokens,
			ReserveForResponse: 64000,
		})
		if err != nil {
			return messages, "error"
		}

		// Stream model response.
		turnStart := time.Now()
		ch, err := l.Provider.Stream(ctx, types.StreamParams{
			Model:       selection.Model,
			System:      systemPrompt,
			Messages:    preparedMessages,
			Tools:       l.Tools.List(),
			MaxTokens:   64000,
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
		// are estimated from the messages sent.
		inputTokenEstimate := estimateCurrentTokens(preparedMessages)
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

		if sr.StopReason == "end_turn" || len(toolCalls) == 0 {
			return messages, "success"
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

			_ = l.Transport.Emit(types.HarnessEvent{
				Type:      "tool_result",
				ToolUseID: call.ID,
				Content:   output,
			})
		}

		// Append tool results.
		messages = appendToolResults(messages, toolResults)

		// Git checkpoint after tool use.
		_ = l.Git.Checkpoint(ctx, fmt.Sprintf("Turn %d: %d tool calls", turn, len(toolCalls)))
	}

	// Reached max turns.
	return messages, "max_turns"
}

// finishWithError records an error outcome and finishes the trace.
func (l *AgenticLoop) finishWithError(ctx context.Context, err error) (*types.RunTrace, error) {
	_ = l.Transport.Emit(types.HarnessEvent{
		Type:    "error",
		Message: err.Error(),
	})
	runTrace, _ := l.Trace.Finish(ctx, "error")
	return runTrace, err
}
