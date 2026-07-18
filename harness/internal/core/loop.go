package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/hook"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/ruleoftwo"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// outcomeCtxDone is a sentinel outcome from runInnerLoop signalling
// ctx.Done(); Run reclassifies it via context.Cause into
// cancelled/timeout/error.
const outcomeCtxDone = "_ctx_done"

// batchModeAdapter is the duck-typed view of a batch-wrapping provider
// adapter used to populate TurnTrace.Mode / BatchID without importing
// internal/provider, preserving the loop's pure-interfaces invariant.
type batchModeAdapter interface {
	LastBatchID() string
}

// turnModeInfo derives the TurnTrace.Mode / BatchID pair for the
// selected provider, defaulting to (TurnModeStreaming, "") for a nil
// or non-batch adapter.
func turnModeInfo(selected any) (mode, batchID string) {
	if ba, ok := selected.(batchModeAdapter); ok {
		return types.TurnModeBatch, ba.LastBatchID()
	}
	return types.TurnModeStreaming, ""
}

const (
	// maxVerificationRetries bounds retries before verification_failed terminates the run.
	maxVerificationRetries = 3

	// defaultMaxContextTokens is the assumed context window when RunConfig omits one.
	defaultMaxContextTokens = 200_000

	// defaultReserveForResponse is the token reserve for the model's
	// response; effectiveReserveForResponse scales it down for small budgets.
	defaultReserveForResponse = 64_000

	// smallContextReserveDivisor is the fraction of a small MaxTokens
	// budget reserved for the response once the flat default would not fit.
	smallContextReserveDivisor = 4

	// defaultTemperature is the sampling temperature applied when
	// RunConfig.Temperature is nil, biasing for determinism on coding
	// tasks. Per-model suppression for providers that reject non-default
	// values is handled by the quirks registry — see docs/provider-quirks.md.
	defaultTemperature = 0.1

	// tokenEstimationDivisor is the approximate character-to-token ratio (≈4 chars/token).
	tokenEstimationDivisor = 4

	// messageOverheadTokens accounts for the JSON structure around each message.
	messageOverheadTokens = 4

	// blockOverheadTokens accounts for the JSON structure around each content block.
	blockOverheadTokens = 3

	// toolDefinitionOverheadTokens accounts for the JSON structure wrapping each tool definition.
	toolDefinitionOverheadTokens = 10
)

// effectiveReserveForResponse resolves the response-token reserve
// subtracted from maxTokens before packing message history. Below
// defaultReserveForResponse it scales down to a quarter of maxTokens
// instead of the flat default, keeping the budget positive for small
// context windows; see docs/configuration.md for the full rationale.
func effectiveReserveForResponse(maxTokens int) int {
	if maxTokens <= 0 || maxTokens > defaultReserveForResponse {
		return defaultReserveForResponse
	}
	reserve := maxTokens / smallContextReserveDivisor
	if reserve < 1 {
		reserve = 1
	}
	return reserve
}

