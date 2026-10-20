package core

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/types"
)

// fitPreparedRequest enforces the context-fit postcondition on a prepared
// request: its estimate (messages, system prompt and tool definitions) must
// not exceed contextpkg.FitLimit. An oversized request has its tool result
// content truncated in a copy, oldest first; the stored history is never
// touched. It returns the messages to send and their estimated size, or an
// error naming the sizes involved when even full truncation does not fit.
func (l *AgenticLoop) fitPreparedRequest(
	ctx context.Context,
	prepared []types.Message,
	systemPrompt string,
	toolDefs []types.ToolDefinition,
	budget turnBudget,
	span oteltrace.Span,
) ([]types.Message, int, error) {
	systemTokens := estimateSystemPromptTokens(systemPrompt)
	toolTokens := estimateLoadedToolDefinitionTokens(toolDefs, l.deferredToolLoaded)
	tokens := estimateCurrentTokens(prepared, budget.structuredResults) + systemTokens + toolTokens
	limit := contextpkg.FitLimit(budget.prepare)
	if tokens <= limit {
		return prepared, tokens, nil
	}

	fitted, cut := contextpkg.TruncateToolResults(prepared, tokens-limit)
	if cut > 0 {
		l.historyRewritten = true
		fitted = types.StripThinkingBlocks(fitted)
		before := tokens
		tokens = estimateCurrentTokens(fitted, budget.structuredResults) + systemTokens + toolTokens
		span.SetAttributes(attribute.Int("context.fit.truncated_results", cut))
		l.Logger.WarnContext(ctx, "tool results truncated to fit the context budget",
			"toolResults", cut,
			"tokens.before", before,
			"tokens.after", tokens,
			"limit", limit,
		)
	}
	if tokens <= limit {
		return fitted, tokens, nil
	}

	taskTokens := 0
	if len(fitted) > 0 {
		taskTokens = estimateCurrentTokens(fitted[:1], budget.structuredResults)
	}
	return fitted, tokens, fmt.Errorf(
		"context budget exhausted: the request needs about %d estimated tokens (system prompt %d, tool definitions %d, task %d, other messages %d after truncating %d tool results) "+
			"but %s; %s or shorten the system prompt, the tool set or the task",
		tokens, systemTokens, toolTokens, taskTokens, tokens-systemTokens-toolTokens-taskTokens, cut,
		budget.describeLimit(), budget.window.remedy,
	)
}
