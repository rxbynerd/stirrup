package core

import (
	"context"
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
			l.Logger.Warn("context strategy degraded", "strategy", compaction.Strategy, "warning", warning)
			_ = l.Transport.Emit(types.HarnessEvent{Type: "warning", Message: warning})
		}
	}
	if l.historyRewritten {
		// A thinking block is bound to the exact history before it, so
		// none is replayed once the run has sent a rewritten history.
		preparedMessages = types.StripThinkingBlocks(preparedMessages)
	}
	preparedMessages, tokens, fitErr := l.fitPreparedRequest(preparedMessages, systemPrompt, toolDefs, budget, contextSpan)
	// Feeds the ContextTokens observable gauge registered in Run;
	// compaction shrinks this value, new messages grow it.
	l.lastContextTokens.Store(int64(tokens))
	contextSpan.SetAttributes(attribute.Int("messages.after", len(preparedMessages)))
	if fitErr != nil {
		recordSpanError(contextSpan, fitErr)
		l.reportContextExhausted(fitErr)
		return nil, 0, "context_exhausted"
	}
	if compaction != nil {
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
	return preparedMessages, tokens, ""
}

// reportContextExhausted logs and emits the error that ends a run as
// context_exhausted.
func (l *AgenticLoop) reportContextExhausted(err error) {
	l.Logger.Error("context budget exhausted", "error", err)
	_ = l.Transport.Emit(types.HarnessEvent{Type: "warning", Message: err.Error()})
}

// reportOverflowRetry logs and emits that a turn exceeded the context
// window and is retried after a forced compaction.
func (l *AgenticLoop) reportOverflowRetry(turn int, selection router.ModelSelection, attempt providerAttempt, rejected int) {
	cause := "the provider rejected the request"
	if attempt.err == nil {
		cause = "the response filled the window"
	}
	target := rejected * overflowCompactionPercent / 100
	l.Logger.Warn("context window exceeded; compacting and retrying the turn",
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
	// tokens is the turn's usage; Input falls back to the request
	// estimate when the provider reported none.
	tokens        types.TokenUsage
	inputReported bool
	duration      time.Duration
	err           error
	// errText is err scrubbed of secrets, safe for the run's failure text.
	errText string
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

// requestProvider sends one request and consumes its stream. A failed
// request is logged, emitted, counted and recorded as an "error" turn
// before it returns; a successful one is returned for the caller to
// record. requestTokens is the request's estimate.
func (l *AgenticLoop) requestProvider(
	ctx context.Context,
	config *types.RunConfig,
	turn int,
	selection router.ModelSelection,
	selectedProvider provider.ProviderAdapter,
	providerAttrs metric.MeasurementOption,
	params types.StreamParams,
	requestTokens int,
) providerAttempt {
	start := time.Now()
	l.Metrics.ProviderRequests.Add(ctx, 1, providerAttrs)
	spanCtx, providerSpan := l.Tracer.Start(l.traceCtx(ctx), "provider.stream",
		oteltrace.WithAttributes(
			attribute.String("provider.type", selection.Provider),
			attribute.String("provider.model", selection.Model),
			attribute.Int("turn.number", turn),
		),
	)

	// category distinguishes a rejected request from a fault after the
	// stream opened.
	fail := func(err error, category observability.ToolFailureCategory) providerAttempt {
		scrubbedErr := recordSpanError(providerSpan, err)
		providerSpan.End()
		// Skipped when ctx is already cancelled: the cancel/timeout
		// path reports instead.
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
		duration := time.Since(start)
		turnMode, turnBatchID := turnModeInfo(selectedProvider)
		l.Trace.RecordTurn(types.TurnTrace{
			Turn:       turn,
			StopReason: "error",
			DurationMs: duration.Milliseconds(),
			Mode:       turnMode,
			BatchID:    turnBatchID,
			Model:      selection.Model,
		})
		return providerAttempt{duration: duration, err: err, errText: scrubbedErr}
	}

	ch, err := selectedProvider.Stream(spanCtx, params)
	if err != nil {
		return fail(err, observability.ToolFailureProviderRequest)
	}
	sr, err := streamEventsToResult(ctx, ch, l.Transport, l.logEmitFailure)
	if err != nil {
		return fail(err, observability.ToolFailureProviderStream)
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
	return providerAttempt{
		result:        sr,
		tokens:        tokens,
		inputReported: inputReported,
		duration:      time.Since(start),
	}
}

// recordAttemptUsage charges a completed request's tokens to the run and
// records its turn summary, metrics and log line.
func (l *AgenticLoop) recordAttemptUsage(
	ctx context.Context,
	config *types.RunConfig,
	tokenTracker *TokenTracker,
	turn int,
	model string,
	selectedProvider provider.ProviderAdapter,
	attempt providerAttempt,
) {
	sr, tokens := attempt.result, attempt.tokens
	tokenTracker.RecordTurn(tokens)

	turnMode, turnBatchID := turnModeInfo(selectedProvider)
	l.Trace.RecordTurn(types.TurnTrace{
		Turn:          turn,
		Tokens:        tokens,
		InputReported: attempt.inputReported,
		StopReason:    sr.StopReason,
		StopDetails:   sr.StopDetails,
		DurationMs:    attempt.duration.Milliseconds(),
		Mode:          turnMode,
		BatchID:       turnBatchID,
		Model:         model,
	})

	modeAttr := l.metricAttrs(attribute.String("run.mode", config.Mode))
	l.Metrics.Turns.Add(ctx, 1, modeAttr)
	l.Metrics.TokensInput.Add(ctx, int64(tokens.Input), l.metricAttrs())
	l.Metrics.TokensOutput.Add(ctx, int64(tokens.Output), l.metricAttrs())
	if tokens.CacheRead > 0 {
		l.Metrics.TokensCacheRead.Add(ctx, int64(tokens.CacheRead), l.metricAttrs())
	}
	if tokens.CacheWrite > 0 {
		l.Metrics.TokensCacheWrite.Add(ctx, int64(tokens.CacheWrite), l.metricAttrs())
	}
	l.Metrics.TurnDuration.Record(ctx, float64(attempt.duration.Milliseconds()), modeAttr)

	l.Logger.Info("turn completed", "turn", turn,
		"tokens.input", tokens.Input,
		"tokens.input_reported", attempt.inputReported,
		"tokens.output", tokens.Output,
		"tokens.cache_read", tokens.CacheRead,
		"tokens.cache_write", tokens.CacheWrite,
		"tokens.reasoning", tokens.Reasoning,
		"stopReason", sr.StopReason)
}
