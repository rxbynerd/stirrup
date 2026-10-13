package core

import (
	"context"
	"testing"

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
	loop, _ := otelLoop(t, nil)
	spy := &chatEndSpy{OTelTraceEmitter: loop.Trace.(*trace.OTelTraceEmitter)}
	loop.Trace = spy
	loop.Provider = &errorProvider{err: errSpanProbe}
	_, _ = loop.Run(context.Background(), buildTestConfig())

	if len(spy.ends) != 1 {
		t.Fatalf("chat spans ended %d times, want 1", len(spy.ends))
	}
	end := spy.ends[0]
	if end.ErrorType != "provider_request_failed" {
		t.Fatalf("ErrorType = %q, want provider_request_failed", end.ErrorType)
	}
	if end.Record == nil || len(end.Record.ModelInput.Messages) == 0 {
		t.Fatalf("Record = %+v, want the messages that were sent", end.Record)
	}
	if end.Record.ModelInput.Model == "" || len(end.Record.ModelOutput) != 0 {
		t.Errorf("Record = %+v, want the model and no output", end.Record)
	}
}
