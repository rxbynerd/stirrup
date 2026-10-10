package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

const invalidArgsSecret = "sk-ant-api03-INVALIDARGSSENTINEL0123456789"

func TestInvalidArgsMessage_QuotesBoundedScrubbedPrefix(t *testing.T) {
	inv := &types.InvalidToolInput{
		Raw:   `{"path": "main.go", "token": "` + invalidArgsSecret + `",}`,
		Error: "invalid character '}' looking for beginning of object key string (at byte offset 72)",
	}
	msg := invalidArgsMessage("read_file", inv)
	if !strings.HasPrefix(msg, "Your arguments for `read_file` were not valid JSON: `invalid character '}' looking for beginning of object key string (at byte offset 72)`. Re-issue the call with valid JSON.") {
		t.Errorf("message does not start with the fixed format: %q", msg)
	}
	if strings.Contains(msg, invalidArgsSecret) {
		t.Errorf("message quotes the secret unscrubbed: %q", msg)
	}
	if !strings.Contains(msg, "Arguments received: {\"path\": \"main.go\"") {
		t.Errorf("message does not quote the short arguments in full: %q", msg)
	}

	long := &types.InvalidToolInput{Raw: `{"content": "` + strings.Repeat("é", 500) + `"`, Error: "unexpected end of JSON input"}
	msg = invalidArgsMessage("write_file", long)
	label := fmt.Sprintf("First %d characters of the arguments received: ", invalidArgsQuoteRunes)
	_, quoted, ok := strings.Cut(msg, label)
	if !ok {
		t.Fatalf("long arguments message lacks the truncation label: %q", msg)
	}
	if n := utf8.RuneCountInString(quoted); n != invalidArgsQuoteRunes {
		t.Errorf("quoted %d runes, want %d", n, invalidArgsQuoteRunes)
	}
	if !utf8.ValidString(quoted) {
		t.Errorf("quoted prefix is not valid UTF-8: %q", quoted)
	}
}

func TestInvalidArgsTraceReason_BoundedAndScrubbed(t *testing.T) {
	raw := `{"token": "` + invalidArgsSecret + `", "pad": "` + strings.Repeat("x", 2*invalidArgsTraceBytes) + `"`
	reason := invalidArgsTraceReason("read_file", &types.InvalidToolInput{Raw: raw, Error: "unexpected end of JSON input"})
	if strings.Contains(reason, invalidArgsSecret) {
		t.Errorf("trace reason carries the secret unscrubbed")
	}
	if !strings.Contains(reason, fmt.Sprintf("raw arguments (%d bytes): ", len(raw))) {
		t.Errorf("trace reason lacks the raw byte count: %.200q", reason)
	}
	if !strings.HasSuffix(reason, asyncResultTruncationSuffix) {
		t.Errorf("trace reason for oversized arguments lacks the truncation suffix")
	}
	if len(reason) > invalidArgsTraceBytes+512 {
		t.Errorf("trace reason is %d bytes, want it bounded near %d", len(reason), invalidArgsTraceBytes)
	}
}

func TestTruncateBytes_BacksOffToRuneBoundary(t *testing.T) {
	got, cut := truncateBytes("aé", 2)
	if !cut || got != "a" {
		t.Errorf("truncateBytes(\"aé\", 2) = %q, %v; want \"a\", true", got, cut)
	}
	got, cut = truncateBytes("abc", 3)
	if cut || got != "abc" {
		t.Errorf("truncateBytes(\"abc\", 3) = %q, %v; want \"abc\", false", got, cut)
	}
}

func TestStallInput_UsesRawArgumentsForMarkedCalls(t *testing.T) {
	valid := types.ToolCall{Name: "read_file", Input: json.RawMessage(`{}`)}
	if got := string(stallInput(valid)); got != "{}" {
		t.Errorf("stallInput(valid) = %q, want {}", got)
	}
	marked := types.ToolCall{Name: "read_file", Input: json.RawMessage(`{}`), InvalidInput: &types.InvalidToolInput{Raw: `{"path":`}}
	if got := string(stallInput(marked)); got != `{"path":` {
		t.Errorf("stallInput(marked) = %q, want the raw arguments", got)
	}
}

