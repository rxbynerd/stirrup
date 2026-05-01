// Package core implements the agentic loop and factory for the stirrup harness.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
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

// DefaultAsyncToolTimeout is the per-call wait used by the async tool
// dispatch path when a tool's AsyncDispatch.Timeout is non-positive.
// Matches permission.DefaultAskUpstreamTimeout for consistency; both are
// "wait for a control-plane response" timeouts.
const DefaultAsyncToolTimeout = 60 * time.Second

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
	Tracer       oteltrace.Tracer         // OTel tracer for loop-level spans (noop when not using OTel)
	TraceContext context.Context          // context carrying the root span for child span parenting
	Metrics      *observability.Metrics   // OTel metric instruments (noop when disabled)
	Security     *security.SecurityLogger // optional, for structured security event logging
	Logger       *slog.Logger             // structured logger with secret scrubbing
	emitReady    bool
	ownedClosers []io.Closer

	// lastContextTokens holds the most recent absolute context-window token
	// estimate for the in-flight run. It is published from runInnerLoop
	// after each Context.Prepare and read from the ContextTokens gauge
	// callback registered in Run. atomic ensures the read in the OTel
	// collection goroutine sees a complete value.
	lastContextTokens atomic.Int64

	// asyncOnce guards lazy construction of asyncCorrelator. The
	// correlator is created and attached to the transport on the first
	// async tool dispatch — most runs never use any async tools and pay
	// no cost. The pointer is held in an atomic so non-dispatcher
	// goroutines (e.g. tests, diagnostics) can read it safely without
	// going through Once.Do.
	asyncOnce       sync.Once
	asyncCorrelator atomic.Pointer[transport.Correlator]
}

// asyncToolResult carries the resolved payload of an async tool call from
// the transport correlator back to the dispatch path.
type asyncToolResult struct {
	content string
	isError bool
}

// extractAsyncToolResult is the PayloadExtractor for tool_result_response
// control events. Returns an empty id (and so is ignored) for any other
// event type.
func extractAsyncToolResult(event types.ControlEvent) (string, any) {
	if event.Type != "tool_result_response" {
		return "", nil
	}
	isErr := event.IsError != nil && *event.IsError
	return event.RequestID, asyncToolResult{
		content: event.Content,
		isError: isErr,
	}
}

// ensureAsyncCorrelator returns the loop's async tool correlator, lazily
// constructing it and attaching it to the loop's transport on first use.
// Safe to call concurrently. Returns nil when the loop's transport has no
// way to deliver responses (NullTransport / nil); callers should handle
// nil by failing the dispatch fast.
func (l *AgenticLoop) ensureAsyncCorrelator() *transport.Correlator {
	if l.Transport == nil || transport.IsNull(l.Transport) {
		return nil
	}
	l.asyncOnce.Do(func() {
		c := transport.NewCorrelator("async-tool")
		c.AttachTo(l.Transport, extractAsyncToolResult)
		l.asyncCorrelator.Store(c)
	})
	return l.asyncCorrelator.Load()
}

