package provider

// Zero-argument tool calls (git_status, git_diff with no optional fields)
// must survive both directions of a turn: parsed off the wire without an
// error, and replayed back as a JSON object rather than null.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// sseTestServer serves a fixed SSE body to the first request.
func sseTestServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// requireEmptyToolCall asserts the stream produced exactly one tool_call for
// the named tool, carrying no arguments and no error event.
func requireEmptyToolCall(t *testing.T, events []types.StreamEvent, wantName string) {
	t.Helper()

	var calls []types.StreamEvent
	for i := range events {
		switch events[i].Type {
		case "tool_call":
			calls = append(calls, events[i])
		case "error":
			t.Fatalf("unexpected error event: %v", events[i].Error)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool_call event, got %d: %+v", len(calls), calls)
	}
	if calls[0].Name != wantName {
		t.Errorf("tool_call.Name = %q, want %q", calls[0].Name, wantName)
	}
	if len(calls[0].Input) != 0 {
		t.Errorf("tool_call.Input = %+v, want no arguments", calls[0].Input)
	}

	// The loop marshals Input into the stored tool_use block. A nil map
	// marshals to null, so the adapter's output must normalize to an object.
	raw, err := json.Marshal(calls[0].Input)
	if err != nil {
		t.Fatalf("marshal tool input: %v", err)
	}
	if got := string(types.NormalizeToolInput(raw)); got != "{}" {
		t.Errorf("normalized tool input = %q, want {}", got)
	}
}

// Anthropic sends a tool_use content_block_start with no input_json_delta
// events at all for a parameterless call.
func TestAnthropicAdapter_ZeroArgumentToolCall(t *testing.T) {
	srv := sseTestServer(t, joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_empty","name":"git_status"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"tool_use"}}`),
		makeSSE("message_stop", `{}`),
	))

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "claude-sonnet-4-6", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	requireEmptyToolCall(t, collectEvents(t, ch), "git_status")
}

// OpenAI opens the call with an empty arguments string and never sends a
// delta for it.
func TestOpenAIAdapter_ZeroArgumentToolCall(t *testing.T) {
	srv := sseTestServer(t, strings.Join([]string{
		makeOpenAIChunk(`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_empty","type":"function","function":{"name":"git_status","arguments":""}}]},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		"data: [DONE]\n\n",
	}, ""))

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4o", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	requireEmptyToolCall(t, collectEvents(t, ch), "git_status")
}

// The Responses API reports the finished call with an empty arguments
// string and emits no arguments.delta events.
func TestOpenAIResponsesAdapter_ZeroArgumentToolCall(t *testing.T) {
	srv := sseTestServer(t, strings.Join([]string{
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"function_call","id":"fc_empty","call_id":"call_empty","name":"git_status"}}`),
		makeResponsesEvent("response.function_call_arguments.done", `{"item_id":"fc_empty","output_index":0,"arguments":""}`),
		makeResponsesEvent("response.completed", `{"response":{"id":"resp_1","status":"completed","output":[{"type":"function_call","id":"fc_empty","call_id":"call_empty","name":"git_status","arguments":""}]}}`),
	}, ""))

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-5", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	requireEmptyToolCall(t, collectEvents(t, ch), "git_status")
}

