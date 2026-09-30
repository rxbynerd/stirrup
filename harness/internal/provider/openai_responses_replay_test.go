package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirkstest"
	"github.com/rxbynerd/stirrup/types"
)

// The Responses replay fixtures are synthetic: no OpenAI credential was
// available, so the wire shapes below are documented, not probed.
const (
	responsesReplayFixtureDir = "testdata/quirks/openai-responses/gpt-5.6-sol"
	responsesReplayModel      = "gpt-5.6-sol"

	// testReplayOrigin is an origin for hand-built stored turns.
	testReplayOrigin = "gpt-5.6-sol@000000000000"

	replayDisabledMsg = "openai-responses output replay disabled for turn; it will be reconstructed"
)

var replayUserPrompt = types.Message{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "read a.go and b.go"}}}

// responsesScript is an httptest server that answers each request with the
// next scripted SSE body, then with an empty completed response once the
// script runs out, and records every request body.
type responsesScript struct {
	srv    *httptest.Server
	mu     sync.Mutex
	sse    [][]byte
	bodies [][]byte
}

func newResponsesScript(t *testing.T, sse ...[]byte) *responsesScript {
	t.Helper()
	s := &responsesScript{sse: sse}
	empty := []byte(makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[]}}`))
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		reply := empty
		if len(s.sse) > 0 {
			reply, s.sse = s.sse[0], s.sse[1:]
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(reply)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *responsesScript) adapter(logger *slog.Logger) *OpenAIResponsesAdapter {
	a := NewOpenAIResponsesAdapter(staticBearer("test-key"), s.srv.URL, OpenAIAuthConfig{})
	if logger != nil {
		a.Logger = logger
	}
	return a
}

func (s *responsesScript) lastBody(t *testing.T) []byte {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		t.Fatal("no request recorded")
	}
	return s.bodies[len(s.bodies)-1]
}

// streamTurn sends one request through adapter and drains the stream,
// failing on an error event.
func streamTurn(t *testing.T, adapter *OpenAIResponsesAdapter, model string, messages ...types.Message) []types.StreamEvent {
	t.Helper()
	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     model,
		MaxTokens: 1024,
		Messages:  messages,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return drainStream(t, ch)
}

// streamResponsesSSE drives sse through a fresh gpt-5.6-sol adapter.
func streamResponsesSSE(t *testing.T, sse []byte, logger *slog.Logger) []types.StreamEvent {
	t.Helper()
	return streamTurn(t, newResponsesScript(t, sse).adapter(logger), responsesReplayModel, replayUserPrompt)
}

func debugJSONLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// replayDisabledLevels returns the level of each replay-disabled log record.
func replayDisabledLevels(t *testing.T, logs string) []string {
	t.Helper()
	var levels []string
	for _, line := range strings.Split(logs, "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if rec.Msg == replayDisabledMsg {
			levels = append(levels, rec.Level)
		}
	}
	return levels
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

func replayOriginOf(t *testing.T, fields map[string]json.RawMessage) string {
	t.Helper()
	var origin string
	if err := json.Unmarshal(fields[responsesReplayOriginKey], &origin); err != nil {
		t.Fatalf("decode %s: %v", responsesReplayOriginKey, err)
	}
	return origin
}

// jsonValuesEqual reports whether a and b decode to the same JSON value.
func jsonValuesEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("decode %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return reflect.DeepEqual(av, bv)
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
// message and its stored output items. Content is [text, call_fx1,
// call_fx2].
func fixtureReplayTurn(t *testing.T) (types.Message, []json.RawMessage) {
	t.Helper()
	events := streamResponsesSSE(t, streamFixtureSSE(t, responsesReplayFixtureDir+"/response.sse"), nil)
	return assistantFromEvents(events), storedReplayItems(t, messageComplete(t, events))
}

// cloneMessage copies msg deeply enough for a test to mutate its content
// and replay state.
func cloneMessage(msg types.Message) types.Message {
	out := msg
	out.Content = append([]types.ContentBlock(nil), msg.Content...)
	out.ReplayFields = make(map[string]json.RawMessage, len(msg.ReplayFields))
	for k, v := range msg.ReplayFields {
		out.ReplayFields[k] = v
	}
	return out
}

func fixtureToolResults() types.Message {
	return types.Message{Role: "user", Content: []types.ContentBlock{
		{Type: "tool_result", ToolUseID: "call_fx1", Content: "package a"},
		{Type: "tool_result", ToolUseID: "call_fx2", Content: "package b"},
	}}
}

// storedTurn returns an assistant message whose stored output items are
// items, recorded under origin.
func storedTurn(t *testing.T, origin string, blocks []types.ContentBlock, items ...string) types.Message {
	t.Helper()
	originJSON, err := json.Marshal(origin)
	if err != nil {
		t.Fatalf("marshal origin: %v", err)
	}
	return types.Message{
		Role:    "assistant",
		Content: blocks,
		ReplayFields: map[string]json.RawMessage{
			responsesReplayKey:       json.RawMessage("[" + strings.Join(items, ",") + "]"),
			responsesReplayOriginKey: originJSON,
		},
	}
}

// assertCallsPaired checks that every function_call in a request's input
// has a function_call_output with the same call_id, and vice versa.
func assertCallsPaired(t *testing.T, input []json.RawMessage) {
	t.Helper()
	calls := map[string]int{}
	for _, raw := range input {
		var item struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatalf("decode input item: %v", err)
		}
		switch item.Type {
		case "function_call":
			calls[item.CallID]++
		case "function_call_output":
			calls[item.CallID]--
		}
	}
	for id, n := range calls {
		if n != 0 {
			t.Errorf("call_id %q is unpaired (calls minus outputs = %d)", id, n)
		}
	}
}

// requestInput decodes a request body's input array.
func requestInput(t *testing.T, body []byte) []json.RawMessage {
	t.Helper()
	var req struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return req.Input
}

// TestOpenAIResponsesAdapter_ReplayCapturesOutputItems drives the
// gpt-5.6-sol fixture (a reasoning item with encrypted_content, a
// commentary-phase message carrying created_by, two function calls) and
// pins that message_complete carries the ordered output array and its
// origin under the adapter-owned keys. created_by is stripped; every other
// item is kept byte-identical to the response.completed payload.
func TestOpenAIResponsesAdapter_ReplayCapturesOutputItems(t *testing.T) {
	sse := streamFixtureSSE(t, responsesReplayFixtureDir+"/response.sse")
	script := newResponsesScript(t, sse)
	events := streamTurn(t, script.adapter(nil), responsesReplayModel, replayUserPrompt)

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
	if len(complete.ReplayFields) != 2 {
		t.Errorf("ReplayFields keys = %d, want %s and %s", len(complete.ReplayFields), responsesReplayKey, responsesReplayOriginKey)
	}
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath("testdata", "quirks", "openai-responses", "gpt-5.6-sol", "replay.json"), complete.ReplayFields[responsesReplayKey])

	wantOrigin := responsesReplayOrigin(responsesReplayModel, script.srv.URL)
	if got := replayOriginOf(t, complete.ReplayFields); got != wantOrigin {
		t.Errorf("origin = %q, want %q", got, wantOrigin)
	}
	if strings.Contains(string(complete.ReplayFields[responsesReplayOriginKey]), "127.0.0.1") {
		t.Errorf("origin carries the base URL: %s", complete.ReplayFields[responsesReplayOriginKey])
	}

	items := storedReplayItems(t, complete)
	if len(items) != 4 {
		t.Fatalf("stored %d items, want 4", len(items))
	}
	if bytes.Contains(complete.ReplayFields[responsesReplayKey], []byte("created_by")) {
		t.Errorf("stored items keep created_by: %s", complete.ReplayFields[responsesReplayKey])
	}
	if !bytes.Contains(items[1], []byte(`"phase":"commentary"`)) {
		t.Errorf("message item lost its phase: %s", items[1])
	}
	for _, i := range []int{0, 2, 3} {
		if !bytes.Contains(sse, items[i]) {
			t.Errorf("item %d is not byte-identical to the streamed payload: %s", i, items[i])
		}
	}
}

// TestOpenAIResponsesAdapter_ReplayPrefersCompletedOutput pins that the
// terminal event's output array wins over the streamed done items: it
// carries the final encrypted_content.
func TestOpenAIResponsesAdapter_ReplayPrefersCompletedOutput(t *testing.T) {
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

// TestOpenAIResponsesAdapter_ReplayFallsBackToStreamedItemsInOutputOrder
// pins the fallback for a terminal event without an output array: the done
// items are stored, ordered by output_index rather than arrival.
func TestOpenAIResponsesAdapter_ReplayFallsBackToStreamedItemsInOutputOrder(t *testing.T) {
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

// TestOpenAIResponsesAdapter_ReplaySkipsIncompleteTurns pins that a
// truncated turn stores nothing: its items may be partial, and partial
// replay is reported to be rejected.
func TestOpenAIResponsesAdapter_ReplaySkipsIncompleteTurns(t *testing.T) {
	var logs bytes.Buffer
	sse := makeResponsesEvent("response.incomplete", `{"response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"},{"type":"message","id":"msg_1","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"part"}]}]}}`)
	complete := messageComplete(t, streamResponsesSSE(t, []byte(sse), debugJSONLogger(&logs)))
	if complete.StopReason != "max_tokens" {
		t.Errorf("StopReason = %q, want max_tokens", complete.StopReason)
	}
	if complete.ReplayFields != nil {
		t.Errorf("ReplayFields = %s, want nil", complete.ReplayFields[responsesReplayKey])
	}
	if !strings.Contains(logs.String(), replayReasonIncomplete) {
		t.Errorf("log missing reason %q: %s", replayReasonIncomplete, logs.String())
	}
}

