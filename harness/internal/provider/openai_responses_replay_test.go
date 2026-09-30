package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirkstest"
	"github.com/rxbynerd/stirrup/types"
)

// The Responses replay fixtures are synthetic: no OpenAI credential was
// available, so the wire shapes below are documented, not probed.
const responsesReplayFixtureDir = "testdata/quirks/openai-responses/gpt-5.6-sol"

// streamResponsesSSE drives sse through the production Responses Stream path
// and returns every event, failing on an error event.
func streamResponsesSSE(t *testing.T, sse []byte, logger *slog.Logger) []types.StreamEvent {
	t.Helper()
	srv := sseStubServer(t, sse)
	t.Cleanup(srv.Close)
	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	if logger != nil {
		adapter.Logger = logger
	}
	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-5.6-sol",
		MaxTokens: 1024,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "read a.go and b.go"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return drainStream(t, ch)
}

func messageComplete(t *testing.T, events []types.StreamEvent) types.StreamEvent {
	t.Helper()
	for _, ev := range events {
		if ev.Type == "message_complete" {
			return ev
		}
	}
	t.Fatal("no message_complete event")
	return types.StreamEvent{}
}

func storedReplayItems(t *testing.T, ev types.StreamEvent) []json.RawMessage {
	t.Helper()
	raw, ok := ev.ReplayFields[responsesReplayKey]
	if !ok {
		t.Fatalf("message_complete carries no %s: %v", responsesReplayKey, ev.ReplayFields)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("stored replay is not a JSON array: %v", err)
	}
	return items
}

// TestResponsesReplay_CapturesOutputItems drives the gpt-5.6-sol fixture (a
// reasoning item with encrypted_content, a commentary-phase message, two
// function calls) and pins that message_complete carries the ordered output
// array under the adapter-owned key, each item byte-identical to the
// response.completed payload. The streamed text and tool calls are
// unaffected.
func TestResponsesReplay_CapturesOutputItems(t *testing.T) {
	sse := streamFixtureSSE(t, responsesReplayFixtureDir+"/response.sse")
	events := streamResponsesSSE(t, sse, nil)

	var text strings.Builder
	var callIDs []string
	for _, ev := range events {
		switch ev.Type {
		case "text_delta":
			text.WriteString(ev.Text)
		case "tool_call":
			callIDs = append(callIDs, ev.ID)
		}
	}
	if text.String() != "Reading both files." {
		t.Errorf("text = %q, want %q", text.String(), "Reading both files.")
	}
	if strings.Join(callIDs, ",") != "call_fx1,call_fx2" {
		t.Errorf("tool calls = %v, want [call_fx1 call_fx2]", callIDs)
	}

	complete := messageComplete(t, events)
	if complete.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", complete.StopReason)
	}
	if len(complete.ReplayFields) != 1 {
		t.Errorf("ReplayFields keys = %d, want only %s", len(complete.ReplayFields), responsesReplayKey)
	}
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath("testdata", "quirks", "openai-responses", "gpt-5.6-sol", "replay.json"), complete.ReplayFields[responsesReplayKey])

	items := storedReplayItems(t, complete)
	if len(items) != 4 {
		t.Fatalf("stored %d items, want 4", len(items))
	}
	for i, item := range items {
		if !bytes.Contains(sse, item) {
			t.Errorf("item %d is not byte-identical to the streamed payload: %s", i, item)
		}
	}
	if !bytes.Contains(items[1], []byte(`"phase":"commentary"`)) {
		t.Errorf("message item lost its phase: %s", items[1])
	}
}

// TestResponsesReplay_PrefersCompletedOutput pins that the terminal
// event's output array wins over the streamed done items: it carries the
// final encrypted_content.
func TestResponsesReplay_PrefersCompletedOutput(t *testing.T) {
	sse := strings.Join([]string{
		makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"streamed-copy"}}`),
		makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":1,"delta":"done"}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done"}]}}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"final-copy"},{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done"}]}]}}`),
	}, "")
	complete := messageComplete(t, streamResponsesSSE(t, []byte(sse), nil))
	stored := string(complete.ReplayFields[responsesReplayKey])
	if !strings.Contains(stored, "final-copy") || strings.Contains(stored, "streamed-copy") {
		t.Errorf("stored replay = %s, want the response.completed items", stored)
	}
}

