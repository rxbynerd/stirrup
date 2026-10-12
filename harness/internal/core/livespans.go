package core

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
)

// startChatSpan opens the span for one provider request under the run
// span. The returned context carries the new span but descends from ctx,
// so the request is still cancelled with the run.
func (l *AgenticLoop) startChatSpan(ctx context.Context, config *types.RunConfig, turn int, selection router.ModelSelection, selectedProvider provider.ProviderAdapter, params types.StreamParams) (context.Context, trace.ChatSpan) {
	start := trace.ChatSpanStart{
		Turn:         turn,
		ProviderType: providerTypeFor(config, selection.Provider),
		Model:        selection.Model,
		MaxTokens:    params.MaxTokens,
		Temperature:  params.Temperature,
	}
	start.ServerAddress, start.ServerPort = provider.ServerEndpointOf(selectedProvider)
	var chat trace.ChatSpan
	if starter, ok := l.Trace.(trace.LiveSpanStarter); ok {
		chat = starter.StartChatSpan(l.traceCtx(ctx), start)
	}
	if chat == nil {
		chat = l.startFallbackChatSpan(ctx, start)
	}
	return oteltrace.ContextWithSpan(ctx, chat.Span()), chat
}

// startToolSpan opens the execute_tool span for one call under the run
// span, with the same context rule as startChatSpan.
func (l *AgenticLoop) startToolSpan(ctx context.Context, call types.ToolCall, known bool) (context.Context, trace.ToolSpan) {
	start := trace.ToolSpanStart{
		CallID:    call.ID,
		Name:      call.Name,
		Known:     known,
		InputSize: toolInputSize(call),
	}
	// The call context numbers turns from 1; turn.number counts from 0.
	if meta := tool.CallContextFrom(ctx); meta.Turn > 0 {
		start.Turn = meta.Turn - 1
	}
	var ts trace.ToolSpan
	if starter, ok := l.Trace.(trace.LiveSpanStarter); ok {
		ts = starter.StartToolSpan(l.traceCtx(ctx), start)
	}
	if ts == nil {
		ts = l.startFallbackToolSpan(ctx, start)
	}
	return oteltrace.ContextWithSpan(ctx, ts.Span()), ts
}

// fallbackSpan is the handle used when the trace emitter records no live
// spans. It keeps the span tree shape on l.Tracer, which is a noop tracer
// in production for such emitters, but records only bounded failure
// attributes.
type fallbackSpan struct {
	span oteltrace.Span
	once sync.Once
}

func (f *fallbackSpan) Span() oteltrace.Span { return f.span }

func (f *fallbackSpan) end(errorType string, opts ...oteltrace.SpanEndOption) {
	f.once.Do(func() {
		if errorType != "" {
			f.span.SetAttributes(attribute.String("error.type", errorType))
			f.span.SetStatus(codes.Error, "error")
		}
		f.span.End(opts...)
	})
}

type fallbackChatSpan struct{ fallbackSpan }

func (f *fallbackChatSpan) End(end trace.ChatSpanEnd) {
	errorType := end.ErrorType
	if errorType == "" && end.Summary != nil && end.Summary.StopReason == "error" {
		errorType = "error"
	}
	f.end(errorType)
}

type fallbackToolSpan struct{ fallbackSpan }

func (f *fallbackToolSpan) End(end trace.ToolSpanEnd) {
	errorType := ""
	if s := end.Summary; s != nil && !s.Success {
		errorType = s.ErrorCategory
		if errorType == "" {
			errorType = "error"
		}
	}
	var opts []oteltrace.SpanEndOption
	if !end.EndTime.IsZero() {
		opts = append(opts, oteltrace.WithTimestamp(end.EndTime))
	}
	f.end(errorType, opts...)
}

func (l *AgenticLoop) startFallbackChatSpan(ctx context.Context, start trace.ChatSpanStart) trace.ChatSpan {
	name := "chat"
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.Int("turn.number", start.Turn),
	}
	if start.Model != "" {
		name += " " + start.Model
		attrs = append(attrs, attribute.String("gen_ai.request.model", start.Model))
	}
	_, span := l.Tracer.Start(l.traceCtx(ctx), name,
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(attrs...),
	)
	return &fallbackChatSpan{fallbackSpan{span: span}}
}

func (l *AgenticLoop) startFallbackToolSpan(ctx context.Context, start trace.ToolSpanStart) trace.ToolSpan {
	name := "execute_tool"
	if start.Known {
		name += " " + start.Name
	}
	_, span := l.Tracer.Start(l.traceCtx(ctx), name,
		oteltrace.WithAttributes(
			attribute.String("gen_ai.operation.name", "execute_tool"),
			attribute.String("gen_ai.tool.name", start.Name),
			attribute.Int("tool.input_size", start.InputSize),
			attribute.Int("turn.number", start.Turn),
		),
	)
	return &fallbackToolSpan{fallbackSpan{span: span}}
}
