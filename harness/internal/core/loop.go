package core

import (
	"context"
	"fmt"
	"time"

	contextpkg "github.com/rubynerd/stirrup/harness/internal/context"
	"github.com/rubynerd/stirrup/harness/internal/prompt"
	"github.com/rubynerd/stirrup/harness/internal/router"
	"github.com/rubynerd/stirrup/harness/internal/verifier"
	"github.com/rubynerd/stirrup/types"
)

// Run executes the agentic loop as described in VERSION1.md:
//
//	repeat {
//	  run agentic loop until model says "done"
//	  run verifier
//	  if verifier passes → done
//	  if retries exhausted → done (with failure)
//	  else → feed verifier feedback back into the loop
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

	// Main agentic loop.
	var lastStopReason string
	outcome := "success"

	for turn := 0; turn < config.MaxTurns; turn++ {
		// Check budget before each turn.
		budgetCheck := costTracker.CheckBudget(config.MaxCostBudget, config.MaxTokenBudget)
		if !budgetCheck.WithinBudget {
			outcome = "budget_exceeded"
			break
		}

		// Check context cancellation.
		select {
		case <-ctx.Done():
			outcome = "error"
		default:
		}
		if outcome != "success" {
			break
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
			outcome = "error"
			break
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
			outcome = "error"
			l.Trace.RecordTurn(types.TurnTrace{
				Turn:       turn,
				StopReason: "error",
				DurationMs: time.Since(turnStart).Milliseconds(),
			})
			break
		}

		// Consume stream events.
		blocks, stopReason, streamErr := streamEventsToResult(ctx, ch, l.Transport)
		turnDuration := time.Since(turnStart)

		if streamErr != nil {
			// Rollback on stream error — don't append partial content.
			outcome = "error"
			l.Trace.RecordTurn(types.TurnTrace{
				Turn:       turn,
				StopReason: "error",
				DurationMs: turnDuration.Milliseconds(),
			})
			break
		}

		lastStopReason = stopReason

		// Record turn in trace.
		l.Trace.RecordTurn(types.TurnTrace{
			Turn:       turn,
			StopReason: stopReason,
			DurationMs: turnDuration.Milliseconds(),
		})

		// Append assistant message.
		messages = appendAssistantContent(messages, blocks)

		// Extract tool calls.
		toolCalls := collectToolCalls(blocks)

		if stopReason == "end_turn" || len(toolCalls) == 0 {
			// Model is done — exit the inner loop.
			break
		}

		// Dispatch tool calls.
		var toolResults []types.ToolResult
		for _, call := range toolCalls {
			callStart := time.Now()

			output, success := l.dispatchToolCall(ctx, call)
			callDuration := time.Since(callStart)

			// Record tool call trace.
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

			// Emit tool result event.
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

		// If we've reached max turns, record it.
		if turn == config.MaxTurns-1 {
			outcome = "max_turns"
		}
	}

	// Run verifier if the loop completed normally.
	if outcome == "success" {
		vResult, verifyErr := l.Verifier.Verify(ctx, verifier.VerifyContext{
			Mode:     config.Mode,
			Executor: l.Executor,
			Messages: messages,
		})
		if verifyErr == nil && !vResult.Passed {
			outcome = "verification_failed"
		}
	}

	// Finalise git.
	_, _ = l.Git.Finalise(ctx)

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

// finishWithError records an error outcome and finishes the trace.
func (l *AgenticLoop) finishWithError(ctx context.Context, err error) (*types.RunTrace, error) {
	_ = l.Transport.Emit(types.HarnessEvent{
		Type:    "error",
		Message: err.Error(),
	})
	runTrace, _ := l.Trace.Finish(ctx, "error")
	return runTrace, err
}