// Run executes the agentic loop:
//
//	repeat {
//	  run agentic loop until model says "done"
//	  run verifier
//	  if verifier passes → done
//	  if retries exhausted → done (with failure)
//	  else → feed verifier feedback back into the loop as a user message
//	}
func (l *AgenticLoop) Run(ctx context.Context, config *types.RunConfig) (*types.RunTrace, error) {
	// WithCancelCause lets Run later disambiguate control-plane
	// cancellation, deadline expiry, and caller cancellation via
	// context.Cause().
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)

	// OnControl fan-out is supported by all production transports;
	// sub-agents use NullTransport whose OnControl is a no-op.
	l.Transport.OnControl(func(event types.ControlEvent) {
		if event.Type == "cancel" {
			cancelRun(ErrCancelledByControlPlane)
		}
	})

	l.Trace.Start(config.RunID, config)

	// A non-nil TraceContext here means the caller (e.g. SpawnSubAgent)
	// already set one so child spans nest correctly; otherwise establish
	// the OTel root or a plain ctx as the span parent.
	if l.TraceContext == nil {
		if otelEmitter, ok := l.Trace.(*trace.OTelTraceEmitter); ok {
			l.TraceContext = otelEmitter.RootContext()
		} else {
			l.TraceContext = runCtx
		}
	}

	// Lets the control plane know the run is alive during long turns.
	stopHeartbeat := l.startHeartbeat(runCtx, 30*time.Second)

	// The Rule-of-Two-bearing entry shape (DynamicContextValue) carries a
	// per-entry Sensitive flag the validator and future GuardRail wiring
	// read; downstream consumers only need the string content, so this
	// projects to a values map here at the boundary.
	dynamicContext := config.DynamicContextValues()
	if len(dynamicContext) > 0 {
		var events []security.DynamicContextSanitizationEvent
		dynamicContext, events = security.SanitizeDynamicContext(dynamicContext)
		if l.Security != nil {
			for _, event := range events {
				l.Security.DynamicContextSanitized(event)
			}
		}
	}

	// Turn-0 Rule-of-Two scan: classify the operator prompt and the
	// sanitized dynamic-context values before Prompt.Build so a
	// latch-tier hit is recorded before the first provider call.
	var turnZeroTransition bool
	if config.Prompt != "" {
		if det := l.observeSensitive(runCtx, config, "prompt", 0, []string{config.Prompt}); det.Transition {
			turnZeroTransition = true
		}
	}
	if det := l.observeSensitive(runCtx, config, "dynamic_context", 0, sortedContextValues(dynamicContext)); det.Transition {
		turnZeroTransition = true
	}

	// Turn-0 Rule-of-Two enforcement, before Prompt.Build; see
	// docs/safety-rings.md for the redact/abort semantics.
	turnZeroAbort := false
	switch l.ruleOfTwoAction() {
	case "redact":
		if n := l.redactDynamicContext(dynamicContext); n > 0 {
			l.recordRuleOfTwoAction(runCtx, "redact")
		}
	case "abort":
		if turnZeroTransition {
			l.recordRuleOfTwoAction(runCtx, "abort")
			turnZeroAbort = true
		}
	}

	promptModel := config.EffectivePromptModel()
	systemPrompt, err := l.Prompt.Build(runCtx, prompt.PromptContext{
		Mode:           config.Mode,
		Workspace:      config.Executor.Workspace,
		MaxTurns:       config.MaxTurns,
		Model:          promptModel,
		DynamicContext: dynamicContext,
	})
	if err != nil {
		return l.finishWithError(runCtx, fmt.Errorf("build system prompt: %w", err))
	}

	// The otel emitter's opt-in gen_ai.system_instructions capture; a
	// no-op for emitters that don't implement this optional interface.
	if recorder, ok := l.Trace.(trace.SystemInstructionsRecorder); ok {
		recorder.RecordSystemInstructions(systemPrompt)
	}

	// Config metadata (model identity + guidance tier), not content, so
	// this is recorded regardless of content-capture settings.
	if recorder, ok := l.Trace.(trace.PromptResolutionRecorder); ok {
		recorder.RecordPromptResolution(promptModel, prompt.TierFor(promptModel))
	}

	// Runs before Git.Setup: the deterministic git strategy assumes an
	// existing checkout, so a clone hook must create it first. A nil
	// Hooks is a no-op. See docs/configuration.md#lifecycle-hooks.
	if l.Hooks != nil {
		_, preHookSpan := l.Tracer.Start(l.traceCtx(runCtx), "hooks."+hook.PhasePreRun)
		preResults, preErr := l.Hooks.RunPre(runCtx)
		l.recordHookExecutions(preResults)
		if preErr != nil {
			preHookSpan.RecordError(preErr)
			preHookSpan.SetStatus(codes.Error, preErr.Error())
		}
		preHookSpan.End()
		if preErr != nil {

			outcome := "setup_failed"
			if runCtx.Err() != nil {
				outcome = classifyCtxOutcome(context.Cause(runCtx))
			}
			return l.finishWithOutcome(runCtx, outcome, fmt.Errorf("pre-run hooks: %w", preErr))
		}
	}

	_, gitSetupSpan := l.Tracer.Start(l.traceCtx(runCtx), "git.setup")
	if err := l.Git.Setup(runCtx, config.Executor.Workspace, config.RunID); err != nil {
		gitSetupSpan.RecordError(err)
		gitSetupSpan.SetStatus(codes.Error, err.Error())
		gitSetupSpan.End()
		return l.finishWithError(runCtx, fmt.Errorf("git setup: %w", err))
	}
	gitSetupSpan.End()

	messages := buildMessages(config.Prompt)

	// Token tracking (cost estimation is a control plane concern).
	tokenTracker := &TokenTracker{}

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
		l.metricAttrs(attribute.String("run.mode", config.Mode)),
	)

	// Reset the per-run absolute token estimate before registering the
	// gauge callback so the first observation (before any Context.Prepare)
	// is 0 rather than the value from a previous run.
	l.lastContextTokens.Store(0)

	// Tagged with run.id and run.mode; unregistered at run end so the
	// OTel SDK does not keep observing this run after it finishes.
	unregisterCtxTokens, err := l.Metrics.RegisterContextTokensCallback(func() (int64, []attribute.KeyValue) {
		attrs := make([]attribute.KeyValue, 0, 2+len(l.MetricAttrs))
		attrs = append(attrs, l.MetricAttrs...)
		attrs = append(attrs,
			attribute.String("run.mode", config.Mode),
			attribute.String("run.id", config.RunID),
		)
		return l.lastContextTokens.Load(), attrs
	})
	if err != nil {
		l.Logger.Warn("register context_tokens callback failed", "error", err)
	}
	defer unregisterCtxTokens()

	// A turn-0 Rule-of-Two abort skips this loop entirely: the latch
	// already tripped before any provider call, so it terminates via
	// the same finish path as every other outcome below.
	outcome := "success"
	verificationAttempts := 0
	// finalAssistantText accumulates the last non-empty assistant text across
	// every runInnerLoop invocation (verification retries re-enter the loop).
	var finalAssistantText string
	if turnZeroAbort {
		outcome = "rule_of_two_violation"
	}

	for !turnZeroAbort && verificationAttempts <= maxVerificationRetries {

		var innerOutcome, innerFinalText string
		messages, innerOutcome, innerFinalText = l.runInnerLoop(runCtx, config, systemPrompt, messages, tokenTracker)
		if innerFinalText != "" {
			finalAssistantText = innerFinalText
		}

		if innerOutcome != "success" {
			outcome = innerOutcome
			break
		}

		l.Metrics.VerificationAttempts.Add(runCtx, 1, l.metricAttrs())
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

		verificationAttempts++
		if verificationAttempts > maxVerificationRetries {
			outcome = "verification_failed"
			break
		}

		feedback := vResult.Feedback
		if feedback == "" {
			feedback = "Verification failed. Please review and fix the issues."
		}
		messages = append(messages, types.Message{
			Role:      "user",
			Synthetic: true,
			Content: []types.ContentBlock{
				{Type: "text", Text: feedback},
			},
		})
	}

	// A cancel arriving between the inner loop returning and Verify
	// completing can otherwise mask the true termination reason behind
	// outcome="verification_error"; reclassify so the cancel/timeout
	// path below runs.
	if runCtx.Err() != nil && outcome != outcomeCtxDone {
		outcome = outcomeCtxDone
	}

	// See classifyCtxOutcome for the cause → outcome mapping.
	if outcome == outcomeCtxDone {
		cause := context.Cause(runCtx)
		outcome = classifyCtxOutcome(cause)
		l.setRootCancelAttribute(cause)
	}

	// Uses the parent ctx (not runCtx): if the run was cancelled,
	// git.Finalise should still be able to persist whatever state exists.
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

	// Runs on a ctx detached from the run's own deadline/cancellation
	// (context.WithoutCancel) so a long-running hook can still finish;
	// l.Shutdown still races a genuine process shutdown against that
	// budget. See docs/configuration.md#lifecycle-hooks and
	// docs/cloud-run-jobs.md for the outcome-override and SIGTERM detail.
	if l.Hooks != nil {
		postCtx, postCancel := context.WithTimeout(context.WithoutCancel(ctx), postHookBudget(config.Hooks))
		if l.Shutdown != nil {
			go func() {
				select {
				case <-l.Shutdown.Done():
					postCancel()
				case <-postCtx.Done():
				}
			}()
		}
		_, postHookSpan := l.Tracer.Start(l.traceCtx(ctx), "hooks."+hook.PhasePostRun)
		postResults, postErr := l.Hooks.RunPost(postCtx, outcome)
		l.recordHookExecutions(postResults)
		if postErr != nil {
			postHookSpan.RecordError(postErr)
			postHookSpan.SetStatus(codes.Error, postErr.Error())
		}
		postHookSpan.End()
		postCancel()
		if postErr != nil && outcome == "success" {
			outcome = "hook_failed"
		}
	}

	l.Logger.Info("run finished", "outcome", outcome)

	l.Metrics.RunDuration.Record(ctx, float64(time.Since(runStart).Milliseconds()),
		l.metricAttrs(
			attribute.String("run.mode", config.Mode),
			attribute.String("run.outcome", outcome),
		),
	)

	if err := l.Transport.Emit(types.HarnessEvent{
		Type:       "done",
		StopReason: outcome,
	}); err != nil {
		l.Logger.Warn("transport emit failed", "event", "done", "error", err)
	}

	stopHeartbeat()

	// PhasePostTurn is a sensitivity classifier, not a deterministic
	// secret redactor, so this is the single scrub site for the field —
	// see docs/security.md.
	finalAssistantText = security.Scrub(finalAssistantText)

	// Must run before Finish: emitters serialise RunTrace inside Finish,
	// so a post-Finish assignment on the returned struct would never
	// reach disk.
	if recorder, ok := l.Trace.(trace.FinalAssistantTextRecorder); ok {
		recorder.RecordFinalAssistantText(finalAssistantText)
	}

	// Uses the parent ctx: the trace exporter's ForceFlush should still
	// have a usable deadline even if the run-scoped ctx is cancelled.
	runTrace, traceErr := l.Trace.Finish(ctx, outcome)
	if traceErr != nil {
		return nil, fmt.Errorf("finish trace: %w", traceErr)
	}

	return runTrace, nil
}

