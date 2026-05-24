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

// maxAsyncToolResultBytes caps the length of control-plane-supplied tool
// result content. The control plane is partially trusted; an unbounded
// string here is a DoS vector (CWE-400). 1MB matches the existing command
// output cap used by the run_command sync tool.
const maxAsyncToolResultBytes = 1 << 20 // 1MB

// asyncResultTruncationSuffix is appended when content exceeds
// maxAsyncToolResultBytes. The model and the trace see this marker.
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
	// Tools (issue #234). Nil means the default (identity) profile. Held on
	// the loop so SpawnSubAgent can re-present the filtered child registry
	// under the same profile, keeping parent and child tool names
	// consistent. The factory always sets Tools to a *tool.Presenter built
	// with this profile.
	ToolProfile  *tool.Profile
	Executor     executor.Executor
	Edit         edit.EditStrategy
	Verifier     verifier.Verifier
	Permissions  permission.PermissionPolicy
	Git          git.GitStrategy
	GuardRail    guard.GuardRail
	Escalation   EscalationPolicy // tool-choice missed-tool recovery (#230); nil = disabled
	Transport    transport.Transport
	Trace        trace.TraceEmitter
	Tracer       oteltrace.Tracer         // OTel tracer for loop-level spans (noop when not using OTel)
	TraceContext context.Context          // context carrying the root span for child span parenting
	Metrics      *observability.Metrics   // OTel metric instruments (noop when disabled)
	Security     *security.SecurityLogger // optional, for structured security event logging
	Logger       *slog.Logger             // structured logger with secret scrubbing
	// MetricAttrs is a set of attributes prepended to every metric
	// observation emitted from this loop. Empty for top-level runs;
	// SpawnSubAgent populates it on child loops with subagent=true and
	// parent.run_id=<parent run id> so dashboards can decompose a run
	// into parent vs child observations.
	MetricAttrs  []attribute.KeyValue
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
// event type. Content is truncated at maxAsyncToolResultBytes before
// flowing into tool output, message history, or the wire — the cap is
// applied here, at the extraction boundary, so it is enforced regardless
// of how the payload is later consumed (success path, error path, trace).
// Truncation happens in bytes (not runes) to keep the bound predictable;
// downstream consumers do not assume valid UTF-8.
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
// Preserved as a (string, bool) wrapper around dispatchToolCallCategorized
// so existing test sites that take a two-value return continue to compile.
// Production call sites in planAndDispatch use the categorised variant
// directly so the per-failure category flows into the
// stirrup.harness.tool_failures metric and the ToolCallTrace ErrorCategory
// field.
//
// NOTE: this shim drops the structured payload. New tests that need to assert
// structured output must call dispatchToolCallCategorized directly.
func (l *AgenticLoop) dispatchToolCall(ctx context.Context, call types.ToolCall) (string, bool) {
	out, ok, _, _ := l.dispatchToolCallCategorized(ctx, call)
	return out, ok
}

// structuredOutput carries the optional typed result payload (issue #231)
// from a StructuredHandler back through dispatch. The zero value (nil payload,
// empty kind) means "no structured data": every failure path and every plain-
// Handler tool returns it, so a text-only result stays text-only. Kind names
// the payload's shape so it can be routed without unmarshalling (see
// types.ToolResult.Kind).
type structuredOutput struct {
	payload json.RawMessage
	kind    string
}

