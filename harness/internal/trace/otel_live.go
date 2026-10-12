package trace

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
)

const (
	genAIToolTypeKey           = "gen_ai.tool.type"
	genAIRequestMaxTokensKey   = "gen_ai.request.max_tokens"
	genAIRequestTemperatureKey = "gen_ai.request.temperature"
	serverAddressKey           = "server.address"
	serverPortKey              = "server.port"
	spanAbandonedKey           = "stirrup.span.abandoned"
	toolSpanOperationName      = "execute_tool"
	chatSpanOperationName      = "chat"
)

var (
	_ LiveSpanStarter     = (*OTelTraceEmitter)(nil)
	_ SubAgentSpanStarter = (*OTelTraceEmitter)(nil)
	_ ChatSpan            = (*otelChatSpan)(nil)
	_ ToolSpan            = (*otelToolSpan)(nil)
	_ AgentSpan           = (*otelAgentSpan)(nil)
)

// liveHandle is an open span handle the emitter can end at Finish when
// the loop never did, so the subtree is still exported.
type liveHandle interface {
	abandon()
}

// liveSpan is the state shared by every live handle. once makes End
// idempotent and arbitrates between End and abandon.
type liveSpan struct {
	span  oteltrace.Span
	e     *OTelTraceEmitter
	runID string
	once  sync.Once
}

func (l *liveSpan) Span() oteltrace.Span { return l.span }

func (l *liveSpan) abandon() {
	l.once.Do(func() {
		l.span.SetAttributes(
			attribute.Bool(spanAbandonedKey, true),
			attribute.String(errorTypeKey, "abandoned"),
		)
		l.span.SetStatus(codes.Error, "span abandoned at run finish")
		l.span.End()
	})
}

// trackLocked registers h as open. Must be called with e.mu held.
func (e *OTelTraceEmitter) trackLocked(h liveHandle) {
	if e.open == nil {
		e.open = map[liveHandle]struct{}{}
	}
	e.open[h] = struct{}{}
}

// takeOpenHandles empties the open-handle set and returns its members.
func (e *OTelTraceEmitter) takeOpenHandles() []liveHandle {
	e.mu.Lock()
	defer e.mu.Unlock()
	handles := make([]liveHandle, 0, len(e.open))
	for h := range e.open {
		handles = append(handles, h)
	}
	e.open = nil
	return handles
}

// retainRunContentLocked feeds a captured request's content into its
// run's I/O slot. Input is set-once (the first request's seed prompt);
// output overwrites so the final assistant message wins, and an empty
// serialisation never clobbers earlier content. Must be called with e.mu
// held.
func (e *OTelTraceEmitter) retainRunContentLocked(runID string, content *turnContent) {
	if e.runContent == nil {
		e.runContent = map[string]*turnContent{}
	}
	run, ok := e.runContent[runID]
	if !ok {
		run = &turnContent{}
		e.runContent[runID] = run
	}
	if run.inputMessages == "" {
		run.inputMessages = content.inputMessages
	}
	if content.outputMessages != "" {
		run.outputMessages = content.outputMessages
	}
}

// StartChatSpan opens a CLIENT span for one model request under parent.
func (e *OTelTraceEmitter) StartChatSpan(parent context.Context, start ChatSpanStart) ChatSpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rootCtx == nil {
		return nil
	}

	model := start.Model
	if model == "" && e.config != nil {
		model = e.config.ModelRouter.Model
	}
	name := chatSpanOperationName
	attrs := []attribute.KeyValue{
		attribute.String(genAIOperationNameKey, chatSpanOperationName),
		attribute.Int("turn.number", start.Turn),
	}
	if model != "" {
		name += " " + model
		attrs = append(attrs, attribute.String(genAIRequestModelKey, model))
	}
	providerType := start.ProviderType
	if providerType == "" && e.config != nil {
		providerType = e.config.Provider.Type
	}
	if providerType != "" {
		attrs = append(attrs, attribute.String(genAIProviderNameKey, observability.GenAIProviderName(providerType)))
	}
	if start.ServerAddress != "" {
		attrs = append(attrs, attribute.String(serverAddressKey, start.ServerAddress))
		if start.ServerPort > 0 {
			attrs = append(attrs, attribute.Int(serverPortKey, start.ServerPort))
		}
	}
	if start.MaxTokens > 0 {
		attrs = append(attrs, attribute.Int(genAIRequestMaxTokensKey, start.MaxTokens))
	}
	if start.Temperature != nil {
		attrs = append(attrs, attribute.Float64(genAIRequestTemperatureKey, *start.Temperature))
	}

	_, span := e.tracer.Start(parent, name,
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(attrs...),
	)
	h := &otelChatSpan{liveSpan: liveSpan{span: span, e: e, runID: start.RunID}}
	e.trackLocked(h)
	return h
}

