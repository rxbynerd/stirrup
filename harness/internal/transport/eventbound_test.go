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

func TestBoundEventFields_CapsUserMessageAndThinkingText(t *testing.T) {
	big := strings.Repeat("é", maxEventFieldBytes)

	for _, typ := range []string{"user_message", "thinking_delta"} {
		ev := types.HarnessEvent{Type: typ, Source: types.UserMessageSourceInitialPrompt, Text: big}
		boundEventFields(&ev)
		if len(ev.Text) > maxEventFieldBytes || !utf8.ValidString(ev.Text) || !strings.HasSuffix(ev.Text, fmt.Sprintf(" of %d bytes shown]", len(big))) {
			t.Errorf("%s text is %d bytes (valid UTF-8: %v) or lacks the marker", typ, len(ev.Text), utf8.ValidString(ev.Text))
		}
	}

	delta := types.HarnessEvent{Type: "text_delta", Text: big}
	boundEventFields(&delta)
	if delta.Text != big {
		t.Error("text_delta text was altered; it is not capped")
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

func TestCapEventInput_KeepsHeadOfEscapeHeavyInput(t *testing.T) {
	for name, tc := range map[string]struct {
		body     string
		wantHead string
	}{
		"html":   {body: strings.Repeat("<b>&</b>", 1<<17), wantHead: `{"content":"<b>&</b>`},
		"quotes": {body: strings.Repeat(`\"`, 1<<18), wantHead: `{"content":"\"\"`},
	} {
		t.Run(name, func(t *testing.T) {
			input := json.RawMessage(`{"content":"` + tc.body + `"}`)
			got := capEventInput(input)
			if len(got) > maxEventFieldBytes {
				t.Fatalf("capped input is %d bytes, want <= %d", len(got), maxEventFieldBytes)
			}
			var s string
			if err := json.Unmarshal(got, &s); err != nil {
				t.Fatalf("capped input must be a valid JSON string: %v", err)
			}
			if !strings.HasPrefix(s, tc.wantHead) || len(s) < 32<<10 {
				t.Fatalf("capped input dropped its head: %d bytes, starts %q", len(s), s[:min(len(s), 40)])
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

func TestBoundEventFields_CapsIdentifiers(t *testing.T) {
	big := strings.Repeat("é", maxEventIdentifierBytes)

	for _, ev := range []types.HarnessEvent{
		{Type: "tool_call", ID: big, Name: big},
		{Type: "tool_result", ToolUseID: big, Name: big},
		{Type: "user_message", Source: types.UserMessageSourceUserResponse, RequestID: big},
		{Type: "warning", Message: "user_response dropped: empty text", RequestID: big},
		{Type: "permission_request", RequestID: big, ToolName: big},
	} {
		boundEventFields(&ev)
		for field, got := range map[string]string{
			"id": ev.ID, "name": ev.Name, "tool_use_id": ev.ToolUseID, "request_id": ev.RequestID, "tool_name": ev.ToolName,
		} {
			if len(got) > maxEventIdentifierBytes || !utf8.ValidString(got) {
				t.Errorf("%s %s is %d bytes (valid UTF-8: %v)", ev.Type, field, len(got), utf8.ValidString(got))
			}
		}
	}

	call := types.HarnessEvent{Type: "tool_call", ID: big}
	result := types.HarnessEvent{Type: "tool_result", ToolUseID: big}
	boundEventFields(&call)
	boundEventFields(&result)
	if call.ID != result.ToolUseID {
		t.Error("a capped tool_result tool_use_id no longer matches its capped tool_call id")
	}

	done := types.HarnessEvent{Type: "done", StopReason: big}
	boundEventFields(&done)
	if len(done.StopReason) > maxEventIdentifierBytes {
		t.Errorf("done stop_reason is %d bytes", len(done.StopReason))
	}

	bigMessage := strings.Repeat("m", 6<<20)
	warning := types.HarnessEvent{Type: "warning", Message: bigMessage}
	boundEventFields(&warning)
	if len(warning.Message) > maxEventFieldBytes || !strings.HasSuffix(warning.Message, fmt.Sprintf(" of %d bytes shown]", len(bigMessage))) {
		t.Errorf("warning message is %d bytes or lacks the marker", len(warning.Message))
	}

	short := types.HarnessEvent{Type: "tool_call", ID: "tc1", Name: "read_file"}
	boundEventFields(&short)
	if short.ID != "tc1" || short.Name != "read_file" {
		t.Errorf("identifiers under the cap were altered: %+v", short)
	}
}

func TestGRPCTransport_OversizedIdentifiersDoNotEndStream(t *testing.T) {
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	big := strings.Repeat("n", 6<<20)
	for _, ev := range []types.HarnessEvent{
		{Type: "tool_call", ID: "tc1", Name: big, Input: json.RawMessage(`{}`)},
		{Type: "tool_result", ToolUseID: "tc1", Name: big, Content: "ok"},
		{Type: "user_message", Source: types.UserMessageSourceUserResponse, Text: "hi", RequestID: big},
		{Type: "text_delta", Text: "after"},
	} {
		if err := tr.Emit(ev); err != nil {
			t.Fatalf("Emit %s: %v", ev.Type, err)
		}
	}

	_ = tr.stream.CloseSend()
	deadline := time.Now().Add(5 * time.Second)
	for len(srv.getReceived()) < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	received := srv.getReceived()
	if len(received) != 4 {
		t.Fatalf("server received %d events, want 4", len(received))
	}
	if len(received[0].Name) != maxEventIdentifierBytes || len(received[1].Name) != maxEventIdentifierBytes || len(received[2].RequestId) != maxEventIdentifierBytes {
		t.Fatalf("identifier lengths = %d, %d, %d, want %d", len(received[0].Name), len(received[1].Name), len(received[2].RequestId), maxEventIdentifierBytes)
	}
	if received[3].Text != "after" {
		t.Fatalf("later event text = %q, want after", received[3].Text)
	}
}