// dispatchToolCallCategorized is the production entry point. The third
// return value is the bounded failure category for failed calls; on
// success the category is empty. Every failure path in this function and
// its async helper must assign a ToolFailureCategory drawn from the enum
// in harness/internal/observability/toolfailure.go.
//
// The fourth return value is the optional structured result payload (issue
// #231). It is the zero structuredOutput on every failure path and for any
// tool that exposes only a plain Handler, so a text-only result stays
// text-only; it is populated only on the success path of a StructuredHandler.
func (l *AgenticLoop) dispatchToolCallCategorized(ctx context.Context, call types.ToolCall) (string, bool, observability.ToolFailureCategory, structuredOutput) {
	t := l.Tools.Resolve(call.Name)
	if t == nil {
		// Issue #225 split the legacy search_files tool into two strictly-
		// typed tools (grep_files for regex content search, find_files for
		// glob filename search). Emitting a directional error here turns
		// what would otherwise be an opaque "Unknown tool" miss into a
		// migration hint the model can act on in-loop, while still
		// preserving the unknown-tool failure category for telemetry.
		if msg, ok := renamedToolHint(call.Name); ok {
			return msg, false, observability.ToolFailureUnknownTool, structuredOutput{}
		}
		return "Unknown tool: " + call.Name, false, observability.ToolFailureUnknownTool, structuredOutput{}
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

	// Validate input against the tool's JSON Schema. Mandatory; cannot be disabled.
	if err := security.ValidateJSONSchema(inputForCall, t.InputSchema); err != nil {
		if l.Security != nil {
			l.Security.ToolInputRejected(call.Name, []string{err.Error()})
		}
		return fmt.Sprintf("Invalid input for %s: %v", call.Name, err), false, observability.ToolFailureSchemaValidation, structuredOutput{}
	}

	// Key the write-target guard on the internal tool ID (t.Name), not the
	// model-facing alias (call.Name): a guard rule written against the
	// internal name must fire under any toolset profile (issue #234). t is
	// resolved above; the gating layers (permission policy, mutating-tool
	// set, this guard) all uniformly key on internal identity.
	if findings := security.GuardToolCall(t.Name, t.WorkspaceMutating, call.Input); len(findings) > 0 {
		if l.Security != nil {
			l.Security.ToolCallGuardTriggered(call.Name, findings)
		}
		return fmt.Sprintf("Tool call rejected by security guard for %s", call.Name), false, observability.ToolFailureSecurityGuard, structuredOutput{}
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
	// preflight returns an AsyncDispatch describing the request_id and
	// per-call timeout to use, the loop emits a tool_result_request event,
	// and blocks until a tool_result_response arrives (or ctx is cancelled
	// / the timeout fires). Permission and security checks above already
	// ran and gated dispatch identically to the sync path.
	if t.AsyncHandler != nil {
		output, success, category := l.dispatchAsyncToolCall(ctx, t, call, inputForCall)
		return output, success, category, structuredOutput{}
	}

	// Execute the tool handler with the cleaned input so the handler does not
	// see prototype-pollution keys either. A StructuredHandler is preferred
	// over the plain Handler: it yields the identical text plus an optional
	// typed structured payload (issue #231). Either form is acceptable; a
	// tool that sets only Handler simply returns no structured payload.
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
) (string, bool, observability.ToolFailureCategory) {
	correlator := l.ensureAsyncCorrelator()
	if correlator == nil {
		// NullTransport (sub-agent) — no control plane to round-trip
		// through. Fail fast rather than burning the per-call timeout.
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
			return fmt.Sprintf("async tool %s transport_disconnect: %s", call.Name, err.Error()), false, observability.ToolFailureAsyncTransport
		}
		// Deadline expiry MUST be checked before cancellation. Both the
		// correlator's per-call timeout and a run-level deadline on ctx
		// surface as context.DeadlineExceeded wrapped by %w; the
		// previous combined branch mis-routed every deadline-expired
		// run to async_cancelled, polluting any alert keyed on
		// async_cancelled spikes (which signal user cancellation, not
		// timeout). errors.Is(DeadlineExceeded) and errors.Is(Canceled)
		// can both return true for some wrapper chains, so order
		// matters: timeout-first preserves the operator-meaningful
		// distinction.
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Sprintf("async tool %s timeout: %s", call.Name, err.Error()), false, observability.ToolFailureAsyncTimeout
		}
		if errors.Is(err, context.Canceled) {
			return fmt.Sprintf("async tool %s cancelled: %s", call.Name, err.Error()), false, observability.ToolFailureAsyncCancelled
		}
		// Default: timeout (correlator: "timed out after ...") and any
		// other unexpected wrapping go here.
		return fmt.Sprintf("async tool %s timeout: %s", call.Name, err.Error()), false, observability.ToolFailureAsyncTimeout
	}

	resp, ok := payload.(asyncToolResult)
	if !ok {
		// Defensive: extractAsyncToolResult only ever delivers
		// asyncToolResult, so reaching this branch means the correlator
		// was wired with a different extractor. Treat as a hard error.
		//
		// TODO(#229-followup): no dispatch-site test covers this
		// emission because the production wiring exposes no seam to
		// inject a non-asyncToolResult payload — ensureAsyncCorrelator
		// always attaches extractAsyncToolResult. Gap deferred per
		// the wave-1-issue-229 synthesis brief; revisit if a future
		// refactor exposes a way to swap the extractor for tests.
		return fmt.Sprintf("async tool %s internal error: unexpected payload type %T", call.Name, payload), false, observability.ToolFailureAsyncInternal
	}
	if resp.isError {
		// The control plane is partially trusted: a compromised or
		// misbehaving control plane could embed secret-shaped strings
		// in the error payload, and the failure path forwards this
		// content into the JSONL trace via RecordToolCall.ErrorReason
		// without going through the transport's outbound scrub. Scrub
		// at the point of entry so both the model context and the
		// trace see the redacted form. The structured prefix matches
		// the other three error taxonomy paths (transport_disconnect,
		// timeout, internal error) so the model can disambiguate
		// upstream failures from harness-side failures.
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
			// ThoughtSignature is provider-opaque state that the harness
			// must echo back unchanged on the next request so the model
			// can resume its prior reasoning. Currently populated only by
			// the Gemini 3.x adapter (#194). Adapters that do not emit
			// the field leave it empty, and `omitempty` keeps it off the
			// wire for downstream consumers (request marshallers, traces).
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
			// The structured envelope (issue #231) rides on tool_result
			// blocks and, for a Gemini object-response result, can be up to
			// maxMCPStructuredSize; counting it keeps the budget estimate from
			// under-shooting and triggering a mid-run context overflow. Kind
			// is a short discriminator and not worth counting.
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

// renamedToolHint maps legacy tool names removed in the issue #225 schema
// redesign to a directional error message naming the replacement(s). It
// returns ("", false) for any name that was never registered under a
// previous taxonomy; the caller falls through to the generic unknown-tool
// path in that case.
//
// Kept as a small table rather than a sentinel so future renames can land
// here without touching dispatch logic. The strings are stable: a future
// reviewer searching for the migration message can grep this file.
func renamedToolHint(name string) (string, bool) {
	switch name {
	case "search_files":
		return "tool not found: search_files; use grep_files (regex content search) or find_files (glob filename search)", true
	}
	return "", false
}
