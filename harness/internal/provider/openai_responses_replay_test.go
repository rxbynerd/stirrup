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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	check := assert.New(t)
	s := &responsesScript{sse: sse}
	empty := []byte(makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[]}}`))
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		check.NoError(err, "read request body")
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
	must := require.New(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	must.NotEmpty(s.bodies, "no request recorded")
	return s.bodies[len(s.bodies)-1]
}

// streamTurn sends one request through adapter and drains the stream,
// failing on an error event.
func streamTurn(t *testing.T, adapter *OpenAIResponsesAdapter, model string, messages ...types.Message) []types.StreamEvent {
	t.Helper()
	must := require.New(t)
	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     model,
		MaxTokens: 1024,
		Messages:  messages,
	})
	must.NoError(err, "Stream")
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
	must := require.New(t)
	var levels []string
	for _, line := range strings.Split(logs, "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		must.NoError(json.Unmarshal([]byte(line), &rec), "decode log line %q", line)
		if rec.Msg == replayDisabledMsg {
			levels = append(levels, rec.Level)
		}
	}
	return levels
}

func messageComplete(t *testing.T, events []types.StreamEvent) types.StreamEvent {
	t.Helper()
	must := require.New(t)
	for _, ev := range events {
		if ev.Type == "message_complete" {
			return ev
		}
	}
	must.FailNow("no message_complete event")
	return types.StreamEvent{}
}

func storedReplayItems(t *testing.T, ev types.StreamEvent) []json.RawMessage {
	t.Helper()
	must := require.New(t)
	must.Contains(ev.ReplayFields, responsesReplayKey, "message_complete carries no replay items")
	var items []json.RawMessage
	must.NoError(json.Unmarshal(ev.ReplayFields[responsesReplayKey], &items), "stored replay is not a JSON array")
	return items
}

func replayOriginOf(t *testing.T, fields map[string]json.RawMessage) string {
	t.Helper()
	must := require.New(t)
	var origin string
	must.NoError(json.Unmarshal(fields[responsesReplayOriginKey], &origin), "decode %s", responsesReplayOriginKey)
	return origin
}

// jsonValuesEqual reports whether a and b decode to the same JSON value.
func jsonValuesEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	must := require.New(t)
	var av, bv any
	must.NoError(json.Unmarshal(a, &av), "decode %s", a)
	must.NoError(json.Unmarshal(b, &bv), "decode %s", b)
	return reflect.DeepEqual(av, bv)
}

// assistantFromEvents builds the assistant Message the agentic loop
// persists for a stream: text deltas are joined into a text block that
// closes at each tool call or tool search, tool searches become
// tool_search and tool_search_result blocks, and message_complete's
// ReplayFields ride along.
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
		case "tool_search":
			flush()
			input, _ := json.Marshal(ev.Input)
			msg.Content = append(msg.Content, types.ContentBlock{Type: "tool_search", ID: ev.ID, Name: ev.Name, Input: input})
		case "tool_search_result":
			flush()
			msg.Content = append(msg.Content, types.ContentBlock{Type: "tool_search_result", ToolUseID: ev.ID, ToolReferences: ev.ToolReferences})
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
	must := require.New(t)
	originJSON, err := json.Marshal(origin)
	must.NoError(err, "marshal origin")
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
	must := require.New(t)
	check := assert.New(t)
	calls := map[string]int{}
	for _, raw := range input {
		var item struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		must.NoError(json.Unmarshal(raw, &item), "decode input item")
		switch item.Type {
		case "function_call":
			calls[item.CallID]++
		case "function_call_output":
			calls[item.CallID]--
		}
	}
	for id, n := range calls {
		check.Zero(n, "call_id %q is unpaired (calls minus outputs)", id)
	}
}

// requestInput decodes a request body's input array.
func requestInput(t *testing.T, body []byte) []json.RawMessage {
	t.Helper()
	must := require.New(t)
	var req struct {
		Input []json.RawMessage `json:"input"`
	}
	must.NoError(json.Unmarshal(body, &req), "decode body")
	return req.Input
}

// TestOpenAIResponsesAdapter_ReplayCapturesOutputItems drives the
// gpt-5.6-sol fixture (a reasoning item with encrypted_content, a
// commentary-phase message carrying created_by, two function calls) and
// pins that message_complete carries the ordered output array and its
// origin under the adapter-owned keys. created_by is stripped; every other
// item is kept byte-identical to the response.completed payload.
func TestOpenAIResponsesAdapter_ReplayCapturesOutputItems(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.Equal("Reading both files.", text.String())
	must.Equal([]string{"call_fx1", "call_fx2"}, callIDs)

	complete := messageComplete(t, events)
	must.Equal("tool_use", complete.StopReason)
	must.Len(complete.ReplayFields, 2, "want keys %s and %s", responsesReplayKey, responsesReplayOriginKey)
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath("testdata", "quirks", "openai-responses", "gpt-5.6-sol", "replay.json"), complete.ReplayFields[responsesReplayKey])

	wantOrigin := responsesReplayOrigin(responsesReplayModel, script.srv.URL)
	must.Equal(wantOrigin, replayOriginOf(t, complete.ReplayFields))
	must.NotContains(string(complete.ReplayFields[responsesReplayOriginKey]), "127.0.0.1", "origin carries the base URL")

	items := storedReplayItems(t, complete)
	must.Len(items, 4)
	must.NotContains(string(complete.ReplayFields[responsesReplayKey]), "created_by")
	must.Contains(string(items[1]), `"phase":"commentary"`, "message item lost its phase")
	for _, i := range []int{0, 2, 3} {
		check.True(bytes.Contains(sse, items[i]), "item %d is not byte-identical to the streamed payload: %s", i, items[i])
	}
}

// TestOpenAIResponsesAdapter_ReplayPrefersCompletedOutput pins that the
// terminal event's output array wins over the streamed done items: it
// carries the final encrypted_content.
func TestOpenAIResponsesAdapter_ReplayPrefersCompletedOutput(t *testing.T) {
	must := require.New(t)
	sse := strings.Join([]string{
		makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"streamed-copy"}}`),
		makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":1,"delta":"done"}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done"}]}}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"final-copy"},{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done"}]}]}}`),
	}, "")
	complete := messageComplete(t, streamResponsesSSE(t, []byte(sse), nil))
	stored := string(complete.ReplayFields[responsesReplayKey])
	must.Contains(stored, "final-copy", "stored replay should use the response.completed items")
	must.NotContains(stored, "streamed-copy")
}

