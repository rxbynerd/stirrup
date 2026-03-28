// Package core implements the agentic loop and factory for the stirrup harness.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// AgenticLoop drives the ReAct loop. All dependencies are injected as struct
// fields — the loop has no imports from concrete implementations, no environment
// variable reads, no direct file system access.
type AgenticLoop struct {
	Provider     provider.ProviderAdapter
	Providers    map[string]provider.ProviderAdapter
	Router       router.ModelRouter
	Prompt       prompt.PromptBuilder
	Context      contextpkg.ContextStrategy
	Tools        tool.ToolRegistry
	Executor     executor.Executor
	Edit         edit.EditStrategy
	Verifier     verifier.Verifier
	Permissions  permission.PermissionPolicy
	Git          git.GitStrategy
	Transport    transport.Transport
	Trace        trace.TraceEmitter
	Security     *security.SecurityLogger // optional, for structured security event logging
	Logger       *slog.Logger             // structured logger with secret scrubbing
	emitReady    bool
	ownedClosers []io.Closer
}

// TokenTracker tracks cumulative token usage per run and enforces token budgets.
// Cost estimation is a control plane concern — the harness only tracks tokens.
type TokenTracker struct {
	totalInputTokens  int
	totalOutputTokens int
}

// RecordTurn records token usage for a single turn.
func (tt *TokenTracker) RecordTurn(inputTokens, outputTokens int) {
	tt.totalInputTokens += inputTokens
	tt.totalOutputTokens += outputTokens
}

// Tokens returns the cumulative token usage.
func (tt *TokenTracker) Tokens() types.TokenUsage {
	return types.TokenUsage{Input: tt.totalInputTokens, Output: tt.totalOutputTokens}
}

// CheckBudget verifies the run is within the configured token budget.
func (tt *TokenTracker) CheckBudget(maxTokenBudget *int) types.BudgetCheck {
	totalTokens := tt.totalInputTokens + tt.totalOutputTokens
	if maxTokenBudget != nil && totalTokens > *maxTokenBudget {
		return types.BudgetCheck{
			WithinBudget:  false,
			CurrentTokens: tt.Tokens(),
			Reason:        "token_limit_exceeded",
		}
	}
	return types.BudgetCheck{
		WithinBudget:  true,
		CurrentTokens: tt.Tokens(),
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
func streamEventsToResult(ctx context.Context, ch <-chan types.StreamEvent, tp transport.Transport, logger *slog.Logger) (*streamResult, error) {
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
			if err := tp.Emit(types.HarnessEvent{
				Type: "text_delta",
				Text: event.Text,
			}); err != nil {
				logger.Warn("transport emit failed", "event", "text_delta", "error", err)
			}

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
			if event.StopReason != "" {
				result.StopReason = event.StopReason
			}
			if event.OutputTokens > 0 {
				result.OutputTokens = event.OutputTokens
			}

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

// Close releases resources owned by the loop, such as container executors,
// internally-created transports, and closable trace emitters.
func (l *AgenticLoop) Close() error {
	var errs []string
	for i := len(l.ownedClosers) - 1; i >= 0; i-- {
		if err := l.ownedClosers[i].Close(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("close loop resources: %s", strings.Join(errs, "; "))
	}
	return nil
}


// estimateCurrentTokens provides a calibrated token count for the message
// history. It accounts for per-message structural overhead, per-block
// overhead, and metadata fields (IDs, names) in addition to content.
func estimateCurrentTokens(messages []types.Message) int {
	total := 0
	for _, msg := range messages {
		total += messageOverheadTokens
		for _, block := range msg.Content {
			total += blockOverheadTokens
			total += len(block.Text) / tokenEstimationDivisor
			total += len(block.Content) / tokenEstimationDivisor
			total += len(block.Input) / tokenEstimationDivisor
			total += len(block.ID) / tokenEstimationDivisor
			total += len(block.Name) / tokenEstimationDivisor
			total += len(block.ToolUseID) / tokenEstimationDivisor
		}
	}
	if total == 0 {
		total = 1
	}
	return total
}

// estimateSystemPromptTokens estimates the token count for the system prompt.
func estimateSystemPromptTokens(systemPrompt string) int {
	return len(systemPrompt)/tokenEstimationDivisor + messageOverheadTokens
}

// estimateToolDefinitionTokens estimates the token count for tool definitions
// that are sent alongside messages in each API call.
func estimateToolDefinitionTokens(tools []types.ToolDefinition) int {
	total := 0
	for _, t := range tools {
		total += toolDefinitionOverheadTokens
		total += len(t.Name) / tokenEstimationDivisor
		total += len(t.Description) / tokenEstimationDivisor
		total += len(t.InputSchema) / tokenEstimationDivisor
	}
	return total
}
