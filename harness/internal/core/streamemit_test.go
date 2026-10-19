package core

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// TestStreamEventsToResult_DeltasAreScrubbedBeforeEmit pins that the loop
// scrubs each released delta itself, so a transport that does not scrub,
// such as an embedder's, still never receives a secret split across
// provider deltas.
func TestStreamEventsToResult_DeltasAreScrubbedBeforeEmit(t *testing.T) {
	for _, eventType := range []string{"text_delta", "thinking_delta"} {
		split := len(fakeAnthropicKey) / 2
		rec := &recordingTransport{}
		ch := make(chan types.StreamEvent, 3)
		ch <- types.StreamEvent{Type: eventType, Text: "the key is " + fakeAnthropicKey[:split]}
		ch <- types.StreamEvent{Type: eventType, Text: fakeAnthropicKey[split:] + " and that is all"}
		ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn"}
		close(ch)
		if _, err := streamEventsToResult(context.Background(), ch, rec, true, ignoreEmitFailure, nil); err != nil {
			t.Fatal(err)
		}
		if got := joinedText(rec.events, eventType); got != "the key is [REDACTED] and that is all" {
			t.Errorf("%s reaching the transport = %q, want the key redacted", eventType, got)
		}
	}
}

// TestStreamEventsToResult_ShortReplyReleasedBeforeNextEvent pins that a
// short reply reaches the transport while the provider is still silent,
// rather than waiting for the stream's next event.
func TestStreamEventsToResult_ShortReplyReleasedBeforeNextEvent(t *testing.T) {
	ch := make(chan types.StreamEvent)
	emitted := make(chan string, 4)
	tp := emitFunc(func(ev types.HarnessEvent) error {
		if ev.Type == "text_delta" {
			emitted <- ev.Text
		}
		return nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = streamEventsToResult(context.Background(), ch, tp, true, ignoreEmitFailure, nil)
	}()
	const reply = "Hello there, short reply."
	ch <- types.StreamEvent{Type: "text_delta", Text: reply}
	var got string
	for got != reply {
		select {
		case text := <-emitted:
			got += text
		case <-time.After(5 * time.Second):
			t.Fatalf("reply on the transport = %q before the next provider event, want %q", got, reply)
		}
	}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn"}
	close(ch)
	<-done
}

// TestStreamEventsToResult_DeltaEmitFailureReported pins that a failed
// delta Emit is reported once per failed release, the deferred flush
// included, and does not change the stream result.
func TestStreamEventsToResult_DeltaEmitFailureReported(t *testing.T) {
	boom := errors.New("emit failed")
	tp := emitFunc(func(ev types.HarnessEvent) error {
		if ev.Type == "text_delta" || ev.Type == "thinking_delta" {
			return boom
		}
		return nil
	})
	var mu sync.Mutex
	var failed []string
	report := func(event string, err error) {
		mu.Lock()
		defer mu.Unlock()
		if errors.Is(err, boom) {
			failed = append(failed, event)
		}
	}
	ch := make(chan types.StreamEvent, 3)
	ch <- types.StreamEvent{Type: "thinking_delta", Text: "plan the change. "}
	// The trailing "sk-" is held and only leaves through the deferred
	// flush when the channel closes.
	ch <- types.StreamEvent{Type: "text_delta", Text: "the answer, then sk-"}
	close(ch)
	result, err := streamEventsToResult(context.Background(), ch, tp, true, report, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "thinking_delta text_delta text_delta"; strings.Join(failed, " ") != want {
		t.Errorf("reported failures = %v, want %q", failed, want)
	}
	if len(result.Blocks) != 1 || result.Blocks[0].Text != "the answer, then sk-" {
		t.Errorf("stream result blocks = %+v, want the full text block", result.Blocks)
	}
}

// TestStreamEventsToResult_RedactionReported pins that the loop reports a
// secret it scrubs from a delta, since the transport then has nothing
// left to redact and report.
func TestStreamEventsToResult_RedactionReported(t *testing.T) {
	var buf bytes.Buffer
	loop := &AgenticLoop{Security: security.NewSecurityLogger(&buf, "run-1")}
	split := len(fakeAnthropicKey) / 2
	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{Type: "text_delta", Text: "key " + fakeAnthropicKey[:split]}
	ch <- types.StreamEvent{Type: "text_delta", Text: fakeAnthropicKey[split:] + " done"}
	close(ch)
	if _, err := streamEventsToResult(context.Background(), ch, transport.NewNullTransport(), true, ignoreEmitFailure, loop.reportStreamRedaction); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, `"secret_redacted_in_output"`) || !strings.Contains(out, `"core.stream.text_delta"`) || !strings.Contains(out, "anthropic_api_key") {
		t.Errorf("security events = %s, want a secret_redacted_in_output for the delta", out)
	}
}

// TestStreamEventsToResult_WithheldTextIsReported pins that the carry
// withholding a possible secret past its bound is visible: the control
// stream gets a warning, a security event is raised, and the text after
// the match still arrives.
func TestStreamEventsToResult_WithheldTextIsReported(t *testing.T) {
	t.Cleanup(security.SetStreamCarryBoundsForTest(64<<10, 8<<10))
	var buf bytes.Buffer
	loop := &AgenticLoop{Security: security.NewSecurityLogger(&buf, "run-1"), Logger: slog.New(slog.DiscardHandler)}
	run := "token ghp_" + strings.Repeat("a", 80<<10)
	var events []types.StreamEvent
	for i := 0; i < len(run); i += 4 << 10 {
		events = append(events, types.StreamEvent{Type: "text_delta", Text: run[i:min(i+4<<10, len(run))]})
	}
	events = append(events, types.StreamEvent{Type: "text_delta", Text: " then prose"}, types.StreamEvent{Type: "message_complete", StopReason: "end_turn"})
	ch := make(chan types.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	tp := &recordingTransport{}
	if _, err := streamEventsToResult(context.Background(), ch, tp, true, ignoreEmitFailure, loop.reportStreamRedaction); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	warned := false
	for _, ev := range tp.events {
		switch ev.Type {
		case "text_delta":
			text.WriteString(ev.Text)
		case "warning":
			warned = warned || strings.HasPrefix(ev.Message, "text_delta withheld:")
		}
	}
	if text.String() != "token [REDACTED] then prose" || !warned {
		t.Errorf("stream = %q, warned = %v; want the text after the match and a warning", text.String(), warned)
	}
	if !strings.Contains(buf.String(), `"stream_text_withheld"`) {
		t.Errorf("security events = %s, want a stream_text_withheld", buf.String())
	}
}

// TestStreamEventsToResult_GRPCTransportReceivesScrubbedDeltas runs split
// deltas through the real gRPC transport: the control plane receives the
// key redacted once, and the thinking cap applies to the scrubbed text.
func TestStreamEventsToResult_GRPCTransportReceivesScrubbedDeltas(t *testing.T) {
	srv := &recordingHarnessServer{}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	pb.RegisterHarnessServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()
	tp, err := transport.NewGRPCTransport(context.Background(), "passthrough:///bufconn",
		transport.WithDialOptions(
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		))
	if err != nil {
		t.Fatal(err)
	}

	split := len(fakeAnthropicKey) / 2
	bigBlock := "reasoning with " + fakeAnthropicKey + " " + strings.Repeat("long reasoning. ", 20<<10)
	ch := make(chan types.StreamEvent, 4)
	ch <- types.StreamEvent{Type: "text_delta", Text: "the key is " + fakeAnthropicKey[:split]}
	ch <- types.StreamEvent{Type: "text_delta", Text: fakeAnthropicKey[split:] + " and that is all"}
	ch <- types.StreamEvent{Type: "thinking", Text: bigBlock}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn"}
	close(ch)
	if _, err := streamEventsToResult(context.Background(), ch, tp, true, ignoreEmitFailure, nil); err != nil {
		t.Fatal(err)
	}
	_ = tp.Close()

	var text, thinking string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		text, thinking = srv.joined("text_delta"), srv.joined("thinking_delta")
		if strings.Contains(thinking, "truncated by harness") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if text != "the key is [REDACTED] and that is all" {
		t.Errorf("text on the control stream = %q, want the key redacted once", text)
	}
	if !strings.HasPrefix(thinking, "reasoning with [REDACTED] long") || !strings.Contains(thinking, "truncated by harness") {
		t.Errorf("thinking on the control stream starts %q and is %d bytes; want the scrubbed head, capped", thinking[:min(len(thinking), 60)], len(thinking))
	}
	if strings.Contains(text+thinking, fakeAnthropicKey[split:]) {
		t.Error("part of the key reached the control stream")
	}
}

type emitFunc func(types.HarnessEvent) error

func (f emitFunc) Emit(ev types.HarnessEvent) error { return f(ev) }
func (emitFunc) OnControl(func(types.ControlEvent)) {}
func (emitFunc) Close() error                       { return nil }

// recordingHarnessServer records the HarnessEvents a RunTask stream
// carries.
type recordingHarnessServer struct {
	pb.UnimplementedHarnessServiceServer
	mu     sync.Mutex
	events []*pb.HarnessEvent
}

func (s *recordingHarnessServer) RunTask(stream pb.HarnessService_RunTaskServer) error {
	for {
		ev, err := stream.Recv()
		if err != nil {
			return nil
		}
		s.mu.Lock()
		s.events = append(s.events, ev)
		s.mu.Unlock()
	}
}

func (s *recordingHarnessServer) joined(eventType string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, ev := range s.events {
		if ev.Type == eventType {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}
