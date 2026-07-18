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
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/hook"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/ruleoftwo"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// DefaultAsyncToolTimeout is the per-call wait used by the async tool
// dispatch path when a tool's AsyncDispatch.Timeout is non-positive.
// Matches permission.DefaultAskUpstreamTimeout.
const DefaultAsyncToolTimeout = 60 * time.Second

// maxAsyncToolResultBytes caps control-plane-supplied tool result content;
// the control plane is partially trusted, so an unbounded string here is a
// DoS vector (CWE-400). Matches the run_command sync tool's cap.
const maxAsyncToolResultBytes = 1 << 20 // 1MB

// asyncResultTruncationSuffix is appended when content exceeds
// maxAsyncToolResultBytes.
const asyncResultTruncationSuffix = "... [truncated by harness]"

// AgenticLoop drives the ReAct loop. All dependencies are injected as struct
// fields — the loop has no imports from concrete implementations, no environment
// variable reads, no direct file system access.
type AgenticLoop struct {
	Provider  provider.ProviderAdapter
	Providers map[string]provider.ProviderAdapter
	Router    router.ModelRouter
	Prompt    prompt.PromptBuilder
	Context   contextpkg.ContextStrategy
	Tools     tool.ToolRegistry
	// ToolProfile is the resolved toolset-profile presentation applied to
	// Tools; nil means the default (identity) profile. When Tools is a
	// *tool.Presenter its Profile() must equal ToolProfile — the factory
	// guarantees this by construction, but a hand-assembled loop (tests,
	// embedders) must uphold it manually. See docs/architecture.md.
	ToolProfile *tool.Profile
	Executor    executor.Executor
	Edit        edit.EditStrategy
	Verifier    verifier.Verifier
	Permissions permission.PermissionPolicy
	Git         git.GitStrategy
	GuardRail   guard.GuardRail
	// RuleOfTwo is the Rule-of-Two runtime sensitive-data monitor.
	// Enforcement keys on its run-scoped latch. The factory injects
	// ruleoftwo.NewNoop() when the run is unarmed so call sites stay
	// unconditional; a hand-assembled loop may leave it nil.
	RuleOfTwo  ruleoftwo.Monitor
	Escalation EscalationPolicy // tool-choice missed-tool recovery; nil = disabled
	// Hooks runs the run's configured lifecycle hooks; optional, like
	// GuardRail/RuleOfTwo/Escalation. The factory always injects a
	// non-nil value. See docs/configuration.md#lifecycle-hooks.
	Hooks hook.Runner
	// Shutdown, when non-nil, is a process-lifetime context done when the
	// harness receives a process-level shutdown signal (SIGTERM/SIGINT/pod
	// deletion) — independent of Run()'s ctx, which carries the run's own
	// deadline and control-plane cancel. The detached postRun hook phase
	// races its bounded budget against Shutdown so it can survive a
	// run-deadline/control-plane cancel while still observing a genuine
	// process shutdown promptly. See docs/architecture.md. Nil-safe.
	Shutdown     context.Context
	Transport    transport.Transport
	Trace        trace.TraceEmitter
	Tracer       oteltrace.Tracer         // OTel tracer for loop-level spans (noop when not using OTel)
	TraceContext context.Context          // context carrying the root span for child span parenting
	Metrics      *observability.Metrics   // OTel metric instruments (noop when disabled)
	Security     *security.SecurityLogger // optional, for structured security event logging
	Logger       *slog.Logger             // structured logger with secret scrubbing
	// MetricAttrs is prepended to every metric observation from this loop.
	// Empty for top-level runs; SpawnSubAgent populates it on child loops
	// with subagent=true and parent.run_id so dashboards can decompose a
	// run into parent vs child observations.
	MetricAttrs  []attribute.KeyValue
	emitReady    bool
	ownedClosers []io.Closer

	// lastContextTokens holds the most recent absolute context-window token
	// estimate, published after each Context.Prepare and read from the
	// ContextTokens gauge callback. atomic ensures a complete value is seen
	// by the OTel collection goroutine.
	lastContextTokens atomic.Int64

	// asyncOnce guards lazy construction of asyncCorrelator: most runs
	// never use async tools and pay no cost. Held in an atomic so
	// non-dispatcher goroutines can read it without going through Once.Do.
	asyncOnce       sync.Once
	asyncCorrelator atomic.Pointer[transport.Correlator]

	// asyncExtractorOverride, when non-nil, replaces extractAsyncToolResult
	// on the async correlator. Test-only seam: the production extractor
	// never delivers a non-asyncToolResult payload, so the defensive
	// async_internal_error branch is otherwise unreachable. Set via
	// withAsyncExtractor before the first dispatch; never set in
	// production wiring.
	asyncExtractorOverride transport.PayloadExtractor
}