type otelChatSpan struct {
	liveSpan
}

// End stamps the request's usage, finish reason, failure and (when
// capture is on) scrubbed content on the span, then ends it. A failed
// request that reported no usage carries no usage attributes, and its
// "error" stop reason is not a model finish reason: error.type alone
// names the failure.
func (h *otelChatSpan) End(end ChatSpanEnd) {
	h.once.Do(func() {
		e := h.e
		failed := end.ErrorType != "" || (end.Summary != nil && end.Summary.StopReason == "error")
		var attrs []attribute.KeyValue
		stopReason := ""
		if s := end.Summary; s != nil {
			stopReason = s.StopReason
			attrs = append(attrs,
				attribute.Int("turn.tool_calls", s.ToolCalls),
				attribute.Int64("turn.duration_ms", s.DurationMs),
			)
			if !failed || s.Tokens.Input > 0 || s.Tokens.Output > 0 {
				attrs = append(attrs,
					attribute.Int(genAIUsageInputTokens, s.Tokens.Input),
					attribute.Int(genAIUsageOutputTokens, s.Tokens.Output),
				)
			}
			if s.StopReason != "" && s.StopReason != "error" {
				// The semconv attribute is a string array; stirrup streams
				// one choice per request, so it carries one element.
				attrs = append(attrs, attribute.StringSlice(genAIFinishReasonsKey, []string{s.StopReason}))
			}
			if s.ResponseModel != "" {
				attrs = append(attrs, attribute.String(genAIResponseModelKey, s.ResponseModel))
			}
			attrs = append(attrs, turnUsageBreakdownAttributes(*s)...)
			if s.Retries > 0 {
				attrs = append(attrs, attribute.Int("turn.retries", s.Retries))
			}
		}

		var content *turnContent
		if e.captureContent && end.Record != nil {
			scrubbed := scrubTurnRecord(*end.Record, e.redactionDisabled)
			content = &turnContent{
				inputMessages:  genAIInputMessagesJSON(scrubbed.ModelInput.Messages),
				outputMessages: genAIOutputMessagesJSON(scrubbed.ModelOutput, stopReason),
			}
		}

		e.mu.Lock()
		delete(e.open, liveHandle(h))
		systemInstructions := ""
		if content != nil {
			e.retainRunContentLocked(h.runID, content)
			// A sub-agent's system prompt differs from the run's and is
			// not recorded, so only top-level requests carry one.
			if h.runID == "" {
				systemInstructions = e.systemInstructionsJSON
			}
		}
		e.mu.Unlock()

		if content != nil {
			attrs = append(attrs, content.attributes(systemInstructions)...)
		}

		if failed {
			errorType := end.ErrorType
			if errorType == "" {
				errorType = "error"
			}
			attrs = append(attrs, attribute.String(errorTypeKey, errorType))
		}
		h.span.SetAttributes(attrs...)
		if failed {
			desc := scrubOrPass(end.ErrorText, e.redactionDisabled)
			if desc == "" {
				desc = "error"
			}
			h.span.SetStatus(codes.Error, desc)
		}
		h.span.End()
	})
}

// StartToolSpan opens the execute_tool span for one tool call under
// parent.
func (e *OTelTraceEmitter) StartToolSpan(parent context.Context, start ToolSpanStart) ToolSpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rootCtx == nil {
		return nil
	}

	// An unresolved tool name is model-controlled, so it stays out of the
	// span name to bound cardinality; gen_ai.tool.name still carries it.
	name := toolSpanOperationName
	if start.Known {
		name += " " + start.Name
	}
	attrs := []attribute.KeyValue{
		attribute.String(genAIOperationNameKey, toolSpanOperationName),
		attribute.String(genAIToolNameKey, start.Name),
		attribute.String(genAIToolTypeKey, "function"),
		attribute.Int("tool.input_size", start.InputSize),
	}
	if start.CallID != "" {
		attrs = append(attrs, attribute.String(genAIToolCallIDKey, scrubOrPass(start.CallID, e.redactionDisabled)))
	}
	_, span := e.tracer.Start(parent, name, oteltrace.WithAttributes(attrs...))
	h := &otelToolSpan{liveSpan: liveSpan{span: span, e: e, runID: start.RunID}}
	e.trackLocked(h)
	return h
}