func TestStreamEventsToResult_MarkedToolCallKeepsHistoryValid(t *testing.T) {
	inv := &types.InvalidToolInput{Raw: `{"path": "a",}`, Error: "invalid character '}' (at byte offset 14)"}
	ch := make(chan types.StreamEvent, 4)
	ch <- types.StreamEvent{Type: "text_delta", Text: "reading"}
	ch <- types.StreamEvent{Type: "tool_call", ID: "call_bad", Name: "read_file", InvalidInput: inv}
	ch <- types.StreamEvent{Type: "tool_call", ID: "call_ok", Name: "read_file", Input: map[string]any{"path": "b"}}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: "tool_use"}
	close(ch)

	tp := &recordingTransport{}
	sr, err := streamEventsToResult(context.Background(), ch, tp, ignoreEmitFailure)
	if err != nil {
		t.Fatalf("streamEventsToResult: %v", err)
	}
	if len(sr.Blocks) != 3 {
		t.Fatalf("got %d blocks, want text + 2 tool_use: %+v", len(sr.Blocks), sr.Blocks)
	}
	if got := string(sr.Blocks[1].Input); got != "{}" {
		t.Errorf("marked block Input = %q, want {}", got)
	}
	if len(sr.InvalidToolInputs) != 1 || sr.InvalidToolInputs[1] != inv {
		t.Errorf("InvalidToolInputs = %+v, want the marker at block index 1", sr.InvalidToolInputs)
	}

	calls := collectToolCalls(sr.Blocks, sr.InvalidToolInputs)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].InvalidInput != inv || string(calls[0].Input) != "{}" {
		t.Errorf("first call = %+v, want the marker and an empty-object input", calls[0])
	}
	if calls[1].InvalidInput != nil {
		t.Errorf("second call carries a marker: %+v", calls[1].InvalidInput)
	}

	var sawToolCall, sawWarning bool
	for _, e := range tp.events {
		switch {
		case e.Type == "tool_call" && e.ID == "call_bad":
			sawToolCall = true
			if string(e.Input) != "{}" {
				t.Errorf("transport tool_call input = %q, want {}", e.Input)
			}
		case e.Type == "warning" && strings.Contains(e.Message, "call_bad"):
			sawWarning = true
			if !strings.Contains(e.Message, "not valid JSON") {
				t.Errorf("warning = %q, want it to say the arguments were not valid JSON", e.Message)
			}
		}
	}
	if !sawToolCall || !sawWarning {
		t.Errorf("transport events = %+v, want a tool_call and a warning for call_bad", tp.events)
	}
}

// TestLoop_RepeatedMalformedToolArgumentsEndRun pins that the stall
// detector bounds a model that keeps sending malformed arguments: five
// distinct malformed calls end the run with tool_failures, and three
// identical ones end it as stalled.
func TestLoop_RepeatedMalformedToolArgumentsEndRun(t *testing.T) {
	malformedTurns := func(raw func(i int) string) [][]types.StreamEvent {
		turns := make([][]types.StreamEvent, 6)
		for i := range turns {
			turns[i] = []types.StreamEvent{
				{Type: "tool_call", ID: fmt.Sprintf("tc_bad_%d", i), Name: "test_tool", InvalidInput: &types.InvalidToolInput{Raw: raw(i), Error: "unexpected end of JSON input"}},
				{Type: "message_complete", StopReason: "tool_use"},
			}
		}
		return turns
	}

	cases := []struct {
		name        string
		raw         func(i int) string
		wantOutcome string
		wantTurns   int
	}{
		{name: "distinct", raw: func(i int) string { return fmt.Sprintf(`{"attempt": %d`, i) }, wantOutcome: "tool_failures", wantTurns: 5},
		{name: "identical", raw: func(int) string { return `{"attempt":` }, wantOutcome: "stalled", wantTurns: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &multiCallProvider{calls: malformedTurns(tc.raw)}
			loop := buildTestLoop(&mockProvider{})
			loop.Provider = prov

			runTrace, err := loop.Run(context.Background(), buildTestConfig())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if runTrace.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %q, want %q", runTrace.Outcome, tc.wantOutcome)
			}
			if prov.idx != tc.wantTurns {
				t.Errorf("provider called %d times, want %d", prov.idx, tc.wantTurns)
			}
			for _, c := range runTrace.ToolCalls {
				if c.ErrorCategory != string(observability.ToolFailureInvalidArguments) {
					t.Errorf("tool call %s category = %q, want invalid_arguments", c.ID, c.ErrorCategory)
				}
			}
		})
	}
}
