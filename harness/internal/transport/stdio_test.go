package transport

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// receiveControlEvent returns the next control event delivered on received,
// failing the test if none arrives within a second.
func receiveControlEvent(t *testing.T, received <-chan types.ControlEvent) types.ControlEvent {
	t.Helper()
	select {
	case ev := <-received:
		return ev
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for control event")
		return types.ControlEvent{}
	}
}

func TestStdioTransport_Emit_WritesJSONEvent(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	require.NoError(t, tr.Emit(types.HarnessEvent{Type: "text_delta", Text: "hello"}))

	var got types.HarnessEvent
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Equal(t, "text_delta", got.Type)
	require.Equal(t, "hello", got.Text)
}

func TestStdioTransport_Emit_WritesNewlineDelimitedLines(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	_ = tr.Emit(types.HarnessEvent{Type: "text_delta", Text: "a"})
	_ = tr.Emit(types.HarnessEvent{Type: "done", StopReason: "end_turn"})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 2, "output: %q", buf.String())
}

func TestStdioTransport_OnControl_DeliversControlEvent(t *testing.T) {
	reader := strings.NewReader(`{"type":"cancel"}` + "\n")
	tr := NewStdioTransport(&bytes.Buffer{}, reader)

	received := make(chan types.ControlEvent, 1)
	tr.OnControl(func(event types.ControlEvent) {
		received <- event
	})

	require.Equal(t, "cancel", receiveControlEvent(t, received).Type)
}

func TestStdioTransport_OnControl_SkipsMalformedLines(t *testing.T) {
	input := "not json\n" + `{"type":"cancel"}` + "\n"
	reader := strings.NewReader(input)
	tr := NewStdioTransport(&bytes.Buffer{}, reader)

	received := make(chan types.ControlEvent, 2)
	tr.OnControl(func(event types.ControlEvent) {
		received <- event
	})

	require.Equal(t, "cancel", receiveControlEvent(t, received).Type)
}

func TestStdioTransport_Emit_ScrubsSecrets(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	event := types.HarnessEvent{
		Type:    "tool_result",
		Content: "key is sk-ant-abc123-secret",
		Message: "token ghp_abcdef1234567890",
	}

	require.NoError(t, tr.Emit(event))

	output := buf.String()
	t.Run("removes anthropic api key", func(t *testing.T) {
		require.NotContains(t, output, "sk-ant-")
	})
	t.Run("removes github pat", func(t *testing.T) {
		require.NotContains(t, output, "ghp_")
	})
	t.Run("writes redacted placeholder", func(t *testing.T) {
		require.Contains(t, output, "[REDACTED]")
	})
}

func TestStdioTransport_Emit_ScrubsThinkingAndUserMessageText(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	events := []types.HarnessEvent{
		{Type: "thinking_delta", Text: "the key is sk-ant-abc123-secret and ghp_abcdef1234567890"},
		{Type: "user_message", Source: types.UserMessageSourceUserResponse, RequestID: "r1", Text: "use sk-ant-abc123-secret and ghp_abcdef1234567890"},
	}
	for _, ev := range events {
		require.NoError(t, tr.Emit(ev), "emit %s", ev.Type)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, len(events), "output: %q", buf.String())
	for i, line := range lines {
		var got types.HarnessEvent
		require.NoError(t, json.Unmarshal([]byte(line), &got), "line %d", i)

		t.Run(got.Type+" text is scrubbed", func(t *testing.T) {
			require.NotContains(t, got.Text, "sk-ant-")
			require.NotContains(t, got.Text, "ghp_")
			require.Contains(t, got.Text, "[REDACTED]")
		})

		if i == 1 {
			t.Run("user_message keeps source and request id", func(t *testing.T) {
				require.Equal(t, types.UserMessageSourceUserResponse, got.Source)
				require.Equal(t, "r1", got.RequestID)
			})
		}
	}
}

// TestStdioTransport_Emit_ScrubsInput checks that a permission_request
// forwards the tool call's raw Input once the sensitive-data latch has
// tripped, so a secret echoed into the call is scrubbed before it reaches
// the control plane.
func TestStdioTransport_Emit_ScrubsInput(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	event := types.HarnessEvent{
		Type:  "permission_request",
		Name:  "run_command",
		Input: json.RawMessage(`{"command":"echo AKIAQWERTYUIOPASDFGH"}`),
	}
	require.NoError(t, tr.Emit(event))

	output := buf.String()
	t.Run("removes live-shaped aws key", func(t *testing.T) {
		require.NotContains(t, output, "AKIAQWERTYUIOPASDFGH", "output: %s", output)
	})
	t.Run("writes redacted placeholder", func(t *testing.T) {
		require.Contains(t, output, "[REDACTED]", "output: %s", output)
	})

	// The scrubbed Input must remain valid JSON on the wire.
	var got types.HarnessEvent
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(output)), &got))
	require.True(t, json.Valid(got.Input), "scrubbed Input is not valid JSON: %q", got.Input)
}