type otelToolSpan struct {
	liveSpan
}

// End stamps the call's outcome and (when capture is on) its scrubbed
// arguments and result on the span, then ends it at end.EndTime.
func (h *otelToolSpan) End(end ToolSpanEnd) {
	h.once.Do(func() {
		e := h.e
		e.mu.Lock()
		delete(e.open, liveHandle(h))
		e.mu.Unlock()

		var attrs []attribute.KeyValue
		failed, desc := false, ""
		if s := end.Summary; s != nil {
			attrs = append(attrs,
				attribute.Bool("tool.success", s.Success),
				attribute.Int64("tool.duration_ms", s.DurationMs),
				attribute.Int("tool.output_size", s.OutputSize),
			)
			if end.Queued > 0 {
				attrs = append(attrs, attribute.Int64("tool.queue_ms", end.Queued.Milliseconds()))
			}
			if !s.Success {
				failed = true
				if s.ErrorCategory != "" {
					attrs = append(attrs, attribute.String(errorTypeKey, s.ErrorCategory))
				}
				// ErrorReason is scrubbed at dispatch; span status strings
				// bypass the log scrubber, so it is scrubbed again here.
				desc = scrubOrPass(s.ErrorReason, e.redactionDisabled)
				if desc == "" {
					desc = "tool call failed"
				}
			}
		}
		if e.captureContent && end.Record != nil {
			rec := scrubToolCallRecord(*end.Record, e.redactionDisabled)
			content := toolContent{arguments: validRawJSON(rec.Input), result: rec.Output}
			attrs = append(attrs, content.attributes()...)
		}

		h.span.SetAttributes(attrs...)
		if failed {
			h.span.SetStatus(codes.Error, desc)
		}
		if end.EndTime.IsZero() {
			h.span.End()
		} else {
			h.span.End(oteltrace.WithTimestamp(end.EndTime))
		}
	})
}

// StartSubAgentSpan opens the "invoke_agent stirrup.subagent" span for a
// sub-agent run under parent, the spawning tool call's context.
func (e *OTelTraceEmitter) StartSubAgentSpan(parent context.Context, start SubAgentSpanStart) AgentSpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rootCtx == nil {
		return nil
	}
	attrs := []attribute.KeyValue{
		attribute.String("run.id", start.RunID),
		attribute.String("run.parent_id", start.ParentRunID),
	}
	attrs = append(attrs, agentSpanAttributes(agentNameSubAgent, start.Config)...)
	ctx, span := e.tracer.Start(parent, "invoke_agent "+agentNameSubAgent, oteltrace.WithAttributes(attrs...))
	h := &otelAgentSpan{liveSpan: liveSpan{span: span, e: e, runID: start.RunID}, ctx: ctx}
	e.trackLocked(h)
	return h
}

type otelAgentSpan struct {
	liveSpan
	ctx context.Context //nolint:containedctx // the span's context, handed to the child run as its span parent
}

func (h *otelAgentSpan) Context() context.Context { return h.ctx }

// End stamps the sub-agent's outcome and (when capture is on) its seed
// prompt and final answer on the span, then ends it.
func (h *otelAgentSpan) End(end AgentSpanEnd) {
	h.once.Do(func() {
		e := h.e
		e.mu.Lock()
		delete(e.open, liveHandle(h))
		content := e.runContent[h.runID]
		delete(e.runContent, h.runID)
		e.mu.Unlock()

		attrs := []attribute.KeyValue{
			attribute.String("run.outcome", end.Outcome),
			attribute.Int("run.turns", end.Turns),
		}
		if content != nil {
			attrs = append(attrs, content.attributes("")...)
		}
		failed := end.Outcome != "success"
		if failed {
			attrs = append(attrs, attribute.String(errorTypeKey, end.Outcome))
		}
		h.span.SetAttributes(attrs...)
		if failed {
			// Outcome is a bounded enum, so it needs no scrubbing.
			h.span.SetStatus(codes.Error, end.Outcome)
		}
		h.span.End()
	})
}
