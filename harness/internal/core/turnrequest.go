package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// overflowCompactionPercent is the share of an overflowing request's
// estimated size that the forced compaction before the retry targets.
const overflowCompactionPercent = 70

// forcedCompaction returns the budget for retrying a turn whose request,
// estimated at rejected tokens, exceeded the model's context window: the
// strategy compacts to overflowCompactionPercent of that estimate.
func (b turnBudget) forcedCompaction(rejected int) turnBudget {
	b.prepare.MaxTokens = b.prepare.ReserveForResponse + rejected*overflowCompactionPercent/100
	b.window.source = fmt.Sprintf("the forced compaction target (%d%% of the %d-token request that exceeded the model's context window)",
		overflowCompactionPercent, rejected)
	b.window.remedy = "check that contextStrategy.maxTokens is no larger than the model's context window"
	return b
}

// prepareRequest runs the context strategy and the fit postcondition for
// one provider request. forcedFrom is the estimate of the request that
// exceeded the context window when this is a forced compaction, else 0.
// It returns the messages to send and their estimate, or a non-empty
// outcome that ends the run.
func (l *AgenticLoop) prepareRequest(
	ctx context.Context,
	messages []types.Message,
	systemPrompt string,
	toolDefs []types.ToolDefinition,
	budget turnBudget,
	forcedFrom int,
) ([]types.Message, int, string) {
	attrs := []attribute.KeyValue{
		attribute.Int("messages.before", len(messages)),
		attribute.Int("tokens.before", budget.prepare.CurrentTokens),
		attribute.Float64("context.token_ratio", budget.ratio),
	}
	if forcedFrom > 0 {
		attrs = append(attrs, attribute.Int("context.forced_from", forcedFrom))
	}
	_, contextSpan := l.Tracer.Start(l.traceCtx(ctx), "context.prepare", oteltrace.WithAttributes(attrs...))
	defer contextSpan.End()

	preparedMessages, err := l.Context.Prepare(ctx, messages, budget.prepare)
	if err != nil {
		scrubbed := recordSpanError(contextSpan, err)
		if ctx.Err() != nil {
			return nil, 0, outcomeCtxDone
		}
		l.noteFailure("error", "context strategy: "+scrubbed)
		return nil, 0, "error"
	}
	compaction := l.Context.LastCompaction()
	if compaction != nil {
		l.historyRewritten = true
		for _, warning := range compaction.Warnings {
			l.Logger.WarnContext(ctx, "context strategy degraded", "strategy", compaction.Strategy, "warning", warning)
			_ = l.Transport.Emit(types.HarnessEvent{Type: "warning", Message: warning})
		}
	}
	if l.historyRewritten {
		// A thinking block is bound to the exact history before it, so
		// none is replayed once the run has sent a rewritten history.
		preparedMessages = types.StripThinkingBlocks(preparedMessages)
	}
	preparedMessages, tokens, fitErr := l.fitPreparedRequest(ctx, preparedMessages, systemPrompt, toolDefs, budget, contextSpan)
	// Feeds the ContextTokens observable gauge registered in Run;
	// compaction shrinks this value, new messages grow it.
	l.lastContextTokens.Store(int64(tokens))
	contextSpan.SetAttributes(attribute.Int("messages.after", len(preparedMessages)))
	if fitErr != nil {
		recordSpanError(contextSpan, fitErr)
		l.reportContextExhausted(ctx, fitErr)
		return nil, 0, "context_exhausted"
	}
	if compaction != nil {
		contextSpan.SetAttributes(
			attribute.String("context.strategy", compaction.Strategy),
			attribute.Int("context.tokens.after", compaction.TokensAfter),
		)
		logAttrs := []any{
			"strategy", compaction.Strategy,
			"messages.before", compaction.MessagesBefore,
			"messages.after", compaction.MessagesAfter,
			"tokens.before", compaction.TokensBefore,
			"tokens.after", compaction.TokensAfter,
		}
		if compaction.ToolResultsCleared > 0 {
			contextSpan.SetAttributes(attribute.Int("context.tool_results.cleared", compaction.ToolResultsCleared))
			logAttrs = append(logAttrs, "toolResults.cleared", compaction.ToolResultsCleared)
		}
		l.Metrics.ContextCompactions.Add(ctx, 1,
			l.metricAttrs(attribute.String("context.strategy", compaction.Strategy)),
		)
		l.Logger.InfoContext(ctx, "context compacted", logAttrs...)
	}
	return preparedMessages, tokens, ""
}