// asyncToolResult carries the resolved payload of an async tool call from
// the transport correlator back to the dispatch path.
type asyncToolResult struct {
	content string
	isError bool
}

// extractAsyncToolResult is the PayloadExtractor for tool_result_response
// control events. Returns an empty id (and so is ignored) for any other
// event type. Content is truncated at maxAsyncToolResultBytes here, at the
// extraction boundary, so the cap is enforced regardless of how the payload
// is later consumed. Truncation is by bytes, not runes, since downstream
// consumers do not assume valid UTF-8.
func extractAsyncToolResult(event types.ControlEvent) (string, any) {
	if event.Type != "tool_result_response" {
		return "", nil
	}
	content := event.Content
	if len(content) > maxAsyncToolResultBytes {
		content = content[:maxAsyncToolResultBytes] + asyncResultTruncationSuffix
	}
	isErr := event.IsError != nil && *event.IsError
	return event.RequestID, asyncToolResult{
		content: content,
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
		extract := extractAsyncToolResult
		if l.asyncExtractorOverride != nil {
			extract = l.asyncExtractorOverride
		}
		c.AttachTo(l.Transport, extract)
		l.asyncCorrelator.Store(c)
	})
	return l.asyncCorrelator.Load()
}

// withAsyncExtractor installs a test-only PayloadExtractor override on the
// async correlator-attach path, used to deliver a payload other than
// asyncToolResult and exercise dispatchAsyncToolCall's defensive
// async_internal_error branch. Must be called before the first async
// dispatch (the correlator is constructed once, lazily). Returns the loop
// for call-chaining in test setup.
func (l *AgenticLoop) withAsyncExtractor(extract transport.PayloadExtractor) *AgenticLoop {
	l.asyncExtractorOverride = extract
	return l
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

// metricAttrs returns a metric.MeasurementOption that combines the loop's
// MetricAttrs (set per-run, e.g. subagent=true on child loops) with the
// supplied per-call extras. Used at every metric instrument call site so
// sub-agent observations are attributable on dashboards without touching
// every call individually.
func (l *AgenticLoop) metricAttrs(extra ...attribute.KeyValue) metric.MeasurementOption {
	if len(l.MetricAttrs) == 0 {
		return metric.WithAttributes(extra...)
	}
	if len(extra) == 0 {
		return metric.WithAttributes(l.MetricAttrs...)
	}
	combined := make([]attribute.KeyValue, 0, len(l.MetricAttrs)+len(extra))
	combined = append(combined, l.MetricAttrs...)
	combined = append(combined, extra...)
	return metric.WithAttributes(combined...)
}

// dispatchToolCall executes a single tool call, checking permissions and
// validating input against the tool's JSON Schema. Returns the tool result
// string and whether it succeeded.
//
// A (string, bool) wrapper around dispatchToolCallCategorized kept so
// existing two-value test call sites still compile; it drops the structured
// payload and failure category. Production call sites use the categorised
// variant directly.
func (l *AgenticLoop) dispatchToolCall(ctx context.Context, call types.ToolCall) (string, bool) {
	out, ok, _, _ := l.dispatchToolCallCategorized(ctx, call)
	return out, ok
}

// structuredOutput carries the optional typed result payload from a
// StructuredHandler back through dispatch. The zero value means "no
// structured data": every failure path and every plain-Handler tool
// returns it. Kind names the payload's shape (see types.ToolResult.Kind).
type structuredOutput struct {
	payload json.RawMessage
	kind    string
}

// dispatchToolCallCategorized is the production entry point. The third
// return value is the bounded failure category for failed calls (empty on
// success); every failure path here and in the async helper must assign one
// from the enum in harness/internal/observability/toolfailure.go. The fourth
// return value is the optional structured result payload, populated only on
// a StructuredHandler's success path.
func (l *AgenticLoop) dispatchToolCallCategorized(ctx context.Context, call types.ToolCall) (string, bool, observability.ToolFailureCategory, structuredOutput) {
	t := l.Tools.Resolve(call.Name)
	if t == nil {
		// A directional rename hint turns an opaque "Unknown tool" miss
		// into a migration the model can act on in-loop.
		if msg, ok := renamedToolHint(call.Name); ok {
			return msg, false, observability.ToolFailureUnknownTool, structuredOutput{}
		}
		return "Unknown tool: " + call.Name, false, observability.ToolFailureUnknownTool, structuredOutput{}
	}

	// Strip prototype-pollution keys before validation so we can both notify
	// the SecurityLogger AND continue validating the cleaned form.
	// ValidateJSONSchema also strips internally; calling here in addition
	// gives us a chance to surface the security event.
	cleaned, droppedKeys, stripErr := security.StripDangerousKeysFromInput(call.Input)
	if stripErr == nil && len(droppedKeys) > 0 && l.Security != nil {
		l.Security.PrototypePollutionBlocked(call.Name, droppedKeys)
	}
	inputForCall := call.Input
	if stripErr == nil && len(droppedKeys) > 0 {
		inputForCall = cleaned
	}

	// Validate input against the tool's JSON Schema. Mandatory; cannot be disabled.
	if err := security.ValidateJSONSchema(inputForCall, t.InputSchema); err != nil {
		if l.Security != nil {
			l.Security.ToolInputRejected(call.Name, []string{err.Error()})
		}
		return fmt.Sprintf("Invalid input for %s: %v", call.Name, err), false, observability.ToolFailureSchemaValidation, structuredOutput{}
	}

	// Key the write-target guard on the internal tool ID (t.Name), not the
	// model-facing alias (call.Name): a guard rule written against the
	// internal name must fire under any toolset profile.
	if findings := security.GuardToolCall(t.Name, t.WorkspaceMutating, call.Input); len(findings) > 0 {
		if l.Security != nil {
			l.Security.ToolCallGuardTriggered(call.Name, findings)
		}
		return fmt.Sprintf("Tool call rejected by security guard for %s", call.Name), false, observability.ToolFailureSecurityGuard, structuredOutput{}
	}

	// Check permissions for tools that mutate the workspace or otherwise
	// require upstream approval (e.g. web_fetch, spawn_agent).
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
			return "Permission check error: " + err.Error(), false, observability.ToolFailurePermissionError, structuredOutput{}
		}
		permSpan.SetAttributes(attribute.Bool("permission.allowed", result.Allowed))
		permSpan.End()
		if !result.Allowed {
			if l.Security != nil {
				l.Security.PermissionDenied(call.Name, result.Reason)
			}
			if l.Trace != nil {
				l.Trace.RecordPermissionDenial()
			}
			return "Permission denied: " + result.Reason, false, observability.ToolFailurePermissionDenied, structuredOutput{}
		}
	}

	// Async tools resolve their result via the transport correlator: the
	// preflight returns an AsyncDispatch, the loop emits a
	// tool_result_request event, and blocks until a tool_result_response
	// arrives (or ctx is cancelled / the timeout fires).
	if t.AsyncHandler != nil {
		output, success, category := l.dispatchAsyncToolCall(ctx, t, call, inputForCall)
		return output, success, category, structuredOutput{}
	}

	// Use the cleaned input so the handler never sees prototype-pollution
	// keys either. A StructuredHandler yields text plus an optional typed
	// payload; a tool with only Handler returns no structured payload.
	switch {
	case t.StructuredHandler != nil:
		res, err := t.StructuredHandler(ctx, inputForCall)
		if err != nil {
			return "Tool error: " + err.Error(), false, observability.ToolFailureHandlerError, structuredOutput{}
		}
		return res.Text, true, "", structuredOutput{payload: res.Structured, kind: res.Kind}
	case t.Handler != nil:
		output, err := t.Handler(ctx, inputForCall)
		if err != nil {
			return "Tool error: " + err.Error(), false, observability.ToolFailureHandlerError, structuredOutput{}
		}
		return output, true, "", structuredOutput{}
	default:
		return fmt.Sprintf("Tool %s has no handler registered", call.Name), false, observability.ToolFailureHandlerMissing, structuredOutput{}
	}
}

