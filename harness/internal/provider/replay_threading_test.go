package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// This file covers the outbound half of the quirks ReplayFields
// round-trip: flattening the per-stream capture onto the
// message_complete event, attaching it to assistant wire messages, and
// the cross-provider leakage guards that keep the state from reaching a
// provider that does not own it.

// TestFlattenReplayCapture_ConcatenatesStringsOtherwiseSnapshots pins the
// flattening rule documented on types.StreamEvent.ReplayFields: all-string
// captures concatenate in arrival order; anything else snapshots the last
// value.
func TestFlattenReplayCapture_ConcatenatesStringsOtherwiseSnapshots(t *testing.T) {
	t.Run("string pieces concatenate in arrival order", func(t *testing.T) {
		got := flattenReplayCapture(map[string][]any{
			"reasoning_content": {"", "Let me think. ", "The answer is 4."},
		})
		require.Equal(t, `"Let me think. The answer is 4."`, string(got["reasoning_content"]))
	})
	t.Run("non-string values snapshot the last capture", func(t *testing.T) {
		got := flattenReplayCapture(map[string][]any{
			"thinking": {
				map[string]any{"step": float64(1)},
				map[string]any{"step": float64(2)},
			},
		})
		require.Equal(t, `{"step":2}`, string(got["thinking"]))
	})
	t.Run("mixed types snapshot the last capture", func(t *testing.T) {
		got := flattenReplayCapture(map[string][]any{
			"field": {map[string]any{"a": "b"}, "tail"},
		})
		require.Equal(t, `"tail"`, string(got["field"]))
	})
	t.Run("nil first chunk then strings concatenates", func(t *testing.T) {
		// A provider that opens the stream with `"reasoning_content":
		// null` decodes to a Go nil. The nil must be stripped before
		// the all-strings check — otherwise the last-value snapshot
		// arm fires and silently drops every intermediate piece.
		got := flattenReplayCapture(map[string][]any{
			"reasoning_content": {nil, "a", "b"},
		})
		require.Equal(t, `"ab"`, string(got["reasoning_content"]))
	})
	t.Run("nil capture returns nil", func(t *testing.T) {
		require.Nil(t, flattenReplayCapture(nil))
	})
	t.Run("empty capture map returns nil", func(t *testing.T) {
		require.Nil(t, flattenReplayCapture(map[string][]any{}))
	})
	t.Run("empty value slice returns nil", func(t *testing.T) {
		require.Nil(t, flattenReplayCapture(map[string][]any{"p": {}}))
	})
	t.Run("all-nil value slice returns nil", func(t *testing.T) {
		require.Nil(t, flattenReplayCapture(map[string][]any{"p": {nil, nil}}))
	})
}

// TestOpenAIMessage_MarshalJSON_MergesReplayFields pins the wire shape
// of an assistant message carrying replay state: canonical fields in
// struct order, replay entries appended in sorted-key order, and the
// defence-in-depth error arms for collisions and invalid payloads.
func TestOpenAIMessage_MarshalJSON_MergesReplayFields(t *testing.T) {
	t.Run("appends replay entries after canonical fields", func(t *testing.T) {
		m := openaiMessage{
			Role:    "assistant",
			Content: "done",
			ReplayFields: map[string]json.RawMessage{
				"reasoning_content": json.RawMessage(`"thinking..."`),
			},
		}
		out, err := json.Marshal(m)
		require.NoError(t, err)
		require.Equal(t, `{"role":"assistant","content":"done","reasoning_content":"thinking..."}`, string(out)) //nolint:testifylint // key order is the contract
	})
	t.Run("multiple entries emit in sorted key order", func(t *testing.T) {
		m := openaiMessage{
			Role: "assistant",
			ReplayFields: map[string]json.RawMessage{
				"zeta_field":  json.RawMessage(`"z"`),
				"alpha_field": json.RawMessage(`"a"`),
			},
		}
		out, err := json.Marshal(m)
		require.NoError(t, err)
		require.Equal(t, `{"role":"assistant","alpha_field":"a","zeta_field":"z"}`, string(out)) //nolint:testifylint // key order is the contract
	})
	t.Run("no replay fields is byte-identical to the plain shape", func(t *testing.T) {
		m := openaiMessage{Role: "tool", Content: "ok", ToolCallID: "call_1"}
		out, err := json.Marshal(m)
		require.NoError(t, err)
		require.Equal(t, `{"role":"tool","content":"ok","tool_call_id":"call_1"}`, string(out)) //nolint:testifylint // exact bytes are the contract
	})
	t.Run("canonical key collision errors", func(t *testing.T) {
		m := openaiMessage{
			Role:         "assistant",
			ReplayFields: map[string]json.RawMessage{"content": json.RawMessage(`"evil"`)},
		}
		_, err := json.Marshal(m)
		require.Error(t, err)
		require.ErrorContains(t, err, "collides with canonical message field")
	})
	t.Run("invalid JSON payload errors", func(t *testing.T) {
		m := openaiMessage{
			Role:         "assistant",
			ReplayFields: map[string]json.RawMessage{"reasoning_content": json.RawMessage(`{not json`)},
		}
		_, err := json.Marshal(m)
		require.Error(t, err)
		require.ErrorContains(t, err, "invalid JSON")
	})
}

