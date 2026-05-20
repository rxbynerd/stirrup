package transport

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
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

func TestStdioTransport_EmitFiresSecretRedactedInOutput(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	var secBuf bytes.Buffer
	secLogger := security.NewSecurityLogger(&secBuf, "run-1")
	tr.Security = secLogger

	if err := tr.Emit(types.HarnessEvent{
		Type:    "tool_result",
		Content: "key=sk-ant-abc123-secret",
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	got := secBuf.String()
	if !strings.Contains(got, `"event":"secret_redacted_in_output"`) {
		t.Errorf("expected secret_redacted_in_output event, got %q", got)
	}
	if !strings.Contains(got, `"pattern":"anthropic_api_key"`) {
		t.Errorf("expected pattern=anthropic_api_key, got %q", got)
	}
	if !strings.Contains(got, `"location":"transport.stdio.event.content"`) {
		t.Errorf("expected location=transport.stdio.event.content, got %q", got)
	}
}

// TestStdioTransport_RoundTripsBatchEventTypes confirms the stdio
// transport carries the phase-2 batch event-type discriminators through
// without dropping or remapping them. The transport is pass-through on
// Type (no allowlist), so the test guards against a future regression
// that would add one.
func TestStdioTransport_RoundTripsBatchEventTypes(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))

	outbound := []types.HarnessEvent{
		{Type: "batch_submission", RequestID: "batch-1", Input: []byte(`{"provider_type":"anthropic"}`)},
		{Type: "batch_waiting", RequestID: "batch-1"},
		{Type: "batch_cancel_request", RequestID: "batch-1"},
	}
	for _, ev := range outbound {
		if err := tr.Emit(ev); err != nil {
			t.Fatalf("Emit %s: %v", ev.Type, err)
		}
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != len(outbound) {
		t.Fatalf("expected %d lines, got %d: %q", len(outbound), len(lines), buf.String())
	}
	for i, line := range lines {
		var got types.HarnessEvent
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("unmarshal line %d: %v", i, err)
		}
		if got.Type != outbound[i].Type {
			t.Errorf("line %d: got type %q, want %q", i, got.Type, outbound[i].Type)
		}
		if got.RequestID != outbound[i].RequestID {
			t.Errorf("line %d: got requestID %q, want %q", i, got.RequestID, outbound[i].RequestID)
		}
	}

	// Inbound batch_result via the read path.
	inbound := `{"type":"batch_result","requestId":"batch-1","content":"{\"response\":null,\"err\":{\"type\":\"batch_expired\"}}"}` + "\n"
	tr2 := NewStdioTransport(&bytes.Buffer{}, strings.NewReader(inbound))
	received := make(chan types.ControlEvent, 1)
	tr2.OnControl(func(event types.ControlEvent) {
		received <- event
	})
	select {
	case ev := <-received:
		if ev.Type != "batch_result" {
			t.Errorf("got type %q, want batch_result", ev.Type)
		}
		if ev.RequestID != "batch-1" {
			t.Errorf("got requestID %q, want batch-1", ev.RequestID)
		}
		if ev.Content == "" {
			t.Error("expected non-empty content on batch_result")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for batch_result control event")
	}
}

func TestStdioTransport_EmitNoEventWhenNoSecret(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdioTransport(&buf, strings.NewReader(""))
	var secBuf bytes.Buffer
	tr.Security = security.NewSecurityLogger(&secBuf, "run-1")

	if err := tr.Emit(types.HarnessEvent{
		Type: "text_delta",
		Text: "hello world",
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if secBuf.Len() != 0 {
		t.Errorf("expected no security event, got %q", secBuf.String())
	}
}
