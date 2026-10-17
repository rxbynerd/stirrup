package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

func TestResponsesRequest_ReasoningSummary(t *testing.T) {
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
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(body, &fields); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got, ok := fields["reasoning"]
			if tc.want == "" {
				if ok {
					t.Errorf("reasoning = %s, want absent", got)
				}
				return
			}
			if !ok || !jsonValuesEqual(t, got, []byte(tc.want)) {
				t.Errorf("reasoning = %s, want %s", got, tc.want)
			}

			var round responsesRequest
			if err := json.Unmarshal(body, &round); err != nil {
				t.Fatalf("round trip: %v", err)
			}
			if round.ReasoningSummary != req.ReasoningSummary {
				t.Errorf("round-tripped ReasoningSummary = %q, want %q", round.ReasoningSummary, req.ReasoningSummary)
			}
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
func TestOpenAIResponsesAdapter_ReasoningSummaryStreamsAndReplays(t *testing.T) {
	script := newResponsesScript(t, responsesSummaryTurn())
	adapter := script.adapter(nil)

	params := effortParams(responsesReplayModel, "high", true)
	params.ReasoningSummary = types.ReasoningSummaryAuto
	ch, err := adapter.Stream(context.Background(), params)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drainStream(t, ch)

	thinking, text := splitDeltas(events)
	if got, want := strings.Join(thinking, "|"), "**Plan**\n\n|Read a.go first."; got != want {
		t.Errorf("thinking_delta fragments = %q, want %q", got, want)
	}
	if text != "" {
		t.Errorf("answer text = %q, want none", text)
	}
	for _, ev := range events {
		if ev.Type == "tool_call" && strings.Contains(string(mustJSON(t, ev.Input)), "Plan") {
			t.Errorf("summary text leaked into tool input: %v", ev.Input)
		}
	}
	if body := string(script.lastBody(t)); !strings.Contains(body, `"reasoning":{"effort":"high","summary":"auto"}`) {
		t.Errorf("first request does not ask for a summary: %s", body)
	}

	stored := storedReplayItems(t, messageComplete(t, events))
	if len(stored) != 2 || !strings.Contains(string(stored[0]), `"summary_text"`) {
		t.Fatalf("stored items = %s, want the reasoning item with its summary first", stored)
	}

	assistant := assistantFromEvents(events)
	results := types.Message{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "call_s1", Content: "package a"}}}
	params.Messages = append(params.Messages, assistant, results)
	ch, err = adapter.Stream(context.Background(), params)
	if err != nil {
		t.Fatalf("second Stream: %v", err)
	}
	drainStream(t, ch)

	input := requestInput(t, script.lastBody(t))
	if len(input) < 3 || !jsonValuesEqual(t, input[1], stored[0]) {
		t.Fatalf("second request input = %s, want the stored reasoning item replayed at index 1", input)
	}
	assertCallsPaired(t, input)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
