package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

const invalidArgsSecret = "sk-ant-api03-INVALIDARGSSENTINEL0123456789"

func TestInvalidArgsMessage_QuotesBoundedScrubbedPrefix(t *testing.T) {
	t.Run("short arguments are quoted in full and scrubbed", func(t *testing.T) {
		must := require.New(t)
		inv := &types.InvalidToolInput{
			Raw:   `{"path": "main.go", "token": "` + invalidArgsSecret + `",}`,
			Error: "invalid character '}' looking for beginning of object key string (at byte offset 72)",
		}
		msg := invalidArgsMessage("read_file", inv)
		must.True(strings.HasPrefix(msg, "Your arguments for `read_file` were not valid JSON: `invalid character '}' looking for beginning of object key string (at byte offset 72)`. Re-issue the call with valid JSON."),
			"message does not start with the fixed format: %q", msg)
		must.NotContains(msg, invalidArgsSecret)
		must.Contains(msg, "Arguments received: {\"path\": \"main.go\"")
	})

	t.Run("long arguments are truncated to the quote bound", func(t *testing.T) {
		must := require.New(t)
		long := &types.InvalidToolInput{Raw: `{"content": "` + strings.Repeat("é", 500) + `"`, Error: "unexpected end of JSON input"}
		msg := invalidArgsMessage("write_file", long)
		label := fmt.Sprintf("First %d characters of the arguments received: ", invalidArgsQuoteRunes)
		_, quoted, ok := strings.Cut(msg, label)
		must.True(ok, "long arguments message lacks the truncation label: %q", msg)
		must.Equal(invalidArgsQuoteRunes, utf8.RuneCountInString(quoted))
		must.True(utf8.ValidString(quoted), "quoted prefix is not valid UTF-8: %q", quoted)
	})
}

func TestInvalidArgsTraceReason_BoundedAndScrubbed(t *testing.T) {
	raw := `{"token": "` + invalidArgsSecret + `", "pad": "` + strings.Repeat("x", 2*invalidArgsTraceBytes) + `"`
	reason := invalidArgsTraceReason("read_file", &types.InvalidToolInput{Raw: raw, Error: "unexpected end of JSON input"})

	t.Run("scrubs the secret", func(t *testing.T) {
		must := require.New(t)
		must.NotContains(reason, invalidArgsSecret)
	})

	t.Run("states the raw byte count", func(t *testing.T) {
		must := require.New(t)
		must.Contains(reason, fmt.Sprintf("raw arguments (%d bytes): ", len(raw)))
	})

	t.Run("marks oversized arguments as truncated", func(t *testing.T) {
		must := require.New(t)
		must.True(strings.HasSuffix(reason, asyncResultTruncationSuffix), "trace reason lacks the truncation suffix")
	})

	t.Run("stays near the trace byte bound", func(t *testing.T) {
		must := require.New(t)
		must.LessOrEqual(len(reason), invalidArgsTraceBytes+512, "trace reason should be bounded near %d bytes", invalidArgsTraceBytes)
	})
}

func TestTruncateBytes_BacksOffToRuneBoundary(t *testing.T) {
	t.Run("backs off to a rune boundary", func(t *testing.T) {
		must := require.New(t)
		got, cut := truncateBytes("aé", 2)
		must.True(cut)
		must.Equal("a", got)
	})

	t.Run("returns input unchanged when it fits", func(t *testing.T) {
		must := require.New(t)
		got, cut := truncateBytes("abc", 3)
		must.False(cut)
		must.Equal("abc", got)
	})
}

func TestStallInput_UsesRawArgumentsForMarkedCalls(t *testing.T) {
	t.Run("valid call uses its input", func(t *testing.T) {
		must := require.New(t)
		valid := types.ToolCall{Name: "read_file", Input: json.RawMessage(`{}`)}
		must.Equal("{}", string(stallInput(valid)))
	})

	t.Run("marked call uses the raw arguments", func(t *testing.T) {
		must := require.New(t)
		marked := types.ToolCall{Name: "read_file", Input: json.RawMessage(`{}`), InvalidInput: &types.InvalidToolInput{Raw: `{"path":`}}
		must.Equal(`{"path":`, string(stallInput(marked)))
	})
}

func TestStreamEventsToResult_MarkedToolCallKeepsHistoryValid(t *testing.T) {
	must := require.New(t)
	inv := &types.InvalidToolInput{Raw: `{"path": "a",}`, Error: "invalid character '}' (at byte offset 14)"}
	ch := make(chan types.StreamEvent, 4)
	ch <- types.StreamEvent{Type: "text_delta", Text: "reading"}
	ch <- types.StreamEvent{Type: "tool_call", ID: "call_bad", Name: "read_file", InvalidInput: inv}
	ch <- types.StreamEvent{Type: "tool_call", ID: "call_ok", Name: "read_file", Input: map[string]any{"path": "b"}}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: "tool_use"}
	close(ch)

	tp := &recordingTransport{}
	sr, err := streamEventsToResult(context.Background(), ch, tp, true, ignoreEmitFailure, nil)
	must.NoError(err, "streamEventsToResult")

	must.Len(sr.Blocks, 3, "want text + 2 tool_use blocks: %+v", sr.Blocks)
	must.Equal("{}", string(sr.Blocks[1].Input))
	must.Len(sr.InvalidToolInputs, 1)
	must.Same(inv, sr.InvalidToolInputs[1], "the marker should sit at block index 1")

	calls := collectToolCalls(sr.Blocks, sr.InvalidToolInputs)
	must.Len(calls, 2)
	must.Same(inv, calls[0].InvalidInput)
	must.Equal("{}", string(calls[0].Input))
	must.Nil(calls[1].InvalidInput, "the second call should not carry a marker")

	var sawToolCall, sawWarning bool
	for _, e := range tp.events {
		switch {
		case e.Type == "tool_call" && e.ID == "call_bad":
			sawToolCall = true
			must.Equal("{}", string(e.Input), "transport tool_call input")
		case e.Type == "warning" && strings.Contains(e.Message, "call_bad"):
			sawWarning = true
			must.Contains(e.Message, "not valid JSON", "warning should say the arguments were not valid JSON")
		}
	}
	must.True(sawToolCall, "transport events = %+v, want a tool_call for call_bad", tp.events)
	must.True(sawWarning, "transport events = %+v, want a warning for call_bad", tp.events)
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
		{name: "distinct arguments", raw: func(i int) string { return fmt.Sprintf(`{"attempt": %d`, i) }, wantOutcome: "tool_failures", wantTurns: 5},
		{name: "identical arguments", raw: func(int) string { return `{"attempt":` }, wantOutcome: "tool_failures", wantTurns: 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			prov := &multiCallProvider{calls: malformedTurns(tc.raw)}
			loop := buildTestLoop(&mockProvider{})
			loop.Provider = prov

			runTrace, err := loop.Run(context.Background(), buildTestConfig())
			must.NoError(err, "Run")
			must.Equal(tc.wantOutcome, runTrace.Outcome)
			must.Equal(tc.wantTurns, prov.idx, "provider call count")
			for _, c := range runTrace.ToolCalls {
				must.Equal(string(observability.ToolFailureInvalidArguments), c.ErrorCategory, "tool call %s", c.ID)
			}
		})
	}
}