// TestResponsesReplay_FallsBackToStreamedItemsInOutputOrder pins the
// fallback for a terminal event without an output array: the done items
// are stored, ordered by output_index rather than arrival.
func TestResponsesReplay_FallsBackToStreamedItemsInOutputOrder(t *testing.T) {
	reasoning := `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"}`
	message := `{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}`
	call := `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{}","status":"completed"}`
	sse := strings.Join([]string{
		makeResponsesEvent("response.output_item.done", `{"output_index":2,"item":`+call+`}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":`+reasoning+`}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":`+message+`}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`),
	}, "")
	complete := messageComplete(t, streamResponsesSSE(t, []byte(sse), nil))
	want := "[" + reasoning + "," + message + "," + call + "]"
	if got := string(complete.ReplayFields[responsesReplayKey]); got != want {
		t.Errorf("stored replay =\n%s\nwant\n%s", got, want)
	}
}

// TestResponsesReplay_IncompleteCarriesItems pins that a truncated turn
// still records its output for replay, like a completed one.
func TestResponsesReplay_IncompleteCarriesItems(t *testing.T) {
	sse := makeResponsesEvent("response.incomplete", `{"response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"}]}}`)
	complete := messageComplete(t, streamResponsesSSE(t, []byte(sse), nil))
	if complete.StopReason != "max_tokens" {
		t.Errorf("StopReason = %q, want max_tokens", complete.StopReason)
	}
	if got := string(complete.ReplayFields[responsesReplayKey]); got != `[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"}]` {
		t.Errorf("stored replay = %s", got)
	}
}

// TestResponsesReplay_NoOutputNoReplayFields pins that a turn with no
// output items leaves ReplayFields nil, so the persisted message is
// unchanged.
func TestResponsesReplay_NoOutputNoReplayFields(t *testing.T) {
	sse := strings.Join([]string{
		makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":0,"delta":"hi"}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[]}}`),
	}, "")
	complete := messageComplete(t, streamResponsesSSE(t, []byte(sse), nil))
	if complete.ReplayFields != nil {
		t.Errorf("ReplayFields = %v, want nil", complete.ReplayFields)
	}
}

