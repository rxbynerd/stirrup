package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// This file covers the outbound half of the quirks ReplayFields
// round-trip (design §9 risk 7): flattening the per-stream capture onto
// the message_complete event, attaching it to assistant wire messages,
// and the cross-provider leakage guards that keep the state from
// reaching a provider that does not own it.

// TestFlattenReplayCapture pins the flattening rule documented on
// types.StreamEvent.ReplayFields: all-string captures concatenate in
// arrival order; anything else snapshots the last value.
func TestFlattenReplayCapture(t *testing.T) {
	t.Run("string pieces concatenate in arrival order", func(t *testing.T) {
		got := flattenReplayCapture(map[string][]any{
			"reasoning_content": {"", "Let me think. ", "The answer is 4."},
		})
		want := `"Let me think. The answer is 4."`
		if string(got["reasoning_content"]) != want {
			t.Errorf("reasoning_content = %s, want %s", got["reasoning_content"], want)
		}
	})
	t.Run("non-string values snapshot the last capture", func(t *testing.T) {
		got := flattenReplayCapture(map[string][]any{
			"thinking": {
				map[string]any{"step": float64(1)},
				map[string]any{"step": float64(2)},
			},
		})
		want := `{"step":2}`
		if string(got["thinking"]) != want {
			t.Errorf("thinking = %s, want %s", got["thinking"], want)
		}
	})
	t.Run("mixed types snapshot the last capture", func(t *testing.T) {
		got := flattenReplayCapture(map[string][]any{
			"field": {map[string]any{"a": "b"}, "tail"},
		})
		want := `"tail"`
		if string(got["field"]) != want {
			t.Errorf("field = %s, want %s", got["field"], want)
		}
	})
	t.Run("nil first chunk then strings concatenates", func(t *testing.T) {
		// A provider that opens the stream with `"reasoning_content":
		// null` decodes to a Go nil. The nil must be stripped before
		// the all-strings check — otherwise the last-value snapshot
		// arm fires and silently drops every intermediate piece.
		got := flattenReplayCapture(map[string][]any{
			"reasoning_content": {nil, "a", "b"},
		})
		if string(got["reasoning_content"]) != `"ab"` {
			t.Errorf("reasoning_content = %s, want \"ab\"", got["reasoning_content"])
		}
	})
	t.Run("empty capture returns nil", func(t *testing.T) {
		if got := flattenReplayCapture(nil); got != nil {
			t.Errorf("nil capture: got %v, want nil", got)
		}
		if got := flattenReplayCapture(map[string][]any{}); got != nil {
			t.Errorf("empty capture: got %v, want nil", got)
		}
		if got := flattenReplayCapture(map[string][]any{"p": {}}); got != nil {
			t.Errorf("empty value slice: got %v, want nil", got)
		}
		if got := flattenReplayCapture(map[string][]any{"p": {nil, nil}}); got != nil {
			t.Errorf("all-nil value slice: got %v, want nil", got)
		}
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
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		want := `{"role":"assistant","content":"done","reasoning_content":"thinking..."}`
		if string(out) != want {
			t.Errorf("Marshal = %s, want %s", out, want)
		}
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
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		want := `{"role":"assistant","alpha_field":"a","zeta_field":"z"}`
		if string(out) != want {
			t.Errorf("Marshal = %s, want %s", out, want)
		}
	})
	t.Run("no replay fields is byte-identical to the plain shape", func(t *testing.T) {
		m := openaiMessage{Role: "tool", Content: "ok", ToolCallID: "call_1"}
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		want := `{"role":"tool","content":"ok","tool_call_id":"call_1"}`
		if string(out) != want {
			t.Errorf("Marshal = %s, want %s", out, want)
		}
	})
	t.Run("canonical key collision errors", func(t *testing.T) {
		m := openaiMessage{
			Role:         "assistant",
			ReplayFields: map[string]json.RawMessage{"content": json.RawMessage(`"evil"`)},
		}
		_, err := json.Marshal(m)
		if err == nil {
			t.Fatal("expected collision error, got nil")
		}
		if !strings.Contains(err.Error(), "collides with canonical message field") {
			t.Errorf("error = %q, want collision message", err.Error())
		}
	})
	t.Run("invalid JSON payload errors", func(t *testing.T) {
		m := openaiMessage{
			Role:         "assistant",
			ReplayFields: map[string]json.RawMessage{"reasoning_content": json.RawMessage(`{not json`)},
		}
		_, err := json.Marshal(m)
		if err == nil {
			t.Fatal("expected invalid-JSON error, got nil")
		}
		if !strings.Contains(err.Error(), "invalid JSON") {
			t.Errorf("error = %q, want invalid-JSON message", err.Error())
		}
	})
}

