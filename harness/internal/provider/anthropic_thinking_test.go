package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// TestAnthropicAdapter_StreamThinkingBlock pins capture of a thinking block
// ahead of the text and tool call it precedes: each text delta is forwarded
// as it arrives, and the whole block, signature included, is emitted as
// one event when it stops.
func TestAnthropicAdapter_StreamThinkingBlock(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":"Let me "}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":"check."}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"signature_delta","signature":"EqQBCk"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"signature_delta","signature":"YIBRgC"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("content_block_start", `{"index":1,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"index":1,"delta":{"type":"text_delta","text":"Reading."}}`),
		makeSSE("content_block_stop", `{"index":1}`),
		makeSSE("content_block_start", `{"index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{},"caller":{"type":"direct"}}}`),
		makeSSE("content_block_delta", `{"index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"main.go\"}"}}`),
		makeSSE("content_block_stop", `{"index":2}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`),
		makeSSE("message_stop", `{}`),
	)

	events := streamAnthropicSSE(t, body)

	wantTypes := []string{"thinking_delta", "thinking_delta", "thinking", "text_delta", "tool_call", "message_complete"}
	must.Len(events, len(wantTypes), "events: %+v", events)
	check.Equal(wantTypes, eventTypes(events))

	check.Equal("Let me ", events[0].Text, "first thinking delta")
	check.Equal("check.", events[1].Text, "second thinking delta")
	check.Empty(events[0].ThoughtSignature, "a thinking_delta carried a signature; only the complete block may")
	check.Empty(events[1].ThoughtSignature, "a thinking_delta carried a signature; only the complete block may")
	check.Equal("Let me check.", events[2].Text, "complete thinking text")
	check.Equal("EqQBCkYIBRgC", events[2].ThoughtSignature, "complete thinking signature")
	check.Empty(events[4].ThoughtSignature, "only thinking events may carry a signature")
}

