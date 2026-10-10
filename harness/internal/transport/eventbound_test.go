package transport

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/types"
)

func TestCapEventText(t *testing.T) {
	small := strings.Repeat("a", maxEventFieldBytes)
	if got := capEventText(small); got != small {
		t.Fatal("text at the cap must pass through unchanged")
	}
	big := strings.Repeat("é", maxEventFieldBytes)
	got := capEventText(big)
	if len(got) > maxEventFieldBytes || !utf8.ValidString(got) {
		t.Fatalf("capped text is %d bytes (valid UTF-8: %v)", len(got), utf8.ValidString(got))
	}
	if !strings.HasSuffix(got, fmt.Sprintf(" of %d bytes shown]", len(big))) {
		t.Fatalf("marker must name the original size: %q", got[len(got)-80:])
	}
}

func TestCapEventInput(t *testing.T) {
	small := json.RawMessage(`{"path":"a.go"}`)
	if got := capEventInput(small); string(got) != string(small) {
		t.Fatal("input under the cap must pass through unchanged")
	}

	for name, input := range map[string]json.RawMessage{
		"plain":           json.RawMessage(`{"content":"` + strings.Repeat("x", 1<<20) + `"}`),
		"escape-expanded": json.RawMessage(`{"content":"` + strings.Repeat("<", 1<<20) + `"}`),
	} {
		t.Run(name, func(t *testing.T) {
			got := capEventInput(input)
			if len(got) > maxEventFieldBytes {
				t.Fatalf("capped input is %d bytes, want <= %d", len(got), maxEventFieldBytes)
			}
			var s string
			if err := json.Unmarshal(got, &s); err != nil {
				t.Fatalf("capped input must be a valid JSON string: %v", err)
			}
			if !strings.HasSuffix(s, fmt.Sprintf(" of %d bytes shown]", len(input))) {
				t.Fatalf("marker must name the original size: %q", s[len(s)-80:])
			}
		})
	}
}

// TestGRPCTransport_OversizedToolResultDoesNotEndStream drives a 6 MiB
// tool_result and tool_call through a default grpc.NewServer, whose 4 MiB
// receive limit aborts the stream on an oversized message, and checks the
// stream survives to deliver a later event.
func TestGRPCTransport_OversizedToolResultDoesNotEndStream(t *testing.T) {
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	const size = 6 << 20
	if err := tr.Emit(types.HarnessEvent{Type: "tool_result", ToolUseID: "tc1", Content: strings.Repeat("r", size)}); err != nil {
		t.Fatalf("Emit oversized tool_result: %v", err)
	}
	bigInput := json.RawMessage(`{"content":"` + strings.Repeat("w", size) + `"}`)
	if err := tr.Emit(types.HarnessEvent{Type: "tool_call", ID: "tc2", Name: "write_file", Input: bigInput}); err != nil {
		t.Fatalf("Emit oversized tool_call: %v", err)
	}
	if err := tr.Emit(types.HarnessEvent{Type: "text_delta", Text: "after"}); err != nil {
		t.Fatalf("Emit after oversized events: %v", err)
	}

	_ = tr.stream.CloseSend()
	deadline := time.Now().Add(5 * time.Second)
	for len(srv.getReceived()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	received := srv.getReceived()
	if len(received) != 3 {
		t.Fatalf("server received %d events, want 3", len(received))
	}
	if got := received[0].Content; len(got) > maxEventFieldBytes || !strings.HasSuffix(got, fmt.Sprintf(" of %d bytes shown]", size)) {
		t.Fatalf("tool_result content is %d bytes or lacks the marker", len(got))
	}
	if got := received[1].Input; len(got) > maxEventFieldBytes || !json.Valid(got) {
		t.Fatalf("tool_call input is %d bytes (valid JSON: %v)", len(got), json.Valid(got))
	}
	if received[2].Text != "after" {
		t.Fatalf("later event text = %q, want after", received[2].Text)
	}
}