// TestOpenAIResponsesAdapter_ReplayFailedTurnStoresNothing pins that a
// response.failed turn surfaces an error and no stored items, even when
// output items streamed before the failure.
func TestOpenAIResponsesAdapter_ReplayFailedTurnStoresNothing(t *testing.T) {
	sse := strings.Join([]string{
		makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"}}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"part"}]}}`),
		makeResponsesEvent("response.failed", `{"response":{"status":"failed","error":{"message":"server error","type":"server_error"},"output":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"}]}}`),
	}, "")
	adapter := newResponsesScript(t, []byte(sse)).adapter(nil)
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: responsesReplayModel, MaxTokens: 1024, Messages: []types.Message{replayUserPrompt}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	sawError := false
	for _, ev := range collectEvents(t, ch) {
		if ev.Type == "error" {
			sawError = true
		}
		if ev.ReplayFields != nil {
			t.Errorf("%s event carries ReplayFields", ev.Type)
		}
	}
	if !sawError {
		t.Error("failed turn emitted no error event")
	}
}

// TestOpenAIResponsesAdapter_ReplayNoOutputNoReplayFields pins that a turn
// with no output items leaves ReplayFields nil, so the persisted message is
// unchanged.
func TestOpenAIResponsesAdapter_ReplayNoOutputNoReplayFields(t *testing.T) {
	sse := strings.Join([]string{
		makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":0,"delta":"hi"}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[]}}`),
	}, "")
	complete := messageComplete(t, streamResponsesSSE(t, []byte(sse), nil))
	if complete.ReplayFields != nil {
		t.Errorf("ReplayFields = %v, want nil", complete.ReplayFields)
	}
}

