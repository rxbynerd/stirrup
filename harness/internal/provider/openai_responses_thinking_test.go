package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

func TestResponsesRequest_ReasoningObjectSummary(t *testing.T) {
	cases := []struct {
		name, model, effort, summary string
		want                         string // "" means no reasoning object
	}{
		{"unset sends effort only", "gpt-5.6-sol", "high", "", `{"effort":"high"}`},
		{"auto asks for a summary", "gpt-5.6-sol", "low", types.ReasoningSummaryAuto, `{"effort":"low","summary":"auto"}`},
		{"off sends effort only", "gpt-5.6-sol", "high", types.ReasoningSummaryOff, `{"effort":"high"}`},
		{"no effort sends no reasoning object", "gpt-5.6-sol", "", types.ReasoningSummaryAuto, ""},
		{"dropped effort sends no reasoning object", "gpt-4.1", "high", types.ReasoningSummaryAuto, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := effortParams(tc.model, tc.effort, false)
			params.ReasoningSummary = tc.summary
			q := quirks.DefaultRegistry().Resolve("openai-responses", tc.model)
			req, err := buildResponsesRequest(params, q, nil, "")
			require.NoError(t, err, "build request")
			body, err := json.Marshal(req)
			require.NoError(t, err, "marshal request")
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(body, &fields), "decode request")

			got, ok := fields["reasoning"]
			if tc.want == "" {
				require.False(t, ok, "reasoning = %s, want absent", got)
				return
			}
			require.True(t, ok, "reasoning is absent, want %s", tc.want)
			require.JSONEq(t, tc.want, string(got))

			var round responsesRequest
			require.NoError(t, json.Unmarshal(body, &round), "round trip")
			require.Equal(t, req.ReasoningSummary, round.ReasoningSummary, "round-tripped ReasoningSummary")
		})
	}
}

// responsesSummaryTurn is a documented-shape tool-calling turn whose
// reasoning item carries a summary as well as encrypted_content.
func responsesSummaryTurn() []byte {
	reasoning := `{"type":"reasoning","id":"rs_s1","summary":[{"type":"summary_text","text":"**Plan**\n\nRead a.go first."}],"encrypted_content":"gAAAA-placeholder"}`
	call := `{"type":"function_call","id":"fc_s1","call_id":"call_s1","name":"read_file","arguments":"{\"path\":\"a.go\"}","status":"completed"}`
	return []byte(strings.Join([]string{
		makeResponsesEvent("response.created", `{"response":{"id":"resp_s1","status":"in_progress","model":"gpt-5.6-sol","output":[]}}`),
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"reasoning","id":"rs_s1","summary":[]}}`),
		makeResponsesEvent("response.reasoning_summary_part.added", `{"item_id":"rs_s1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`),
		makeResponsesEvent("response.reasoning_summary_text.delta", `{"item_id":"rs_s1","output_index":0,"summary_index":0,"delta":"**Plan**\n\n"}`),
		makeResponsesEvent("response.reasoning_summary_text.delta", `{"item_id":"rs_s1","output_index":0,"summary_index":0,"delta":"Read a.go first."}`),
		makeResponsesEvent("response.reasoning_summary_text.done", `{"item_id":"rs_s1","output_index":0,"summary_index":0,"text":"**Plan**\n\nRead a.go first."}`),
		makeResponsesEvent("response.reasoning_summary_part.done", `{"item_id":"rs_s1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"**Plan**\n\nRead a.go first."}}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":`+reasoning+`}`),
		makeResponsesEvent("response.output_item.added", `{"output_index":1,"item":{"type":"function_call","id":"fc_s1","call_id":"call_s1","name":"read_file","arguments":"","status":"in_progress"}}`),
		makeResponsesEvent("response.function_call_arguments.done", `{"item_id":"fc_s1","output_index":1,"arguments":"{\"path\":\"a.go\"}"}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":`+call+`}`),
		makeResponsesEvent("response.completed", `{"response":{"id":"resp_s1","status":"completed","model":"gpt-5.6-sol","output":[`+reasoning+`,`+call+`]}}`),
	}, ""))
}

