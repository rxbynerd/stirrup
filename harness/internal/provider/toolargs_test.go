package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// requireMarkedToolCall asserts the malformed-arguments contract every
// streaming adapter shares: the stream carries exactly one tool_call,
// marked with the raw arguments and a parse error, followed by a
// message_complete, and no error event.
func requireMarkedToolCall(t *testing.T, events []types.StreamEvent, wantID, wantName, wantRaw string) types.StreamEvent {
	t.Helper()
	check := assert.New(t)
	must := require.New(t)
	var (
		calls       []types.StreamEvent
		sawComplete bool
	)
	for _, ev := range events {
		switch ev.Type {
		case "tool_call":
			calls = append(calls, ev)
		case "message_complete":
			check.NotEmpty(calls, "message_complete should follow the tool_call")
			sawComplete = true
		case "error":
			check.Fail("unexpected error event", "%v", ev.Error)
		}
	}
	must.Len(calls, 1, "tool_call events: %+v", calls)
	check.True(sawComplete, "a message_complete should follow the malformed tool call")

	call := calls[0]
	if wantID != "" {
		check.Equal(wantID, call.ID)
	}
	check.Equal(wantName, call.Name)
	check.Nil(call.Input, "a marked tool_call should carry no input")
	must.NotNil(call.InvalidInput, "the tool_call should carry the malformed-arguments marker")
	check.Equal(wantRaw, call.InvalidInput.Raw)
	check.NotEmpty(call.InvalidInput.Error, "the marker should carry the parse error")
	return call
}

func TestToolCallEvent_ArgumentDecoding(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantInvalid bool
		wantErrPart string
		wantInput   map[string]any
	}{
		{name: "empty arguments are a zero-argument call", raw: ""},
		{name: "null arguments are a zero-argument call", raw: "null"},
		{name: "object arguments decode into input", raw: `{"path":"main.go"}`, wantInput: map[string]any{"path": "main.go"}},
		{name: "trailing comma is marked invalid", raw: `{"path": "main.go",}`, wantInvalid: true, wantErrPart: "at byte offset 20"},
		{name: "truncated object is marked invalid", raw: `{"path": "ma`, wantInvalid: true, wantErrPart: "unexpected end of JSON input"},
		{name: "array is marked invalid", raw: `["main.go"]`, wantInvalid: true, wantErrPart: "cannot unmarshal array"},
		{name: "string is marked invalid", raw: `"main.go"`, wantInvalid: true, wantErrPart: "cannot unmarshal string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			ev := toolCallEvent("call_1", "read_file", tc.raw)
			must.Equal("tool_call", ev.Type)
			must.Equal("call_1", ev.ID)
			must.Equal("read_file", ev.Name)

			if !tc.wantInvalid {
				must.Nil(ev.InvalidInput, "valid arguments should not be marked")
				must.Len(ev.Input, len(tc.wantInput))
				if tc.wantInput != nil {
					must.Equal(tc.wantInput["path"], ev.Input["path"])
				}
				return
			}

			must.NotNil(ev.InvalidInput, "malformed arguments should be marked")
			must.Nil(ev.Input, "a marked tool_call should carry no input")
			must.Equal(tc.raw, ev.InvalidInput.Raw)
			must.Contains(ev.InvalidInput.Error, tc.wantErrPart)
		})
	}
}