// TestOpenAIResponsesAdapter_ReplayOptOuts pins every condition that
// disables replay for a turn: the turn still streams normally,
// message_complete carries no ReplayFields, and one WARN names the reason
// without any item content.
func TestOpenAIResponsesAdapter_ReplayOptOuts(t *testing.T) {
	oversized := strings.Repeat("A", maxResponsesReplayBytes+1)
	bigReasoning := `{"type":"reasoning","id":"rs_big","summary":[],"encrypted_content":"` + oversized + `"}`
	call := `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"secret-arg\"}","status":"completed"}`
	completed := func(items ...string) string {
		return makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[`+strings.Join(items, ",")+`]}}`)
	}
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
				completed(`{"type":"web_search_call","id":"ws_1","status":"completed"}`, call),
			},
			wantReason: replayReasonUnreplayable,
		},
		{
			name:       "unknown item type only in completed output",
			sse:        []string{completed(`{"type":"program","id":"prog_1"}`, call)},
			wantReason: replayReasonUnreplayable,
		},
		{
			name: "reasoning without encrypted_content",
			sse: []string{
				makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":`+call+`}`),
				completed(`{"type":"reasoning","id":"rs_1","summary":[]}`, call),
			},
			wantReason: replayReasonNoEncrypted,
		},
		{
			name:       "function_call without call_id",
			sse:        []string{completed(`{"type":"function_call","id":"fc_2","name":"read_file","arguments":"{}","status":"completed"}`)},
			wantReason: replayReasonNoCallID,
		},
		{
			name:       "message not from the assistant",
			sse:        []string{completed(`{"type":"message","id":"msg_1","role":"user","status":"completed","content":[{"type":"output_text","text":"hi"}]}`)},
			wantReason: replayReasonNotAssistant,
		},
		{
			name:       "message with a non-text part",
			sse:        []string{completed(`{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_audio","data":"secret-arg"}]}`)},
			wantReason: replayReasonNotAssistant,
		},
		{
			name:       "item not completed",
			sse:        []string{completed(`{"type":"message","id":"msg_1","role":"assistant","status":"in_progress","content":[{"type":"output_text","text":"hi"}]}`)},
			wantReason: replayReasonNotCompleted,
		},
		{
			name:       "turn ends with a reasoning item",
			sse:        []string{completed(call, `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"}`)},
			wantReason: replayReasonEndsInReasoning,
		},
		{
			name: "streamed items over the size cap",
			sse: []string{
				makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":`+bigReasoning+`}`),
				makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":`+call+`}`),
				completed(bigReasoning, call),
			},
			wantReason: replayReasonOverCap,
		},
		{
			name:       "completed output over the size cap",
			sse:        []string{completed(bigReasoning, call)},
			wantReason: replayReasonOverCap,
		},
		{
			name: "done event without an item",
			sse: []string{
				makeResponsesEvent("response.output_item.done", `{"output_index":0}`),
				makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`),
			},
			wantReason: replayReasonUnreplayable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			events := streamResponsesSSE(t, []byte(strings.Join(tc.sse, "")), debugJSONLogger(&logs))
			complete := messageComplete(t, events)
			if complete.ReplayFields != nil {
				t.Errorf("ReplayFields = %d keys, want nil", len(complete.ReplayFields))
			}
			out := logs.String()
			if levels := replayDisabledLevels(t, out); len(levels) != 1 || levels[0] != "WARN" {
				t.Errorf("replay-disabled records = %v, want one WARN; log: %.500s", levels, out)
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

// TestOpenAIResponsesAdapter_ReplayMissingEncryptedContentWarnsOnce pins
// that an endpoint which never returns encrypted_content produces one WARN
// per adapter, then DEBUG records for later turns.
func TestOpenAIResponsesAdapter_ReplayMissingEncryptedContentWarnsOnce(t *testing.T) {
	sse := []byte(makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[]},{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}]}}`))
	script := newResponsesScript(t, sse, sse, sse, sse)

	var logs bytes.Buffer
	adapter := script.adapter(debugJSONLogger(&logs))
	for range 3 {
		streamTurn(t, adapter, responsesReplayModel, replayUserPrompt)
	}
	if got := replayDisabledLevels(t, logs.String()); !reflect.DeepEqual(got, []string{"WARN", "DEBUG", "DEBUG"}) {
		t.Errorf("levels = %v, want [WARN DEBUG DEBUG]", got)
	}

	logs.Reset()
	streamTurn(t, script.adapter(debugJSONLogger(&logs)), responsesReplayModel, replayUserPrompt)
	if got := replayDisabledLevels(t, logs.String()); !reflect.DeepEqual(got, []string{"WARN"}) {
		t.Errorf("a new adapter logged %v, want [WARN]", got)
	}
}

