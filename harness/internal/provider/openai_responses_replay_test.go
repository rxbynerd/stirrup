package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

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
