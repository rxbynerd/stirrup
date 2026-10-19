package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// fakeBatchClient is a controllable BatchClient for table-driven tests.
type fakeBatchClient struct {
	submitErr error
	submitFn  func(entries []BatchEntry) (string, error)

	resultErr error
	resultFn  func(batchID string) (map[string]*BatchResult, error)

	mu             sync.Mutex
	submitted      [][]BatchEntry
	resultRequests []string
}

func (f *fakeBatchClient) Submit(_ context.Context, entries []BatchEntry) (string, error) {
	f.mu.Lock()
	f.submitted = append(f.submitted, entries)
	f.mu.Unlock()

	if f.submitFn != nil {
		return f.submitFn(entries)
	}
	if f.submitErr != nil {
		return "", f.submitErr
	}
	return "fake-batch-1", nil
}

func (f *fakeBatchClient) Result(_ context.Context, batchID string) (map[string]*BatchResult, error) {
	f.mu.Lock()
	f.resultRequests = append(f.resultRequests, batchID)
	f.mu.Unlock()

	if f.resultFn != nil {
		return f.resultFn(batchID)
	}
	if f.resultErr != nil {
		return nil, f.resultErr
	}
	return nil, errors.New("fakeBatchClient: no result configured")
}

// stubProvider is a streaming ProviderAdapter that replays a fixed sequence
// of StreamEvents. Used to assert FallbackOnTimeout pumps the inner.
type stubProvider struct {
	events []types.StreamEvent
	called atomic.Int64
}

func (s *stubProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	s.called.Add(1)
	ch := make(chan types.StreamEvent, len(s.events))
	go func() {
		defer close(ch)
		for _, ev := range s.events {
			ch <- ev
		}
	}()
	return ch, nil
}

// drain reads everything from ch with a generous wall-clock cap so a hung
// goroutine fails the test rather than wedging the whole suite.
func drain(t *testing.T, ch <-chan types.StreamEvent) []types.StreamEvent {
	t.Helper()
	deadline := time.After(2 * time.Second)
	var out []types.StreamEvent
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			require.FailNowf(t, "timed out waiting for channel close", "drain: collected %d events", len(out))
		}
	}
}

// -----------------------------------------------------------------------------
// fabricateStream
// -----------------------------------------------------------------------------

func TestFabricateStream_AnthropicTextAndToolUse(t *testing.T) {
	response := []byte(`{
		"content": [
			{"type": "text", "text": "hello "},
			{"type": "text", "text": "world"},
			{"type": "tool_use", "id": "tu_1", "name": "read_file", "input": {"path": "/etc/hosts"}}
		],
		"stop_reason": "tool_use",
		"usage": {"output_tokens": 42}
	}`)

	ch := make(chan types.StreamEvent, 8)
	fabricateStream(ch, response, "anthropic")
	close(ch)

	var got []types.StreamEvent
	for ev := range ch {
		got = append(got, ev)
	}

	require.Len(t, got, 4, "expected 2 text_delta, 1 tool_call and 1 message_complete events: %+v", got)
	require.Equal(t, "text_delta", got[0].Type, "event 0 type")
	require.Equal(t, "hello ", got[0].Text, "event 0 text")
	require.Equal(t, "text_delta", got[1].Type, "event 1 type")
	require.Equal(t, "world", got[1].Text, "event 1 text")
	require.Equal(t, "tool_call", got[2].Type, "event 2 type")
	require.Equal(t, "tu_1", got[2].ID, "event 2 id")
	require.Equal(t, "read_file", got[2].Name, "event 2 name")
	require.Equal(t, "/etc/hosts", got[2].Input["path"], "event 2 input path")
	require.Equal(t, "message_complete", got[3].Type, "event 3 type")
	require.Equal(t, "tool_use", got[3].StopReason, "event 3 stop reason")
	require.Equal(t, 42, got[3].OutputTokens, "event 3 output tokens")
	// message_complete carries the assembled content blocks the
	// streaming consumer would have produced for the same response.
	require.Len(t, got[3].Content, 3, "event 3 content blocks")
}

// TestFabricateStream_OpenAIChatCompletions covers the mapping of an
// OpenAI Chat Completions response body (the JSON the streaming SSE
// path consumes via consumeSSE) into the StreamEvent sequence the
// agentic loop expects. Text content + tool_calls + finish_reason +
// usage.completion_tokens together exercise the four-event happy path.
func TestFabricateStream_OpenAIChatCompletions(t *testing.T) {
	response := []byte(`{
		"choices": [
			{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": "hello world",
					"tool_calls": [
						{"id": "call_1", "type": "function",
						 "function": {"name": "read_file", "arguments": "{\"path\":\"/etc/hosts\"}"}}
					]
				},
				"finish_reason": "tool_calls"
			}
		],
		"usage": {"prompt_tokens": 5, "completion_tokens": 42}
	}`)

	ch := make(chan types.StreamEvent, 8)
	fabricateStream(ch, response, "openai-compatible")
	close(ch)

	var got []types.StreamEvent
	for ev := range ch {
		got = append(got, ev)
	}
	require.Len(t, got, 3, "expected text_delta, tool_call and message_complete events: %+v", got)
	require.Equal(t, "text_delta", got[0].Type, "event 0 type")
	require.Equal(t, "hello world", got[0].Text, "event 0 text")
	require.Equal(t, "tool_call", got[1].Type, "event 1 type")
	require.Equal(t, "call_1", got[1].ID, "event 1 id")
	require.Equal(t, "read_file", got[1].Name, "event 1 name")
	require.Equal(t, "/etc/hosts", got[1].Input["path"], "event 1 input path")
	require.Equal(t, "message_complete", got[2].Type, "event 2 type")
	// mapFinishReason converts "tool_calls" to "tool_use" — match
	// the streaming adapter's stop-reason vocabulary.
	require.Equal(t, "tool_use", got[2].StopReason, "event 2 stop reason")
	require.Equal(t, 42, got[2].OutputTokens, "event 2 output tokens")
}

// TestFabricateStream_OpenAIChatCompletions_TextOnly pins the
// text-only happy path (no tool_calls, finish_reason=stop): one
// text_delta + one message_complete with stop_reason mapped to
// end_turn.
func TestFabricateStream_OpenAIChatCompletions_TextOnly(t *testing.T) {
	response := []byte(`{
		"choices": [
			{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}
		],
		"usage": {"completion_tokens": 1}
	}`)
	ch := make(chan types.StreamEvent, 4)
	fabricateStream(ch, response, "openai-compatible")
	close(ch)
	var got []types.StreamEvent
	for ev := range ch {
		got = append(got, ev)
	}
	require.Len(t, got, 2, "unexpected events: %+v", got)
	require.Equal(t, "text_delta", got[0].Type)
	require.Equal(t, "message_complete", got[1].Type)
	require.Equal(t, "end_turn", got[1].StopReason, "stop reason")
}

// TestFabricateStream_OpenAIChatCompletions_NoChoices covers the
// defensive branch when the response body is well-formed JSON but
// carries no choices array (an upstream contract break). Surfacing
// the error keeps the failure debuggable instead of silently emitting
// only a message_complete.
func TestFabricateStream_OpenAIChatCompletions_NoChoices(t *testing.T) {
	ch := make(chan types.StreamEvent, 1)
	fabricateStream(ch, []byte(`{"choices":[]}`), "openai-compatible")
	close(ch)
	got := <-ch
	require.Equal(t, "error", got.Type, "expected error event, got %+v", got)
}

// TestFabricateStream_OpenAIResponses covers the Responses-API
// response shape: output[] items of type message (with output_text
// content blocks) and function_call (flat id/name/arguments). The
// derived stop reason picks tool_use when any function_call item is
// present, matching openai_responses.go::deriveStopReason.
func TestFabricateStream_OpenAIResponses(t *testing.T) {
	response := []byte(`{
		"status": "completed",
		"output": [
			{"type": "message", "id": "msg_1",
			 "content": [{"type": "output_text", "text": "hi"}]},
			{"type": "function_call", "id": "fc_1", "call_id": "call_1",
			 "name": "read_file", "arguments": "{\"path\":\"/etc/hosts\"}"}
		],
		"usage": {"input_tokens": 3, "output_tokens": 12}
	}`)

	ch := make(chan types.StreamEvent, 8)
	fabricateStream(ch, response, "openai-responses")
	close(ch)
	var got []types.StreamEvent
	for ev := range ch {
		got = append(got, ev)
	}
	require.Len(t, got, 3, "expected text_delta, tool_call and message_complete events: %+v", got)
	require.Equal(t, "text_delta", got[0].Type, "event 0 type")
	require.Equal(t, "hi", got[0].Text, "event 0 text")
	require.Equal(t, "tool_call", got[1].Type, "event 1 type")
	require.Equal(t, "call_1", got[1].ID, "event 1 id")
	require.Equal(t, "read_file", got[1].Name, "event 1 name")
	require.Equal(t, "/etc/hosts", got[1].Input["path"], "event 1 input path")
	require.Equal(t, "message_complete", got[2].Type, "event 2 type")
	require.Equal(t, "tool_use", got[2].StopReason, "event 2 stop reason")
	require.Equal(t, 12, got[2].OutputTokens, "event 2 output tokens")
}

