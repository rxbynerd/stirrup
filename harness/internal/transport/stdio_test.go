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
	must := require.New(t)
	select {
	case ev := <-received:
		return ev
	case <-time.After(time.Second):
		must.Fail("timed out waiting for control event")
		return types.ControlEvent{}
	}
}

func TestStdioTransport_Emit_WritesJSONEvent(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	must.NoError(tr.Emit(types.HarnessEvent{Type: "text_delta", Text: "hello"}))

	var got types.HarnessEvent
	must.NoError(json.Unmarshal(buf.Bytes(), &got))
	must.Equal("text_delta", got.Type)
	must.Equal("hello", got.Text)
}

func TestStdioTransport_Emit_WritesNewlineDelimitedLines(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	_ = tr.Emit(types.HarnessEvent{Type: "text_delta", Text: "a"})
	_ = tr.Emit(types.HarnessEvent{Type: "done", StopReason: "end_turn"})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	must.Len(lines, 2, "output: %q", buf.String())
}

func TestStdioTransport_OnControl_DeliversControlEvent(t *testing.T) {
	must := require.New(t)
	reader := strings.NewReader(`{"type":"cancel"}` + "\n")
	tr := NewStdioTransport(&bytes.Buffer{}, reader)

	received := make(chan types.ControlEvent, 1)
	tr.OnControl(func(event types.ControlEvent) {
		received <- event
	})

	must.Equal("cancel", receiveControlEvent(t, received).Type)
}

func TestStdioTransport_OnControl_SkipsMalformedLines(t *testing.T) {
	must := require.New(t)
	input := "not json\n" + `{"type":"cancel"}` + "\n"
	reader := strings.NewReader(input)
	tr := NewStdioTransport(&bytes.Buffer{}, reader)

	received := make(chan types.ControlEvent, 2)
	tr.OnControl(func(event types.ControlEvent) {
		received <- event
	})

	must.Equal("cancel", receiveControlEvent(t, received).Type)
}

func TestStdioTransport_Emit_ScrubsSecrets(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	event := types.HarnessEvent{
		Type:    "tool_result",
		Content: "key is sk-ant-abc123-secret",
		Message: "token ghp_abcdef1234567890",
	}

	must.NoError(tr.Emit(event))

	output := buf.String()
	t.Run("removes anthropic api key", func(t *testing.T) {
		must := require.New(t)
		must.NotContains(output, "sk-ant-")
	})
	t.Run("removes github pat", func(t *testing.T) {
		must := require.New(t)
		must.NotContains(output, "ghp_")
	})
	t.Run("writes redacted placeholder", func(t *testing.T) {
		must := require.New(t)
		must.Contains(output, "[REDACTED]")
	})
}