// TestResponsesReplay_OptOuts pins every condition that disables replay for
// a turn: the turn still streams normally, message_complete carries no
// ReplayFields, and one WARN names the reason without any item content.
func TestResponsesReplay_OptOuts(t *testing.T) {
	oversized := strings.Repeat("A", maxResponsesReplayBytes+1)
	bigReasoning := `{"type":"reasoning","id":"rs_big","summary":[],"encrypted_content":"` + oversized + `"}`
	call := `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"secret-arg\"}","status":"completed"}`
	cases := []struct {
		name       string
		sse        []string
		wantReason string
	}{
		{
			name: "unknown item type streamed",
			sse: []string{
				makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"completed"}}`),
				makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":`+call+`}`),
				makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[{"type":"web_search_call","id":"ws_1","status":"completed"},`+call+`]}}`),
			},
			wantReason: "unreplayable output item type",
		},
		{
			name: "unknown item type only in completed output",
			sse: []string{
				makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[{"type":"program","id":"prog_1"},`+call+`]}}`),
			},
			wantReason: "unreplayable output item type",
		},
		{
			name: "reasoning without encrypted_content",
			sse: []string{
				makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":`+call+`}`),
				makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[]},`+call+`]}}`),
			},
			wantReason: "reasoning item has no encrypted_content",
		},
		{
			name: "streamed items over the size cap",
			sse: []string{
				makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":`+bigReasoning+`}`),
				makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":`+call+`}`),
				makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[`+bigReasoning+`,`+call+`]}}`),
			},
			wantReason: "output items exceed replay size cap",
		},
		{
			name: "completed output over the size cap",
			sse: []string{
				makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[`+bigReasoning+`,`+call+`]}}`),
			},
			wantReason: "output items exceed replay size cap",
		},
		{
			name: "done event without an item",
			sse: []string{
				makeResponsesEvent("response.output_item.done", `{"output_index":0}`),
				makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`),
			},
			wantReason: "unreplayable output item type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			events := streamResponsesSSE(t, []byte(strings.Join(tc.sse, "")), logger)
			complete := messageComplete(t, events)
			if complete.ReplayFields != nil {
				t.Errorf("ReplayFields = %d keys, want nil", len(complete.ReplayFields))
			}
			out := logs.String()
			if n := strings.Count(out, "openai-responses output replay disabled for turn"); n != 1 {
				t.Errorf("replay-disabled WARN count = %d, want 1; log: %.500s", n, out)
			}
			if !strings.Contains(out, tc.wantReason) {
				t.Errorf("log missing reason %q: %.500s", tc.wantReason, out)
			}
			if strings.Contains(out, "AAAAAAAAAAAAAAAA") || strings.Contains(out, "secret-arg") {
				t.Errorf("log leaked item content: %.500s", out)
			}
		})
	}
}

// TestResponsesReplay_CaptureLogIsLengthOnly pins the DEBUG summary for a
// captured turn: item count and byte total, never the encrypted reasoning
// or the message text.
func TestResponsesReplay_CaptureLogIsLengthOnly(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	streamResponsesSSE(t, streamFixtureSSE(t, responsesReplayFixtureDir+"/response.sse"), logger)

	var record map[string]any
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "openai-responses output replay captured") {
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("decode log line: %v", err)
			}
		}
	}
	if record == nil {
		t.Fatalf("no capture log line: %s", logs.String())
	}
	if record["items"] != float64(4) {
		t.Errorf("items = %v, want 4", record["items"])
	}
	if n, ok := record["total_len"].(float64); !ok || n <= 0 {
		t.Errorf("total_len = %v, want a positive byte count", record["total_len"])
	}
	for _, leak := range []string{"placeholder-encrypted-reasoning-state", "Reading both files."} {
		if strings.Contains(logs.String(), leak) {
			t.Errorf("log leaked %q", leak)
		}
	}
}

// assistantFromEvents builds the assistant Message the agentic loop
// persists for a stream: text deltas are joined into a text block that
// closes at each tool call, and message_complete's ReplayFields ride along.
func assistantFromEvents(events []types.StreamEvent) types.Message {
	msg := types.Message{Role: "assistant"}
	var text strings.Builder
	inText := false
	flush := func() {
		if inText {
			msg.Content = append(msg.Content, types.ContentBlock{Type: "text", Text: text.String()})
			text.Reset()
			inText = false
		}
	}
	for _, ev := range events {
		switch ev.Type {
		case "text_delta":
			inText = true
			text.WriteString(ev.Text)
		case "tool_call":
			flush()
			input, _ := json.Marshal(ev.Input)
			msg.Content = append(msg.Content, types.ContentBlock{Type: "tool_use", ID: ev.ID, Name: ev.Name, Input: input})
		case "message_complete":
			flush()
			msg.ReplayFields = ev.ReplayFields
		}
	}
	return msg
}

// fixtureReplayTurn returns the gpt-5.6-sol fixture's persisted assistant
// message and its stored output items.
func fixtureReplayTurn(t *testing.T) (types.Message, []json.RawMessage) {
	t.Helper()
	events := streamResponsesSSE(t, streamFixtureSSE(t, responsesReplayFixtureDir+"/response.sse"), nil)
	return assistantFromEvents(events), storedReplayItems(t, messageComplete(t, events))
}

func fixtureToolResults() types.Message {
	return types.Message{Role: "user", Content: []types.ContentBlock{
		{Type: "tool_result", ToolUseID: "call_fx1", Content: "package a"},
		{Type: "tool_result", ToolUseID: "call_fx2", Content: "package b"},
	}}
}

// TestTranslateMessagesResponses_ReplaysStoredItemsVerbatim pins the replay
// half: a consistent assistant turn is emitted as its stored items,
// unmodified (ids, status, phase and encrypted_content included), in place
// of the reconstructed message and function_call items.
func TestTranslateMessagesResponses_ReplaysStoredItemsVerbatim(t *testing.T) {
	assistant, stored := fixtureReplayTurn(t)
	user := types.Message{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "read a.go and b.go"}}}

	got := translateMessagesResponses([]types.Message{user, assistant, fixtureToolResults()})
	if len(got) != 1+len(stored)+2 {
		t.Fatalf("got %d input items, want %d", len(got), 1+len(stored)+2)
	}
	for i, item := range stored {
		if !bytes.Equal(got[1+i].Raw, item) {
			t.Errorf("input item %d = %s, want stored item %s", 1+i, got[1+i].Raw, item)
		}
	}
	for i, callID := range []string{"call_fx1", "call_fx2"} {
		out := got[1+len(stored)+i]
		if out.Type != "function_call_output" || out.CallID != callID {
			t.Errorf("item %d = %+v, want function_call_output for %s", 1+len(stored)+i, out, callID)
		}
	}

	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	last := -1
	for i, item := range stored {
		idx := bytes.Index(body, item)
		if idx < 0 {
			t.Fatalf("stored item %d not byte-identical in the wire body: %s", i, body)
		}
		if idx <= last {
			t.Errorf("stored item %d out of order in the wire body", i)
		}
		last = idx
	}
}

// TestTranslateMessagesResponses_ReplayConsistencyFallback pins the
// all-or-nothing rule: a turn whose stored items no longer match its
// content is reconstructed exactly as a turn without stored items would
// be, with no item ids and no partial replay.
func TestTranslateMessagesResponses_ReplayConsistencyFallback(t *testing.T) {
	base, _ := fixtureReplayTurn(t)
	clone := func() types.Message {
		m := base
		m.Content = append([]types.ContentBlock(nil), base.Content...)
		m.ReplayFields = map[string]json.RawMessage{responsesReplayKey: base.ReplayFields[responsesReplayKey]}
		return m
	}
	cases := []struct {
		name   string
		mutate func(*types.Message)
	}{
		{"tool_use ID differs", func(m *types.Message) { m.Content[1].ID = "call_other" }},
		{"tool_use dropped", func(m *types.Message) { m.Content = m.Content[:2] }},
		{"extra tool_use", func(m *types.Message) {
			m.Content = append(m.Content, types.ContentBlock{Type: "tool_use", ID: "call_fx3", Name: "read_file", Input: json.RawMessage(`{}`)})
		}},
		{"text rewritten", func(m *types.Message) { m.Content[0].Text = "Reading one file." }},
		{"text dropped", func(m *types.Message) { m.Content = m.Content[1:] }},
		{"stored value not an array", func(m *types.Message) {
			m.ReplayFields[responsesReplayKey] = json.RawMessage(`{"type":"message"}`)
		}},
		{"stored value malformed", func(m *types.Message) {
			m.ReplayFields[responsesReplayKey] = json.RawMessage(`[{"type":`)
		}},
		{"stored array empty", func(m *types.Message) { m.ReplayFields[responsesReplayKey] = json.RawMessage(`[]`) }},
		{"stored item of unreplayable type", func(m *types.Message) {
			m.ReplayFields[responsesReplayKey] = json.RawMessage(`[{"type":"web_search_call","id":"ws_1"}]`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := clone()
			tc.mutate(&msg)
			reconstructed := msg
			reconstructed.ReplayFields = nil

			got, err := json.Marshal(translateMessagesResponses([]types.Message{msg}))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			want, err := json.Marshal(translateMessagesResponses([]types.Message{reconstructed}))
			if err != nil {
				t.Fatalf("marshal reconstruction: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("got\n%s\nwant reconstruction\n%s", got, want)
			}
			if bytes.Contains(got, []byte(`"id":`)) || bytes.Contains(got, []byte("encrypted_content")) {
				t.Errorf("fallback leaked stored item fields: %s", got)
			}
		})
	}
}

// TestTranslateMessagesResponses_OtherReplayKeysIgnored pins that a message
// without the adapter-owned key keeps today's reconstructed shape, even when
// it carries another adapter's replay state.
func TestTranslateMessagesResponses_OtherReplayKeysIgnored(t *testing.T) {
	msg := types.Message{
		Role:         "assistant",
		Content:      []types.ContentBlock{{Type: "text", Text: "done"}},
		ReplayFields: map[string]json.RawMessage{"reasoning_content": json.RawMessage(`"thinking"`)},
	}
	got, err := json.Marshal(translateMessagesResponses([]types.Message{msg}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]`
	if string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestResponsesReplay_SecondTurnRequestBody drives two turns through the
// production Stream path: the first turn's fixture output is captured, and
// the second request replays those items verbatim between the user prompt
// and the tool outputs, with encrypted reasoning requested.
func TestResponsesReplay_SecondTurnRequestBody(t *testing.T) {
	assistant, stored := fixtureReplayTurn(t)
	user := types.Message{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "read a.go and b.go"}}}

	bodies := make(chan []byte, 1)
	srv := responsesCaptureServer(t, bodies)
	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})
	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-5.6-sol",
		MaxTokens: 1024,
		Messages:  []types.Message{user, assistant, fixtureToolResults()},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, ch)
	body := <-bodies

	if !bytes.Contains(body, []byte(`"include":["reasoning.encrypted_content"]`)) {
		t.Errorf("second-turn body lacks the encrypted-reasoning include: %s", body)
	}
	var req struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(req.Input) != 1+len(stored)+2 {
		t.Fatalf("input has %d items, want %d: %s", len(req.Input), 1+len(stored)+2, body)
	}
	for i, item := range stored {
		if !bytes.Equal(req.Input[1+i], item) {
			t.Errorf("input[%d] = %s, want stored item %s", 1+i, req.Input[1+i], item)
		}
	}
	if n := bytes.Count(body, []byte(`"role":"assistant"`)); n != 1 {
		t.Errorf("assistant message items = %d, want 1 (no reconstructed duplicate)", n)
	}
}