// reportContextExhausted logs and emits the error that ends a run as
// context_exhausted.
func (l *AgenticLoop) reportContextExhausted(ctx context.Context, err error) {
	l.Logger.ErrorContext(ctx, "context budget exhausted", "error", err)
	_ = l.Transport.Emit(types.HarnessEvent{Type: "warning", Message: err.Error()})
}

// reportOverflowRetry logs and emits that a turn exceeded the context
// window and is retried after a forced compaction.
func (l *AgenticLoop) reportOverflowRetry(ctx context.Context, turn int, selection router.ModelSelection, attempt providerAttempt, rejected int) {
	cause := "the provider rejected the request"
	if attempt.err == nil {
		cause = "the response filled the window, and its streamed output is discarded"
	}
	target := rejected * overflowCompactionPercent / 100
	l.Logger.WarnContext(ctx, "context window exceeded; compacting and retrying the turn",
		"turn", turn,
		"provider", selection.Provider,
		"model", selection.Model,
		"tokens.estimated", rejected,
		"tokens.target", target,
	)
	_ = l.Transport.Emit(types.HarnessEvent{
		Type: "warning",
		Message: fmt.Sprintf("provider %s (%s) exceeded the model's context window on turn %d (%s); compacting the request from about %d to at most %d estimated tokens and retrying the turn once",
			selection.Provider, selection.Model, turn, cause, rejected, target),
	})
}

// overflowExhaustedError describes a turn whose retry after a forced
// compaction exceeded the context window again.
func overflowExhaustedError(rejected, retried int) error {
	return fmt.Errorf(
		"context window exceeded: the provider reported the request as too large for the model's context window again after it was compacted from about %d to %d estimated tokens; "+
			"set contextStrategy.maxTokens to the model's context window or shorten the system prompt, the tool set or the task",
		rejected, retried,
	)
}

// providerAttempt is the outcome of one provider request within a turn.
type providerAttempt struct {
	// result is nil when err is set.
	result *streamResult
	// tokens is the turn's usage, including usage discarded by retried
	// attempts; Input falls back to the request estimate when the
	// provider reported none.
	tokens        types.TokenUsage
	inputReported bool
	duration      time.Duration
	// retries is the number of times the turn was re-issued after a
	// retryable provider failure.
	retries int
	err     error
	// errText is err scrubbed of secrets, safe for the run's failure text.
	errText string
	// errorType is the bounded error.type for a failed request.
	errorType string
	// summary is the "error" turn summary a failed request recorded; nil
	// when none was recorded.
	summary *types.TurnTrace
	// toolsLoaded lists the deferred tools the response's native tool
	// searches loaded, by internal ID.
	toolsLoaded []string
}

// requestErrorType classifies a failed request for error.type: a run
// cancellation or deadline takes precedence over the failure category.
func requestErrorType(ctx context.Context, category observability.ToolFailureCategory) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "timeout"
	case ctx.Err() != nil:
		return "cancelled"
	}
	return category.String()
}

// inferenceAttrs identifies the model inference a turn's provider request
// performs, for the GenAI client metrics. The response model is filled in
// by callers once the provider reports one.
func inferenceAttrs(config *types.RunConfig, selection router.ModelSelection, selectedProvider provider.ProviderAdapter) observability.InferenceAttrs {
	host, port := provider.ServerEndpointOf(selectedProvider)
	return observability.InferenceAttrs{
		ProviderType:  providerTypeFor(config, selection.Provider),
		RequestModel:  selection.Model,
		ServerAddress: host,
		ServerPort:    port,
	}
}

// overflowed reports whether the request exceeded the model's context
// window: rejected as too large, or stopped because the response filled
// the window.
func (a providerAttempt) overflowed() bool {
	if a.err != nil {
		return types.IsContextOverflow(a.err)
	}
	return a.result.StopReason == types.StopReasonContextWindowExceeded
}

