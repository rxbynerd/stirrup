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
	must := require.New(t)
	small := strings.Repeat("a", maxEventFieldBytes)
	must.Equal(small, capEventText(small), "text at the cap must pass through unchanged")

	big := strings.Repeat("é", maxEventFieldBytes)
	got := capEventText(big)
	must.LessOrEqual(len(got), maxEventFieldBytes)
	must.True(utf8.ValidString(got), "capped text must be valid UTF-8")
	must.True(strings.HasSuffix(got, truncationMarkerSuffix(len(big))), "marker must name the original size")
}

func TestBoundEventFields_CapsUserMessageAndThinkingText(t *testing.T) {
	must := require.New(t)
	big := strings.Repeat("é", maxEventFieldBytes)

	for _, typ := range []string{"user_message", "thinking_delta"} {
		t.Run(typ, func(t *testing.T) {
			must := require.New(t)
			ev := types.HarnessEvent{Type: typ, Source: types.UserMessageSourceInitialPrompt, Text: big}
			boundEventFields(&ev)
			must.LessOrEqual(len(ev.Text), maxEventFieldBytes)
			must.True(utf8.ValidString(ev.Text), "capped text must be valid UTF-8")
			must.True(strings.HasSuffix(ev.Text, truncationMarkerSuffix(len(big))), "marker must name the original size")
		})
	}

	delta := types.HarnessEvent{Type: "text_delta", Text: big}
	boundEventFields(&delta)
	must.Equal(big, delta.Text, "text_delta text must not be capped")
}

func TestCapEventInput_PassesSmallInputAndTruncatesLarge(t *testing.T) {
	must := require.New(t)
	small := json.RawMessage(`{"path":"a.go"}`)
	must.Equal(small, capEventInput(small), "input under the cap must pass through unchanged")

	for name, input := range map[string]json.RawMessage{
		"plain":           json.RawMessage(`{"content":"` + strings.Repeat("x", 1<<20) + `"}`),
		"escape expanded": json.RawMessage(`{"content":"` + strings.Repeat("<", 1<<20) + `"}`),
	} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			got := capEventInput(input)
			must.LessOrEqual(len(got), maxEventFieldBytes)
			var s string
			must.NoError(json.Unmarshal(got, &s), "capped input must be a valid JSON string")
			must.True(strings.HasSuffix(s, truncationMarkerSuffix(len(input))), "marker must name the original size")
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
			must := require.New(t)
			input := json.RawMessage(`{"content":"` + tc.body + `"}`)
			got := capEventInput(input)
			must.LessOrEqual(len(got), maxEventFieldBytes)
			var s string
			must.NoError(json.Unmarshal(got, &s), "capped input must be a valid JSON string")
			must.True(strings.HasPrefix(s, tc.wantHead), "capped input must keep its head")
			must.GreaterOrEqual(len(s), 32<<10, "capped input must keep at least 32 KiB")
		})
	}
}

// TestGRPCTransport_OversizedToolResultDoesNotEndStream drives a 6 MiB
// tool_result and tool_call through a default grpc.NewServer, whose 4 MiB
// receive limit aborts the stream on an oversized message, and checks the
// stream survives to deliver a later event.
func TestGRPCTransport_OversizedToolResultDoesNotEndStream(t *testing.T) {
	must := require.New(t)
	srv := newTestServer()
	tr, _, cleanup := setupTestTransport(t, srv)
	defer cleanup()

	const size = 6 << 20
	must.NoError(tr.Emit(types.HarnessEvent{Type: "tool_result", ToolUseID: "tc1", Content: strings.Repeat("r", size)}), "emit oversized tool_result")
	bigInput := json.RawMessage(`{"content":"` + strings.Repeat("w", size) + `"}`)
	must.NoError(tr.Emit(types.HarnessEvent{Type: "tool_call", ID: "tc2", Name: "write_file", Input: bigInput}), "emit oversized tool_call")
	must.NoError(tr.Emit(types.HarnessEvent{Type: "text_delta", Text: "after"}), "emit event after oversized events")

	_ = tr.stream.CloseSend()
	must.Eventually(func() bool { return len(srv.getReceived()) >= 3 }, 5*time.Second, 10*time.Millisecond)
	received := srv.getReceived()
	must.Len(received, 3)

	content := received[0].Content
	must.LessOrEqual(len(content), maxEventFieldBytes)
	must.True(strings.HasSuffix(content, truncationMarkerSuffix(size)), "tool_result content must carry the truncation marker")

	must.LessOrEqual(len(received[1].Input), maxEventFieldBytes)
	must.True(json.Valid(received[1].Input), "tool_call input must remain valid JSON")

	must.Equal("after", received[2].Text, "event after the oversized events must be delivered")
}