// TestOpenAIResponsesAdapter_ReplayCaptureGatedByModel pins that models
// whose rules do not replay output items retain nothing and log nothing
// about replay.
func TestOpenAIResponsesAdapter_ReplayCaptureGatedByModel(t *testing.T) {
	sse := streamFixtureSSE(t, responsesReplayFixtureDir+"/response.sse")
	for _, model := range []string{"gpt-4o", "gpt-4.1", "gpt-5-chat-latest"} {
		t.Run(model, func(t *testing.T) {
			var logs bytes.Buffer
			events := streamTurn(t, newResponsesScript(t, sse).adapter(debugJSONLogger(&logs)), model, replayUserPrompt)
			if complete := messageComplete(t, events); complete.ReplayFields != nil {
				t.Errorf("ReplayFields = %v, want nil", complete.ReplayFields)
			}
			if strings.Contains(logs.String(), "openai-responses output replay") {
				t.Errorf("replay logged for a model without replay: %s", logs.String())
			}
		})
	}
}

// TestOpenAIResponsesAdapter_ReplayCaptureLogIsLengthOnly pins the DEBUG
// summary for a captured turn: item count and byte total, never the
// encrypted reasoning or the message text.
func TestOpenAIResponsesAdapter_ReplayCaptureLogIsLengthOnly(t *testing.T) {
	var logs bytes.Buffer
	streamResponsesSSE(t, streamFixtureSSE(t, responsesReplayFixtureDir+"/response.sse"), debugJSONLogger(&logs))

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

// TestTranslateMessagesResponses_ReplaysStoredItemsVerbatim pins the replay
// half: a consistent assistant turn from the request's origin is emitted as
// its stored items (ids, status, phase and encrypted_content included), in
// place of the reconstructed message and function_call items.
func TestTranslateMessagesResponses_ReplaysStoredItemsVerbatim(t *testing.T) {
	assistant, stored := fixtureReplayTurn(t)
	origin := replayOriginOf(t, assistant.ReplayFields)

	got := translateMessagesResponses([]types.Message{replayUserPrompt, assistant, fixtureToolResults()}, origin)
	if len(got) != 1+len(stored)+2 {
		t.Fatalf("got %d input items, want %d", len(got), 1+len(stored)+2)
	}
	for i, item := range stored {
		if got[1+i].Raw == nil || !jsonValuesEqual(t, got[1+i].Raw, item) {
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
	sent := requestInput(t, []byte(`{"input":`+string(body)+`}`))
	for i, item := range stored {
		if !jsonValuesEqual(t, sent[1+i], item) {
			t.Errorf("wire item %d = %s, want stored item %s", 1+i, sent[1+i], item)
		}
	}
}

// TestTranslateMessagesResponses_ReplayIsSemanticallyIdentical pins that
// replay preserves each stored item's JSON value, while json.Marshal
// compacts whitespace and HTML-escapes <, > and &.
func TestTranslateMessagesResponses_ReplayIsSemanticallyIdentical(t *testing.T) {
	message := "{ \"type\" : \"message\", \"id\": \"msg_1\", \"role\": \"assistant\", \"status\": \"completed\",\n  \"content\": [{\"type\": \"output_text\", \"text\": \"a < b && c > d\"}] }"
	call := `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"run","arguments":"{\"cmd\":\"x<y&&z\"}","status":"completed"}`
	msg := storedTurn(t, testReplayOrigin, []types.ContentBlock{
		{Type: "text", Text: "a < b && c > d"},
		{Type: "tool_use", ID: "call_1", Name: "run", Input: json.RawMessage(`{"cmd":"x<y&&z"}`)},
	}, message, call)

	got := translateMessagesResponses([]types.Message{msg}, testReplayOrigin)
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sent := requestInput(t, []byte(`{"input":`+string(body)+`}`))
	if len(sent) != 2 {
		t.Fatalf("sent %d items, want 2: %s", len(sent), body)
	}
	for i, want := range []string{message, call} {
		if !jsonValuesEqual(t, sent[i], []byte(want)) {
			t.Errorf("item %d = %s, want the value of %s", i, sent[i], want)
		}
	}
	if !bytes.Contains(body, []byte("u003c")) || bytes.ContainsAny(body, "<\n") {
		t.Errorf("replayed items are not compacted and HTML-escaped: %s", body)
	}
}

// TestTranslateMessagesResponses_ReplayConsistencyFallback pins the
// all-or-nothing rule: a turn whose stored items are invalid, come from
// another origin, or no longer match its content is reconstructed exactly
// as a turn without stored items would be, with no item ids and no partial
// replay.
func TestTranslateMessagesResponses_ReplayConsistencyFallback(t *testing.T) {
	base, _ := fixtureReplayTurn(t)
	origin := replayOriginOf(t, base.ReplayFields)
	fromFixture := func(mutate func(*types.Message)) func(*testing.T) types.Message {
		return func(*testing.T) types.Message {
			m := cloneMessage(base)
			mutate(&m)
			return m
		}
	}
	text := func(s string) types.ContentBlock { return types.ContentBlock{Type: "text", Text: s} }
	okMessage := `{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}`
	handBuilt := func(blocks []types.ContentBlock, items ...string) func(*testing.T) types.Message {
		return func(t *testing.T) types.Message { return storedTurn(t, origin, blocks, items...) }
	}

	cases := []struct {
		name string
		msg  func(*testing.T) types.Message
	}{
		{"tool_use ID differs", fromFixture(func(m *types.Message) { m.Content[1].ID = "call_other" })},
		{"tool_use dropped", fromFixture(func(m *types.Message) { m.Content = m.Content[:2] })},
		{"extra tool_use", fromFixture(func(m *types.Message) {
			m.Content = append(m.Content, types.ContentBlock{Type: "tool_use", ID: "call_fx3", Name: "read_file", Input: json.RawMessage(`{}`)})
		})},
		{"duplicate tool_use id", fromFixture(func(m *types.Message) { m.Content = append(m.Content, m.Content[1]) })},
		{"tool_use input rewritten", fromFixture(func(m *types.Message) { m.Content[1].Input = json.RawMessage(`{"path":"c.go"}`) })},
		{"tool_use name differs", fromFixture(func(m *types.Message) { m.Content[1].Name = "write_file" })},
		{"text rewritten", fromFixture(func(m *types.Message) { m.Content[0].Text = "Reading one file." })},
		{"text dropped", fromFixture(func(m *types.Message) { m.Content = m.Content[1:] })},
		{"origin missing", fromFixture(func(m *types.Message) { delete(m.ReplayFields, responsesReplayOriginKey) })},
		{"origin from another endpoint", fromFixture(func(m *types.Message) {
			m.ReplayFields[responsesReplayOriginKey] = json.RawMessage(`"gpt-5.6-sol@ffffffffffff"`)
		})},
		{"stored value not an array", fromFixture(func(m *types.Message) {
			m.ReplayFields[responsesReplayKey] = json.RawMessage(`{"type":"message"}`)
		})},
		{"stored value malformed", fromFixture(func(m *types.Message) { m.ReplayFields[responsesReplayKey] = json.RawMessage(`[{"type":`) })},
		{"stored array empty", fromFixture(func(m *types.Message) { m.ReplayFields[responsesReplayKey] = json.RawMessage(`[]`) })},
		{"stored item of unreplayable type", handBuilt([]types.ContentBlock{text("ok")}, okMessage, `{"type":"web_search_call","id":"ws_1","status":"completed"}`)},
		{"stored reasoning without encrypted_content", handBuilt([]types.ContentBlock{text("ok")},
			`{"type":"reasoning","id":"rs_1","summary":[]}`, okMessage)},
		{"stored function_call without call_id", handBuilt(
			[]types.ContentBlock{text("ok"), {Type: "tool_use", Name: "read_file", Input: json.RawMessage(`{}`)}},
			okMessage, `{"type":"function_call","id":"fc_1","name":"read_file","arguments":"{}","status":"completed"}`)},
		{"stored duplicate call_id", handBuilt(
			[]types.ContentBlock{text("ok"), {Type: "tool_use", ID: "call_1", Name: "read_file", Input: json.RawMessage(`{}`)}},
			okMessage,
			`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{}","status":"completed"}`,
			`{"type":"function_call","id":"fc_2","call_id":"call_1","name":"read_file","arguments":"{}","status":"completed"}`)},
		{"stored message from the user role", handBuilt([]types.ContentBlock{text("ok")},
			`{"type":"message","id":"msg_1","role":"user","status":"completed","content":[{"type":"output_text","text":"ok"}]}`)},
		{"stored message with a non-text part", handBuilt([]types.ContentBlock{text("ok")},
			`{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"},{"type":"input_image"}]}`)},
		{"stored item not completed", handBuilt([]types.ContentBlock{text("ok")},
			`{"type":"message","id":"msg_1","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"ok"}]}`)},
		{"reasoning-only turn", handBuilt(nil, `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"}`)},
		{"refusal-only stored message against a text block", handBuilt([]types.ContentBlock{text("I can't help with that.")},
			`{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I can't help with that."}]}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.msg(t)
			reconstructed := msg
			reconstructed.ReplayFields = nil

			got, err := json.Marshal(translateMessagesResponses([]types.Message{msg}, origin))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			want, err := json.Marshal(translateMessagesResponses([]types.Message{reconstructed}, origin))
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

// TestTranslateMessagesResponses_ReplayConsistencyAccepts pins the stored
// turns that still describe their message and therefore replay: the
// comparison is by call_id set and JSON value, not by order or layout.
func TestTranslateMessagesResponses_ReplayConsistencyAccepts(t *testing.T) {
	base, _ := fixtureReplayTurn(t)
	origin := replayOriginOf(t, base.ReplayFields)
	reasoning := `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"}`
	done := `{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done"}]}`

	cases := []struct {
		name  string
		msg   types.Message
		items int
	}{
		{"tool calls reordered", func() types.Message {
			m := cloneMessage(base)
			m.Content[1], m.Content[2] = m.Content[2], m.Content[1]
			return m
		}(), 4},
		{"reasoning then message without tools", storedTurn(t, origin,
			[]types.ContentBlock{{Type: "text", Text: "done"}}, reasoning, done), 2},
		{"refusal-only message without text blocks", storedTurn(t, origin, nil,
			`{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"no"}]}`), 1},
		{"arguments differ only in layout", storedTurn(t, origin,
			[]types.ContentBlock{{Type: "tool_use", ID: "call_1", Name: "run", Input: json.RawMessage(`{"a":[1,2],"b":1}`)}},
			reasoning, `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"run","arguments":"{ \"b\": 1, \"a\": [1, 2] }","status":"completed"}`), 2},
		{"empty arguments against empty input", storedTurn(t, origin,
			[]types.ContentBlock{{Type: "tool_use", ID: "call_1", Name: "list"}},
			reasoning, `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"list","arguments":"","status":"completed"}`), 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translateMessagesResponses([]types.Message{tc.msg}, origin)
			if len(got) != tc.items {
				t.Fatalf("got %d items, want %d replayed: %+v", len(got), tc.items, got)
			}
			for i, item := range got {
				if item.Raw == nil {
					t.Errorf("item %d was reconstructed, want replayed: %+v", i, item)
				}
			}
		})
	}
}

// TestTranslateMessagesResponses_EmptyOriginReconstructs pins that a
// request without a replay origin reconstructs every turn, whatever it
// stores.
func TestTranslateMessagesResponses_EmptyOriginReconstructs(t *testing.T) {
	assistant, _ := fixtureReplayTurn(t)
	stripped := cloneMessage(assistant)
	stripped.ReplayFields = nil
	got, err := json.Marshal(translateMessagesResponses([]types.Message{assistant}, ""))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want, err := json.Marshal(translateMessagesResponses([]types.Message{stripped}, ""))
	if err != nil {
		t.Fatalf("marshal reconstruction: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got\n%s\nwant reconstruction\n%s", got, want)
	}
}

// TestTranslateMessagesResponses_OtherReplayKeysIgnored pins that a message
// without the adapter-owned key keeps the reconstructed shape, even when it
// carries another adapter's replay state.
func TestTranslateMessagesResponses_OtherReplayKeysIgnored(t *testing.T) {
	msg := types.Message{
		Role:         "assistant",
		Content:      []types.ContentBlock{{Type: "text", Text: "done"}},
		ReplayFields: map[string]json.RawMessage{"reasoning_content": json.RawMessage(`"thinking"`)},
	}
	got, err := json.Marshal(translateMessagesResponses([]types.Message{msg}, testReplayOrigin))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]`
	if string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestOpenAIResponsesAdapter_ReplaySecondTurnRequestBody drives two turns
// through one adapter: the first turn's fixture output is captured, and the
// second request replays those items between the user prompt and the tool
// outputs, with encrypted reasoning requested and every call paired with
// its output.
func TestOpenAIResponsesAdapter_ReplaySecondTurnRequestBody(t *testing.T) {
	script := newResponsesScript(t, streamFixtureSSE(t, responsesReplayFixtureDir+"/response.sse"))
	adapter := script.adapter(nil)
	events := streamTurn(t, adapter, responsesReplayModel, replayUserPrompt)
	assistant, stored := assistantFromEvents(events), storedReplayItems(t, messageComplete(t, events))

	streamTurn(t, adapter, responsesReplayModel, replayUserPrompt, assistant, fixtureToolResults())
	body := script.lastBody(t)

	if !bytes.Contains(body, []byte(`"include":["reasoning.encrypted_content"]`)) {
		t.Errorf("second-turn body lacks the encrypted-reasoning include: %s", body)
	}
	input := requestInput(t, body)
	if len(input) != 1+len(stored)+2 {
		t.Fatalf("input has %d items, want %d: %s", len(input), 1+len(stored)+2, body)
	}
	for i, item := range stored {
		if !jsonValuesEqual(t, input[1+i], item) {
			t.Errorf("input[%d] = %s, want stored item %s", 1+i, input[1+i], item)
		}
	}
	if n := bytes.Count(body, []byte(`"role":"assistant"`)); n != 1 {
		t.Errorf("assistant message items = %d, want 1 (no reconstructed duplicate)", n)
	}
	if bytes.Contains(body, []byte("created_by")) {
		t.Errorf("replayed body carries the output-only created_by: %s", body)
	}
	assertCallsPaired(t, input)
}

// TestOpenAIResponsesAdapter_ReplayPairingFallback pins that stored
// call_ids which no longer match the harness's tool_use and tool_result
// ids fall back to the reconstruction, which pairs each call with its
// output under the harness's ids.
func TestOpenAIResponsesAdapter_ReplayPairingFallback(t *testing.T) {
	script := newResponsesScript(t, streamFixtureSSE(t, responsesReplayFixtureDir+"/response.sse"))
	adapter := script.adapter(nil)
	assistant := assistantFromEvents(streamTurn(t, adapter, responsesReplayModel, replayUserPrompt))
	assistant.Content[1].ID, assistant.Content[2].ID = "call_h1", "call_h2"
	results := types.Message{Role: "user", Content: []types.ContentBlock{
		{Type: "tool_result", ToolUseID: "call_h1", Content: "package a"},
		{Type: "tool_result", ToolUseID: "call_h2", Content: "package b"},
	}}

	streamTurn(t, adapter, responsesReplayModel, replayUserPrompt, assistant, results)
	body := script.lastBody(t)
	if bytes.Contains(body, []byte("placeholder-encrypted-reasoning-state")) || bytes.Contains(body, []byte("call_fx")) || bytes.Contains(body, []byte(`"id":`)) {
		t.Errorf("body replayed stale stored items: %s", body)
	}
	input := requestInput(t, body)
	assertCallsPaired(t, input)
	if !bytes.Contains(body, []byte(`"call_id":"call_h1"`)) || !bytes.Contains(body, []byte(`"call_id":"call_h2"`)) {
		t.Errorf("reconstruction lost the harness call ids: %s", body)
	}
}

// TestOpenAIResponsesAdapter_ReplayOriginMismatchReconstructs pins the
// origin gate: stored items replay only to the model and endpoint that
// produced them, so a router switch to another endpoint or model sends the
// reconstruction.
func TestOpenAIResponsesAdapter_ReplayOriginMismatchReconstructs(t *testing.T) {
	sse := streamFixtureSSE(t, responsesReplayFixtureDir+"/response.sse")
	cases := []struct {
		name       string
		sameServer bool
		model      string
		wantReplay bool
	}{
		{"same endpoint and model", true, responsesReplayModel, true},
		{"another endpoint", false, responsesReplayModel, false},
		{"another model", true, "gpt-5.5", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := newResponsesScript(t, sse)
			assistant := assistantFromEvents(streamTurn(t, first.adapter(nil), responsesReplayModel, replayUserPrompt))

			second := first
			if !tc.sameServer {
				second = newResponsesScript(t)
			}
			streamTurn(t, second.adapter(nil), tc.model, replayUserPrompt, assistant, fixtureToolResults())
			body := second.lastBody(t)
			if got := bytes.Contains(body, []byte("placeholder-encrypted-reasoning-state")); got != tc.wantReplay {
				t.Errorf("replayed = %v, want %v: %s", got, tc.wantReplay, body)
			}
			if !tc.wantReplay && bytes.Contains(body, []byte(`"id":`)) {
				t.Errorf("reconstruction carries item ids: %s", body)
			}
			assertCallsPaired(t, requestInput(t, body))
		})
	}
}

// TestOpenAIResponsesAdapter_ReplayNonReasoningModelsReconstruct pins the
// replay gate: on a model whose rules do not set ReplayOutputItems, an
// assistant turn carrying stored items from the same origin is still sent
// in the reconstructed, id-less shape, and no encrypted reasoning is
// requested.
func TestOpenAIResponsesAdapter_ReplayNonReasoningModelsReconstruct(t *testing.T) {
	assistant, _ := fixtureReplayTurn(t)
	stripped := cloneMessage(assistant)
	stripped.ReplayFields = nil
	want, err := json.Marshal(translateMessagesResponses([]types.Message{replayUserPrompt, stripped, fixtureToolResults()}, ""))
	if err != nil {
		t.Fatalf("marshal reconstruction: %v", err)
	}

	for _, model := range []string{"gpt-4o", "gpt-4.1", "gpt-5-chat-latest"} {
		t.Run(model, func(t *testing.T) {
			script := newResponsesScript(t)
			stored := cloneMessage(assistant)
			origin, err := json.Marshal(responsesReplayOrigin(model, script.srv.URL))
			if err != nil {
				t.Fatalf("marshal origin: %v", err)
			}
			stored.ReplayFields[responsesReplayOriginKey] = origin

			streamTurn(t, script.adapter(nil), model, replayUserPrompt, stored, fixtureToolResults())
			body := script.lastBody(t)
			var req struct {
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			assertJSONEqual(t, req.Input, string(want))
			for _, leak := range []string{`"id":`, "encrypted_content", `"phase":`, `"include":`} {
				if bytes.Contains(body, []byte(leak)) {
					t.Errorf("body carries %s: %s", leak, body)
				}
			}
		})
	}
}

// TestOpenAIResponsesAdapter_WireTapDumpsReplayedRequest pins that the
// debug-build wire tap sees the request exactly as sent, replayed
// encrypted reasoning included.
func TestOpenAIResponsesAdapter_WireTapDumpsReplayedRequest(t *testing.T) {
	script := newResponsesScript(t, streamFixtureSSE(t, responsesReplayFixtureDir+"/response.sse"))
	adapter := script.adapter(nil)
	assistant := assistantFromEvents(streamTurn(t, adapter, responsesReplayModel, replayUserPrompt))

	var dump bytes.Buffer
	adapter.WireTap(&dump)
	streamTurn(t, adapter, responsesReplayModel, replayUserPrompt, assistant, fixtureToolResults())
	for _, want := range []string{"POST /responses", "reasoning.encrypted_content", "placeholder-encrypted-reasoning-state"} {
		if !strings.Contains(dump.String(), want) {
			t.Errorf("wire dump lacks %q: %.800s", want, dump.String())
		}
	}
}

// TestResponsesInput_ReplayedItemRoundTrip pins the test-side decode of a
// body carrying replayed items: an id-bearing replayable item is kept raw
// and re-marshals to the same JSON value, while an id-less reasoning item
// is still an unknown variant.
func TestResponsesInput_ReplayedItemRoundTrip(t *testing.T) {
	raw := []byte("{ \"type\": \"reasoning\", \"id\": \"rs_1\", \"summary\": [],\n  \"encrypted_content\": \"a<b&c>d\" }")
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
	if !jsonValuesEqual(t, back, raw) {
		t.Errorf("re-marshal = %s, want the value of %s", back, raw)
	}
	if err := json.Unmarshal([]byte(`{"type":"reasoning"}`), &responsesInput{}); err == nil {
		t.Error("id-less reasoning item must stay an unknown variant")
	}
}

// TestOpenAIAdapter_IgnoresResponsesReplayKey pins that the
// openai-compatible adapter can never echo the Responses replay state:
// neither key is a threadable path, so even a resolved rule naming one
// would be skipped, and no built-in rule names them.
func TestOpenAIAdapter_IgnoresResponsesReplayKey(t *testing.T) {
	for _, key := range []string{responsesReplayKey, responsesReplayOriginKey} {
		if threadableOpenAIReplayPath(key) {
			t.Fatalf("%s must not be a threadable Chat Completions replay path", key)
		}
	}
	assistant, _ := fixtureReplayTurn(t)
	for _, paths := range [][]string{
		{responsesReplayKey, responsesReplayOriginKey},
		quirks.DefaultRegistry().Resolve("openai-compatible", "gpt-5.6-sol").ReplayFields,
	} {
		out := translateMessages("", []types.Message{assistant}, paths)
		body, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if bytes.Contains(body, []byte("encrypted_content")) || bytes.Contains(body, []byte("openai_responses")) || bytes.Contains(body, []byte("@")) {
			t.Errorf("chat body leaked Responses replay state (paths %v): %s", paths, body)
		}
	}
}