// requestProvider sends one request and consumes its stream, re-issuing
// it after a retryable provider failure within the turn retry policy.
// Tools run only after a stream completes and a failed attempt appends
// nothing to history, so a re-issued request cannot repeat a workspace
// side effect. A failed request is logged, emitted, counted and recorded
// as an "error" turn before it returns, unless it is a context overflow
// and retryOverflow says the caller retries it; a successful one is
// returned for the caller to record. requestTokens is the request's
// estimate. Usage reported by failed attempts is charged to tokenTracker and ledger.
// ctx carries the request's chat span, which each attempt's
// provider.stream span nests under.
func (l *AgenticLoop) requestProvider(
	ctx context.Context,
	config *types.RunConfig,
	tokenTracker *TokenTracker,
	ledger *TokenLedger,
	turn int,
	selection router.ModelSelection,
	selectedProvider provider.ProviderAdapter,
	providerAttrs metric.MeasurementOption,
	params types.StreamParams,
	requestTokens int,
	retryOverflow bool,
	windDown string,
) providerAttempt {
	start := time.Now()
	// A batch turn is not retried: a retry would submit and wait out a
	// fresh batch.
	retrier := &turnRetrier{}
	if _, isBatch := selectedProvider.(batchModeAdapter); !isBatch {
		retrier.policy = turnRetryPolicyFor(config, selection.Provider)
	}
	var discarded types.TokenUsage
	inference := inferenceAttrs(config, selection, selectedProvider)

	for attemptNo := 1; ; attemptNo++ {
		attemptStart := time.Now()
		l.Metrics.ProviderRequests.Add(ctx, 1, providerAttrs)
		spanCtx, providerSpan := l.Tracer.Start(ctx, "provider.stream",
			oteltrace.WithAttributes(
				attribute.String("provider.type", selection.Provider),
				attribute.String("provider.model", selection.Model),
				attribute.Int("turn.number", turn),
				attribute.Int("turn.attempt", attemptNo),
			),
		)

		// category distinguishes a rejected request from a fault after
		// the stream opened.
		category := observability.ToolFailureProviderRequest
		var sr *streamResult
		ch, err := selectedProvider.Stream(spanCtx, params)
		if err == nil {
			category = observability.ToolFailureProviderStream
			sr, err = streamEventsToResult(ctx, ch, l.Transport, streamsThinking(config), l.logEmitFailure, l.reportStreamRedaction)
		}
		if err == nil {
			succeeded := inference
			succeeded.ResponseModel = sr.ResponseModel
			l.Metrics.RecordInferenceDuration(ctx, succeeded, time.Since(attemptStart), "")
			l.Metrics.RecordInferenceTokens(ctx, succeeded, sr.Usage.Input, sr.Usage.Output)
			return l.completeAttempt(ctx, providerSpan, sr, discarded, retrier, selection, requestTokens, start)
		}
		l.Metrics.RecordInferenceDuration(ctx, inference, time.Since(attemptStart), category.String())

		// A failed stream's result is partial; only its usage is kept.
		if sr != nil {
			discarded.Add(sr.Usage)
			l.Metrics.RecordInferenceTokens(ctx, inference, sr.Usage.Input, sr.Usage.Output)
		}
		scrubbedErr := recordSpanError(providerSpan, err)
		errorType := requestErrorType(ctx, category)
		overflow := ctx.Err() == nil && types.IsContextOverflow(err)
		if overflow {
			errorType = "context_overflow"
		}
		providerSpan.SetAttributes(attribute.String("error.type", errorType))
		providerSpan.End()
		if retryOverflow && overflow {
			tokenTracker.RecordTurn(discarded)
			ledger.Charge(discarded)
			return providerAttempt{tokens: discarded, duration: time.Since(start), retries: retrier.retries, err: err, errText: scrubbedErr, errorType: errorType}
		}
		l.Metrics.ProviderErrors.Add(ctx, 1, providerAttrs)
		// Co-emit into tool-failure metrics only when tools were
		// offered: a pure text-only request error isn't a tool-use
		// failure from the model's perspective.
		if len(params.Tools) > 0 {
			l.Metrics.ToolFailures.Add(ctx, 1, l.metricAttrs(
				attribute.String("tool.name", observability.ToolNameProviderScope),
				attribute.String("category", category.String()),
				attribute.String("provider.type", selection.Provider),
				attribute.String("provider.model", selection.Model),
				attribute.String("run.mode", config.Mode),
			))
		}

		var retryOutcome string
		switch {
		case ctx.Err() != nil:
			if retrier.engaged {
				retryOutcome = turnRetryOutcomeContextDone
			}
		case types.IsRetryable(err):
			delay, outcome, retry := retrier.next(time.Now())
			retryOutcome = outcome
			if retry {
				l.Logger.WarnContext(ctx, "provider turn retry",
					"provider", selection.Provider,
					"model", selection.Model,
					"turn", turn,
					"attempt", attemptNo,
					"delay_ms", delay.Milliseconds(),
					"error", scrubbedErr,
				)
				_ = l.Transport.Emit(types.HarnessEvent{
					Type: "warning",
					Message: fmt.Sprintf(
						"provider %s (%s): turn %d attempt %d failed with a retryable error; its streamed output is discarded and the turn is retried in %s (attempt %d of %d): %s",
						selection.Provider, selection.Model, turn, attemptNo, delay.Round(time.Millisecond), attemptNo+1, retrier.maxAttempts(), scrubbedErr,
					),
				})
				if sleepCtx(ctx, delay) {
					continue
				}
				retryOutcome = turnRetryOutcomeContextDone
			}
		default:
			if retrier.engaged {
				retryOutcome = turnRetryOutcomeNonRetryable
			}
		}
		if retryOutcome != "" {
			l.recordTurnRetryOutcome(ctx, selection.Provider, selection.Model, retryOutcome)
		}

		// Skipped when ctx is already cancelled: the cancel/timeout
		// path reports instead.
		if ctx.Err() == nil {
			l.Logger.ErrorContext(ctx, "provider stream failed",
				"provider", selection.Provider,
				"model", selection.Model,
				"error", scrubbedErr,
			)
			_ = l.Transport.Emit(types.HarnessEvent{
				Type:    "warning",
				Message: fmt.Sprintf("provider %s (%s): %s", selection.Provider, selection.Model, scrubbedErr),
			})
		}
		tokenTracker.RecordTurn(discarded)
		ledger.Charge(discarded)
		duration := time.Since(start)
		turnMode, turnBatchID := turnModeInfo(selectedProvider)
		summary := types.TurnTrace{
			Turn:       turn,
			Tokens:     discarded,
			StopReason: "error",
			DurationMs: duration.Milliseconds(),
			Mode:       turnMode,
			BatchID:    turnBatchID,
			Model:      selection.Model,
			Retries:    retrier.retries,
			WindDown:   windDown,
		}
		l.Trace.RecordTurn(summary)
		// The run's cancellation can land after the attempt failed for
		// another reason; it still decides the request's error type.
		errorType = requestErrorType(ctx, category)
		if overflow {
			errorType = "context_overflow"
		}
		return providerAttempt{
			tokens:    discarded,
			duration:  duration,
			retries:   retrier.retries,
			err:       err,
			errText:   scrubbedErr,
			errorType: errorType,
			summary:   &summary,
		}
	}
}