// TestThreadableOpenAIReplayPath_SelectsSingleSegmentKeys pins which path
// shapes qualify for outbound emission: single-segment, non-array,
// non-canonical only.
func TestThreadableOpenAIReplayPath_SelectsSingleSegmentKeys(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"accepts reasoning_content", "reasoning_content", true},
		{"accepts thinking", "thinking", true},
		{"rejects nested path", "delta.reasoning_content", false},
		{"rejects array iteration", "candidates[].content.parts[].thoughtSignature", false},
		{"rejects canonical role key", "role", false},
		{"rejects canonical content key", "content", false},
		{"rejects canonical tool_calls key", "tool_calls", false},
		{"rejects canonical tool_call_id key", "tool_call_id", false},
		{"rejects canonical name key", "name", false},
		{"rejects empty segment", "a..b", false},
		{"rejects empty path", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, threadableOpenAIReplayPath(tc.path))
		})
	}
}

// TestTranslateMessages_ThreadsQualifyingReplayFields pins the selection
// rules at the translateMessages seam: only paths named by the resolved
// quirks for THIS stream are emitted, and non-threadable paths are
// silently skipped at runtime.
func TestTranslateMessages_ThreadsQualifyingReplayFields(t *testing.T) {
	assistant := types.Message{
		Role:    "assistant",
		Content: []types.ContentBlock{{Type: "text", Text: "done"}},
		ReplayFields: map[string]json.RawMessage{
			"reasoning_content": json.RawMessage(`"thinking"`),
			"stale_field":       json.RawMessage(`"from another model"`),
		},
	}

	t.Run("qualifying path is emitted", func(t *testing.T) {
		out := translateMessages("", []types.Message{assistant}, []string{"reasoning_content"})
		require.Len(t, out, 1)
		require.Equal(t, `"thinking"`, string(out[0].ReplayFields["reasoning_content"]))
		require.NotContains(t, out[0].ReplayFields, "stale_field", "fields outside the resolved paths must not leak")
	})

	t.Run("no resolved paths emits nothing", func(t *testing.T) {
		out := translateMessages("", []types.Message{assistant}, nil)
		require.Nil(t, out[0].ReplayFields)
		body, err := json.Marshal(out[0])
		require.NoError(t, err)
		require.NotContains(t, string(body), "reasoning_content")
	})

	t.Run("multi-segment and colliding paths are skipped", func(t *testing.T) {
		msg := types.Message{
			Role:    "assistant",
			Content: []types.ContentBlock{{Type: "text", Text: "done"}},
			ReplayFields: map[string]json.RawMessage{
				"delta.reasoning_content": json.RawMessage(`"nested"`),
				"role":                    json.RawMessage(`"evil"`),
				"reasoning_content":       json.RawMessage(`"ok"`),
			},
		}
		paths := []string{"delta.reasoning_content", "role", "reasoning_content"}
		out := translateMessages("", []types.Message{msg}, paths)
		require.Len(t, out[0].ReplayFields, 1, "only reasoning_content should be threaded")
		require.Equal(t, `"ok"`, string(out[0].ReplayFields["reasoning_content"]))
	})

	t.Run("empty payload is skipped", func(t *testing.T) {
		msg := types.Message{
			Role:         "assistant",
			Content:      []types.ContentBlock{{Type: "text", Text: "done"}},
			ReplayFields: map[string]json.RawMessage{"reasoning_content": nil},
		}
		out := translateMessages("", []types.Message{msg}, []string{"reasoning_content"})
		require.Nil(t, out[0].ReplayFields)
	})

	t.Run("user and tool messages never carry replay fields", func(t *testing.T) {
		msgs := []types.Message{
			{
				Role:    "user",
				Content: []types.ContentBlock{{Type: "text", Text: "hi"}},
				// A user message carrying replay state is malformed input;
				// the assistant-only emission point must ignore it.
				ReplayFields: map[string]json.RawMessage{"reasoning_content": json.RawMessage(`"x"`)},
			},
		}
		out := translateMessages("", msgs, []string{"reasoning_content"})
		for i, m := range out {
			require.Nil(t, m.ReplayFields, "message %d (%s)", i, m.Role)
		}
	})
}