// classifyCtxOutcome maps a context cancellation cause onto the
// outcome reported on "done" / RunTrace.Outcome: a nil cause or bare
// context.Canceled is a user-initiated cancellation (SIGINT/SIGTERM,
// or a caller's plain cancel()); ErrCancelledByControlPlane and
// context.DeadlineExceeded map to their own outcomes; anything else
// is "error".
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

// setRootCancelAttribute tags the root "run" OTel span with the
// cancellation reason, distinguishing plain/signal cancel from
// control-plane cancel even though both map to outcome="cancelled".
// Only applied when the run ended via ctx cancellation.
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
// Returns the updated messages, the outcome, and the last non-empty
// assistant text observed across the turns (empty when no turn produced
// any text).
func (l *AgenticLoop) runInnerLoop(
	ctx context.Context,
	config *types.RunConfig,
	systemPrompt string,
	messages []types.Message,
	tokenTracker *TokenTracker,
) ([]types.Message, string, string) {
	var lastStopReason string
	// finalAssistantText holds the last non-empty assistant text seen across
	// all turns. Threaded onto RunTrace.FinalAssistantText at loop completion.
	var finalAssistantText string
	stall := &stallDetector{}

	// Tool-choice escalation state: priorToolCalls tracks whether the
	// model has dispatched any tool yet this run; escalationsSoFar
	// bounds missed-tool recovery; pendingToolChoice forces a tool
	// choice on the next turn's Stream call, consumed once. All three
	// are inert when EscalationPolicy is nil (default off).
	priorToolCalls := 0
	escalationsSoFar := 0
	pendingToolChoice := types.ToolChoiceAuto

	for turn := 0; turn < config.MaxTurns; turn++ {
		l.Logger.Info("turn started", "turn", turn)

		budgetCheck := tokenTracker.CheckBudget(config.MaxTokenBudget)
		if !budgetCheck.WithinBudget {
			return messages, "budget_exceeded", finalAssistantText
		}

		// Sentinel outcome; see outcomeCtxDone.
		select {
		case <-ctx.Done():
			return messages, outcomeCtxDone, finalAssistantText
		default:
		}

		// See collectUntrustedChunks / docs/guardrails.md for what
		// counts as untrusted content per turn.
		var preTurnDynamic map[string]types.DynamicContextValue
		if turn == 0 {
			preTurnDynamic = config.DynamicContext
		}
		if chunks := collectUntrustedChunks(messages, turn, preTurnDynamic, config.Prompt); len(chunks) > 0 {
			// Deterministic-first, before the guard (see
			// docs/safety-rings.md). Turn 0 chunks were already scanned
			// in Run() under their own labels, so only turn>0 is
			// rescanned here.
			if turn > 0 {
				det := l.observeSensitive(ctx, config, "tool_result", turn, chunks)
				switch l.ruleOfTwoAction() {
				case "abort":
					if det.Transition {
						l.recordRuleOfTwoAction(ctx, "abort")
						return messages, "rule_of_two_violation", finalAssistantText
					}
				case "redact":
					if n := l.redactSensitiveSpans(messages, turn); n > 0 {
						l.recordRuleOfTwoAction(ctx, "redact")
					}
				}
			}
			batched := batchUntrustedChunks(chunks)
			in := guard.Input{
				Phase:   guard.PhasePreTurn,
				Content: batched,
				Source:  fmt.Sprintf("batched:n=%d", len(chunks)),
				Mode:    config.Mode,
				RunID:   config.RunID,
			}
			allow, decision, spotlight := l.guardCheck(ctx, in, guardFailOpen(config))
			l.ratchetRuleOfTwo(ctx, config, decision, turn)
			switch {
			case !allow:
				// Turn-0 vs later-turn deny handling differs; see
				// docs/guardrails.md.
				if turn == 0 {
					return messages, "guardrail_blocked", finalAssistantText
				}

				replaceUntrustedChunks(messages, turn, "[content blocked by guardrail]")
			case spotlight:
				if turn == 0 {
					// No-op on turn 0 (no tool_result blocks); skip the
					// metric so dashboards reflect applied spotlights only.
					break
				}
				spotlightUntrustedChunks(messages, turn)
				l.recordSpotlightApplied(ctx, guard.PhasePreTurn, decision)
			}
		}

		selection := l.Router.Select(ctx, router.RouterContext{
			Mode:           config.Mode,
			Turn:           turn,
			LastStopReason: lastStopReason,
			TokenUsage: router.TokenUsage{
				Input:  tokenTracker.Tokens().Input,
				Output: tokenTracker.Tokens().Output,
			},
		})

		// Token estimate includes system prompt and tool definitions —
		// these consume context but aren't in the message history.
		toolDefs := l.Tools.List()
		currentTokens := estimateCurrentTokens(messages) +
			estimateSystemPromptTokens(systemPrompt) +
			estimateToolDefinitionTokens(toolDefs)
		maxTokens := defaultMaxContextTokens
		if config.ContextStrategy.MaxTokens > 0 {
			maxTokens = config.ContextStrategy.MaxTokens
		}
		responseReserve := effectiveReserveForResponse(maxTokens)
		if turn == 0 && responseReserve < defaultReserveForResponse {
			// Once per run (turn 0): maxTokens is constant, so
			// re-emitting this every turn would be noise.
			l.Logger.Warn("context response reserve scaled down for small context budget",
				"maxTokens", maxTokens,
				"reserve", responseReserve,
				"defaultReserve", defaultReserveForResponse,
			)
			_ = l.Transport.Emit(types.HarnessEvent{
				Type: "warning",
				Message: fmt.Sprintf(
					"contextStrategy.maxTokens=%d is at or below the default response reserve (%d); scaling the reserve down to %d tokens so history packing keeps a usable budget",
					maxTokens, defaultReserveForResponse, responseReserve,
				),
			})
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
			ReserveForResponse: responseReserve,
		})
		if err != nil {
			contextSpan.RecordError(err)
			contextSpan.SetStatus(codes.Error, err.Error())
			contextSpan.End()
			if ctx.Err() != nil {
				return messages, outcomeCtxDone, finalAssistantText
			}
			return messages, "error", finalAssistantText
		}
		// Feeds the ContextTokens observable gauge registered in Run;
		// compaction shrinks this value, new messages grow it.
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
				l.metricAttrs(attribute.String("context.strategy", compaction.Strategy)),
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

		turnStart := time.Now()
		selectedProvider := l.Provider
		if selection.Provider != "" && len(l.Providers) > 0 {
			prov, ok := l.Providers[selection.Provider]
			if !ok {
				// Pre-resolution: no provider selected yet; empty Mode is
				// the documented "unknown" wire value (TurnTrace.Mode).
				l.Trace.RecordTurn(types.TurnTrace{
					Turn:       turn,
					StopReason: "error",
					DurationMs: time.Since(turnStart).Milliseconds(),
					Mode:       "",
					Model:      selection.Model,
				})
				return messages, "error", finalAssistantText
			}
			selectedProvider = prov
		}
		if selectedProvider == nil {
			// See above: pre-resolution Mode is empty.
			l.Trace.RecordTurn(types.TurnTrace{
				Turn:       turn,
				StopReason: "error",
				DurationMs: time.Since(turnStart).Milliseconds(),
				Mode:       "",
				Model:      selection.Model,
			})
			return messages, "error", finalAssistantText
		}
		providerAttrs := l.metricAttrs(
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

		// Forward an explicit override verbatim (including 0.0 for
		// greedy decoding); otherwise fall back to defaultTemperature
		// so the loop never silently sends a nil temperature and lets
		// a provider fall through to its own (higher) service default.
		temperature := config.Temperature
		if temperature == nil {
			temperature = types.Float64Ptr(defaultTemperature)
		}
		// Reset to auto immediately so a forced tool choice from the
		// escalation path applies to exactly one turn, not a sticky
		// mode. Zero value leaves non-escalating runs unaffected.
		turnToolChoice := pendingToolChoice
		pendingToolChoice = types.ToolChoiceAuto

		ch, err := selectedProvider.Stream(spanCtx, types.StreamParams{
			Model:       selection.Model,
			System:      systemPrompt,
			Messages:    preparedMessages,
			Tools:       l.Tools.List(),
			MaxTokens:   responseReserve,
			Temperature: temperature,
			ToolChoice:  turnToolChoice,
		})
		if err != nil {
			// ScrubHandler doesn't cover OTel spans; scrub explicitly
			// before it reaches the span status. See docs/security.md.
			scrubbedErr := security.Scrub(err.Error())
			providerSpan.RecordError(err)
			providerSpan.SetStatus(codes.Error, scrubbedErr)
			providerSpan.End()
			// Surfaces the failure outside OTel too (log + transport
			// warning); skipped when ctx is already cancelled — the
			// cancel/timeout path below reports instead.
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
			// Co-emit into tool-failure metrics only when tools were
			// offered: a pure text-only request error isn't a
			// tool-use failure from the model's perspective.
			if len(toolDefs) > 0 {
				l.Metrics.ToolFailures.Add(ctx, 1, l.metricAttrs(
					attribute.String("tool.name", observability.ToolNameProviderScope),
					attribute.String("category", observability.ToolFailureProviderRequest.String()),
					attribute.String("provider.type", selection.Provider),
					attribute.String("provider.model", selection.Model),
					attribute.String("run.mode", config.Mode),
				))
			}
			turnMode, turnBatchID := turnModeInfo(selectedProvider)
			l.Trace.RecordTurn(types.TurnTrace{
				Turn:       turn,
				StopReason: "error",
				DurationMs: time.Since(turnStart).Milliseconds(),
				Mode:       turnMode,
				BatchID:    turnBatchID,
				Model:      selection.Model,
			})
			// Lets the outer loop classify cancelled/timeout rather
			// than a generic error.
			if ctx.Err() != nil {
				return messages, outcomeCtxDone, finalAssistantText
			}
			return messages, "error", finalAssistantText
		}

		sr, streamErr := streamEventsToResult(ctx, ch, l.Transport, l.Logger)
		turnDuration := time.Since(turnStart)

		if streamErr != nil {
			// Same rationale as the Stream() scrub above; see docs/security.md.
			scrubbedErr := security.Scrub(streamErr.Error())
			providerSpan.RecordError(streamErr)
			providerSpan.SetStatus(codes.Error, scrubbedErr)
			providerSpan.End()
			// Same as the Stream() log+emit above.
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
			// Distinguished from provider_request_failed: a fault after
			// the stream opened is stream-side, not a rejected request.
			if len(toolDefs) > 0 {
				l.Metrics.ToolFailures.Add(ctx, 1, l.metricAttrs(
					attribute.String("tool.name", observability.ToolNameProviderScope),
					attribute.String("category", observability.ToolFailureProviderStream.String()),
					attribute.String("provider.type", selection.Provider),
					attribute.String("provider.model", selection.Model),
					attribute.String("run.mode", config.Mode),
				))
			}
			turnMode, turnBatchID := turnModeInfo(selectedProvider)
			l.Trace.RecordTurn(types.TurnTrace{
				Turn:       turn,
				StopReason: "error",
				DurationMs: turnDuration.Milliseconds(),
				Mode:       turnMode,
				BatchID:    turnBatchID,
				Model:      selection.Model,
			})
			// Lets the outer loop classify a ctx-abort correctly.
			if ctx.Err() != nil {
				return messages, outcomeCtxDone, finalAssistantText
			}
			return messages, "error", finalAssistantText
		}
		providerSpan.SetAttributes(
			attribute.Int("tokens.output", sr.OutputTokens),
			attribute.String("stop_reason", sr.StopReason),
		)
		providerSpan.End()

		lastStopReason = sr.StopReason

		// Output tokens come from the stream; input is estimated from
		// the messages sent plus system prompt and tools.
		inputTokenEstimate := estimateCurrentTokens(preparedMessages) +
			estimateSystemPromptTokens(systemPrompt) +
			estimateToolDefinitionTokens(toolDefs)
		tokenTracker.RecordTurn(inputTokenEstimate, sr.OutputTokens)

		turnMode, turnBatchID := turnModeInfo(selectedProvider)
		l.Trace.RecordTurn(types.TurnTrace{
			Turn: turn,
			Tokens: types.TokenUsage{
				Input:  inputTokenEstimate,
				Output: sr.OutputTokens,
			},
			StopReason: sr.StopReason,
			DurationMs: turnDuration.Milliseconds(),
			Mode:       turnMode,
			BatchID:    turnBatchID,
			Model:      selection.Model,
		})

		// Persisted as a TurnRecord (full transcript) by recording
		// emitters (JSONL); summary-only emitters ignore it. Tool
		// calls are filled in after planAndDispatch runs below.
		turnRecord := types.TurnRecord{
			Turn: turn,
			ModelInput: types.ModelInput{
				Messages: preparedMessages,
				Tools:    toolDefs,
				Model:    selection.Model,
			},
			ModelOutput: sr.Blocks,
		}

		modeAttr := l.metricAttrs(attribute.String("run.mode", config.Mode))
		l.Metrics.Turns.Add(ctx, 1, modeAttr)
		l.Metrics.TokensInput.Add(ctx, int64(inputTokenEstimate), l.metricAttrs())
		l.Metrics.TokensOutput.Add(ctx, int64(sr.OutputTokens), l.metricAttrs())
		l.Metrics.TurnDuration.Record(ctx, float64(turnDuration.Milliseconds()), modeAttr)

		l.Logger.Info("turn completed", "turn", turn,
			"tokens.input", inputTokenEstimate,
			"tokens.output", sr.OutputTokens,
			"stopReason", sr.StopReason)

		// Carries provider replay state so the next request can
		// round-trip it.
		messages = appendAssistantContent(messages, sr.Blocks, sr.ReplayFields)

		// finalText feeds RunTrace.FinalAssistantText and the end_turn
		// PhasePostTurn guard below. priorFinalText is the pre-commit
		// snapshot the guard must return on deny — see docs/guardrails.md.
		finalText := lastAssistantText(sr.Blocks)
		priorFinalText := finalAssistantText
		if finalText != "" {
			finalAssistantText = finalText
		}

		toolCalls := collectToolCalls(sr.Blocks)

		// The loop makes no judgement — it forwards turn facts to
		// EscalationPolicy (nil = inert, off by default) and acts on
		// its decision, before the terminal end_turn/non-tool-use
		// returns below so a recovery continues the loop.
		if len(toolCalls) == 0 && sr.StopReason != "tool_use" {
			decision := l.escalationDecision(EscalationInput{
				Mode:             config.Mode,
				Provider:         selection.Provider,
				Model:            selection.Model,
				StopReason:       sr.StopReason,
				ToolsAvailable:   len(toolDefs) > 0,
				PriorToolCalls:   priorToolCalls,
				Turn:             turn,
				EscalationsSoFar: escalationsSoFar,
			})
			if decision.Kind != EscalationNone {
				// Persist the no-tool turn transcript before the retry so
				// replay/mining still sees the rejected answer. The retry
				// turn is recorded separately on its own iteration.
				l.Trace.RecordTurnRecord(turnRecord)
				messages = l.applyEscalation(ctx, config, decision, selection.Provider, selection.Model, messages, &pendingToolChoice)
				escalationsSoFar++
				continue
			}
		}

		if sr.StopReason == "end_turn" {
			// Even a no-tool-call end_turn is worth preserving: replay
			// needs the final answer, and mine-failures distinguishes
			// end_turn from ran-out-of-turns.
			l.Trace.RecordTurnRecord(turnRecord)
			// PhasePostTurn: a deny returns guardrail_blocked; spotlight
			// is log-only in v1 — see docs/guardrails.md.
			if finalText != "" {
				in := guard.Input{
					Phase:   guard.PhasePostTurn,
					Content: finalText,
					Source:  "model_output",
					Mode:    config.Mode,
					RunID:   config.RunID,
				}
				allow, decision, spotlight := l.guardCheck(ctx, in, guardFailOpen(config))
				l.ratchetRuleOfTwo(ctx, config, decision, turn)
				if !allow {

					return messages, "guardrail_blocked", priorFinalText
				}
				if spotlight {

					l.Logger.Info("postTurn guard requested spotlight; not rewriting in v1")
				}
			}
			return messages, "success", finalAssistantText
		}
		if sr.StopReason != "tool_use" {
			if sr.StopReason == "" {
				l.Logger.Warn("provider returned empty stop reason", "turn", turn)
				return messages, "error", finalAssistantText
			}
			// Non-tool-use, non-end-turn stop reasons still represent
			// a completed exchange that replay/mining cares about.
			l.Trace.RecordTurnRecord(turnRecord)
			return messages, sr.StopReason, finalAssistantText
		}
		if len(toolCalls) == 0 {
			// Provider declared tool_use but produced no tool blocks —
			// degenerate but observable.
			l.Trace.RecordTurnRecord(turnRecord)
			return messages, "error", finalAssistantText
		}

		// planAndDispatch preserves result/stall order, per-call
		// timeouts, and ctx cancellation; see dispatch.go. Provider/
		// model forwarded so tool_failures metrics attribute back to
		// the emitting model.
		toolResults, toolRecords, stallOutcome := l.planAndDispatch(ctx, config, toolCalls, stall, selection.Provider, selection.Model)
		turnRecord.ToolCalls = toolRecords
		l.Trace.RecordTurnRecord(turnRecord)
		messages = appendToolResults(messages, toolResults)
		// Escalation triggers only on the first assistant turn with
		// no prior tool calls; once non-zero, a later no-tool answer
		// is a legitimate judgement and left alone.
		priorToolCalls += len(toolCalls)
		if stallOutcome != "" {
			return messages, stallOutcome, finalAssistantText
		}

		// Prevents the next turn from sending an over-budget context.
		budgetCheck = tokenTracker.CheckBudget(config.MaxTokenBudget)
		if !budgetCheck.WithinBudget {
			return messages, "budget_exceeded", finalAssistantText
		}

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

	return messages, "max_turns", finalAssistantText
}

// applyEscalation performs the recovery EscalationPolicy chose for a
// suspected missed-tool turn, returning the (possibly extended)
// message history. EscalationNative forces ToolChoiceRequired on the
// next turn (only honoured if the provider supports it); EscalationPrompt
// appends a user message nudging the model. Both emit a
// stirrup.harness.tool_failures observation and an escalation span for
// operator auditing.
func (l *AgenticLoop) applyEscalation(
	ctx context.Context,
	config *types.RunConfig,
	decision EscalationDecision,
	providerType, model string,
	messages []types.Message,
	pendingToolChoice *types.ToolChoiceMode,
) []types.Message {
	switch decision.Kind {
	case EscalationNative:
		*pendingToolChoice = types.ToolChoiceRequired
	case EscalationPrompt:
		if decision.PromptMessage != "" {
			messages = append(messages, types.Message{
				Role:      "user",
				Synthetic: true,
				Content: []types.ContentBlock{
					{Type: "text", Text: decision.PromptMessage},
				},
			})
		}
	}

	// tool.name is the bounded empty-sentinel — no tool was involved,
	// matching the provider_request/stream failure paths. Gate on
	// IsValid() so a future category can't widen label cardinality.
	if observability.ToolFailureNoToolWhenRequired.IsValid() {
		l.Metrics.ToolFailures.Add(ctx, 1, l.metricAttrs(
			attribute.String("tool.name", observability.ToolNameProviderScope),
			attribute.String("category", observability.ToolFailureNoToolWhenRequired.String()),
			attribute.String("provider.type", providerType),
			attribute.String("provider.model", model),
			attribute.String("run.mode", config.Mode),
		))
	}

	// EscalationPolicy is a public interface — a future policy could
	// interpolate untrusted content into Reason, so scrub before it
	// reaches the span/log (ScrubHandler doesn't cover either; see
	// docs/security.md).
	scrubbedReason := security.Scrub(decision.Reason)

	_, span := l.Tracer.Start(l.traceCtx(ctx), "tool_choice.escalation",
		oteltrace.WithAttributes(
			attribute.String("run.mode", config.Mode),
			attribute.String("escalation.kind", decision.Kind.String()),
			// Reason is a short static string; see EscalationDecision.Reason
			// godoc — NOT a metric label.
			attribute.String("escalation.reason", scrubbedReason),
		),
	)
	span.End()

	l.Logger.Info("tool-choice escalation",
		"mode", config.Mode,
		"kind", decision.Kind.String(),
		"reason", scrubbedReason,
	)

	return messages
}

// RunFollowUpLoop waits for follow-up user_response control events
// after the primary run completes, re-running the agentic loop with
// each new prompt. Exits on grace-period timeout, ctx cancellation, or
// a "cancel" control event. graceSecs must be > 0; the transport must
// support fan-out OnControl (GRPCTransport and StdioTransport do).
func RunFollowUpLoop(ctx context.Context, loop *AgenticLoop, config *types.RunConfig, graceSecs int) {
	followUpCh := make(chan string, 1)
	cancelCh := make(chan struct{}, 1)

	loop.Transport.OnControl(func(event types.ControlEvent) {
		switch event.Type {
		case "user_response":
			select {
			case followUpCh <- event.UserResponse:
			default:
				// Already queued; control plane should wait for "done"
				// before sending another request.
			}
		case "cancel":
			// In-flight Run has its own cancel handler and terminates on
			// the next turn boundary.
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
			// Non-blocking pre-check biases toward cancellation: exit
			// before racing the ticker if ctx is already done.
			select {
			case <-ctx.Done():
				return
			default:
			}
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
	// When the caller's ctx already carries an active span (PhasePreTool,
	// via toolSpanCtx) nest the guard span under it; PreTurn/PostTurn carry
	// no span, so fall back to the loop's run-root TraceContext.
	spanParent := ctx
	if !oteltrace.SpanFromContext(ctx).SpanContext().IsValid() {
		spanParent = l.traceCtx(ctx)
	}
	_, span := l.Tracer.Start(spanParent, "guard."+string(in.Phase),
		oteltrace.WithAttributes(
			attribute.String("guard.phase", string(in.Phase)),
			attribute.String("guard.source", in.Source),
		),
	)
	decision, err := l.GuardRail.Check(ctx, in)
	elapsed := time.Since(start)
	if err != nil {
		// Scrub once, reuse for span/log/security-event — see
		// docs/security.md.
		scrubbed := security.Scrub(err.Error())
		span.RecordError(err)
		span.SetStatus(codes.Error, scrubbed)
		span.End()
		guardID := guardIDFromDecision(decision)
		if l.Metrics != nil {
			l.Metrics.GuardErrors.Add(ctx, 1, l.metricAttrs(
				attribute.String("guard.phase", string(in.Phase)),
				attribute.String("guard.id", guardID),
			))
			l.Metrics.GuardDuration.Record(ctx, float64(elapsed.Milliseconds()), l.metricAttrs(
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
		// Contract violation (nil, nil): record synthetic allow rather
		// than panicking downstream.
		decision = &guard.Decision{Verdict: guard.VerdictAllow, GuardID: "unknown"}
	}
	span.SetAttributes(
		attribute.String("guard.id", decision.GuardID),
		attribute.String("guard.verdict", string(decision.Verdict)),
		attribute.Float64("guard.score", decision.Score),
		attribute.Int64("guard.latency_ms", elapsed.Milliseconds()),
	)
	span.End()

	// Distinct from a regular allow — surfaced as its own metric/event
	// so dashboards don't confuse skips with classifier-validated
	// allows. See docs/guardrails.md.
	isSkip := decision.Reason == guard.ReasonSkippedMinChunk
	if l.Metrics != nil {
		if isSkip {
			l.Metrics.GuardSkips.Add(ctx, 1, l.metricAttrs(
				attribute.String("guard.phase", string(in.Phase)),
				attribute.String("guard.id", decision.GuardID),
				attribute.String("reason", "min_chunk_chars"),
			))
		} else {
			l.Metrics.GuardChecks.Add(ctx, 1, l.metricAttrs(
				attribute.String("guard.phase", string(in.Phase)),
				attribute.String("guard.id", decision.GuardID),
				attribute.String("guard.verdict", string(decision.Verdict)),
			))
		}
		l.Metrics.GuardDuration.Record(ctx, float64(elapsed.Milliseconds()), l.metricAttrs(
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
			// Emitted by the call site only after the spotlight is
			// actually applied (recordSpotlightApplied), not here.
		default:
			l.Security.GuardAllowed(string(in.Phase), decision.GuardID)
		}
	}
	if decision.Verdict == guard.VerdictAllowSpot {
		return true, decision, true
	}
	return decision.Verdict != guard.VerdictDeny, decision, false
}

// recordSpotlightApplied emits the spotlight security event and metric.
// Call this only after a spotlight request has actually been honoured
// (e.g. spotlightUntrustedChunks has run). Calling guardCheck alone
// must NOT increment the spotlights counter — the loop currently
// no-ops PostTurn spotlight requests, and conflating "requested" with
// "applied" would mislead operators monitoring spotlight rates.
func (l *AgenticLoop) recordSpotlightApplied(ctx context.Context, phase guard.Phase, decision *guard.Decision) {
	if decision == nil {
		return
	}
	if l.Metrics != nil {
		l.Metrics.GuardSpotlights.Add(ctx, 1, l.metricAttrs(
			attribute.String("guard.id", decision.GuardID),
			attribute.String("guard.phase", string(phase)),
		))
	}
	if l.Security != nil {
		l.Security.GuardSpotlighted(string(phase), decision.GuardID, decision.Reason)
	}
}

// observeSensitive runs the Rule-of-Two monitor over freshly-arrived
// untrusted chunks, recording sensitive_scan_ms and, on a finding,
// sensitive_data_detected / rule_of_two_detections plus the
// once-per-run rule_of_two_triggered warning at the latch transition.
// It only observes; the caller applies enforcement from the returned
// Detection. Nil monitor no-ops.
func (l *AgenticLoop) observeSensitive(ctx context.Context, config *types.RunConfig, source string, turn int, chunks []string) ruleoftwo.Detection {
	if l.RuleOfTwo == nil || len(chunks) == 0 {
		return ruleoftwo.Detection{}
	}
	start := time.Now()
	det := l.RuleOfTwo.ObserveChunks(ctx, source, turn, chunks)
	if l.Metrics != nil {
		// Fractional ms: scans are routinely sub-millisecond, and integer
		// truncation would flatten the histogram to zero.
		elapsedMs := float64(time.Since(start)) / float64(time.Millisecond)
		l.Metrics.SensitiveScan.Record(ctx, elapsedMs, l.metricAttrs(
			attribute.String("source", source),
		))
	}
	if len(det.Patterns) == 0 {
		return det
	}
	action := l.RuleOfTwo.Action()
	if l.Security != nil {
		l.Security.SensitiveDataDetected(det.Patterns, det.Tier, source, turn, action, det.Transition)
	}
	if l.Metrics != nil {
		for _, p := range det.Patterns {
			l.Metrics.RuleOfTwoDetections.Add(ctx, 1, l.metricAttrs(
				attribute.String("pattern", p),
				attribute.String("tier", det.Tier),
				attribute.String("source", source),
			))
		}
	}
	if det.Transition {
		l.emitRuleOfTwoTriggered(config, source, action)
	}
	return det
}

// ruleOfTwoAction returns the monitor's effective on-detect action, or
// "" when no monitor is wired. "warn" (observe-only) and "block-external"
// / "ask-upstream" (handled in the permission gate) carry no loop-level
// behaviour; only "redact" and "abort" are acted on at the scan sites.
func (l *AgenticLoop) ruleOfTwoAction() string {
	if l.RuleOfTwo == nil {
		return ""
	}
	return l.RuleOfTwo.Action()
}

// recordRuleOfTwoAction increments stirrup.ruleoftwo.actions for one
// applied enforcement action (a redacted chunk set, an abort). Gate
// denials are recorded by the gate itself in the permission layer.
func (l *AgenticLoop) recordRuleOfTwoAction(ctx context.Context, action string) {
	if l.Metrics == nil {
		return
	}
	l.Metrics.RuleOfTwoActions.Add(ctx, 1, l.metricAttrs(
		attribute.String("action", action),
	))
}

// ratchetRuleOfTwo forwards a guard decision's criterion to the
// Rule-of-Two monitor's one-way ratchet. See docs/safety-rings.md
// "The guard-criterion ratchet".
func (l *AgenticLoop) ratchetRuleOfTwo(ctx context.Context, config *types.RunConfig, decision *guard.Decision, turn int) {
	if l.RuleOfTwo == nil || decision == nil || decision.Criterion == "" {
		return
	}
	if !l.RuleOfTwo.TripFromGuard(decision.GuardID, decision.Criterion) {
		return
	}
	source := "guard:" + decision.GuardID
	action := l.RuleOfTwo.Action()
	// Namespaced "guard:<criterion>" so a guard-originated trip can never
	// impersonate a deterministic detector; see docs/safety-rings.md.
	pattern := "guard:" + decision.Criterion
	if l.Security != nil {
		l.Security.SensitiveDataDetected([]string{pattern}, security.TierLatch, source, turn, action, true)
	}
	if l.Metrics != nil {
		l.Metrics.RuleOfTwoDetections.Add(ctx, 1, l.metricAttrs(
			attribute.String("pattern", pattern),
			attribute.String("tier", security.TierLatch),
			attribute.String("source", source),
		))
	}
	l.emitRuleOfTwoTriggered(config, source, action)
}

// emitRuleOfTwoTriggered records the once-per-run rule_of_two_triggered
// event/warning at the latch transition; see docs/safety-rings.md.
func (l *AgenticLoop) emitRuleOfTwoTriggered(config *types.RunConfig, source, action string) {
	untrusted, _, external := types.RuleOfTwoState(config)
	scanningSuspended := action != "redact"
	if l.Security != nil {
		l.Security.RuleOfTwoTriggered(untrusted, external, action, source, scanningSuspended)
	}
	var msg string
	if l.RuleOfTwo != nil && l.RuleOfTwo.Enforcing() {
		msg = fmt.Sprintf("rule of two: sensitive data detected (source %q); enforcing action %q", source, action)
	} else {
		msg = fmt.Sprintf("rule of two: sensitive data detected (source %q); action %q is observe-only", source, action)
	}
	_ = l.Transport.Emit(types.HarnessEvent{
		Type:    "warning",
		Message: msg,
	})
}

// sortedContextValues returns the non-empty dynamic-context values in
// key order, matching collectUntrustedChunks' deterministic ordering so
// detection events are stable across runs of the same config.
func sortedContextValues(dynamicContext map[string]string) []string {
	if len(dynamicContext) == 0 {
		return nil
	}
	keys := make([]string, 0, len(dynamicContext))
	for k := range dynamicContext {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, k := range keys {
		if v := dynamicContext[k]; v != "" {
			values = append(values, v)
		}
	}
	return values
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
// just entered the message history at the start of the given turn: on
// turn 0, the initial prompt plus DynamicContext entries (sorted by
// key); on later turns, every tool_result block's Content and
// Structured payload in the last message — both are model-visible
// (see docs/architecture.md "Structured tool results"), so both need
// classification. v1 does not reclassify earlier-turn content already
// in history, nor model-emitted text (handled at PhasePostTurn).
func collectUntrustedChunks(messages []types.Message, turn int, dynamicContext map[string]types.DynamicContextValue, prompt string) []string {
	if turn == 0 {
		chunks := make([]string, 0, 1+len(dynamicContext))
		if prompt != "" {
			chunks = append(chunks, prompt)
		}
		// Deterministic ordering: the guard adapter assigns chunk indices,
		// and operators debugging a deny benefit from stability.
		keys := make([]string, 0, len(dynamicContext))
		for k := range dynamicContext {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if v := dynamicContext[k]; v.Value != "" {
				chunks = append(chunks, v.Value)
			}
		}
		return chunks
	}
	if len(messages) == 0 {
		return nil
	}
	last := messages[len(messages)-1]
	// Synthetic messages (escalation prompts, verifier feedback) are
	// harness-controlled, not untrusted external input.
	if last.Role != "user" || last.Synthetic {
		return nil
	}
	chunks := make([]string, 0, len(last.Content))
	for _, b := range last.Content {
		if b.Type != "tool_result" {
			continue
		}
		if b.Content != "" {
			chunks = append(chunks, b.Content)
		}
		if len(b.Structured) > 0 {
			chunks = append(chunks, string(b.Structured))
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

// replaceUntrustedChunks replaces every tool_result block's content in
// the last message with placeholder, for a PhasePreTurn deny on turn
// N>0. Turn 0 has no tool_result blocks yet (the prompt is the
// untrusted content) — callers must abort instead; see
// docs/guardrails.md.
func replaceUntrustedChunks(messages []types.Message, turn int, placeholder string) {
	if turn == 0 {
		// Defensive no-op; callers must abort on turn 0 instead.
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

// redactSensitiveSpans rewrites latch-tier sensitive spans in every
// just-arrived tool_result block's Content AND Structured payload (both
// are model-visible; see docs/architecture.md "Structured tool
// results"). Returns the number of spans replaced. Turn 0 is a no-op —
// its untrusted content is the prompt/dynamic context, handled by
// redactDynamicContext before Prompt.Build.
func (l *AgenticLoop) redactSensitiveSpans(messages []types.Message, turn int) int {
	if turn == 0 || len(messages) == 0 || l.RuleOfTwo == nil {
		return 0
	}
	last := &messages[len(messages)-1]
	if last.Role != "user" {
		return 0
	}
	total := 0
	for i := range last.Content {
		if last.Content[i].Type != "tool_result" {
			continue
		}
		if last.Content[i].Content != "" {
			redacted, n := l.RuleOfTwo.Redact(last.Content[i].Content)
			if n > 0 {
				last.Content[i].Content = redacted
				total += n
			}
		}
		if len(last.Content[i].Structured) > 0 {
			redacted, n := l.RuleOfTwo.Redact(string(last.Content[i].Structured))
			if n > 0 {
				// Keep the JSON valid: fall back to a whole-payload
				// placeholder if the redaction broke JSON structure.
				if json.Valid([]byte(redacted)) {
					last.Content[i].Structured = json.RawMessage(redacted)
				} else {
					// json.Marshal of a Go string never errors; avoids
					// relying on RedactionPlaceholder staying quote-free.
					b, _ := json.Marshal(ruleoftwo.RedactionPlaceholder)
					last.Content[i].Structured = json.RawMessage(b)
				}
				total += n
			}
		}
	}
	return total
}

// redactDynamicContext rewrites latch-tier sensitive spans in every
// dynamic-context value in place, before Prompt.Build. Returns the
// number of spans replaced. config.Prompt is never redacted here —
// see docs/safety-rings.md.
func (l *AgenticLoop) redactDynamicContext(dynamicContext map[string]string) int {
	if l.RuleOfTwo == nil {
		return 0
	}
	total := 0
	for k, v := range dynamicContext {
		redacted, n := l.RuleOfTwo.Redact(v)
		if n > 0 {
			dynamicContext[k] = redacted
			total += n
		}
	}
	return total
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

// recordHookExecutions forwards each lifecycle hook result to the
// trace emitter's optional HookRecorder capability and emits a
// transport "warning" for any continueOnError failure — visible to
// the control plane even though it never touches the run's outcome.
func (l *AgenticLoop) recordHookExecutions(execs []types.HookExecution) {
	recorder, hasRecorder := l.Trace.(trace.HookRecorder)
	for _, exec := range execs {
		if hasRecorder {
			recorder.RecordHookExecution(exec)
		}
		if !exec.ContinuedOnError {
			continue
		}
		if err := l.Transport.Emit(types.HarnessEvent{
			Type:    "warning",
			Message: formatHookWarning(exec),
		}); err != nil {
			l.Logger.Warn("transport emit failed", "event", "warning", "error", err)
		}
	}
}

// formatHookWarning renders the transport "warning" message for a
// continueOnError hook failure. The full Command is deliberately
// omitted (it can be up to 16KB); Name identifies the hook when the
// operator supplied one.
func formatHookWarning(exec types.HookExecution) string {
	label := fmt.Sprintf("%s hook %d", exec.Phase, exec.Index)
	if exec.Name != "" {
		label = fmt.Sprintf("%s (%s)", label, exec.Name)
	}
	if exec.Error == "" {
		return label + " failed and continued"
	}
	return fmt.Sprintf("%s failed and continued: %s", label, exec.Error)
}

// postHookBudget returns the wall-clock budget for the detached
// post-hook ctx: the sum of every postRun hook's effective timeout
// plus a 30s margin (see docs/configuration.md#lifecycle-hooks).
// ValidateRunConfig bounds the sum, so this is always bounded.
func postHookBudget(hooks *types.HooksConfig) time.Duration {
	sum := 0
	if hooks != nil {
		for _, h := range hooks.PostRun {
			sum += types.EffectiveHookTimeout(h)
		}
	}
	return time.Duration(sum)*time.Second + 30*time.Second
}

// finishWithError records an "error" outcome and finishes the trace.
func (l *AgenticLoop) finishWithError(ctx context.Context, err error) (*types.RunTrace, error) {
	return l.finishWithOutcome(ctx, "error", err)
}

// finishWithOutcome generalises finishWithError to a caller-supplied
// outcome (e.g. "setup_failed" from a fatal preRun hook failure) and
// finishes the trace.
func (l *AgenticLoop) finishWithOutcome(ctx context.Context, outcome string, err error) (*types.RunTrace, error) {
	if emitErr := l.Transport.Emit(types.HarnessEvent{
		Type:    "error",
		Message: err.Error(),
	}); emitErr != nil {
		l.Logger.Warn("transport emit failed", "event", "error", "error", emitErr)
	}
	// Always emit "done" (not just "error") so control planes relying on
	// it as the terminal signal see this early-return path too.
	if emitErr := l.Transport.Emit(types.HarnessEvent{
		Type:       "done",
		StopReason: outcome,
	}); emitErr != nil {
		l.Logger.Warn("transport emit failed", "event", "done", "error", emitErr)
	}
	runTrace, traceErr := l.Trace.Finish(ctx, outcome)
	if traceErr != nil {
		l.Logger.Warn("trace finish failed", "error", traceErr)
	}
	return runTrace, err
}
