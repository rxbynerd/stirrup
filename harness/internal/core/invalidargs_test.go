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
		inv := &types.InvalidToolInput{
			Raw:   `{"path": "main.go", "token": "` + invalidArgsSecret + `",}`,
			Error: "invalid character '}' looking for beginning of object key string (at byte offset 72)",
		}
		msg := invalidArgsMessage("read_file", inv)
		require.True(t, strings.HasPrefix(msg, "Your arguments for `read_file` were not valid JSON: `invalid character '}' looking for beginning of object key string (at byte offset 72)`. Re-issue the call with valid JSON."),
			"message does not start with the fixed format: %q", msg)
		require.NotContains(t, msg, invalidArgsSecret)
		require.Contains(t, msg, "Arguments received: {\"path\": \"main.go\"")
	})

	t.Run("long arguments are truncated to the quote bound", func(t *testing.T) {
		long := &types.InvalidToolInput{Raw: `{"content": "` + strings.Repeat("é", 500) + `"`, Error: "unexpected end of JSON input"}
		msg := invalidArgsMessage("write_file", long)
		label := fmt.Sprintf("First %d characters of the arguments received: ", invalidArgsQuoteRunes)
		_, quoted, ok := strings.Cut(msg, label)
		require.True(t, ok, "long arguments message lacks the truncation label: %q", msg)
		require.Equal(t, invalidArgsQuoteRunes, utf8.RuneCountInString(quoted))
		require.True(t, utf8.ValidString(quoted), "quoted prefix is not valid UTF-8: %q", quoted)
	})
}

func TestInvalidArgsTraceReason_BoundedAndScrubbed(t *testing.T) {
	raw := `{"token": "` + invalidArgsSecret + `", "pad": "` + strings.Repeat("x", 2*invalidArgsTraceBytes) + `"`
	reason := invalidArgsTraceReason("read_file", &types.InvalidToolInput{Raw: raw, Error: "unexpected end of JSON input"})

	t.Run("scrubs the secret", func(t *testing.T) {
		require.NotContains(t, reason, invalidArgsSecret)
	})

	t.Run("states the raw byte count", func(t *testing.T) {
		require.Contains(t, reason, fmt.Sprintf("raw arguments (%d bytes): ", len(raw)))
	})

	t.Run("marks oversized arguments as truncated", func(t *testing.T) {
		require.True(t, strings.HasSuffix(reason, asyncResultTruncationSuffix), "trace reason lacks the truncation suffix")
	})

	t.Run("stays near the trace byte bound", func(t *testing.T) {
		require.LessOrEqual(t, len(reason), invalidArgsTraceBytes+512, "trace reason should be bounded near %d bytes", invalidArgsTraceBytes)
	})
}

func TestTruncateBytes_BacksOffToRuneBoundary(t *testing.T) {
	t.Run("backs off to a rune boundary", func(t *testing.T) {
		got, cut := truncateBytes("aé", 2)
		require.True(t, cut)
		require.Equal(t, "a", got)
	})

	t.Run("returns input unchanged when it fits", func(t *testing.T) {
		got, cut := truncateBytes("abc", 3)
		require.False(t, cut)
		require.Equal(t, "abc", got)
	})
}

func TestStallInput_UsesRawArgumentsForMarkedCalls(t *testing.T) {
	t.Run("valid call uses its input", func(t *testing.T) {
		valid := types.ToolCall{Name: "read_file", Input: json.RawMessage(`{}`)}
		require.Equal(t, "{}", string(stallInput(valid)))
	})

	t.Run("marked call uses the raw arguments", func(t *testing.T) {
		marked := types.ToolCall{Name: "read_file", Input: json.RawMessage(`{}`), InvalidInput: &types.InvalidToolInput{Raw: `{"path":`}}
		require.Equal(t, `{"path":`, string(stallInput(marked)))
	})
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
	sr, err := streamEventsToResult(context.Background(), ch, tp, true, ignoreEmitFailure, nil)
	require.NoError(t, err, "streamEventsToResult")

	require.Len(t, sr.Blocks, 3, "want text + 2 tool_use blocks: %+v", sr.Blocks)
	require.Equal(t, "{}", string(sr.Blocks[1].Input))
	require.Len(t, sr.InvalidToolInputs, 1)
	require.Same(t, inv, sr.InvalidToolInputs[1], "the marker should sit at block index 1")

	calls := collectToolCalls(sr.Blocks, sr.InvalidToolInputs)
	require.Len(t, calls, 2)
	require.Same(t, inv, calls[0].InvalidInput)
	require.Equal(t, "{}", string(calls[0].Input))
	require.Nil(t, calls[1].InvalidInput, "the second call should not carry a marker")

	var sawToolCall, sawWarning bool
	for _, e := range tp.events {
		switch {
		case e.Type == "tool_call" && e.ID == "call_bad":
			sawToolCall = true
			require.Equal(t, "{}", string(e.Input), "transport tool_call input")
		case e.Type == "warning" && strings.Contains(e.Message, "call_bad"):
			sawWarning = true
			require.Contains(t, e.Message, "not valid JSON", "warning should say the arguments were not valid JSON")
		}
	}
	require.True(t, sawToolCall, "transport events = %+v, want a tool_call for call_bad", tp.events)
	require.True(t, sawWarning, "transport events = %+v, want a warning for call_bad", tp.events)
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
			prov := &multiCallProvider{calls: malformedTurns(tc.raw)}
			loop := buildTestLoop(&mockProvider{})
			loop.Provider = prov

			runTrace, err := loop.Run(context.Background(), buildTestConfig())
			require.NoError(t, err, "Run")
			require.Equal(t, tc.wantOutcome, runTrace.Outcome)
			require.Equal(t, tc.wantTurns, prov.idx, "provider call count")
			for _, c := range runTrace.ToolCalls {
				require.Equal(t, string(observability.ToolFailureInvalidArguments), c.ErrorCategory, "tool call %s", c.ID)
			}
		})
	}
}