// Gemini omits the args field entirely on a parameterless functionCall.
func TestGeminiAdapter_ZeroArgumentToolCall(t *testing.T) {
	srv := sseTestServer(t, makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"git_status"}}]},"finishReason":"STOP"}]}`))

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-3.1-pro-preview"})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	requireEmptyToolCall(t, collectEvents(t, ch), "git_status")
}

// TestAnthropicAdapter_ZeroArgumentToolCallRoundTrip walks the reported
// failure end to end on the adapter that 400s: stream a parameterless
// tool_use, rebuild the assistant turn the way the loop does, and send it
// back. The replayed request must carry an object, since Anthropic rejects
// the entire conversation over a null tool input.
func TestAnthropicAdapter_ZeroArgumentToolCallRoundTrip(t *testing.T) {
	turns := []string{
		joinLines(
			makeSSE("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_empty","name":"git_status"}}`),
			makeSSE("content_block_stop", `{"index":0}`),
			makeSSE("message_delta", `{"delta":{"stop_reason":"tool_use"}}`),
			makeSSE("message_stop", `{}`),
		),
		joinLines(
			makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
			makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"the tree is clean"}}`),
			makeSSE("content_block_stop", `{"index":0}`),
			makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
			makeSSE("message_stop", `{}`),
		),
	}

	var requestBodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		idx := len(requestBodies)
		requestBodies = append(requestBodies, string(body))
		if idx >= len(turns) {
			t.Errorf("unexpected request %d", idx)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, turns[idx])
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	messages := []types.Message{{
		Role:    "user",
		Content: []types.ContentBlock{{Type: "text", Text: "Report the working tree state."}},
	}}

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-haiku-4-5",
		MaxTokens: 1024,
		Messages:  messages,
	})
	if err != nil {
		t.Fatalf("Stream() turn 1: %v", err)
	}

	// Store the tool call's input unnormalized — a nil map marshals to null.
	// The adapter is the last line of defence for history it did not build
	// itself, so the null must not survive to the wire regardless.
	var call types.StreamEvent
	for _, ev := range collectEvents(t, ch) {
		if ev.Type == "tool_call" {
			call = ev
		}
	}
	if call.Name != "git_status" {
		t.Fatalf("turn 1 produced no git_status call: %+v", call)
	}
	inputBytes, err := json.Marshal(call.Input)
	if err != nil {
		t.Fatalf("marshal tool input: %v", err)
	}

	messages = append(messages,
		types.Message{Role: "assistant", Content: []types.ContentBlock{{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Name,
			Input: inputBytes,
		}}},
		types.Message{Role: "user", Content: []types.ContentBlock{{
			Type:      "tool_result",
			ToolUseID: call.ID,
			Content:   "working tree clean",
		}}},
	)

	ch, err = adapter.Stream(context.Background(), types.StreamParams{
		Model:     "claude-haiku-4-5",
		MaxTokens: 1024,
		Messages:  messages,
	})
	if err != nil {
		t.Fatalf("Stream() turn 2: %v", err)
	}
	collectEvents(t, ch)

	if len(requestBodies) < 2 {
		t.Fatalf("expected 2 requests, got %d", len(requestBodies))
	}
	replay := requestBodies[1]
	if strings.Contains(replay, `"input":null`) {
		t.Errorf("replayed turn carries a null tool input:\n%s", replay)
	}
	if !strings.Contains(replay, `"input":{}`) {
		t.Errorf("replayed turn missing an empty tool input object:\n%s", replay)
	}
}

// emptyToolInputForms are the shapes a stored tool_use block can carry for a
// zero-argument call: absent, blank, and the literal null a marshalled nil
// map produces.
var emptyToolInputForms = map[string]json.RawMessage{
	"nil":   nil,
	"empty": json.RawMessage(``),
	"null":  json.RawMessage(`null`),
}

func zeroArgAssistantTurn(input json.RawMessage) []types.Message {
	return []types.Message{{
		Role: "assistant",
		Content: []types.ContentBlock{
			{Type: "text", Text: "checking the tree"},
			{Type: "tool_use", ID: "toolu_empty", Name: "git_status", Input: input},
		},
	}}
}

// Replaying an assistant turn whose tool_use input is null draws a 400 from
// the Messages API, which ends the run. Every empty form must reach the wire
// as an object instead.
func TestTranslateMessagesAnthropic_EmptyToolInputBecomesObject(t *testing.T) {
	for name, form := range emptyToolInputForms {
		t.Run(name, func(t *testing.T) {
			out := translateMessagesAnthropic(zeroArgAssistantTurn(form), quirks.StructuredToolResultCapability{})

			wire, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("marshal request messages: %v", err)
			}
			if strings.Contains(string(wire), `"input":null`) {
				t.Errorf("wire carries a null tool input: %s", wire)
			}
			if !strings.Contains(string(wire), `"input":{}`) {
				t.Errorf("wire missing an empty tool input object: %s", wire)
			}
		})
	}
}

// The `input` key belongs to tool_use blocks alone; normalizing must not
// graft an empty object onto text or tool_result blocks.
func TestTranslateMessagesAnthropic_NonToolBlocksOmitInput(t *testing.T) {
	messages := []types.Message{
		{Role: "assistant", Content: []types.ContentBlock{{Type: "text", Text: "hello"}}},
		{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "toolu_empty", Content: "clean"}}},
	}

	wire, err := json.Marshal(translateMessagesAnthropic(messages, quirks.StructuredToolResultCapability{}))
	if err != nil {
		t.Fatalf("marshal request messages: %v", err)
	}
	if strings.Contains(string(wire), `"input"`) {
		t.Errorf("non-tool_use block carries an input key: %s", wire)
	}
}

func TestTranslateMessages_EmptyToolInputBecomesObjectArguments(t *testing.T) {
	for name, form := range emptyToolInputForms {
		t.Run(name, func(t *testing.T) {
			out := translateMessages("", zeroArgAssistantTurn(form), nil)

			var args string
			for _, msg := range out {
				for _, tc := range msg.ToolCalls {
					args = tc.Function.Arguments
				}
			}
			if args != "{}" {
				t.Errorf("tool call arguments = %q, want {}", args)
			}
		})
	}
}

func TestTranslateMessagesResponses_EmptyToolInputBecomesObjectArguments(t *testing.T) {
	for name, form := range emptyToolInputForms {
		t.Run(name, func(t *testing.T) {
			var args string
			for _, item := range translateMessagesResponses(zeroArgAssistantTurn(form)) {
				if item.Type == "function_call" {
					args = item.Arguments
				}
			}
			if args != "{}" {
				t.Errorf("function_call arguments = %q, want {}", args)
			}
		})
	}
}

func TestTranslateMessagesGemini_EmptyToolInputBecomesObjectArgs(t *testing.T) {
	for name, form := range emptyToolInputForms {
		t.Run(name, func(t *testing.T) {
			contents, _, err := translateMessagesGemini("", zeroArgAssistantTurn(form), quirks.StructuredToolResultCapability{}, "function")
			if err != nil {
				t.Fatalf("translateMessagesGemini: %v", err)
			}

			var args string
			for _, c := range contents {
				for _, p := range c.Parts {
					if p.FunctionCall != nil {
						args = string(p.FunctionCall.Args)
					}
				}
			}
			if args != "{}" {
				t.Errorf("functionCall.args = %q, want {}", args)
			}
		})
	}
}

func TestBedrockTranslateContentBlocks_EmptyToolInputBecomesObject(t *testing.T) {
	for name, form := range emptyToolInputForms {
		t.Run(name, func(t *testing.T) {
			blocks, err := bedrockTranslateContentBlocks(zeroArgAssistantTurn(form)[0].Content)
			if err != nil {
				t.Fatalf("bedrockTranslateContentBlocks: %v", err)
			}

			var found bool
			for _, b := range blocks {
				use, ok := b.(*brtypes.ContentBlockMemberToolUse)
				if !ok {
					continue
				}
				found = true
				raw, err := use.Value.Input.MarshalSmithyDocument()
				if err != nil {
					t.Fatalf("marshal tool use document: %v", err)
				}
				if string(raw) != "{}" {
					t.Errorf("tool use input = %q, want {}", raw)
				}
			}
			if !found {
				t.Fatal("no toolUse block produced")
			}
		})
	}
}