// TestFabricateStream_OpenAIResponses_Incomplete covers the incomplete
// branch of deriveOpenAIResponsesStopReason: a status=incomplete
// response with reason=max_output_tokens maps to the harness-side
// max_tokens vocabulary so the agentic loop's truncation handling
// fires.
func TestFabricateStream_OpenAIResponses_Incomplete(t *testing.T) {
	response := []byte(`{
		"status": "incomplete",
		"incomplete_details": {"reason": "max_output_tokens"},
		"output": [{"type": "message", "content": [{"type": "output_text", "text": "partial"}]}],
		"usage": {"output_tokens": 64}
	}`)
	ch := make(chan types.StreamEvent, 4)
	fabricateStream(ch, response, "openai-responses")
	close(ch)
	var got []types.StreamEvent
	for ev := range ch {
		got = append(got, ev)
	}
	require.Len(t, got, 2, "unexpected events: %+v", got)
	require.Equal(t, "message_complete", got[1].Type)
	require.Equal(t, "max_tokens", got[1].StopReason, "stop reason")
}

// TestBatchAdapter_marshalRequestBody_OpenAICompatible pins the
// openai-compatible arm of marshalRequestBody: the body is a Chat
// Completions request with stream set to false.
func TestBatchAdapter_marshalRequestBody_OpenAICompatible(t *testing.T) {
	a := NewBatchAdapter(nil, &fakeBatchClient{}, &types.BatchProviderConfig{Enabled: true}, "openai-compatible", "run-test")
	body, err := a.marshalRequestBody(types.StreamParams{
		Model:     "gpt-4o-mini",
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens: 256,
	})
	require.NoError(t, err, "marshalRequestBody")
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &raw), "decode marshalled body")
	require.Contains(t, raw, "model", "marshalled body: %s", body)
	require.Contains(t, raw, "stream", "openai-compatible body must carry stream=false: %s", body)
	require.Equal(t, "false", string(raw["stream"]), "openai-compatible stream")
}

// TestBatchAdapter_marshalRequestBody_AnthropicSendsNoCacheControl pins
// that a batch body carries a plain string system and no cache_control
// anywhere, even for a model the registry caches and a request carrying a
// cache key.
func TestBatchAdapter_marshalRequestBody_AnthropicSendsNoCacheControl(t *testing.T) {
	const model = "claude-sonnet-4-6"
	require.True(t, quirks.DefaultRegistry().Resolve("anthropic", model).BehaviourFlags.Anthropic.PromptCaching,
		"precondition: registry must enable PromptCaching for %s", model)
	a := NewBatchAdapter(nil, &fakeBatchClient{}, &types.BatchProviderConfig{Enabled: true}, "anthropic", "run-test")
	body, err := a.marshalRequestBody(types.StreamParams{
		Model:     model,
		System:    "sys",
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens: 256,
		CacheKey:  "k",
	})
	require.NoError(t, err, "marshalRequestBody")
	require.NotContains(t, string(body), "cache_control", "batch body carries cache_control: %s", body)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &raw), "decode marshalled body")
	require.Equal(t, `"sys"`, string(raw["system"]), "system must be a plain string")
	require.Equal(t, "false", string(raw["stream"]), "stream")
}

// TestBatchAdapter_MarshalUsesInjectedRegistry pins the Registry
// field plumbing: a BatchAdapter constructed with a registry that
// includes a compat rule (e.g. Z.ai's TokenFieldMaxTokens +
// tool_stream) must produce a batch body matching the rule's wire
// shape. Without this test the factory's
// `batchAdapter.Registry = compatible.Registry` line could be
// deleted and the batch path would silently fall back to the
// default registry — a future Z.ai-via-batch run would emit
// max_completion_tokens (wrong key) and omit tool_stream.
//
// The test injects a registry containing only a synthetic compat-
// like rule rather than depending on the real Z.ai package, to
// keep batch_test self-contained.
func TestBatchAdapter_MarshalUsesInjectedRegistry(t *testing.T) {
	customRule := quirks.Rule{
		ProviderType: "openai-compatible",
		ModelMatch:   "synthetic-*",
		Description:  "test rule: max_tokens + tool_stream",
		LastVerified: quirks.Date("2026-05-24"),
		Apply: func(q *quirks.ProviderQuirks) {
			q.BehaviourFlags.OpenAI.TokenField = quirks.TokenFieldMaxTokens
			q.BehaviourFlags.OpenAI.ExtraBodyFields["tool_stream"] = true
		},
	}
	a := NewBatchAdapter(nil, &fakeBatchClient{}, &types.BatchProviderConfig{Enabled: true}, "openai-compatible", "run-test")
	a.Registry = quirks.NewRegistry([]quirks.Rule{customRule})

	body, err := a.marshalRequestBody(types.StreamParams{
		Model:     "synthetic-foo",
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens: 256,
	})
	require.NoError(t, err, "marshalRequestBody")
	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw), "decode body")
	require.Contains(t, raw, "max_tokens", "injected registry rule did not apply: %s", body)
	require.NotContains(t, raw, "max_completion_tokens", "injected rule should have switched the key: %s", body)
	require.Equal(t, true, raw["tool_stream"], "injected rule did not populate ExtraBodyFields: %s", body)
}

// TestBatchAdapter_MarshalFallsBackToDefaultRegistryWhenNil pins the
// nil-registry fallback in marshalRequestBody. The vast majority of
// BatchAdapter call sites (every test in the package, every existing
// production code path) leave Registry unset; the fallback to
// quirks.DefaultRegistry() preserves their behaviour.
func TestBatchAdapter_MarshalFallsBackToDefaultRegistryWhenNil(t *testing.T) {
	a := NewBatchAdapter(nil, &fakeBatchClient{}, &types.BatchProviderConfig{Enabled: true}, "openai-compatible", "run-test")
	// Registry intentionally left nil.
	body, err := a.marshalRequestBody(types.StreamParams{
		Model:     "o1-mini", // matches the builtin reasoning-class rule
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens: 256,
	})
	require.NoError(t, err, "marshalRequestBody")
	require.Contains(t, string(body), `"max_completion_tokens"`, "nil-registry fallback failed: %s", body)
}

// TestBatchAdapter_marshalRequestBody_OpenAIResponses pins the
// openai-responses arm of marshalRequestBody: the marshalled JSON is the
// Responses wire body shape, model present, stream key absent (omitempty
// stripping in buildResponsesRequest).
func TestBatchAdapter_marshalRequestBody_OpenAIResponses(t *testing.T) {
	a := NewBatchAdapter(nil, &fakeBatchClient{}, &types.BatchProviderConfig{Enabled: true}, "openai-responses", "run-test")
	body, err := a.marshalRequestBody(types.StreamParams{
		Model:     "gpt-4o-mini",
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens: 256,
	})
	require.NoError(t, err, "marshalRequestBody")
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &raw), "decode marshalled body")
	require.Contains(t, raw, "model", "marshalled body: %s", body)
	require.NotContains(t, raw, "stream", "openai-responses body must not carry stream key (omitempty), got: %s", body)
}

// TestBatchAdapter_marshalRequestBody_OpenAIResponsesReasoningParity pins
// that a batch request carries the same quirk-driven fields as a streamed
// one: the encrypted-reasoning include, strict tools, and verbatim replay
// of a stored turn from the inner adapter's origin, all only on a
// reasoning model.
func TestBatchAdapter_marshalRequestBody_OpenAIResponsesReasoningParity(t *testing.T) {
	const baseURL = "https://api.example.test/v1"
	cases := []struct {
		model  string
		gated  bool
		strict bool
	}{
		{model: "gpt-5.6-sol", gated: true, strict: true},
		{model: "gpt-4o", gated: false, strict: false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			inner := NewOpenAIResponsesAdapter(staticBearer("test-key"), baseURL, OpenAIAuthConfig{})
			a := NewBatchAdapter(inner, &fakeBatchClient{}, &types.BatchProviderConfig{Enabled: true}, "openai-responses", "run-test")
			assistant := storedTurn(t, responsesReplayOrigin(tc.model, baseURL),
				[]types.ContentBlock{{Type: "tool_use", ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"a.go"}`)}},
				`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc-batch"}`,
				`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.go\"}","status":"completed"}`)
			body, err := a.marshalRequestBody(types.StreamParams{
				Model: tc.model,
				Messages: []types.Message{
					replayUserPrompt,
					assistant,
					{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "call_1", Content: "package a"}}},
				},
				Tools: []types.ToolDefinition{{
					Name:        "read_file",
					Description: "Read a file",
					InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
				}},
				MaxTokens: 256,
			})
			require.NoError(t, err, "marshalRequestBody")
			s := string(body)
			for field, want := range map[string]bool{
				`"include":["reasoning.encrypted_content"]`: tc.gated,
				`"strict":true`:                   tc.strict,
				`"encrypted_content":"enc-batch"`: tc.gated,
				`"id":"fc_1"`:                     tc.gated,
			} {
				assert.Equal(t, want, strings.Contains(s, field), "body contains %s: %s", field, s)
			}
			assertCallsPaired(t, requestInput(t, body))
		})
	}
}

