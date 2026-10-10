package provider

import (
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// requireMarkedToolCall asserts the malformed-arguments contract every
// streaming adapter shares: the stream carries exactly one tool_call,
// marked with the raw arguments and a parse error, followed by a
// message_complete, and no error event.
func requireMarkedToolCall(t *testing.T, events []types.StreamEvent, wantID, wantName, wantRaw string) types.StreamEvent {
	t.Helper()
	var (
		calls       []types.StreamEvent
		sawComplete bool
	)
	for _, ev := range events {
		switch ev.Type {
		case "tool_call":
			calls = append(calls, ev)
		case "message_complete":
			if len(calls) == 0 {
				t.Errorf("message_complete arrived before the tool_call")
			}
			sawComplete = true
		case "error":
			t.Errorf("unexpected error event: %v", ev.Error)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("got %d tool_call events, want 1: %+v", len(calls), calls)
	}
	if !sawComplete {
		t.Error("no message_complete event after the malformed tool call")
	}
	call := calls[0]
	if wantID != "" && call.ID != wantID {
		t.Errorf("tool_call.ID = %q, want %q", call.ID, wantID)
	}
	if call.Name != wantName {
		t.Errorf("tool_call.Name = %q, want %q", call.Name, wantName)
	}
	if call.Input != nil {
		t.Errorf("tool_call.Input = %v, want nil for a marked call", call.Input)
	}
	if call.InvalidInput == nil {
		t.Fatal("tool_call.InvalidInput is nil, want the malformed-arguments marker")
	}
	if call.InvalidInput.Raw != wantRaw {
		t.Errorf("InvalidInput.Raw = %q, want %q", call.InvalidInput.Raw, wantRaw)
	}
	if call.InvalidInput.Error == "" {
		t.Error("InvalidInput.Error is empty, want the parse error")
	}
	return call
}

func TestToolCallEvent(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantInvalid bool
		wantErrPart string
		wantInput   map[string]any
	}{
		{name: "empty arguments are a zero-argument call", raw: ""},
		{name: "null arguments are a zero-argument call", raw: "null"},
		{name: "object", raw: `{"path":"main.go"}`, wantInput: map[string]any{"path": "main.go"}},
		{name: "trailing comma", raw: `{"path": "main.go",}`, wantInvalid: true, wantErrPart: "at byte offset 20"},
		{name: "truncated object", raw: `{"path": "ma`, wantInvalid: true, wantErrPart: "unexpected end of JSON input"},
		{name: "array is not an object", raw: `["main.go"]`, wantInvalid: true, wantErrPart: "cannot unmarshal array"},
		{name: "string is not an object", raw: `"main.go"`, wantInvalid: true, wantErrPart: "cannot unmarshal string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := toolCallEvent("call_1", "read_file", tc.raw)
			if ev.Type != "tool_call" || ev.ID != "call_1" || ev.Name != "read_file" {
				t.Fatalf("event = %+v, want a tool_call for call_1/read_file", ev)
			}
			if !tc.wantInvalid {
				if ev.InvalidInput != nil {
					t.Fatalf("InvalidInput = %+v, want nil", ev.InvalidInput)
				}
				if len(ev.Input) != len(tc.wantInput) || (tc.wantInput != nil && ev.Input["path"] != tc.wantInput["path"]) {
					t.Errorf("Input = %v, want %v", ev.Input, tc.wantInput)
				}
				return
			}
			if ev.InvalidInput == nil {
				t.Fatal("InvalidInput is nil, want a marker")
			}
			if ev.Input != nil {
				t.Errorf("Input = %v, want nil", ev.Input)
			}
			if ev.InvalidInput.Raw != tc.raw {
				t.Errorf("Raw = %q, want %q", ev.InvalidInput.Raw, tc.raw)
			}
			if !strings.Contains(ev.InvalidInput.Error, tc.wantErrPart) {
				t.Errorf("Error = %q, want it to contain %q", ev.InvalidInput.Error, tc.wantErrPart)
			}
		})
	}
}