// TestThreadableOpenAIReplayPath pins which path shapes qualify for
// outbound emission: single-segment, non-array, non-canonical only.
func TestThreadableOpenAIReplayPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"reasoning_content", true},
		{"thinking", true},
		{"delta.reasoning_content", false},                       // multi-segment
		{"candidates[].content.parts[].thoughtSignature", false}, // array iteration
		{"role", false},                                          // canonical message key
		{"content", false},                                       // canonical message key
		{"tool_calls", false},                                    // canonical message key
		{"tool_call_id", false},                                  // canonical message key
		{"name", false},                                          // canonical (documented optional key)
		{"a..b", false},                                          // malformed
		{"", false},                                              // malformed
	}
	for _, tc := range cases {
		if got := threadableOpenAIReplayPath(tc.path); got != tc.want {
			t.Errorf("threadableOpenAIReplayPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
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
		if len(out) != 1 {
			t.Fatalf("expected 1 message, got %d", len(out))
		}
		if string(out[0].ReplayFields["reasoning_content"]) != `"thinking"` {
			t.Errorf("ReplayFields = %v, want reasoning_content threaded", out[0].ReplayFields)
		}
		if _, leaked := out[0].ReplayFields["stale_field"]; leaked {
			t.Errorf("stale_field not in resolved paths must not leak: %v", out[0].ReplayFields)
		}
	})

	t.Run("no resolved paths emits nothing (registry is the source of truth)", func(t *testing.T) {
		out := translateMessages("", []types.Message{assistant}, nil)
		if out[0].ReplayFields != nil {
			t.Errorf("expected no replay fields without resolved paths, got %v", out[0].ReplayFields)
		}
		body, err := json.Marshal(out[0])
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if strings.Contains(string(body), "reasoning_content") {
			t.Errorf("wire message leaked reasoning_content with no resolved rule: %s", body)
		}
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
		if len(out[0].ReplayFields) != 1 {
			t.Fatalf("ReplayFields = %v, want only reasoning_content", out[0].ReplayFields)
		}
		if string(out[0].ReplayFields["reasoning_content"]) != `"ok"` {
			t.Errorf("reasoning_content = %s, want \"ok\"", out[0].ReplayFields["reasoning_content"])
		}
	})

	t.Run("empty payload is skipped", func(t *testing.T) {
		msg := types.Message{
			Role:         "assistant",
			Content:      []types.ContentBlock{{Type: "text", Text: "done"}},
			ReplayFields: map[string]json.RawMessage{"reasoning_content": nil},
		}
		out := translateMessages("", []types.Message{msg}, []string{"reasoning_content"})
		if out[0].ReplayFields != nil {
			t.Errorf("expected empty payload skipped, got %v", out[0].ReplayFields)
		}
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
			if m.ReplayFields != nil {
				t.Errorf("message %d (%s) carries replay fields: %v", i, m.Role, m.ReplayFields)
			}
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
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drainStream(t, ch)

	var complete *types.StreamEvent
	for i := range events {
		if events[i].Type == "message_complete" {
			complete = &events[i]
		}
	}
	if complete == nil {
		t.Fatalf("no message_complete event in %v", events)
	}
	// The fixture streams reasoning_content as "" then "Considering the
	// request carefully." — the flattened value is the in-order
	// concatenation, marshalled as a JSON string.
	want := `"Considering the request carefully."`
	if got := string(complete.ReplayFields["reasoning_content"]); got != want {
		t.Errorf("message_complete ReplayFields[reasoning_content] = %s, want %s", got, want)
	}
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
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for _, ev := range drainStream(t, ch) {
		if ev.Type == "message_complete" && ev.ReplayFields != nil {
			t.Errorf("message_complete carries ReplayFields with no rule: %v", ev.ReplayFields)
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
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		var completes []types.StreamEvent
		for _, ev := range drainStream(t, ch) {
			if ev.Type == "message_complete" {
				completes = append(completes, ev)
			}
		}
		if len(completes) != 1 {
			t.Fatalf("expected exactly 1 message_complete, got %d", len(completes))
		}
		if got := string(completes[0].ReplayFields["reasoning_content"]); got != `"part one part two"` {
			t.Errorf("ReplayFields[reasoning_content] = %s, want \"part one part two\"", got)
		}
		if completes[0].StopReason != "end_turn" {
			t.Errorf("StopReason = %q, want end_turn (canonical mapping of the synthetic stop)", completes[0].StopReason)
		}
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
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		count := 0
		var last types.StreamEvent
		for _, ev := range drainStream(t, ch) {
			if ev.Type == "message_complete" {
				count++
				last = ev
			}
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 message_complete, got %d (the [DONE] fallback must not double-emit)", count)
		}
		if last.StopReason != "end_turn" {
			t.Errorf("StopReason = %q, want end_turn from the finish_reason chunk", last.StopReason)
		}
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
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var complete *types.StreamEvent
	for _, ev := range drainStream(t, ch) {
		if ev.Type == "message_complete" {
			evCopy := ev
			complete = &evCopy
		}
	}
	if complete == nil {
		t.Fatal("no message_complete event")
	}
	got := complete.ReplayFields["reasoning_content"]
	if len(got) == 0 {
		t.Fatal("truncation must keep the accumulated prefix, not drop the field")
	}
	// The flattened value is a JSON string: content bytes plus two
	// quotes (the fixture content needs no escaping).
	if len(got) > maxReplayFieldBytes+2 {
		t.Errorf("flattened value is %d bytes, want <= %d (cap not enforced)", len(got), maxReplayFieldBytes+2)
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "replay field accumulator cap reached, truncating") {
		t.Errorf("missing cap WARN log; log:\n%.2000s", logOutput)
	}
	if !strings.Contains(logOutput, `"level":"WARN"`) {
		t.Errorf("cap log entry must be WARN level; log:\n%.2000s", logOutput)
	}
	if strings.Contains(logOutput, piece) {
		t.Errorf("cap WARN leaked captured content into the log")
	}
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
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
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
	if !found {
		t.Fatalf("replay_fields_captured.* attributes missing from recorded spans")
	}
	// The deepseek-v4 fixture streams two reasoning_content pieces
	// ("" + "Considering the request carefully.").
	if count != 2 {
		t.Errorf("replay_fields_captured.count = %d, want 2", count)
	}
	if totalLen != int64(len("Considering the request carefully.")) {
		t.Errorf("replay_fields_captured.total_len = %d, want %d", totalLen, len("Considering the request carefully."))
	}
}

// TestSummarizeReplayCaptures pins both arms of the length proxy
// directly: raw length for strings, JSON-encoded length for anything
// else (the arm no streaming fixture reaches, since the current rules
// only capture string fields).
func TestSummarizeReplayCaptures(t *testing.T) {
	t.Run("string values use raw length", func(t *testing.T) {
		count, totalLen := summarizeReplayCaptures(map[string][]any{
			"reasoning_content": {"ab", "cde"},
		})
		if count != 2 || totalLen != 5 {
			t.Errorf("got (count=%d, totalLen=%d), want (2, 5)", count, totalLen)
		}
	})
	t.Run("non-string values use JSON-encoded length", func(t *testing.T) {
		count, totalLen := summarizeReplayCaptures(map[string][]any{
			"x": {map[string]any{"step": 1}},
		})
		// json.Marshal(map[string]any{"step": 1}) = `{"step":1}` (10 bytes).
		if count != 1 || totalLen != 10 {
			t.Errorf("got (count=%d, totalLen=%d), want (1, 10)", count, totalLen)
		}
	})
	t.Run("cap accounting helper agrees with the summary proxy", func(t *testing.T) {
		if got := replayCaptureByteLen("abc"); got != 3 {
			t.Errorf("string: got %d, want 3", got)
		}
		if got := replayCaptureByteLen(map[string]any{"step": 1}); got != 10 {
			t.Errorf("object: got %d, want 10 (JSON-encoded length)", got)
		}
		// Unmarshalable values degrade to zero rather than failing —
		// the accumulator only ever holds decoded JSON, so this is the
		// defensive arm.
		if got := replayCaptureByteLen(func() {}); got != 0 {
			t.Errorf("unmarshalable: got %d, want 0", got)
		}
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
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
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
	if replay == nil {
		t.Fatal("turn 1 produced no ReplayFields")
	}
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
	if err != nil {
		t.Fatalf("build turn-2 request: %v", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal turn-2 request: %v", err)
	}
	want := `"reasoning_content":"Scanning the request. Choosing an answer."`
	if !strings.Contains(string(body), want) {
		t.Errorf("turn-2 wire body missing replayed reasoning_content:\nwant substring: %s\nbody: %s", want, body)
	}
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
// ThoughtSignature (#194).
func TestTranslateMessagesAnthropic_IgnoresMessageReplayFields(t *testing.T) {
	without, err := json.Marshal(translateMessagesAnthropic(replayLeakageMessages(false), quirks.StructuredToolResultCapability{}))
	if err != nil {
		t.Fatalf("marshal without: %v", err)
	}
	with, err := json.Marshal(translateMessagesAnthropic(replayLeakageMessages(true), quirks.StructuredToolResultCapability{}))
	if err != nil {
		t.Fatalf("marshal with: %v", err)
	}
	if !bytes.Equal(without, with) {
		t.Errorf("anthropic wire bytes diverge when Message.ReplayFields is set:\nwithout: %s\nwith:    %s", without, with)
	}
	if bytes.Contains(with, []byte("reasoning_content")) {
		t.Errorf("anthropic wire body leaked reasoning_content: %s", with)
	}
}

// TestTranslateMessagesGemini_IgnoresMessageReplayFields mirrors the
// Anthropic guard for the Gemini adapter: Gemini's round-trip state is
// the typed block-level ThoughtSignature, never the message-level
// ReplayFields map.
func TestTranslateMessagesGemini_IgnoresMessageReplayFields(t *testing.T) {
	contentsWithout, _, err := translateMessagesGemini("sys", replayLeakageMessages(false), quirks.StructuredToolResultCapability{})
	if err != nil {
		t.Fatalf("translate without: %v", err)
	}
	contentsWith, _, err := translateMessagesGemini("sys", replayLeakageMessages(true), quirks.StructuredToolResultCapability{})
	if err != nil {
		t.Fatalf("translate with: %v", err)
	}
	without, err := json.Marshal(contentsWithout)
	if err != nil {
		t.Fatalf("marshal without: %v", err)
	}
	with, err := json.Marshal(contentsWith)
	if err != nil {
		t.Fatalf("marshal with: %v", err)
	}
	if !bytes.Equal(without, with) {
		t.Errorf("gemini wire bytes diverge when Message.ReplayFields is set:\nwithout: %s\nwith:    %s", without, with)
	}
	if bytes.Contains(with, []byte("reasoning_content")) {
		t.Errorf("gemini wire body leaked reasoning_content: %s", with)
	}
}