// dispatchAsyncToolCall runs the async tool path: it refuses the call up
// front when the transport cannot deliver responses, invokes the tool's
// AsyncHandler as a preflight, emits a "tool_result_request" HarnessEvent,
// and blocks on the matching "tool_result_response" via the loop's async
// correlator under run-context cancellation and the per-call timeout. The
// model sees IsError=true on every failure path.
func (l *AgenticLoop) dispatchAsyncToolCall(
	ctx context.Context,
	t *tool.Tool,
	call types.ToolCall,
	inputForCall json.RawMessage,
) (string, bool, observability.ToolFailureCategory) {
	correlator := l.ensureAsyncCorrelator()
	if correlator == nil {
		// NullTransport (sub-agent): no control plane to round-trip
		// through, so fail fast rather than burning the per-call timeout.
		return fmt.Sprintf(
			"async tool %s unavailable: this loop has no live control-plane transport",
			call.Name,
		), false, observability.ToolFailureAsyncTransport
	}

	dispatch, err := t.AsyncHandler(ctx, inputForCall)
	if err != nil {
		return fmt.Sprintf("async tool %s internal error: %s", call.Name, err.Error()), false, observability.ToolFailureAsyncPreflight
	}

	timeout := dispatch.Timeout
	if timeout <= 0 {
		timeout = DefaultAsyncToolTimeout
	}

	// The correlator allocates the wire request ID; AsyncDispatch does not
	// expose it to tool authors, since correlation is a loop concern.

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
		// The correlator wraps emit errors with "emit failed", timeouts
		// with "timed out", and ctx errors carry context.Canceled /
		// context.DeadlineExceeded in the chain.
		if strings.Contains(err.Error(), "emit failed") {
			return fmt.Sprintf("async tool %s transport_disconnect: %s", call.Name, err.Error()), false, observability.ToolFailureAsyncTransport
		}
		// Deadline expiry MUST be checked before cancellation: both the
		// correlator's per-call timeout and a run-level deadline surface as
		// context.DeadlineExceeded, and errors.Is can match both sentinels
		// on some wrapper chains, so timeout-first preserves the
		// operator-meaningful distinction (vs. polluting async_cancelled,
		// which should mean user cancellation only).
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Sprintf("async tool %s timeout: %s", call.Name, err.Error()), false, observability.ToolFailureAsyncTimeout
		}
		if errors.Is(err, context.Canceled) {
			return fmt.Sprintf("async tool %s cancelled: %s", call.Name, err.Error()), false, observability.ToolFailureAsyncCancelled
		}

		return fmt.Sprintf("async tool %s timeout: %s", call.Name, err.Error()), false, observability.ToolFailureAsyncTimeout
	}

	resp, ok := payload.(asyncToolResult)
	if !ok {
		// Defensive: extractAsyncToolResult only ever delivers
		// asyncToolResult, so reaching this branch means the correlator
		// was wired with a different extractor. Treat as a hard error.
		return fmt.Sprintf("async tool %s internal error: unexpected payload type %T", call.Name, payload), false, observability.ToolFailureAsyncInternal
	}
	if resp.isError {
		// The control plane is partially trusted and could embed
		// secret-shaped strings in the error payload; scrub at the point of
		// entry so both the model context and the trace see the redacted
		// form (this path bypasses the transport's outbound scrub).
		scrubbed := security.Scrub(resp.content)
		return fmt.Sprintf("async tool %s upstream_error: %s", call.Name, scrubbed), false, observability.ToolFailureAsyncUpstreamError
	}
	return resp.content, true, ""
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
// message history as an assistant message. replayFields is provider-opaque
// round-trip state from the stream's message_complete event; attaching it
// lets the originating provider's adapter echo it back on subsequent turns.
// nil for providers that emit none, keeping the serialised message
// byte-identical in that case.
func appendAssistantContent(messages []types.Message, blocks []types.ContentBlock, replayFields map[string]json.RawMessage) []types.Message {
	return append(messages, types.Message{
		Role:         "assistant",
		Content:      blocks,
		ReplayFields: replayFields,
	})
}