// TestReplayFields_DeepSeekV4_MessageCompleteCarriesFlattenedValue
// drives the synthetic deepseek-v4 SSE fixture through the production
// Stream path and asserts the message_complete event carries the
// concatenation of every reasoning_content piece — the parse-side half
// of the round-trip the loop persists onto the assistant Message.
func TestReplayFields_DeepSeekV4_MessageCompleteCarriesFlattenedValue(t *testing.T) {
	srv := sseStubServer(t, streamFixtureSSE(t, "testdata/quirks/openai-compatible/deepseek-v4/response.sse"))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "deepseek-v4",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	require.NoError(t, err)
	events := drainStream(t, ch)

	var complete *types.StreamEvent
	for i := range events {
		if events[i].Type == "message_complete" {
			complete = &events[i]
		}
	}
	require.NotNil(t, complete, "no message_complete event in %v", events)
	// The fixture streams reasoning_content as "" then "Considering the
	// request carefully." — the flattened value is the in-order
	// concatenation, marshalled as a JSON string.
	want := `"Considering the request carefully."`
	require.Equal(t, want, string(complete.ReplayFields["reasoning_content"]))
}

// TestReplayFields_NoRule_MessageCompleteCarriesNothing pins the
// negative: a model whose resolved quirks register no ReplayFields
// produces a message_complete with a nil ReplayFields map, keeping the
// event byte-identical to the pre-threading shape.
func TestReplayFields_NoRule_MessageCompleteCarriesNothing(t *testing.T) {
	srv := sseStubServer(t, streamFixtureSSE(t, "testdata/quirks/openai-compatible/o1-mini/response.sse"))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	require.NoError(t, err)
	for _, ev := range drainStream(t, ch) {
		if ev.Type == "message_complete" {
			require.Nil(t, ev.ReplayFields, "message_complete carries ReplayFields with no rule")
		}
	}
}