// TestOpenAIResponsesAdapter_ReplayFallsBackToStreamedItemsInOutputOrder
// pins the fallback for a terminal event without an output array: the done
// items are stored, ordered by output_index rather than arrival.
func TestOpenAIResponsesAdapter_ReplayFallsBackToStreamedItemsInOutputOrder(t *testing.T) {
	must := require.New(t)
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
	must.Equal(want, string(complete.ReplayFields[responsesReplayKey]))
}

// TestOpenAIResponsesAdapter_ReplaySkipsIncompleteTurns pins that a
// truncated turn stores nothing: its items may be partial, and partial
// replay is reported to be rejected.
func TestOpenAIResponsesAdapter_ReplaySkipsIncompleteTurns(t *testing.T) {
	must := require.New(t)
	var logs bytes.Buffer
	sse := makeResponsesEvent("response.incomplete", `{"response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"},{"type":"message","id":"msg_1","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"part"}]}]}}`)
	complete := messageComplete(t, streamResponsesSSE(t, []byte(sse), debugJSONLogger(&logs)))
	must.Equal("max_tokens", complete.StopReason)
	must.Nil(complete.ReplayFields)
	must.Contains(logs.String(), replayReasonIncomplete)
}

// TestOpenAIResponsesAdapter_ReplayFailedTurnStoresNothing pins that a
// response.failed turn surfaces an error and no stored items, even when
// output items streamed before the failure.
func TestOpenAIResponsesAdapter_ReplayFailedTurnStoresNothing(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	sse := strings.Join([]string{
		makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"}}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"part"}]}}`),
		makeResponsesEvent("response.failed", `{"response":{"status":"failed","error":{"message":"server error","type":"server_error"},"output":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"}]}}`),
	}, "")
	adapter := newResponsesScript(t, []byte(sse)).adapter(nil)
	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: responsesReplayModel, MaxTokens: 1024, Messages: []types.Message{replayUserPrompt}})
	must.NoError(err, "Stream")
	sawError := false
	for _, ev := range collectEvents(t, ch) {
		if ev.Type == "error" {
			sawError = true
		}
		check.Nil(ev.ReplayFields, "%s event carries ReplayFields", ev.Type)
	}
	must.True(sawError, "failed turn emitted no error event")
}