// TestFabricateStream_OpenAIResponses_CarriesNoReplayFields pins that batch
// results carry no stored output items, so a batched turn is reconstructed on
// the next request even on a reasoning model.
func TestFabricateStream_OpenAIResponses_CarriesNoReplayFields(t *testing.T) {
	response := []byte(`{
		"status": "completed",
		"output": [
			{"type": "reasoning", "id": "rs_1", "summary": [], "encrypted_content": "enc"},
			{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed",
			 "content": [{"type": "output_text", "text": "hi"}]}
		]
	}`)
	ch := make(chan types.StreamEvent, 4)
	fabricateStream(ch, response, "openai-responses")
	close(ch)
	sawComplete := false
	for ev := range ch {
		if ev.Type == "message_complete" {
			sawComplete = true
		}
		assert.Nil(t, ev.ReplayFields, "%s event carries ReplayFields", ev.Type)
	}
	require.True(t, sawComplete, "no message_complete event")
}

// TestDeriveOpenAIResponsesStopReason_MapsStatusAndReason pins the full
// status × incomplete-reason × has-tool matrix for the batch path's
// deriveOpenAIResponsesStopReason.
func TestDeriveOpenAIResponsesStopReason_MapsStatusAndReason(t *testing.T) {
	mkIncomplete := func(reason string) *struct {
		Reason string `json:"reason"`
	} {
		return &struct {
			Reason string `json:"reason"`
		}{Reason: reason}
	}

	tests := []struct {
		name    string
		resp    openaiResponsesBatchResponse
		hasTool bool
		want    string
	}{
		{
			name: "completed/no-tool",
			resp: openaiResponsesBatchResponse{Status: "completed"},
			want: "end_turn",
		},
		{
			name:    "completed/with-tool",
			resp:    openaiResponsesBatchResponse{Status: "completed"},
			hasTool: true,
			want:    "tool_use",
		},
		{
			name: "incomplete/max_output_tokens",
			resp: openaiResponsesBatchResponse{
				Status:            "incomplete",
				IncompleteDetails: mkIncomplete("max_output_tokens"),
			},
			want: "max_tokens",
		},
		{
			name: "incomplete/max_tokens alias",
			resp: openaiResponsesBatchResponse{
				Status:            "incomplete",
				IncompleteDetails: mkIncomplete("max_tokens"),
			},
			want: "max_tokens",
		},
		{
			name: "incomplete/content_filter (non-standard reason verbatim)",
			resp: openaiResponsesBatchResponse{
				Status:            "incomplete",
				IncompleteDetails: mkIncomplete("content_filter"),
			},
			want: "content_filter",
		},
		{
			name: "incomplete/nil-details",
			resp: openaiResponsesBatchResponse{Status: "incomplete"},
			want: "incomplete",
		},
		{
			name: "incomplete/empty-reason",
			resp: openaiResponsesBatchResponse{
				Status:            "incomplete",
				IncompleteDetails: mkIncomplete(""),
			},
			want: "incomplete",
		},
		{
			name: "default/non-empty-unknown status verbatim",
			resp: openaiResponsesBatchResponse{Status: "failed"},
			want: "failed",
		},
		{
			name:    "default/empty status with tool",
			resp:    openaiResponsesBatchResponse{Status: ""},
			hasTool: true,
			want:    "tool_use",
		},
		{
			name: "default/empty status no tool",
			resp: openaiResponsesBatchResponse{Status: ""},
			want: "end_turn",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveOpenAIResponsesStopReason(tc.resp, tc.hasTool)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestFabricateStream_UnsupportedProviderEmitsError(t *testing.T) {
	ch := make(chan types.StreamEvent, 1)
	fabricateStream(ch, []byte(`{}`), "bedrock")
	close(ch)
	got := <-ch
	require.Equal(t, "error", got.Type)
	require.ErrorContains(t, got.Error, "unsupported provider type")
}

// -----------------------------------------------------------------------------
// BatchAdapter
// -----------------------------------------------------------------------------

func batchAdapter(t *testing.T, client BatchClient, cfg *types.BatchProviderConfig, inner ProviderAdapter) *BatchAdapter {
	t.Helper()
	return NewBatchAdapter(inner, client, cfg, "anthropic", "run-test")
}

func anthropicParams() types.StreamParams {
	return types.StreamParams{
		Model:     "claude-3-5-sonnet-20241022",
		System:    "be brief",
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens: 256,
	}
}

func TestBatchAdapter_Stream_HappyPath(t *testing.T) {
	response := json.RawMessage(`{
		"content": [{"type": "text", "text": "hello"}],
		"stop_reason": "end_turn",
		"usage": {"output_tokens": 7}
	}`)
	client := &fakeBatchClient{
		resultFn: func(batchID string) (map[string]*BatchResult, error) {
			return map[string]*BatchResult{
				"stirrup-run-test-turn-1": {Response: response},
			}, nil
		},
	}

	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)
	ch, err := a.Stream(context.Background(), anthropicParams())
	require.NoError(t, err)
	events := drain(t, ch)

	require.Len(t, events, 2)
	require.Equal(t, "text_delta", events[0].Type)
	require.Equal(t, "hello", events[0].Text)
	require.Equal(t, "message_complete", events[1].Type)
	require.Equal(t, "end_turn", events[1].StopReason)

	client.mu.Lock()
	defer client.mu.Unlock()
	require.Len(t, client.submitted, 1)
	require.Len(t, client.submitted[0], 1)
	require.Equal(t, "stirrup-run-test-turn-1", client.submitted[0][0].CustomID)
	require.Equal(t, "anthropic", client.submitted[0][0].Provider)
}

func TestBatchAdapter_Stream_TurnCounterIncrements(t *testing.T) {
	client := &fakeBatchClient{
		resultFn: func(batchID string) (map[string]*BatchResult, error) {
			return map[string]*BatchResult{}, nil // missing -> error, but we only inspect submitted
		},
		submitFn: func(entries []BatchEntry) (string, error) { return "id", nil },
	}

	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)
	for i := 0; i < 3; i++ {
		ch, err := a.Stream(context.Background(), anthropicParams())
		require.NoError(t, err, "Stream %d", i)
		drain(t, ch)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	require.Len(t, client.submitted, 3)
	for i, batch := range client.submitted {
		want := fmt.Sprintf("stirrup-run-test-turn-%d", i+1)
		require.Equal(t, want, batch[0].CustomID, "submit %d", i)
	}
}

func TestBatchAdapter_Stream_SubmitError(t *testing.T) {
	client := &fakeBatchClient{submitErr: errors.New("network unreachable")}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)

	ch, err := a.Stream(context.Background(), anthropicParams())
	require.NoError(t, err, "submit failures surface via the channel")
	events := drain(t, ch)
	require.Len(t, events, 1)
	require.Equal(t, "error", events[0].Type)
	require.ErrorContains(t, events[0].Error, "network unreachable")
}

// TestBatchAdapter_LastBatchID_EmptyBeforeSubmit pins the contract
// documented on LastBatchID(): readers (the agentic loop) must see an
// empty string when no batch has been submitted yet, so a
// streaming-only fallback path can detect the absence cleanly.
func TestBatchAdapter_LastBatchID_EmptyBeforeSubmit(t *testing.T) {
	a := batchAdapter(t, &fakeBatchClient{}, &types.BatchProviderConfig{Enabled: true}, nil)
	require.Empty(t, a.LastBatchID(), "LastBatchID before Submit")
}

// TestBatchAdapter_LastBatchID_PopulatedAfterSubmit confirms the
// adapter surfaces the provider-assigned batch identifier returned
// from Submit so the agentic loop can attach it to the TurnTrace.
// Both the control-plane handle ("batch-N") and the polling client's
// "msgbatch_..." flow through this single accessor unchanged.
func TestBatchAdapter_LastBatchID_PopulatedAfterSubmit(t *testing.T) {
	response := json.RawMessage(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"output_tokens":1}}`)
	client := &fakeBatchClient{
		submitFn: func(_ []BatchEntry) (string, error) { return "msgbatch_abc123", nil },
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return map[string]*BatchResult{"stirrup-run-test-turn-1": {Response: response}}, nil
		},
	}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)

	ch, err := a.Stream(context.Background(), anthropicParams())
	require.NoError(t, err)
	drain(t, ch)

	require.Equal(t, "msgbatch_abc123", a.LastBatchID())
}

// TestBatchAdapter_LastBatchID_UnchangedOnSubmitError pins the
// rationale called out in the field doc: a failed Submit leaves the
// previous batch identifier in place rather than clobbering with "".
// A loop that reads LastBatchID() on a fallback path then still
// references the most recent successful submission, not a transient
// failure that produced no provider-side batch.
func TestBatchAdapter_LastBatchID_UnchangedOnSubmitError(t *testing.T) {
	response := json.RawMessage(`{"content":[],"stop_reason":"end_turn","usage":{"output_tokens":0}}`)
	calls := 0
	client := &fakeBatchClient{
		submitFn: func(_ []BatchEntry) (string, error) {
			calls++
			if calls == 1 {
				return "msgbatch_first", nil
			}
			return "", errors.New("transient submit failure")
		},
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return map[string]*BatchResult{"stirrup-run-test-turn-1": {Response: response}}, nil
		},
	}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)

	ch, err := a.Stream(context.Background(), anthropicParams())
	require.NoError(t, err)
	drain(t, ch)
	require.Equal(t, "msgbatch_first", a.LastBatchID(), "after first Submit")

	// The second Submit fails; LastBatchID must still hold the previous value.
	ch2, err := a.Stream(context.Background(), anthropicParams())
	require.NoError(t, err, "second Stream")
	drain(t, ch2)
	require.Equal(t, "msgbatch_first", a.LastBatchID(), "after failed Submit")
}

func TestBatchAdapter_Stream_ResultError(t *testing.T) {
	client := &fakeBatchClient{
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return map[string]*BatchResult{
				"stirrup-run-test-turn-1": {Err: &BatchResultError{Type: "batch_cancelled", Message: "user aborted"}},
			}, nil
		},
	}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)
	ch, err := a.Stream(context.Background(), anthropicParams())
	require.NoError(t, err)
	events := drain(t, ch)
	require.Len(t, events, 1)
	require.Equal(t, "error", events[0].Type)
	require.ErrorContains(t, events[0].Error, "batch_cancelled")
	require.ErrorContains(t, events[0].Error, "user aborted")
}

func TestBatchAdapter_Stream_MissingEntry(t *testing.T) {
	client := &fakeBatchClient{
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return map[string]*BatchResult{"other-id": {Response: json.RawMessage(`{}`)}}, nil
		},
	}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)
	ch, _ := a.Stream(context.Background(), anthropicParams())
	events := drain(t, ch)
	require.Len(t, events, 1)
	require.Equal(t, "error", events[0].Type)
	require.ErrorContains(t, events[0].Error, "missing entry")
}

func TestBatchAdapter_Stream_CtxCancel(t *testing.T) {
	client := &fakeBatchClient{
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return nil, context.Canceled
		},
	}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch, err := a.Stream(ctx, anthropicParams())
	require.NoError(t, err)
	events := drain(t, ch)
	require.Len(t, events, 1)
	require.Equal(t, "error", events[0].Type)
	require.ErrorContains(t, events[0].Error, "cancelled")
}

func TestBatchAdapter_Stream_TimeoutWithoutFallback(t *testing.T) {
	client := &fakeBatchClient{
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return nil, fmt.Errorf("%w: simulated", errBatchExpired)
		},
	}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true, FallbackOnTimeout: false}, nil)
	ch, _ := a.Stream(context.Background(), anthropicParams())
	events := drain(t, ch)
	require.Len(t, events, 1)
	require.Equal(t, "error", events[0].Type)
	require.ErrorIs(t, events[0].Error, errBatchExpired)
}

func TestBatchAdapter_Stream_TimeoutFallback(t *testing.T) {
	client := &fakeBatchClient{
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return nil, fmt.Errorf("%w: simulated", errBatchExpired)
		},
	}
	inner := &stubProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "fallback"},
			{Type: "message_complete", StopReason: "end_turn", OutputTokens: 3},
		},
	}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true, FallbackOnTimeout: true}, inner)

	ch, _ := a.Stream(context.Background(), anthropicParams())
	events := drain(t, ch)

	require.Equal(t, int64(1), inner.called.Load(), "inner adapter calls")
	require.Len(t, events, 2)
	require.Equal(t, "text_delta", events[0].Type)
	require.Equal(t, "fallback", events[0].Text)
	require.Equal(t, "message_complete", events[1].Type)
}

// TestBatchAdapter_Stream_DeadRunDoesNotFallBack pins the boundary
// behaviour once maxWaitSeconds defaults to the run timeout: a wall-clock
// cap that fires against an already-cancelled run must report the
// cancellation rather than route into the streaming fallback, which could
// only fail on the same dead context.
func TestBatchAdapter_Stream_DeadRunDoesNotFallBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeBatchClient{
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			cancel()
			return nil, expiredOrCancelled(ctx, fmt.Errorf("%w: simulated", errBatchExpired))
		},
	}
	inner := &stubProvider{events: []types.StreamEvent{{Type: "text_delta", Text: "fallback"}}}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true, FallbackOnTimeout: true}, inner)

	ch, _ := a.Stream(ctx, anthropicParams())
	events := drain(t, ch)

	require.Zero(t, inner.called.Load(), "the streaming fallback must not run for a cancelled run")
	require.Len(t, events, 1)
	require.Equal(t, "error", events[0].Type)
	require.ErrorIs(t, events[0].Error, context.Canceled)
}

// TestExpiredOrCancelled_LiveVersusCancelledContext pins the tie-break the
// batch clients share: a live context keeps the errBatchExpired sentinel (so
// FallbackOnTimeout still routes), a dead one yields ctx.Err().
func TestExpiredOrCancelled_LiveVersusCancelledContext(t *testing.T) {
	expired := fmt.Errorf("%w: simulated", errBatchExpired)

	t.Run("live context keeps the expired sentinel", func(t *testing.T) {
		require.ErrorIs(t, expiredOrCancelled(context.Background(), expired), errBatchExpired)
	})

	t.Run("cancelled context yields context.Canceled", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		got := expiredOrCancelled(cancelled, expired)
		require.ErrorIs(t, got, context.Canceled)
		require.NotErrorIs(t, got, errBatchExpired, "a cancelled run must not surface the batch-expired sentinel")
	})
}

// TestBatchWaitCap_ArmsOnlyInsideRunDeadline pins which batch waits get a wall-clock cap at all.
// A cap at or beyond the run deadline is dropped so the run deadline is
// the only reachable outcome; only a cap strictly inside the run's
// remaining budget is armed, and only then can FallbackOnTimeout fire.
func TestBatchWaitCap_ArmsOnlyInsideRunDeadline(t *testing.T) {
	t.Run("no run deadline arms the cap", func(t *testing.T) {
		capAt, armed := batchWaitCap(context.Background(), time.Minute)
		require.True(t, armed, "a context without a deadline must arm the cap")
		require.Positive(t, time.Until(capAt), "cap must be in the future, got %s", capAt)
	})

	t.Run("cap inside the run budget is armed", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		_, armed := batchWaitCap(ctx, time.Minute)
		require.True(t, armed, "a cap well inside the run deadline must be armed")
	})

	for _, tc := range []struct {
		name      string
		maxWait   time.Duration
		runBudget time.Duration
	}{
		{"cap equal to the run budget is dropped", time.Minute, time.Minute},
		{"cap beyond the run budget is dropped", time.Hour, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), tc.runBudget)
			defer cancel()
			_, armed := batchWaitCap(ctx, tc.maxWait)
			require.False(t, armed, "the cap must be dropped when the run deadline binds first")
		})
	}
}

// TestFabricateAnthropicStream_MalformedToolInput pins the
// tool_use input decode error path in fabricateAnthropicStream
// directly. The wider fabricateStream wrapper takes the error
// arm only after this layer has returned a non-nil error; testing
// the inner function isolates the failure to the input decode
// without depending on the wrapper's plumbing.
func TestFabricateAnthropicStream_MalformedToolInput(t *testing.T) {
	// Note: the tool_use Input field is json.RawMessage, so a
	// truncated object literal inside the message body forces the
	// unmarshal of Input itself to fail.
	response := []byte(`{
		"content": [
			{"type": "tool_use", "id": "tu_bad", "name": "read_file", "input": {"not-json`)

	ch := make(chan types.StreamEvent, 4)
	err := fabricateAnthropicStream(ch, response)
	require.ErrorContains(t, err, "decode")
}

// TestFabricateStream_AnthropicDecodeError covers the
// fabricateStream wrapper's error relay arm for the anthropic case:
// when fabricateAnthropicStream returns a non-nil error, the
// wrapper must emit a single error StreamEvent rather than swallow
// it (the agentic loop is the only consumer and expects all
// failures on-channel).
func TestFabricateStream_AnthropicDecodeError(t *testing.T) {
	// A response whose tool_use input field is a truncated JSON
	// value triggers fabricateAnthropicStream's decode error path,
	// which fabricateStream must relay to the channel.
	response := []byte(`{
		"content": [
			{"type": "tool_use", "id": "tu_bad", "name": "read_file", "input": {"path": "/etc/hosts`)

	ch := make(chan types.StreamEvent, 4)
	fabricateStream(ch, response, "anthropic")
	close(ch)

	var sawError bool
	for ev := range ch {
		if ev.Type == "error" && ev.Error != nil && strings.Contains(ev.Error.Error(), "decode") {
			sawError = true
		}
	}
	require.True(t, sawError, "expected a decode error StreamEvent from fabricateStream")
}

// TestBatchAdapter_Stream_ResultNonTransientError covers the
// awaitAndFabricate default error arm: a Result error that is
// neither errBatchExpired nor a context error must surface as a
// single "batch result:" StreamEvent. Without this guard, a
// provider-side rejection (rate limit, invalid model) could be
// swallowed or misclassified as a timeout.
func TestBatchAdapter_Stream_ResultNonTransientError(t *testing.T) {
	client := &fakeBatchClient{
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return nil, fmt.Errorf("provider-side rejection")
		},
	}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true}, nil)
	ch, err := a.Stream(context.Background(), anthropicParams())
	require.NoError(t, err)
	events := drain(t, ch)
	require.Len(t, events, 1)
	require.Equal(t, "error", events[0].Type)
	require.ErrorContains(t, events[0].Error, "batch result:")
	require.ErrorContains(t, events[0].Error, "provider-side rejection")
}

func TestBatchAdapter_Stream_UnsupportedProviderEmitsError(t *testing.T) {
	client := &fakeBatchClient{}
	a := NewBatchAdapter(nil, client, &types.BatchProviderConfig{Enabled: true}, "bedrock", "run-test")
	ch, err := a.Stream(context.Background(), anthropicParams())
	require.NoError(t, err, "unsupported provider surfaces via the channel")
	events := drain(t, ch)
	require.Len(t, events, 1)
	require.Equal(t, "error", events[0].Type)
	require.ErrorContains(t, events[0].Error, `unsupported provider type "bedrock"`)
	client.mu.Lock()
	defer client.mu.Unlock()
	require.Empty(t, client.submitted, "no submits for unsupported provider")
}

// -----------------------------------------------------------------------------
// controlPlaneBatchClient
// -----------------------------------------------------------------------------

type mockBatchTransport struct {
	mu       sync.Mutex
	emitted  []types.HarnessEvent
	handlers []func(types.ControlEvent)
	// emitErr, when non-nil and emitErrTypes is empty, causes every
	// Emit call to fail with that error. When emitErrTypes is populated,
	// only matching event types fail; others succeed. This lets a test
	// drive submission failures while still observing later cancel
	// emits, etc.
	emitErr      error
	emitErrTypes map[string]bool
}

func (m *mockBatchTransport) Emit(event types.HarnessEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.emitted = append(m.emitted, event)
	if m.emitErr != nil {
		if len(m.emitErrTypes) == 0 || m.emitErrTypes[event.Type] {
			return m.emitErr
		}
	}
	return nil
}

func (m *mockBatchTransport) OnControl(handler func(types.ControlEvent)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers = append(m.handlers, handler)
}

func (m *mockBatchTransport) Close() error { return nil }

func (m *mockBatchTransport) deliver(event types.ControlEvent) {
	m.mu.Lock()
	hs := make([]func(types.ControlEvent), len(m.handlers))
	copy(hs, m.handlers)
	m.mu.Unlock()
	for _, h := range hs {
		h(event)
	}
}

func (m *mockBatchTransport) lastEmitted() *types.HarnessEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.emitted) == 0 {
		return nil
	}
	e := m.emitted[len(m.emitted)-1]
	return &e
}

