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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	must := require.New(t)
	check := assert.New(t)

	var calls []types.StreamEvent
	for i := range events {
		switch events[i].Type {
		case "tool_call":
			calls = append(calls, events[i])
		case "error":
			must.Failf("unexpected error event", "%v", events[i].Error)
		}
	}
	must.Len(calls, 1, "tool_call events: %+v", calls)
	check.Equal(wantName, calls[0].Name)
	check.Empty(calls[0].Input, "tool_call should carry no arguments")

	// The loop marshals Input into the stored tool_use block. A nil map
	// marshals to null, so the adapter's output must normalize to an object.
	raw, err := json.Marshal(calls[0].Input)
	must.NoError(err, "marshal tool input")
	check.Equal("{}", string(types.NormalizeToolInput(raw)))
}

// Anthropic sends a tool_use content_block_start with no input_json_delta
// events at all for a parameterless call.
func TestAnthropicAdapter_ZeroArgumentToolCall(t *testing.T) {
	must := require.New(t)
	srv := sseTestServer(t, joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_empty","name":"git_status"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"tool_use"}}`),
		makeSSE("message_stop", `{}`),
	))

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "claude-sonnet-4-6", MaxTokens: 1024})
	must.NoError(err)
	requireEmptyToolCall(t, collectEvents(t, ch), "git_status")
}

// OpenAI opens the call with an empty arguments string and never sends a
// delta for it.
func TestOpenAIAdapter_ZeroArgumentToolCall(t *testing.T) {
	must := require.New(t)
	srv := sseTestServer(t, strings.Join([]string{
		makeOpenAIChunk(`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_empty","type":"function","function":{"name":"git_status","arguments":""}}]},"finish_reason":null}]}`),
		makeOpenAIChunk(`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		"data: [DONE]\n\n",
	}, ""))

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-4o", MaxTokens: 1024})
	must.NoError(err)
	requireEmptyToolCall(t, collectEvents(t, ch), "git_status")
}

// The Responses API reports the finished call with an empty arguments
// string and emits no arguments.delta events.
func TestOpenAIResponsesAdapter_ZeroArgumentToolCall(t *testing.T) {
	must := require.New(t)
	srv := sseTestServer(t, strings.Join([]string{
		makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"function_call","id":"fc_empty","call_id":"call_empty","name":"git_status"}}`),
		makeResponsesEvent("response.function_call_arguments.done", `{"item_id":"fc_empty","output_index":0,"arguments":""}`),
		makeResponsesEvent("response.completed", `{"response":{"id":"resp_1","status":"completed","output":[{"type":"function_call","id":"fc_empty","call_id":"call_empty","name":"git_status","arguments":""}]}}`),
	}, ""))

	adapter := NewOpenAIResponsesAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gpt-5", MaxTokens: 1024})
	must.NoError(err)
	requireEmptyToolCall(t, collectEvents(t, ch), "git_status")
}

