// Package core implements the agentic loop and factory for the stirrup harness.
package core

import (
	"context"
	"encoding/json"
	"fmt"

	contextpkg "github.com/rubynerd/stirrup/harness/internal/context"
	"github.com/rubynerd/stirrup/harness/internal/edit"
	"github.com/rubynerd/stirrup/harness/internal/executor"
	"github.com/rubynerd/stirrup/harness/internal/git"
	"github.com/rubynerd/stirrup/harness/internal/permission"
	"github.com/rubynerd/stirrup/harness/internal/prompt"
	"github.com/rubynerd/stirrup/harness/internal/provider"
	"github.com/rubynerd/stirrup/harness/internal/router"
	"github.com/rubynerd/stirrup/harness/internal/security"
	"github.com/rubynerd/stirrup/harness/internal/tool"
	"github.com/rubynerd/stirrup/harness/internal/trace"
	"github.com/rubynerd/stirrup/harness/internal/transport"
	"github.com/rubynerd/stirrup/harness/internal/verifier"
	"github.com/rubynerd/stirrup/types"
)

// AgenticLoop drives the ReAct loop. All dependencies are injected as struct
// fields — the loop has no imports from concrete implementations, no environment
// variable reads, no direct file system access.
type AgenticLoop struct {
	Provider    provider.ProviderAdapter
	Router      router.ModelRouter
	Prompt      prompt.PromptBuilder
	Context     contextpkg.ContextStrategy
	Tools       tool.ToolRegistry
	Executor    executor.Executor
	Edit        edit.EditStrategy
	Verifier    verifier.Verifier
	Permissions permission.PermissionPolicy
	Git         git.GitStrategy
	Transport   transport.Transport
	Trace       trace.TraceEmitter
	Security    *security.SecurityLogger // optional, for structured security event logging
}

// CostTracker tracks cumulative cost per run and enforces budgets.
type CostTracker struct {
	totalInputTokens  int
	totalOutputTokens int
	totalCost         float64
}

// RecordTurn records tokens and cost for a single turn.
func (ct *CostTracker) RecordTurn(inputTokens, outputTokens int, pricing types.ModelPricing) {
	ct.totalInputTokens += inputTokens
	ct.totalOutputTokens += outputTokens
	ct.totalCost += float64(inputTokens) / 1_000_000 * pricing.InputPer1M
	ct.totalCost += float64(outputTokens) / 1_000_000 * pricing.OutputPer1M
}

// CurrentCost returns the cumulative cost so far.
func (ct *CostTracker) CurrentCost() float64 {
	return ct.totalCost
}

// Tokens returns the cumulative token usage.
func (ct *CostTracker) Tokens() types.TokenUsage {
	return types.TokenUsage{Input: ct.totalInputTokens, Output: ct.totalOutputTokens}
}

// CheckBudget verifies the run is within configured budgets.
func (ct *CostTracker) CheckBudget(maxCostBudget *float64, maxTokenBudget *int) types.BudgetCheck {
	if maxCostBudget != nil && ct.totalCost > *maxCostBudget {
		return types.BudgetCheck{
			WithinBudget:  false,
			CurrentCost:   ct.totalCost,
			CurrentTokens: ct.Tokens(),
			Reason:        "cost_limit_exceeded",
		}
	}
	totalTokens := ct.totalInputTokens + ct.totalOutputTokens
	if maxTokenBudget != nil && totalTokens > *maxTokenBudget {
		return types.BudgetCheck{
			WithinBudget:  false,
			CurrentCost:   ct.totalCost,
			CurrentTokens: ct.Tokens(),
			Reason:        "token_limit_exceeded",
		}
	}
	return types.BudgetCheck{
		WithinBudget:  true,
		CurrentCost:   ct.totalCost,
		CurrentTokens: ct.Tokens(),
	}
}

// dispatchToolCall executes a single tool call, checking permissions and
// validating input against the tool's JSON Schema. Returns the tool result
// string and whether it succeeded.
func (l *AgenticLoop) dispatchToolCall(ctx context.Context, call types.ToolCall) (string, bool) {
	t := l.Tools.Resolve(call.Name)
	if t == nil {
		return "Unknown tool: " + call.Name, false
	}

	// Validate input against the tool's JSON Schema. This is mandatory and
	// cannot be disabled (VERSION1.md section 7: "Tool input validation").
	if err := security.ValidateJSONSchema(call.Input, t.InputSchema); err != nil {
		if l.Security != nil {
			l.Security.ToolInputRejected(call.Name, []string{err.Error()})
		}
		return fmt.Sprintf("Invalid input for %s: %v", call.Name, err), false
	}

	// Check permissions for side-effecting tools.
	if t.SideEffects {
		result, err := l.Permissions.Check(ctx, t.Definition(), call.Input)
		if err != nil {
			return "Permission check error: " + err.Error(), false
		}
		if !result.Allowed {
			return "Permission denied: " + result.Reason, false
		}
	}

	// Execute the tool handler.
	output, err := t.Handler(ctx, call.Input)
	if err != nil {
		return "Tool error: " + err.Error(), false
	}
	return output, true
}