func (m *mockBatchTransport) emittedSnapshot() []types.HarnessEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]types.HarnessEvent, len(m.emitted))
	copy(out, m.emitted)
	return out
}

func TestControlPlaneBatchClient_SubmitAndResult(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, 5*time.Second, false)

	entry := BatchEntry{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{"model":"claude-3-5-sonnet-20241022"}`),
	}

	batchID, err := c.Submit(context.Background(), []BatchEntry{entry})
	require.NoError(t, err)

	em := tr.lastEmitted()
	require.NotNil(t, em, "expected batch_submission emitted")
	require.Equal(t, "batch_submission", em.Type)
	require.Equal(t, batchID, em.RequestID, "requestID echoed")
	var payload BatchSubmission
	require.NoError(t, json.Unmarshal(em.Input, &payload), "unmarshal submission payload")
	require.Equal(t, "anthropic", payload.ProviderType)
	require.Equal(t, "run-test-turn-1", payload.CustomID)
	require.Equal(t, string(entry.Body), string(payload.Body), "payload body")

	// Inject the matching batch_result; Result should return a map keyed
	// by the original custom_id.
	go func() {
		// Small delay so Result blocks first.
		time.Sleep(20 * time.Millisecond)
		tr.deliver(types.ControlEvent{
			Type:      "batch_result",
			RequestID: batchID,
			Content:   `{"response":{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"output_tokens":1}}}`,
		})
	}()

	results, err := c.Result(context.Background(), batchID)
	require.NoError(t, err)
	require.Contains(t, results, "run-test-turn-1", "expected result keyed by custom_id")
	got := results["run-test-turn-1"]
	require.Nil(t, got.Err, "expected success")
	require.Contains(t, string(got.Response), `"text":"hi"`)

	// A completed batch is released, so a second Result reports no pending submission.
	_, err = c.Result(context.Background(), batchID)
	require.ErrorContains(t, err, "no pending submission", "second Result")
}

func TestControlPlaneBatchClient_SubmitMultiEntryRejected(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Second, false)
	_, err := c.Submit(context.Background(), []BatchEntry{{}, {}})
	require.ErrorContains(t, err, "expected exactly 1 entry")
}

func TestControlPlaneBatchClient_Result_Timeout(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, 50*time.Millisecond, false)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	require.NoError(t, err)

	_, err = c.Result(context.Background(), batchID)
	require.ErrorIs(t, err, errBatchExpired)
	require.ErrorContains(t, err, batchID, "timeout error should name the batch")
}

// TestControlPlaneBatchClient_Result_DeadlineRacesCap arms the run
// deadline and the wall-clock cap at the same instant, the shape a run
// takes once maxWaitSeconds defaults to the run timeout. Whichever select
// arm Go picks, the result must classify as DeadlineExceeded and must not
// carry the batch-expired sentinel that routes into FallbackOnTimeout.
func TestControlPlaneBatchClient_Result_DeadlineRacesCap(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, 20*time.Millisecond, false)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err = c.Result(ctx, batchID)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, errBatchExpired, "a run past its deadline must not surface the batch-expired sentinel")
	require.ErrorContains(t, err, batchID)
}

func TestControlPlaneBatchClient_Result_ContextCancel(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Hour, false)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err = c.Result(ctx, batchID)
	require.ErrorIs(t, err, context.Canceled)
}

func TestControlPlaneBatchClient_Result_EmitsWaitingHeartbeats(t *testing.T) {
	prev := setBatchWaitingHeartbeatInterval(25 * time.Millisecond)
	t.Cleanup(func() { setBatchWaitingHeartbeatInterval(prev) })

	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Second, false)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	require.NoError(t, err)

	// Resolve the batch after enough ticks have fired so we can assert
	// at least two heartbeats land alongside the result.
	go func() {
		time.Sleep(90 * time.Millisecond)
		tr.deliver(types.ControlEvent{
			Type:      "batch_result",
			RequestID: batchID,
			Content:   `{"response":{"content":[],"stop_reason":"end_turn","usage":{"output_tokens":0}}}`,
		})
	}()

	_, err = c.Result(context.Background(), batchID)
	require.NoError(t, err)

	// Settle: give the heartbeat goroutine one extra tick to notice the
	// pending entry was removed and exit cleanly.
	time.Sleep(40 * time.Millisecond)

	var waiting int
	for _, ev := range tr.emittedSnapshot() {
		if ev.Type == "batch_waiting" && ev.RequestID == batchID {
			waiting++
		}
	}
	require.GreaterOrEqual(t, waiting, 2, "batch_waiting heartbeats (emitted=%+v)", tr.emittedSnapshot())
}

func TestControlPlaneBatchClient_IgnoresUnrelatedControlEvents(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Second, false)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	require.NoError(t, err)

	go func() {
		time.Sleep(20 * time.Millisecond)
		// Unrelated event types and wrong request IDs must not unblock
		// the Result wait.
		tr.deliver(types.ControlEvent{Type: "permission_response", RequestID: batchID})
		tr.deliver(types.ControlEvent{Type: "batch_result", RequestID: "wrong-id"})
		// Empty content surfaces a synthetic invalid_request_error so
		// the BatchAdapter still sees a non-nil entry.
		tr.deliver(types.ControlEvent{Type: "batch_result", RequestID: batchID})
	}()

	results, err := c.Result(context.Background(), batchID)
	require.NoError(t, err)
	require.Contains(t, results, "run-test-turn-1")
	got := results["run-test-turn-1"]
	require.NotNil(t, got)
	require.NotNil(t, got.Err, "expected synthetic invalid_request_error")
	require.Equal(t, "invalid_request_error", got.Err.Type)
}

// TestControlPlaneBatchClient_Result_BuffersEarlyDelivery exercises the race
// where handleControl delivers a batch_result before Result is even
// called: Result must return the buffered value rather than spuriously
// surfacing "no pending submission".
func TestControlPlaneBatchClient_Result_BuffersEarlyDelivery(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Second, false)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	require.NoError(t, err)

	// Deliver the result before Result is called; the buffered channel
	// holds the value until Result drains it.
	tr.deliver(types.ControlEvent{
		Type:      "batch_result",
		RequestID: batchID,
		Content:   `{"response":{"content":[],"stop_reason":"end_turn","usage":{"output_tokens":0}}}`,
	})

	results, err := c.Result(context.Background(), batchID)
	require.NoError(t, err, "Result after early delivery")
	require.Contains(t, results, "run-test-turn-1")
	got := results["run-test-turn-1"]
	require.NotNil(t, got)
	require.Nil(t, got.Err, "expected success")

	_, err = c.Result(context.Background(), batchID)
	require.ErrorContains(t, err, "no pending submission", "second Result")
}

// TestControlPlaneBatchClient_CancelBundle_TimeoutEmits: a timeout exit
// emits a batch_cancel_request when cancelBundleOnExit=true.
func TestControlPlaneBatchClient_CancelBundle_TimeoutEmits(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, 30*time.Millisecond, true)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	require.NoError(t, err)

	_, err = c.Result(context.Background(), batchID)
	require.ErrorIs(t, err, errBatchExpired)

	var cancel int
	for _, ev := range tr.emittedSnapshot() {
		if ev.Type == "batch_cancel_request" && ev.RequestID == batchID {
			cancel++
		}
	}
	require.Equal(t, 1, cancel, "batch_cancel_request count (emitted=%+v)", tr.emittedSnapshot())
}

// TestControlPlaneBatchClient_CancelBundle_CtxCancelEmits mirrors
// TestControlPlaneBatchClient_CancelBundle_TimeoutEmits on the ctx-cancel arm.
func TestControlPlaneBatchClient_CancelBundle_CtxCancelEmits(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Hour, true)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancel()
	}()
	_, err = c.Result(ctx, batchID)
	require.ErrorIs(t, err, context.Canceled)

	var cancelEvents int
	for _, ev := range tr.emittedSnapshot() {
		if ev.Type == "batch_cancel_request" && ev.RequestID == batchID {
			cancelEvents++
		}
	}
	require.Equal(t, 1, cancelEvents, "batch_cancel_request count (emitted=%+v)", tr.emittedSnapshot())
}

// TestControlPlaneBatchClient_CancelBundle_DisabledNoEmit: when
// cancelBundleOnExit=false, the cancel/timeout arms emit nothing beyond
// the original batch_submission.
func TestControlPlaneBatchClient_CancelBundle_DisabledNoEmit(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Hour, false)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Result(ctx, batchID)
	require.ErrorIs(t, err, context.Canceled)

	var cancelEvents []types.HarnessEvent
	for _, ev := range tr.emittedSnapshot() {
		if ev.Type == "batch_cancel_request" {
			cancelEvents = append(cancelEvents, ev)
		}
	}
	require.Empty(t, cancelEvents, "cancelBundleOnExit=false must not emit batch_cancel_request")
}

// TestDecodeBatchResult_MalformedJSON asserts a non-JSON content
// surfaces as a synthetic invalid_request_error rather than panicking.
func TestDecodeBatchResult_MalformedJSON(t *testing.T) {
	got := decodeBatchResult(types.ControlEvent{
		Type:      "batch_result",
		RequestID: "batch-1",
		Content:   "{not-json",
	})
	require.NotNil(t, got)
	require.NotNil(t, got.Err, "expected synthetic error")
	require.Equal(t, "invalid_request_error", got.Err.Type)
	require.Contains(t, got.Err.Message, "decode batch_result")
}

// TestDecodeBatchResult_RejectsOversizedContent asserts a Content payload above
// maxBatchResponseBytes surfaces as a synthetic invalid_request_error
// without attempting to decode the oversized blob.
func TestDecodeBatchResult_RejectsOversizedContent(t *testing.T) {
	oversized := strings.Repeat("a", maxBatchResponseBytes+1)
	got := decodeBatchResult(types.ControlEvent{
		Type:      "batch_result",
		RequestID: "batch-1",
		Content:   oversized,
	})
	require.NotNil(t, got)
	require.NotNil(t, got.Err, "expected synthetic error")
	require.Equal(t, "invalid_request_error", got.Err.Type)
	require.Contains(t, got.Err.Message, "exceeds")
}

// TestDecodeBatchResult_ContentIsCanonical walks the batch_result wire
// contract: content decides success or failure, is_error is only ever
// cross-checked, and every malformed or self-contradictory combination
// resolves to an invalid_request_error with a diagnostic naming what
// arrived.
func TestDecodeBatchResult_ContentIsCanonical(t *testing.T) {
	const successContent = `{"response":{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"output_tokens":1}}}`
	boolPtr := func(b bool) *bool { return &b }

	t.Run("success", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			isError *bool
		}{
			{"is_error absent", nil},
			{"is_error false", boolPtr(false)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := decodeBatchResult(types.ControlEvent{
					Type:      "batch_result",
					RequestID: "batch-1",
					Content:   successContent,
					IsError:   tc.isError,
				})
				require.Nil(t, got.Err, "expected success")
				require.Contains(t, string(got.Response), `"text":"hi"`, "decoded payload")
			})
		}
	})

	// Every error type in the taxonomy must survive the round trip
	// verbatim rather than being flattened into invalid_request_error.
	t.Run("error types", func(t *testing.T) {
		for _, errType := range []string{
			"batch_expired", "batch_cancelled", "invalid_request_error", "server_error",
		} {
			for _, tc := range []struct {
				name    string
				isError *bool
			}{
				{"is_error absent", nil},
				{"is_error true", boolPtr(true)},
			} {
				t.Run(errType+"/"+tc.name, func(t *testing.T) {
					got := decodeBatchResult(types.ControlEvent{
						Type:      "batch_result",
						RequestID: "batch-1",
						Content:   fmt.Sprintf(`{"err":{"type":%q,"message":"upstream said so"}}`, errType),
						IsError:   tc.isError,
					})
					require.NotNil(t, got.Err, "expected an error entry")
					require.Equal(t, errType, got.Err.Type)
					require.Equal(t, "upstream said so", got.Err.Message)
				})
			}
		}
	})

	t.Run("rejected", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			content     string
			isError     *bool
			wantSubstrs []string
		}{
			{
				name:        "missing content",
				content:     "",
				wantSubstrs: []string{"missing content"},
			},
			{
				name:        "neither response nor err",
				content:     `{}`,
				wantSubstrs: []string{"neither response nor err"},
			},
			{
				name:        "neither with is_error true",
				content:     `{}`,
				isError:     boolPtr(true),
				wantSubstrs: []string{"neither response nor err"},
			},
			{
				name:        "neither with is_error false",
				content:     `{}`,
				isError:     boolPtr(false),
				wantSubstrs: []string{"neither response nor err"},
			},
			{
				// json.RawMessage stores a JSON null as four bytes, so a
				// bare length test would read this as a success and
				// fabricate an empty assistant turn.
				name:        "null response counts as absent",
				content:     `{"response":null}`,
				wantSubstrs: []string{"neither response nor err"},
			},
			{
				name:        "both response and err",
				content:     `{"response":{"content":[]},"err":{"type":"server_error"}}`,
				wantSubstrs: []string{"both response and err", `"server_error"`},
			},
			{
				name:        "both with is_error true",
				content:     `{"response":{"content":[]},"err":{"type":"server_error"}}`,
				isError:     boolPtr(true),
				wantSubstrs: []string{"both response and err"},
			},
			{
				name:        "both with is_error false",
				content:     `{"response":{"content":[]},"err":{"type":"server_error"}}`,
				isError:     boolPtr(false),
				wantSubstrs: []string{"both response and err"},
			},
			{
				name:        "is_error true with success payload",
				content:     successContent,
				isError:     boolPtr(true),
				wantSubstrs: []string{"is_error=true", "a success response"},
			},
			{
				name:        "is_error false with error payload",
				content:     `{"err":{"type":"batch_expired","message":"gone"}}`,
				isError:     boolPtr(false),
				wantSubstrs: []string{"is_error=false", `err.type="batch_expired"`},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := decodeBatchResult(types.ControlEvent{
					Type:      "batch_result",
					RequestID: "batch-1",
					Content:   tc.content,
					IsError:   tc.isError,
				})
				require.NotNil(t, got)
				require.NotNil(t, got.Err, "expected a synthetic error")
				require.Equal(t, "invalid_request_error", got.Err.Type)
				require.Empty(t, got.Response, "a rejected entry must not carry a response")
				for _, want := range tc.wantSubstrs {
					require.Contains(t, got.Err.Message, want)
				}
			})
		}
	})
}

// TestControlPlaneBatchClient_ContradictoryIsErrorReachesResult drives a
// contradicting batch_result through the transport rather than calling
// decodeBatchResult directly, pinning that handleControl routes the
// rejection to the waiting Result caller.
func TestControlPlaneBatchClient_ContradictoryIsErrorReachesResult(t *testing.T) {
	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Second, false)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	require.NoError(t, err)

	isError := true
	tr.deliver(types.ControlEvent{
		Type:      "batch_result",
		RequestID: batchID,
		Content:   `{"response":{"content":[],"stop_reason":"end_turn","usage":{"output_tokens":0}}}`,
		IsError:   &isError,
	})

	results, err := c.Result(context.Background(), batchID)
	require.NoError(t, err)
	require.Contains(t, results, "run-test-turn-1")
	got := results["run-test-turn-1"]
	require.NotNil(t, got)
	require.NotNil(t, got.Err, "expected invalid_request_error")
	require.Equal(t, "invalid_request_error", got.Err.Type)
	require.Contains(t, got.Err.Message, "is_error=true", "message should name the contradicting flag")
}

// TestControlPlaneBatchClient_Submit_EmitFailureDropsPending drives an Emit
// failure on batch_submission and asserts the pending entry was dropped
// (no leak; subsequent Result reports "no pending submission").
func TestControlPlaneBatchClient_Submit_EmitFailureDropsPending(t *testing.T) {
	tr := &mockBatchTransport{
		emitErr:      errors.New("simulated emit failure"),
		emitErrTypes: map[string]bool{"batch_submission": true},
	}
	c := NewControlPlaneBatchClient(tr, time.Second, false)

	batchID, err := c.Submit(context.Background(), []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	require.ErrorContains(t, err, "simulated emit failure")
	require.Empty(t, batchID, "Submit must return no batch ID on failure")

	emitted := tr.emittedSnapshot()
	require.Len(t, emitted, 1, "emitted=%+v", emitted)
	require.Equal(t, "batch_submission", emitted[0].Type)
	requestID := emitted[0].RequestID

	_, err = c.Result(context.Background(), requestID)
	require.ErrorContains(t, err, "no pending submission")
}

// TestControlPlaneBatchClient_Heartbeat_StopsOnCtxCancel asserts the
// heartbeat goroutine terminates promptly when its context is cancelled
// before any tick has fired.
func TestControlPlaneBatchClient_Heartbeat_StopsOnCtxCancel(t *testing.T) {
	// Use a long interval so the ticker cannot fire during the test
	// window; the goroutine should exit via the ctx.Done() arm.
	prev := setBatchWaitingHeartbeatInterval(time.Hour)
	t.Cleanup(func() { setBatchWaitingHeartbeatInterval(prev) })

	tr := &mockBatchTransport{}
	c := NewControlPlaneBatchClient(tr, time.Hour, false)

	ctx, cancel := context.WithCancel(context.Background())
	batchID, err := c.Submit(ctx, []BatchEntry{{
		CustomID: "run-test-turn-1",
		Provider: "anthropic",
		Body:     json.RawMessage(`{}`),
	}})
	require.NoError(t, err)
	cancel()
	c.releasePending(batchID)

	// Settle. With the long interval, no batch_waiting events should
	// have been emitted; only the original batch_submission is on the
	// wire.
	time.Sleep(20 * time.Millisecond)
	var waiting []types.HarnessEvent
	for _, ev := range tr.emittedSnapshot() {
		if ev.Type == "batch_waiting" {
			waiting = append(waiting, ev)
		}
	}
	require.Empty(t, waiting, "batch_waiting emitted before ticker fired")
}

// TestBatchAdapter_Stream_TimeoutFallback_InnerStreamError asserts that
// when FallbackOnTimeout fires and the inner adapter itself returns an
// error, the BatchAdapter surfaces a single error event.
func TestBatchAdapter_Stream_TimeoutFallback_InnerStreamError(t *testing.T) {
	client := &fakeBatchClient{
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return nil, fmt.Errorf("%w: simulated", errBatchExpired)
		},
	}
	inner := &erroringProvider{err: errors.New("upstream provider down")}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true, FallbackOnTimeout: true}, inner)

	ch, _ := a.Stream(context.Background(), anthropicParams())
	events := drain(t, ch)

	require.Len(t, events, 1, "events: %+v", events)
	require.Equal(t, "error", events[0].Type)
	require.ErrorContains(t, events[0].Error, "batch fallback to streaming failed")
}

// erroringProvider is a ProviderAdapter that always fails on Stream.
type erroringProvider struct{ err error }

func (e *erroringProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	return nil, e.err
}

// TestBatchAdapter_Stream_TimeoutFallback_CtxCancelDuringRelay asserts
// the channel closes cleanly when ctx is cancelled mid-relay.
func TestBatchAdapter_Stream_TimeoutFallback_CtxCancelDuringRelay(t *testing.T) {
	client := &fakeBatchClient{
		resultFn: func(_ string) (map[string]*BatchResult, error) {
			return nil, fmt.Errorf("%w: simulated", errBatchExpired)
		},
	}
	inner := &blockingProvider{first: types.StreamEvent{Type: "text_delta", Text: "first"}, release: make(chan struct{})}
	a := batchAdapter(t, client, &types.BatchProviderConfig{Enabled: true, FallbackOnTimeout: true}, inner)

	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := a.Stream(ctx, anthropicParams())

	// Read the first event from the relay, then cancel.
	deadline := time.After(2 * time.Second)
	select {
	case ev, ok := <-ch:
		require.True(t, ok, "channel closed before first event")
		require.Equal(t, "text_delta", ev.Type)
		require.Equal(t, "first", ev.Text)
	case <-deadline:
		require.Fail(t, "timed out waiting for first event from inner")
	}

	cancel()
	close(inner.release)

	// The relay may or may not send the second event after cancel; the
	// channel must close without wedging either way.
	deadline = time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			require.Fail(t, "channel did not close after ctx cancel")
		}
	}
}

// blockingProvider emits `first`, then waits for release to close
// before emitting a second event and closing the channel. Used to drive
// the ctx-cancel-during-relay path.
type blockingProvider struct {
	first   types.StreamEvent
	release chan struct{}
}

func (b *blockingProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent, 1)
	go func() {
		defer close(ch)
		ch <- b.first
		<-b.release
		ch <- types.StreamEvent{Type: "text_delta", Text: "second"}
	}()
	return ch, nil
}

// TestFabricateStream_AnthropicMatchesReferenceSequence compares the
// fabricator's output against a hand-rolled reference sequence for a
// single text + tool_use fixture. Guards against drift between the SSE
// consumer (consumeSSE in anthropic.go) and the batch fabricator.
func TestFabricateStream_AnthropicMatchesReferenceSequence(t *testing.T) {
	response := []byte(`{
		"content": [
			{"type": "text", "text": "hi"},
			{"type": "tool_use", "id": "tu_a", "name": "read_file", "input": {"path": "/etc/hosts"}}
		],
		"stop_reason": "tool_use",
		"usage": {"output_tokens": 5}
	}`)

	ch := make(chan types.StreamEvent, 8)
	require.NoError(t, fabricateAnthropicStream(ch, response))
	close(ch)
	got := drain(t, ch)

	// Reference sequence: one text_delta per text block, one tool_call
	// per tool_use block, then a single message_complete carrying the
	// assembled content blocks.
	want := []types.StreamEvent{
		{Type: "text_delta", Text: "hi"},
		{Type: "tool_call", ID: "tu_a", Name: "read_file", Input: map[string]any{"path": "/etc/hosts"}},
		{Type: "message_complete", StopReason: "tool_use", OutputTokens: 5, Content: []types.ContentBlock{
			{Type: "text", Text: "hi"},
			{Type: "tool_use", ID: "tu_a", Name: "read_file", Input: json.RawMessage(`{"path": "/etc/hosts"}`)},
		}},
	}
	require.Len(t, got, len(want))
	for i := range want {
		t.Run(fmt.Sprintf("event %d %s", i, want[i].Type), func(t *testing.T) {
			require.Equal(t, want[i].Type, got[i].Type)
			require.Equal(t, want[i].Text, got[i].Text)
			require.Equal(t, want[i].ID, got[i].ID)
			require.Equal(t, want[i].Name, got[i].Name)
			if want[i].StopReason != "" {
				require.Equal(t, want[i].StopReason, got[i].StopReason)
			}
			if want[i].OutputTokens != 0 {
				require.Equal(t, want[i].OutputTokens, got[i].OutputTokens)
			}
			if want[i].Type == "tool_call" {
				require.Equal(t, want[i].Input["path"], got[i].Input["path"])
			}
		})
	}
}

// TestFabricateStream_AnthropicThinkingBlocksKeepOrder pins that a batch
// result's thinking and redacted_thinking blocks produce the same events, in
// the same order, as consumeSSE does for a streamed response.
func TestFabricateStream_AnthropicThinkingBlocksKeepOrder(t *testing.T) {
	response := []byte(`{
		"content": [
			{"type": "thinking", "thinking": "", "signature": "sig-batch"},
			{"type": "text", "text": "hi"},
			{"type": "redacted_thinking", "data": "data-batch"},
			{"type": "tool_use", "id": "tu_a", "name": "read_file", "input": {"path": "a"}}
		],
		"stop_reason": "tool_use",
		"usage": {"output_tokens": 5}
	}`)

	ch := make(chan types.StreamEvent, 8)
	require.NoError(t, fabricateAnthropicStream(ch, response))
	close(ch)
	got := drain(t, ch)

	want := []types.StreamEvent{
		{Type: "thinking", ThoughtSignature: "sig-batch"},
		{Type: "text_delta", Text: "hi"},
		{Type: "redacted_thinking", ThoughtSignature: "data-batch"},
		{Type: "tool_call", ID: "tu_a"},
		{Type: "message_complete"},
	}
	require.Len(t, got, len(want))
	for i := range want {
		t.Run(fmt.Sprintf("event %d %s", i, want[i].Type), func(t *testing.T) {
			require.Equal(t, want[i].Type, got[i].Type)
			require.Equal(t, want[i].Text, got[i].Text)
			require.Equal(t, want[i].ThoughtSignature, got[i].ThoughtSignature)
			require.Equal(t, want[i].ID, got[i].ID)
		})
	}

	blocks := got[4].Content
	require.Len(t, blocks, 4, "message_complete content")
	require.Equal(t, "thinking", blocks[0].Type)
	require.Equal(t, "sig-batch", blocks[0].ThoughtSignature)
	require.Equal(t, "redacted_thinking", blocks[2].Type)
	require.Equal(t, "data-batch", blocks[2].ThoughtSignature)
}

// TestFabricateStream_AnthropicThinkingMatchesStream pins the parity the
// batch path promises: a batch result yields the same events as a streamed
// response with the same content, thinking text included.
func TestFabricateStream_AnthropicThinkingMatchesStream(t *testing.T) {
	response := []byte(`{
		"content": [
			{"type": "thinking", "thinking": "plan it", "signature": "sig-batch"},
			{"type": "text", "text": "hi"},
			{"type": "redacted_thinking", "data": "data-batch"},
			{"type": "tool_use", "id": "tu_a", "name": "read_file", "input": {"path": "a"}}
		],
		"stop_reason": "tool_use",
		"usage": {"output_tokens": 5}
	}`)
	streamed := streamAnthropicSSE(t, joinLines(
		makeSSE("content_block_start", `{"index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":"plan it"}}`),
		makeSSE("content_block_delta", `{"index":0,"delta":{"type":"signature_delta","signature":"sig-batch"}}`),
		makeSSE("content_block_stop", `{"index":0}`),
		makeSSE("content_block_start", `{"index":1,"content_block":{"type":"text","text":""}}`),
		makeSSE("content_block_delta", `{"index":1,"delta":{"type":"text_delta","text":"hi"}}`),
		makeSSE("content_block_stop", `{"index":1}`),
		makeSSE("content_block_start", `{"index":2,"content_block":{"type":"redacted_thinking","data":"data-batch"}}`),
		makeSSE("content_block_stop", `{"index":2}`),
		makeSSE("content_block_start", `{"index":3,"content_block":{"type":"tool_use","id":"tu_a","name":"read_file","input":{}}}`),
		makeSSE("content_block_delta", `{"index":3,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a\"}"}}`),
		makeSSE("content_block_stop", `{"index":3}`),
		makeSSE("message_delta", `{"delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`),
		makeSSE("message_stop", `{}`),
	))

	ch := make(chan types.StreamEvent, 8)
	require.NoError(t, fabricateAnthropicStream(ch, response))
	close(ch)
	batched := drain(t, ch)

	// A batch response has no incremental reasoning to report; the loop
	// forwards its whole thinking block instead.
	streamed = slices.DeleteFunc(streamed, func(ev types.StreamEvent) bool { return ev.Type == "thinking_delta" })
	require.Equal(t, eventTypes(streamed), eventTypes(batched))
	for i := range streamed {
		t.Run(fmt.Sprintf("event %d %s", i, streamed[i].Type), func(t *testing.T) {
			s, b := streamed[i], batched[i]
			require.Equal(t, s.Type, b.Type)
			require.Equal(t, s.Text, b.Text)
			require.Equal(t, s.ThoughtSignature, b.ThoughtSignature)
			require.Equal(t, s.ID, b.ID)
			require.Equal(t, s.Name, b.Name)
			require.Equal(t, fmt.Sprint(s.Input), fmt.Sprint(b.Input))
			require.Equal(t, s.StopReason, b.StopReason)
		})
	}
}

