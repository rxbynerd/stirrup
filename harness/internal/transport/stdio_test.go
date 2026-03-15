package transport

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rubynerd/stirrup/types"
)

func TestStdioTransport_Emit(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	event := types.HarnessEvent{
		Type: "text_delta",
		Text: "hello",
	}

	if err := tr.Emit(event); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var got types.HarnessEvent
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if got.Type != "text_delta" || got.Text != "hello" {
		t.Errorf("got %+v, want type=text_delta text=hello", got)
	}
}

func TestStdioTransport_EmitNewlineDelimited(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	_ = tr.Emit(types.HarnessEvent{Type: "text_delta", Text: "a"})
	_ = tr.Emit(types.HarnessEvent{Type: "done", StopReason: "end_turn"})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), buf.String())
	}
}

func TestStdioTransport_OnControl(t *testing.T) {
	controlLine := `{"type":"cancel"}` + "\n"
	reader := strings.NewReader(controlLine)
	tr := NewStdioTransport(&bytes.Buffer{}, reader)

	received := make(chan types.ControlEvent, 1)
	tr.OnControl(func(event types.ControlEvent) {
		received <- event
	})

	select {
	case ev := <-received:
		if ev.Type != "cancel" {
			t.Errorf("got type %q, want cancel", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for control event")
	}
}

func TestStdioTransport_OnControlSkipsMalformed(t *testing.T) {
	input := "not json\n" + `{"type":"cancel"}` + "\n"
	reader := strings.NewReader(input)
	tr := NewStdioTransport(&bytes.Buffer{}, reader)

	received := make(chan types.ControlEvent, 2)
	tr.OnControl(func(event types.ControlEvent) {
		received <- event
	})

	select {
	case ev := <-received:
		if ev.Type != "cancel" {
			t.Errorf("got type %q, want cancel", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for control event")
	}
}

func TestStdioTransport_EmitScrubsSecrets(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	event := types.HarnessEvent{
		Type:    "tool_result",
		Content: "key is sk-ant-abc123-secret",
		Message: "token ghp_abcdef1234567890",
	}

	if err := tr.Emit(event); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if strings.Contains(output, "sk-ant-") {
		t.Error("Anthropic API key was not scrubbed from output")
	}
	if strings.Contains(output, "ghp_") {
		t.Error("GitHub PAT was not scrubbed from output")
	}
	if !strings.Contains(output, "[REDACTED]") {
		t.Error("expected [REDACTED] placeholder in output")
	}
}

func TestStdioTransport_Close(t *testing.T) {
	tr := NewStdioTransport(&bytes.Buffer{}, strings.NewReader(""))
	if err := tr.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
