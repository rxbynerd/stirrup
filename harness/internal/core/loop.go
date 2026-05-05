package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// outcomeCtxDone is a sentinel outcome returned by runInnerLoop when the
// loop observes ctx.Done(). The outer Run loop inspects context.Cause to
// translate this into a user-visible outcome: "cancelled" (control plane
// or plain/signal cancel), "timeout" (deadline), or "error" (non-nil but
// unrecognised cause).
const outcomeCtxDone = "_ctx_done"

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
	// Derive a cancellable context so a "cancel" ControlEvent can abort the
	// run within one turn boundary. WithCancelCause lets us disambiguate
	// control-plane cancellation from deadline expiry and caller cancellation
	// later via context.Cause().
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)

	// Register a cancel handler on the transport. Fan-out OnControl is
	// supported by all production transports (stdio, gRPC); sub-agents use
	// NullTransport whose OnControl is a no-op, so this is a harmless no-op
	// in the sub-agent case.
	l.Transport.OnControl(func(event types.ControlEvent) {
		if event.Type == "cancel" {
			cancelRun(ErrCancelledByControlPlane)
		}
	})

	// Start tracing.
	l.Trace.Start(config.RunID, config)

	// Extract the root trace context for child span parenting.
	if otelEmitter, ok := l.Trace.(*trace.OTelTraceEmitter); ok {
		l.TraceContext = otelEmitter.RootContext()
	} else {
		l.TraceContext = runCtx
	}

	// Start heartbeat emission so the control plane knows we are alive.
	stopHeartbeat := l.startHeartbeat(runCtx, 30*time.Second)

	// Build the system prompt.
	dynamicContext := config.DynamicContext
	if len(dynamicContext) > 0 {
		var events []security.DynamicContextSanitizationEvent
		dynamicContext, events = security.SanitizeDynamicContext(dynamicContext)
		if l.Security != nil {
			for _, event := range events {
				l.Security.DynamicContextSanitized(event)
			}
		}
	}
	systemPrompt, err := l.Prompt.Build(runCtx, prompt.PromptContext{
		Mode:           config.Mode,
		Workspace:      config.Executor.Workspace,
		MaxTurns:       config.MaxTurns,
		DynamicContext: dynamicContext,
	})
	if err != nil {
		return l.finishWithError(runCtx, fmt.Errorf("build system prompt: %w", err))
	}

	// Set up git workspace.
	_, gitSetupSpan := l.Tracer.Start(l.traceCtx(runCtx), "git.setup")
	if err := l.Git.Setup(runCtx, config.Executor.Workspace, config.RunID); err != nil {
		gitSetupSpan.RecordError(err)
		gitSetupSpan.SetStatus(codes.Error, err.Error())
		gitSetupSpan.End()
		return l.finishWithError(runCtx, fmt.Errorf("git setup: %w", err))
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
	l.Metrics.Runs.Add(runCtx, 1,
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
		messages, innerOutcome = l.runInnerLoop(runCtx, config, systemPrompt, messages, tokenTracker)

		if innerOutcome != "success" {
			outcome = innerOutcome
			break
		}

		// Run verifier.
		l.Metrics.VerificationAttempts.Add(runCtx, 1)
		_, verifySpan := l.Tracer.Start(l.traceCtx(runCtx), "verifier.verify",
			oteltrace.WithAttributes(
				attribute.Int("verifier.attempt", verificationAttempts),
			),
		)
		vResult, verifyErr := l.Verifier.Verify(runCtx, verifier.VerifyContext{
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

	// Cancellation wins over verification-path outcomes. A cancel arriving
	// between the inner loop returning and Verify completing can otherwise
	// cause Verify to return a ctx-cancelled error and set
	// outcome="verification_error", masking the true termination reason on
	// the wire. If the run context is done, reclassify so the cancel/timeout
	// path below runs.
	if runCtx.Err() != nil && outcome != outcomeCtxDone {
		outcome = outcomeCtxDone
	}

	// If the inner loop exited because the context was cancelled, inspect
	// the cause to distinguish control-plane cancellation ("cancelled"),
	// deadline expiry ("timeout"), plain/signal cancel ("cancelled"), and
	// anything else ("error").
	if outcome == outcomeCtxDone {
		cause := context.Cause(runCtx)
		outcome = classifyCtxOutcome(cause)
		l.setRootCancelAttribute(cause)
	}

	// Finalise git. Use the parent ctx here: if the run was cancelled, we
	// still want git.Finalise to be able to persist whatever state exists.
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

	// Finish trace using the parent ctx — the trace exporter's ForceFlush
	// should still have a usable deadline even if the run-scoped ctx is
	// already cancelled.
	runTrace, traceErr := l.Trace.Finish(ctx, outcome)
	if traceErr != nil {
		return nil, fmt.Errorf("finish trace: %w", traceErr)
	}

	return runTrace, nil
}

// classifyCtxOutcome maps a context cancellation cause onto the outcome
// string reported on the "done" event and recorded in RunTrace.Outcome.
//
// A nil cause or a bare context.Canceled indicates the run was cancelled
// via a plain cancel() without a cause attached — e.g. SIGINT/SIGTERM via
// the root cobra signal handler, or a caller invoking context.WithCancel
// on a parent and then cancel() (which propagates context.Canceled as the
// cause of our WithCancelCause child). The spec treats this as a
// user-initiated cancellation, distinct from a deadline-driven timeout or
// an internal error. A non-nil cause that is neither a recognised cancel
// sentinel nor a deadline is surfaced as "error" since we cannot attribute
// it to a known cancel or timeout path.
func classifyCtxOutcome(cause error) string {
	switch {
	case errors.Is(cause, ErrCancelledByControlPlane):
		return "cancelled"
	case errors.Is(cause, context.DeadlineExceeded):
		return "timeout"
	case cause == nil, errors.Is(cause, context.Canceled):
		return "cancelled"
	default:
		return "error"
	}
}

// setRootCancelAttribute tags the root "run" OTel span with the reason for
// context cancellation so operators can filter cancelled runs from timed-out
// or errored runs in tracing backends. Only applied when the run actually
// ended via ctx cancellation.
//
// The attribute is derived from the context cause so that a plain/signal
// cancel and a control-plane cancel are distinguished on the span even
// though both map to outcome="cancelled".
//
//	run.cancelled_by="control_plane" — ErrCancelledByControlPlane cause
//	run.cancelled_by="deadline"      — context.DeadlineExceeded cause
//	run.cancelled_by="signal"        — nil cause or bare context.Canceled
//	                                   (plain cancel(), SIGINT, etc.)
//	(no attribute)                   — non-nil unrecognised cause ("error")
func (l *AgenticLoop) setRootCancelAttribute(cause error) {
	otelEmitter, ok := l.Trace.(*trace.OTelTraceEmitter)
	if !ok {
		return
	}
	span := oteltrace.SpanFromContext(otelEmitter.RootContext())
	if !span.SpanContext().IsValid() {
		return
	}
	var reason string
	switch {
	case errors.Is(cause, ErrCancelledByControlPlane):
		reason = "control_plane"
	case errors.Is(cause, context.DeadlineExceeded):
		reason = "deadline"
	case cause == nil, errors.Is(cause, context.Canceled):
		reason = "signal"
	default:
		// Non-nil unrecognised cause → outcome=="error"; no attribute.
		return
	}
	span.SetAttributes(attribute.String("run.cancelled_by", reason))
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

		// Check context cancellation. Return a sentinel outcome so the
		// outer Run loop can distinguish control-plane cancellation,
		// deadline expiry, and caller-initiated cancellation via
		// context.Cause().
		select {
		case <-ctx.Done():
			return messages, outcomeCtxDone
		default:
		}

		// PhasePreTurn guard. Classifies untrusted content that just
		// entered the message history. On turn 0 the chunks include the
		// initial user prompt and DynamicContext entries; on turn N>0
		// the chunks are the contents of every tool_result block in the
		// last user message. The chunks are concatenated under a "--- chunk i ---"
		// envelope so the adapter sees a single batched request.
		var preTurnDynamic map[string]string
		if turn == 0 {
			preTurnDynamic = config.DynamicContext
		}
		if chunks := collectUntrustedChunks(messages, turn, preTurnDynamic, config.Prompt); len(chunks) > 0 {
			batched := batchUntrustedChunks(chunks)
			in := guard.Input{
				Phase:   guard.PhasePreTurn,
				Content: batched,
				Source:  fmt.Sprintf("batched:n=%d", len(chunks)),
				Mode:    config.Mode,
				RunID:   config.RunID,
			}
			allow, _, spotlight := l.guardCheck(ctx, in, guardFailOpen(config))
			switch {
			case !allow:
				// PreTurn deny scrubs the untrusted content rather than
				// aborting the run: the issue treats PreTurn as a
				// content-level rather than call-level intervention. The
				// run continues so the model can still respond (typically
				// with a refusal) and operators see the deny event.
				replaceUntrustedChunks(messages, turn, "[content blocked by guardrail]")
			case spotlight:
				spotlightUntrustedChunks(messages, turn)
			}
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
			if ctx.Err() != nil {
				return messages, outcomeCtxDone
			}
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
			// Scrub the status string before it lands on the OTel span.
			// On HTTP transport failures Go wraps the underlying error in
			// *url.Error, which embeds the full request URL — including
			// any query parameters configured via Provider.QueryParams.
			// OTel spans bypass ScrubHandler (which only intercepts slog),
			// so without scrubbing here a future sensitive QueryParams
			// value would land in OTLP exports unredacted. RecordError
			// keeps the raw error so the span retains type information;
			// only the user-visible status message is scrubbed.
			scrubbedErr := security.Scrub(err.Error())
			providerSpan.RecordError(err)
			providerSpan.SetStatus(codes.Error, scrubbedErr)
			providerSpan.End()
			// Surface the failure outside of OTel: log it and emit a
			// transport warning. Without this, operators running without
			// an OTLP collector see only outcome=error with no detail.
			// ScrubHandler only intercepts string-kind slog attrs, so a
			// raw error value would slip through as KindAny — pass the
			// pre-scrubbed string explicitly. Skip when the context is
			// already cancelled: the cancel/timeout path below produces
			// the user-visible message.
			if ctx.Err() == nil {
				l.Logger.Error("provider stream failed",
					"provider", selection.Provider,
					"model", selection.Model,
					"error", scrubbedErr,
				)
				_ = l.Transport.Emit(types.HarnessEvent{
					Type:    "warning",
					Message: fmt.Sprintf("provider %s (%s): %s", selection.Provider, selection.Model, scrubbedErr),
				})
			}
			// Rollback: don't append anything on error.
			l.Metrics.ProviderErrors.Add(ctx, 1, providerAttrs)
			l.Trace.RecordTurn(types.TurnTrace{
				Turn:       turn,
				StopReason: "error",
				DurationMs: time.Since(turnStart).Milliseconds(),
			})
			// If the provider call failed because the run context was
			// cancelled, surface that so the outer loop can classify the
			// outcome as cancelled/timeout rather than a generic error.
			if ctx.Err() != nil {
				return messages, outcomeCtxDone
			}
			return messages, "error"
		}

		// Consume stream events.
		sr, streamErr := streamEventsToResult(ctx, ch, l.Transport, l.Logger)
		turnDuration := time.Since(turnStart)

		if streamErr != nil {
			// See the matching scrub above the Stream() call for rationale:
			// stream errors can wrap *url.Error or other strings derived
			// from HTTP transport state, and the OTel span status string
			// is not covered by ScrubHandler.
			scrubbedErr := security.Scrub(streamErr.Error())
			providerSpan.RecordError(streamErr)
			providerSpan.SetStatus(codes.Error, scrubbedErr)
			providerSpan.End()
			// Surface the failure outside of OTel — see the matching
			// log + emit at the Stream() call above for rationale.
			if ctx.Err() == nil {
				l.Logger.Error("provider stream failed",
					"provider", selection.Provider,
					"model", selection.Model,
					"error", scrubbedErr,
				)
				_ = l.Transport.Emit(types.HarnessEvent{
					Type:    "warning",
					Message: fmt.Sprintf("provider %s (%s): %s", selection.Provider, selection.Model, scrubbedErr),
				})
			}
			// Rollback on stream error — don't append partial content.
			l.Metrics.ProviderErrors.Add(ctx, 1, providerAttrs)
			l.Trace.RecordTurn(types.TurnTrace{
				Turn:       turn,
				StopReason: "error",
				DurationMs: turnDuration.Milliseconds(),
			})
			// Distinguish stream-abort-due-to-ctx from other stream errors
			// so the outer loop can classify the outcome correctly.
			if ctx.Err() != nil {
				return messages, outcomeCtxDone
			}
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
			// PhasePostTurn guard: classify the assistant's final text
			// before forwarding it. A deny terminates the run with the
			// "guardrail_blocked" outcome. Spotlight is opt-in for
			// future sub-agent contexts where the parent loop can safely
			// rewrap the child's output; for v1 we log the request and
			// forward the response unchanged because rewriting the
			// user-visible text would break tool-protocol expectations.
			finalText := lastAssistantText(sr.Blocks)
			if finalText != "" {
				in := guard.Input{
					Phase:   guard.PhasePostTurn,
					Content: finalText,
					Source:  "model_output",
					Mode:    config.Mode,
					RunID:   config.RunID,
				}
				allow, _, spotlight := l.guardCheck(ctx, in, guardFailOpen(config))
				if !allow {
					return messages, "guardrail_blocked"
				}
				if spotlight {
					l.Logger.Info("postTurn guard requested spotlight; not rewriting in v1")
				}
			}
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

			// PhasePreTool guard: classify the proposed tool call before
			// dispatch. A deny short-circuits dispatch as a tool failure
			// — same metrics, trace, and transport surface as a regular
			// tool error — so the model gets a structured error to
			// recover from and the existing stall detector accumulates
			// the failure against its consecutive-failures threshold.
			preToolIn := guard.Input{
				Phase:     guard.PhasePreTool,
				Content:   string(call.Input),
				Source:    "tool_call:" + call.Name,
				ToolName:  call.Name,
				ToolInput: call.Input,
				Mode:      config.Mode,
				RunID:     config.RunID,
			}
			preToolAllow, preToolDecision, _ := l.guardCheck(ctx, preToolIn, guardFailOpen(config))
			if !preToolAllow {
				reason := "guardrail blocked tool call"
				if preToolDecision != nil && preToolDecision.Reason != "" {
					reason = "guardrail blocked tool call: " + preToolDecision.Reason
				}
				output := reason
				callDuration := time.Since(callStart)
				toolSpan.SetAttributes(
					attribute.Bool("tool.success", false),
					attribute.Int("tool.output_size", len(output)),
					attribute.Int64("tool.duration_ms", callDuration.Milliseconds()),
				)
				toolSpan.SetStatus(codes.Error, "guardrail_denied")
				toolSpan.End()
				l.Trace.RecordToolCall(types.ToolCallTrace{
					Name:        call.Name,
					DurationMs:  callDuration.Milliseconds(),
					Success:     false,
					ErrorReason: reason,
					InputSize:   len(call.Input),
					OutputSize:  len(output),
				})
				toolNameAttr := metric.WithAttributes(attribute.String("tool.name", call.Name))
				l.Metrics.ToolCalls.Add(ctx, 1, toolNameAttr)
				l.Metrics.ToolCallDuration.Record(ctx, float64(callDuration.Milliseconds()), toolNameAttr)
				l.Metrics.ToolErrors.Add(ctx, 1, toolNameAttr)
				toolResults = append(toolResults, types.ToolResult{
					ToolUseID: call.ID,
					Content:   output,
					IsError:   true,
				})
				if err := l.Transport.Emit(types.HarnessEvent{
					Type:      "tool_result",
					ToolUseID: call.ID,
					Content:   output,
				}); err != nil {
					l.Logger.Warn("transport emit failed", "event", "tool_result", "error", err)
				}
				if outcome := stall.recordToolCall(call.Name, call.Input, false); outcome != "" {
					l.Metrics.Stalls.Add(ctx, 1,
						metric.WithAttributes(attribute.String("run.mode", config.Mode)),
					)
					messages = appendToolResults(messages, toolResults)
					return messages, outcome
				}
				continue
			}

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
	cancelCh := make(chan struct{}, 1)

	loop.Transport.OnControl(func(event types.ControlEvent) {
		switch event.Type {
		case "user_response":
			select {
			case followUpCh <- event.UserResponse:
			default:
				// A follow-up is already queued. Drop this one; the control
				// plane should wait for "done" before sending another request.
			}
		case "cancel":
			// Exit the grace window immediately on cancel. Any in-flight
			// Run invocation has its own cancel handler and will terminate
			// on the next turn boundary.
			select {
			case cancelCh <- struct{}{}:
			default:
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

		case <-cancelCh:
			return

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

// guardCheck wraps a guard.Check call with: trace span, metrics, security
// events, fail-open decoding, and skip detection. Returns:
//
//   - allow=true  → caller continues (decision is non-nil)
//   - allow=false → caller treats as a deny. The caller decides how to
//     surface the deny per phase: tool failure for PhasePreTool,
//     "guardrail_blocked" outcome for PhasePostTurn, content-scrub for
//     PhasePreTurn.
//   - decision is the underlying decision (always non-nil on allow=true,
//     non-nil with VerdictDeny on allow=false from a deny verdict, nil
//     when allow=false because of a hard error and FailOpen is false)
//   - spotlight=true means the caller should rewrap content with
//     ApplySpotlight before forwarding (PhasePreTurn / PhasePostTurn)
//
// failOpen tells the helper how to interpret an error: when true, errors
// produce allow=true with a guard_error security event; when false,
// errors produce allow=false with a guard_error event AND the loop
// should treat as deny.
func (l *AgenticLoop) guardCheck(ctx context.Context, in guard.Input, failOpen bool) (bool, *guard.Decision, bool) {
	if l.GuardRail == nil {
		return true, &guard.Decision{Verdict: guard.VerdictAllow, GuardID: "none"}, false
	}
	start := time.Now()
	_, span := l.Tracer.Start(l.traceCtx(ctx), "guard."+string(in.Phase),
		oteltrace.WithAttributes(
			attribute.String("guard.phase", string(in.Phase)),
			attribute.String("guard.source", in.Source),
		),
	)
	decision, err := l.GuardRail.Check(ctx, in)
	elapsed := time.Since(start)
	if err != nil {
		// Scrub before surfacing: error strings can wrap *url.Error or
		// classifier-side payloads that legitimately contain operator
		// hostnames or query parameters. ScrubHandler covers slog but
		// not OTel span statuses or security event data, so scrub here
		// once and reuse the redacted string everywhere.
		scrubbed := security.Scrub(err.Error())
		span.RecordError(err)
		span.SetStatus(codes.Error, scrubbed)
		span.End()
		guardID := guardIDFromDecision(decision)
		if l.Metrics != nil {
			l.Metrics.GuardErrors.Add(ctx, 1, metric.WithAttributes(
				attribute.String("guard.phase", string(in.Phase)),
				attribute.String("guard.id", guardID),
			))
			l.Metrics.GuardDuration.Record(ctx, float64(elapsed.Milliseconds()), metric.WithAttributes(
				attribute.String("guard.phase", string(in.Phase)),
				attribute.String("guard.id", guardID),
			))
		}
		if l.Security != nil {
			l.Security.GuardError(string(in.Phase), guardID, scrubbed)
		}
		if failOpen {
			return true, &guard.Decision{
				Verdict: guard.VerdictAllow,
				Reason:  "fail_open: " + scrubbed,
				GuardID: guardID,
			}, false
		}
		return false, nil, false
	}
	if decision == nil {
		// Defensive: a guard returning (nil, nil) is a contract
		// violation. Record a synthetic allow rather than panicking
		// downstream.
		decision = &guard.Decision{Verdict: guard.VerdictAllow, GuardID: "unknown"}
	}
	span.SetAttributes(
		attribute.String("guard.id", decision.GuardID),
		attribute.String("guard.verdict", string(decision.Verdict)),
		attribute.Float64("guard.score", decision.Score),
		attribute.Int64("guard.latency_ms", elapsed.Milliseconds()),
	)
	span.End()

	// Skip detection — distinct from a regular allow. The granite-
	// guardian adapter sets Reason==ReasonSkippedMinChunk when content
	// is below the configured MinChunkChars threshold. We surface this
	// as a separate metric and security event so dashboards do not
	// confuse cost-saving skips with classifier-validated allows.
	isSkip := decision.Reason == guard.ReasonSkippedMinChunk
	if l.Metrics != nil {
		if isSkip {
			l.Metrics.GuardSkips.Add(ctx, 1, metric.WithAttributes(
				attribute.String("guard.phase", string(in.Phase)),
				attribute.String("guard.id", decision.GuardID),
				attribute.String("reason", "min_chunk_chars"),
			))
		} else {
			l.Metrics.GuardChecks.Add(ctx, 1, metric.WithAttributes(
				attribute.String("guard.phase", string(in.Phase)),
				attribute.String("guard.id", decision.GuardID),
				attribute.String("guard.verdict", string(decision.Verdict)),
			))
		}
		l.Metrics.GuardDuration.Record(ctx, float64(elapsed.Milliseconds()), metric.WithAttributes(
			attribute.String("guard.phase", string(in.Phase)),
			attribute.String("guard.id", decision.GuardID),
		))
	}
	if l.Security != nil {
		switch {
		case isSkip:
			l.Security.GuardSkipped(string(in.Phase), decision.GuardID)
		case decision.Verdict == guard.VerdictDeny:
			l.Security.GuardDenied(string(in.Phase), decision.GuardID, decision.Criterion, decision.Reason)
		case decision.Verdict == guard.VerdictAllowSpot:
			l.Security.GuardSpotlighted(string(in.Phase), decision.GuardID, decision.Reason)
		default:
			l.Security.GuardAllowed(string(in.Phase), decision.GuardID)
		}
	}
	if decision.Verdict == guard.VerdictAllowSpot {
		if l.Metrics != nil {
			l.Metrics.GuardSpotlights.Add(ctx, 1, metric.WithAttributes(
				attribute.String("guard.id", decision.GuardID),
			))
		}
		return true, decision, true
	}
	return decision.Verdict != guard.VerdictDeny, decision, false
}

// guardIDFromDecision returns the GuardID from a Decision, defaulting to
// "unknown" when the decision is nil or its GuardID is empty. Used for
// metric labelling on the error path where a Decision may not exist.
func guardIDFromDecision(d *guard.Decision) string {
	if d != nil && d.GuardID != "" {
		return d.GuardID
	}
	return "unknown"
}

// guardFailOpen returns the fail-open policy from RunConfig. When the
// guardrail is unconfigured, fail-open is false (which is moot because
// the guard is a Noop and cannot error).
func guardFailOpen(config *types.RunConfig) bool {
	if config == nil || config.GuardRail == nil {
		return false
	}
	return config.GuardRail.FailOpen
}

// collectUntrustedChunks returns the chunks of untrusted content that
// just entered the message history at the start of the given turn. On
// turn 0 this includes the initial user prompt and any DynamicContext
// entries (sorted by key for determinism). On subsequent turns it
// returns the Content field of every tool_result block in the last
// message — those entries arrived from external tool execution and
// have not yet been classified.
//
// v1 keeps this conservative: we do not attempt to classify earlier
// turns' content (already in history), nor model-emitted text (handled
// at PhasePostTurn). Only freshly arrived untrusted material is sent
// to the pre-turn guard, batched into a single classification call.
func collectUntrustedChunks(messages []types.Message, turn int, dynamicContext map[string]string, prompt string) []string {
	if turn == 0 {
		chunks := make([]string, 0, 1+len(dynamicContext))
		if prompt != "" {
			chunks = append(chunks, prompt)
		}
		// Sort keys for deterministic batched ordering — the guard
		// adapter assigns chunk indices to the batch and operators
		// debugging a deny benefit from a stable ordering.
		keys := make([]string, 0, len(dynamicContext))
		for k := range dynamicContext {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if v := dynamicContext[k]; v != "" {
				chunks = append(chunks, v)
			}
		}
		return chunks
	}
	if len(messages) == 0 {
		return nil
	}
	last := messages[len(messages)-1]
	if last.Role != "user" {
		return nil
	}
	chunks := make([]string, 0, len(last.Content))
	for _, b := range last.Content {
		if b.Type == "tool_result" && b.Content != "" {
			chunks = append(chunks, b.Content)
		}
	}
	return chunks
}

// batchUntrustedChunks concatenates chunks under per-chunk delimiters
// suitable for the granite-guardian batched composite criterion.
// Single-chunk batches still get a "--- chunk 0 ---" header so the
// model sees a consistent envelope shape regardless of chunk count.
func batchUntrustedChunks(chunks []string) string {
	if len(chunks) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, c := range chunks {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "--- chunk %d ---\n", i)
		sb.WriteString(c)
	}
	return sb.String()
}

// replaceUntrustedChunks replaces the content of every tool_result
// block in the last message with the supplied placeholder. Used when
// PhasePreTurn returns VerdictDeny to drop the untrusted content from
// this turn rather than feed it to the model. Turn 0 is a no-op
// because the user prompt itself is the untrusted content and we
// cannot rewrite it without producing an opaque user-facing failure
// — the loop's pre-turn deny on turn 0 surfaces as "guardrail_blocked"
// at the outer level instead. (Currently turn 0 PreTurn deny is logged
// but does not abort; v1 prefers progress to a strict refusal here.)
func replaceUntrustedChunks(messages []types.Message, turn int, placeholder string) {
	if turn == 0 {
		// Turn 0 untrusted content is the user prompt — already in the
		// system input. We log via the security event in the caller and
		// otherwise let the run continue; PostTurn guards still apply.
		return
	}
	if len(messages) == 0 {
		return
	}
	last := &messages[len(messages)-1]
	if last.Role != "user" {
		return
	}
	for i := range last.Content {
		if last.Content[i].Type == "tool_result" {
			last.Content[i].Content = placeholder
		}
	}
}

// spotlightUntrustedChunks rewraps every tool_result block in the last
// message via guard.ApplySpotlight. Used when PhasePreTurn returns
// VerdictAllowSpot for batched untrusted content. Turn 0 is a no-op
// because the user prompt already lives in the system input layer; we
// cannot retroactively spotlight it without rewriting prompts.
func spotlightUntrustedChunks(messages []types.Message, turn int) {
	if turn == 0 {
		return
	}
	if len(messages) == 0 {
		return
	}
	last := &messages[len(messages)-1]
	if last.Role != "user" {
		return
	}
	for i := range last.Content {
		if last.Content[i].Type == "tool_result" {
			last.Content[i].Content = guard.ApplySpotlight(last.Content[i].Content)
		}
	}
}

// lastAssistantText concatenates every text block in the assistant's
// final response. Tool-use blocks are skipped because PhasePreTool
// already gated them per-call.
func lastAssistantText(blocks []types.ContentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
			sb.WriteString("\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
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