// TestAnthropicAdapter_StreamThinkingTextOnBlockStart pins that thinking
// text carried by the block start is forwarded as a delta like the text
// that follows it, so the deltas always add up to the complete block.
func TestAnthropicAdapter_StreamThinkingTextOnBlockStart(t *testing.T) {
	check := assert.New(t)
	events := streamAnthropicSSE(t, joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"thinking","thinking":"Start. ","signature":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":"Then."}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"signature_delta","signature":"sig"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	))

	var deltas strings.Builder
	var block string
	for _, ev := range events {
		switch ev.Type {
		case "thinking_delta":
			deltas.WriteString(ev.Text)
		case "thinking":
			block = ev.Text
		}
	}
	check.Equal("Start. Then.", deltas.String(), "thinking deltas; events %v", eventTypes(events))
	check.Equal("Start. Then.", block, "complete thinking block")
}

// TestAnthropicAdapter_StreamInterleavedThinking pins event order when
// thinking blocks follow text and tool_use blocks in one response: each
// is emitted at its own block stop, between the events around it.
func TestAnthropicAdapter_StreamInterleavedThinking(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":"first"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"signature_delta","signature":"sig-A"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("content_block_start", `{"index":1,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"index":1,"delta":{"type":"text_delta","text":"Reading."}}`),
		makeSSE("content_block_stop", `{"index":1}`),
		makeSSE("content_block_start", `{"index":2,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		makeSSE("content_block_delta", `{"index":2,"delta":{"type":"signature_delta","signature":"sig-"}}`),
		makeSSE("content_block_delta", `{"index":2,"delta":{"type":"signature_delta","signature":"B"}}`),
		makeSSE("content_block_stop", `{"index":2}`),
		makeSSE("content_block_start", `{"index":3,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}`),
		makeSSE("content_block_delta", `{"index":3,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a\"}"}}`),
		makeSSE("content_block_stop", `{"index":3}`),
		makeSSE("content_block_start", `{"index":4,"content_block":{"type":"thinking","thinking":"","signature":"sig-C"}}`),
		makeSSE("content_block_stop", `{"index":4}`),
		makeSSE("content_block_start", `{"index":5,"content_block":{"type":"tool_use","id":"toolu_2","name":"read_file","input":{}}}`),
		makeSSE("content_block_delta", `{"index":5,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"b\"}"}}`),
		makeSSE("content_block_stop", `{"index":5}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"tool_use"}}`),
		makeSSE("message_stop", `{}`),
	)

	events := streamAnthropicSSE(t, body)

	want := []types.StreamEvent{
		{Type: "thinking_delta", Text: "first"},
		{Type: "thinking", Text: "first", ThoughtSignature: "sig-A"},
		{Type: "text_delta", Text: "Reading."},
		{Type: "thinking", ThoughtSignature: "sig-B"},
		{Type: "tool_call", ID: "toolu_1"},
		{Type: "thinking", ThoughtSignature: "sig-C"},
		{Type: "tool_call", ID: "toolu_2"},
		{Type: "message_complete"},
	}
	must.Len(events, len(want), "event types %v", eventTypes(events))
	for i, w := range want {
		got := events[i]
		check.Equal(w.Type, got.Type, "event %d type", i)
		check.Equal(w.Text, got.Text, "event %d text", i)
		check.Equal(w.ThoughtSignature, got.ThoughtSignature, "event %d signature", i)
		check.Equal(w.ID, got.ID, "event %d id", i)
	}
}

// TestAnthropicAdapter_RedactedThinkingEmptyData pins both halves of an
// empty redacted block: capture emits it with empty data, and egress never
// resends it, since the API cannot verify an empty payload.
func TestAnthropicAdapter_RedactedThinkingEmptyData(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	events := streamAnthropicSSE(t, joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"redacted_thinking","data":""}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	))
	must.Len(events, 2, "events %+v: want redacted_thinking with empty data, then message_complete", events)
	must.Equal("redacted_thinking", events[0].Type)
	must.Empty(events[0].ThoughtSignature)

	history := thinkingReplayHistory()
	history[1].Content = []types.ContentBlock{
		{Type: "redacted_thinking"},
		{Type: "text", Text: "Reading it."},
		{Type: "tool_use", ID: "toolu_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
	}
	q := quirks.DefaultRegistry().Resolve("anthropic", "claude-sonnet-5-5")
	body, err := json.Marshal(buildAnthropicRequest(types.StreamParams{Model: "claude-sonnet-5-5", MaxTokens: 16, Messages: history}, true, q))
	must.NoError(err)
	for _, raw := range assistantWireBlocks(t, body, 1) {
		check.NotContains(string(raw), `"redacted_thinking"`, "empty redacted_thinking block reached the wire")
	}
}

// TestAnthropicAdapter_StreamThinkingOmittedText covers the default display
// on 5.x models: no thinking_delta arrives and only the signature carries
// the content.
func TestAnthropicAdapter_StreamThinkingOmittedText(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"signature_delta","signature":"sig-omitted"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	)

	events := streamAnthropicSSE(t, body)

	must.Len(events, 2, "events: %+v", events)
	check.Equal("thinking", events[0].Type)
	check.Empty(events[0].Text)
	check.Equal("sig-omitted", events[0].ThoughtSignature)
}

// TestAnthropicAdapter_StreamRedactedThinking pins that a redacted block's
// data, which arrives whole on content_block_start, is emitted verbatim.
func TestAnthropicAdapter_StreamRedactedThinking(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	body := joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"redacted_thinking","data":"EmwKAhgBEgy3va"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("content_block_start", `{"index":1,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"index":1,"delta":{"type":"text_delta","text":"Done."}}`),
		makeSSE("content_block_stop", `{"index":1}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	)

	events := streamAnthropicSSE(t, body)

	must.Len(events, 3, "events: %+v", events)
	check.Equal("redacted_thinking", events[0].Type)
	check.Equal("EmwKAhgBEgy3va", events[0].ThoughtSignature)
	check.Empty(events[0].Text)
}

// jsonString returns n bytes of filler as a JSON string literal.
func jsonString(t *testing.T, n int) string {
	t.Helper()
	must := require.New(t)
	b, err := json.Marshal(strings.Repeat("a", n))
	must.NoError(err)
	return string(b)
}

func eventTypes(events []types.StreamEvent) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.Type
	}
	return out
}

