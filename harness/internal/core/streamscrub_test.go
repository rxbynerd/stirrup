package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// stdioWireEvents runs events through streamEventsToResult into a real
// stdio transport, which scrubs each event on its own, and decodes what
// reached the wire.
func stdioWireEvents(t *testing.T, ctx context.Context, events []types.StreamEvent) []types.HarnessEvent {
	t.Helper()
	var wire bytes.Buffer
	tp := transport.NewStdioTransport(&wire, strings.NewReader(""))
	ch := make(chan types.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	_, _ = streamEventsToResult(ctx, ch, tp, true, ignoreEmitFailure)
	var out []types.HarnessEvent
	sc := bufio.NewScanner(&wire)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var ev types.HarnessEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("decode wire event: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

func joinedText(events []types.HarnessEvent, eventType string) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Type == eventType {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

// TestStreamEventsToResult_SecretSplitAcrossDeltasIsScrubbed pins that a
// secret the model writes across two deltas reaches the control stream
// redacted, for text and reasoning alike, even though the transport
// scrubs one event at a time.
func TestStreamEventsToResult_SecretSplitAcrossDeltasIsScrubbed(t *testing.T) {
	for _, eventType := range []string{"text_delta", "thinking_delta"} {
		t.Run(eventType, func(t *testing.T) {
			split := len(fakeAnthropicKey) / 2
			wire := stdioWireEvents(t, context.Background(), []types.StreamEvent{
				{Type: eventType, Text: "the key is " + fakeAnthropicKey[:split]},
				{Type: eventType, Text: fakeAnthropicKey[split:] + " and that is all"},
				{Type: "message_complete", StopReason: "end_turn"},
			})
			got := joinedText(wire, eventType)
			if strings.Contains(got, fakeAnthropicKey[split:]) || got != "the key is [REDACTED] and that is all" {
				t.Errorf("%s text on the wire = %q, want the split key redacted", eventType, got)
			}
		})
	}
}

// TestStreamEventsToResult_HeldDeltasFlushInOrderOnEveryExit pins that
// text the carry holds is emitted before the next event and on every way
// the stream can end, so nothing is lost or reordered.
func TestStreamEventsToResult_HeldDeltasFlushInOrderOnEveryExit(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name   string
		ctx    context.Context
		events []types.StreamEvent
		want   []string
	}{
		{
			name: "before a tool call",
			ctx:  context.Background(),
			events: []types.StreamEvent{
				{Type: "text_delta", Text: "calling"}, {Type: "text_delta", Text: " the tool"},
				{Type: "tool_call", ID: "c1", Name: "t", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			want: []string{"text_delta:calling the tool", "tool_call:"},
		},
		{
			name: "between reasoning and text",
			ctx:  context.Background(),
			events: []types.StreamEvent{
				{Type: "thinking_delta", Text: "think"}, {Type: "text_delta", Text: "say"},
				{Type: "thinking_delta", Text: "again"}, {Type: "message_complete", StopReason: "end_turn"},
			},
			want: []string{"thinking_delta:think", "text_delta:say", "thinking_delta:again"},
		},
		{
			name:   "on a provider error",
			ctx:    context.Background(),
			events: []types.StreamEvent{{Type: "text_delta", Text: "partial"}, {Type: "error", Error: errors.New("boom")}},
			want:   []string{"text_delta:partial"},
		},
		{
			name:   "when the stream closes without completing",
			ctx:    context.Background(),
			events: []types.StreamEvent{{Type: "text_delta", Text: "cut off"}},
			want:   []string{"text_delta:cut off"},
		},
		{
			name:   "on cancellation",
			ctx:    cancelled,
			events: []types.StreamEvent{{Type: "text_delta", Text: "never sent"}},
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingTransport{}
			ch := make(chan types.StreamEvent, len(tc.events))
			for _, ev := range tc.events {
				ch <- ev
			}
			close(ch)
			_, _ = streamEventsToResult(tc.ctx, ch, rec, true, ignoreEmitFailure)
			var got []string
			for _, ev := range rec.events {
				got = append(got, ev.Type+":"+ev.Text)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("emitted %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStreamEventsToResult_CancellationFlushesHeldText pins that text the
// carry still holds is emitted when the context ends mid-stream: the
// transport cancels on the first release, and the loop returns through
// its cancellation check on the next event.
func TestStreamEventsToResult_CancellationFlushesHeldText(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	long := strings.Repeat("words of reasoning ", 20)
	events := []types.StreamEvent{
		{Type: "text_delta", Text: "first "},
		{Type: "text_delta", Text: long},
		{Type: "text_delta", Text: "never read"},
	}
	ch := make(chan types.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	rec := &cancelOnEmitTransport{cancel: cancel}
	if _, err := streamEventsToResult(ctx, ch, rec, true, ignoreEmitFailure); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	if len(rec.events) < 2 {
		t.Fatalf("emitted %d events, want the release that cancelled and the flush after it", len(rec.events))
	}
	if got := joinedText(rec.events, "text_delta"); got != "first "+long {
		t.Errorf("text emitted = %q, want everything pushed before the cancellation", got)
	}
}

// cancelOnEmitTransport records events and cancels its context on the
// first one.
type cancelOnEmitTransport struct {
	recordingTransport
	cancel context.CancelFunc
}

func (t *cancelOnEmitTransport) Emit(event types.HarnessEvent) error {
	t.cancel()
	return t.recordingTransport.Emit(event)
}