// responsesCaptureServer records each request body and answers with a
// minimal completed stream.
func responsesCaptureServer(t *testing.T, bodies chan<- []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		bodies <- b
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[]}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestResponsesInput_ReplayedItemRoundTrip pins the test-side decode of a
// body carrying replayed items: an id-bearing replayable item is kept raw
// and re-marshals unchanged, while an id-less reasoning item is still an
// unknown variant.
func TestResponsesInput_ReplayedItemRoundTrip(t *testing.T) {
	raw := []byte(`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"}`)
	var item responsesInput
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if item.Type != "reasoning" || !bytes.Equal(item.Raw, raw) {
		t.Errorf("decoded %+v, want reasoning kept raw", item)
	}
	back, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(back, raw) {
		t.Errorf("re-marshal = %s, want %s", back, raw)
	}
	if err := json.Unmarshal([]byte(`{"type":"reasoning"}`), &responsesInput{}); err == nil {
		t.Error("id-less reasoning item must stay an unknown variant")
	}
}

// TestChatCompletions_IgnoresResponsesReplayKey pins that the
// openai-compatible adapter can never echo the Responses replay state: the
// key is not a threadable path, so even a resolved rule naming it would be
// skipped, and no built-in rule names it.
func TestChatCompletions_IgnoresResponsesReplayKey(t *testing.T) {
	if threadableOpenAIReplayPath(responsesReplayKey) {
		t.Fatalf("%s must not be a threadable Chat Completions replay path", responsesReplayKey)
	}
	assistant, _ := fixtureReplayTurn(t)
	for _, paths := range [][]string{
		{responsesReplayKey},
		quirks.DefaultRegistry().Resolve("openai-compatible", "gpt-5.6-sol").ReplayFields,
	} {
		out := translateMessages("", []types.Message{assistant}, paths)
		body, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if bytes.Contains(body, []byte("encrypted_content")) || bytes.Contains(body, []byte("openai_responses")) {
			t.Errorf("chat body leaked Responses replay state (paths %v): %s", paths, body)
		}
	}
}