// appendToolResults adds tool results as a user message (per Anthropic API format).
func appendToolResults(messages []types.Message, results []types.ToolResult) []types.Message {
	blocks := make([]types.ContentBlock, len(results))
	for i, r := range results {
		blocks[i] = types.ContentBlock{
			Type:       "tool_result",
			ToolUseID:  r.ToolUseID,
			Content:    r.Content,
			IsError:    r.IsError,
			Structured: r.Structured,
			Kind:       r.Kind,
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
// ReplayFields is provider-opaque round-trip state from the message_complete
// event, plumbed through to the persisted assistant Message without being
// inspected or logged.
type streamResult struct {
	Blocks       []types.ContentBlock
	StopReason   string
	OutputTokens int
	ReplayFields map[string]json.RawMessage
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
			// ThoughtSignature is provider-opaque state the harness must
			// echo back unchanged on the next request so the model can
			// resume its prior reasoning. Adapters that do not emit it
			// leave it empty; omitempty keeps it off the wire.
			result.Blocks = append(result.Blocks, types.ContentBlock{
				Type:             "tool_use",
				ID:               event.ID,
				Name:             event.Name,
				Input:            json.RawMessage(inputBytes),
				ThoughtSignature: event.ThoughtSignature,
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
			if len(event.ReplayFields) > 0 {
				result.ReplayFields = event.ReplayFields
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
			// The structured envelope rides on tool_result blocks and can be
			// large (e.g. a Gemini object-response result); counting it
			// avoids under-shooting the budget and overflowing mid-run.
			total += len(block.Structured) / tokenEstimationDivisor
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

// renamedToolHint maps legacy tool names to a directional error message
// naming the replacement(s). Returns ("", false) for any unrecognised name;
// the caller falls through to the generic unknown-tool path in that case.
func renamedToolHint(name string) (string, bool) {
	switch name {
	case "search_files":
		return "tool not found: search_files; use grep_files (regex content search) or find_files (glob filename search)", true
	}
	return "", false
}