func TestStdioTransport_Emit_ScrubsThinkingAndUserMessageText(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	events := []types.HarnessEvent{
		{Type: "thinking_delta", Text: "the key is sk-ant-abc123-secret and ghp_abcdef1234567890"},
		{Type: "user_message", Source: types.UserMessageSourceUserResponse, RequestID: "r1", Text: "use sk-ant-abc123-secret and ghp_abcdef1234567890"},
	}
	for _, ev := range events {
		must.NoError(tr.Emit(ev), "emit %s", ev.Type)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	must.Len(lines, len(events), "output: %q", buf.String())
	for i, line := range lines {
		var got types.HarnessEvent
		must.NoError(json.Unmarshal([]byte(line), &got), "line %d", i)

		t.Run(got.Type+" text is scrubbed", func(t *testing.T) {
			must := require.New(t)
			must.NotContains(got.Text, "sk-ant-")
			must.NotContains(got.Text, "ghp_")
			must.Contains(got.Text, "[REDACTED]")
		})

		if i == 1 {
			t.Run("user_message keeps source and request id", func(t *testing.T) {
				must := require.New(t)
				must.Equal(types.UserMessageSourceUserResponse, got.Source)
				must.Equal("r1", got.RequestID)
			})
		}
	}
}

// TestStdioTransport_Emit_ScrubsInput checks that a permission_request
// forwards the tool call's raw Input once the sensitive-data latch has
// tripped, so a secret echoed into the call is scrubbed before it reaches
// the control plane.
func TestStdioTransport_Emit_ScrubsInput(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	event := types.HarnessEvent{
		Type:  "permission_request",
		Name:  "run_command",
		Input: json.RawMessage(`{"command":"echo AKIAQWERTYUIOPASDFGH"}`),
	}
	must.NoError(tr.Emit(event))

	output := buf.String()
	t.Run("removes live-shaped aws key", func(t *testing.T) {
		must := require.New(t)
		must.NotContains(output, "AKIAQWERTYUIOPASDFGH", "output: %s", output)
	})
	t.Run("writes redacted placeholder", func(t *testing.T) {
		must := require.New(t)
		must.Contains(output, "[REDACTED]", "output: %s", output)
	})

	// The scrubbed Input must remain valid JSON on the wire.
	var got types.HarnessEvent
	must.NoError(json.Unmarshal([]byte(strings.TrimSpace(output)), &got))
	must.True(json.Valid(got.Input), "scrubbed Input is not valid JSON: %q", got.Input)
}

// TestStdioTransport_Emit_ScrubInvalidatedJSONUsesPlaceholder covers the
// fallback path: when scrubbing breaks the JSON structure, the Input is
// replaced with a fixed placeholder rather than emitting malformed JSON.
// The generic_hex_secret pattern's trailing optional-quote consumes the
// JSON value's closing quote, so the [REDACTED] replacement leaves an
// unterminated string and invalidates the document.
func TestStdioTransport_Emit_ScrubInvalidatedJSONUsesPlaceholder(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	const hex = "0123456789abcdef0123456789abcdef" // 32 hex chars
	raw := `{"note":"password=` + hex + `"}`
	must.NoError(tr.Emit(types.HarnessEvent{Type: "permission_request", Input: json.RawMessage(raw)}))

	output := strings.TrimSpace(buf.String())
	var got types.HarnessEvent
	must.NoError(json.Unmarshal([]byte(output), &got))
	must.True(json.Valid(got.Input), "Input must be valid JSON even on the fallback path, got: %q", got.Input)
	must.NotContains(string(got.Input), hex, "secret survived the fallback")
	must.Contains(string(got.Input), "scrub-invalidated-json", "expected the scrub-invalidated-json placeholder")
}

func TestStdioTransport_Close_ReturnsNoError(t *testing.T) {
	must := require.New(t)
	tr := NewStdioTransport(&bytes.Buffer{}, strings.NewReader(""))
	must.NoError(tr.Close())
}

func TestStdioTransport_Emit_LogsSecretRedactedEvent(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	var secBuf bytes.Buffer
	tr.Security = security.NewSecurityLogger(&secBuf, "run-1")

	must.NoError(tr.Emit(types.HarnessEvent{
		Type:    "tool_result",
		Content: "key=sk-ant-abc123-secret",
	}))

	got := secBuf.String()
	must.Contains(got, `"event":"secret_redacted_in_output"`)
	must.Contains(got, `"pattern":"anthropic_api_key"`)
	must.Contains(got, `"location":"transport.stdio.event.content"`)
}

// TestStdioTransport_RoundTripsBatchEventTypes confirms the stdio transport
// carries batch event-type discriminators through unchanged in both
// directions. The transport passes Type through without an allowlist.
func TestStdioTransport_RoundTripsBatchEventTypes(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	outbound := []types.HarnessEvent{
		{Type: "batch_submission", RequestID: "batch-1", Input: []byte(`{"provider_type":"anthropic"}`)},
		{Type: "batch_waiting", RequestID: "batch-1"},
		{Type: "batch_cancel_request", RequestID: "batch-1"},
	}
	for _, ev := range outbound {
		must.NoError(tr.Emit(ev), "emit %s", ev.Type)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	must.Len(lines, len(outbound), "output: %q", buf.String())
	for i, line := range lines {
		t.Run("outbound "+outbound[i].Type, func(t *testing.T) {
			must := require.New(t)
			var got types.HarnessEvent
			must.NoError(json.Unmarshal([]byte(line), &got))
			must.Equal(outbound[i].Type, got.Type)
			must.Equal(outbound[i].RequestID, got.RequestID)
		})
	}

	inbound := `{"type":"batch_result","requestId":"batch-1","content":"{\"response\":null,\"err\":{\"type\":\"batch_expired\"}}"}` + "\n"
	tr2 := NewStdioTransport(&bytes.Buffer{}, strings.NewReader(inbound))
	received := make(chan types.ControlEvent, 1)
	tr2.OnControl(func(event types.ControlEvent) {
		received <- event
	})

	ev := receiveControlEvent(t, received)
	must.Equal("batch_result", ev.Type)
	must.Equal("batch-1", ev.RequestID)
	must.NotEmpty(ev.Content, "batch_result content")
}

func TestStdioTransport_Emit_NoSecurityEventWithoutSecret(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))
	var secBuf bytes.Buffer
	tr.Security = security.NewSecurityLogger(&secBuf, "run-1")

	must.NoError(tr.Emit(types.HarnessEvent{
		Type: "text_delta",
		Text: "hello world",
	}))
	must.Empty(secBuf.String())
}