func TestBoundEventFields_CapsIdentifiers(t *testing.T) {
	must := require.New(t)
	big := strings.Repeat("é", maxEventIdentifierBytes)

	for _, ev := range []types.HarnessEvent{
		{Type: "tool_call", ID: big, Name: big},
		{Type: "tool_result", ToolUseID: big, Name: big},
		{Type: "user_message", Source: types.UserMessageSourceUserResponse, RequestID: big},
		{Type: "warning", Message: "user_response dropped: empty text", RequestID: big},
		{Type: "permission_request", RequestID: big, ToolName: big},
	} {
		t.Run(ev.Type, func(t *testing.T) {
			must := require.New(t)
			boundEventFields(&ev)
			for field, got := range map[string]string{
				"id": ev.ID, "name": ev.Name, "tool_use_id": ev.ToolUseID, "request_id": ev.RequestID, "tool_name": ev.ToolName,
			} {
				must.LessOrEqual(len(got), maxEventIdentifierBytes, "%s is over the identifier cap", field)
				must.True(utf8.ValidString(got), "%s must be valid UTF-8", field)
			}
		})
	}

	call := types.HarnessEvent{Type: "tool_call", ID: big}
	result := types.HarnessEvent{Type: "tool_result", ToolUseID: big}
	boundEventFields(&call)
	boundEventFields(&result)
	must.Equal(call.ID, result.ToolUseID, "capped tool_result tool_use_id must match capped tool_call id")

	done := types.HarnessEvent{Type: "done", StopReason: big}
	boundEventFields(&done)
	must.LessOrEqual(len(done.StopReason), maxEventIdentifierBytes, "done stop_reason must be capped")

	bigMessage := strings.Repeat("m", 6<<20)
	warning := types.HarnessEvent{Type: "warning", Message: bigMessage}
	boundEventFields(&warning)
	must.LessOrEqual(len(warning.Message), maxEventFieldBytes)
	must.True(strings.HasSuffix(warning.Message, truncationMarkerSuffix(len(bigMessage))), "warning message must carry the truncation marker")

	short := types.HarnessEvent{Type: "tool_call", ID: "tc1", Name: "read_file"}
	boundEventFields(&short)
	must.Equal("tc1", short.ID, "identifier under the cap must not be altered")
	must.Equal("read_file", short.Name, "identifier under the cap must not be altered")
}

func TestBoundEventFields_CapsToolsLoaded(t *testing.T) {
	t.Run("tool list is cut to the entry cap", func(t *testing.T) {
		must := require.New(t)
		tools := make([]string, maxEventToolEntries+44)
		for i := range tools {
			tools[i] = fmt.Sprintf("mcp_server_tool_%d", i)
		}
		ev := types.HarnessEvent{Type: "tools_loaded", Tools: tools}
		boundEventFields(&ev)
		must.Len(ev.Tools, maxEventToolEntries)
		must.Equal(tools[:maxEventToolEntries], ev.Tools)
	})

	t.Run("oversized entries, name and source are capped", func(t *testing.T) {
		must := require.New(t)
		big := strings.Repeat("é", maxEventIdentifierBytes)
		ev := types.HarnessEvent{Type: "tools_loaded", Name: big, Source: big, Tools: []string{"read_file", big}}
		boundEventFields(&ev)
		must.Equal("read_file", ev.Tools[0], "an entry under the cap is not altered")
		for field, got := range map[string]string{"tools[1]": ev.Tools[1], "name": ev.Name, "source": ev.Source} {
			must.LessOrEqual(len(got), maxEventIdentifierBytes, "%s is over the identifier cap", field)
			must.True(utf8.ValidString(got), "%s must be valid UTF-8", field)
		}
	})

	t.Run("oversized input stays valid JSON", func(t *testing.T) {
		must := require.New(t)
		input := json.RawMessage(`{"query":"` + strings.Repeat("a", maxEventFieldBytes) + `"}`)
		ev := types.HarnessEvent{Type: "tools_loaded", Input: input}
		boundEventFields(&ev)
		must.LessOrEqual(len(ev.Input), maxEventFieldBytes)
		must.True(json.Valid(ev.Input), "tools_loaded input must remain valid JSON")
	})

	t.Run("nil tool list stays nil", func(t *testing.T) {
		must := require.New(t)
		ev := types.HarnessEvent{Type: "tools_loaded", Source: types.ToolsLoadedSourceHarness}
		boundEventFields(&ev)
		must.Nil(ev.Tools)
		must.Equal("harness", ev.Source)
	})
}

func TestGRPCTransport_OversizedIdentifiersDoNotEndStream(t *testing.T) {
	must := require.New(t)
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
		must.NoError(tr.Emit(ev), "emit %s", ev.Type)
	}

	_ = tr.stream.CloseSend()
	must.Eventually(func() bool { return len(srv.getReceived()) >= 4 }, 5*time.Second, 10*time.Millisecond)
	received := srv.getReceived()
	must.Len(received, 4)
	must.Len(received[0].Name, maxEventIdentifierBytes, "tool_call name must be capped")
	must.Len(received[1].Name, maxEventIdentifierBytes, "tool_result name must be capped")
	must.Len(received[2].RequestId, maxEventIdentifierBytes, "request_id must be capped")
	must.Equal("after", received[3].Text, "event after the oversized identifiers must be delivered")
}