// TestOpenAIResponsesAdapter_ReplayNoOutputNoReplayFields pins that a turn
// with no output items leaves ReplayFields nil, so the persisted message is
// unchanged.
func TestOpenAIResponsesAdapter_ReplayNoOutputNoReplayFields(t *testing.T) {
	must := require.New(t)
	sse := strings.Join([]string{
		makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":0,"delta":"hi"}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[]}}`),
	}, "")
	complete := messageComplete(t, streamResponsesSSE(t, []byte(sse), nil))
	must.Nil(complete.ReplayFields)
}

// TestOpenAIResponsesAdapter_ReplayOptOutConditions pins every condition that
// disables replay for a turn: the turn still streams normally,
// message_complete carries no ReplayFields, and one WARN names the reason
// without any item content.
func TestOpenAIResponsesAdapter_ReplayOptOutConditions(t *testing.T) {
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
			name:       "tool search item not completed",
			sse:        []string{completed(`{"type":"tool_search_call","id":"tsc_1","execution":"server","call_id":null,"status":"in_progress","arguments":{"paths":["secret-arg"]}}`, call)},
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
			must := require.New(t)
			var logs bytes.Buffer
			events := streamResponsesSSE(t, []byte(strings.Join(tc.sse, "")), debugJSONLogger(&logs))
			complete := messageComplete(t, events)
			must.Nil(complete.ReplayFields)
			out := logs.String()
			must.Equal([]string{"WARN"}, replayDisabledLevels(t, out), "replay-disabled records should be one WARN")
			must.Contains(out, tc.wantReason)
			must.NotContains(out, "AAAAAAAAAAAAAAAA", "log leaked item content")
			must.NotContains(out, "secret-arg", "log leaked item content")
		})
	}
}