// asyncCorrelatorForTest returns the loop's async tool correlator if it
// has been constructed, nil otherwise. Race-safe with concurrent
// dispatchAsyncToolCall calls because the underlying field is an
// atomic.Pointer. Used by tests that want to assert correlator state
// (e.g. PendingCount) without forcing construction.
func (l *AgenticLoop) asyncCorrelatorForTest() *transport.Correlator {
	return l.asyncCorrelator.Load()
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

// traceCtx returns the context carrying the root OTel span, falling back to
// the provided context if no trace context has been set.
func (l *AgenticLoop) traceCtx(fallback context.Context) context.Context {
	if l.TraceContext != nil {
		return l.TraceContext
	}
	return fallback
}

// dispatchToolCall executes a single tool call, checking permissions and
// validating input against the tool's JSON Schema. Returns the tool result
// string and whether it succeeded.
func (l *AgenticLoop) dispatchToolCall(ctx context.Context, call types.ToolCall) (string, bool) {
	t := l.Tools.Resolve(call.Name)
	if t == nil {
		return "Unknown tool: " + call.Name, false
	}

	// Strip prototype-pollution keys before validation so we can both notify
	// the SecurityLogger AND continue validating the cleaned form. ValidateJSONSchema
	// also strips internally; calling here in addition is harmless and gives us
	// a chance to surface the security event. Errors here mean unparseable JSON,
	// which ValidateJSONSchema will report with its own message.
	cleaned, droppedKeys, stripErr := security.StripDangerousKeysFromInput(call.Input)
	if stripErr == nil && len(droppedKeys) > 0 && l.Security != nil {
		l.Security.PrototypePollutionBlocked(call.Name, droppedKeys)
	}
	inputForCall := call.Input
	if stripErr == nil && len(droppedKeys) > 0 {
		inputForCall = cleaned
	}

	// Validate input against the tool's JSON Schema. This is mandatory and
	// cannot be disabled (VERSION1.md section 7: "Tool input validation").
	if err := security.ValidateJSONSchema(inputForCall, t.InputSchema); err != nil {
		if l.Security != nil {
			l.Security.ToolInputRejected(call.Name, []string{err.Error()})
		}
		return fmt.Sprintf("Invalid input for %s: %v", call.Name, err), false
	}

	if findings := security.GuardToolCall(call.Name, t.WorkspaceMutating, call.Input); len(findings) > 0 {
		if l.Security != nil {
			l.Security.ToolCallGuardTriggered(call.Name, findings)
		}
		return fmt.Sprintf("Tool call rejected by security guard for %s", call.Name), false
	}

	// Check permissions for tools that mutate the workspace or that
	// otherwise require upstream approval (e.g. network-touching tools
	// like web_fetch, or budget-consuming tools like spawn_agent). The
	// permission policy decides what to actually do with each flag.
	if t.WorkspaceMutating || t.RequiresApproval {
		_, permSpan := l.Tracer.Start(l.traceCtx(ctx), "permission.check",
			oteltrace.WithAttributes(
				attribute.String("tool.name", call.Name),
			),
		)
		result, err := l.Permissions.Check(ctx, t.Definition(), inputForCall)
		if err != nil {
			permSpan.RecordError(err)
			permSpan.SetStatus(codes.Error, err.Error())
			permSpan.End()
			return "Permission check error: " + err.Error(), false
		}
		permSpan.SetAttributes(attribute.Bool("permission.allowed", result.Allowed))
		permSpan.End()
		if !result.Allowed {
			if l.Security != nil {
				l.Security.PermissionDenied(call.Name, result.Reason)
			}
			return "Permission denied: " + result.Reason, false
		}
	}

	// Async tools resolve their result via the transport correlator: the
	// preflight returns an AsyncDispatch describing the request_id and
	// per-call timeout to use, the loop emits a tool_result_request event,
	// and blocks until a tool_result_response arrives (or ctx is cancelled
	// / the timeout fires). Permission and security checks above already
	// ran and gated dispatch identically to the sync path.
	if t.AsyncHandler != nil {
		return l.dispatchAsyncToolCall(ctx, t, call, inputForCall)
	}

	// Execute the tool handler with the cleaned input so the handler does not
	// see prototype-pollution keys either.
	if t.Handler == nil {
		return fmt.Sprintf("Tool %s has no handler registered", call.Name), false
	}
	output, err := t.Handler(ctx, inputForCall)
	if err != nil {
		return "Tool error: " + err.Error(), false
	}
	return output, true
}

// dispatchAsyncToolCall runs the async tool path:
//
//  1. Refuse the call up front when the transport cannot deliver responses
//     (NullTransport): an async tool here would block until the per-call
//     timeout for nothing. Returning a clear error lets the model recover.
//  2. Invoke the tool's AsyncHandler as a preflight. The handler returns
//     an AsyncDispatch carrying any per-call timeout override; the loop
//     owns the wire request ID via its transport correlator.
//  3. Emit a "tool_result_request" HarnessEvent carrying the request_id,
//     the model's tool_use_id, the tool name, and the input.
//  4. Block on the matching "tool_result_response" via the loop's async
//     correlator under run-context cancellation and the per-call timeout.
//
// The error taxonomy surfaced via the returned content string is documented
// in dispatchAsyncToolCall's call sites (see the constants below); the
// model sees IsError=true on every failure path.
func (l *AgenticLoop) dispatchAsyncToolCall(
	ctx context.Context,
	t *tool.Tool,
	call types.ToolCall,
	inputForCall json.RawMessage,
) (string, bool) {
	correlator := l.ensureAsyncCorrelator()
	if correlator == nil {
		// NullTransport (sub-agent) — no control plane to round-trip
		// through. Fail fast rather than burning the per-call timeout.
		return fmt.Sprintf(
			"async tool %s unavailable: this loop has no live control-plane transport",
			call.Name,
		), false
	}

	dispatch, err := t.AsyncHandler(ctx, inputForCall)
	if err != nil {
		return fmt.Sprintf("async tool %s internal error: %s", call.Name, err.Error()), false
	}

	timeout := dispatch.Timeout
	if timeout <= 0 {
		timeout = DefaultAsyncToolTimeout
	}

	// The correlator allocates the wire request ID. The AsyncDispatch
	// struct intentionally does not expose that ID to tool authors —
	// correlation is a loop concern, single source of truth, and a
	// tool-supplied value would be silently overridden.

	payload, err := correlator.Await(ctx, timeout, func(requestID string) error {
		return l.Transport.Emit(types.HarnessEvent{
			Type:      "tool_result_request",
			RequestID: requestID,
			ToolName:  call.Name,
			ToolUseID: call.ID,
			Input:     inputForCall,
		})
	})
	if err != nil {
		// Distinguish error classes for the model:
		//   - emit failure   → "transport_disconnect"
		//   - timeout        → "async tool timeout"
		//   - ctx cancellation → "async tool cancelled"
		// The correlator wraps emit errors with "emit failed", timeouts
		// with "timed out", and ctx errors carry context.Canceled /
		// context.DeadlineExceeded in the chain.
		if strings.Contains(err.Error(), "emit failed") {
			return fmt.Sprintf("async tool %s transport_disconnect: %s", call.Name, err.Error()), false
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Sprintf("async tool %s cancelled: %s", call.Name, err.Error()), false
		}
		// Default: timeout (correlator: "timed out after ...") and any
		// other unexpected wrapping go here.
		return fmt.Sprintf("async tool %s timeout: %s", call.Name, err.Error()), false
	}

	resp, ok := payload.(asyncToolResult)
	if !ok {
		// Defensive: extractAsyncToolResult only ever delivers
		// asyncToolResult, so reaching this branch means the correlator
		// was wired with a different extractor. Treat as a hard error.
		return fmt.Sprintf("async tool %s internal error: unexpected payload type %T", call.Name, payload), false
	}
	if resp.isError {
		return resp.content, false
	}
	return resp.content, true
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