// TestStdioTransport_Emit_ScrubInvalidatedJSONUsesPlaceholder covers the
// fallback path: when scrubbing breaks the JSON structure, the Input is
// replaced with a fixed placeholder rather than emitting malformed JSON.
// The generic_hex_secret pattern's trailing optional-quote consumes the
// JSON value's closing quote, so the [REDACTED] replacement leaves an
// unterminated string and invalidates the document.
func TestStdioTransport_Emit_ScrubInvalidatedJSONUsesPlaceholder(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	const hex = "0123456789abcdef0123456789abcdef" // 32 hex chars
	raw := `{"note":"password=` + hex + `"}`
	require.NoError(t, tr.Emit(types.HarnessEvent{Type: "permission_request", Input: json.RawMessage(raw)}))

	output := strings.TrimSpace(buf.String())
	var got types.HarnessEvent
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	require.True(t, json.Valid(got.Input), "Input must be valid JSON even on the fallback path, got: %q", got.Input)
	require.NotContains(t, string(got.Input), hex, "secret survived the fallback")
	require.Contains(t, string(got.Input), "scrub-invalidated-json", "expected the scrub-invalidated-json placeholder")
}

func TestStdioTransport_Close_ReturnsNoError(t *testing.T) {
	tr := NewStdioTransport(&bytes.Buffer{}, strings.NewReader(""))
	require.NoError(t, tr.Close())
}

func TestStdioTransport_Emit_LogsSecretRedactedEvent(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	var secBuf bytes.Buffer
	tr.Security = security.NewSecurityLogger(&secBuf, "run-1")

	require.NoError(t, tr.Emit(types.HarnessEvent{
		Type:    "tool_result",
		Content: "key=sk-ant-abc123-secret",
	}))

	got := secBuf.String()
	require.Contains(t, got, `"event":"secret_redacted_in_output"`)
	require.Contains(t, got, `"pattern":"anthropic_api_key"`)
	require.Contains(t, got, `"location":"transport.stdio.event.content"`)
}

// TestStdioTransport_RoundTripsBatchEventTypes confirms the stdio transport
// carries batch event-type discriminators through unchanged in both
// directions. The transport passes Type through without an allowlist.
func TestStdioTransport_RoundTripsBatchEventTypes(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	outbound := []types.HarnessEvent{
		{Type: "batch_submission", RequestID: "batch-1", Input: []byte(`{"provider_type":"anthropic"}`)},
		{Type: "batch_waiting", RequestID: "batch-1"},
		{Type: "batch_cancel_request", RequestID: "batch-1"},
	}
	for _, ev := range outbound {
		require.NoError(t, tr.Emit(ev), "emit %s", ev.Type)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, len(outbound), "output: %q", buf.String())
	for i, line := range lines {
		t.Run("outbound "+outbound[i].Type, func(t *testing.T) {
			var got types.HarnessEvent
			require.NoError(t, json.Unmarshal([]byte(line), &got))
			require.Equal(t, outbound[i].Type, got.Type)
			require.Equal(t, outbound[i].RequestID, got.RequestID)
		})
	}

	inbound := `{"type":"batch_result","requestId":"batch-1","content":"{\"response\":null,\"err\":{\"type\":\"batch_expired\"}}"}` + "\n"
	tr2 := NewStdioTransport(&bytes.Buffer{}, strings.NewReader(inbound))
	received := make(chan types.ControlEvent, 1)
	tr2.OnControl(func(event types.ControlEvent) {
		received <- event
	})

	ev := receiveControlEvent(t, received)
	require.Equal(t, "batch_result", ev.Type)
	require.Equal(t, "batch-1", ev.RequestID)
	require.NotEmpty(t, ev.Content, "batch_result content")
}

func TestStdioTransport_Emit_NoSecurityEventWithoutSecret(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))
	var secBuf bytes.Buffer
	tr.Security = security.NewSecurityLogger(&secBuf, "run-1")

	require.NoError(t, tr.Emit(types.HarnessEvent{
		Type: "text_delta",
		Text: "hello world",
	}))
	require.Empty(t, secBuf.String())
}