// TestReplayFields_DoneOnlyTermination_EmitsMessageComplete pins the
// bare-[DONE] fallback: some proxy gateways omit the finish_reason
// chunk and terminate with [DONE] alone. Accumulated replay state must
// still reach a message_complete event — silently dropping it would
// 400 the next DeepSeek turn. The companion sub-test pins the guard's
// other half: a stream that completed normally must emit exactly one
// message_complete (a duplicate from the fallback would clobber the
// real stop reason in streamEventsToResult's last-write-wins merge).
func TestReplayFields_DoneOnlyTermination_EmitsMessageComplete(t *testing.T) {
	t.Run("bare DONE emits synthetic message_complete", func(t *testing.T) {
		sse := "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"part one \"},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"part two\"},\"finish_reason\":null}]}\n\n" +
			"data: [DONE]\n\n"
		srv := sseStubServer(t, []byte(sse))
		defer srv.Close()

		adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
		ch, err := adapter.Stream(context.Background(), types.StreamParams{
			Model:     "deepseek-v4-flash",
			MaxTokens: 1024,
			Messages: []types.Message{
				{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
			},
		})
		require.NoError(t, err)
		var completes []types.StreamEvent
		for _, ev := range drainStream(t, ch) {
			if ev.Type == "message_complete" {
				completes = append(completes, ev)
			}
		}
		require.Len(t, completes, 1, "expected exactly one message_complete")
		require.Equal(t, `"part one part two"`, string(completes[0].ReplayFields["reasoning_content"]))
		require.Equal(t, "end_turn", completes[0].StopReason, "the synthetic stop maps to end_turn")
	})

	t.Run("normal finish_reason stream emits exactly one message_complete", func(t *testing.T) {
		srv := sseStubServer(t, streamFixtureSSE(t, "testdata/quirks/openai-compatible/deepseek-v4/response.sse"))
		defer srv.Close()

		adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
		ch, err := adapter.Stream(context.Background(), types.StreamParams{
			Model:     "deepseek-v4",
			MaxTokens: 1024,
			Messages: []types.Message{
				{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
			},
		})
		require.NoError(t, err)
		var completes []types.StreamEvent
		for _, ev := range drainStream(t, ch) {
			if ev.Type == "message_complete" {
				completes = append(completes, ev)
			}
		}
		require.Len(t, completes, 1, "the [DONE] fallback must not double-emit")
		require.Equal(t, "end_turn", completes[0].StopReason, "the stop reason comes from the finish_reason chunk")
	})
}

// TestReplayFields_CapBoundsAccumulator pins the maxReplayFieldBytes
// budget: a provider streaming far more captured content than any real
// chain-of-thought field must be truncated to a bounded prefix, the
// stream must complete normally, and one WARN must name the cap. 600
// chunks of 1 kB exceed the 512 kB budget by ~17%.
func TestReplayFields_CapBoundsAccumulator(t *testing.T) {
	piece := strings.Repeat("a", 1024)
	var sse strings.Builder
	for i := 0; i < 600; i++ {
		sse.WriteString(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":"` + piece + `"},"finish_reason":null}]}`)
		sse.WriteString("\n\n")
	}
	sse.WriteString(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	sse.WriteString("\n\ndata: [DONE]\n\n")

	srv := sseStubServer(t, []byte(sse.String()))
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Logger = logger

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "deepseek-v4-flash",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	require.NoError(t, err)
	var complete *types.StreamEvent
	for _, ev := range drainStream(t, ch) {
		if ev.Type == "message_complete" {
			evCopy := ev
			complete = &evCopy
		}
	}
	require.NotNil(t, complete)
	got := complete.ReplayFields["reasoning_content"]
	require.NotEmpty(t, got, "truncation must keep the accumulated prefix, not drop the field")
	// The flattened value is a JSON string: content bytes plus two
	// quotes (the fixture content needs no escaping).
	require.LessOrEqual(t, len(got), maxReplayFieldBytes+2, "the flattened value must respect the cap")

	logOutput := buf.String()
	t.Run("logs a WARN naming the cap", func(t *testing.T) {
		require.Contains(t, logOutput, "replay field accumulator cap reached, truncating")
		require.Contains(t, logOutput, `"level":"WARN"`)
	})
	t.Run("cap log omits captured content", func(t *testing.T) {
		require.NotContains(t, logOutput, piece)
	})
}

// TestReplayFields_SpanAttributesRecordCaptureSummary pins the OTel
// half of the per-stream capture observability: when a span is active
// on the Stream context, the deferred capture summary must set
// replay_fields_captured.count / .total_len (length-only — values
// never reach the trace). The stub-server tests never carried a span,
// leaving summarizeReplayCaptures unexercised.
func TestReplayFields_SpanAttributesRecordCaptureSummary(t *testing.T) {
	srv := sseStubServer(t, streamFixtureSSE(t, "testdata/quirks/openai-compatible/deepseek-v4/response.sse"))
	defer srv.Close()

	ctx, exporter, span := withRecordingSpan(t)

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(ctx, types.StreamParams{
		Model:     "deepseek-v4",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	require.NoError(t, err)
	_ = drainStream(t, ch)
	span.End()

	var count, totalLen int64
	found := false
	for _, stub := range exporter.GetSpans() {
		for _, attr := range stub.Attributes {
			switch attr.Key {
			case "replay_fields_captured.count":
				count = attr.Value.AsInt64()
				found = true
			case "replay_fields_captured.total_len":
				totalLen = attr.Value.AsInt64()
			}
		}
	}
	require.True(t, found, "replay_fields_captured.* attributes missing from recorded spans")
	// The deepseek-v4 fixture streams two reasoning_content pieces
	// ("" + "Considering the request carefully.").
	require.Equal(t, int64(2), count, "replay_fields_captured.count")
	require.Equal(t, int64(len("Considering the request carefully.")), totalLen, "replay_fields_captured.total_len")
}

// TestSummarizeReplayCaptures_LengthProxy pins both arms of the length proxy
// directly: raw length for strings, JSON-encoded length for anything
// else (the arm no streaming fixture reaches, since the current rules
// only capture string fields).
func TestSummarizeReplayCaptures_LengthProxy(t *testing.T) {
	t.Run("string values use raw length", func(t *testing.T) {
		count, totalLen := summarizeReplayCaptures(map[string][]any{
			"reasoning_content": {"ab", "cde"},
		})
		require.Equal(t, 2, count)
		require.Equal(t, 5, totalLen)
	})
	t.Run("non-string values use JSON-encoded length", func(t *testing.T) {
		count, totalLen := summarizeReplayCaptures(map[string][]any{
			"x": {map[string]any{"step": 1}},
		})
		// json.Marshal(map[string]any{"step": 1}) = `{"step":1}` (10 bytes).
		require.Equal(t, 1, count)
		require.Equal(t, 10, totalLen)
	})
	t.Run("byte length of a string is its raw length", func(t *testing.T) {
		require.Equal(t, 3, replayCaptureByteLen("abc"))
	})
	t.Run("byte length of an object is its JSON length", func(t *testing.T) {
		require.Equal(t, 10, replayCaptureByteLen(map[string]any{"step": 1}))
	})
	// Unmarshalable values degrade to zero rather than failing — the
	// accumulator only ever holds decoded JSON, so this is the defensive arm.
	t.Run("byte length of an unmarshalable value is zero", func(t *testing.T) {
		require.Equal(t, 0, replayCaptureByteLen(func() {}))
	})
}

// TestReplayFields_TwoTurnRoundTrip drives the full failure mode the
// feature exists to prevent: turn 1 streams a DeepSeek v4 response with
// reasoning_content, the captured state is attached to the assistant
// message exactly as the agentic loop does it, and the turn-2 request
// built from that history must carry reasoning_content as a top-level
// key on the assistant wire message — the shape whose absence 400s.
func TestReplayFields_TwoTurnRoundTrip(t *testing.T) {
	srv := sseStubServer(t, streamFixtureSSE(t, "testdata/quirks/openai-compatible/deepseek-v4-flash/response.sse"))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	history := []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
	}

	// Turn 1: stream and assemble the assistant message the way
	// streamEventsToResult + appendAssistantContent do — text deltas
	// concatenate into one block, message_complete supplies the
	// replay state.
	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "deepseek-v4-flash",
		MaxTokens: 1024,
		Messages:  history,
	})
	require.NoError(t, err)
	var text strings.Builder
	var replay map[string]json.RawMessage
	for _, ev := range drainStream(t, ch) {
		switch ev.Type {
		case "text_delta":
			text.WriteString(ev.Text)
		case "message_complete":
			replay = ev.ReplayFields
		}
	}
	require.NotNil(t, replay, "turn 1 produced no ReplayFields")
	history = append(history, types.Message{
		Role:         "assistant",
		Content:      []types.ContentBlock{{Type: "text", Text: text.String()}},
		ReplayFields: replay,
	})
	history = append(history, types.Message{
		Role:    "user",
		Content: []types.ContentBlock{{Type: "text", Text: "and now?"}},
	})

	// Turn 2: the request built from the updated history must replay
	// the captured value verbatim on the assistant message.
	q := quirks.DefaultRegistry().Resolve("openai-compatible", "deepseek-v4-flash")
	req, err := buildOpenAIRequest(types.StreamParams{
		Model:     "deepseek-v4-flash",
		MaxTokens: 1024,
		Messages:  history,
	}, true, q, nil)
	require.NoError(t, err, "build turn-2 request")
	body, err := json.Marshal(req)
	require.NoError(t, err, "marshal turn-2 request")
	want := `"reasoning_content":"Scanning the request. Choosing an answer."`
	require.Contains(t, string(body), want, "turn-2 wire body must replay reasoning_content")
}

// replayLeakageMessages is the shared history for the cross-provider
// leakage guards: an assistant message carrying ReplayFields state that
// only an openai-compatible adapter is allowed to serialise.
func replayLeakageMessages(withReplay bool) []types.Message {
	msgs := []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "list main.go"}}},
		{
			Role: "assistant",
			Content: []types.ContentBlock{
				{Type: "text", Text: "Reading."},
				{Type: "tool_use", ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
			},
		},
		{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "call_1", Content: "package main"}}},
	}
	if withReplay {
		msgs[1].ReplayFields = map[string]json.RawMessage{
			"reasoning_content": json.RawMessage(`"DeepSeek-private chain of thought"`),
		}
	}
	return msgs
}