// Gemini omits the args field entirely on a parameterless functionCall.
func TestGeminiAdapter_ZeroArgumentToolCall(t *testing.T) {
	must := require.New(t)
	srv := sseTestServer(t, makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"git_status"}}]},"finishReason":"STOP"}]}`))

	adapter := newGeminiTestAdapter(srv.URL, &stubTokenSource{token: "tok"})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{Model: "gemini-3.1-pro-preview"})
	must.NoError(err)
	requireEmptyToolCall(t, collectEvents(t, ch), "git_status")
}

// TestAnthropicAdapter_ZeroArgumentToolCallRoundTrip streams a parameterless
// tool_use, rebuilds the assistant turn the way the loop does, and sends it
// back. Anthropic rejects the whole conversation when a replayed tool input
// is null, so the replayed request must carry an object.
func TestAnthropicAdapter_ZeroArgumentToolCallRoundTrip(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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

	// The handler runs on the server's goroutine, so it reports with assert.
	var requestBodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !check.NoError(err, "read request body") {
			return
		}
		idx := len(requestBodies)
		requestBodies = append(requestBodies, string(body))
		if !check.Less(idx, len(turns), "unexpected request %d", idx) {
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
	must.NoError(err, "stream turn 1")

	// Store the tool call's input unnormalized — a nil map marshals to null.
	// The adapter is the last line of defence for history it did not build
	// itself, so the null must not survive to the wire regardless.
	var call types.StreamEvent
	for _, ev := range collectEvents(t, ch) {
		if ev.Type == "tool_call" {
			call = ev
		}
	}
	must.Equal("git_status", call.Name, "turn 1 produced no git_status call: %+v", call)
	inputBytes, err := json.Marshal(call.Input)
	must.NoError(err, "marshal tool input")

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
	must.NoError(err, "stream turn 2")
	collectEvents(t, ch)

	must.GreaterOrEqual(len(requestBodies), 2, "expected 2 requests")
	replay := requestBodies[1]
	check.NotContains(replay, `"input":null`, "replayed turn carries a null tool input")
	check.Contains(replay, `"input":{}`, "replayed turn missing an empty tool input object")
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
			must := require.New(t)
			out := translateMessagesAnthropic(zeroArgAssistantTurn(form), quirks.StructuredToolResultCapability{}, false)

			wire, err := json.Marshal(out)
			must.NoError(err, "marshal request messages")
			must.NotContains(string(wire), `"input":null`)
			must.Contains(string(wire), `"input":{}`)
		})
	}
}

// The `input` key belongs to tool_use blocks alone; normalizing must not
// graft an empty object onto text or tool_result blocks.
func TestTranslateMessagesAnthropic_NonToolBlocksOmitInput(t *testing.T) {
	must := require.New(t)
	messages := []types.Message{
		{Role: "assistant", Content: []types.ContentBlock{{Type: "text", Text: "hello"}}},
		{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "toolu_empty", Content: "clean"}}},
	}

	wire, err := json.Marshal(translateMessagesAnthropic(messages, quirks.StructuredToolResultCapability{}, false))
	must.NoError(err, "marshal request messages")
	must.NotContains(string(wire), `"input"`, "non-tool_use block carries an input key")
}

func TestTranslateMessages_EmptyToolInputBecomesObjectArguments(t *testing.T) {
	for name, form := range emptyToolInputForms {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			out := translateMessages("", zeroArgAssistantTurn(form), nil)

			var args string
			for _, msg := range out {
				for _, tc := range msg.ToolCalls {
					args = tc.Function.Arguments
				}
			}
			must.Equal("{}", args)
		})
	}
}

func TestTranslateMessagesResponses_EmptyToolInputBecomesObjectArguments(t *testing.T) {
	for name, form := range emptyToolInputForms {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			var args string
			for _, item := range translateMessagesResponses(zeroArgAssistantTurn(form), "", nil) {
				if item.Type == "function_call" {
					args = item.Arguments
				}
			}
			must.Equal("{}", args)
		})
	}
}

func TestTranslateMessagesGemini_EmptyToolInputBecomesObjectArgs(t *testing.T) {
	for name, form := range emptyToolInputForms {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			contents, _, err := translateMessagesGemini("", zeroArgAssistantTurn(form), quirks.StructuredToolResultCapability{}, "function")
			must.NoError(err, "translateMessagesGemini")

			var args string
			for _, c := range contents {
				for _, p := range c.Parts {
					if p.FunctionCall != nil {
						args = string(p.FunctionCall.Args)
					}
				}
			}
			must.Equal("{}", args)
		})
	}
}

func TestBedrockTranslateContentBlocks_EmptyToolInputBecomesObject(t *testing.T) {
	for name, form := range emptyToolInputForms {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			blocks, err := bedrockTranslateContentBlocks(zeroArgAssistantTurn(form)[0].Content)
			must.NoError(err, "bedrockTranslateContentBlocks")

			var uses []*brtypes.ContentBlockMemberToolUse
			for _, b := range blocks {
				if use, ok := b.(*brtypes.ContentBlockMemberToolUse); ok {
					uses = append(uses, use)
				}
			}
			must.NotEmpty(uses, "no toolUse block produced")

			for _, use := range uses {
				raw, err := use.Value.Input.MarshalSmithyDocument()
				must.NoError(err, "marshal tool use document")
				must.Equal("{}", string(raw))
			}
		})
	}
}