// completeAttempt ends a successful attempt's span and builds its
// outcome. discarded is the usage of the turn's failed attempts, charged
// with the successful one.
func (l *AgenticLoop) completeAttempt(
	ctx context.Context,
	providerSpan oteltrace.Span,
	sr *streamResult,
	discarded types.TokenUsage,
	retrier *turnRetrier,
	selection router.ModelSelection,
	requestTokens int,
	start time.Time,
) providerAttempt {
	if retrier.engaged {
		l.recordTurnRetryOutcome(ctx, selection.Provider, selection.Model, turnRetryOutcomeSucceeded)
	}

	// Input is the provider's figure when it reports one; otherwise it
	// is the estimate of the messages sent plus system prompt and tools.
	tokens := sr.Usage
	inputReported := tokens.Input > 0
	if !inputReported {
		tokens.Input = requestTokens
	}
	providerSpan.SetAttributes(
		attribute.Int("tokens.input", tokens.Input),
		attribute.Bool("tokens.input_reported", inputReported),
		attribute.Int("tokens.output", tokens.Output),
		attribute.String("stop_reason", sr.StopReason),
	)
	providerSpan.SetAttributes(usageBreakdownAttributes(tokens)...)
	if sr.StopDetails != nil {
		// Explanation is provider free text bound for the persisted
		// trace, so it gets the same scrub as final assistant text.
		// The copy leaves the provider's event untouched.
		scrubbed := *sr.StopDetails
		scrubbed.Explanation = security.Scrub(scrubbed.Explanation)
		sr.StopDetails = &scrubbed
		if sr.StopDetails.Category != "" {
			providerSpan.SetAttributes(attribute.String("stop.category", sr.StopDetails.Category))
		}
	}
	providerSpan.End()
	tokens.Add(discarded)
	return providerAttempt{
		result:        sr,
		tokens:        tokens,
		inputReported: inputReported,
		duration:      time.Since(start),
		retries:       retrier.retries,
	}
}