// TestAnthropicAdapter_ThinkingSizeCap pins the response-wide cap on
// thinking text plus signatures: a response exactly at the cap streams
// normally, and every route past it (a delta, a block start, a redacted
// block, or the sum over several blocks) ends the stream with one error.
func TestAnthropicAdapter_ThinkingSizeCap(t *testing.T) {
	const capBytes = maxThinkingResponseSize
	half := capBytes / 2
	thinkingStart := makeSSE("content_block_start", `{"index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`)
	delta := func(index int, kind, field string, n int) string {
		return makeSSE("content_block_delta", fmt.Sprintf(`{"index":%d,"delta":{"type":%q,%q:%s}}`, index, kind, field, jsonString(t, n)))
	}
	end := joinLines(
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	)

	overflows := map[string]string{
		"text and signature counted together": joinLines(
			thinkingStart,
			delta(0, "thinking_delta", "thinking", half+1),
			delta(0, "signature_delta", "signature", half),
		),
		"signature_delta": joinLines(
			thinkingStart,
			delta(0, "signature_delta", "signature", capBytes+1),
		),
		"thinking on block start": makeSSE("content_block_start",
			fmt.Sprintf(`{"index":0,"content_block":{"type":"thinking","thinking":%s,"signature":""}}`, jsonString(t, capBytes+1))),
		"redacted_thinking on block start": makeSSE("content_block_start",
			fmt.Sprintf(`{"index":0,"content_block":{"type":"redacted_thinking","data":%s}}`, jsonString(t, capBytes+1))),
		"sum over two blocks": joinLines(
			thinkingStart,
			delta(0, "signature_delta", "signature", half+1),
			makeSSE("content_block_stop", `{"index":0}`),
			makeSSE("content_block_start", `{"index":1,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
			delta(1, "signature_delta", "signature", half),
		),
	}
	for name, body := range overflows {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			events := streamAnthropicSSE(t, joinLines(body, makeSSE("content_block_stop", `{"index":0}`), end))
			last := events[len(events)-1]
			must.Equal("error", last.Type, "event types %v: stream should end on the thinking size-cap error", eventTypes(events))
			must.ErrorContains(last.Error, "thinking blocks exceed")

			earlier := eventTypes(events[:len(events)-1])
			check.NotContains(earlier, "error", "event types %v: want exactly one error, ending the stream", eventTypes(events))
			check.NotContains(earlier, "message_complete", "event types %v: want exactly one error, ending the stream", eventTypes(events))
		})
	}

	t.Run("exactly at the cap", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		events := streamAnthropicSSE(t, joinLines(
			thinkingStart,
			delta(0, "thinking_delta", "thinking", half),
			delta(0, "signature_delta", "signature", capBytes-half),
			makeSSE("content_block_stop", `{"index":0}`),
			end,
		))
		must.Equal([]string{"thinking_delta", "thinking", "message_complete"}, eventTypes(events))
		check.Equal(capBytes, len(events[1].Text)+len(events[1].ThoughtSignature), "thinking event bytes")
	})
}

// TestAnthropicAdapter_OpenContentBlockCap pins the bound on content blocks
// started and not yet stopped, while sequential blocks stay unbounded.
func TestAnthropicAdapter_OpenContentBlockCap(t *testing.T) {
	must := require.New(t)
	var open, sequential []string
	for i := 0; i <= maxOpenContentBlocks; i++ {
		start := makeSSE("content_block_start", fmt.Sprintf(`{"index":%d,"content_block":{"type":"text","text":""}}`, i))
		open = append(open, start)
		sequential = append(sequential, start, makeSSE("content_block_stop", fmt.Sprintf(`{"index":%d}`, i)))
	}
	end := joinLines(
		makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
		makeSSE("message_stop", `{}`),
	)

	events := streamAnthropicSSE(t, joinLines(joinLines(open...), end))
	must.Len(events, 1, "event types %v", eventTypes(events))
	must.Equal("error", events[0].Type)
	must.ErrorContains(events[0].Error, "content blocks open at once")

	events = streamAnthropicSSE(t, joinLines(joinLines(sequential...), end))
	must.Equal([]string{"message_complete"}, eventTypes(events), "sequential blocks")
}