// TestTranslateMessagesAnthropic_IgnoresMessageReplayFields is the
// cross-provider leakage guard for the message-level replay state: the
// Anthropic wire body must be byte-identical with and without
// Message.ReplayFields populated, the same invariant
// anthropicContentBlock already enforces for the block-level
// ThoughtSignature.
func TestTranslateMessagesAnthropic_IgnoresMessageReplayFields(t *testing.T) {
	without, err := json.Marshal(translateMessagesAnthropic(replayLeakageMessages(false), quirks.StructuredToolResultCapability{}))
	require.NoError(t, err, "marshal without replay fields")
	with, err := json.Marshal(translateMessagesAnthropic(replayLeakageMessages(true), quirks.StructuredToolResultCapability{}))
	require.NoError(t, err, "marshal with replay fields")
	require.Equal(t, string(without), string(with), "anthropic wire bytes diverge when Message.ReplayFields is set")
	require.NotContains(t, string(with), "reasoning_content", "anthropic wire body leaked reasoning_content")
}

// TestTranslateMessagesGemini_IgnoresMessageReplayFields mirrors the
// Anthropic guard for the Gemini adapter: Gemini's round-trip state is
// the typed block-level ThoughtSignature, never the message-level
// ReplayFields map.
func TestTranslateMessagesGemini_IgnoresMessageReplayFields(t *testing.T) {
	contentsWithout, _, err := translateMessagesGemini("sys", replayLeakageMessages(false), quirks.StructuredToolResultCapability{}, "function")
	require.NoError(t, err, "translate without replay fields")
	contentsWith, _, err := translateMessagesGemini("sys", replayLeakageMessages(true), quirks.StructuredToolResultCapability{}, "function")
	require.NoError(t, err, "translate with replay fields")
	without, err := json.Marshal(contentsWithout)
	require.NoError(t, err, "marshal without replay fields")
	with, err := json.Marshal(contentsWith)
	require.NoError(t, err, "marshal with replay fields")
	require.Equal(t, string(without), string(with), "gemini wire bytes diverge when Message.ReplayFields is set")
	require.NotContains(t, string(with), "reasoning_content", "gemini wire body leaked reasoning_content")
}