// Summary deltas stream as thinking_delta only, and the reasoning item
// that carries the summary is replayed verbatim on the next turn.
func TestOpenAIResponsesAdapter_ReasoningSummary_StreamsAndReplays(t *testing.T) {
	script := newResponsesScript(t, responsesSummaryTurn())
	adapter := script.adapter(nil)

	params := effortParams(responsesReplayModel, "high", true)
	params.ReasoningSummary = types.ReasoningSummaryAuto
	ch, err := adapter.Stream(context.Background(), params)
	require.NoError(t, err, "Stream")
	events := drainStream(t, ch)

	thinking, text := splitDeltas(events)
	require.Equal(t, "**Plan**\n\n|Read a.go first.", strings.Join(thinking, "|"), "thinking_delta fragments")
	require.Empty(t, text, "answer text")
	for _, ev := range events {
		if ev.Type == "tool_call" {
			require.NotContains(t, string(mustJSON(t, ev.Input)), "Plan", "summary text leaked into tool input")
		}
	}
	body := string(script.lastBody(t))
	require.Contains(t, body, `"reasoning":{"effort":"high","summary":"auto"}`, "first request does not ask for a summary")

	stored := storedReplayItems(t, messageComplete(t, events))
	require.Len(t, stored, 2, "stored items")
	require.Contains(t, string(stored[0]), `"summary_text"`, "the stored reasoning item should carry its summary")

	assistant := assistantFromEvents(events)
	results := types.Message{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "call_s1", Content: "package a"}}}
	params.Messages = append(params.Messages, assistant, results)
	ch, err = adapter.Stream(context.Background(), params)
	require.NoError(t, err, "second Stream")
	drainStream(t, ch)

	input := requestInput(t, script.lastBody(t))
	require.GreaterOrEqual(t, len(input), 3, "second request input = %s", input)
	require.JSONEq(t, string(stored[0]), string(input[1]), "second request should replay the stored reasoning item at index 1")
	assertCallsPaired(t, input)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err, "marshal")
	return b
}

// responsesSummaryDeltas is a text-only turn whose reasoning summary
// arrives as the given reasoning_summary_text.delta payloads.
func responsesSummaryDeltas(payloads ...string) []byte {
	events := []string{
		makeResponsesEvent("response.created", `{"response":{"id":"resp_p","status":"in_progress","model":"gpt-5.6-sol","output":[]}}`),
	}
	for _, p := range payloads {
		events = append(events, makeResponsesEvent("response.reasoning_summary_text.delta", p))
	}
	events = append(events,
		makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_p","output_index":1,"delta":"done"}`),
		makeResponsesEvent("response.completed", `{"response":{"id":"resp_p","status":"completed","model":"gpt-5.6-sol","output":[]}}`),
	)
	return []byte(strings.Join(events, ""))
}

func TestOpenAIResponsesAdapter_ReasoningSummary_PartsAreParagraphs(t *testing.T) {
	delta := func(item string, index int, text string) string {
		return fmt.Sprintf(`{"item_id":%q,"output_index":0,"summary_index":%d,"delta":%s}`, item, index, mustJSON(t, text))
	}
	cases := []struct {
		name     string
		payloads []string
		want     []string
	}{
		{
			name:     "a new part starts a new paragraph",
			payloads: []string{delta("rs_1", 0, "End of part"), delta("rs_1", 0, " one."), delta("rs_1", 1, "Start of part two.")},
			want:     []string{"End of part", " one.", "\n\nStart of part two."},
		},
		{
			name:     "a single part has no separator",
			payloads: []string{delta("rs_1", 0, "Only "), delta("rs_1", 0, "part.")},
			want:     []string{"Only ", "part."},
		},
		{
			name:     "a part of a later reasoning item starts a new paragraph",
			payloads: []string{delta("rs_1", 0, "First item."), delta("rs_2", 0, "Second item.")},
			want:     []string{"First item.", "\n\nSecond item."},
		},
		{
			name:     "malformed and empty deltas are dropped",
			payloads: []string{`{"delta":`, delta("rs_1", 0, ""), delta("rs_1", 0, "Kept.")},
			want:     []string{"Kept."},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := newResponsesScript(t, responsesSummaryDeltas(tc.payloads...))
			params := effortParams(responsesReplayModel, "high", true)
			params.ReasoningSummary = types.ReasoningSummaryAuto
			ch, err := script.adapter(nil).Stream(context.Background(), params)
			require.NoError(t, err, "Stream")
			events := drainStream(t, ch)
			for _, ev := range events {
				require.NotEqual(t, "error", ev.Type, "stream failed: %v", ev.Error)
			}
			thinking, text := splitDeltas(events)
			require.Equal(t, strings.Join(tc.want, "|"), strings.Join(thinking, "|"), "thinking_delta fragments")
			require.Equal(t, "done", text, "answer text")
		})
	}
}