// TestBatchAdapter_marshalRequestBody_AnthropicInputExamples pins that the
// batch path sends tool examples on the native input_examples field and
// leaves the schema untouched, as Stream does.
func TestBatchAdapter_marshalRequestBody_AnthropicInputExamples(t *testing.T) {
	a := NewBatchAdapter(nil, &fakeBatchClient{}, &types.BatchProviderConfig{Enabled: true}, "anthropic", "run-test")
	params := types.StreamParams{
		Model:     "claude-sonnet-5-5",
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens: 256,
		Tools: []types.ToolDefinition{{
			Name:         "echo",
			Description:  "Echo the input.",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
			Presentation: &types.ToolPresentation{InputExamples: []json.RawMessage{json.RawMessage(`{"x":"hi"}`)}},
		}},
	}
	body, err := a.marshalRequestBody(params)
	require.NoError(t, err)
	var req struct {
		Tools []struct {
			InputSchema   map[string]json.RawMessage `json:"input_schema"`
			InputExamples []json.RawMessage          `json:"input_examples"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(body, &req), "decode batch body:\n%s", body)
	require.Len(t, req.Tools, 1, "batch body:\n%s", body)
	require.Len(t, req.Tools[0].InputExamples, 1, "batch body:\n%s", body)
	require.JSONEq(t, `{"x":"hi"}`, string(req.Tools[0].InputExamples[0]), "batch body:\n%s", body)
	require.NotContains(t, req.Tools[0].InputSchema, "examples",
		"input_schema must not carry examples; batch body:\n%s", body)
}

// TestBatchAdapter_marshalRequestBody_AnthropicEffortAllowList pins that the
// batch path applies the same effort allow-list as Stream: a supported level
// is projected onto output_config and an unsupported one fails at marshal
// time rather than as a per-item batch error hours later.
func TestBatchAdapter_marshalRequestBody_AnthropicEffortAllowList(t *testing.T) {
	a := NewBatchAdapter(nil, &fakeBatchClient{}, &types.BatchProviderConfig{Enabled: true}, "anthropic", "run-test")
	params := types.StreamParams{
		Model:           "claude-sonnet-5-5",
		Messages:        []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens:       256,
		ReasoningEffort: "high",
	}
	body, err := a.marshalRequestBody(params)
	require.NoError(t, err)
	require.Contains(t, string(body), `"output_config":{"effort":"high"}`)

	params.ReasoningEffort = "minimal"
	_, err = a.marshalRequestBody(params)
	require.Error(t, err, "reasoningEffort minimal on claude-sonnet-5-5 should be rejected")
}
