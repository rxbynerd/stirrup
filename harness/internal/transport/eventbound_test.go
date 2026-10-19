package transport

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// truncationMarkerSuffix is the tail a capped value ends with, naming the
// original size in bytes.
func truncationMarkerSuffix(originalBytes int) string {
	return fmt.Sprintf(" of %d bytes shown]", originalBytes)
}

func TestCapEventText_TruncatesWithMarkerAndValidUTF8(t *testing.T) {
	small := strings.Repeat("a", maxEventFieldBytes)
	require.Equal(t, small, capEventText(small), "text at the cap must pass through unchanged")

	big := strings.Repeat("é", maxEventFieldBytes)
	got := capEventText(big)
	require.LessOrEqual(t, len(got), maxEventFieldBytes)
	require.True(t, utf8.ValidString(got), "capped text must be valid UTF-8")
	require.True(t, strings.HasSuffix(got, truncationMarkerSuffix(len(big))), "marker must name the original size")
}

func TestBoundEventFields_CapsUserMessageAndThinkingText(t *testing.T) {
	big := strings.Repeat("é", maxEventFieldBytes)

	for _, typ := range []string{"user_message", "thinking_delta"} {
		t.Run(typ, func(t *testing.T) {
			ev := types.HarnessEvent{Type: typ, Source: types.UserMessageSourceInitialPrompt, Text: big}
			boundEventFields(&ev)
			require.LessOrEqual(t, len(ev.Text), maxEventFieldBytes)
			require.True(t, utf8.ValidString(ev.Text), "capped text must be valid UTF-8")
			require.True(t, strings.HasSuffix(ev.Text, truncationMarkerSuffix(len(big))), "marker must name the original size")
		})
	}

	delta := types.HarnessEvent{Type: "text_delta", Text: big}
	boundEventFields(&delta)
	require.Equal(t, big, delta.Text, "text_delta text must not be capped")
}

func TestCapEventInput_PassesSmallInputAndTruncatesLarge(t *testing.T) {
	small := json.RawMessage(`{"path":"a.go"}`)
	require.Equal(t, small, capEventInput(small), "input under the cap must pass through unchanged")

	for name, input := range map[string]json.RawMessage{
		"plain":           json.RawMessage(`{"content":"` + strings.Repeat("x", 1<<20) + `"}`),
		"escape expanded": json.RawMessage(`{"content":"` + strings.Repeat("<", 1<<20) + `"}`),
	} {
		t.Run(name, func(t *testing.T) {
			got := capEventInput(input)
			require.LessOrEqual(t, len(got), maxEventFieldBytes)
			var s string
			require.NoError(t, json.Unmarshal(got, &s), "capped input must be a valid JSON string")
			require.True(t, strings.HasSuffix(s, truncationMarkerSuffix(len(input))), "marker must name the original size")
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
			require.LessOrEqual(t, len(got), maxEventFieldBytes)
			var s string
			require.NoError(t, json.Unmarshal(got, &s), "capped input must be a valid JSON string")
			require.True(t, strings.HasPrefix(s, tc.wantHead), "capped input must keep its head")
			require.GreaterOrEqual(t, len(s), 32<<10, "capped input must keep at least 32 KiB")
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
	require.NoError(t, tr.Emit(types.HarnessEvent{Type: "tool_result", ToolUseID: "tc1", Content: strings.Repeat("r", size)}), "emit oversized tool_result")
	bigInput := json.RawMessage(`{"content":"` + strings.Repeat("w", size) + `"}`)
	require.NoError(t, tr.Emit(types.HarnessEvent{Type: "tool_call", ID: "tc2", Name: "write_file", Input: bigInput}), "emit oversized tool_call")
	require.NoError(t, tr.Emit(types.HarnessEvent{Type: "text_delta", Text: "after"}), "emit event after oversized events")

	_ = tr.stream.CloseSend()
	require.Eventually(t, func() bool { return len(srv.getReceived()) >= 3 }, 5*time.Second, 10*time.Millisecond)
	received := srv.getReceived()
	require.Len(t, received, 3)

	content := received[0].Content
	require.LessOrEqual(t, len(content), maxEventFieldBytes)
	require.True(t, strings.HasSuffix(content, truncationMarkerSuffix(size)), "tool_result content must carry the truncation marker")

	require.LessOrEqual(t, len(received[1].Input), maxEventFieldBytes)
	require.True(t, json.Valid(received[1].Input), "tool_call input must remain valid JSON")

	require.Equal(t, "after", received[2].Text, "event after the oversized events must be delivered")
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
		t.Run(ev.Type, func(t *testing.T) {
			boundEventFields(&ev)
			for field, got := range map[string]string{
				"id": ev.ID, "name": ev.Name, "tool_use_id": ev.ToolUseID, "request_id": ev.RequestID, "tool_name": ev.ToolName,
			} {
				require.LessOrEqual(t, len(got), maxEventIdentifierBytes, "%s is over the identifier cap", field)
				require.True(t, utf8.ValidString(got), "%s must be valid UTF-8", field)
			}
		})
	}

	call := types.HarnessEvent{Type: "tool_call", ID: big}
	result := types.HarnessEvent{Type: "tool_result", ToolUseID: big}
	boundEventFields(&call)
	boundEventFields(&result)
	require.Equal(t, call.ID, result.ToolUseID, "capped tool_result tool_use_id must match capped tool_call id")

	done := types.HarnessEvent{Type: "done", StopReason: big}
	boundEventFields(&done)
	require.LessOrEqual(t, len(done.StopReason), maxEventIdentifierBytes, "done stop_reason must be capped")

	bigMessage := strings.Repeat("m", 6<<20)
	warning := types.HarnessEvent{Type: "warning", Message: bigMessage}
	boundEventFields(&warning)
	require.LessOrEqual(t, len(warning.Message), maxEventFieldBytes)
	require.True(t, strings.HasSuffix(warning.Message, truncationMarkerSuffix(len(bigMessage))), "warning message must carry the truncation marker")

	short := types.HarnessEvent{Type: "tool_call", ID: "tc1", Name: "read_file"}
	boundEventFields(&short)
	require.Equal(t, "tc1", short.ID, "identifier under the cap must not be altered")
	require.Equal(t, "read_file", short.Name, "identifier under the cap must not be altered")
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
		require.NoError(t, tr.Emit(ev), "emit %s", ev.Type)
	}

	_ = tr.stream.CloseSend()
	require.Eventually(t, func() bool { return len(srv.getReceived()) >= 4 }, 5*time.Second, 10*time.Millisecond)
	received := srv.getReceived()
	require.Len(t, received, 4)
	require.Len(t, received[0].Name, maxEventIdentifierBytes, "tool_call name must be capped")
	require.Len(t, received[1].Name, maxEventIdentifierBytes, "tool_result name must be capped")
	require.Len(t, received[2].RequestId, maxEventIdentifierBytes, "request_id must be capped")
	require.Equal(t, "after", received[3].Text, "event after the oversized identifiers must be delivered")
}