// buildMessages constructs the initial message list from the user prompt.
func buildMessages(userPrompt string) []types.Message {
	return []types.Message{
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "text", Text: userPrompt},
			},
		},
	}
}

// appendAssistantContent adds the model's response content blocks to the
// message history as an assistant message.
func appendAssistantContent(messages []types.Message, blocks []types.ContentBlock) []types.Message {
	return append(messages, types.Message{
		Role:    "assistant",
		Content: blocks,
	})
}

// appendToolResults adds tool results as a user message (per Anthropic API format).
func appendToolResults(messages []types.Message, results []types.ToolResult) []types.Message {
	blocks := make([]types.ContentBlock, len(results))
	for i, r := range results {
		blocks[i] = types.ContentBlock{
			Type:      "tool_result",
			ToolUseID: r.ToolUseID,
			Content:   r.Content,
			IsError:   r.IsError,
		}
	}
	return append(messages, types.Message{
		Role:    "user",
		Content: blocks,
	})
}

// collectToolCalls extracts tool calls from the model's streamed content blocks.
func collectToolCalls(blocks []types.ContentBlock) []types.ToolCall {
	var calls []types.ToolCall
	for _, b := range blocks {
		if b.Type == "tool_use" {
			calls = append(calls, types.ToolCall{
				ID:    b.ID,
				Name:  b.Name,
				Input: b.Input,
			})
		}
	}
	return calls
}

// streamResult holds the results of consuming a model response stream.
type streamResult struct {
	Blocks       []types.ContentBlock
	StopReason   string
	OutputTokens int
}

// streamEventsToResult consumes a stream event channel and returns the
// accumulated content blocks, final stop reason, and token usage.
func streamEventsToResult(ctx context.Context, ch <-chan types.StreamEvent, tp transport.Transport) (*streamResult, error) {
	result := &streamResult{}
	var currentText string
	inText := false

	for event := range ch {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		switch event.Type {
		case "text_delta":
			if !inText {
				inText = true
				currentText = ""
			}
			currentText += event.Text
			_ = tp.Emit(types.HarnessEvent{
				Type: "text_delta",
				Text: event.Text,
			})

		case "tool_call":
			// Flush any accumulated text block.
			if inText {
				result.Blocks = append(result.Blocks, types.ContentBlock{Type: "text", Text: currentText})
				inText = false
				currentText = ""
			}
			inputBytes, _ := json.Marshal(event.Input)
			result.Blocks = append(result.Blocks, types.ContentBlock{
				Type:  "tool_use",
				ID:    event.ID,
				Name:  event.Name,
				Input: json.RawMessage(inputBytes),
			})

		case "message_complete":
			if inText {
				result.Blocks = append(result.Blocks, types.ContentBlock{Type: "text", Text: currentText})
				inText = false
			}
			result.StopReason = event.StopReason
			result.OutputTokens = event.OutputTokens

		case "error":
			if event.Error != nil {
				return nil, event.Error
			}
		}
	}

	if inText {
		result.Blocks = append(result.Blocks, types.ContentBlock{Type: "text", Text: currentText})
	}

	return result, nil
}

// defaultModelPricing returns pricing for known models.
func defaultModelPricing(model string) types.ModelPricing {
	knownPricing := map[string]types.ModelPricing{
		"claude-sonnet-4-6": {InputPer1M: 3.0, OutputPer1M: 15.0},
		"claude-haiku-4-5":  {InputPer1M: 0.80, OutputPer1M: 4.0},
		"claude-opus-4-6":   {InputPer1M: 15.0, OutputPer1M: 75.0},
	}
	if p, ok := knownPricing[model]; ok {
		return p
	}
	return types.ModelPricing{InputPer1M: 3.0, OutputPer1M: 15.0}
}

// estimateCurrentTokens provides a rough token count for the message history.
func estimateCurrentTokens(messages []types.Message) int {
	total := 0
	for _, msg := range messages {
		for _, block := range msg.Content {
			total += len(block.Text) / 4
			total += len(block.Content) / 4
			total += len(block.Input) / 4
		}
	}
	if total == 0 {
		total = 1
	}
	return total
}