// TestOpenAIResponsesAdapter_ReplayMissingEncryptedContentWarnsOnce pins
// that an endpoint which never returns encrypted_content produces one WARN
// per adapter, then DEBUG records for later turns.
func TestOpenAIResponsesAdapter_ReplayMissingEncryptedContentWarnsOnce(t *testing.T) {
	must := require.New(t)
	sse := []byte(makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[]},{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}]}}`))
	script := newResponsesScript(t, sse, sse, sse, sse)

	var logs bytes.Buffer
	adapter := script.adapter(debugJSONLogger(&logs))
	for range 3 {
		streamTurn(t, adapter, responsesReplayModel, replayUserPrompt)
	}
	must.Equal([]string{"WARN", "DEBUG", "DEBUG"}, replayDisabledLevels(t, logs.String()))

	logs.Reset()
	streamTurn(t, script.adapter(debugJSONLogger(&logs)), responsesReplayModel, replayUserPrompt)
	must.Equal([]string{"WARN"}, replayDisabledLevels(t, logs.String()), "a new adapter should log one WARN")
}

// TestOpenAIResponsesAdapter_ReplayCaptureGatedByModel pins that models
// whose rules do not replay output items retain nothing and log nothing
// about replay.
func TestOpenAIResponsesAdapter_ReplayCaptureGatedByModel(t *testing.T) {
	sse := streamFixtureSSE(t, responsesReplayFixtureDir+"/response.sse")
	for _, model := range []string{"gpt-4o", "gpt-4.1", "gpt-5-chat-latest"} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			var logs bytes.Buffer
			events := streamTurn(t, newResponsesScript(t, sse).adapter(debugJSONLogger(&logs)), model, replayUserPrompt)
			complete := messageComplete(t, events)
			must.Nil(complete.ReplayFields)
			must.NotContains(logs.String(), "openai-responses output replay", "replay logged for a model without replay")
		})
	}
}

// TestOpenAIResponsesAdapter_ReplayCaptureLogIsLengthOnly pins the DEBUG
// summary for a captured turn: item count and byte total, never the
// encrypted reasoning or the message text.
func TestOpenAIResponsesAdapter_ReplayCaptureLogIsLengthOnly(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	var logs bytes.Buffer
	streamResponsesSSE(t, streamFixtureSSE(t, responsesReplayFixtureDir+"/response.sse"), debugJSONLogger(&logs))

	var record map[string]any
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "openai-responses output replay captured") {
			must.NoError(json.Unmarshal([]byte(line), &record), "decode log line")
		}
	}
	must.NotNil(record, "no capture log line: %s", logs.String())
	must.InDelta(4, record["items"], 0)
	totalLen, ok := record["total_len"].(float64)
	must.True(ok, "total_len = %v, want a byte count", record["total_len"])
	must.Positive(totalLen)
	for _, leak := range []string{"placeholder-encrypted-reasoning-state", "Reading both files."} {
		check.NotContains(logs.String(), leak)
	}
}

// TestTranslateMessagesResponses_ReplaysStoredItemsVerbatim pins the replay
// half: a consistent assistant turn from the request's origin is emitted as
// its stored items (ids, status, phase and encrypted_content included), in
// place of the reconstructed message and function_call items.
func TestTranslateMessagesResponses_ReplaysStoredItemsVerbatim(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	assistant, stored := fixtureReplayTurn(t)
	origin := replayOriginOf(t, assistant.ReplayFields)

	got := translateMessagesResponses([]types.Message{replayUserPrompt, assistant, fixtureToolResults()}, origin, nil)
	must.Len(got, 1+len(stored)+2)
	for i, item := range stored {
		raw := got[1+i].Raw
		check.True(raw != nil && jsonValuesEqual(t, raw, item), "input item %d = %s, want stored item %s", 1+i, raw, item)
	}
	for i, callID := range []string{"call_fx1", "call_fx2"} {
		out := got[1+len(stored)+i]
		check.Equal("function_call_output", out.Type, "input item %d", 1+len(stored)+i)
		check.Equal(callID, out.CallID, "input item %d", 1+len(stored)+i)
	}

	body, err := json.Marshal(got)
	must.NoError(err, "marshal")
	sent := requestInput(t, []byte(`{"input":`+string(body)+`}`))
	for i, item := range stored {
		check.True(jsonValuesEqual(t, sent[1+i], item), "wire item %d = %s, want stored item %s", 1+i, sent[1+i], item)
	}
}

// TestTranslateMessagesResponses_ReplayIsSemanticallyIdentical pins that
// replay preserves each stored item's JSON value, while json.Marshal
// compacts whitespace and HTML-escapes <, > and &.
func TestTranslateMessagesResponses_ReplayIsSemanticallyIdentical(t *testing.T) {
	must := require.New(t)
	message := "{ \"type\" : \"message\", \"id\": \"msg_1\", \"role\": \"assistant\", \"status\": \"completed\",\n  \"content\": [{\"type\": \"output_text\", \"text\": \"a < b && c > d\"}] }"
	call := `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"run","arguments":"{\"cmd\":\"x<y&&z\"}","status":"completed"}`
	msg := storedTurn(t, testReplayOrigin, []types.ContentBlock{
		{Type: "text", Text: "a < b && c > d"},
		{Type: "tool_use", ID: "call_1", Name: "run", Input: json.RawMessage(`{"cmd":"x<y&&z"}`)},
	}, message, call)

	got := translateMessagesResponses([]types.Message{msg}, testReplayOrigin, nil)
	body, err := json.Marshal(got)
	must.NoError(err, "marshal")
	sent := requestInput(t, []byte(`{"input":`+string(body)+`}`))
	must.Len(sent, 2, "replayed items: %s", body)
	for i, want := range []string{message, call} {
		must.JSONEq(want, string(sent[i]), "item %d", i)
	}
	must.Contains(string(body), "u003c", "replayed items are not HTML-escaped")
	must.NotContains(string(body), "<", "replayed items are not compacted")
	must.NotContains(string(body), "\n", "replayed items are not compacted")
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
		{"stored tool search items without a hosted search", handBuilt(toolSearchTurnBlocks(), toolSearchCallItem, toolSearchOutputItem, loadedFunctionCall)},
		{"reasoning-only turn", handBuilt(nil, `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"}`)},
		{"refusal-only stored message against a text block", handBuilt([]types.ContentBlock{text("I can't help with that.")},
			`{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I can't help with that."}]}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			msg := tc.msg(t)
			reconstructed := msg
			reconstructed.ReplayFields = nil

			got, err := json.Marshal(translateMessagesResponses([]types.Message{msg}, origin, nil))
			must.NoError(err, "marshal")
			want, err := json.Marshal(translateMessagesResponses([]types.Message{reconstructed}, origin, nil))
			must.NoError(err, "marshal reconstruction")
			must.Equal(string(want), string(got), "fallback differs from the reconstruction")
			must.NotContains(string(got), `"id":`, "fallback leaked stored item ids")
			must.NotContains(string(got), "encrypted_content", "fallback leaked stored reasoning state")
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

	search := newResponsesToolSearch(nil)

	cases := []struct {
		name   string
		msg    types.Message
		search *responsesToolSearch
		items  int
	}{
		{"tool calls reordered", func() types.Message {
			m := cloneMessage(base)
			m.Content[1], m.Content[2] = m.Content[2], m.Content[1]
			return m
		}(), nil, 4},
		{"reasoning then message without tools", storedTurn(t, origin,
			[]types.ContentBlock{{Type: "text", Text: "done"}}, reasoning, done), nil, 2},
		{"refusal-only message without text blocks", storedTurn(t, origin, nil,
			`{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"no"}]}`), nil, 1},
		{"arguments differ only in layout", storedTurn(t, origin,
			[]types.ContentBlock{{Type: "tool_use", ID: "call_1", Name: "run", Input: json.RawMessage(`{"a":[1,2],"b":1}`)}},
			reasoning, `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"run","arguments":"{ \"b\": 1, \"a\": [1, 2] }","status":"completed"}`), nil, 2},
		{"empty arguments against empty input", storedTurn(t, origin,
			[]types.ContentBlock{{Type: "tool_use", ID: "call_1", Name: "list"}},
			reasoning, `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"list","arguments":"","status":"completed"}`), nil, 2},
		{"tool search items under a hosted search", storedTurn(t, origin,
			toolSearchTurnBlocks(), toolSearchCallItem, toolSearchOutputItem, loadedFunctionCall), search, 3},
		{"tool search blocks dropped from the message", storedTurn(t, origin,
			toolSearchTurnBlocks()[2:], toolSearchCallItem, toolSearchOutputItem, loadedFunctionCall), search, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got := translateMessagesResponses([]types.Message{tc.msg}, origin, tc.search)
			must.Len(got, tc.items, "replayed items: %+v", got)
			for i, item := range got {
				must.NotNil(item.Raw, "item %d was reconstructed, want replayed", i)
			}
		})
	}
}

// TestTranslateMessagesResponses_EmptyOriginReconstructs pins that a
// request without a replay origin reconstructs every turn, whatever it
// stores.
func TestTranslateMessagesResponses_EmptyOriginReconstructs(t *testing.T) {
	must := require.New(t)
	assistant, _ := fixtureReplayTurn(t)
	stripped := cloneMessage(assistant)
	stripped.ReplayFields = nil
	got, err := json.Marshal(translateMessagesResponses([]types.Message{assistant}, "", nil))
	must.NoError(err, "marshal")
	want, err := json.Marshal(translateMessagesResponses([]types.Message{stripped}, "", nil))
	must.NoError(err, "marshal reconstruction")
	must.Equal(string(want), string(got), "origin-less request differs from the reconstruction")
}

// TestTranslateMessagesResponses_OtherReplayKeysIgnored pins that a message
// without the adapter-owned key keeps the reconstructed shape, even when it
// carries another adapter's replay state.
func TestTranslateMessagesResponses_OtherReplayKeysIgnored(t *testing.T) {
	must := require.New(t)
	msg := types.Message{
		Role:         "assistant",
		Content:      []types.ContentBlock{{Type: "text", Text: "done"}},
		ReplayFields: map[string]json.RawMessage{"reasoning_content": json.RawMessage(`"thinking"`)},
	}
	got, err := json.Marshal(translateMessagesResponses([]types.Message{msg}, testReplayOrigin, nil))
	must.NoError(err, "marshal")
	want := `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]`
	must.Equal(want, string(got))
}

// TestOpenAIResponsesAdapter_ReplaySecondTurnRequestBody drives two turns
// through one adapter: the first turn's fixture output is captured, and the
// second request replays those items between the user prompt and the tool
// outputs, with encrypted reasoning requested and every call paired with
// its output.
func TestOpenAIResponsesAdapter_ReplaySecondTurnRequestBody(t *testing.T) {
	must := require.New(t)
	script := newResponsesScript(t, streamFixtureSSE(t, responsesReplayFixtureDir+"/response.sse"))
	adapter := script.adapter(nil)
	events := streamTurn(t, adapter, responsesReplayModel, replayUserPrompt)
	assistant, stored := assistantFromEvents(events), storedReplayItems(t, messageComplete(t, events))

	streamTurn(t, adapter, responsesReplayModel, replayUserPrompt, assistant, fixtureToolResults())
	body := script.lastBody(t)

	must.Contains(string(body), `"include":["reasoning.encrypted_content"]`, "second-turn body lacks the encrypted-reasoning include")
	input := requestInput(t, body)
	must.Len(input, 1+len(stored)+2, "input items: %s", body)
	for i, item := range stored {
		must.JSONEq(string(item), string(input[1+i]), "input[%d] should replay the stored item", 1+i)
	}
	must.Equal(1, strings.Count(string(body), `"role":"assistant"`), "no reconstructed duplicate of the assistant message")
	must.NotContains(string(body), "created_by", "replayed body carries the output-only created_by")
	assertCallsPaired(t, input)
}

// TestOpenAIResponsesAdapter_ReplayPairingFallback pins that stored
// call_ids which no longer match the harness's tool_use and tool_result
// ids fall back to the reconstruction, which pairs each call with its
// output under the harness's ids.
func TestOpenAIResponsesAdapter_ReplayPairingFallback(t *testing.T) {
	must := require.New(t)
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
	must.NotContains(string(body), "placeholder-encrypted-reasoning-state", "body replayed stored reasoning state")
	must.NotContains(string(body), "call_fx", "body replayed stored call ids")
	must.NotContains(string(body), `"id":`, "body replayed stored item ids")
	input := requestInput(t, body)
	assertCallsPaired(t, input)
	must.Contains(string(body), `"call_id":"call_h1"`, "reconstruction lost the harness call id call_h1")
	must.Contains(string(body), `"call_id":"call_h2"`, "reconstruction lost the harness call id call_h2")
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
			must := require.New(t)
			first := newResponsesScript(t, sse)
			assistant := assistantFromEvents(streamTurn(t, first.adapter(nil), responsesReplayModel, replayUserPrompt))

			second := first
			if !tc.sameServer {
				second = newResponsesScript(t)
			}
			streamTurn(t, second.adapter(nil), tc.model, replayUserPrompt, assistant, fixtureToolResults())
			body := second.lastBody(t)
			replayed := strings.Contains(string(body), "placeholder-encrypted-reasoning-state")
			must.Equal(tc.wantReplay, replayed, "stored reasoning state replayed: %s", body)
			if !tc.wantReplay {
				must.NotContains(string(body), `"id":`, "reconstruction carries item ids")
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
	must := require.New(t)
	assistant, _ := fixtureReplayTurn(t)
	stripped := cloneMessage(assistant)
	stripped.ReplayFields = nil
	want, err := json.Marshal(translateMessagesResponses([]types.Message{replayUserPrompt, stripped, fixtureToolResults()}, "", nil))
	must.NoError(err, "marshal reconstruction")

	for _, model := range []string{"gpt-4o", "gpt-4.1", "gpt-5-chat-latest"} {
		t.Run(model, func(t *testing.T) {
			must := require.New(t)
			script := newResponsesScript(t)
			stored := cloneMessage(assistant)
			origin, err := json.Marshal(responsesReplayOrigin(model, script.srv.URL))
			must.NoError(err, "marshal origin")
			stored.ReplayFields[responsesReplayOriginKey] = origin

			streamTurn(t, script.adapter(nil), model, replayUserPrompt, stored, fixtureToolResults())
			body := script.lastBody(t)
			var req struct {
				Input json.RawMessage `json:"input"`
			}
			must.NoError(json.Unmarshal(body, &req), "decode body")
			assertJSONEqual(t, req.Input, string(want))
			for _, leak := range []string{`"id":`, "encrypted_content", `"phase":`, `"include":`} {
				must.NotContains(string(body), leak, "body carries %s", leak)
			}
		})
	}
}

// TestOpenAIResponsesAdapter_WireTapDumpsReplayedRequest pins that the
// debug-build wire tap sees the request exactly as sent, replayed
// encrypted reasoning included.
func TestOpenAIResponsesAdapter_WireTapDumpsReplayedRequest(t *testing.T) {
	must := require.New(t)
	script := newResponsesScript(t, streamFixtureSSE(t, responsesReplayFixtureDir+"/response.sse"))
	adapter := script.adapter(nil)
	assistant := assistantFromEvents(streamTurn(t, adapter, responsesReplayModel, replayUserPrompt))

	var dump bytes.Buffer
	adapter.WireTap(&dump)
	streamTurn(t, adapter, responsesReplayModel, replayUserPrompt, assistant, fixtureToolResults())
	for _, want := range []string{"POST /responses", "reasoning.encrypted_content", "placeholder-encrypted-reasoning-state"} {
		must.Contains(dump.String(), want, "wire dump")
	}
}

// TestResponsesInput_ReplayedItemRoundTrip pins the test-side decode of a
// body carrying replayed items: an id-bearing replayable item is kept raw
// and re-marshals to the same JSON value, while an id-less reasoning item
// is still an unknown variant.
func TestResponsesInput_ReplayedItemRoundTrip(t *testing.T) {
	must := require.New(t)
	raw := []byte("{ \"type\": \"reasoning\", \"id\": \"rs_1\", \"summary\": [],\n  \"encrypted_content\": \"a<b&c>d\" }")
	var item responsesInput
	must.NoError(json.Unmarshal(raw, &item), "unmarshal")
	must.Equal("reasoning", item.Type)
	must.Equal(string(raw), string(item.Raw), "reasoning item is kept raw")
	back, err := json.Marshal(item)
	must.NoError(err, "marshal")
	must.JSONEq(string(raw), string(back), "re-marshal")
	err = json.Unmarshal([]byte(`{"type":"reasoning"}`), &responsesInput{})
	must.Error(err, "id-less reasoning item must stay an unknown variant")
}

// TestOpenAIAdapter_IgnoresResponsesReplayKey pins that the
// openai-compatible adapter can never echo the Responses replay state:
// neither key is a threadable path, so even a resolved rule naming one
// would be skipped, and no built-in rule names them.
func TestOpenAIAdapter_IgnoresResponsesReplayKey(t *testing.T) {
	must := require.New(t)
	for _, key := range []string{responsesReplayKey, responsesReplayOriginKey} {
		must.False(threadableOpenAIReplayPath(key), "%s must not be a threadable Chat Completions replay path", key)
	}
	assistant, _ := fixtureReplayTurn(t)
	for _, paths := range [][]string{
		{responsesReplayKey, responsesReplayOriginKey},
		quirks.DefaultRegistry().Resolve("openai-compatible", "gpt-5.6-sol").ReplayFields,
	} {
		out := translateMessages("", []types.Message{assistant}, paths)
		body, err := json.Marshal(out)
		must.NoError(err, "marshal")
		for _, leak := range []string{"encrypted_content", "openai_responses", "@"} {
			must.NotContains(string(body), leak, "chat body leaked Responses replay state (paths %v)", paths)
		}
	}
}
