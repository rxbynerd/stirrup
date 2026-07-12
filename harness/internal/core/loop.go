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
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/commandoutput"
	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
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

// batchModeAdapter is the duck-typed view of a batch-wrapping
// ProviderAdapter the loop type-asserts against to populate
// TurnTrace.Mode / BatchID (#138). The loop avoids importing
// internal/provider here so the loop-as-pure-interfaces invariant
// (CLAUDE.md) is preserved; any future wrapper that surfaces a batch
// identifier need only implement this single method.
type batchModeAdapter interface {
	LastBatchID() string
}

// turnModeInfo derives the TurnTrace.Mode / BatchID pair for the
// selected provider. A nil or non-batch adapter resolves to
// (TurnModeStreaming, "") so streaming-only paths take no extra
// branches at the construction site.
func turnModeInfo(selected any) (mode, batchID string) {
	if ba, ok := selected.(batchModeAdapter); ok {
		return types.TurnModeBatch, ba.LastBatchID()
	}
	return types.TurnModeStreaming, ""
}

const (
	// maxVerificationRetries is the maximum number of times the verifier can
	// request a retry before the run is terminated with verification_failed.
	maxVerificationRetries = 3

	// defaultMaxContextTokens is the assumed context window size when the
	// RunConfig does not specify one explicitly.
	defaultMaxContextTokens = 200_000

	// defaultReserveForResponse is the number of tokens reserved for the
	// model's response within the context window, for any budget large
	// enough to absorb it. See effectiveReserveForResponse for the
	// small-budget case.
	defaultReserveForResponse = 64_000

	// smallContextReserveDivisor is the fraction of a small
	// ContextStrategy.MaxTokens budget reserved for the model's response
	// once the flat defaultReserveForResponse would consume the whole
	// window (see effectiveReserveForResponse). Skewed toward history
	// over completion length: an agentic coding turn's context is
	// typically dominated by file contents and tool output, not the
	// model's own response.
	smallContextReserveDivisor = 4

	// defaultTemperature is the sampling temperature applied to every
	// provider call when RunConfig.Temperature is nil. A low value
	// biases for determinism on coding tasks — the historical
	// hardcoded value, preserved as the harness default so unset
	// configs see no behaviour change.
	//
	// Not every model accepts this value on the wire: Claude Opus 4.7+,
	// Claude Sonnet 5, and Claude Fable 5 / Mythos 5 return an HTTP 400
	// on a non-default temperature rather than ignoring it (as do
	// OpenAI's reasoning-class models, which already had this
	// constraint). The loop still resolves defaultTemperature
	// unconditionally for every provider — narrowing it here would
	// need per-model knowledge this package deliberately does not
	// have (CLAUDE.md: the loop is a pure function of its interfaces).
	// The adapters suppress the field for those models via the
	// per-(provider, model) quirks registry instead
	// (quirks.AnthropicBehaviourFlags.OmitSamplingParams,
	// quirks.OpenAIBehaviourFlags.OmitSamplingParams) — see
	// docs/provider-quirks.md.
	defaultTemperature = 0.1

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

// effectiveReserveForResponse resolves the response-token reserve
// subtracted from maxTokens before the context strategy packs message
// history, and the max output tokens requested from the provider.
//
// Historically both call sites used the flat defaultReserveForResponse
// (64k) unconditionally. For any config.ContextStrategy.MaxTokens at or
// below that constant, `available := budget.MaxTokens -
// budget.ReserveForResponse` (computed by every context strategy) goes
// non-positive, and the strategy short-circuits to its
// minimum-preserved-messages branch on every turn — silently dropping
// the run's actual history, including the original user prompt on
// turns after the first (#444). This is squarely in the small-context
// local-model regime (LM Studio / Ollama commonly run 4k-32k windows).
//
// Below the flat-reserve threshold, scale the reserve down to a
// quarter of maxTokens instead: the resulting budget is always
// positive, and — because a quarter of even a very small window still
// leaves the majority for history — the context strategy gets a real,
// useful budget rather than a degenerate one. Configs whose maxTokens
// is 0 (unset — the loop assumes defaultMaxContextTokens) or above
// defaultReserveForResponse are untouched byte-for-byte, so this is
// not a behaviour change for any config that did not already hit the
// bug.
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
	//
	// If TraceContext was set by the caller before Run (notably by
	// SpawnSubAgent, which threads the parent's tool.spawn_agent span ctx
	// into the child loop), preserve it so child spans nest correctly. The
	// parent run path leaves TraceContext nil at construction time, so this
	// fall-through still establishes the OTel root or a plain ctx as the
	// span parent for top-level runs.
	if l.TraceContext == nil {
		if otelEmitter, ok := l.Trace.(*trace.OTelTraceEmitter); ok {
			l.TraceContext = otelEmitter.RootContext()
		} else {
			l.TraceContext = runCtx
		}
	}

	// Start heartbeat emission so the control plane knows we are alive.
	stopHeartbeat := l.startHeartbeat(runCtx, 30*time.Second)

	// Build the system prompt. The Rule-of-Two-bearing entry shape
	// (DynamicContextValue) carries a per-entry Sensitive flag the
	// validator and any future GuardRail wiring read; downstream
	// consumers (Sanitize, PromptBuilder) only need the string content,
	// so we project to a values map here at the boundary.
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
	// Observe-only — redact-mode rewriting of dynamic context lands
	// with enforcement in wave 4.
	if config.Prompt != "" {
		l.observeSensitive(runCtx, config, "prompt", 0, []string{config.Prompt})
	}
	l.observeSensitive(runCtx, config, "dynamic_context", 0, sortedContextValues(dynamicContext))

	systemPrompt, err := l.Prompt.Build(runCtx, prompt.PromptContext{
		Mode:           config.Mode,
		Workspace:      config.Executor.Workspace,
		MaxTurns:       config.MaxTurns,
		DynamicContext: dynamicContext,
	})
	if err != nil {
		return l.finishWithError(runCtx, fmt.Errorf("build system prompt: %w", err))
	}

	// Forward the built system prompt to emitters that can record it
	// (the otel emitter's opt-in gen_ai.system_instructions capture).
	// The optional-capability assertion mirrors the *trace.OTelTraceEmitter
	// assertions above and below: emitters without content capture do
	// not implement the interface and the call disappears. The emitter
	// owns the scrub-and-gate logic, so this is unconditional.
	if recorder, ok := l.Trace.(trace.SystemInstructionsRecorder); ok {
		recorder.RecordSystemInstructions(systemPrompt)
	}

	// Pre-run lifecycle hooks (issue #461): operator-authored setup
	// (clone, provision a runtime) that must complete before
	// Git.Setup — the deterministic git strategy assumes an existing
	// checkout, so a clone hook has to create it first. Runs under
	// runCtx: it counts against the run's wall-clock timeout and
	// heartbeats are already flowing (started above). A nil Hooks (no
	// HooksConfig, or a hand-assembled loop that left it unset) is a
	// no-op, mirroring GuardRail/RuleOfTwo.
	if l.Hooks != nil {
		_, preHookSpan := l.Tracer.Start(l.traceCtx(runCtx), "hooks.preRun")
		preResults, preErr := l.Hooks.RunPre(runCtx)
		l.recordHookExecutions(preResults)
		if preErr != nil {
			preHookSpan.RecordError(preErr)
			preHookSpan.SetStatus(codes.Error, preErr.Error())
		}
		preHookSpan.End()
		if preErr != nil {
			// A dead run ctx (timeout/cancel) wins over setup_failed:
			// the hook almost certainly failed *because* the deadline
			// hit or a control-plane cancel arrived mid-exec, and that
			// is the more useful outcome to report.
			outcome := "setup_failed"
			if runCtx.Err() != nil {
				outcome = classifyCtxOutcome(context.Cause(runCtx))
			}
			return l.finishWithOutcome(runCtx, outcome, fmt.Errorf("pre-run hooks: %w", preErr))
		}
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
		l.metricAttrs(attribute.String("run.mode", config.Mode)),
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

	// Outer verification loop.
	outcome := "success"
	verificationAttempts := 0
	// finalAssistantText accumulates the last non-empty assistant text across
	// every runInnerLoop invocation (verification retries re-enter the loop).
	var finalAssistantText string

	for verificationAttempts <= maxVerificationRetries {
		// Run the inner agentic loop.
		var innerOutcome, innerFinalText string
		messages, innerOutcome, innerFinalText = l.runInnerLoop(runCtx, config, systemPrompt, messages, tokenTracker)
		if innerFinalText != "" {
			finalAssistantText = innerFinalText
		}

		if innerOutcome != "success" {
			outcome = innerOutcome
			break
		}

		// Run verifier.
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
			Role:      "user",
			Synthetic: true,
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

	// Post-run lifecycle hooks (issue #461): artifact submission, smoke
	// tests. Runs after outcome classification and Git.Finalise, before
	// the "done" event, on a ctx detached from the run's wall-clock
	// timeout/cancellation (context.WithoutCancel) so a hook that
	// outlives the deadline (e.g. uploading a large artifact) can still
	// finish; bounded by the sum of the configured postRun timeouts
	// plus a 30s margin so a misbehaving hook cannot hang the process
	// forever. Skipped entirely when the pre-run phase already failed —
	// Run() returned above in that case, so this point is never
	// reached. A fatal post-hook failure overrides outcome to
	// "hook_failed" only when outcome was "success": the primary
	// failure cause must never be masked, and it stays visible via
	// HookResults / RunResult.HookFailures regardless.
	//
	// context.WithoutCancel(ctx) detaches postCtx from the run's own
	// wall-clock deadline / control-plane cancel ONLY — those are the
	// signals a postRun hook is meant to survive. A genuine process
	// shutdown (SIGTERM/SIGINT/pod deletion) must still cut it short:
	// unconditionally surviving up to the full budget while the
	// orchestrator's SIGKILL escalation counts down (10s on Cloud Run
	// and `docker stop`, see docs/cloud-run-jobs.md) orphans the
	// sandbox and drops the trace. l.Shutdown carries that distinct
	// signal in from the cmd layer; racing postCancel against it keeps
	// the loop itself free of any signal/env read (CLAUDE.md
	// invariant) while still terminating promptly. Nil-safe: a
	// hand-assembled loop (tests, embedders) with no Shutdown set
	// keeps the budget-only behaviour.
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
		_, postHookSpan := l.Tracer.Start(l.traceCtx(ctx), "hooks.postRun")
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

	outcome = l.finalizeCommandOutput(ctx, outcome)

	l.Logger.Info("run finished", "outcome", outcome)

	l.Metrics.RunDuration.Record(ctx, float64(time.Since(runStart).Milliseconds()),
		l.metricAttrs(
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

	// Scrub the run's last non-empty assistant text before it reaches any
	// external surface. The PhasePostTurn guard that cleared this text is a
	// sensitivity classifier, not a deterministic secret redactor: content
	// that clears the guard can still carry a secret-shaped substring (e.g. a
	// credential echoed back from a tool result). This is the single scrub
	// site for the field — downstream of the guard gate — so both the
	// returned RunResult and the persisted trace carry the scrubbed form.
	// Mirrors the scrubbedErr treatment at the provider/guard call sites.
	finalAssistantText = security.Scrub(finalAssistantText)

	// Hand the scrubbed, guard-approved text to the emitter BEFORE Finish so
	// the persisted trace (JSONL run_finished line, GCS trace object) carries
	// it: the concrete emitters build and serialise the RunTrace inside
	// Finish, so a post-Finish assignment on the returned struct would never
	// reach disk. Same optional-capability pattern as RecordSystemInstructions.
	if recorder, ok := l.Trace.(trace.FinalAssistantTextRecorder); ok {
		recorder.RecordFinalAssistantText(finalAssistantText)
	}

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

	// Tool-choice escalation state (#230). priorToolCalls tracks whether
	// the model has dispatched any tool yet this inner-loop run;
	// escalationsSoFar bounds the missed-tool recovery; pendingToolChoice
	// carries a forced tool-choice mode onto the next turn's Stream call
	// (set by the native escalation path, consumed once). All three stay
	// at their zero values — and the escalation path is never taken — when
	// the loop's EscalationPolicy is nil (the OFF-by-default case).
	priorToolCalls := 0
	escalationsSoFar := 0
	pendingToolChoice := types.ToolChoiceAuto

	for turn := 0; turn < config.MaxTurns; turn++ {
		l.Logger.Info("turn started", "turn", turn)

		// Check budget before each turn.
		budgetCheck := tokenTracker.CheckBudget(config.MaxTokenBudget)
		if !budgetCheck.WithinBudget {
			return messages, "budget_exceeded", finalAssistantText
		}

		// Check context cancellation. Return a sentinel outcome so the
		// outer Run loop can distinguish control-plane cancellation,
		// deadline expiry, and caller-initiated cancellation via
		// context.Cause().
		select {
		case <-ctx.Done():
			return messages, outcomeCtxDone, finalAssistantText
		default:
		}

		// PhasePreTurn guard. Classifies untrusted content that just
		// entered the message history. On turn 0 the chunks include the
		// initial user prompt and DynamicContext entries; on turn N>0
		// the chunks are the contents of every tool_result block in the
		// last user message. The chunks are concatenated under a "--- chunk i ---"
		// envelope so the adapter sees a single batched request.
		var preTurnDynamic map[string]types.DynamicContextValue
		if turn == 0 {
			preTurnDynamic = config.DynamicContext
		}
		if chunks := collectUntrustedChunks(messages, turn, preTurnDynamic, config.Prompt); len(chunks) > 0 {
			// Rule-of-Two scan of the just-arrived tool results,
			// deterministic-first (before the guard so a later guard-deny
			// scrub cannot un-trip the latch). Turn-0 chunks are the
			// prompt and dynamic context, already observed in Run()
			// under their own source labels — rescanning them here would
			// double-emit warn-tier detections mislabelled "tool_result".
			if turn > 0 {
				l.observeSensitive(ctx, config, "tool_result", turn, chunks)
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
				// On turn 0 the user prompt itself is the untrusted
				// content; replaceUntrustedChunks cannot rewrite the
				// initial prompt (it has not been appended to the
				// message history at this point). The only correct
				// action is to abort the run before the model sees
				// the offending input.
				if turn == 0 {
					return messages, "guardrail_blocked", finalAssistantText
				}
				// On later turns PreTurn deny scrubs the untrusted
				// content rather than aborting: the just-arrived
				// tool_result blocks are rewritten so the run continues
				// and the model can refuse, while operators still see
				// the deny event.
				replaceUntrustedChunks(messages, turn, "[content blocked by guardrail]")
			case spotlight:
				if turn == 0 {
					// Turn 0 has no tool_result blocks to rewrap —
					// spotlightUntrustedChunks is a no-op. Skip the
					// spotlight metric/event so dashboards reflect
					// applied (not merely requested) spotlights.
					break
				}
				spotlightUntrustedChunks(messages, turn)
				l.recordSpotlightApplied(ctx, guard.PhasePreTurn, decision)
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
		responseReserve := effectiveReserveForResponse(maxTokens)
		if turn == 0 && responseReserve < defaultReserveForResponse {
			// One warning per run (turn 0 only): maxTokens is constant
			// across turns, so re-emitting this every turn would just be
			// noise. See effectiveReserveForResponse for why the reserve
			// is scaled down instead of left at the flat default.
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

		// Stream model response.
		turnStart := time.Now()
		selectedProvider := l.Provider
		if selection.Provider != "" && len(l.Providers) > 0 {
			prov, ok := l.Providers[selection.Provider]
			if !ok {
				// Pre-resolution: no concrete provider selected yet, so
				// Mode is honestly unknown. Empty string is the wire
				// encoding the TurnTrace.Mode godoc reserves for this
				// case; downstream consumers (lakehouse, mine-failures)
				// already treat empty as streaming for legacy traces
				// and route this turn through the same fallback.
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
			// See comment above: pre-resolution Mode is honestly empty.
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

		// Resolve sampling temperature. Forward an explicit override
		// verbatim (including 0.0 for greedy decoding); fall back to
		// the harness default when the config left it nil. The
		// invariant — loop must never silently forward a nil
		// temperature to providers that would otherwise fall through
		// to their own (higher) service defaults — is preserved by
		// the fallback branch.
		temperature := config.Temperature
		if temperature == nil {
			temperature = types.Float64Ptr(defaultTemperature)
		}
		// Consume any forced tool choice the escalation path set on the
		// previous iteration. Reset to auto immediately so the override
		// applies to exactly one turn — a single bounded nudge, not a
		// sticky mode. The zero value (ToolChoiceAuto) leaves the request
		// byte-for-byte unchanged, so a run that never escalates is
		// unaffected.
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
			// Co-emit into the tool-failure series when the failed
			// request carried tool definitions: from the model's
			// perspective the harness asked it to use tools and the
			// provider refused. A pure text-only request error is a
			// provider failure but not a tool-use failure.
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
			// If the provider call failed because the run context was
			// cancelled, surface that so the outer loop can classify the
			// outcome as cancelled/timeout rather than a generic error.
			if ctx.Err() != nil {
				return messages, outcomeCtxDone, finalAssistantText
			}
			return messages, "error", finalAssistantText
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
			// Co-emit into the tool-failure series when this turn had
			// tools attached: mid-stream parser/disconnect failures
			// abort tool-call assembly. Distinguished from
			// provider_request_failed by the category — a failure
			// after the stream opened is a stream-side fault, not a
			// rejected request.
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
			// Distinguish stream-abort-due-to-ctx from other stream errors
			// so the outer loop can classify the outcome correctly.
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

		// Track token usage. Output tokens come from the stream; input tokens
		// are estimated from the messages sent plus system prompt and tools.
		inputTokenEstimate := estimateCurrentTokens(preparedMessages) +
			estimateSystemPromptTokens(systemPrompt) +
			estimateToolDefinitionTokens(toolDefs)
		tokenTracker.RecordTurn(inputTokenEstimate, sr.OutputTokens)

		// Record turn in trace.
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

		// Snapshot the model input the provider just saw and the
		// content blocks it produced. The full transcript is captured
		// as a TurnRecord via RecordTurnRecord; recording emitters
		// (streaming JSONLTraceEmitter) persist it for downstream
		// replay / mine-failures, while summary-only emitters
		// (OTel, GCS) ignore it.
		//
		// modelInput.Messages is the exact prepared-message slice the
		// provider received this turn (pre-tool-result append).
		// ModelOutput carries the assistant's content blocks. Tool
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

		// Append assistant message, carrying any provider replay state
		// from the stream so the next request can round-trip it.
		messages = appendAssistantContent(messages, sr.Blocks, sr.ReplayFields)

		// Capture the assistant's text for this turn. The last non-empty
		// value across all turns becomes RunTrace.FinalAssistantText; the
		// end_turn path below reuses finalText for its PhasePostTurn guard.
		//
		// priorFinalText snapshots the accumulator BEFORE this turn's
		// commit. The PhasePostTurn guard on the end_turn path below runs
		// AFTER the commit, so on a guard deny the run must return this
		// prior value — never the just-committed, denied text. Returning
		// the denied text would forward it through RunTrace/RunResult and
		// out the resultSink, bypassing the guard's explicit "do not
		// forward this content" decision. This is the one new forwarding
		// channel the FinalAssistantText field adds, so the guard contract
		// must hold on it.
		finalText := lastAssistantText(sr.Blocks)
		priorFinalText := finalAssistantText
		if finalText != "" {
			finalAssistantText = finalText
		}

		// Extract tool calls.
		toolCalls := collectToolCalls(sr.Blocks)

		// Tool-choice escalation (#230). When the model returns a
		// final/text answer (no tool calls) on a turn where the harness
		// expected tool use, the injected EscalationPolicy decides whether
		// this is a likely missed-tool failure and how to recover. The
		// loop itself makes no judgement — it forwards the turn facts and
		// acts on the decision. A nil policy (OFF by default) makes
		// escalationDecision a no-op, so this block is inert on a bare run.
		// The check runs before the terminal end_turn / non-tool-use
		// returns so a recovered turn continues the loop instead of being
		// accepted as the final answer.
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
			// Record the turn transcript with no tool calls before the
			// success return. Even an end_turn carries a transcript
			// worth preserving: replay needs the model's final answer,
			// and mine-failures distinguishes "model declared end_turn
			// at turn N" from "loop ran out of turns at N".
			l.Trace.RecordTurnRecord(turnRecord)
			// PhasePostTurn guard: classify the assistant's final text
			// before forwarding it. A deny terminates the run with the
			// "guardrail_blocked" outcome. Spotlight is opt-in for
			// future sub-agent contexts where the parent loop can safely
			// rewrap the child's output; for v1 we log the request and
			// forward the response unchanged because rewriting the
			// user-visible text would break tool-protocol expectations.
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
					// Return the prior (non-denied) accumulated text, not
					// this turn's just-committed text — see priorFinalText.
					return messages, "guardrail_blocked", priorFinalText
				}
				if spotlight {
					// PostTurn spotlight is intentionally a log-only
					// no-op in v1: rewriting the user-visible
					// assistant text would break tool-protocol
					// expectations. The spotlight metric / event are
					// NOT emitted for unapplied PostTurn spotlights —
					// see recordSpotlightApplied.
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

		// Dispatch tool calls. Sync calls run inline in assistant-message
		// order; async calls (those with an AsyncHandler, e.g. deep-research
		// spawn_agent invocations) fan out under a semaphore sized to
		// config.EffectiveToolDispatchMaxParallel(). planAndDispatch preserves
		// result order, stall-detector ordering, per-call timeouts, and ctx
		// cancellation propagation; see harness/internal/core/dispatch.go.
		// The router's provider/model selection is forwarded so per-call
		// failure metrics (stirrup.harness.tool_failures) can be attributed
		// back to the model that emitted the offending tool_use block.
		dispatchCtx := commandoutput.WithCallContext(ctx, commandoutput.CallContext{
			RunID: config.RunID, ParentRunID: l.ParentRunID, Turn: turn + 1,
		})
		toolResults, toolRecords, stallOutcome := l.planAndDispatch(dispatchCtx, config, toolCalls, stall, selection.Provider, selection.Model)
		turnRecord.ToolCalls = toolRecords
		l.Trace.RecordTurnRecord(turnRecord)
		messages = appendToolResults(messages, toolResults)
		// Record that the model has now used tools this run. The
		// escalation trigger (#230) fires only on the first assistant
		// turn with no prior tool calls; once this is non-zero a later
		// no-tool answer is a legitimate judgement and is left alone.
		priorToolCalls += len(toolCalls)
		if stallOutcome != "" {
			return messages, stallOutcome, finalAssistantText
		}

		// Re-check budget after tool results are appended. This prevents the
		// next turn from sending an over-budget context to the provider.
		budgetCheck = tokenTracker.CheckBudget(config.MaxTokenBudget)
		if !budgetCheck.WithinBudget {
			return messages, "budget_exceeded", finalAssistantText
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
	return messages, "max_turns", finalAssistantText
}

// applyEscalation performs the recovery the EscalationPolicy chose for a
// suspected missed-tool turn (#230) and records its observability. It
// returns the (possibly extended) message history.
//
//   - EscalationNative sets *pendingToolChoice to ToolChoiceRequired so
//     the next turn's Stream call forces a tool. The provider adapter only
//     honours this when the resolved capability supports it; the policy has
//     already confirmed support, but the prompt path is the safe fallback
//     either way.
//   - EscalationPrompt appends a user message stating the unmet
//     requirement so the model is nudged to call a tool on the next turn.
//
// Both paths emit a stirrup.harness.tool_failures observation under the
// no_tool_when_required category (bounded labels only — no model strings)
// and an escalation OTel span tagged with the run mode, the chosen kind,
// and the policy's reason, so operators can audit why a retry happened.
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

	// Co-emit into the tool-failure series so dashboards keyed on
	// stirrup.harness.tool_failures see missed-tool recovery alongside the
	// dispatch-site categories. tool.name is the empty bounded sentinel —
	// no tool was involved — matching the provider_request/stream paths.
	// Gate on IsValid() like every dispatch.go emit site so a future
	// dynamic category can never widen label cardinality past the enum.
	if observability.ToolFailureNoToolWhenRequired.IsValid() {
		l.Metrics.ToolFailures.Add(ctx, 1, l.metricAttrs(
			attribute.String("tool.name", observability.ToolNameProviderScope),
			attribute.String("category", observability.ToolFailureNoToolWhenRequired.String()),
			attribute.String("provider.type", providerType),
			attribute.String("provider.model", model),
			attribute.String("run.mode", config.Mode),
		))
	}

	// Scrub the policy reason before it lands on the OTel span and the
	// slog line. The only built-in policy builds Reason from the validated
	// mode enum (no secret can reach it), but EscalationPolicy is a public
	// interface: a future policy interpolating in.StopReason or workspace
	// content would otherwise leak past ScrubHandler, which covers neither
	// span attributes nor this log path. Mirrors the scrubbedErr pattern at
	// the provider.Stream call sites.
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
			// Non-blocking pre-check biases the select toward cancellation:
			// if ctx is already done, exit before racing the ticker. Narrows
			// the common post-cancel-tick window described in issue #128.
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
	// Span parent: when the caller's ctx already carries an active span
	// (PhasePreTool — tool.<name> via toolSpanCtx) use it directly so the
	// guard span nests under the dispatch path (#55, B3). For PreTurn /
	// PostTurn the caller's ctx carries no span, so fall back to the
	// loop's run-root TraceContext to preserve existing trace shape.
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
			// Spotlight events and the stirrup.guard.spotlights metric
			// are emitted by the call site only after the spotlight is
			// actually applied (recordSpotlightApplied). guardCheck
			// returns spotlight=true to signal the request; whether
			// the caller acts on it depends on the phase. Emitting
			// here would over-count: PostTurn currently logs and
			// forwards the response unchanged.
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
// untrusted chunks and emits the observe-only telemetry: the
// sensitive_scan_ms histogram on every scan, sensitive_data_detected +
// rule_of_two_detections on any finding, and the once-per-run
// rule_of_two_triggered + transport warning at the latch transition.
// Nothing here changes run behaviour — wave 3 ships dark; enforcement
// consumers arrive in wave 4. A nil monitor (hand-assembled loops)
// no-ops, mirroring guardCheck's nil-GuardRail branch.
func (l *AgenticLoop) observeSensitive(ctx context.Context, config *types.RunConfig, source string, turn int, chunks []string) {
	if l.RuleOfTwo == nil || len(chunks) == 0 {
		return
	}
	start := time.Now()
	det := l.RuleOfTwo.ObserveChunks(ctx, source, turn, chunks)
	if l.Metrics != nil {
		// Fractional milliseconds: scans are routinely sub-millisecond
		// and this histogram exists to keep regex cost observable —
		// integer truncation would flatten the series to zero.
		elapsedMs := float64(time.Since(start)) / float64(time.Millisecond)
		l.Metrics.SensitiveScan.Record(ctx, elapsedMs, l.metricAttrs(
			attribute.String("source", source),
		))
	}
	if len(det.Patterns) == 0 {
		return
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
}

// ratchetRuleOfTwo forwards a guard decision's criterion to the
// Rule-of-Two monitor's one-way ratchet. Every non-nil decision is
// forwarded — the monitor filters against its configured guard-criteria
// set internally, keeping the loop free of criteria logic. Telemetry
// fires only on the false→true transition: the guard's own deny/allow
// events already record the decision itself.
func (l *AgenticLoop) ratchetRuleOfTwo(ctx context.Context, config *types.RunConfig, decision *guard.Decision, turn int) {
	if l.RuleOfTwo == nil || decision == nil || decision.Criterion == "" {
		return
	}
	if !l.RuleOfTwo.TripFromGuard(decision.GuardID, decision.Criterion) {
		return
	}
	source := "guard:" + decision.GuardID
	action := l.RuleOfTwo.Action()
	// The criterion is namespaced "guard:<criterion>" in the patterns
	// field and the pattern metric label so guard-originated trips can
	// never impersonate deterministic detector names: a coerced guard
	// returning criterion "secret/aws_access_key_id" must not make
	// telemetry (or alerting rules keyed on pattern names) read as if
	// the detector fired.
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

// emitRuleOfTwoTriggered records the once-per-run latch transition: the
// rule_of_two_triggered security event (key names mirror the run-start
// audit events from emitRuleOfTwoEvents) and a one-time transport
// warning so operators without a security-event pipeline still see the
// posture change on the wire.
func (l *AgenticLoop) emitRuleOfTwoTriggered(config *types.RunConfig, source, action string) {
	untrusted, _, external := types.RuleOfTwoState(config)
	if l.Security != nil {
		l.Security.RuleOfTwoTriggered(untrusted, external, action, source)
	}
	_ = l.Transport.Emit(types.HarnessEvent{
		Type:    "warning",
		Message: fmt.Sprintf("rule of two: sensitive data detected (source %q); action %q is observe-only this release", source, action),
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
// just entered the message history at the start of the given turn. On
// turn 0 this includes the initial user prompt and any DynamicContext
// entries (sorted by key for determinism). On subsequent turns it
// returns the Content field of every tool_result block in the last
// message — those entries arrived from external tool execution and
// have not yet been classified — plus the Structured payload (issue
// #231) when present: the Anthropic adapter forwards it to the model
// as a second text block and the Gemini adapter embeds it under
// functionResponse.response.structured, so a credential present only
// in the structured JSON is model-visible and must be classified too.
//
// v1 keeps this conservative: we do not attempt to classify earlier
// turns' content (already in history), nor model-emitted text (handled
// at PhasePostTurn). Only freshly arrived untrusted material is sent
// to the pre-turn guard, batched into a single classification call.
func collectUntrustedChunks(messages []types.Message, turn int, dynamicContext map[string]types.DynamicContextValue, prompt string) []string {
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
	// Synthetic messages are harness-controlled content (escalation prompts,
	// verifier feedback); they are never untrusted external input and do not
	// need pre-turn classification.
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

// replaceUntrustedChunks replaces the content of every tool_result
// block in the last message with the supplied placeholder. Used when
// PhasePreTurn returns VerdictDeny to drop the untrusted content from
// this turn rather than feed it to the model. Turn 0 is a no-op
// because the user prompt itself is the untrusted content and is
// not yet appended to the message history; turn 0 PreTurn denies
// must be handled by the caller (the loop aborts the run with
// outcome "guardrail_blocked").
func replaceUntrustedChunks(messages []types.Message, turn int, placeholder string) {
	if turn == 0 {
		// Turn 0 has no tool_result blocks to rewrite. Callers must
		// abort the run rather than calling into this helper, so this
		// branch is a defensive no-op only.
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

// recordHookExecutions forwards each lifecycle hook result (issue #461)
// to the trace emitter's optional HookRecorder capability and surfaces a
// transport "warning" event for any hook that failed but was configured
// with continueOnError: true — visible to the control plane even though
// the failure never touches the run's outcome. Mirrors the
// git.Finalise-failure -> "warning" event precedent elsewhere in Run().
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

// postHookBudget returns the wall-clock budget granted to the detached
// post-hook ctx (issue #461): the sum of every postRun hook's effective
// timeout (the worst case where every hook actually runs to its own
// timeout) plus a 30s margin, so artifact submission and smoke tests can
// still complete even after the run's own wall-clock timeout has
// expired. A nil or hookless config resolves to just the 30s margin so
// the detached ctx is never unbounded. ValidateRunConfig bounds the sum
// to types.MaxHookTimeoutSeconds, so this is bounded regardless of
// operator input.
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

// finishWithOutcome is finishWithError's generalisation (issue #461): it
// records the given outcome — rather than always "error" — and finishes
// the trace. Used by the preRun hook fatal-failure path, which reports
// "setup_failed" (or a ctx-cause outcome if the run's own wall-clock
// timeout/cancel raced the hook failure) instead of the generic "error".
func (l *AgenticLoop) finishWithOutcome(ctx context.Context, outcome string, err error) (*types.RunTrace, error) {
	outcome = l.finalizeCommandOutput(ctx, outcome)
	if emitErr := l.Transport.Emit(types.HarnessEvent{
		Type:    "error",
		Message: err.Error(),
	}); emitErr != nil {
		l.Logger.Warn("transport emit failed", "event", "error", "error", emitErr)
	}
	// Emit "done" with StopReason=outcome, matching the shape of the
	// normal end-of-Run() emission (loop.go's Run(), immediately before
	// stopHeartbeat) — this early-return path previously emitted only
	// "error", so a control plane never saw the terminal "done" event
	// documented as the definitive end-of-stream signal for a run this
	// path terminates, and the CLI entrypoints' `if err != nil { return
	// }` shortcut (see cmd/harness.go, cmd/job.go) meant an operator's
	// preRun hook failure — outcome "setup_failed" — produced no
	// structured RunResult at all despite a valid RunTrace existing.
	// See finishWithOutcome's own doc comment and the cmd-layer fix
	// this pairs with.
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

func (l *AgenticLoop) finalizeCommandOutput(ctx context.Context, outcome string) string {
	if l.CommandOutput == nil || !l.OwnsCommandOutput {
		return outcome
	}
	captureFailed := l.CommandOutput.FatalError() != nil
	finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 6*time.Minute)
	defer cancel()
	archive, err := l.CommandOutput.Finalize(finalizeCtx)
	if archive != "" {
		if recorder, ok := l.Trace.(trace.CommandOutputArchiveRecorder); ok {
			recorder.RecordCommandOutputArchive(archive)
		}
	}
	if err != nil {
		l.Logger.Error("command output archive finalization failed", "error", err, "archive", archive)
	}
	// Like hook_failed, capture and archive failures only claim the outcome
	// of an otherwise-successful run — the primary failure cause stays
	// authoritative. Capture failure outranks archive failure: the former
	// lost command output, the latter only its durable copy.
	if outcome != "success" {
		return outcome
	}
	if captureFailed {
		return "command_output_capture_failed"
	}
	if err != nil {
		return "trace_archive_failed"
	}
	return outcome
}
