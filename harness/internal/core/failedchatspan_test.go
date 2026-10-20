package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/trace"
)

// chatEndSpy wraps the OTel emitter and records how each chat span ends.
type chatEndSpy struct {
	*trace.OTelTraceEmitter
	ends []trace.ChatSpanEnd
}

type spiedChatSpan struct {
	trace.ChatSpan
	spy *chatEndSpy
}

func (s *spiedChatSpan) End(end trace.ChatSpanEnd) {
	s.spy.ends = append(s.spy.ends, end)
	s.ChatSpan.End(end)
}

func (c *chatEndSpy) StartChatSpan(parent context.Context, start trace.ChatSpanStart) trace.ChatSpan {
	return &spiedChatSpan{ChatSpan: c.OTelTraceEmitter.StartChatSpan(parent, start), spy: c}
}

func TestLoop_FailedChatSpanEndsWithTheRequestSent(t *testing.T) {
	must := require.New(t)
	loop, _ := otelLoop(t, nil)
	spy := &chatEndSpy{OTelTraceEmitter: loop.Trace.(*trace.OTelTraceEmitter)}
	loop.Trace = spy
	loop.Provider = &errorProvider{err: errSpanProbe}
	_, _ = loop.Run(context.Background(), buildTestConfig())

	must.Len(spy.ends, 1, "chat span should end exactly once")
	end := spy.ends[0]
	must.Equal("provider_request_failed", end.ErrorType)
	must.NotNil(end.Record, "failed span should record the request")
	must.NotEmpty(end.Record.ModelInput.Messages, "recorded request should carry the messages that were sent")
	must.NotEmpty(end.Record.ModelInput.Model, "recorded request should name the model")
	must.Empty(end.Record.ModelOutput, "failed span should record no model output")
}