// recordAttemptUsage charges a completed request's tokens to the loop and
// the run ledger, records its metrics and log line, and returns its turn
// summary for the caller to record once the turn's tool calls, which can
// add to the summary, have run. toolCalls is the number of tool calls the
// turn dispatches; a discarded response passes zero.
func (l *AgenticLoop) recordAttemptUsage(
	ctx context.Context,
	config *types.RunConfig,
	tokenTracker *TokenTracker,
	ledger *TokenLedger,
	turn int,
	selection router.ModelSelection,
	selectedProvider provider.ProviderAdapter,
	attempt providerAttempt,
	toolCalls int,
	outputLimitRecovery bool,
	windDown string,
) types.TurnTrace {
	sr, tokens := attempt.result, attempt.tokens
	tokenTracker.RecordTurn(tokens)
	ledger.Charge(tokens)

	turnMode, turnBatchID := turnModeInfo(selectedProvider)
	summary := types.TurnTrace{
		Turn:                turn,
		Tokens:              tokens,
		InputReported:       attempt.inputReported,
		StopReason:          sr.StopReason,
		StopDetails:         sr.StopDetails,
		DurationMs:          attempt.duration.Milliseconds(),
		Mode:                turnMode,
		BatchID:             turnBatchID,
		Model:               selection.Model,
		ResponseModel:       sr.ResponseModel,
		Retries:             attempt.retries,
		ToolCalls:           toolCalls,
		OutputLimitRecovery: outputLimitRecovery,
		WindDown:            windDown,
		ToolsLoaded:         attempt.toolsLoaded,
	}

	modeAttr := l.metricAttrs(attribute.String("run.mode", config.Mode))
	l.Metrics.Turns.Add(ctx, 1, modeAttr)
	inference := inferenceAttrs(config, selection, selectedProvider)
	tokenAttrs := l.metricAttrs(observability.ProviderModelAttributes(inference.ProviderType, inference.RequestModel)...)
	l.Metrics.TokensInput.Add(ctx, int64(tokens.Input), tokenAttrs)
	l.Metrics.TokensOutput.Add(ctx, int64(tokens.Output), tokenAttrs)
	if tokens.CacheRead > 0 {
		l.Metrics.TokensCacheRead.Add(ctx, int64(tokens.CacheRead), tokenAttrs)
	}
	if tokens.CacheWrite > 0 {
		l.Metrics.TokensCacheWrite.Add(ctx, int64(tokens.CacheWrite), tokenAttrs)
	}
	l.Metrics.TurnDuration.Record(ctx, float64(attempt.duration.Milliseconds()), modeAttr)

	l.Logger.InfoContext(ctx, "turn completed", "turn", turn,
		"tokens.input", tokens.Input,
		"tokens.input_reported", attempt.inputReported,
		"tokens.output", tokens.Output,
		"tokens.cache_read", tokens.CacheRead,
		"tokens.cache_write", tokens.CacheWrite,
		"tokens.reasoning", tokens.Reasoning,
		"stopReason", sr.StopReason)
	return summary
}
